package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Connect creates a verified pgxpool connection to PostgreSQL.
func Connect(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres dsn: %w", err)
	}
	cfg.MaxConns = 16
	cfg.MinConns = 1
	cfg.MaxConnLifetime = 30 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	return pool, nil
}

// EnsureGoAdditiveSchema creates ONLY expand-only Go runtime tables (`go_*`) that frozen Laravel ignores.
// Existing Coolify v4 tables are never destructively altered, preserving rollback compatibility.
func EnsureGoAdditiveSchema(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS go_schema_migrations (
			name TEXT PRIMARY KEY,
			expand_only BOOLEAN NOT NULL DEFAULT TRUE,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS go_sessions (
			id TEXT PRIMARY KEY,
			user_id BIGINT NOT NULL,
			team_id BIGINT NOT NULL DEFAULT 0,
			csrf_token TEXT NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			revoked BOOLEAN NOT NULL DEFAULT FALSE,
			two_fa_valid BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_go_sessions_user_id ON go_sessions (user_id)`,
		`CREATE TABLE IF NOT EXISTS go_outbox_events (
			id BIGSERIAL PRIMARY KEY,
			event_id TEXT NOT NULL UNIQUE,
			schema_version TEXT NOT NULL DEFAULT 'v1',
			producer TEXT NOT NULL,
			team_id BIGINT NOT NULL,
			resource_type TEXT NOT NULL,
			resource_uuid TEXT NOT NULL,
			aggregate_revision BIGINT NOT NULL DEFAULT 1,
			ownership_epoch BIGINT NOT NULL DEFAULT 1,
			mutation_type TEXT NOT NULL,
			payload_json JSONB NOT NULL DEFAULT '{}'::jsonb,
			committed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			dispatched_at TIMESTAMPTZ NULL,
			attempts INT NOT NULL DEFAULT 0,
			dead_letter BOOLEAN NOT NULL DEFAULT FALSE,
			last_error TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_go_outbox_pending ON go_outbox_events (id) WHERE dispatched_at IS NULL AND dead_letter = FALSE`,
		`CREATE TABLE IF NOT EXISTS go_job_operations (
			operation_id TEXT PRIMARY KEY,
			job_id TEXT NOT NULL,
			attempt_id TEXT NOT NULL,
			job_type TEXT NOT NULL DEFAULT 'deploy',
			ownership_epoch BIGINT NOT NULL DEFAULT 0,
			resource_uuid TEXT NOT NULL,
			server_uuid TEXT NOT NULL,
			team_id BIGINT NOT NULL,
			status TEXT NOT NULL DEFAULT 'queued',
			payload_json JSONB NOT NULL DEFAULT '{}'::jsonb,
			attempts INT NOT NULL DEFAULT 0,
			max_attempts INT NOT NULL DEFAULT 3,
			lease_until TIMESTAMPTZ NULL,
			worker_id TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_go_job_ops_claim ON go_job_operations (status, lease_until, updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_go_job_ops_resource ON go_job_operations (resource_uuid, created_at DESC)`,
		`CREATE TABLE IF NOT EXISTS go_schedule_leases (
			schedule_id BIGINT NOT NULL,
			minute_bucket TEXT NOT NULL,
			worker_id TEXT NOT NULL,
			acquired_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (schedule_id, minute_bucket)
		)`,
		`CREATE TABLE IF NOT EXISTS go_totp_used_steps (
			user_id BIGINT NOT NULL,
			time_step BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (user_id, time_step)
		)`,
		`CREATE TABLE IF NOT EXISTS go_operation_logs (
			id BIGSERIAL PRIMARY KEY,
			resource_key TEXT NOT NULL,
			operation_id TEXT NOT NULL DEFAULT '',
			stream TEXT NOT NULL DEFAULT 'stdout',
			text TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE INDEX IF NOT EXISTS idx_go_op_logs_res ON go_operation_logs (resource_key, id)`,
		`CREATE TABLE IF NOT EXISTS go_webhook_deliveries (
			provider TEXT NOT NULL,
			delivery_id TEXT NOT NULL,
			received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (provider, delivery_id)
		)`,
		`CREATE TABLE IF NOT EXISTS go_terminal_tickets (
			token TEXT PRIMARY KEY,
			user_id BIGINT NOT NULL,
			team_id BIGINT NOT NULL,
			target_json JSONB NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			redeemed_at TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS go_oauth_states (
			state TEXT PRIMARY KEY,
			provider TEXT NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			used_at TIMESTAMPTZ NULL
		)`,
		`CREATE TABLE IF NOT EXISTS go_webauthn_challenges (
			challenge_id TEXT PRIMARY KEY,
			user_id BIGINT NOT NULL,
			expires_at TIMESTAMPTZ NOT NULL,
			used_at TIMESTAMPTZ NULL
		)`,
		`CREATE TABLE IF NOT EXISTS go_remote_journals (
			operation_id TEXT PRIMARY KEY,
			server_uuid TEXT NOT NULL,
			journal_json JSONB NOT NULL,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS go_transfer_archives (
			server_uuid TEXT PRIMARY KEY,
			transferred_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			bundle_json JSONB NOT NULL DEFAULT '{}'::jsonb,
			checklist_json JSONB NOT NULL DEFAULT '[]'::jsonb
		)`,
		`CREATE TABLE IF NOT EXISTS go_notification_configs (
			id BIGSERIAL PRIMARY KEY,
			team_id BIGINT NOT NULL,
			channel TEXT NOT NULL,
			enabled BOOLEAN NOT NULL DEFAULT FALSE,
			config_enc TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (team_id, channel)
		)`,
		`CREATE TABLE IF NOT EXISTS go_notification_deliveries (
			id BIGSERIAL PRIMARY KEY,
			team_id BIGINT NOT NULL,
			channel TEXT NOT NULL,
			event TEXT NOT NULL,
			summary TEXT NOT NULL,
			status TEXT NOT NULL,
			http_status INT NOT NULL DEFAULT 0,
			error_text TEXT NOT NULL DEFAULT '',
			delivered_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
	}

	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("apply go additive schema: %w", err)
		}
	}

	migrations := []string{
		"2026_11_19_000001_go_sessions",
		"2026_11_19_000002_go_outbox_events",
		"2026_11_19_000003_go_job_operations",
		"2026_11_19_000004_go_schedule_leases",
		"2026_11_19_000005_go_operation_logs",
		"2026_11_19_000006_go_runtime_aux_tables",
	}
	for _, name := range migrations {
		if _, err := pool.Exec(ctx, `INSERT INTO go_schema_migrations (name, expand_only) VALUES ($1, TRUE) ON CONFLICT (name) DO NOTHING`, name); err != nil {
			return fmt.Errorf("record go migration %s: %w", name, err)
		}
	}
	return ensureExtendedGoAdditiveTables(ctx, pool)
}

// EnsureBaselineSchema creates Coolify v4 core PostgreSQL tables if missing (for clean install & isolated tests).
// When running against an existing Laravel Coolify database, IF NOT EXISTS leaves every existing column untouched.
func EnsureBaselineSchema(ctx context.Context, pool *pgxpool.Pool) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id BIGSERIAL PRIMARY KEY,
			name TEXT NOT NULL DEFAULT '',
			email TEXT NOT NULL UNIQUE,
			password TEXT NOT NULL DEFAULT '',
			email_verified_at TIMESTAMPTZ NULL,
			force_password_reset BOOLEAN NOT NULL DEFAULT FALSE,
			two_factor_secret TEXT NULL,
			two_factor_recovery_codes TEXT NULL,
			two_factor_confirmed_at TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS teams (
			id BIGSERIAL PRIMARY KEY,
			name TEXT NOT NULL,
			description TEXT NULL,
			personal_team BOOLEAN NOT NULL DEFAULT FALSE,
			show_boarding BOOLEAN NOT NULL DEFAULT FALSE,
			is_two_factor_required BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS team_user (
			id BIGSERIAL PRIMARY KEY,
			team_id BIGINT NOT NULL,
			user_id BIGINT NOT NULL,
			role TEXT NOT NULL DEFAULT 'member',
			access JSONB NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			UNIQUE (team_id, user_id)
		)`,
		`CREATE TABLE IF NOT EXISTS team_invitations (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			team_id BIGINT NOT NULL,
			email TEXT NOT NULL,
			role TEXT NOT NULL DEFAULT 'member',
			link TEXT NOT NULL DEFAULT '',
			via TEXT NOT NULL DEFAULT 'link',
			access JSONB NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS personal_access_tokens (
			id BIGSERIAL PRIMARY KEY,
			tokenable_type TEXT NOT NULL DEFAULT 'App\Models\User',
			tokenable_id BIGINT NOT NULL,
			name TEXT NOT NULL,
			token VARCHAR(64) NOT NULL UNIQUE,
			team_id TEXT NOT NULL DEFAULT '0',
			abilities TEXT NULL,
			scopes JSONB NULL,
			last_used_at TIMESTAMPTZ NULL,
			expires_at TIMESTAMPTZ NULL,
			paused_at TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS passkeys (
			id BIGSERIAL PRIMARY KEY,
			authenticatable_id BIGINT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			credential_id TEXT NOT NULL UNIQUE,
			data JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS instance_settings (
			id BIGINT PRIMARY KEY,
			fqdn TEXT NULL,
			instance_name TEXT NOT NULL DEFAULT 'Coolify',
			is_api_enabled BOOLEAN NOT NULL DEFAULT TRUE,
			is_mcp_server_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			is_registration_enabled BOOLEAN NOT NULL DEFAULT TRUE,
			allowed_ips TEXT NULL,
			instance_timezone TEXT NOT NULL DEFAULT 'UTC',
			smtp_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			smtp_host TEXT NULL,
			smtp_port INT NULL,
			smtp_username TEXT NULL,
			smtp_password TEXT NULL,
			smtp_from_address TEXT NULL,
			smtp_from_name TEXT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS private_keys (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			private_key TEXT NOT NULL,
			fingerprint TEXT NULL,
			is_git_related BOOLEAN NOT NULL DEFAULT FALSE,
			team_id BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS servers (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			ip TEXT NOT NULL,
			port INT NOT NULL DEFAULT 22,
			"user" TEXT NOT NULL DEFAULT 'root',
			team_id BIGINT NOT NULL,
			private_key_id BIGINT NOT NULL DEFAULT 0,
			proxy JSONB NOT NULL DEFAULT '{"type":"TRAEFIK"}'::jsonb,
			server_metadata JSONB NULL,
			deleted_at TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS server_settings (
			id BIGSERIAL PRIMARY KEY,
			server_id BIGINT NOT NULL UNIQUE,
			is_reachable BOOLEAN NOT NULL DEFAULT FALSE,
			is_usable BOOLEAN NOT NULL DEFAULT FALSE,
			force_disabled BOOLEAN NOT NULL DEFAULT FALSE,
			is_terminal_enabled BOOLEAN NOT NULL DEFAULT TRUE,
			concurrent_builds INT NOT NULL DEFAULT 2,
			wildcard_domain TEXT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS standalone_dockers (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT 'standalone-docker',
			network TEXT NOT NULL DEFAULT 'coolify',
			server_id BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS projects (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			team_id BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS environments (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			project_id BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS applications (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			fqdn TEXT NULL,
			git_repository TEXT NULL,
			git_branch TEXT NOT NULL DEFAULT 'main',
			build_pack TEXT NOT NULL DEFAULT 'dockerimage',
			docker_registry_image_name TEXT NULL,
			docker_registry_image_tag TEXT NULL,
			dockerfile_location TEXT NULL DEFAULT '/Dockerfile',
			docker_compose_raw TEXT NULL,
			docker_compose TEXT NULL,
			docker_compose_domains TEXT NULL,
			ports_exposes TEXT NULL DEFAULT '80',
			status TEXT NOT NULL DEFAULT 'exited',
			environment_id BIGINT NOT NULL,
			destination_id BIGINT NOT NULL DEFAULT 0,
			destination_type TEXT NOT NULL DEFAULT 'App\Models\StandaloneDocker',
			private_key_id BIGINT NULL,
			source_id BIGINT NULL,
			source_type TEXT NULL,
			manual_webhook_secret_github TEXT NULL,
			manual_webhook_secret_gitlab TEXT NULL,
			manual_webhook_secret_bitbucket TEXT NULL,
			manual_webhook_secret_gitea TEXT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS application_settings (
			id BIGSERIAL PRIMARY KEY,
			application_id BIGINT NOT NULL UNIQUE,
			is_static BOOLEAN NOT NULL DEFAULT FALSE,
			is_spa BOOLEAN NOT NULL DEFAULT FALSE,
			is_git_submodules_enabled BOOLEAN NOT NULL DEFAULT TRUE,
			is_git_lfs_enabled BOOLEAN NOT NULL DEFAULT TRUE,
			is_git_shallow_clone_enabled BOOLEAN NOT NULL DEFAULT TRUE,
			is_auto_deploy_enabled BOOLEAN NOT NULL DEFAULT TRUE,
			is_force_https_enabled BOOLEAN NOT NULL DEFAULT TRUE,
			is_debug_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			is_preview_deployments_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			is_pr_deployments_public_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			is_log_drain_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			is_gpu_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			gpu_driver TEXT NULL,
			gpu_count TEXT NULL,
			gpu_device_ids TEXT NULL,
			gpu_options TEXT NULL,
			is_include_timestamps BOOLEAN NOT NULL DEFAULT FALSE,
			is_swarm_only_worker_nodes BOOLEAN NOT NULL DEFAULT FALSE,
			is_raw_compose_deployment_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			is_build_server_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			is_consistent_container_name_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			is_gzip_enabled BOOLEAN NOT NULL DEFAULT TRUE,
			is_stripprefix_enabled BOOLEAN NOT NULL DEFAULT TRUE,
			is_permanent_redirect_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			connect_to_docker_network BOOLEAN NOT NULL DEFAULT FALSE,
			custom_internal_name TEXT NULL,
			custom_container_name_prefix TEXT NULL,
			is_container_label_escape_enabled BOOLEAN NOT NULL DEFAULT TRUE,
			is_container_label_readonly_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			is_preserve_repository_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			disable_build_cache BOOLEAN NOT NULL DEFAULT FALSE,
			use_build_secrets BOOLEAN NOT NULL DEFAULT FALSE,
			redact_all_env_values_in_logs BOOLEAN NOT NULL DEFAULT FALSE,
			inject_build_args_to_dockerfile BOOLEAN NOT NULL DEFAULT TRUE,
			include_source_commit_in_build BOOLEAN NOT NULL DEFAULT FALSE,
			include_network_hosts_in_build BOOLEAN NOT NULL DEFAULT TRUE,
			is_env_sorting_enabled BOOLEAN NOT NULL DEFAULT FALSE,
			include_coolify_default_variables BOOLEAN NOT NULL DEFAULT TRUE,
			traefik_certificate_resolver TEXT NULL,
			docker_images_to_keep INT NOT NULL DEFAULT 2,
			stop_grace_period INT NULL,
			rolling_update_drain_seconds INT NOT NULL DEFAULT 0,
			is_image_scan_enabled BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS application_previews (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			application_id BIGINT NOT NULL,
			pull_request_id BIGINT NOT NULL,
			pull_request_html_url TEXT NULL,
			fqdn TEXT NULL,
			status TEXT NOT NULL DEFAULT 'exited',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS application_deployment_queues (
			id BIGSERIAL PRIMARY KEY,
			deployment_uuid TEXT NOT NULL UNIQUE,
			application_id BIGINT NOT NULL,
			application_name TEXT NULL,
			server_id BIGINT NOT NULL DEFAULT 0,
			server_name TEXT NULL,
			status TEXT NOT NULL DEFAULT 'queued',
			commit TEXT NULL,
			force_rebuild BOOLEAN NOT NULL DEFAULT FALSE,
			is_webhook BOOLEAN NOT NULL DEFAULT FALSE,
			is_api BOOLEAN NOT NULL DEFAULT FALSE,
			logs TEXT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS services (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			docker_compose_raw TEXT NOT NULL DEFAULT '',
			docker_compose TEXT NULL,
			fqdn TEXT NULL,
			status TEXT NOT NULL DEFAULT 'exited',
			environment_id BIGINT NOT NULL,
			server_id BIGINT NOT NULL DEFAULT 0,
			destination_id BIGINT NOT NULL DEFAULT 0,
			destination_type TEXT NOT NULL DEFAULT 'App\Models\StandaloneDocker',
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS standalone_postgresqls (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			postgres_user TEXT NOT NULL DEFAULT 'postgres',
			postgres_password TEXT NOT NULL DEFAULT '',
			postgres_db TEXT NOT NULL DEFAULT 'postgres',
			image TEXT NOT NULL DEFAULT 'postgres:16-alpine',
			is_public BOOLEAN NOT NULL DEFAULT FALSE,
			public_port INT NULL,
			status TEXT NOT NULL DEFAULT 'exited',
			environment_id BIGINT NULL,
			destination_id BIGINT NOT NULL DEFAULT 0,
			destination_type TEXT NOT NULL DEFAULT 'App\Models\StandaloneDocker',
			deleted_at TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS standalone_mysqls (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			mysql_root_password TEXT NOT NULL DEFAULT '',
			mysql_user TEXT NOT NULL DEFAULT 'mysql',
			mysql_password TEXT NOT NULL DEFAULT '',
			mysql_database TEXT NOT NULL DEFAULT 'default',
			image TEXT NOT NULL DEFAULT 'mysql:8',
			is_public BOOLEAN NOT NULL DEFAULT FALSE,
			public_port INT NULL,
			status TEXT NOT NULL DEFAULT 'exited',
			environment_id BIGINT NULL,
			destination_id BIGINT NOT NULL DEFAULT 0,
			destination_type TEXT NOT NULL DEFAULT 'App\Models\StandaloneDocker',
			deleted_at TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS standalone_mariadbs (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			mariadb_root_password TEXT NOT NULL DEFAULT '',
			mariadb_user TEXT NOT NULL DEFAULT 'mariadb',
			mariadb_password TEXT NOT NULL DEFAULT '',
			mariadb_database TEXT NOT NULL DEFAULT 'default',
			image TEXT NOT NULL DEFAULT 'mariadb:11',
			is_public BOOLEAN NOT NULL DEFAULT FALSE,
			public_port INT NULL,
			status TEXT NOT NULL DEFAULT 'exited',
			environment_id BIGINT NULL,
			destination_id BIGINT NOT NULL DEFAULT 0,
			destination_type TEXT NOT NULL DEFAULT 'App\Models\StandaloneDocker',
			deleted_at TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS standalone_mongodbs (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			mongo_initdb_root_username TEXT NOT NULL DEFAULT 'root',
			mongo_initdb_root_password TEXT NOT NULL DEFAULT '',
			mongo_initdb_database TEXT NOT NULL DEFAULT 'default',
			image TEXT NOT NULL DEFAULT 'mongo:7',
			is_public BOOLEAN NOT NULL DEFAULT FALSE,
			public_port INT NULL,
			status TEXT NOT NULL DEFAULT 'exited',
			environment_id BIGINT NULL,
			destination_id BIGINT NOT NULL DEFAULT 0,
			destination_type TEXT NOT NULL DEFAULT 'App\Models\StandaloneDocker',
			deleted_at TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS standalone_redis (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			redis_password TEXT NOT NULL DEFAULT '',
			image TEXT NOT NULL DEFAULT 'redis:7-alpine',
			is_public BOOLEAN NOT NULL DEFAULT FALSE,
			public_port INT NULL,
			status TEXT NOT NULL DEFAULT 'exited',
			environment_id BIGINT NULL,
			destination_id BIGINT NOT NULL DEFAULT 0,
			destination_type TEXT NOT NULL DEFAULT 'App\Models\StandaloneDocker',
			deleted_at TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS standalone_keydbs (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			keydb_password TEXT NOT NULL DEFAULT '',
			image TEXT NOT NULL DEFAULT 'eqalpha/keydb:latest',
			is_public BOOLEAN NOT NULL DEFAULT FALSE,
			public_port INT NULL,
			status TEXT NOT NULL DEFAULT 'exited',
			environment_id BIGINT NULL,
			destination_id BIGINT NOT NULL DEFAULT 0,
			destination_type TEXT NOT NULL DEFAULT 'App\Models\StandaloneDocker',
			deleted_at TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS standalone_dragonflies (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			dragonfly_password TEXT NOT NULL DEFAULT '',
			image TEXT NOT NULL DEFAULT 'docker.dragonflydb.io/dragonflydb/dragonfly:latest',
			is_public BOOLEAN NOT NULL DEFAULT FALSE,
			public_port INT NULL,
			status TEXT NOT NULL DEFAULT 'exited',
			environment_id BIGINT NULL,
			destination_id BIGINT NOT NULL DEFAULT 0,
			destination_type TEXT NOT NULL DEFAULT 'App\Models\StandaloneDocker',
			deleted_at TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS standalone_clickhouses (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			clickhouse_admin_user TEXT NOT NULL DEFAULT 'default',
			clickhouse_admin_password TEXT NOT NULL DEFAULT '',
			image TEXT NOT NULL DEFAULT 'clickhouse/clickhouse-server:latest',
			is_public BOOLEAN NOT NULL DEFAULT FALSE,
			public_port INT NULL,
			status TEXT NOT NULL DEFAULT 'exited',
			environment_id BIGINT NULL,
			destination_id BIGINT NOT NULL DEFAULT 0,
			destination_type TEXT NOT NULL DEFAULT 'App\Models\StandaloneDocker',
			deleted_at TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS standalone_sqlites (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			sqlite_databases TEXT NOT NULL DEFAULT 'database.sqlite',
			image TEXT NOT NULL DEFAULT 'peakimages/sqlite:3.53.4-v0.1.0',
			is_public BOOLEAN NOT NULL DEFAULT FALSE,
			status TEXT NOT NULL DEFAULT 'exited',
			environment_id BIGINT NULL,
			destination_id BIGINT NOT NULL DEFAULT 0,
			destination_type TEXT NOT NULL DEFAULT 'App\Models\StandaloneDocker',
			deleted_at TIMESTAMPTZ NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS environment_variables (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			key TEXT NOT NULL,
			value TEXT NULL,
			is_preview BOOLEAN NOT NULL DEFAULT FALSE,
			is_buildtime BOOLEAN NOT NULL DEFAULT TRUE,
			is_runtime BOOLEAN NOT NULL DEFAULT TRUE,
			is_literal BOOLEAN NOT NULL DEFAULT FALSE,
			is_multiline BOOLEAN NOT NULL DEFAULT FALSE,
			is_shown_once BOOLEAN NOT NULL DEFAULT FALSE,
			resourceable_type TEXT NOT NULL,
			resourceable_id BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS shared_environment_variables (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			key TEXT NOT NULL,
			value TEXT NULL,
			type TEXT NOT NULL,
			is_shown_once BOOLEAN NOT NULL DEFAULT FALSE,
			team_id BIGINT NOT NULL,
			project_id BIGINT NULL,
			environment_id BIGINT NULL,
			server_id BIGINT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS s3_storages (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			description TEXT NULL,
			region TEXT NOT NULL DEFAULT 'us-east-1',
			key TEXT NOT NULL DEFAULT '',
			secret TEXT NOT NULL DEFAULT '',
			bucket TEXT NOT NULL DEFAULT '',
			endpoint TEXT NOT NULL DEFAULT '',
			is_usable BOOLEAN NOT NULL DEFAULT TRUE,
			team_id BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS scheduled_database_backups (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			enabled BOOLEAN NOT NULL DEFAULT TRUE,
			frequency TEXT NOT NULL DEFAULT '0 0 * * *',
			timezone TEXT NOT NULL DEFAULT 'UTC',
			save_s3 BOOLEAN NOT NULL DEFAULT FALSE,
			database_retention_count INT NOT NULL DEFAULT 7,
			database_id BIGINT NOT NULL,
			database_type TEXT NOT NULL,
			s3_storage_id BIGINT NULL,
			team_id BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS scheduled_database_backup_executions (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			scheduled_database_backup_id BIGINT NOT NULL,
			status TEXT NOT NULL DEFAULT 'running',
			message TEXT NULL,
			size BIGINT NULL,
			filename TEXT NULL,
			checksum_sha256 TEXT NULL,
			s3_uploaded BOOLEAN NOT NULL DEFAULT FALSE,
			database_name TEXT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS tags (
			id BIGSERIAL PRIMARY KEY,
			name TEXT NOT NULL,
			team_id BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS local_persistent_volumes (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL,
			mount_path TEXT NOT NULL,
			host_path TEXT NULL,
			resource_type TEXT NOT NULL,
			resource_id BIGINT NOT NULL,
			is_preview_suffix_enabled BOOLEAN NOT NULL DEFAULT TRUE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS local_file_volumes (
			id BIGSERIAL PRIMARY KEY,
			uuid TEXT NOT NULL UNIQUE,
			fs_path TEXT NOT NULL,
			mount_path TEXT NOT NULL,
			content TEXT NULL,
			is_directory BOOLEAN NOT NULL DEFAULT FALSE,
			is_host_file BOOLEAN NOT NULL DEFAULT FALSE,
			is_preview_suffix_enabled BOOLEAN NOT NULL DEFAULT TRUE,
			resource_type TEXT NOT NULL,
			resource_id BIGINT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`CREATE TABLE IF NOT EXISTS audit_events (
			id BIGSERIAL PRIMARY KEY,
			event TEXT NOT NULL,
			actor_user_id BIGINT NULL,
			team_id BIGINT NULL,
			resource_type TEXT NULL,
			resource_uuid TEXT NULL,
			severity TEXT NOT NULL DEFAULT 'info',
			metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
	}

	for _, stmt := range stmts {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("apply baseline schema: %w", err)
		}
	}
	if err := ensureExtendedBaselineSchema(ctx, pool); err != nil {
		return err
	}
	return EnsureGoAdditiveSchema(ctx, pool)
}

// SeedInstanceSentinels idempotently creates Coolify's id = 0 sentinel rows while preserving sequence values >= 1.
func SeedInstanceSentinels(ctx context.Context, pool *pgxpool.Pool, rootEmail string, rootPasswordHash string, encryptedLocalKey string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Root team id = 0
	if _, err := tx.Exec(ctx, `
		INSERT INTO teams (id, name, description, personal_team, show_boarding)
		VALUES (0, 'Root Team', 'Instance root team', TRUE, FALSE)
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		return fmt.Errorf("seed root team: %w", err)
	}

	// InstanceSettings id = 0
	if _, err := tx.Exec(ctx, `
		INSERT INTO instance_settings (id, instance_name, is_api_enabled, is_mcp_server_enabled, is_registration_enabled)
		VALUES (0, 'Coolify', TRUE, FALSE, TRUE)
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		return fmt.Errorf("seed instance settings: %w", err)
	}

	// Sentinel localhost private key id = 0
	if encryptedLocalKey == "" {
		encryptedLocalKey = "sentinel-key-unconfigured"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO private_keys (id, uuid, name, description, private_key, team_id)
		VALUES (0, 'pk-0', 'localhost-key', 'Localhost SSH key', $1, 0)
		ON CONFLICT (id) DO NOTHING
	`, encryptedLocalKey); err != nil {
		return fmt.Errorf("seed private key 0: %w", err)
	}

	// Sentinel localhost server id = 0
	if _, err := tx.Exec(ctx, `
		INSERT INTO servers (id, uuid, name, description, ip, port, "user", team_id, private_key_id)
		VALUES (0, 'localhost', 'localhost', 'This is the server where Coolify is running on.', 'host.docker.internal', 22, 'root', 0, 0)
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		return fmt.Errorf("seed localhost server: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO server_settings (server_id, is_reachable, is_usable, force_disabled, is_terminal_enabled)
		VALUES (0, TRUE, TRUE, FALSE, TRUE)
		ON CONFLICT (server_id) DO NOTHING
	`); err != nil {
		return fmt.Errorf("seed localhost server settings: %w", err)
	}

	// Sentinel standalone_dockers id = 0
	if _, err := tx.Exec(ctx, `
		INSERT INTO standalone_dockers (id, uuid, name, network, server_id)
		VALUES (0, 'standalone-docker-0', 'standalone-docker', 'coolify', 0)
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		return fmt.Errorf("seed standalone docker 0: %w", err)
	}

	// Sentinel coolify-db id = 0
	if _, err := tx.Exec(ctx, `
		INSERT INTO standalone_postgresqls (id, uuid, name, description, postgres_user, postgres_password, postgres_db, status, environment_id, destination_id)
		VALUES (0, 'coolify-db', 'coolify-db', 'Coolify instance database', 'coolify', '', 'coolify', 'running', 0, 0)
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		return fmt.Errorf("seed coolify-db 0: %w", err)
	}

	// Sentinel default public GitHub App id = 0
	if _, err := tx.Exec(ctx, `
		INSERT INTO github_apps (id, uuid, name, api_url, html_url, is_public, is_system_wide, team_id)
		VALUES (0, 'github-app-0', 'Public GitHub', 'https://api.github.com', 'https://github.com', TRUE, TRUE, 0)
		ON CONFLICT (id) DO NOTHING
	`); err != nil {
		return fmt.Errorf("seed github app 0: %w", err)
	}

	// Optional root user id = 0 bootstrap (only if rootEmail provided and user 0 does not yet exist)
	if rootEmail != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO users (id, name, email, password, email_verified_at)
			VALUES (0, 'Root User', $1, $2, NOW())
			ON CONFLICT (id) DO NOTHING
		`, rootEmail, rootPasswordHash); err != nil {
			return fmt.Errorf("seed root user 0: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO team_user (team_id, user_id, role)
			VALUES (0, 0, 'owner')
			ON CONFLICT (team_id, user_id) DO NOTHING
		`); err != nil {
			return fmt.Errorf("seed root team_user: %w", err)
		}
	}

	// Ensure sequences stay >= 1 so ordinary rows NEVER get assigned sentinel id = 0.
	seqs := []string{
		"users_id_seq", "teams_id_seq", "private_keys_id_seq", "servers_id_seq",
		"standalone_dockers_id_seq", "standalone_postgresqls_id_seq",
		"standalone_mysqls_id_seq", "standalone_mariadbs_id_seq",
		"standalone_mongodbs_id_seq", "standalone_redis_id_seq",
		"standalone_keydbs_id_seq", "standalone_dragonflies_id_seq",
		"standalone_clickhouses_id_seq", "standalone_sqlites_id_seq",
		"projects_id_seq", "environments_id_seq", "applications_id_seq",
		"application_settings_id_seq", "services_id_seq", "scheduled_database_backups_id_seq",
		"github_apps_id_seq", "gitlab_apps_id_seq", "gitea_apps_id_seq",
	}
	for _, seq := range seqs {
		_, _ = tx.Exec(ctx, fmt.Sprintf(`SELECT setval('%s', GREATEST((SELECT COALESCE(MAX(id), 0) FROM %s), 1), true)`, seq, tableFromSeq(seq)))
	}

	return tx.Commit(ctx)
}

func tableFromSeq(seq string) string {
	return seq[:len(seq)-len("_id_seq")]
}
