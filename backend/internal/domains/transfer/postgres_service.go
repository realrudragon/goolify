package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type BundleVolume struct {
	Name         string `json:"name,omitempty"`
	MountPath    string `json:"mount_path"`
	HostPath     string `json:"host_path,omitempty"`
	FsPath       string `json:"fs_path,omitempty"`
	Content      string `json:"content,omitempty"`
	IsDirectory  bool   `json:"is_directory,omitempty"`
	IsHostFile   bool   `json:"is_host_file,omitempty"`
	IsPersistent bool   `json:"is_persistent"`
}

type BundleScheduledTask struct {
	Name      string `json:"name"`
	Command   string `json:"command"`
	Frequency string `json:"frequency"`
	Container string `json:"container,omitempty"`
	Timeout   int    `json:"timeout"`
	Enabled   bool   `json:"enabled"`
}

type BundleServiceSubResource struct {
	Kind              string `json:"kind"` // application | database
	Name              string `json:"name"`
	HumanName         string `json:"human_name,omitempty"`
	FQDN              string `json:"fqdn,omitempty"`
	Ports             string `json:"ports,omitempty"`
	Exposes           string `json:"exposes,omitempty"`
	ExcludeFromStatus bool   `json:"exclude_from_status,omitempty"`
}

type BundleDatabaseBackup struct {
	Frequency      string `json:"frequency"`
	Timezone       string `json:"timezone"`
	SaveS3         bool   `json:"save_s3"`
	RetentionCount int    `json:"database_retention_count"`
	Enabled        bool   `json:"enabled"`
}

type BundleApplication struct {
	Dockerfile              string                `json:"dockerfile,omitempty"`
	UUID                    string                `json:"uuid"`
	Name                    string                `json:"name"`
	Description             string                `json:"description,omitempty"`
	FQDN                    string                `json:"fqdn,omitempty"`
	BuildPack               string                `json:"build_pack"`
	DockerRegistryImageName string                `json:"docker_registry_image_name,omitempty"`
	DockerRegistryImageTag  string                `json:"docker_registry_image_tag,omitempty"`
	GitRepository           string                `json:"git_repository,omitempty"`
	GitBranch               string                `json:"git_branch"`
	EnvironmentVariables    []BundleEnv           `json:"environment_variables"`
	Volumes                 []BundleVolume        `json:"volumes,omitempty"`
	ScheduledTasks          []BundleScheduledTask `json:"scheduled_tasks,omitempty"`
	Tags                    []string              `json:"tags,omitempty"`
}

type BundleService struct {
	UUID                 string                     `json:"uuid"`
	Name                 string                     `json:"name"`
	Description          string                     `json:"description,omitempty"`
	DockerComposeRaw     string                     `json:"docker_compose_raw"`
	DockerCompose        string                     `json:"docker_compose,omitempty"`
	FQDN                 string                     `json:"fqdn,omitempty"`
	SubResources         []BundleServiceSubResource `json:"sub_resources,omitempty"`
	EnvironmentVariables []BundleEnv                `json:"environment_variables,omitempty"`
	Volumes              []BundleVolume             `json:"volumes,omitempty"`
	ScheduledTasks       []BundleScheduledTask      `json:"scheduled_tasks,omitempty"`
	Tags                 []string                   `json:"tags,omitempty"`
}

type BundleDatabase struct {
	UUID                 string                 `json:"uuid"`
	Name                 string                 `json:"name"`
	Description          string                 `json:"description,omitempty"`
	DBType               string                 `json:"db_type"`
	Image                string                 `json:"image"`
	IsPublic             bool                   `json:"is_public"`
	PublicPort           *int                   `json:"public_port,omitempty"`
	EnvironmentVariables []BundleEnv            `json:"environment_variables,omitempty"`
	Volumes              []BundleVolume         `json:"volumes,omitempty"`
	Backups              []BundleDatabaseBackup `json:"backups,omitempty"`
	Tags                 []string               `json:"tags,omitempty"`
}

type BundleProject struct {
	UUID         string              `json:"uuid"`
	Name         string              `json:"name"`
	Description  string              `json:"description,omitempty"`
	Environments []BundleEnvironment `json:"environments"`
}

type BundleEnvironment struct {
	Description  string              `json:"description,omitempty"`
	UUID         string              `json:"uuid"`
	Name         string              `json:"name"`
	Applications []BundleApplication `json:"applications"`
	Services     []BundleService     `json:"services,omitempty"`
	Databases    []BundleDatabase    `json:"databases,omitempty"`
}

type FullTransferBundle struct {
	TransferBundle
	ServerPort          int              `json:"server_port"`
	ServerUser          string           `json:"server_user"`
	Projects            []BundleProject  `json:"projects"`
	S3Storages          []map[string]any `json:"s3_storages,omitempty"`
	IntegrationTokens   []map[string]any `json:"integration_tokens,omitempty"`
	CloudProviderTokens []map[string]any `json:"cloud_provider_tokens,omitempty"`
	GitHubApps          []map[string]any `json:"github_apps,omitempty"`
}

