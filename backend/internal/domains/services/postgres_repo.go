package services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/compose"
	"github.com/coollabsio/coolify/backend/internal/domains/deployments"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/coollabsio/coolify/backend/internal/realtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("service not found")

type PostgresServiceManager struct {
	pool     *pgxpool.Pool
	authz    *access.Authorizer
	hooks    *compat.EloquentHooks
	parser   *compose.ComposeParser
	outbox   *realtime.PostgresOutboxStore
	jobCoord *jobs.PostgresJobCoordinator
}

func NewPostgresServiceManager(
	pool *pgxpool.Pool,
	authz *access.Authorizer,
	hooks *compat.EloquentHooks,
	parser *compose.ComposeParser,
	outbox *realtime.PostgresOutboxStore,
	jobCoord *jobs.PostgresJobCoordinator,
) *PostgresServiceManager {
	return &PostgresServiceManager{
		pool: pool, authz: authz, hooks: hooks, parser: parser, outbox: outbox, jobCoord: jobCoord,
	}
}

type PersistedService struct {
	ID               int64     `json:"id"`
	UUID             string    `json:"uuid"`
	Name             string    `json:"name"`
	Description      string    `json:"description,omitempty"`
	DockerComposeRaw string    `json:"docker_compose_raw"`
	DockerCompose    string    `json:"docker_compose,omitempty"`
	FQDN             string    `json:"fqdn,omitempty"`
	Status           string    `json:"status"`
	EnvironmentID    int64     `json:"environment_id"`
	EnvironmentUUID  string    `json:"environment_uuid,omitempty"`
	ProjectUUID      string    `json:"project_uuid,omitempty"`
	ServerID         int64     `json:"server_id"`
	ServerUUID       string    `json:"server_uuid"`
	TeamID           int64     `json:"team_id"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (m *PostgresServiceManager) Create(ctx context.Context, principal auth.Principal, envUUID string, serverUUID string, name string, rawCompose string, fqdn string) (PersistedService, error) {
	var envID, teamID int64
	var projUUID string
	if err := m.pool.QueryRow(ctx, `
		SELECT e.id, p.uuid, p.team_id FROM environments e JOIN projects p ON p.id = e.project_id WHERE e.uuid = $1
	`, envUUID).Scan(&envID, &projUUID, &teamID); err != nil {
		return PersistedService{}, errors.New("environment not found")
	}
	if err := m.authz.Check(ctx, principal, access.ActionCreate, access.ResourceRef{
		Type: "service", ProjectUUID: projUUID, EnvironmentUUID: envUUID, TeamID: &teamID, APISegment: "services",
	}); err != nil {
		return PersistedService{}, err
	}
	if strings.TrimSpace(serverUUID) == "" {
		serverUUID = "localhost"
	}
	var srvID, srvTeamID int64
	if err := m.pool.QueryRow(ctx, `SELECT id, team_id FROM servers WHERE uuid = $1 AND deleted_at IS NULL`, serverUUID).Scan(&srvID, &srvTeamID); err != nil {
		return PersistedService{}, errors.New("server not found")
	}
	if srvTeamID != teamID {
		return PersistedService{}, fmt.Errorf("%w: cross-team server placement denied", access.ErrForbidden)
	}
	if err := m.authz.Check(ctx, principal, access.ActionPlaceResource, access.ResourceRef{
		Type: "server", ID: srvID, UUID: serverUUID, TeamID: &srvTeamID, IsLocalhost: srvID == 0, APISegment: "servers",
	}); err != nil {
		return PersistedService{}, err
	}
	row := compat.ModelRow{Name: compat.SanitizeString(name)}
	_ = m.hooks.ApplyModelDefaults("service", &row)
	rawCompose = DecodeComposeInput(rawCompose)
	parsed, err := m.parser.Parse(ctx, compose.ComposeParseInput{
		ParserVersion: "5", RawCompose: rawCompose, ResourceUUID: row.UUID, TeamID: teamID, ProxyType: "traefik", FQDN: fqdn,
	})
	if err != nil {
		return PersistedService{}, err
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return PersistedService{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var svc PersistedService
	err = tx.QueryRow(ctx, `
		INSERT INTO services (uuid, name, docker_compose_raw, docker_compose, fqdn, status, environment_id, server_id)
		VALUES ($1,$2,$3,$4,$5,'exited',$6,$7)
		RETURNING id, uuid, name, docker_compose_raw, COALESCE(docker_compose,''), COALESCE(fqdn,''), status, environment_id, server_id, updated_at
	`, row.UUID, row.Name, rawCompose, parsed.RenderedYAML, fqdn, envID, srvID).Scan(
		&svc.ID, &svc.UUID, &svc.Name, &svc.DockerComposeRaw, &svc.DockerCompose, &svc.FQDN,
		&svc.Status, &svc.EnvironmentID, &svc.ServerID, &svc.UpdatedAt,
	)
	if err != nil {
		return PersistedService{}, err
	}
	svc.ServerUUID = serverUUID
	svc.TeamID = teamID
	if err := m.syncServiceSubResourcesInTx(ctx, tx, svc.ID, fqdn, parsed); err != nil {
		return PersistedService{}, err
	}
	if m.outbox != nil {
		_ = m.outbox.AppendInTx(ctx, tx, realtime.DomainEvent{
			EventID: fmt.Sprintf("svc.created-%s-%d", svc.UUID, svc.UpdatedAt.UnixNano()), SchemaVersion: "v1",
			Producer: "go-controlplane", TeamID: teamID, ResourceType: "service", ResourceUUID: svc.UUID,
			AggregateRevision: 1, OwnershipEpoch: 1, MutationType: "service.updated", CommittedAt: svc.UpdatedAt.UTC(),
		})
	}
	return svc, tx.Commit(ctx)
}

func (m *PostgresServiceManager) UpdateCompose(ctx context.Context, principal auth.Principal, serviceUUID string, rawCompose string) (PersistedService, error) {
	svc, err := m.loadByUUID(ctx, serviceUUID)
	if err != nil {
		return PersistedService{}, err
	}
	teamID := svc.TeamID
	if err := m.authz.Check(ctx, principal, access.ActionUpdate, access.ResourceRef{
		Type: "service", ID: svc.ID, UUID: svc.UUID, ProjectUUID: svc.ProjectUUID, EnvironmentUUID: svc.EnvironmentUUID, TeamID: &teamID, ServerID: &svc.ServerID, APISegment: "services",
	}); err != nil {
		return PersistedService{}, err
	}
	rawCompose = DecodeComposeInput(rawCompose)
	parsed, err := m.parser.Parse(ctx, compose.ComposeParseInput{
		ParserVersion: "5", RawCompose: rawCompose, ResourceUUID: svc.UUID, TeamID: svc.TeamID, ProxyType: "traefik", FQDN: svc.FQDN,
	})
	if err != nil {
		return PersistedService{}, err
	}
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return PersistedService{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := tx.QueryRow(ctx, `
		UPDATE services SET docker_compose_raw = $2, docker_compose = $3, updated_at = NOW()
		WHERE id = $1 RETURNING updated_at
	`, svc.ID, rawCompose, parsed.RenderedYAML).Scan(&svc.UpdatedAt); err != nil {
		return PersistedService{}, err
	}
	svc.DockerComposeRaw = rawCompose
	svc.DockerCompose = parsed.RenderedYAML
	if err := m.syncServiceSubResourcesInTx(ctx, tx, svc.ID, svc.FQDN, parsed); err != nil {
		return PersistedService{}, err
	}
	if m.outbox != nil {
		_ = m.outbox.AppendInTx(ctx, tx, realtime.DomainEvent{
			EventID: fmt.Sprintf("svc.updated-%s-%d", svc.UUID, svc.UpdatedAt.UnixNano()), SchemaVersion: "v1",
			Producer: "go-controlplane", TeamID: svc.TeamID, ResourceType: "service", ResourceUUID: svc.UUID,
			AggregateRevision: 1, OwnershipEpoch: 1, MutationType: "service.updated", CommittedAt: svc.UpdatedAt.UTC(),
		})
	}
	return svc, tx.Commit(ctx)
}

func (m *PostgresServiceManager) syncServiceSubResourcesInTx(ctx context.Context, tx pgx.Tx, serviceID int64, defaultFQDN string, parsed compose.ComposeParseOutput) error {
	appNames := make([]string, 0, len(parsed.Applications))
	for name := range parsed.Applications {
		appNames = append(appNames, name)
	}
	sort.Strings(appNames)
	for _, appName := range appNames {
		spec := parsed.Applications[appName]
		var existingID int64
		err := tx.QueryRow(ctx, `SELECT id FROM service_applications WHERE service_id = $1 AND name = $2 AND deleted_at IS NULL`, serviceID, appName).Scan(&existingID)
		if errors.Is(err, pgx.ErrNoRows) {
			fqdnVal := spec.FQDN
			if fqdnVal == "" {
				fqdnVal = defaultFQDN
			}
			portsVal := strings.Join(spec.Ports, ",")
			if _, iErr := tx.Exec(ctx, `
				INSERT INTO service_applications (uuid, name, human_name, fqdn, ports, status, service_id, exclude_from_status)
				VALUES ($1, $2, $3, NULLIF($4,''), NULLIF($5,''), 'exited', $6, $7)
			`, m.hooks.NewCUID2(), appName, appName, fqdnVal, portsVal, serviceID, spec.ExcludeFromStatus); iErr != nil {
				return iErr
			}
		} else if err != nil {
			return err
		}
	}

	dbNames := make([]string, 0, len(parsed.Databases))
	for name := range parsed.Databases {
		dbNames = append(dbNames, name)
	}
	sort.Strings(dbNames)
	for _, dbName := range dbNames {
		spec := parsed.Databases[dbName]
		var existingID int64
		err := tx.QueryRow(ctx, `SELECT id FROM service_databases WHERE service_id = $1 AND name = $2 AND deleted_at IS NULL`, serviceID, dbName).Scan(&existingID)
		if errors.Is(err, pgx.ErrNoRows) {
			portsVal := strings.Join(spec.Ports, ",")
			if _, iErr := tx.Exec(ctx, `
				INSERT INTO service_databases (uuid, name, human_name, ports, status, service_id, exclude_from_status)
				VALUES ($1, $2, $3, NULLIF($4,''), 'exited', $5, $6)
			`, m.hooks.NewCUID2(), dbName, dbName, portsVal, serviceID, spec.ExcludeFromStatus); iErr != nil {
				return iErr
			}
		} else if err != nil {
			return err
		}
	}
	return nil
}

// EnqueueDeploy parses the service compose and enqueues a REAL durable `service_deploy` job for the Go worker.
// It NEVER returns a fake "finished" status without real execution.
func (m *PostgresServiceManager) EnqueueDeploy(ctx context.Context, principal auth.Principal, serviceUUID string) (deployments.DeploymentReceipt, error) {
	svc, err := m.loadByUUID(ctx, serviceUUID)
	if err != nil {
		return deployments.DeploymentReceipt{}, err
	}
	teamID := svc.TeamID
	if err := m.authz.Check(ctx, principal, access.ActionDeploy, access.ResourceRef{
		Type: "service", ID: svc.ID, UUID: svc.UUID, ProjectUUID: svc.ProjectUUID, EnvironmentUUID: svc.EnvironmentUUID, TeamID: &teamID, ServerID: &svc.ServerID, APISegment: "services",
	}); err != nil {
		return deployments.DeploymentReceipt{}, err
	}
	parsed, err := m.parser.Parse(ctx, compose.ComposeParseInput{
		ParserVersion: "5", RawCompose: svc.DockerComposeRaw, ResourceUUID: svc.UUID, TeamID: svc.TeamID, ProxyType: "traefik", FQDN: svc.FQDN,
	})
	if err != nil {
		return deployments.DeploymentReceipt{}, err
	}

	opID := "svc-deploy-" + m.hooks.NewCUID2()
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return deployments.DeploymentReceipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `UPDATE services SET docker_compose = $2, status = 'starting', updated_at = NOW() WHERE id = $1`, svc.ID, parsed.RenderedYAML); err != nil {
		return deployments.DeploymentReceipt{}, err
	}
	if err := m.jobCoord.AdmitOperationInTx(ctx, tx, jobs.DurableJobSpec{
		JobOperation: jobs.JobOperation{
			JobID: "job-" + opID, OperationID: opID, AttemptID: "att-0",
			ResourceUUID: svc.UUID, ServerUUID: svc.ServerUUID, TeamID: svc.TeamID, Status: jobs.StatusQueued, MaxAttempts: 3,
		},
		JobType: "service_deploy",
		Payload: map[string]any{"service_uuid": svc.UUID, "service_id": svc.ID, "rendered_yaml": parsed.RenderedYAML},
	}); err != nil {
		return deployments.DeploymentReceipt{}, err
	}
	if m.outbox != nil {
		_ = m.outbox.AppendInTx(ctx, tx, realtime.DomainEvent{
			EventID: "svc.queued-" + opID, SchemaVersion: "v1", Producer: "go-controlplane",
			TeamID: svc.TeamID, ResourceType: "service", ResourceUUID: svc.UUID,
			AggregateRevision: 1, OwnershipEpoch: 1, MutationType: "service.status_changed", CommittedAt: time.Now().UTC(),
			Payload: map[string]any{"status": "queued", "operation_id": opID},
		})
	}
	if err := tx.Commit(ctx); err != nil {
		return deployments.DeploymentReceipt{}, err
	}
	return deployments.DeploymentReceipt{
		DeploymentUUID: opID,
		OperationID:    opID,
		Status:         "queued",
		PreservedPrior: true,
	}, nil
}

func (m *PostgresServiceManager) ListByTeam(ctx context.Context, principal auth.Principal, teamID int64) ([]PersistedService, error) {
	if err := m.authz.CheckList(ctx, principal, teamID, "services"); err != nil {
		return nil, err
	}
	rows, err := m.pool.Query(ctx, `
		SELECT s.id, s.uuid, s.name, COALESCE(s.description,''), s.docker_compose_raw, COALESCE(s.docker_compose,''),
		       COALESCE(s.fqdn,''), s.status, s.environment_id, e.uuid, p.uuid, s.server_id, COALESCE(srv.uuid,'localhost'), p.team_id, s.updated_at
		FROM services s
		JOIN environments e ON e.id = s.environment_id
		JOIN projects p ON p.id = e.project_id
		LEFT JOIN servers srv ON srv.id = s.server_id
		WHERE p.team_id = $1
		ORDER BY s.id ASC
	`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PersistedService, 0)
	for rows.Next() {
		var svc PersistedService
		if err := rows.Scan(
			&svc.ID, &svc.UUID, &svc.Name, &svc.Description, &svc.DockerComposeRaw, &svc.DockerCompose,
			&svc.FQDN, &svc.Status, &svc.EnvironmentID, &svc.EnvironmentUUID, &svc.ProjectUUID, &svc.ServerID, &svc.ServerUUID, &svc.TeamID, &svc.UpdatedAt,
		); err != nil {
			return nil, err
		}
		tid := svc.TeamID
		sid := svc.ServerID
		if m.authz.Check(ctx, principal, access.ActionRead, access.ResourceRef{
			Type: "service", ID: svc.ID, UUID: svc.UUID, ProjectUUID: svc.ProjectUUID, EnvironmentUUID: svc.EnvironmentUUID,
			TeamID: &tid, ServerID: &sid, APISegment: "services",
		}) != nil {
			continue
		}
		if m.authz.Check(ctx, principal, access.ActionReadSensitive, access.ResourceRef{Type: "service", ID: svc.ID, UUID: svc.UUID, ProjectUUID: svc.ProjectUUID, EnvironmentUUID: svc.EnvironmentUUID, TeamID: &tid, ServerID: &sid, APISegment: "services"}) != nil {
			svc.DockerComposeRaw = ""
			svc.DockerCompose = ""
		}
		out = append(out, svc)
	}
	return out, rows.Err()
}

func (m *PostgresServiceManager) GetByUUID(ctx context.Context, principal auth.Principal, uuid string) (PersistedService, error) {
	svc, err := m.loadByUUID(ctx, uuid)
	if err != nil {
		return PersistedService{}, err
	}
	teamID := svc.TeamID
	ref := access.ResourceRef{Type: "service", ID: svc.ID, UUID: svc.UUID, ProjectUUID: svc.ProjectUUID, EnvironmentUUID: svc.EnvironmentUUID, TeamID: &teamID, ServerID: &svc.ServerID, APISegment: "services"}
	if err := m.authz.Check(ctx, principal, access.ActionRead, ref); err != nil {
		return PersistedService{}, err
	}
	if m.authz.Check(ctx, principal, access.ActionReadSensitive, ref) != nil {
		svc.DockerComposeRaw = ""
		svc.DockerCompose = ""
	}
	return svc, nil
}

func (m *PostgresServiceManager) loadByUUID(ctx context.Context, uuid string) (PersistedService, error) {
	var svc PersistedService
	err := m.pool.QueryRow(ctx, `
		SELECT s.id, s.uuid, s.name, COALESCE(s.description,''), s.docker_compose_raw, COALESCE(s.docker_compose,''),
		       COALESCE(s.fqdn,''), s.status, s.environment_id, e.uuid, p.uuid, s.server_id, COALESCE(srv.uuid,'localhost'), p.team_id, s.updated_at
		FROM services s
		JOIN environments e ON e.id = s.environment_id
		JOIN projects p ON p.id = e.project_id
		LEFT JOIN servers srv ON srv.id = s.server_id
		WHERE s.uuid = $1
	`, uuid).Scan(
		&svc.ID, &svc.UUID, &svc.Name, &svc.Description, &svc.DockerComposeRaw, &svc.DockerCompose,
		&svc.FQDN, &svc.Status, &svc.EnvironmentID, &svc.EnvironmentUUID, &svc.ProjectUUID, &svc.ServerID, &svc.ServerUUID, &svc.TeamID, &svc.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return PersistedService{}, ErrNotFound
	}
	return svc, err
}
