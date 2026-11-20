package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/realtime"
	"github.com/jackc/pgx/v5/pgxpool"
)

type TeamDTO struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description,omitempty"`
	PersonalTeam bool   `json:"personal_team"`
	Role         string `json:"role,omitempty"`
}

type MemberDTO struct {
	UserID int64                 `json:"user_id"`
	Name   string                `json:"name"`
	Email  string                `json:"email"`
	Role   string                `json:"role"`
	Access []map[string]any      `json:"access,omitempty"`
}

// PostgresTeamService persists teams, memberships, JSON access grants (`team_user.access`),
// and audit logs in PostgreSQL with transactional `team.grant_revoked` outbox events.
type PostgresTeamService struct {
	pool   *pgxpool.Pool
	authz  *access.Authorizer
	outbox *realtime.PostgresOutboxStore
}

func NewPostgresTeamService(pool *pgxpool.Pool, authz *access.Authorizer, outbox *realtime.PostgresOutboxStore) *PostgresTeamService {
	return &PostgresTeamService{pool: pool, authz: authz, outbox: outbox}
}

func (s *PostgresTeamService) ListUserTeams(ctx context.Context, userID int64) ([]TeamDTO, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.name, COALESCE(t.description, ''), t.personal_team, tu.role
		FROM teams t
		JOIN team_user tu ON tu.team_id = t.id
		WHERE tu.user_id = $1
		ORDER BY t.id ASC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TeamDTO, 0)
	for rows.Next() {
		var t TeamDTO
		if err := rows.Scan(&t.ID, &t.Name, &t.Description, &t.PersonalTeam, &t.Role); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *PostgresTeamService) ListMembers(ctx context.Context, principal auth.Principal, teamID int64) ([]MemberDTO, error) {
	if err := s.authz.Check(ctx, principal, access.ActionRead, access.ResourceRef{
		Type: "team", ID: teamID, TeamID: &teamID, APISegment: "teams",
	}); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.name, u.email, tu.role, tu.access
		FROM team_user tu
		JOIN users u ON u.id = tu.user_id
		WHERE tu.team_id = $1
		ORDER BY u.id ASC
	`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]MemberDTO, 0)
	for rows.Next() {
		var m MemberDTO
		var accessRaw []byte
		if err := rows.Scan(&m.UserID, &m.Name, &m.Email, &m.Role, &accessRaw); err != nil {
			return nil, err
		}
		if len(accessRaw) > 0 && string(accessRaw) != "null" {
			_ = json.Unmarshal(accessRaw, &m.Access)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetMemberAccessGrants updates `team_user.access` JSON and emits a transactional `team.grant_revoked`
// outbox event + audit row so active SSE/WebSocket streams immediately reauthorize or close.
func (s *PostgresTeamService) SetMemberAccessGrants(ctx context.Context, principal auth.Principal, teamID int64, memberID int64, grants []map[string]any) error {
	if err := s.authz.Check(ctx, principal, access.ActionUpdate, access.ResourceRef{
		Type: "team", ID: teamID, TeamID: &teamID, APISegment: "teams",
	}); err != nil {
		return err
	}
	raw, err := json.Marshal(grants)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	tag, err := tx.Exec(ctx, `
		UPDATE team_user SET access = $3::jsonb, updated_at = NOW()
		WHERE team_id = $1 AND user_id = $2
	`, teamID, memberID, string(raw))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("member %d not found in team %d", memberID, teamID)
	}
	_, _ = tx.Exec(ctx, `
		INSERT INTO audit_events (event, actor_user_id, team_id, resource_type, resource_uuid, severity, metadata)
		VALUES ('team.grants.updated', $1, $2, 'user', $3, 'info', $4::jsonb)
	`, principal.UserID, teamID, fmt.Sprintf("user-%d", memberID), string(raw))

	if s.outbox != nil {
		if err := s.outbox.AppendInTx(ctx, tx, realtime.DomainEvent{
			EventID:           fmt.Sprintf("grant-%d-%d-%d", teamID, memberID, time.Now().UnixNano()),
			SchemaVersion:     "v1",
			Producer:          "go-controlplane",
			TeamID:            teamID,
			ResourceType:      "access",
			ResourceUUID:      fmt.Sprintf("user-%d", memberID),
			AggregateRevision: time.Now().UnixNano(),
			OwnershipEpoch:    1,
			MutationType:      "team.grant_revoked",
			CommittedAt:       time.Now().UTC(),
			Payload:           map[string]any{"user_id": memberID, "team_id": teamID},
		}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *PostgresTeamService) RecordAudit(ctx context.Context, actorUserID *int64, teamID *int64, event string, resourceType string, resourceUUID string, severity string, meta map[string]any) {
	if severity == "" {
		severity = "info"
	}
	raw, _ := json.Marshal(meta)
	if len(raw) == 0 || string(raw) == "null" {
		raw = []byte("{}")
	}
	_, _ = s.pool.Exec(ctx, `
		INSERT INTO audit_events (event, actor_user_id, team_id, resource_type, resource_uuid, severity, metadata)
		VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb)
	`, event, actorUserID, teamID, resourceType, resourceUUID, severity, string(raw))
}

func (s *PostgresTeamService) ListAudit(ctx context.Context, principal auth.Principal, teamID int64, limit int) ([]map[string]any, error) {
	if err := s.authz.Check(ctx, principal, access.ActionUpdate, access.ResourceRef{
		Type: "team", ID: teamID, TeamID: &teamID, APISegment: "teams",
	}); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, event, actor_user_id, team_id, COALESCE(resource_type,''), COALESCE(resource_uuid,''), severity, metadata, created_at
		FROM audit_events
		WHERE team_id = $1
		ORDER BY id DESC
		LIMIT $2
	`, teamID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0)
	for rows.Next() {
		var id int64
		var actor, tid *int64
		var ev, rt, ru, sev string
		var metaRaw []byte
		var ts time.Time
		if err := rows.Scan(&id, &ev, &actor, &tid, &rt, &ru, &sev, &metaRaw, &ts); err != nil {
			return nil, err
		}
		var meta map[string]any
		_ = json.Unmarshal(metaRaw, &meta)
		out = append(out, map[string]any{
			"id": id, "event": ev, "actor_user_id": actor, "team_id": tid,
			"resource_type": rt, "resource_uuid": ru, "severity": sev, "metadata": meta, "created_at": ts,
		})
	}
	return out, rows.Err()
}
