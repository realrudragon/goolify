package projects

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/realtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("project or environment not found")

type SharedVariable struct {
	ID            int64  `json:"id"`
	UUID          string `json:"uuid"`
	Key           string `json:"key"`
	Value         string `json:"value,omitempty"` // Redacted if is_shown_once or member read-only
	Type          string `json:"type"`            // team | project | environment | server
	IsShownOnce   bool   `json:"is_shown_once"`
	TeamID        int64  `json:"team_id"`
	ProjectID     *int64 `json:"project_id,omitempty"`
	EnvironmentID *int64 `json:"environment_id,omitempty"`
	ServerID      *int64 `json:"server_id,omitempty"`
}

// PostgresProjectService persists projects, environments, shared variables, and tags in PostgreSQL with transactional outbox.
type PostgresProjectService struct {
	pool   *pgxpool.Pool
	authz  *access.Authorizer
	hooks  *compat.EloquentHooks
	cipher *compat.Cipher
	outbox *realtime.PostgresOutboxStore
}

func NewPostgresProjectService(
	pool *pgxpool.Pool,
	authz *access.Authorizer,
	hooks *compat.EloquentHooks,
	cipher *compat.Cipher,
	outbox *realtime.PostgresOutboxStore,
) *PostgresProjectService {
	return &PostgresProjectService{pool: pool, authz: authz, hooks: hooks, cipher: cipher, outbox: outbox}
}

func (s *PostgresProjectService) ListProjects(ctx context.Context, principal auth.Principal, teamID int64) ([]Project, error) {
	if err := s.authz.CheckList(ctx, principal, teamID, "projects"); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, uuid, name, COALESCE(description, ''), team_id
		FROM projects
		WHERE team_id = $1
		ORDER BY id ASC
	`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all := make([]Project, 0)
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.ID, &p.UUID, &p.Name, &p.Description, &p.TeamID); err != nil {
			return nil, err
		}
		all = append(all, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	envByProj, resByProj := s.loadProjectHierarchyUUIDs(ctx, teamID)
	out := make([]Project, 0, len(all))
	for _, p := range all {
		if s.authz.CanOpenProject(ctx, principal, p.TeamID, p.UUID, envByProj[p.ID], resByProj[p.ID]) == nil {
			out = append(out, p)
		}
	}
	return out, nil
}

func (s *PostgresProjectService) GetProject(ctx context.Context, principal auth.Principal, projectUUID string) (Project, []Environment, error) {
	var p Project
	err := s.pool.QueryRow(ctx, `SELECT id, uuid, name, COALESCE(description, ''), team_id FROM projects WHERE uuid = $1`, projectUUID).
		Scan(&p.ID, &p.UUID, &p.Name, &p.Description, &p.TeamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, nil, ErrNotFound
	}
	if err != nil {
		return Project{}, nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id, uuid, name, project_id, COALESCE(description,'') FROM environments WHERE project_id = $1 ORDER BY id ASC`, p.ID)
	if err != nil {
		return Project{}, nil, err
	}
	defer rows.Close()
	envs := make([]Environment, 0)
	envUUIDs := make([]string, 0)
	for rows.Next() {
		var e Environment
		e.TeamID = p.TeamID
		if err := rows.Scan(&e.ID, &e.UUID, &e.Name, &e.ProjectID, &e.Description); err != nil {
			return Project{}, nil, err
		}
		envs = append(envs, e)
		envUUIDs = append(envUUIDs, e.UUID)
	}
	if err := rows.Err(); err != nil {
		return Project{}, nil, err
	}
	_, resByProj := s.loadProjectHierarchyUUIDs(ctx, p.TeamID)
	if err := s.authz.CanOpenProject(ctx, principal, p.TeamID, p.UUID, envUUIDs, resByProj[p.ID]); err != nil {
		return Project{}, nil, err
	}
	return p, envs, nil
}