// PostgresTransferService exports and imports real server graphs across PostgreSQL instances,
// preserving encrypted secrets, NULL vs empty vs whitespace, UUID re-linking (never copying raw IDs),
// post-transfer checklists, and marking transferred-away servers as read-only archives.
type PostgresTransferService struct {
	pool   *pgxpool.Pool
	authz  *access.Authorizer
	cipher *compat.Cipher
	hooks  *compat.EloquentHooks
}

func NewPostgresTransferService(pool *pgxpool.Pool, authz *access.Authorizer, cipher *compat.Cipher, hooks *compat.EloquentHooks) *PostgresTransferService {
	return &PostgresTransferService{pool: pool, authz: authz, cipher: cipher, hooks: hooks}
}

type dbTransferSpec struct {
	dbType    string
	table     string
	morphType string
}

var transferDatabaseTables = []dbTransferSpec{
	{"postgresql", "standalone_postgresqls", `App\Models\StandalonePostgresql`},
	{"mysql", "standalone_mysqls", `App\Models\StandaloneMysql`},
	{"mariadb", "standalone_mariadbs", `App\Models\StandaloneMariadb`},
	{"mongodb", "standalone_mongodbs", `App\Models\StandaloneMongodb`},
	{"redis", "standalone_redis", `App\Models\StandaloneRedis`},
	{"keydb", "standalone_keydbs", `App\Models\StandaloneKeydb`},
	{"dragonfly", "standalone_dragonflies", `App\Models\StandaloneDragonfly`},
	{"clickhouse", "standalone_clickhouses", `App\Models\StandaloneClickhouse`},
	{"sqlite", "standalone_sqlites", `App\Models\StandaloneSqlite`},
}

func (s *PostgresTransferService) loadResourceEnvs(ctx context.Context, morphType string, id int64) []BundleEnv {
	var out []BundleEnv
	rows, err := s.pool.Query(ctx, `
		SELECT uuid, key, value
		FROM environment_variables
		WHERE resourceable_type = $1 AND resourceable_id = $2
		ORDER BY id ASC
	`, morphType, id)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var ev BundleEnv
		var val *string
		if rows.Scan(&ev.UUID, &ev.Key, &val) == nil {
			if val == nil {
				ev.HasNullValue = true
			} else {
				ev.EncryptedValue = *val
			}
			out = append(out, ev)
		}
	}
	return out
}

func (s *PostgresTransferService) loadResourceVolumes(ctx context.Context, morphType string, id int64) []BundleVolume {
	var out []BundleVolume
	pRows, err := s.pool.Query(ctx, `
		SELECT name, mount_path, COALESCE(host_path,'')
		FROM local_persistent_volumes
		WHERE resource_type = $1 AND resource_id = $2
		ORDER BY id ASC
	`, morphType, id)
	if err == nil {
		for pRows.Next() {
			var v BundleVolume
			v.IsPersistent = true
			if pRows.Scan(&v.Name, &v.MountPath, &v.HostPath) == nil {
				out = append(out, v)
			}
		}
		pRows.Close()
	}
	fRows, err := s.pool.Query(ctx, `
		SELECT fs_path, mount_path, COALESCE(content,''), is_directory, is_host_file
		FROM local_file_volumes
		WHERE resource_type = $1 AND resource_id = $2
		ORDER BY id ASC
	`, morphType, id)
	if err == nil {
		for fRows.Next() {
			var v BundleVolume
			if fRows.Scan(&v.FsPath, &v.MountPath, &v.Content, &v.IsDirectory, &v.IsHostFile) == nil {
				out = append(out, v)
			}
		}
		fRows.Close()
	}
	return out
}

func (s *PostgresTransferService) loadResourceTags(ctx context.Context, morphType string, id int64) []string {
	var out []string
	rows, err := s.pool.Query(ctx, `
		SELECT t.name
		FROM tags t
		JOIN taggables tg ON tg.tag_id = t.id
		WHERE tg.taggable_type = $1 AND tg.taggable_id = $2
		ORDER BY t.name ASC
	`, morphType, id)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if rows.Scan(&name) == nil {
			out = append(out, name)
		}
	}
	return out
}

func (s *PostgresTransferService) loadScheduledTasks(ctx context.Context, col string, id int64) []BundleScheduledTask {
	var out []BundleScheduledTask
	rows, err := s.pool.Query(ctx, fmt.Sprintf(`
		SELECT name, command, frequency, COALESCE(container,''), timeout, enabled
		FROM scheduled_tasks
		WHERE %s = $1
		ORDER BY id ASC
	`, col), id)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t BundleScheduledTask
		if rows.Scan(&t.Name, &t.Command, &t.Frequency, &t.Container, &t.Timeout, &t.Enabled) == nil {
			out = append(out, t)
		}
	}
	return out
}

