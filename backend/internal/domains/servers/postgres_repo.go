package servers

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coollabsio/coolify/backend/internal/realtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresRepository persists servers, server_settings, private_keys, and standalone_dockers in PostgreSQL,
// emitting transactional outbox events inside the same pgx.Tx.
type PostgresRepository struct {
	pool   *pgxpool.Pool
	outbox *realtime.PostgresOutboxStore
}

func NewPostgresRepository(pool *pgxpool.Pool, outbox *realtime.PostgresOutboxStore) *PostgresRepository {
	return &PostgresRepository{pool: pool, outbox: outbox}
}

func (r *PostgresRepository) GetByUUID(ctx context.Context, uuid string) (Server, error) {
	var s Server
	var desc *string
	var pkUUID *string
	var transferredAt *time.Time
	err := r.pool.QueryRow(ctx, `
		SELECT s.id, s.uuid, s.name, s.description, s.ip, s.port, s."user", s.team_id,
		       s.private_key_id, pk.uuid, COALESCE(ss.wildcard_domain, ''),
		       COALESCE(ss.is_reachable, FALSE), COALESCE(ss.is_usable, FALSE), COALESCE(ss.force_disabled, FALSE),
		       ta.transferred_at, s.updated_at
		FROM servers s
		LEFT JOIN server_settings ss ON ss.server_id = s.id
		LEFT JOIN private_keys pk ON pk.id = s.private_key_id
		LEFT JOIN go_transfer_archives ta ON ta.server_uuid = s.uuid
		WHERE s.uuid = $1 AND s.deleted_at IS NULL
	`, uuid).Scan(
		&s.ID, &s.UUID, &s.Name, &desc, &s.IP, &s.Port, &s.User, &s.TeamID,
		&s.PrivateKeyID, &pkUUID, &s.WildcardDomain,
		&s.IsReachable, &s.IsUsable, &s.ForceDisabled,
		&transferredAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Server{}, ErrNotFound
	}
	if err != nil {
		return Server{}, err
	}
	if desc != nil {
		s.Description = *desc
	}
	if pkUUID != nil {
		s.PrivateKeyUUID = *pkUUID
	}
	s.TransferredAt = transferredAt
	return s, nil
}