func (s *PostgresProjectService) loadProjectHierarchyUUIDs(ctx context.Context, teamID int64) (map[int64][]string, map[int64][]string) {
	envByProj := make(map[int64][]string)
	resByProj := make(map[int64][]string)
	if erows, err := s.pool.Query(ctx, `
		SELECT e.project_id, e.uuid
		FROM environments e
		JOIN projects p ON p.id = e.project_id
		WHERE p.team_id = $1
	`, teamID); err == nil {
		defer erows.Close()
		for erows.Next() {
			var pid int64
			var euuid string
			if erows.Scan(&pid, &euuid) == nil {
				envByProj[pid] = append(envByProj[pid], euuid)
			}
		}
	}
	for _, q := range []string{
		`SELECT e.project_id, a.uuid FROM applications a JOIN environments e ON e.id = a.environment_id JOIN projects p ON p.id = e.project_id WHERE p.team_id = $1`,
		`SELECT e.project_id, sv.uuid FROM services sv JOIN environments e ON e.id = sv.environment_id JOIN projects p ON p.id = e.project_id WHERE p.team_id = $1`,
		`SELECT e.project_id, d.uuid FROM standalone_postgresqls d JOIN environments e ON e.id = d.environment_id JOIN projects p ON p.id = e.project_id WHERE p.team_id = $1 AND d.deleted_at IS NULL`,
		`SELECT e.project_id, d.uuid FROM standalone_mysqls d JOIN environments e ON e.id = d.environment_id JOIN projects p ON p.id = e.project_id WHERE p.team_id = $1 AND d.deleted_at IS NULL`,
		`SELECT e.project_id, d.uuid FROM standalone_mariadbs d JOIN environments e ON e.id = d.environment_id JOIN projects p ON p.id = e.project_id WHERE p.team_id = $1 AND d.deleted_at IS NULL`,
		`SELECT e.project_id, d.uuid FROM standalone_mongodbs d JOIN environments e ON e.id = d.environment_id JOIN projects p ON p.id = e.project_id WHERE p.team_id = $1 AND d.deleted_at IS NULL`,
		`SELECT e.project_id, d.uuid FROM standalone_redis d JOIN environments e ON e.id = d.environment_id JOIN projects p ON p.id = e.project_id WHERE p.team_id = $1 AND d.deleted_at IS NULL`,
		`SELECT e.project_id, d.uuid FROM standalone_keydbs d JOIN environments e ON e.id = d.environment_id JOIN projects p ON p.id = e.project_id WHERE p.team_id = $1 AND d.deleted_at IS NULL`,
		`SELECT e.project_id, d.uuid FROM standalone_dragonflies d JOIN environments e ON e.id = d.environment_id JOIN projects p ON p.id = e.project_id WHERE p.team_id = $1 AND d.deleted_at IS NULL`,
		`SELECT e.project_id, d.uuid FROM standalone_clickhouses d JOIN environments e ON e.id = d.environment_id JOIN projects p ON p.id = e.project_id WHERE p.team_id = $1 AND d.deleted_at IS NULL`,
		`SELECT e.project_id, d.uuid FROM standalone_sqlites d JOIN environments e ON e.id = d.environment_id JOIN projects p ON p.id = e.project_id WHERE p.team_id = $1 AND d.deleted_at IS NULL`,
	} {
		if rrows, err := s.pool.Query(ctx, q, teamID); err == nil {
			for rrows.Next() {
				var pid int64
				var ruuid string
				if rrows.Scan(&pid, &ruuid) == nil {
					resByProj[pid] = append(resByProj[pid], ruuid)
				}
			}
			rrows.Close()
		}
	}
	return envByProj, resByProj
}

