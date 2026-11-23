package databases

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/coollabsio/coolify/backend/internal/realtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound          = errors.New("database not found")
	ErrCannotDeleteRoot  = errors.New("cannot delete sentinel instance database coolify-db (id=0)")
)

type PersistedDatabase struct {
	ID              int64     `json:"id"`
	UUID            string    `json:"uuid"`
	Name            string    `json:"name"`
	Description     string    `json:"description,omitempty"`
	Type            string    `json:"type"`
	Image           string    `json:"image"`
	IsPublic        bool      `json:"is_public"`
	CustomPort      int       `json:"custom_port"`
	PublicPort      int       `json:"public_port"`
	Status          string    `json:"status"`
	TeamID          int64     `json:"team_id"`
	EnvironmentID   int64     `json:"environment_id"`
	EnvironmentUUID string    `json:"environment_uuid,omitempty"`
	ProjectUUID     string    `json:"project_uuid,omitempty"`
	ServerUUID      string    `json:"server_uuid"`
	ServerIP        string    `json:"-"`
	IsInstanceDB    bool      `json:"is_instance_db"`
	Password        string    `json:"password,omitempty"`        // Only populated when ActionReadSensitive succeeds
	InternalDBURL   string    `json:"internal_db_url,omitempty"` // Only populated when ActionReadSensitive succeeds
	ExternalDBURL   string    `json:"external_db_url,omitempty"` // Only populated when is_public && ActionReadSensitive succeeds
	UpdatedAt       time.Time `json:"updated_at"`
}

type DatabaseUpdateInput struct {
	Name           *string `json:"name"`
	Description    *string `json:"description"`
	Image          *string `json:"image"`
	IsPublic       *bool   `json:"is_public"`
	CustomPort     *int    `json:"custom_port"`
	PublicPort     *int    `json:"public_port"`
	Password       *string `json:"password"`
	RotatePassword bool    `json:"rotate_password"`
}

type PostgresDatabaseManager struct {
	pool     *pgxpool.Pool
	authz    *access.Authorizer
	hooks    *compat.EloquentHooks
	cipher   *compat.Cipher
	outbox   *realtime.PostgresOutboxStore
	jobCoord *jobs.PostgresJobCoordinator
}

func NewPostgresDatabaseManager(
	pool *pgxpool.Pool,
	authz *access.Authorizer,
	hooks *compat.EloquentHooks,
	cipher *compat.Cipher,
	outbox *realtime.PostgresOutboxStore,
	jobCoord *jobs.PostgresJobCoordinator,
) *PostgresDatabaseManager {
	return &PostgresDatabaseManager{
		pool: pool, authz: authz, hooks: hooks, cipher: cipher, outbox: outbox, jobCoord: jobCoord,
	}
}

type dbTableSpec struct {
	dbType        string
	table         string
	passCol       string
	hasPublicPort bool
}

var allDBSpecs = []dbTableSpec{
	{dbType: "postgresql", table: "standalone_postgresqls", passCol: "postgres_password", hasPublicPort: true},
	{dbType: "mysql", table: "standalone_mysqls", passCol: "mysql_password", hasPublicPort: true},
	{dbType: "mariadb", table: "standalone_mariadbs", passCol: "mariadb_password", hasPublicPort: true},
	{dbType: "mongodb", table: "standalone_mongodbs", passCol: "mongo_initdb_root_password", hasPublicPort: true},
	{dbType: "redis", table: "standalone_redis", passCol: "redis_password", hasPublicPort: true},
	{dbType: "keydb", table: "standalone_keydbs", passCol: "keydb_password", hasPublicPort: true},
	{dbType: "dragonfly", table: "standalone_dragonflies", passCol: "dragonfly_password", hasPublicPort: true},
	{dbType: "clickhouse", table: "standalone_clickhouses", passCol: "clickhouse_admin_password", hasPublicPort: true},
	{dbType: "sqlite", table: "standalone_sqlites", passCol: "", hasPublicPort: false},
}

func specForDBType(dbType string) (dbTableSpec, bool) {
	for _, s := range allDBSpecs {
		if s.dbType == dbType {
			return s, true
		}
	}
	return dbTableSpec{}, false
}

