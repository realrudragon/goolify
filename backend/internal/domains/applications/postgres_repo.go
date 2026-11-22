package applications

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/deploymentcontext"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/coollabsio/coolify/backend/internal/realtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound = errors.New("application not found")
)

type Application struct {
	Dockerfile              string            `json:"dockerfile,omitempty"`
	ID                      int64             `json:"id"`
	UUID                    string            `json:"uuid"`
	Name                    string            `json:"name"`
	Description             string            `json:"description,omitempty"`
	FQDN                    string            `json:"fqdn,omitempty"`
	GitRepository           string            `json:"git_repository,omitempty"`
	GitBranch               string            `json:"git_branch"`
	BuildPack               string            `json:"build_pack"`
	DockerRegistryImageName string            `json:"docker_registry_image_name,omitempty"`
	DockerRegistryImageTag  string            `json:"docker_registry_image_tag,omitempty"`
	DockerfileLocation      string            `json:"dockerfile_location,omitempty"`
	DockerComposeRaw        string            `json:"docker_compose_raw,omitempty"`
	PortsExposes            string            `json:"ports_exposes,omitempty"`
	Status                  string            `json:"status"`
	EnvironmentID           int64             `json:"environment_id"`
	EnvironmentUUID         string            `json:"environment_uuid,omitempty"`
	ProjectUUID             string            `json:"project_uuid,omitempty"`
	TeamID                  int64             `json:"team_id"`
	DestinationID           int64             `json:"destination_id"`
	ServerID                int64             `json:"server_id"`
	ServerUUID              string            `json:"server_uuid,omitempty"`
	PrivateKeyID            *int64            `json:"private_key_id,omitempty"`
	Settings                map[string]string `json:"settings,omitempty"`
	UpdatedAt               time.Time         `json:"updated_at"`
}

type CreateApplicationInput struct {
	Dockerfile              string            `json:"dockerfile"`
	DestinationUUID         string            `json:"destination_uuid"`
	Name                    string            `json:"name"`
	Description             string            `json:"description"`
	ProjectUUID             string            `json:"project_uuid,omitempty"`
	EnvironmentName         string            `json:"environment_name,omitempty"`
	EnvironmentUUID         string            `json:"environment_uuid"`
	ServerUUID              string            `json:"server_uuid"`
	FQDN                    string            `json:"fqdn"`
	GitRepository           string            `json:"git_repository"`
	GitBranch               string            `json:"git_branch"`
	BuildPack               string            `json:"build_pack"`
	DockerRegistryImageName string            `json:"docker_registry_image_name"`
	DockerRegistryImageTag  string            `json:"docker_registry_image_tag"`
	DockerfileLocation      string            `json:"dockerfile_location"`
	DockerComposeRaw        string            `json:"docker_compose_raw"`
	PortsExposes            string            `json:"ports_exposes"`
	WebhookSecretGitHub     string            `json:"webhook_secret_github,omitempty"`
	Settings                map[string]string `json:"settings,omitempty"`
}

type EnvVarInput struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	IsPreview   bool   `json:"is_preview"`
	IsBuildtime bool   `json:"is_buildtime"`
	IsRuntime   bool   `json:"is_runtime"`
	IsLiteral   bool   `json:"is_literal"`
	IsShownOnce bool   `json:"is_shown_once"`
}

type EnvVarDTO struct {
	ID          int64  `json:"id"`
	UUID        string `json:"uuid"`
	Key         string `json:"key"`
	Value       string `json:"value,omitempty"` // Hidden when is_shown_once or lacking ActionReadSensitive
	IsPreview   bool   `json:"is_preview"`
	IsBuildtime bool   `json:"is_buildtime"`
	IsRuntime   bool   `json:"is_runtime"`
	IsLiteral   bool   `json:"is_literal"`
	IsShownOnce bool   `json:"is_shown_once"`
}