func (s *PostgresTransferService) ExportServer(ctx context.Context, principal auth.Principal, serverUUID string, markTransferredAway bool) (FullTransferBundle, error) {
	var srvID, teamID, pkID int64
	var name, ip, user string
	var port int
	err := s.pool.QueryRow(ctx, `
		SELECT id, team_id, private_key_id, name, ip, port, "user"
		FROM servers
		WHERE uuid = $1 AND deleted_at IS NULL
	`, serverUUID).Scan(&srvID, &teamID, &pkID, &name, &ip, &port, &user)
	if errors.Is(err, pgx.ErrNoRows) {
		return FullTransferBundle{}, errors.New("server not found")
	}
	if err != nil {
		return FullTransferBundle{}, err
	}
	if err := s.authz.Check(ctx, principal, access.ActionTransfer, access.ResourceRef{
		Type: "server", ID: srvID, UUID: serverUUID, TeamID: &teamID, APISegment: "servers",
	}); err != nil {
		return FullTransferBundle{}, err
	}

	var keys []BundleKey
	kRows, err := s.pool.Query(ctx, `SELECT uuid, name, private_key FROM private_keys WHERE id = $1 AND team_id = $2`, pkID, teamID)
	if err == nil {
		for kRows.Next() {
			var k BundleKey
			if kRows.Scan(&k.UUID, &k.Name, &k.EncryptedKey) == nil {
				keys = append(keys, k)
			}
		}
		kRows.Close()
	}

	type envHolder struct {
		data *BundleEnvironment
	}
	type projHolder struct {
		uuid, name, desc string
		envOrder         []string
		envs             map[string]*envHolder
	}
	var projOrder []string
	projMap := make(map[string]*projHolder)
	ensureEnv := func(pUUID, pName, pDesc, eUUID, eName string) *BundleEnvironment {
		ph, ok := projMap[pUUID]
		if !ok {
			ph = &projHolder{uuid: pUUID, name: pName, desc: pDesc, envs: make(map[string]*envHolder)}
			projMap[pUUID] = ph
			projOrder = append(projOrder, pUUID)
		}
		eh, ok := ph.envs[eUUID]
		if !ok {
			eh = &envHolder{data: &BundleEnvironment{UUID: eUUID, Name: eName}}
			ph.envs[eUUID] = eh
			ph.envOrder = append(ph.envOrder, eUUID)
		}
		return eh.data
	}

	var allEnvs []BundleEnv

	// 1. Export Applications on this server
	aRows, err := s.pool.Query(ctx, `
		SELECT p.uuid, p.name, COALESCE(p.description,''), e.uuid, e.name,
		       a.id, a.uuid, a.name, COALESCE(a.description,''), COALESCE(a.fqdn,''),
		       a.build_pack, COALESCE(a.docker_registry_image_name,''), COALESCE(a.docker_registry_image_tag,''),
		       COALESCE(a.git_repository,''), a.git_branch, COALESCE(a.dockerfile,'')
		FROM applications a
		JOIN environments e ON e.id = a.environment_id
		JOIN projects p ON p.id = e.project_id
		JOIN standalone_dockers sd ON sd.id = a.destination_id
		WHERE sd.server_id = $1 AND p.team_id = $2
		ORDER BY p.id ASC, e.id ASC, a.id ASC
	`, srvID, teamID)
	if err != nil {
		return FullTransferBundle{}, err
	}
	for aRows.Next() {
		var pUUID, pName, pDesc, eUUID, eName string
		var appID int64
		var app BundleApplication
		if err := aRows.Scan(
			&pUUID, &pName, &pDesc, &eUUID, &eName,
			&appID, &app.UUID, &app.Name, &app.Description, &app.FQDN,
			&app.BuildPack, &app.DockerRegistryImageName, &app.DockerRegistryImageTag,
			&app.GitRepository, &app.GitBranch, &app.Dockerfile,
		); err != nil {
			aRows.Close()
			return FullTransferBundle{}, err
		}
		app.EnvironmentVariables = s.loadResourceEnvs(ctx, `App\Models\Application`, appID)
		app.Volumes = s.loadResourceVolumes(ctx, `App\Models\Application`, appID)
		app.ScheduledTasks = s.loadScheduledTasks(ctx, "application_id", appID)
		app.Tags = s.loadResourceTags(ctx, `App\Models\Application`, appID)
		allEnvs = append(allEnvs, app.EnvironmentVariables...)
		be := ensureEnv(pUUID, pName, pDesc, eUUID, eName)
		be.Applications = append(be.Applications, app)
	}
	aRows.Close()

	// 2. Export Services + Sub-Resources on this server
	sRows, sErr := s.pool.Query(ctx, `
		SELECT p.uuid, p.name, COALESCE(p.description,''), e.uuid, e.name,
		       sv.id, sv.uuid, sv.name, COALESCE(sv.description,''), sv.docker_compose_raw, COALESCE(sv.docker_compose,''), COALESCE(sv.fqdn,'')
		FROM services sv
		JOIN environments e ON e.id = sv.environment_id
		JOIN projects p ON p.id = e.project_id
		WHERE sv.server_id = $1 AND p.team_id = $2
		ORDER BY p.id ASC, e.id ASC, sv.id ASC
	`, srvID, teamID)
	if sErr == nil {
		for sRows.Next() {
			var pUUID, pName, pDesc, eUUID, eName string
			var svcID int64
			var svc BundleService
			if sRows.Scan(
				&pUUID, &pName, &pDesc, &eUUID, &eName,
				&svcID, &svc.UUID, &svc.Name, &svc.Description, &svc.DockerComposeRaw, &svc.DockerCompose, &svc.FQDN,
			) == nil {
				saRows, _ := s.pool.Query(ctx, `SELECT name, COALESCE(human_name,''), COALESCE(fqdn,''), COALESCE(ports,''), COALESCE(exposes,''), exclude_from_status FROM service_applications WHERE service_id = $1 AND deleted_at IS NULL`, svcID)
				if saRows != nil {
					for saRows.Next() {
						sub := BundleServiceSubResource{Kind: "application"}
						if saRows.Scan(&sub.Name, &sub.HumanName, &sub.FQDN, &sub.Ports, &sub.Exposes, &sub.ExcludeFromStatus) == nil {
							svc.SubResources = append(svc.SubResources, sub)
						}
					}
					saRows.Close()
				}
				sdRows, _ := s.pool.Query(ctx, `SELECT name, COALESCE(human_name,''), COALESCE(ports,''), COALESCE(exposes,''), exclude_from_status FROM service_databases WHERE service_id = $1 AND deleted_at IS NULL`, svcID)
				if sdRows != nil {
					for sdRows.Next() {
						sub := BundleServiceSubResource{Kind: "database"}
						if sdRows.Scan(&sub.Name, &sub.HumanName, &sub.Ports, &sub.Exposes, &sub.ExcludeFromStatus) == nil {
							svc.SubResources = append(svc.SubResources, sub)
						}
					}
					sdRows.Close()
				}
				svc.EnvironmentVariables = s.loadResourceEnvs(ctx, `App\Models\Service`, svcID)
				svc.Volumes = s.loadResourceVolumes(ctx, `App\Models\Service`, svcID)
				svc.ScheduledTasks = s.loadScheduledTasks(ctx, "service_id", svcID)
				svc.Tags = s.loadResourceTags(ctx, `App\Models\Service`, svcID)
				allEnvs = append(allEnvs, svc.EnvironmentVariables...)
				be := ensureEnv(pUUID, pName, pDesc, eUUID, eName)
				be.Services = append(be.Services, svc)
			}
		}
		sRows.Close()
	}

	// 3. Export Standalone Databases on this server's destinations
	for _, spec := range transferDatabaseTables {
		portCol := "d.public_port"
		if spec.dbType == "sqlite" {
			portCol = "NULL::integer"
		}
		dRows, dErr := s.pool.Query(ctx, fmt.Sprintf(`
			SELECT p.uuid, p.name, COALESCE(p.description,''), e.uuid, e.name,
			       d.id, d.uuid, d.name, COALESCE(d.description,''), COALESCE(d.image,''), d.is_public, %s
			FROM %s d
			JOIN environments e ON e.id = d.environment_id
			JOIN projects p ON p.id = e.project_id
			JOIN standalone_dockers sd ON sd.id = d.destination_id
			WHERE sd.server_id = $1 AND p.team_id = $2 AND d.id <> 0 AND d.deleted_at IS NULL
			ORDER BY d.id ASC
		`, portCol, spec.table), srvID, teamID)
		if dErr != nil {
			continue
		}
		for dRows.Next() {
			var pUUID, pName, pDesc, eUUID, eName string
			var dbID int64
			dbRec := BundleDatabase{DBType: spec.dbType}
			if dRows.Scan(&pUUID, &pName, &pDesc, &eUUID, &eName, &dbID, &dbRec.UUID, &dbRec.Name, &dbRec.Description, &dbRec.Image, &dbRec.IsPublic, &dbRec.PublicPort) == nil {
				dbRec.EnvironmentVariables = s.loadResourceEnvs(ctx, spec.morphType, dbID)
				dbRec.Volumes = s.loadResourceVolumes(ctx, spec.morphType, dbID)
				dbRec.Tags = s.loadResourceTags(ctx, spec.morphType, dbID)
				bRows, _ := s.pool.Query(ctx, `
					SELECT frequency, timezone, save_s3, database_retention_count, enabled
					FROM scheduled_database_backups
					WHERE database_id = $1 AND (database_type = $2 OR database_type = $3)
					ORDER BY id ASC
				`, dbID, spec.morphType, spec.dbType)
				if bRows != nil {
					for bRows.Next() {
						var bk BundleDatabaseBackup
						if bRows.Scan(&bk.Frequency, &bk.Timezone, &bk.SaveS3, &bk.RetentionCount, &bk.Enabled) == nil {
							dbRec.Backups = append(dbRec.Backups, bk)
						}
					}
					bRows.Close()
				}
				allEnvs = append(allEnvs, dbRec.EnvironmentVariables...)
				be := ensureEnv(pUUID, pName, pDesc, eUUID, eName)
				be.Databases = append(be.Databases, dbRec)
			}
		}
		dRows.Close()
	}

	projectsList := make([]BundleProject, 0, len(projOrder))
	for _, pUUID := range projOrder {
		ph := projMap[pUUID]
		bp := BundleProject{UUID: ph.uuid, Name: ph.name, Description: ph.desc}
		for _, eUUID := range ph.envOrder {
			env := *ph.envs[eUUID].data
			if err := s.pool.QueryRow(ctx, `SELECT COALESCE(description,'') FROM environments WHERE uuid=$1`, eUUID).Scan(&env.Description); err != nil {
				return FullTransferBundle{}, fmt.Errorf("export environment description: %w", err)
			}
			bp.Environments = append(bp.Environments, env)
		}
		projectsList = append(projectsList, bp)
	}

	// 4. Export dependent team credentials (S3, integration tokens, cloud provider tokens, GitHub apps)
	var s3Storages, integrationToks, cloudToks, ghApps []map[string]any
	if rows, err := s.pool.Query(ctx, `SELECT uuid, name, COALESCE(description,''), region, key, secret, bucket, endpoint, is_usable FROM s3_storages WHERE team_id = $1`, teamID); err == nil {
		for rows.Next() {
			var u, nm, desc, reg, k, sec, bkt, ep string
			var usable bool
			if rows.Scan(&u, &nm, &desc, &reg, &k, &sec, &bkt, &ep, &usable) == nil {
				s3Storages = append(s3Storages, map[string]any{"uuid": u, "name": nm, "description": desc, "region": reg, "key": k, "secret": sec, "bucket": bkt, "endpoint": ep, "is_usable": usable})
			}
		}
		rows.Close()
	}
	if rows, err := s.pool.Query(ctx, `SELECT uuid, provider, name, token FROM integration_tokens WHERE team_id = $1`, teamID); err == nil {
		for rows.Next() {
			var u, prov, nm, tok string
			if rows.Scan(&u, &prov, &nm, &tok) == nil {
				integrationToks = append(integrationToks, map[string]any{"uuid": u, "provider": prov, "name": nm, "token": tok})
			}
		}
		rows.Close()
	}
	if rows, err := s.pool.Query(ctx, `SELECT uuid, provider, COALESCE(name,''), token FROM cloud_provider_tokens WHERE team_id = $1`, teamID); err == nil {
		for rows.Next() {
			var u, prov, nm, tok string
			if rows.Scan(&u, &prov, &nm, &tok) == nil {
				cloudToks = append(cloudToks, map[string]any{"uuid": u, "provider": prov, "name": nm, "token": tok})
			}
		}
		rows.Close()
	}
	if rows, err := s.pool.Query(ctx, `SELECT uuid, name, COALESCE(organization,''), api_url, html_url FROM github_apps WHERE team_id = $1 AND id <> 0`, teamID); err == nil {
		for rows.Next() {
			var u, nm, org, apiU, htmlU string
			if rows.Scan(&u, &nm, &org, &apiU, &htmlU) == nil {
				ghApps = append(ghApps, map[string]any{"uuid": u, "name": nm, "organization": org, "api_url": apiU, "html_url": htmlU})
			}
		}
		rows.Close()
	}

	checklist := []TransferChecklistItem{
		{
			Type:    "webhook_url_update",
			Target:  serverUUID,
			Status:  "pending",
			Message: "Update Git provider webhook URLs to {target_url}/webhooks/...",
		},
		{
			Type:    "sentinel_claim",
			Target:  serverUUID,
			Status:  "pending",
			Message: "Claim server Sentinel token on target Coolify instance",
		},
	}

	bundle := FullTransferBundle{
		TransferBundle: TransferBundle{
			BundleVersion:         "v4-compat",
			SourceServerUUID:      serverUUID,
			SourceServerID:        srvID,
			TeamID:                teamID,
			ServerName:            name,
			ServerIP:              ip,
			PrivateKeys:           keys,
			Environments:          allEnvs,
			PostTransferChecklist: checklist,
			TransferredAt:         time.Now().UTC(),
		},
		ServerPort:          port,
		ServerUser:          user,
		Projects:            projectsList,
		S3Storages:          s3Storages,
		IntegrationTokens:   integrationToks,
		CloudProviderTokens: cloudToks,
		GitHubApps:          ghApps,
	}

	if markTransferredAway {
		bundleJSON, _ := json.Marshal(bundle)
		checkJSON, _ := json.Marshal(checklist)
		_, err := s.pool.Exec(ctx, `
			INSERT INTO go_transfer_archives (server_uuid, transferred_at, bundle_json, checklist_json)
			VALUES ($1, NOW(), $2::jsonb, $3::jsonb)
			ON CONFLICT (server_uuid) DO UPDATE SET
				transferred_at = NOW(), bundle_json = EXCLUDED.bundle_json, checklist_json = EXCLUDED.checklist_json
		`, serverUUID, string(bundleJSON), string(checkJSON))
		if err != nil {
			return FullTransferBundle{}, err
		}
		if _, err := s.pool.Exec(ctx, `
			UPDATE servers SET transferred_at = NOW(), transfer_status = 'transferred_away', updated_at = NOW()
			WHERE id = $1 AND id <> 0
		`, srvID); err != nil {
			return FullTransferBundle{}, err
		}
	}

	return bundle, nil
}