func (m *PostgresDatabaseManager) Create(ctx context.Context, principal auth.Principal, teamID int64, name string, dbType string, image string, plainPassword string, customPort int, serverUUID string, envUUIDs ...string) (PersistedDatabase, error) {
	spec, ok := specForDBType(dbType)
	if !ok || !slices.Contains(SupportedDatabaseTypes, dbType) {
		return PersistedDatabase{}, fmt.Errorf("unsupported database type %q", dbType)
	}
	var envIDPtr *int64
	var envUUID, projUUID string
	if len(envUUIDs) > 0 && strings.TrimSpace(envUUIDs[0]) != "" {
		envUUID = strings.TrimSpace(envUUIDs[0])
		var eID, pTeamID int64
		err := m.pool.QueryRow(ctx, `
			SELECT e.id, p.uuid, p.team_id
			FROM environments e
			JOIN projects p ON p.id = e.project_id
			WHERE e.uuid = $1
		`, envUUID).Scan(&eID, &projUUID, &pTeamID)
		if errors.Is(err, pgx.ErrNoRows) {
			return PersistedDatabase{}, errors.New("environment not found")
		}
		if err != nil {
			return PersistedDatabase{}, err
		}
		if pTeamID != teamID {
			return PersistedDatabase{}, fmt.Errorf("%w: cross-team environment placement denied", access.ErrForbidden)
		}
		envIDPtr = &eID
	}
	if err := m.authz.Check(ctx, principal, access.ActionCreate, access.ResourceRef{
		Type: "database", ProjectUUID: projUUID, EnvironmentUUID: envUUID, TeamID: &teamID, APISegment: "databases",
	}); err != nil {
		return PersistedDatabase{}, err
	}
	cleanName := compat.SanitizeString(name)
	if cleanName == "" {
		return PersistedDatabase{}, errors.New("database name is required")
	}
	if strings.TrimSpace(serverUUID) == "" {
		serverUUID = "localhost"
	}
	var srvID, srvTeamID, destID int64
	if err := m.pool.QueryRow(ctx, `
		SELECT s.id, s.team_id, COALESCE(sd.id, -1)
		FROM servers s
		LEFT JOIN standalone_dockers sd ON sd.server_id = s.id
		WHERE s.uuid = $1 AND s.deleted_at IS NULL
		ORDER BY sd.id ASC
		LIMIT 1
	`, serverUUID).Scan(&srvID, &srvTeamID, &destID); err != nil {
		return PersistedDatabase{}, errors.New("server not found")
	}
	if srvTeamID != teamID {
		return PersistedDatabase{}, fmt.Errorf("%w: cross-team server placement denied", access.ErrForbidden)
	}
	if err := m.authz.Check(ctx, principal, access.ActionPlaceResource, access.ResourceRef{
		Type: "server", ID: srvID, UUID: serverUUID, TeamID: &srvTeamID, IsLocalhost: srvID == 0, APISegment: "servers",
	}); err != nil {
		return PersistedDatabase{}, err
	}
	if destID < 0 {
		if srvID == 0 {
			destID = 0
		} else {
			destUUID := "dest-" + m.hooks.NewCUID2()
			if err := m.pool.QueryRow(ctx, `
				INSERT INTO standalone_dockers (uuid, name, network, server_id)
				VALUES ($1, 'standalone-docker', 'coolify', $2) RETURNING id
			`, destUUID, srvID).Scan(&destID); err != nil {
				return PersistedDatabase{}, err
			}
		}
	}

	if strings.TrimSpace(plainPassword) == "" && spec.passCol != "" {
		plainPassword = generateSecurePassword()
	}
	encPass := plainPassword
	if m.cipher != nil && plainPassword != "" {
		enc, err := m.cipher.EncryptLaravelPayload([]byte(plainPassword), false)
		if err != nil {
			return PersistedDatabase{}, err
		}
		encPass = enc
	}
	uuid := m.hooks.NewCUID2()
	resolvedImg := defaultImage(dbType, image)
	var portPtr *int
	if customPort > 0 {
		pVal := customPort
		portPtr = &pVal
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return PersistedDatabase{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var dbID int64
	var updatedAt time.Time
	switch dbType {
	case "postgresql":
		err = tx.QueryRow(ctx, `
			INSERT INTO standalone_postgresqls (uuid, name, postgres_user, postgres_password, postgres_db, image, public_port, status, environment_id, destination_id, destination_type)
			VALUES ($1,$2,'postgres',$3,'postgres',$4,$5,'exited',$6,$7,'App\Models\StandaloneDocker')
			RETURNING id, updated_at
		`, uuid, cleanName, encPass, resolvedImg, portPtr, envIDPtr, destID).Scan(&dbID, &updatedAt)
	case "mysql":
		err = tx.QueryRow(ctx, `
			INSERT INTO standalone_mysqls (uuid, name, mysql_root_password, mysql_user, mysql_password, mysql_database, image, public_port, status, environment_id, destination_id, destination_type)
			VALUES ($1,$2,$3,'mysql',$3,'default',$4,$5,'exited',$6,$7,'App\Models\StandaloneDocker')
			RETURNING id, updated_at
		`, uuid, cleanName, encPass, resolvedImg, portPtr, envIDPtr, destID).Scan(&dbID, &updatedAt)
	case "mariadb":
		err = tx.QueryRow(ctx, `
			INSERT INTO standalone_mariadbs (uuid, name, mariadb_root_password, mariadb_user, mariadb_password, mariadb_database, image, public_port, status, environment_id, destination_id, destination_type)
			VALUES ($1,$2,$3,'mariadb',$3,'default',$4,$5,'exited',$6,$7,'App\Models\StandaloneDocker')
			RETURNING id, updated_at
		`, uuid, cleanName, encPass, resolvedImg, portPtr, envIDPtr, destID).Scan(&dbID, &updatedAt)
	case "mongodb":
		err = tx.QueryRow(ctx, `
			INSERT INTO standalone_mongodbs (uuid, name, mongo_initdb_root_username, mongo_initdb_root_password, mongo_initdb_database, image, public_port, status, environment_id, destination_id, destination_type)
			VALUES ($1,$2,'root',$3,'default',$4,$5,'exited',$6,$7,'App\Models\StandaloneDocker')
			RETURNING id, updated_at
		`, uuid, cleanName, encPass, resolvedImg, portPtr, envIDPtr, destID).Scan(&dbID, &updatedAt)
	case "clickhouse":
		err = tx.QueryRow(ctx, `
			INSERT INTO standalone_clickhouses (uuid, name, clickhouse_admin_user, clickhouse_admin_password, image, public_port, status, environment_id, destination_id, destination_type)
			VALUES ($1,$2,'default',$3,$4,$5,'exited',$6,$7,'App\Models\StandaloneDocker')
			RETURNING id, updated_at
		`, uuid, cleanName, encPass, resolvedImg, portPtr, envIDPtr, destID).Scan(&dbID, &updatedAt)
	case "sqlite":
		err = tx.QueryRow(ctx, `
			INSERT INTO standalone_sqlites (uuid, name, sqlite_databases, image, status, environment_id, destination_id, destination_type)
			VALUES ($1,$2,'database.sqlite',$3,'exited',$4,$5,'App\Models\StandaloneDocker')
			RETURNING id, updated_at
		`, uuid, cleanName, resolvedImg, envIDPtr, destID).Scan(&dbID, &updatedAt)
	default:
		// redis, keydb, dragonfly
		q := fmt.Sprintf(`
			INSERT INTO %s (uuid, name, %s, image, public_port, status, environment_id, destination_id, destination_type)
			VALUES ($1,$2,$3,$4,$5,'exited',$6,$7,'App\Models\StandaloneDocker')
			RETURNING id, updated_at
		`, spec.table, spec.passCol)
		err = tx.QueryRow(ctx, q, uuid, cleanName, encPass, resolvedImg, portPtr, envIDPtr, destID).Scan(&dbID, &updatedAt)
	}
	if err != nil {
		return PersistedDatabase{}, err
	}
	db := PersistedDatabase{
		ID:              dbID,
		UUID:            uuid,
		Name:            cleanName,
		Type:            dbType,
		Image:           resolvedImg,
		CustomPort:      customPort,
		Status:          "exited",
		TeamID:          teamID,
		EnvironmentUUID: envUUID,
		ProjectUUID:     projUUID,
		ServerUUID:      serverUUID,
		UpdatedAt:       updatedAt,
	}
	if envIDPtr != nil {
		db.EnvironmentID = *envIDPtr
	}
	if m.outbox != nil {
		_ = m.outbox.AppendInTx(ctx, tx, realtime.DomainEvent{
			EventID: fmt.Sprintf("db.created-%s-%d", db.UUID, db.UpdatedAt.UnixNano()), SchemaVersion: "v1",
			Producer: "go-controlplane", TeamID: teamID, ResourceType: "database", ResourceUUID: db.UUID,
			AggregateRevision: 1, OwnershipEpoch: 1, MutationType: "database.status_changed", CommittedAt: db.UpdatedAt.UTC(),
		})
	}
	return db, tx.Commit(ctx)
}

func (m *PostgresDatabaseManager) GetByUUID(ctx context.Context, principal auth.Principal, dbUUID string) (PersistedDatabase, error) {
	_, db, encPass, err := m.loadRawWithSpec(ctx, dbUUID)
	if err != nil {
		return PersistedDatabase{}, err
	}
	teamID := db.TeamID
	ref := access.ResourceRef{
		Type: "database", ID: db.ID, UUID: db.UUID,
		ProjectUUID: db.ProjectUUID, EnvironmentUUID: db.EnvironmentUUID,
		TeamID: &teamID, APISegment: "databases",
	}
	if err := m.authz.Check(ctx, principal, access.ActionRead, ref); err != nil {
		return PersistedDatabase{}, err
	}
	if principal.AuthMethod != "mcp" && m.authz.Check(ctx, principal, access.ActionReadSensitive, ref) == nil {
		if encPass != "" {
			if m.cipher != nil {
				if plain, decErr := m.cipher.DecryptLaravelPayload(encPass, false); decErr == nil {
					db.Password = string(plain)
				}
			} else {
				db.Password = encPass
			}
		}
		db.InternalDBURL, db.ExternalDBURL = buildDatabaseURLs(db)
	}
	return db, nil
}

func buildDatabaseURLs(db PersistedDatabase) (internalURL, externalURL string) {
	pass := url.QueryEscape(db.Password)
	host := "coolify-db-" + db.UUID
	extHost := db.ServerIP
	if extHost == "" {
		extHost = "localhost"
	}
	switch db.Type {
	case "postgresql":
		internalURL = fmt.Sprintf("postgres://postgres:%s@%s:5432/postgres", pass, host)
		if db.IsPublic && db.PublicPort > 0 {
			externalURL = fmt.Sprintf("postgres://postgres:%s@%s:%d/postgres", pass, extHost, db.PublicPort)
		}
	case "mysql", "mariadb":
		internalURL = fmt.Sprintf("mysql://mysql:%s@%s:3306/default", pass, host)
		if db.IsPublic && db.PublicPort > 0 {
			externalURL = fmt.Sprintf("mysql://mysql:%s@%s:%d/default", pass, extHost, db.PublicPort)
		}
	case "mongodb":
		internalURL = fmt.Sprintf("mongodb://root:%s@%s:27017/default", pass, host)
		if db.IsPublic && db.PublicPort > 0 {
			externalURL = fmt.Sprintf("mongodb://root:%s@%s:%d/default", pass, extHost, db.PublicPort)
		}
	case "redis", "keydb", "dragonfly":
		internalURL = fmt.Sprintf("redis://default:%s@%s:6379/0", pass, host)
		if db.IsPublic && db.PublicPort > 0 {
			externalURL = fmt.Sprintf("redis://default:%s@%s:%d/0", pass, extHost, db.PublicPort)
		}
	case "clickhouse":
		internalURL = fmt.Sprintf("http://default:%s@%s:8123", pass, host)
		if db.IsPublic && db.PublicPort > 0 {
			externalURL = fmt.Sprintf("http://default:%s@%s:%d", pass, extHost, db.PublicPort)
		}
	}
	return internalURL, externalURL
}

func generateSecurePassword() string {
	buf := make([]byte, 16)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func (m *PostgresDatabaseManager) ListByTeam(ctx context.Context, principal auth.Principal, teamID int64) ([]PersistedDatabase, error) {
	if err := m.authz.CheckList(ctx, principal, teamID, "databases"); err != nil {
		return nil, err
	}
	out := make([]PersistedDatabase, 0)
	for _, spec := range allDBSpecs {
		portExpr := "COALESCE(d.public_port, 0)"
		if !spec.hasPublicPort {
			portExpr = "0"
		}
		q := fmt.Sprintf(`
			SELECT d.id, d.uuid, d.name, d.image, %s, d.status,
			       COALESCE(d.environment_id, 0), COALESCE(e.uuid,''), COALESCE(p.uuid,''),
			       COALESCE(p.team_id, srv.team_id, 0), COALESCE(srv.uuid,'localhost'), d.updated_at
			FROM %s d
			LEFT JOIN environments e ON e.id = d.environment_id
			LEFT JOIN projects p ON p.id = e.project_id
			LEFT JOIN standalone_dockers sd ON sd.id = d.destination_id
			LEFT JOIN servers srv ON srv.id = sd.server_id
			WHERE d.deleted_at IS NULL
			  AND COALESCE(p.team_id, srv.team_id, 0) = $1
			ORDER BY d.id ASC
		`, portExpr, spec.table)
		rows, err := m.pool.Query(ctx, q, teamID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var db PersistedDatabase
			db.Type = spec.dbType
			if err := rows.Scan(&db.ID, &db.UUID, &db.Name, &db.Image, &db.CustomPort, &db.Status,
				&db.EnvironmentID, &db.EnvironmentUUID, &db.ProjectUUID, &db.TeamID, &db.ServerUUID, &db.UpdatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			if spec.dbType == "postgresql" && db.ID == 0 {
				db.IsInstanceDB = true
			}
			tid := db.TeamID
			if m.authz.Check(ctx, principal, access.ActionRead, access.ResourceRef{
				Type: "database", ID: db.ID, UUID: db.UUID,
				ProjectUUID: db.ProjectUUID, EnvironmentUUID: db.EnvironmentUUID,
				TeamID: &tid, APISegment: "databases",
			}) != nil {
				continue
			}
			out = append(out, db)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (m *PostgresDatabaseManager) UpdatePort(ctx context.Context, principal auth.Principal, dbUUID string, port int) (PersistedDatabase, error) {
	return m.UpdateConfig(ctx, principal, dbUUID, DatabaseUpdateInput{PublicPort: &port})
}

func (m *PostgresDatabaseManager) UpdateConfig(ctx context.Context, principal auth.Principal, dbUUID string, in DatabaseUpdateInput) (PersistedDatabase, error) {
	spec, db, _, err := m.loadRawWithSpec(ctx, dbUUID)
	if err != nil {
		return PersistedDatabase{}, err
	}
	teamID := db.TeamID
	ref := access.ResourceRef{
		Type: "database", ID: db.ID, UUID: db.UUID, ProjectUUID: db.ProjectUUID, EnvironmentUUID: db.EnvironmentUUID, TeamID: &teamID, APISegment: "databases",
	}
	if err := m.authz.Check(ctx, principal, access.ActionUpdate, ref); err != nil {
		return PersistedDatabase{}, err
	}

	portPtr := in.PublicPort
	if portPtr == nil {
		portPtr = in.CustomPort
	}
	if db.IsInstanceDB && (portPtr != nil || in.Password != nil || in.RotatePassword || in.IsPublic != nil || in.Image != nil) {
		return PersistedDatabase{}, errors.New("cannot modify sentinel instance database coolify-db configuration")
	}

	if in.Name != nil && strings.TrimSpace(*in.Name) != "" {
		db.Name = compat.SanitizeString(*in.Name)
	}
	if in.Description != nil {
		db.Description = compat.SanitizeString(*in.Description)
	}
	if in.Image != nil && strings.TrimSpace(*in.Image) != "" {
		db.Image = strings.TrimSpace(*in.Image)
	}
	if in.IsPublic != nil {
		db.IsPublic = *in.IsPublic
	}
	if portPtr != nil {
		if !spec.hasPublicPort {
			return PersistedDatabase{}, fmt.Errorf("%s does not support public_port", db.Type)
		}
		db.CustomPort = *portPtr
		db.PublicPort = *portPtr
	}

	if spec.hasPublicPort {
		q := fmt.Sprintf(`UPDATE %s SET name = $2, description = $3, image = $4, is_public = $5, public_port = $6, updated_at = NOW() WHERE id = $1 RETURNING updated_at`, spec.table)
		if err := m.pool.QueryRow(ctx, q, db.ID, db.Name, db.Description, db.Image, db.IsPublic, db.PublicPort).Scan(&db.UpdatedAt); err != nil {
			return PersistedDatabase{}, err
		}
	} else {
		q := fmt.Sprintf(`UPDATE %s SET name = $2, description = $3, image = $4, is_public = $5, updated_at = NOW() WHERE id = $1 RETURNING updated_at`, spec.table)
		if err := m.pool.QueryRow(ctx, q, db.ID, db.Name, db.Description, db.Image, db.IsPublic).Scan(&db.UpdatedAt); err != nil {
			return PersistedDatabase{}, err
		}
	}

	newPass := ""
	if in.RotatePassword {
		newPass = generateSecurePassword()
	} else if in.Password != nil && strings.TrimSpace(*in.Password) != "" {
		newPass = *in.Password
	}
	if newPass != "" {
		if spec.passCol == "" {
			return PersistedDatabase{}, fmt.Errorf("%s does not support password credentials", db.Type)
		}
		stored := newPass
		if m.cipher != nil {
			enc, encErr := m.cipher.EncryptLaravelPayload([]byte(newPass), false)
			if encErr != nil {
				return PersistedDatabase{}, encErr
			}
			stored = enc
		}
		if err := m.pool.QueryRow(ctx, fmt.Sprintf(`UPDATE %s SET %s = $2, updated_at = NOW() WHERE id = $1 RETURNING updated_at`, spec.table, spec.passCol), db.ID, stored).Scan(&db.UpdatedAt); err != nil {
			return PersistedDatabase{}, err
		}
	}

	return m.GetByUUID(ctx, principal, dbUUID)
}

func (m *PostgresDatabaseManager) Delete(ctx context.Context, principal auth.Principal, dbUUID string) error {
	spec, db, _, err := m.loadRawWithSpec(ctx, dbUUID)
	if err != nil {
		return err
	}
	if db.ID == 0 || db.IsInstanceDB {
		return ErrCannotDeleteRoot
	}
	teamID := db.TeamID
	if err := m.authz.Check(ctx, principal, access.ActionDelete, access.ResourceRef{
		Type: "database", ID: db.ID, UUID: db.UUID, ProjectUUID: db.ProjectUUID, EnvironmentUUID: db.EnvironmentUUID, TeamID: &teamID, APISegment: "databases",
	}); err != nil {
		return err
	}
	_, err = m.pool.Exec(ctx, fmt.Sprintf(`DELETE FROM %s WHERE id = $1`, spec.table), db.ID)
	return err
}

// EnqueueProvision admits a real durable `db_provision` job into PostgreSQL for the Go worker to execute.
// Never fakes Status: "running" before execution completes.
func (m *PostgresDatabaseManager) EnqueueProvision(ctx context.Context, principal auth.Principal, dbUUID string) (ProvisionReceipt, error) {
	spec, db, _, err := m.loadRawWithSpec(ctx, dbUUID)
	if err != nil {
		return ProvisionReceipt{}, err
	}
	teamID := db.TeamID
	if err := m.authz.Check(ctx, principal, access.ActionDeploy, access.ResourceRef{
		Type: "database", ID: db.ID, UUID: db.UUID, ProjectUUID: db.ProjectUUID, EnvironmentUUID: db.EnvironmentUUID, TeamID: &teamID, APISegment: "databases",
	}); err != nil {
		return ProvisionReceipt{}, err
	}
	opID := "db-prov-" + m.hooks.NewCUID2()
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return ProvisionReceipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if !db.IsInstanceDB {
		_, _ = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET status = 'starting', updated_at = NOW() WHERE id = $1`, spec.table), db.ID)
	}
	if err := m.jobCoord.AdmitOperationInTx(ctx, tx, jobs.DurableJobSpec{
		JobOperation: jobs.JobOperation{
			JobID: "job-" + opID, OperationID: opID, AttemptID: "att-0",
			ResourceUUID: db.UUID, ServerUUID: db.ServerUUID, TeamID: db.TeamID, Status: jobs.StatusQueued, MaxAttempts: 3,
		},
		JobType: "db_provision",
		Payload: map[string]any{"database_uuid": db.UUID, "db_type": db.Type, "image": db.Image, "custom_port": db.CustomPort},
	}); err != nil {
		return ProvisionReceipt{}, err
	}
	if m.outbox != nil {
		_ = m.outbox.AppendInTx(ctx, tx, realtime.DomainEvent{
			EventID: "db.queued-" + opID, SchemaVersion: "v1", Producer: "go-controlplane",
			TeamID: db.TeamID, ResourceType: "database", ResourceUUID: db.UUID,
			AggregateRevision: 1, OwnershipEpoch: 1, MutationType: "database.status_changed", CommittedAt: time.Now().UTC(),
			Payload: map[string]any{"status": "queued", "operation_id": opID},
		})
	}
	if err := tx.Commit(ctx); err != nil {
		return ProvisionReceipt{}, err
	}
	return ProvisionReceipt{DatabaseUUID: db.UUID, Type: db.Type, Status: "queued"}, nil
}

func (m *PostgresDatabaseManager) loadRawWithSpec(ctx context.Context, dbUUID string) (dbTableSpec, PersistedDatabase, string, error) {
	for _, spec := range allDBSpecs {
		portExpr := "COALESCE(d.public_port, 0)"
		if !spec.hasPublicPort {
			portExpr = "0"
		}
		passExpr := "''"
		if spec.passCol != "" {
			passExpr = "COALESCE(d." + spec.passCol + ", '')"
		}
		whereClause := "d.uuid = $1 AND d.deleted_at IS NULL"
		if spec.dbType == "postgresql" && dbUUID == "coolify-db" {
			whereClause = "(d.uuid = $1 OR d.id = 0) AND d.deleted_at IS NULL"
		}
		q := fmt.Sprintf(`
			SELECT d.id, d.uuid, d.name, COALESCE(d.description, ''), d.image, COALESCE(d.is_public, FALSE), %s, d.status,
			       COALESCE(d.environment_id, 0), COALESCE(e.uuid,''), COALESCE(p.uuid,''),
			       COALESCE(p.team_id, srv.team_id, 0), COALESCE(srv.uuid,'localhost'), COALESCE(srv.ip,'localhost'), %s, d.updated_at
			FROM %s d
			LEFT JOIN environments e ON e.id = d.environment_id
			LEFT JOIN projects p ON p.id = e.project_id
			LEFT JOIN standalone_dockers sd ON sd.id = d.destination_id
			LEFT JOIN servers srv ON srv.id = sd.server_id
			WHERE %s
			LIMIT 1
		`, portExpr, passExpr, spec.table, whereClause)

		var db PersistedDatabase
		var encPass string
		db.Type = spec.dbType
		err := m.pool.QueryRow(ctx, q, dbUUID).Scan(
			&db.ID, &db.UUID, &db.Name, &db.Description, &db.Image, &db.IsPublic, &db.CustomPort,
			&db.Status, &db.EnvironmentID, &db.EnvironmentUUID, &db.ProjectUUID, &db.TeamID, &db.ServerUUID, &db.ServerIP, &encPass, &db.UpdatedAt,
		)
		if err == nil {
			db.PublicPort = db.CustomPort
			if spec.dbType == "postgresql" && db.ID == 0 {
				db.IsInstanceDB = true
			}
			return spec, db, encPass, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return dbTableSpec{}, PersistedDatabase{}, "", err
		}
	}
	return dbTableSpec{}, PersistedDatabase{}, "", ErrNotFound
}

func defaultImage(dbType string, supplied string) string {
	if strings.TrimSpace(supplied) != "" {
		return strings.TrimSpace(supplied)
	}
	switch dbType {
	case "postgresql":
		return "postgres:16-alpine"
	case "mysql":
		return "mysql:8"
	case "mariadb":
		return "mariadb:11"
	case "mongodb":
		return "mongo:7"
	case "redis":
		return "redis:7-alpine"
	case "clickhouse":
		return "clickhouse/clickhouse-server:latest"
	case "keydb":
		return "eqalpha/keydb:latest"
	case "dragonfly":
		return "docker.dragonflydb.io/dragonflydb/dragonfly:latest"
	case "sqlite":
		return "peakimages/sqlite:3.53.4-v0.1.0"
	default:
		return "alpine:3.21"
	}
}