type DeploymentRecord struct {
	DeploymentUUID  string    `json:"deployment_uuid"`
	OperationID     string    `json:"operation_id"`
	ApplicationID   int64     `json:"application_id"`
	ApplicationUUID string    `json:"application_uuid"`
	ServerUUID      string    `json:"server_uuid"`
	Status          string    `json:"status"`
	ForceRebuild    bool      `json:"force_rebuild"`
	IsWebhook       bool      `json:"is_webhook"`
	IsAPI           bool      `json:"is_api"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// PostgresService manages Applications, encrypted EnvironmentVariables, DeploymentContext loading,
// and durable deployment queue admission in PostgreSQL.
type PostgresService struct {
	pool     *pgxpool.Pool
	authz    *access.Authorizer
	hooks    *compat.EloquentHooks
	cipher   *compat.Cipher
	outbox   *realtime.PostgresOutboxStore
	jobCoord *jobs.PostgresJobCoordinator
	resolver *deploymentcontext.Resolver
}

func NewPostgresService(
	pool *pgxpool.Pool,
	authz *access.Authorizer,
	hooks *compat.EloquentHooks,
	cipher *compat.Cipher,
	outbox *realtime.PostgresOutboxStore,
	jobCoord *jobs.PostgresJobCoordinator,
) *PostgresService {
	svc := &PostgresService{
		pool:     pool,
		authz:    authz,
		hooks:    hooks,
		cipher:   cipher,
		outbox:   outbox,
		jobCoord: jobCoord,
	}
	svc.resolver = deploymentcontext.NewResolver(svc, authz)
	return svc
}

func (s *PostgresService) Resolver() *deploymentcontext.Resolver {
	return s.resolver
}

// LoadApplicationSource implements deploymentcontext.Repository directly against PostgreSQL.
func (s *PostgresService) LoadApplicationSource(ctx context.Context, appUUID string, previewID *int64) (deploymentcontext.DeploymentSource, error) {
	var src deploymentcontext.DeploymentSource
	var imgName, imgTag, dfLoc, fqdn, gitRepo, gitBranch *string
	var pkID *int64
	var pkUUID *string
	var pkTeam *int64

	err := s.pool.QueryRow(ctx, `
		SELECT a.id, a.uuid, e.id, e.uuid, p.id, p.uuid, p.team_id,
		       COALESCE(sd.server_id, 0), COALESCE(srv.uuid, 'localhost'),
		       COALESCE(a.private_key_id, srv.private_key_id),
		       pk.uuid, pk.team_id,
		       a.build_pack, a.docker_registry_image_name, a.docker_registry_image_tag,
		       a.dockerfile_location, a.fqdn, a.git_repository, a.git_branch
		FROM applications a
		JOIN environments e ON e.id = a.environment_id
		JOIN projects p ON p.id = e.project_id
		LEFT JOIN standalone_dockers sd ON sd.id = a.destination_id
		LEFT JOIN servers srv ON srv.id = COALESCE(sd.server_id, 0)
		LEFT JOIN private_keys pk ON pk.id = COALESCE(a.private_key_id, srv.private_key_id)
		WHERE a.uuid = $1
	`, appUUID).Scan(
		&src.ApplicationID, &src.ApplicationUUID,
		&src.EnvironmentID, &src.EnvironmentUUID,
		&src.ProjectID, &src.ProjectUUID, &src.ResourceTeamID,
		&src.ServerID, &src.ServerUUID,
		&pkID, &pkUUID, &pkTeam,
		&src.BuildPack, &imgName, &imgTag,
		&dfLoc, &fqdn, &gitRepo, &gitBranch,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return deploymentcontext.DeploymentSource{}, ErrNotFound
	}
	if err != nil {
		return deploymentcontext.DeploymentSource{}, err
	}
	if pkID != nil {
		src.PrivateKeyID = *pkID
	}
	if pkUUID != nil {
		src.PrivateKeyUUID = *pkUUID
	}
	if pkTeam != nil {
		src.PrivateKeyTeam = *pkTeam
	}
	src.Settings = s.loadApplicationSettingsMap(ctx, src.ApplicationID)
	if imgName != nil && *imgName != "" {
		src.Settings["docker_registry_image_name"] = *imgName
	}
	if imgTag != nil && *imgTag != "" {
		src.Settings["docker_registry_image_tag"] = *imgTag
	}
	if dfLoc != nil && *dfLoc != "" {
		src.Settings["dockerfile_location"] = *dfLoc
	}
	if fqdn != nil && *fqdn != "" {
		src.Settings["fqdn"] = *fqdn
	}
	if gitRepo != nil && *gitRepo != "" {
		src.Settings["git_repository"] = *gitRepo
	}
	if gitBranch != nil && *gitBranch != "" {
		src.Settings["git_branch"] = *gitBranch
	}

	// Load shared environment variables for Team, Project, and Environment
	sRows, err := s.pool.Query(ctx, `
		SELECT key, value, type, is_shown_once
		FROM shared_environment_variables
		WHERE team_id = $1
		  AND (
		    type = 'team'
		    OR (type = 'project' AND project_id = $2)
		    OR (type = 'environment' AND environment_id = $3)
		  )
		ORDER BY id ASC
	`, src.ResourceTeamID, src.ProjectID, src.EnvironmentID)
	if err != nil {
		return deploymentcontext.DeploymentSource{}, err
	}
	defer sRows.Close()
	for sRows.Next() {
		var k, typ string
		var rawVal *string
		var locked bool
		if err := sRows.Scan(&k, &rawVal, &typ, &locked); err != nil {
			return deploymentcontext.DeploymentSource{}, err
		}
		plain := s.decryptOpt(rawVal)
		rec := deploymentcontext.EnvVarRecord{
			Key: k, Value: plain, IsBuildtime: true, IsRuntime: true, IsLocked: locked,
		}
		switch typ {
		case "team":
			rec.Scope = deploymentcontext.ScopeTeam
			src.TeamVars = append(src.TeamVars, rec)
		case "project":
			rec.Scope = deploymentcontext.ScopeProject
			src.ProjectVars = append(src.ProjectVars, rec)
		case "environment":
			rec.Scope = deploymentcontext.ScopeEnvironment
			src.EnvironmentVars = append(src.EnvironmentVars, rec)
		}
	}

	// Load resource & preview environment variables
	eRows, err := s.pool.Query(ctx, `
		SELECT key, value, is_preview, is_buildtime, is_runtime, is_literal, is_shown_once
		FROM environment_variables
		WHERE resourceable_type = 'App\Models\Application' AND resourceable_id = $1
		ORDER BY id ASC
	`, src.ApplicationID)
	if err != nil {
		return deploymentcontext.DeploymentSource{}, err
	}
	defer eRows.Close()
	for eRows.Next() {
		var k string
		var rawVal *string
		var isPrev, isBuild, isRun, isLit, isLocked bool
		if err := eRows.Scan(&k, &rawVal, &isPrev, &isBuild, &isRun, &isLit, &isLocked); err != nil {
			return deploymentcontext.DeploymentSource{}, err
		}
		rec := deploymentcontext.EnvVarRecord{
			Key: k, Value: s.decryptOpt(rawVal),
			IsBuildtime: isBuild, IsRuntime: isRun, IsLiteral: isLit, IsLocked: isLocked,
		}
		if isPrev {
			rec.Scope = deploymentcontext.ScopePreview
			src.PreviewVars = append(src.PreviewVars, rec)
		} else {
			rec.Scope = deploymentcontext.ScopeResource
			src.ResourceVars = append(src.ResourceVars, rec)
		}
	}

	return src, nil
}

func (s *PostgresService) decryptOpt(raw *string) string {
	if raw == nil {
		return ""
	}
	if s.cipher == nil || *raw == "" {
		return *raw
	}
	if plain, err := s.cipher.DecryptLaravelPayload(*raw, false); err == nil {
		return string(plain)
	}
	if plain, err := s.cipher.DecryptLaravelPayload(*raw, true); err == nil {
		return string(plain)
	}
	return *raw
}

func (s *PostgresService) ListByTeam(ctx context.Context, principal auth.Principal, teamID int64) ([]Application, error) {
	if err := s.authz.CheckList(ctx, principal, teamID, "applications"); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT a.id, a.uuid, a.name, COALESCE(a.description, ''), COALESCE(a.fqdn, ''),
		       COALESCE(a.git_repository, ''), a.git_branch, a.build_pack,
		       COALESCE(a.docker_registry_image_name, ''), COALESCE(a.docker_registry_image_tag, ''),
		       COALESCE(a.dockerfile_location, ''), COALESCE(a.ports_exposes, ''), a.status,
		       a.environment_id, e.uuid, p.uuid, p.team_id, a.destination_id,
		       COALESCE(sd.server_id, 0), COALESCE(srv.uuid, 'localhost'), a.updated_at, COALESCE(a.dockerfile,'')
		FROM applications a
		JOIN environments e ON e.id = a.environment_id
		JOIN projects p ON p.id = e.project_id
		LEFT JOIN standalone_dockers sd ON sd.id = a.destination_id
		LEFT JOIN servers srv ON srv.id = COALESCE(sd.server_id, 0)
		WHERE p.team_id = $1
		ORDER BY a.id ASC
	`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Application, 0)
	for rows.Next() {
		var a Application
		if err := rows.Scan(
			&a.ID, &a.UUID, &a.Name, &a.Description, &a.FQDN,
			&a.GitRepository, &a.GitBranch, &a.BuildPack,
			&a.DockerRegistryImageName, &a.DockerRegistryImageTag,
			&a.DockerfileLocation, &a.PortsExposes, &a.Status,
			&a.EnvironmentID, &a.EnvironmentUUID, &a.ProjectUUID, &a.TeamID, &a.DestinationID,
			&a.ServerID, &a.ServerUUID, &a.UpdatedAt, &a.Dockerfile,
		); err != nil {
			return nil, err
		}
		tid := a.TeamID
		sid := a.ServerID
		if s.authz.Check(ctx, principal, access.ActionRead, access.ResourceRef{
			Type: "application", ID: a.ID, UUID: a.UUID,
			ProjectUUID: a.ProjectUUID, EnvironmentUUID: a.EnvironmentUUID,
			TeamID: &tid, ServerID: &sid, APISegment: "applications",
		}) != nil {
			continue
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *PostgresService) GetByUUID(ctx context.Context, principal auth.Principal, uuid string) (Application, error) {
	a, err := s.loadByUUID(ctx, uuid)
	if err != nil {
		return Application{}, err
	}
	teamID := a.TeamID
	if err := s.authz.Check(ctx, principal, access.ActionRead, access.ResourceRef{
		Type: "application", ID: a.ID, UUID: a.UUID, ProjectUUID: a.ProjectUUID, EnvironmentUUID: a.EnvironmentUUID,
		TeamID: &teamID, ServerID: &a.ServerID, APISegment: "applications",
	}); err != nil {
		return Application{}, err
	}
	return a, nil
}

func (s *PostgresService) loadByUUID(ctx context.Context, uuid string) (Application, error) {
	var a Application
	err := s.pool.QueryRow(ctx, `
		SELECT a.id, a.uuid, a.name, COALESCE(a.description, ''), COALESCE(a.fqdn, ''),
		       COALESCE(a.git_repository, ''), a.git_branch, a.build_pack,
		       COALESCE(a.docker_registry_image_name, ''), COALESCE(a.docker_registry_image_tag, ''),
		       COALESCE(a.dockerfile_location, ''), COALESCE(a.docker_compose_raw, ''), COALESCE(a.ports_exposes, ''),
		       a.status, a.environment_id, e.uuid, p.uuid, p.team_id, a.destination_id,
		       COALESCE(sd.server_id, 0), COALESCE(srv.uuid, 'localhost'), a.updated_at, COALESCE(a.dockerfile,'')
		FROM applications a
		JOIN environments e ON e.id = a.environment_id
		JOIN projects p ON p.id = e.project_id
		LEFT JOIN standalone_dockers sd ON sd.id = a.destination_id
		LEFT JOIN servers srv ON srv.id = COALESCE(sd.server_id, 0)
		WHERE a.uuid = $1
	`, uuid).Scan(
		&a.ID, &a.UUID, &a.Name, &a.Description, &a.FQDN,
		&a.GitRepository, &a.GitBranch, &a.BuildPack,
		&a.DockerRegistryImageName, &a.DockerRegistryImageTag,
		&a.DockerfileLocation, &a.DockerComposeRaw, &a.PortsExposes,
		&a.Status, &a.EnvironmentID, &a.EnvironmentUUID, &a.ProjectUUID, &a.TeamID, &a.DestinationID,
		&a.ServerID, &a.ServerUUID, &a.UpdatedAt, &a.Dockerfile,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, ErrNotFound
	}
	if err != nil {
		return Application{}, err
	}
	a.Settings = s.loadApplicationSettingsMap(ctx, a.ID)
	return a, nil
}

func (s *PostgresService) loadApplicationSettingsMap(ctx context.Context, appID int64) map[string]string {
	out := make(map[string]string)
	var (
		isStatic, isSPA, submodules, lfs, shallow       bool
		autoDeploy, forceHTTPS, debugMode, prevDeploys  bool
		prPublic, logDrain, gpuEnabled, inclTS          bool
		swarmOnly, rawCompose, buildServer, consNames   bool
		gzipEn, stripPrefix, permRedir, connectNet      bool
		labelEscape, labelReadonly, preserveRepo        bool
		disableCache, buildSecrets, redactAll, injArgs  bool
		inclCommit, inclHosts, envSort, inclCoolifyVars bool
		imgScan                                         bool
		gpuDrv, gpuCnt, gpuDevs, gpuOpts                string
		custIntName, custPrefix, certResolver           string
		imgsToKeep                                      int
	)
	err := s.pool.QueryRow(ctx, `
		SELECT
			COALESCE(is_static,FALSE), COALESCE(is_spa,FALSE),
			COALESCE(is_git_submodules_enabled,TRUE), COALESCE(is_git_lfs_enabled,TRUE), COALESCE(is_git_shallow_clone_enabled,TRUE),
			COALESCE(is_auto_deploy_enabled,TRUE), COALESCE(is_force_https_enabled,TRUE), COALESCE(is_debug_enabled,FALSE), COALESCE(is_preview_deployments_enabled,FALSE),
			COALESCE(is_pr_deployments_public_enabled,FALSE), COALESCE(is_log_drain_enabled,FALSE), COALESCE(is_gpu_enabled,FALSE), COALESCE(is_include_timestamps,FALSE),
			COALESCE(is_swarm_only_worker_nodes,FALSE), COALESCE(is_raw_compose_deployment_enabled,FALSE), COALESCE(is_build_server_enabled,FALSE), COALESCE(is_consistent_container_name_enabled,FALSE),
			COALESCE(is_gzip_enabled,TRUE), COALESCE(is_stripprefix_enabled,TRUE), COALESCE(is_permanent_redirect_enabled,FALSE), COALESCE(connect_to_docker_network,FALSE),
			COALESCE(is_container_label_escape_enabled,TRUE), COALESCE(is_container_label_readonly_enabled,FALSE), COALESCE(is_preserve_repository_enabled,FALSE),
			COALESCE(disable_build_cache,FALSE), COALESCE(use_build_secrets,FALSE), COALESCE(redact_all_env_values_in_logs,FALSE), COALESCE(inject_build_args_to_dockerfile,TRUE),
			COALESCE(include_source_commit_in_build,FALSE), COALESCE(include_network_hosts_in_build,TRUE), COALESCE(is_env_sorting_enabled,FALSE), COALESCE(include_coolify_default_variables,TRUE),
			COALESCE(is_image_scan_enabled,TRUE),
			COALESCE(gpu_driver,''), COALESCE(gpu_count,''), COALESCE(gpu_device_ids,''), COALESCE(gpu_options,''),
			COALESCE(custom_internal_name,''), COALESCE(custom_container_name_prefix,''), COALESCE(traefik_certificate_resolver,''),
			COALESCE(docker_images_to_keep,2)
		FROM application_settings
		WHERE application_id = $1
	`, appID).Scan(
		&isStatic, &isSPA, &submodules, &lfs, &shallow,
		&autoDeploy, &forceHTTPS, &debugMode, &prevDeploys,
		&prPublic, &logDrain, &gpuEnabled, &inclTS,
		&swarmOnly, &rawCompose, &buildServer, &consNames,
		&gzipEn, &stripPrefix, &permRedir, &connectNet,
		&labelEscape, &labelReadonly, &preserveRepo,
		&disableCache, &buildSecrets, &redactAll, &injArgs,
		&inclCommit, &inclHosts, &envSort, &inclCoolifyVars,
		&imgScan, &gpuDrv, &gpuCnt, &gpuDevs, &gpuOpts,
		&custIntName, &custPrefix, &certResolver, &imgsToKeep,
	)
	if err != nil {
		return out
	}
	b := strconv.FormatBool
	out["is_static"] = b(isStatic)
	out["is_spa"] = b(isSPA)
	out["is_git_submodules_enabled"] = b(submodules)
	out["is_git_lfs_enabled"] = b(lfs)
	out["is_git_shallow_clone_enabled"] = b(shallow)
	out["is_auto_deploy_enabled"] = b(autoDeploy)
	out["is_force_https_enabled"] = b(forceHTTPS)
	out["is_debug_enabled"] = b(debugMode)
	out["is_preview_deployments_enabled"] = b(prevDeploys)
	out["is_pr_deployments_public_enabled"] = b(prPublic)
	out["is_log_drain_enabled"] = b(logDrain)
	out["is_gpu_enabled"] = b(gpuEnabled)
	out["is_include_timestamps"] = b(inclTS)
	out["is_swarm_only_worker_nodes"] = b(swarmOnly)
	out["is_raw_compose_deployment_enabled"] = b(rawCompose)
	out["is_build_server_enabled"] = b(buildServer)
	out["is_consistent_container_name_enabled"] = b(consNames)
	out["is_gzip_enabled"] = b(gzipEn)
	out["is_stripprefix_enabled"] = b(stripPrefix)
	out["is_permanent_redirect_enabled"] = b(permRedir)
	out["connect_to_docker_network"] = b(connectNet)
	out["is_container_label_escape_enabled"] = b(labelEscape)
	out["is_container_label_readonly_enabled"] = b(labelReadonly)
	out["is_preserve_repository_enabled"] = b(preserveRepo)
	out["disable_build_cache"] = b(disableCache)
	out["use_build_secrets"] = b(buildSecrets)
	out["redact_all_env_values_in_logs"] = b(redactAll)
	out["inject_build_args_to_dockerfile"] = b(injArgs)
	out["include_source_commit_in_build"] = b(inclCommit)
	out["include_network_hosts_in_build"] = b(inclHosts)
	out["is_env_sorting_enabled"] = b(envSort)
	out["include_coolify_default_variables"] = b(inclCoolifyVars)
	out["is_image_scan_enabled"] = b(imgScan)
	out["docker_images_to_keep"] = strconv.Itoa(imgsToKeep)
	if gpuDrv != "" {
		out["gpu_driver"] = gpuDrv
	}
	if gpuCnt != "" {
		out["gpu_count"] = gpuCnt
	}
	if gpuDevs != "" {
		out["gpu_device_ids"] = gpuDevs
	}
	if gpuOpts != "" {
		out["gpu_options"] = gpuOpts
	}
	if custIntName != "" {
		out["custom_internal_name"] = custIntName
	}
	if custPrefix != "" {
		out["custom_container_name_prefix"] = custPrefix
	}
	if certResolver != "" {
		out["traefik_certificate_resolver"] = certResolver
	}
	return out
}

func settingBool(m map[string]string, key string, def bool) bool {
	if v, ok := m[key]; ok {
		return strings.EqualFold(strings.TrimSpace(v), "true") || strings.TrimSpace(v) == "1"
	}
	return def
}

func settingInt(m map[string]string, key string, def int) int {
	if v, ok := m[key]; ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func upsertApplicationSettingsInTx(ctx context.Context, tx pgx.Tx, appID int64, m map[string]string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO application_settings (
			application_id, is_static, is_spa,
			is_git_submodules_enabled, is_git_lfs_enabled, is_git_shallow_clone_enabled,
			is_auto_deploy_enabled, is_force_https_enabled, is_debug_enabled, is_preview_deployments_enabled,
			is_pr_deployments_public_enabled, is_log_drain_enabled, is_gpu_enabled,
			gpu_driver, gpu_count, gpu_device_ids, gpu_options,
			is_include_timestamps, is_swarm_only_worker_nodes, is_raw_compose_deployment_enabled,
			is_build_server_enabled, is_consistent_container_name_enabled,
			is_gzip_enabled, is_stripprefix_enabled, is_permanent_redirect_enabled, connect_to_docker_network,
			custom_internal_name, custom_container_name_prefix,
			is_container_label_escape_enabled, is_container_label_readonly_enabled, is_preserve_repository_enabled,
			disable_build_cache, use_build_secrets, redact_all_env_values_in_logs, inject_build_args_to_dockerfile,
			include_source_commit_in_build, include_network_hosts_in_build, is_env_sorting_enabled, include_coolify_default_variables,
			traefik_certificate_resolver, docker_images_to_keep, is_image_scan_enabled
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,
			NULLIF($14,''),NULLIF($15,''),NULLIF($16,''),NULLIF($17,''),
			$18,$19,$20,$21,$22,$23,$24,$25,$26,
			NULLIF($27,''),NULLIF($28,''),
			$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,NULLIF($40,''),$41,$42
		)
		ON CONFLICT (application_id) DO UPDATE SET
			is_static = EXCLUDED.is_static,
			is_spa = EXCLUDED.is_spa,
			is_build_server_enabled = EXCLUDED.is_build_server_enabled,
			is_preserve_repository_enabled = EXCLUDED.is_preserve_repository_enabled,
			is_container_label_escape_enabled = EXCLUDED.is_container_label_escape_enabled,
			is_container_label_readonly_enabled = EXCLUDED.is_container_label_readonly_enabled,
			is_auto_deploy_enabled = EXCLUDED.is_auto_deploy_enabled,
			is_force_https_enabled = EXCLUDED.is_force_https_enabled,
			is_debug_enabled = EXCLUDED.is_debug_enabled,
			is_preview_deployments_enabled = EXCLUDED.is_preview_deployments_enabled,
			use_build_secrets = EXCLUDED.use_build_secrets,
			disable_build_cache = EXCLUDED.disable_build_cache,
			inject_build_args_to_dockerfile = EXCLUDED.inject_build_args_to_dockerfile,
			redact_all_env_values_in_logs = EXCLUDED.redact_all_env_values_in_logs,
			is_gpu_enabled = EXCLUDED.is_gpu_enabled,
			gpu_driver = COALESCE(EXCLUDED.gpu_driver, application_settings.gpu_driver),
			gpu_count = COALESCE(EXCLUDED.gpu_count, application_settings.gpu_count),
			gpu_device_ids = COALESCE(EXCLUDED.gpu_device_ids, application_settings.gpu_device_ids),
			gpu_options = COALESCE(EXCLUDED.gpu_options, application_settings.gpu_options),
			custom_internal_name = COALESCE(EXCLUDED.custom_internal_name, application_settings.custom_internal_name),
			custom_container_name_prefix = COALESCE(EXCLUDED.custom_container_name_prefix, application_settings.custom_container_name_prefix),
			traefik_certificate_resolver = COALESCE(EXCLUDED.traefik_certificate_resolver, application_settings.traefik_certificate_resolver),
			docker_images_to_keep = EXCLUDED.docker_images_to_keep,
			updated_at = NOW()
	`,
		appID,
		settingBool(m, "is_static", false),
		settingBool(m, "is_spa", false),
		settingBool(m, "is_git_submodules_enabled", true),
		settingBool(m, "is_git_lfs_enabled", true),
		settingBool(m, "is_git_shallow_clone_enabled", true),
		settingBool(m, "is_auto_deploy_enabled", true),
		settingBool(m, "is_force_https_enabled", true),
		settingBool(m, "is_debug_enabled", false),
		settingBool(m, "is_preview_deployments_enabled", false),
		settingBool(m, "is_pr_deployments_public_enabled", false),
		settingBool(m, "is_log_drain_enabled", false),
		settingBool(m, "is_gpu_enabled", false),
		strings.TrimSpace(m["gpu_driver"]),
		strings.TrimSpace(m["gpu_count"]),
		strings.TrimSpace(m["gpu_device_ids"]),
		strings.TrimSpace(m["gpu_options"]),
		settingBool(m, "is_include_timestamps", false),
		settingBool(m, "is_swarm_only_worker_nodes", false),
		settingBool(m, "is_raw_compose_deployment_enabled", false),
		settingBool(m, "is_build_server_enabled", false),
		settingBool(m, "is_consistent_container_name_enabled", false),
		settingBool(m, "is_gzip_enabled", true),
		settingBool(m, "is_stripprefix_enabled", true),
		settingBool(m, "is_permanent_redirect_enabled", false),
		settingBool(m, "connect_to_docker_network", false),
		strings.TrimSpace(m["custom_internal_name"]),
		strings.TrimSpace(m["custom_container_name_prefix"]),
		settingBool(m, "is_container_label_escape_enabled", true),
		settingBool(m, "is_container_label_readonly_enabled", false),
		settingBool(m, "is_preserve_repository_enabled", false),
		settingBool(m, "disable_build_cache", false),
		settingBool(m, "use_build_secrets", false),
		settingBool(m, "redact_all_env_values_in_logs", false),
		settingBool(m, "inject_build_args_to_dockerfile", true),
		settingBool(m, "include_source_commit_in_build", false),
		settingBool(m, "include_network_hosts_in_build", true),
		settingBool(m, "is_env_sorting_enabled", false),
		settingBool(m, "include_coolify_default_variables", true),
		strings.TrimSpace(m["traefik_certificate_resolver"]),
		settingInt(m, "docker_images_to_keep", 2),
		settingBool(m, "is_image_scan_enabled", true),
	)
	return err
}

func (s *PostgresService) UpdateSettings(ctx context.Context, appID int64, patch map[string]string) error {
	if len(patch) == 0 {
		return nil
	}
	merged := s.loadApplicationSettingsMap(ctx, appID)
	for k, v := range patch {
		merged[k] = v
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := upsertApplicationSettingsInTx(ctx, tx, appID, merged); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresService) Create(ctx context.Context, principal auth.Principal, in CreateApplicationInput) (Application, error) {
	var envID, projTeamID int64
	var envUUID, projUUID string
	var err error
	if strings.TrimSpace(in.EnvironmentUUID) != "" {
		err = s.pool.QueryRow(ctx, `
			SELECT e.id, e.uuid, p.uuid, p.team_id
			FROM environments e
			JOIN projects p ON p.id = e.project_id
			WHERE e.uuid = $1
		`, in.EnvironmentUUID).Scan(&envID, &envUUID, &projUUID, &projTeamID)
	} else if strings.TrimSpace(in.ProjectUUID) != "" {
		if strings.TrimSpace(in.EnvironmentName) != "" {
			err = s.pool.QueryRow(ctx, `
				SELECT e.id, e.uuid, p.uuid, p.team_id
				FROM environments e
				JOIN projects p ON p.id = e.project_id
				WHERE p.uuid = $1 AND LOWER(e.name) = LOWER($2)
				ORDER BY e.id ASC LIMIT 1
			`, strings.TrimSpace(in.ProjectUUID), strings.TrimSpace(in.EnvironmentName)).Scan(&envID, &envUUID, &projUUID, &projTeamID)
		} else {
			err = s.pool.QueryRow(ctx, `
				SELECT e.id, e.uuid, p.uuid, p.team_id
				FROM environments e
				JOIN projects p ON p.id = e.project_id
				WHERE p.uuid = $1
				ORDER BY e.id ASC LIMIT 1
			`, strings.TrimSpace(in.ProjectUUID)).Scan(&envID, &envUUID, &projUUID, &projTeamID)
		}
	} else {
		err = pgx.ErrNoRows
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, errors.New("environment not found")
	}
	if err != nil {
		return Application{}, err
	}
	if err := s.authz.Check(ctx, principal, access.ActionCreate, access.ResourceRef{
		Type: "application", ProjectUUID: projUUID, EnvironmentUUID: envUUID, TeamID: &projTeamID, APISegment: "applications",
	}); err != nil {
		return Application{}, err
	}

	serverUUID := strings.TrimSpace(in.ServerUUID)
	if serverUUID == "" {
		serverUUID = "localhost"
	}
	var srvID, srvTeamID, destID int64
	err = s.pool.QueryRow(ctx, `
		SELECT s.id, s.team_id, COALESCE(sd.id, 0)
		FROM servers s
		LEFT JOIN standalone_dockers sd ON sd.server_id = s.id
		WHERE s.uuid = $1 AND s.deleted_at IS NULL
		ORDER BY sd.id ASC
		LIMIT 1
	`, serverUUID).Scan(&srvID, &srvTeamID, &destID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Application{}, errors.New("target server not found")
	}
	if err != nil {
		return Application{}, err
	}
	if srvTeamID != projTeamID {
		return Application{}, fmt.Errorf("%w: cross-team server placement denied", access.ErrForbidden)
	}
	if in.DestinationUUID != "" {
		if err := s.pool.QueryRow(ctx, `SELECT id FROM standalone_dockers WHERE uuid=$1 AND server_id=$2`, strings.TrimSpace(in.DestinationUUID), srvID).Scan(&destID); err != nil {
			return Application{}, errors.New("destination does not belong to the target server")
		}
	}
	if err := s.authz.Check(ctx, principal, access.ActionPlaceResource, access.ResourceRef{
		Type: "server", ID: srvID, UUID: serverUUID, TeamID: &srvTeamID, IsLocalhost: srvID == 0, APISegment: "servers",
	}); err != nil {
		return Application{}, err
	}

	name := compat.SanitizeString(in.Name)
	if name == "" {
		return Application{}, errors.New("application name is required")
	}
	bp := strings.TrimSpace(in.BuildPack)
	if bp == "" {
		bp = "dockerimage"
	}
	branch := strings.TrimSpace(in.GitBranch)
	if branch == "" {
		branch = "main"
	}
	row := compat.ModelRow{Name: name}
	_ = s.hooks.ApplyModelDefaults("application", &row)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Application{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var appID int64
	var updatedAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO applications (
			uuid, name, description, fqdn, git_repository, git_branch, build_pack,
			docker_registry_image_name, docker_registry_image_tag, dockerfile_location,
			docker_compose_raw, ports_exposes, status, environment_id, destination_id,
			manual_webhook_secret_github, dockerfile
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'exited',$13,$14,$15,$16)
		RETURNING id, updated_at
	`, row.UUID, name, compat.SanitizeString(in.Description), strings.TrimSpace(in.FQDN),
		strings.TrimSpace(in.GitRepository), branch, bp,
		strings.TrimSpace(in.DockerRegistryImageName), strings.TrimSpace(in.DockerRegistryImageTag),
		strings.TrimSpace(in.DockerfileLocation), in.DockerComposeRaw, strings.TrimSpace(in.PortsExposes),
		envID, destID, in.WebhookSecretGitHub, in.Dockerfile,
	).Scan(&appID, &updatedAt)
	if err != nil {
		return Application{}, err
	}
	if err := upsertApplicationSettingsInTx(ctx, tx, appID, in.Settings); err != nil {
		return Application{}, err
	}

	if s.outbox != nil {
		_ = s.outbox.AppendInTx(ctx, tx, realtime.DomainEvent{
			EventID:           fmt.Sprintf("app.created-%s-%d", row.UUID, updatedAt.UnixNano()),
			SchemaVersion:     "v1",
			Producer:          "go-controlplane",
			TeamID:            projTeamID,
			ResourceType:      "application",
			ResourceUUID:      row.UUID,
			AggregateRevision: 1,
			OwnershipEpoch:    1,
			MutationType:      "application.updated",
			CommittedAt:       updatedAt.UTC(),
			Payload:           map[string]any{"uuid": row.UUID, "name": name},
		})
	}
	if err := tx.Commit(ctx); err != nil {
		return Application{}, err
	}
	return s.loadByUUID(ctx, row.UUID)
}

func (s *PostgresService) SetEnvVar(ctx context.Context, principal auth.Principal, appUUID string, in EnvVarInput) (EnvVarDTO, error) {
	a, err := s.loadByUUID(ctx, appUUID)
	if err != nil {
		return EnvVarDTO{}, err
	}
	teamID := a.TeamID
	if err := s.authz.Check(ctx, principal, access.ActionUpdate, access.ResourceRef{
		Type: "application", ID: a.ID, UUID: a.UUID, ProjectUUID: a.ProjectUUID, EnvironmentUUID: a.EnvironmentUUID,
		TeamID: &teamID, APISegment: "applications",
	}); err != nil {
		return EnvVarDTO{}, err
	}
	if strings.TrimSpace(in.Key) == "" {
		return EnvVarDTO{}, errors.New("env var key is required")
	}
	stored := in.Value
	if s.cipher != nil {
		enc, err := s.cipher.EncryptLaravelPayload([]byte(in.Value), false)
		if err != nil {
			return EnvVarDTO{}, err
		}
		stored = enc
	}
	evUUID := s.hooks.NewCUID2()
	var id int64
	err = s.pool.QueryRow(ctx, `
		INSERT INTO environment_variables (
			uuid, key, value, is_preview, is_buildtime, is_runtime, is_literal, is_shown_once,
			resourceable_type, resourceable_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'App\Models\Application',$9)
		RETURNING id
	`, evUUID, in.Key, stored, in.IsPreview, in.IsBuildtime, in.IsRuntime, in.IsLiteral, in.IsShownOnce, a.ID).Scan(&id)
	if err != nil {
		return EnvVarDTO{}, err
	}
	dto := EnvVarDTO{
		ID: id, UUID: evUUID, Key: in.Key, Value: in.Value,
		IsPreview: in.IsPreview, IsBuildtime: in.IsBuildtime, IsRuntime: in.IsRuntime,
		IsLiteral: in.IsLiteral, IsShownOnce: in.IsShownOnce,
	}
	if dto.IsShownOnce {
		dto.Value = ""
	}
	return dto, nil
}

func (s *PostgresService) ListEnvVars(ctx context.Context, principal auth.Principal, appUUID string) ([]EnvVarDTO, error) {
	a, err := s.loadByUUID(ctx, appUUID)
	if err != nil {
		return nil, err
	}
	teamID := a.TeamID
	ref := access.ResourceRef{
		Type: "application", ID: a.ID, UUID: a.UUID, ProjectUUID: a.ProjectUUID, EnvironmentUUID: a.EnvironmentUUID,
		TeamID: &teamID, APISegment: "applications",
	}
	if err := s.authz.Check(ctx, principal, access.ActionRead, ref); err != nil {
		return nil, err
	}
	// MCP NEVER returns environment variable values (per EnvironmentVariable.php docblock)
	canReadSensitive := principal.AuthMethod != "mcp" && s.authz.Check(ctx, principal, access.ActionReadSensitive, ref) == nil

	rows, err := s.pool.Query(ctx, `
		SELECT id, uuid, key, value, is_preview, is_buildtime, is_runtime, is_literal, is_shown_once
		FROM environment_variables
		WHERE resourceable_type = 'App\Models\Application' AND resourceable_id = $1
		ORDER BY id ASC
	`, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]EnvVarDTO, 0)
	for rows.Next() {
		var d EnvVarDTO
		var rawVal *string
		if err := rows.Scan(&d.ID, &d.UUID, &d.Key, &rawVal, &d.IsPreview, &d.IsBuildtime, &d.IsRuntime, &d.IsLiteral, &d.IsShownOnce); err != nil {
			return nil, err
		}
		if canReadSensitive && !d.IsShownOnce {
			d.Value = s.decryptOpt(rawVal)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// EnqueueDeployment resolves DeploymentContext, verifies authorization, and atomically records
// the deployment queue row + durable go_job_operations job + outbox event in ONE PostgreSQL transaction.
func (s *PostgresService) EnqueueDeployment(ctx context.Context, principal auth.Principal, appUUID string, forceRebuild bool, isWebhook bool, isAPI bool) (DeploymentRecord, error) {
	dctx, err := s.resolver.Resolve(ctx, principal, appUUID, nil)
	if err != nil {
		return DeploymentRecord{}, err
	}
	a, err := s.loadByUUID(ctx, appUUID)
	if err != nil {
		return DeploymentRecord{}, err
	}
	depUUID := s.hooks.NewCUID2()
	opID := "deploy-" + depUUID

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeploymentRecord{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var createdAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO application_deployment_queues (
			deployment_uuid, application_id, application_name, server_id, server_name,
			status, force_rebuild, is_webhook, is_api
		) VALUES ($1,$2,$3,$4,$5,'queued',$6,$7,$8)
		RETURNING created_at
	`, depUUID, a.ID, a.Name, a.ServerID, a.ServerUUID, forceRebuild, isWebhook, isAPI).Scan(&createdAt)
	if err != nil {
		return DeploymentRecord{}, err
	}

	if err := s.jobCoord.AdmitOperationInTx(ctx, tx, jobs.DurableJobSpec{
		JobOperation: jobs.JobOperation{
			JobID:        "job-" + depUUID,
			OperationID:  opID,
			AttemptID:    "att-0",
			ResourceUUID: a.UUID,
			ServerUUID:   dctx.ServerUUID,
			TeamID:       dctx.ResourceTeamID,
			Status:       jobs.StatusQueued,
			MaxAttempts:  3,
		},
		JobType: "deploy",
		Payload: map[string]any{
			"deployment_uuid":  depUUID,
			"application_uuid": a.UUID,
			"application_id":   a.ID,
			"build_pack":       dctx.BuildPack,
			"input_revision":   dctx.InputRevision,
			"force_rebuild":    forceRebuild,
			"image":            dctx.Settings["docker_registry_image_name"],
			"tag":              dctx.Settings["docker_registry_image_tag"],
		},
	}); err != nil {
		return DeploymentRecord{}, err
	}

	if s.outbox != nil {
		if err := s.outbox.AppendInTx(ctx, tx, realtime.DomainEvent{
			EventID:           fmt.Sprintf("deploy.queued-%s", depUUID),
			SchemaVersion:     "v1",
			Producer:          "go-controlplane",
			TeamID:            dctx.ResourceTeamID,
			ResourceType:      "application",
			ResourceUUID:      a.UUID,
			AggregateRevision: 1,
			OwnershipEpoch:    1,
			MutationType:      "application.status_changed",
			CommittedAt:       createdAt.UTC(),
			Payload:           map[string]any{"deployment_uuid": depUUID, "operation_id": opID, "status": "queued"},
		}); err != nil {
			return DeploymentRecord{}, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return DeploymentRecord{}, err
	}
	return DeploymentRecord{
		DeploymentUUID:  depUUID,
		OperationID:     opID,
		ApplicationID:   a.ID,
		ApplicationUUID: a.UUID,
		ServerUUID:      dctx.ServerUUID,
		Status:          "queued",
		ForceRebuild:    forceRebuild,
		IsWebhook:       isWebhook,
		IsAPI:           isAPI,
		CreatedAt:       createdAt,
		UpdatedAt:       createdAt,
	}, nil
}

func (s *PostgresService) ListDeploymentsByApplication(ctx context.Context, principal auth.Principal, appUUID string) ([]DeploymentRecord, error) {
	a, err := s.GetByUUID(ctx, principal, appUUID)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT deployment_uuid, application_id, COALESCE(server_name, 'localhost'), status,
		       force_rebuild, is_webhook, is_api, created_at, updated_at
		FROM application_deployment_queues
		WHERE application_id = $1
		ORDER BY id DESC
		LIMIT 50
	`, a.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DeploymentRecord, 0)
	for rows.Next() {
		var d DeploymentRecord
		d.ApplicationUUID = a.UUID
		if err := rows.Scan(&d.DeploymentUUID, &d.ApplicationID, &d.ServerUUID, &d.Status,
			&d.ForceRebuild, &d.IsWebhook, &d.IsAPI, &d.CreatedAt, &d.UpdatedAt); err != nil {
			return nil, err
		}
		d.OperationID = "deploy-" + d.DeploymentUUID
		out = append(out, d)
	}
	return out, rows.Err()
}