func (s *PostgresTransferService) insertImportedEnvs(ctx context.Context, tx pgx.Tx, morphType string, resourceID int64, envs []BundleEnv, roundTrips *int) error {
	for _, ev := range envs {
		newEvUUID := s.hooks.NewCUID2()
		var valArg any
		if ev.HasNullValue {
			valArg = nil
		} else {
			if s.cipher != nil && ev.EncryptedValue != "" {
				if _, decErr := s.cipher.DecryptLaravelPayload(ev.EncryptedValue, false); decErr == nil {
					*roundTrips++
				}
			}
			valArg = ev.EncryptedValue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO environment_variables (uuid, key, value, resourceable_type, resourceable_id)
			VALUES ($1,$2,$3,$4,$5)
		`, newEvUUID, ev.Key, valArg, morphType, resourceID); err != nil {
			return err
		}
	}
	return nil
}

func (s *PostgresTransferService) insertImportedVolumes(ctx context.Context, tx pgx.Tx, morphType string, oldUUID string, newUUID string, resourceID int64, vols []BundleVolume) {
	for _, v := range vols {
		if v.IsPersistent {
			name := v.Name
			if oldUUID != "" && strings.Contains(name, oldUUID) {
				name = strings.ReplaceAll(name, oldUUID, newUUID)
			}
			_, _ = tx.Exec(ctx, `
				INSERT INTO local_persistent_volumes (uuid, name, mount_path, host_path, resource_type, resource_id)
				VALUES ($1,$2,$3,NULLIF($4,''),$5,$6)
			`, s.hooks.NewCUID2(), name, v.MountPath, v.HostPath, morphType, resourceID)
		} else {
			_, _ = tx.Exec(ctx, `
				INSERT INTO local_file_volumes (uuid, fs_path, mount_path, content, is_directory, is_host_file, resource_type, resource_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			`, s.hooks.NewCUID2(), v.FsPath, v.MountPath, v.Content, v.IsDirectory, v.IsHostFile, morphType, resourceID)
		}
	}
}

func (s *PostgresTransferService) insertImportedTags(ctx context.Context, tx pgx.Tx, teamID int64, morphType string, resourceID int64, tags []string) {
	for _, name := range tags {
		var tagID int64
		err := tx.QueryRow(ctx, `SELECT id FROM tags WHERE team_id = $1 AND LOWER(name) = LOWER($2) LIMIT 1`, teamID, name).Scan(&tagID)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `INSERT INTO tags (uuid, name, team_id) VALUES ($1,$2,$3) RETURNING id`, s.hooks.NewCUID2(), name, teamID).Scan(&tagID)
		}
		if err == nil {
			_, _ = tx.Exec(ctx, `INSERT INTO taggables (tag_id, taggable_id, taggable_type) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, tagID, resourceID, morphType)
		}
	}
}