func (s *PostgresProjectService) CreateProject(ctx context.Context, principal auth.Principal, input CreateProjectInput) (Project, Environment, error) {
	teamID := input.TeamID
	if err := s.authz.Check(ctx, principal, access.ActionCreate, access.ResourceRef{
		Type: "project", TeamID: &teamID, APISegment: "projects",
	}); err != nil {
		return Project{}, Environment{}, err
	}
	name := compat.SanitizeString(input.Name)
	if name == "" {
		return Project{}, Environment{}, errors.New("project name is required")
	}
	projRow := compat.ModelRow{Name: name}
	_ = s.hooks.ApplyModelDefaults("project", &projRow)
	envRow := compat.ModelRow{Name: "production"}
	_ = s.hooks.ApplyModelDefaults("environment", &envRow)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Project{}, Environment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var p Project
	err = tx.QueryRow(ctx, `
		INSERT INTO projects (uuid, name, description, team_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id, uuid, name, COALESCE(description, ''), team_id
	`, projRow.UUID, name, compat.SanitizeString(input.Description), teamID).
		Scan(&p.ID, &p.UUID, &p.Name, &p.Description, &p.TeamID)
	if err != nil {
		return Project{}, Environment{}, err
	}
	p.Revision = 1

	var e Environment
	err = tx.QueryRow(ctx, `
		INSERT INTO environments (uuid, name, project_id)
		VALUES ($1, 'production', $2)
		RETURNING id, uuid, name, project_id
	`, envRow.UUID, p.ID).Scan(&e.ID, &e.UUID, &e.Name, &e.ProjectID)
	if err != nil {
		return Project{}, Environment{}, err
	}
	e.TeamID = teamID

	if s.outbox != nil {
		if err := s.outbox.AppendInTx(ctx, tx, realtime.DomainEvent{
			EventID:           fmt.Sprintf("proj.created-%s-%d", p.UUID, time.Now().UnixNano()),
			SchemaVersion:     "v1",
			Producer:          "go-controlplane",
			TeamID:            teamID,
			ResourceType:      "project",
			ResourceUUID:      p.UUID,
			AggregateRevision: 1,
			OwnershipEpoch:    1,
			MutationType:      "project.updated",
			CommittedAt:       time.Now().UTC(),
			Payload:           map[string]any{"uuid": p.UUID, "name": p.Name},
		}); err != nil {
			return Project{}, Environment{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Project{}, Environment{}, err
	}
	return p, e, nil
}

func (s *PostgresProjectService) CreateEnvironment(ctx context.Context, principal auth.Principal, projectUUID string, envName string, descriptions ...string) (Environment, error) {
	var p Project
	err := s.pool.QueryRow(ctx, `SELECT id, uuid, name, team_id FROM projects WHERE uuid = $1`, projectUUID).
		Scan(&p.ID, &p.UUID, &p.Name, &p.TeamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Environment{}, ErrNotFound
	}
	if err != nil {
		return Environment{}, err
	}
	teamID := p.TeamID
	if err := s.authz.Check(ctx, principal, access.ActionUpdate, access.ResourceRef{
		Type: "project", ID: p.ID, UUID: p.UUID, ProjectUUID: p.UUID, TeamID: &teamID, APISegment: "projects",
	}); err != nil {
		return Environment{}, err
	}
	clean := strings.TrimSpace(envName)
	if clean == "" {
		return Environment{}, errors.New("environment name is required")
	}
	envRow := compat.ModelRow{Name: clean}
	_ = s.hooks.ApplyModelDefaults("environment", &envRow)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Environment{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var e Environment
	if len(descriptions) > 0 {
		e.Description = compat.SanitizeString(descriptions[0])
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO environments (uuid, name, project_id, description)
		VALUES ($1, $2, $3, $4)
		RETURNING id, uuid, name, project_id
	`, envRow.UUID, clean, p.ID, e.Description).Scan(&e.ID, &e.UUID, &e.Name, &e.ProjectID)
	if err != nil {
		return Environment{}, err
	}
	e.TeamID = teamID

	if s.outbox != nil {
		if err := s.outbox.AppendInTx(ctx, tx, realtime.DomainEvent{
			EventID:           fmt.Sprintf("env.created-%s-%d", e.UUID, time.Now().UnixNano()),
			SchemaVersion:     "v1",
			Producer:          "go-controlplane",
			TeamID:            teamID,
			ResourceType:      "environment",
			ResourceUUID:      e.UUID,
			AggregateRevision: 1,
			OwnershipEpoch:    1,
			MutationType:      "environment.updated",
			CommittedAt:       time.Now().UTC(),
			Payload:           map[string]any{"uuid": e.UUID, "name": e.Name, "project_uuid": p.UUID},
		}); err != nil {
			return Environment{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Environment{}, err
	}
	return e, nil
}

func (s *PostgresProjectService) DeleteProject(ctx context.Context, principal auth.Principal, projectUUID string) error {
	var p Project
	err := s.pool.QueryRow(ctx, `SELECT id, uuid, team_id FROM projects WHERE uuid = $1`, projectUUID).Scan(&p.ID, &p.UUID, &p.TeamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	teamID := p.TeamID
	if err := s.authz.Check(ctx, principal, access.ActionDelete, access.ResourceRef{
		Type: "project", ID: p.ID, UUID: p.UUID, ProjectUUID: p.UUID, TeamID: &teamID, APISegment: "projects",
	}); err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, _ = tx.Exec(ctx, `DELETE FROM environments WHERE project_id = $1`, p.ID)
	if _, err := tx.Exec(ctx, `DELETE FROM projects WHERE id = $1`, p.ID); err != nil {
		return err
	}
	if s.outbox != nil {
		_ = s.outbox.AppendInTx(ctx, tx, realtime.DomainEvent{
			EventID:           fmt.Sprintf("proj.deleted-%s-%d", p.UUID, time.Now().UnixNano()),
			SchemaVersion:     "v1",
			Producer:          "go-controlplane",
			TeamID:            teamID,
			ResourceType:      "project",
			ResourceUUID:      p.UUID,
			AggregateRevision: 1,
			OwnershipEpoch:    1,
			MutationType:      "project.updated",
			CommittedAt:       time.Now().UTC(),
		})
	}
	return tx.Commit(ctx)
}

func (s *PostgresProjectService) UpsertSharedVariable(ctx context.Context, principal auth.Principal, v SharedVariable) (SharedVariable, error) {
	teamID := v.TeamID
	if err := s.authz.Check(ctx, principal, access.ActionUpdate, access.ResourceRef{
		Type: "project", TeamID: &teamID, APISegment: "projects",
	}); err != nil {
		return SharedVariable{}, err
	}
	if strings.TrimSpace(v.Key) == "" {
		return SharedVariable{}, errors.New("key is required")
	}
	if v.UUID == "" {
		v.UUID = s.hooks.NewCUID2()
	}
	storedVal := v.Value
	if s.cipher != nil {
		enc, err := s.cipher.EncryptLaravelPayload([]byte(v.Value), false)
		if err != nil {
			return SharedVariable{}, err
		}
		storedVal = enc
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO shared_environment_variables (uuid, key, value, type, is_shown_once, team_id, project_id, environment_id, server_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id
	`, v.UUID, v.Key, storedVal, v.Type, v.IsShownOnce, v.TeamID, v.ProjectID, v.EnvironmentID, v.ServerID).Scan(&v.ID)
	if err != nil {
		return SharedVariable{}, err
	}
	if v.IsShownOnce {
		v.Value = ""
	}
	return v, nil
}