func (r *PostgresRepository) ListByTeam(ctx context.Context, teamID int64) ([]Server, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT s.id, s.uuid, s.name, s.description, s.ip, s.port, s."user", s.team_id,
		       s.private_key_id, pk.uuid, COALESCE(ss.wildcard_domain, ''),
		       COALESCE(ss.is_reachable, FALSE), COALESCE(ss.is_usable, FALSE), COALESCE(ss.force_disabled, FALSE),
		       ta.transferred_at, s.updated_at
		FROM servers s
		LEFT JOIN server_settings ss ON ss.server_id = s.id
		LEFT JOIN private_keys pk ON pk.id = s.private_key_id
		LEFT JOIN go_transfer_archives ta ON ta.server_uuid = s.uuid
		WHERE s.team_id = $1 AND s.deleted_at IS NULL
		ORDER BY s.id ASC
	`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Server, 0)
	for rows.Next() {
		var s Server
		var desc *string
		var pkUUID *string
		var transferredAt *time.Time
		if err := rows.Scan(
			&s.ID, &s.UUID, &s.Name, &desc, &s.IP, &s.Port, &s.User, &s.TeamID,
			&s.PrivateKeyID, &pkUUID, &s.WildcardDomain,
			&s.IsReachable, &s.IsUsable, &s.ForceDisabled,
			&transferredAt, &s.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if desc != nil {
			s.Description = *desc
		}
		if pkUUID != nil {
			s.PrivateKeyUUID = *pkUUID
		}
		s.TransferredAt = transferredAt
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *PostgresRepository) FindPrivateKeyByUUID(ctx context.Context, uuid string) (PrivateKeyRef, error) {
	var pk PrivateKeyRef
	err := r.pool.QueryRow(ctx, `SELECT id, uuid, team_id FROM private_keys WHERE uuid = $1`, uuid).Scan(&pk.ID, &pk.UUID, &pk.TeamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PrivateKeyRef{}, ErrNotFound
	}
	if err != nil {
		return PrivateKeyRef{}, err
	}
	return pk, nil
}

func (r *PostgresRepository) Save(ctx context.Context, srv Server) (Server, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Server{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var existingID int64
	var isUpdate bool
	err = tx.QueryRow(ctx, `SELECT id FROM servers WHERE uuid = $1 AND deleted_at IS NULL`, srv.UUID).Scan(&existingID)
	if err == nil {
		isUpdate = true
		srv.ID = existingID
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return Server{}, err
	}

	if isUpdate {
		if err := tx.QueryRow(ctx, `
			UPDATE servers
			SET name = $2, description = $3, ip = $4, port = $5, "user" = $6,
			    private_key_id = $7, updated_at = NOW()
			WHERE id = $1
			RETURNING updated_at
		`, srv.ID, srv.Name, srv.Description, srv.IP, srv.Port, srv.User, srv.PrivateKeyID).Scan(&srv.UpdatedAt); err != nil {
			return Server{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO server_settings (server_id, is_reachable, is_usable, force_disabled, wildcard_domain)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''))
			ON CONFLICT (server_id) DO UPDATE SET
				is_reachable = EXCLUDED.is_reachable,
				is_usable = EXCLUDED.is_usable,
				force_disabled = EXCLUDED.force_disabled,
				wildcard_domain = EXCLUDED.wildcard_domain,
				updated_at = NOW()
		`, srv.ID, srv.IsReachable, srv.IsUsable, srv.ForceDisabled, srv.WildcardDomain); err != nil {
			return Server{}, err
		}
		srv.Revision++
	} else {
		if err := tx.QueryRow(ctx, `
			INSERT INTO servers (uuid, name, description, ip, port, "user", team_id, private_key_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			RETURNING id, updated_at
		`, srv.UUID, srv.Name, srv.Description, srv.IP, srv.Port, srv.User, srv.TeamID, srv.PrivateKeyID).Scan(&srv.ID, &srv.UpdatedAt); err != nil {
			return Server{}, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO server_settings (server_id, is_reachable, is_usable, force_disabled, wildcard_domain)
			VALUES ($1, $2, $3, $4, NULLIF($5, ''))
			ON CONFLICT (server_id) DO NOTHING
		`, srv.ID, srv.IsReachable, srv.IsUsable, srv.ForceDisabled, srv.WildcardDomain); err != nil {
			return Server{}, err
		}
		// Mirror Eloquent Server created hook: create default standalone_dockers destination
		if _, err := tx.Exec(ctx, `
			INSERT INTO standalone_dockers (uuid, name, network, server_id)
			VALUES ($1, 'standalone-docker', 'coolify', $2)
			ON CONFLICT (uuid) DO NOTHING
		`, "dest-"+srv.UUID, srv.ID); err != nil {
			return Server{}, err
		}
		srv.Revision = 1
	}

	if r.outbox != nil {
		mutType := "server.created"
		if isUpdate {
			mutType = "server.updated"
		}
		if err := r.outbox.AppendInTx(ctx, tx, realtime.DomainEvent{
			EventID:           fmt.Sprintf("%s-%s-%d", mutType, srv.UUID, srv.UpdatedAt.UnixNano()),
			SchemaVersion:     "v1",
			Producer:          "go-controlplane",
			TeamID:            srv.TeamID,
			ResourceType:      "server",
			ResourceUUID:      srv.UUID,
			AggregateRevision: maxInt64(srv.Revision, 1),
			OwnershipEpoch:    1,
			MutationType:      mutType,
			CommittedAt:       srv.UpdatedAt.UTC(),
			Payload:           map[string]any{"name": srv.Name, "uuid": srv.UUID},
		}); err != nil {
			return Server{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Server{}, err
	}
	return srv, nil
}

func (r *PostgresRepository) DeleteByUUID(ctx context.Context, uuid string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var id, teamID int64
	err = tx.QueryRow(ctx, `SELECT id, team_id FROM servers WHERE uuid = $1 AND deleted_at IS NULL FOR UPDATE`, uuid).Scan(&id, &teamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if id == 0 {
		return ErrInvalidLocalhost
	}
	if _, err := tx.Exec(ctx, `UPDATE servers SET deleted_at = NOW(), updated_at = NOW() WHERE id = $1`, id); err != nil {
		return err
	}
	if r.outbox != nil {
		if err := r.outbox.AppendInTx(ctx, tx, realtime.DomainEvent{
			EventID:           fmt.Sprintf("server.deleted-%s-%d", uuid, time.Now().UnixNano()),
			SchemaVersion:     "v1",
			Producer:          "go-controlplane",
			TeamID:            teamID,
			ResourceType:      "server",
			ResourceUUID:      uuid,
			AggregateRevision: 1,
			OwnershipEpoch:    1,
			MutationType:      "server.deleted",
			CommittedAt:       time.Now().UTC(),
			Payload:           map[string]any{"uuid": uuid},
		}); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) ListDestinations(ctx context.Context, serverID int64) ([]Destination, error) {
	rows, err := r.pool.Query(ctx, `SELECT id, uuid, server_id, network FROM standalone_dockers WHERE server_id = $1 ORDER BY id ASC`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Destination
	for rows.Next() {
		var d Destination
		d.Type = "standalone"
		if err := rows.Scan(&d.ID, &d.UUID, &d.ServerID, &d.Network); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CreatePrivateKey stores an encrypted SSH private key for a team in PostgreSQL.
func (r *PostgresRepository) CreatePrivateKey(ctx context.Context, uuid string, name string, description string, encryptedPEM string, fingerprint string, teamID int64) (PrivateKeyRef, error) {
	var pk PrivateKeyRef
	err := r.pool.QueryRow(ctx, `
		INSERT INTO private_keys (uuid, name, description, private_key, fingerprint, team_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, uuid, team_id
	`, uuid, name, description, encryptedPEM, fingerprint, teamID).Scan(&pk.ID, &pk.UUID, &pk.TeamID)
	return pk, err
}

func (r *PostgresRepository) ListPrivateKeys(ctx context.Context, teamID int64) ([]map[string]any, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, uuid, name, COALESCE(description, ''), COALESCE(fingerprint, ''), is_git_related, team_id, created_at
		FROM private_keys
		WHERE team_id = $1
		ORDER BY id ASC
	`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0)
	for rows.Next() {
		var id, tid int64
		var uuid, name, desc, fp string
		var gitRel bool
		var created time.Time
		if err := rows.Scan(&id, &uuid, &name, &desc, &fp, &gitRel, &tid, &created); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"id":             id,
			"uuid":           uuid,
			"name":           name,
			"description":    desc,
			"fingerprint":    fp,
			"is_git_related": gitRel,
			"team_id":        tid,
			"created_at":     created,
		})
	}
	return out, rows.Err()
}