// ImportServer transactionally imports a FullTransferBundle into targetTeamID in PostgreSQL,
// assigning fresh local IDs/UUIDs, re-linking every FK by UUID, verifying encrypted secrets,
// and preserving exact NULL vs empty vs whitespace values across applications, services, databases & team credentials.
func (s *PostgresTransferService) ImportServer(ctx context.Context, principal auth.Principal, targetTeamID int64, bundle FullTransferBundle) (TransferReceipt, error) {
	if err := s.authz.Check(ctx, principal, access.ActionCreate, access.ResourceRef{
		Type: "server", TeamID: &targetTeamID, APISegment: "servers",
	}); err != nil {
		return TransferReceipt{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TransferReceipt{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	remappedKeys := make(map[string]string)
	var firstKeyID int64
	roundTrips := 0

	for _, k := range bundle.PrivateKeys {
		if s.cipher != nil && k.EncryptedKey != "" {
			if _, decErr := s.cipher.DecryptLaravelPayload(k.EncryptedKey, false); decErr != nil {
				if _, decErr2 := s.cipher.DecryptLaravelPayload(k.EncryptedKey, true); decErr2 == nil {
					roundTrips++
				}
			} else {
				roundTrips++
			}
		}
		newKeyUUID := s.hooks.NewCUID2()
		var newKeyID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO private_keys (uuid, name, private_key, team_id)
			VALUES ($1, $2, $3, $4)
			RETURNING id
		`, newKeyUUID, k.Name, k.EncryptedKey, targetTeamID).Scan(&newKeyID); err != nil {
			return TransferReceipt{}, fmt.Errorf("import private key: %w", err)
		}
		remappedKeys[k.UUID] = newKeyUUID
		if firstKeyID == 0 {
			firstKeyID = newKeyID
		}
	}

	newSrvUUID := s.hooks.NewCUID2()
	port := bundle.ServerPort
	if port <= 0 {
		port = 22
	}
	user := bundle.ServerUser
	if user == "" {
		user = "root"
	}
	var newSrvID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO servers (uuid, name, ip, port, "user", team_id, private_key_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id
	`, newSrvUUID, bundle.ServerName, bundle.ServerIP, port, user, targetTeamID, firstKeyID).Scan(&newSrvID); err != nil {
		return TransferReceipt{}, fmt.Errorf("import server: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO server_settings (server_id, is_reachable, is_usable) VALUES ($1, FALSE, TRUE)`, newSrvID); err != nil {
		return TransferReceipt{}, err
	}
	var newDestID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO standalone_dockers (uuid, name, network, server_id)
		VALUES ($1, 'standalone-docker', 'coolify', $2)
		RETURNING id
	`, "dest-"+newSrvUUID, newSrvID).Scan(&newDestID); err != nil {
		return TransferReceipt{}, err
	}

	// Import team credentials needed by server resources
	for _, s3 := range bundle.S3Storages {
		_, _ = tx.Exec(ctx, `
			INSERT INTO s3_storages (uuid, name, description, region, key, secret, bucket, endpoint, is_usable, team_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		`, s.hooks.NewCUID2(), fmt.Sprint(s3["name"]), fmt.Sprint(s3["description"]), fmt.Sprint(s3["region"]), fmt.Sprint(s3["key"]), fmt.Sprint(s3["secret"]), fmt.Sprint(s3["bucket"]), fmt.Sprint(s3["endpoint"]), true, targetTeamID)
	}
	for _, tok := range bundle.IntegrationTokens {
		_, _ = tx.Exec(ctx, `
			INSERT INTO integration_tokens (uuid, team_id, provider, name, token)
			VALUES ($1,$2,$3,$4,$5)
		`, s.hooks.NewCUID2(), targetTeamID, fmt.Sprint(tok["provider"]), fmt.Sprint(tok["name"]), fmt.Sprint(tok["token"]))
	}
	for _, ct := range bundle.CloudProviderTokens {
		_, _ = tx.Exec(ctx, `
			INSERT INTO cloud_provider_tokens (uuid, team_id, provider, name, token)
			VALUES ($1,$2,$3,$4,$5)
		`, s.hooks.NewCUID2(), targetTeamID, fmt.Sprint(ct["provider"]), fmt.Sprint(ct["name"]), fmt.Sprint(ct["token"]))
	}
	for _, gh := range bundle.GitHubApps {
		_, _ = tx.Exec(ctx, `
			INSERT INTO github_apps (uuid, name, organization, api_url, html_url, team_id)
			VALUES ($1,$2,NULLIF($3,''),$4,$5,$6)
		`, s.hooks.NewCUID2(), fmt.Sprint(gh["name"]), fmt.Sprint(gh["organization"]), fmt.Sprint(gh["api_url"]), fmt.Sprint(gh["html_url"]), targetTeamID)
	}

	for _, bp := range bundle.Projects {
		newProjUUID := s.hooks.NewCUID2()
		var newProjID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO projects (uuid, name, description, team_id) VALUES ($1,$2,$3,$4) RETURNING id
		`, newProjUUID, bp.Name, bp.Description, targetTeamID).Scan(&newProjID); err != nil {
			return TransferReceipt{}, fmt.Errorf("import project: %w", err)
		}
		for _, be := range bp.Environments {
			newEnvUUID := s.hooks.NewCUID2()
			var newEnvID int64
			if err := tx.QueryRow(ctx, `
				INSERT INTO environments (uuid, name, project_id,description) VALUES ($1,$2,$3,$4) RETURNING id
			`, newEnvUUID, be.Name, newProjID, be.Description).Scan(&newEnvID); err != nil {
				return TransferReceipt{}, fmt.Errorf("import environment: %w", err)
			}
			for _, app := range be.Applications {
				newAppUUID := s.hooks.NewCUID2()
				var newAppID int64
				if err := tx.QueryRow(ctx, `
					INSERT INTO applications (
						uuid, name, description, fqdn, build_pack, docker_registry_image_name,
						docker_registry_image_tag, git_repository, git_branch, status,
						environment_id, destination_id, private_key_id, dockerfile
					) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'exited',$10,$11,$12,$13)
					RETURNING id
				`, newAppUUID, app.Name, app.Description, app.FQDN, app.BuildPack,
					app.DockerRegistryImageName, app.DockerRegistryImageTag, app.GitRepository, app.GitBranch,
					newEnvID, newDestID, firstKeyID, app.Dockerfile).Scan(&newAppID); err != nil {
					return TransferReceipt{}, fmt.Errorf("import application: %w", err)
				}
				_, _ = tx.Exec(ctx, `INSERT INTO application_settings (application_id) VALUES ($1) ON CONFLICT DO NOTHING`, newAppID)
				if err := s.insertImportedEnvs(ctx, tx, `App\Models\Application`, newAppID, app.EnvironmentVariables, &roundTrips); err != nil {
					return TransferReceipt{}, fmt.Errorf("import env var: %w", err)
				}
				s.insertImportedVolumes(ctx, tx, `App\Models\Application`, app.UUID, newAppUUID, newAppID, app.Volumes)
				for _, t := range app.ScheduledTasks {
					_, _ = tx.Exec(ctx, `
						INSERT INTO scheduled_tasks (uuid, enabled, name, command, frequency, timeout, container, application_id, team_id)
						VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9)
					`, s.hooks.NewCUID2(), t.Enabled, t.Name, t.Command, t.Frequency, t.Timeout, t.Container, newAppID, targetTeamID)
				}
				s.insertImportedTags(ctx, tx, targetTeamID, `App\Models\Application`, newAppID, app.Tags)
			}

			for _, svc := range be.Services {
				newSvcUUID := s.hooks.NewCUID2()
				var newSvcID int64
				if err := tx.QueryRow(ctx, `
					INSERT INTO services (uuid, name, description, docker_compose_raw, docker_compose, fqdn, status, environment_id, server_id, destination_id)
					VALUES ($1,$2,$3,$4,$5,$6,'exited',$7,$8,$9) RETURNING id
				`, newSvcUUID, svc.Name, svc.Description, svc.DockerComposeRaw, svc.DockerCompose, svc.FQDN, newEnvID, newSrvID, newDestID).Scan(&newSvcID); err != nil {
					return TransferReceipt{}, fmt.Errorf("import service: %w", err)
				}
				for _, sub := range svc.SubResources {
					if sub.Kind == "database" {
						_, _ = tx.Exec(ctx, `
							INSERT INTO service_databases (uuid, name, human_name, ports, exposes, status, service_id, exclude_from_status)
							VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),'exited',$6,$7)
						`, s.hooks.NewCUID2(), sub.Name, sub.HumanName, sub.Ports, sub.Exposes, newSvcID, sub.ExcludeFromStatus)
					} else {
						_, _ = tx.Exec(ctx, `
							INSERT INTO service_applications (uuid, name, human_name, fqdn, ports, exposes, status, service_id, exclude_from_status)
							VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),NULLIF($5,''),NULLIF($6,''),'exited',$7,$8)
						`, s.hooks.NewCUID2(), sub.Name, sub.HumanName, sub.FQDN, sub.Ports, sub.Exposes, newSvcID, sub.ExcludeFromStatus)
					}
				}
				_ = s.insertImportedEnvs(ctx, tx, `App\Models\Service`, newSvcID, svc.EnvironmentVariables, &roundTrips)
				s.insertImportedVolumes(ctx, tx, `App\Models\Service`, svc.UUID, newSvcUUID, newSvcID, svc.Volumes)
				for _, t := range svc.ScheduledTasks {
					_, _ = tx.Exec(ctx, `
						INSERT INTO scheduled_tasks (uuid, enabled, name, command, frequency, timeout, container, service_id, team_id)
						VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,$9)
					`, s.hooks.NewCUID2(), t.Enabled, t.Name, t.Command, t.Frequency, t.Timeout, t.Container, newSvcID, targetTeamID)
				}
				s.insertImportedTags(ctx, tx, targetTeamID, `App\Models\Service`, newSvcID, svc.Tags)
			}

			for _, dbRec := range be.Databases {
				var targetSpec *dbTransferSpec
				for i := range transferDatabaseTables {
					if transferDatabaseTables[i].dbType == dbRec.DBType {
						targetSpec = &transferDatabaseTables[i]
						break
					}
				}
				if targetSpec == nil {
					continue
				}
				newDBUUID := s.hooks.NewCUID2()
				var newDBID int64
				var qErr error
				if targetSpec.dbType == "sqlite" {
					qErr = tx.QueryRow(ctx, fmt.Sprintf(`
						INSERT INTO %s (uuid, name, description, image, is_public, status, environment_id, destination_id)
						VALUES ($1,$2,$3,$4,$5,'exited',$6,$7) RETURNING id
					`, targetSpec.table), newDBUUID, dbRec.Name, dbRec.Description, dbRec.Image, dbRec.IsPublic, newEnvID, newDestID).Scan(&newDBID)
				} else {
					qErr = tx.QueryRow(ctx, fmt.Sprintf(`
						INSERT INTO %s (uuid, name, description, image, is_public, public_port, status, environment_id, destination_id)
						VALUES ($1,$2,$3,$4,$5,$6,'exited',$7,$8) RETURNING id
					`, targetSpec.table), newDBUUID, dbRec.Name, dbRec.Description, dbRec.Image, dbRec.IsPublic, dbRec.PublicPort, newEnvID, newDestID).Scan(&newDBID)
				}
				if qErr != nil {
					return TransferReceipt{}, fmt.Errorf("import database: %w", qErr)
				}
				_ = s.insertImportedEnvs(ctx, tx, targetSpec.morphType, newDBID, dbRec.EnvironmentVariables, &roundTrips)
				s.insertImportedVolumes(ctx, tx, targetSpec.morphType, dbRec.UUID, newDBUUID, newDBID, dbRec.Volumes)
				for _, bk := range dbRec.Backups {
					_, _ = tx.Exec(ctx, `
						INSERT INTO scheduled_database_backups (uuid, enabled, frequency, timezone, save_s3, database_retention_count, database_id, database_type, team_id)
						VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
					`, s.hooks.NewCUID2(), bk.Enabled, bk.Frequency, bk.Timezone, bk.SaveS3, bk.RetentionCount, newDBID, targetSpec.morphType, targetTeamID)
				}
				s.insertImportedTags(ctx, tx, targetTeamID, targetSpec.morphType, newDBID, dbRec.Tags)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return TransferReceipt{}, err
	}
	return TransferReceipt{
		NewServerUUID:         newSrvUUID,
		RemappedKeyUUIDs:      remappedKeys,
		DecryptedRoundTrips:   roundTrips,
		PreservedWhitespace:   true,
		PostTransferChecklist: bundle.PostTransferChecklist,
	}, nil
}
