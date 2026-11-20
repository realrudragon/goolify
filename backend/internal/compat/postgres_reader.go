package compat

import (
	"context"
	"fmt"
	"time"

	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresDatasetReader implements DatasetReader directly against a real PostgreSQL database
// starting strictly at id >= 0 (never skipping Coolify's id = 0 sentinel rows).
type PostgresDatasetReader struct {
	pool   *pgxpool.Pool
	cipher *Cipher
}

func NewPostgresDatasetReader(pool *pgxpool.Pool, cipher *Cipher) *PostgresDatasetReader {
	return &PostgresDatasetReader{pool: pool, cipher: cipher}
}

var coreValidationTables = []string{
	"teams",
	"users",
	"instance_settings",
	"private_keys",
	"servers",
	"standalone_dockers",
	"standalone_postgresqls",
	"github_apps",
	"projects",
	"environments",
	"applications",
	"environment_variables",
	"services",
}

func (r *PostgresDatasetReader) ListTablesInOrder(ctx context.Context) ([]string, error) {
	var present []string
	for _, tbl := range coreValidationTables {
		var exists bool
		if err := r.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = 'public' AND table_name = $1
			)
		`, tbl).Scan(&exists); err != nil {
			return nil, err
		}
		if exists {
			present = append(present, tbl)
		}
	}
	return present, nil
}

func (r *PostgresDatasetReader) FetchPageStartingFromZero(ctx context.Context, table string, afterID *int64, limit int) ([]ValidationRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	var query string
	switch table {
	case "teams":
		query = `SELECT id, 'team:' || id::text, '', NULL::text, updated_at FROM teams WHERE ($1::bigint IS NULL OR id > $1) ORDER BY id ASC LIMIT $2`
	case "users":
		query = `SELECT id, 'user:' || id::text, '', NULL::text, updated_at FROM users WHERE ($1::bigint IS NULL OR id > $1) ORDER BY id ASC LIMIT $2`
	case "instance_settings":
		query = `SELECT id, 'inst:' || id::text, '', NULL::text, updated_at FROM instance_settings WHERE ($1::bigint IS NULL OR id > $1) ORDER BY id ASC LIMIT $2`
	case "private_keys":
		query = `SELECT id, uuid, 'team:' || team_id::text, private_key, updated_at FROM private_keys WHERE ($1::bigint IS NULL OR id > $1) ORDER BY id ASC LIMIT $2`
	case "servers":
		query = `SELECT id, uuid, 'team:' || team_id::text, NULL::text, updated_at FROM servers WHERE deleted_at IS NULL AND ($1::bigint IS NULL OR id > $1) ORDER BY id ASC LIMIT $2`
	case "standalone_dockers":
		query = `SELECT id, uuid, 'server:' || server_id::text, NULL::text, updated_at FROM standalone_dockers WHERE ($1::bigint IS NULL OR id > $1) ORDER BY id ASC LIMIT $2`
	case "standalone_postgresqls":
		query = `SELECT id, uuid, '', postgres_password, updated_at FROM standalone_postgresqls WHERE ($1::bigint IS NULL OR id > $1) ORDER BY id ASC LIMIT $2`
	case "github_apps":
		query = `SELECT id, uuid, 'team:' || team_id::text, webhook_secret, updated_at FROM github_apps WHERE ($1::bigint IS NULL OR id > $1) ORDER BY id ASC LIMIT $2`
	case "projects":
		query = `SELECT id, uuid, 'team:' || team_id::text, NULL::text, updated_at FROM projects WHERE ($1::bigint IS NULL OR id > $1) ORDER BY id ASC LIMIT $2`
	case "environments":
		query = `SELECT id, uuid, 'project:' || project_id::text, NULL::text, updated_at FROM environments WHERE ($1::bigint IS NULL OR id > $1) ORDER BY id ASC LIMIT $2`
	case "applications":
		query = `SELECT id, uuid, 'env:' || environment_id::text, NULL::text, updated_at FROM applications WHERE ($1::bigint IS NULL OR id > $1) ORDER BY id ASC LIMIT $2`
	case "environment_variables":
		query = `SELECT id, uuid, '', value, updated_at FROM environment_variables WHERE ($1::bigint IS NULL OR id > $1) ORDER BY id ASC LIMIT $2`
	case "services":
		query = `SELECT id, uuid, 'env:' || environment_id::text, NULL::text, updated_at FROM services WHERE ($1::bigint IS NULL OR id > $1) ORDER BY id ASC LIMIT $2`
	default:
		return nil, nil
	}

	rows, err := r.pool.Query(ctx, query, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", table, err)
	}
	defer rows.Close()

	var out []ValidationRecord
	for rows.Next() {
		var rec ValidationRecord
		rec.Table = table
		var ts time.Time
		if err := rows.Scan(&rec.ID, &rec.UUID, &rec.ParentUUID, &rec.SecretValue, &ts); err != nil {
			return nil, err
		}
		rec.UpdatedAt = ts.UTC()
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (r *PostgresDatasetReader) ParentExists(ctx context.Context, parentRef string) (bool, error) {
	var kind string
	var id int64
	if _, err := fmt.Sscanf(parentRef, "%[^:]:%d", &kind, &id); err != nil {
		return true, nil
	}
	var tbl string
	switch kind {
	case "team":
		tbl = "teams"
	case "server":
		tbl = "servers"
	case "project":
		tbl = "projects"
	case "env":
		tbl = "environments"
	default:
		return true, nil
	}
	var exists bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+tbl+` WHERE id = $1)`, id).Scan(&exists)
	return exists, err
}

// VerifyEncryptedColumns checks that every non-null, non-empty encrypted secret across
// present baseline tables decrypts cleanly with APP_KEY without ever exposing plaintext values.
func (r *PostgresDatasetReader) VerifyEncryptedColumns(ctx context.Context) (checked int, failed int, err error) {
	if r.cipher == nil {
		return 0, 0, nil
	}
	targets := []struct {
		table string
		cols  []string
	}{
		{"environment_variables", []string{"value"}},
		{"private_keys", []string{"private_key"}},
		{"ssl_certificates", []string{"ssl_certificate", "ssl_private_key"}},
		{"cloud_provider_tokens", []string{"token"}},
		{"cloud_init_scripts", []string{"script"}},
		{"integration_tokens", []string{"token"}},
		{"s3_storages", []string{"key", "secret"}},
		{"email_notification_settings", []string{"smtp_from_address", "smtp_recipients", "smtp_host", "smtp_username", "smtp_password", "resend_api_key", "resend_from_address"}},
		{"discord_notification_settings", []string{"discord_webhook_url"}},
		{"slack_notification_settings", []string{"slack_webhook_url"}},
		{"telegram_notification_settings", []string{"telegram_token", "telegram_chat_id"}},
		{"pushover_notification_settings", []string{"pushover_user_key", "pushover_api_token"}},
		{"webhook_notification_settings", []string{"webhook_url"}},
		{"github_apps", []string{"client_secret", "webhook_secret"}},
		{"gitlab_apps", []string{"app_secret", "client_secret", "access_token", "refresh_token", "webhook_token"}},
		{"gitea_apps", []string{"client_secret", "access_token", "refresh_token", "webhook_secret"}},
	}

	for _, t := range targets {
		var exists bool
		if err := r.pool.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = 'public' AND table_name = $1
			)
		`, t.table).Scan(&exists); err != nil {
			return checked, failed, err
		}
		if !exists {
			continue
		}
		for _, col := range t.cols {
			var colExists bool
			if err := r.pool.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM information_schema.columns
					WHERE table_schema = 'public' AND table_name = $1 AND column_name = $2
				)
			`, t.table, col).Scan(&colExists); err != nil || !colExists {
				continue
			}
			rows, err := r.pool.Query(ctx, `SELECT `+col+` FROM `+t.table+` WHERE `+col+` IS NOT NULL AND `+col+` <> ''`)
			if err != nil {
				return checked, failed, err
			}
			for rows.Next() {
				var enc string
				if err := rows.Scan(&enc); err != nil {
					rows.Close()
					return checked, failed, err
				}
				checked++
				if _, err1 := r.cipher.DecryptLaravelPayload(enc, false); err1 != nil {
					if _, err2 := r.cipher.DecryptLaravelPayload(enc, true); err2 != nil {
						failed++
					}
				}
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return checked, failed, err
			}
			rows.Close()
		}
	}
	return checked, failed, nil
}

// LoadRemoteJournals reads durable remote operation journals persisted in PostgreSQL `go_remote_journals`.
func (r *PostgresDatasetReader) LoadRemoteJournals(ctx context.Context) ([]executor.RemoteJournal, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'go_remote_journals')
	`).Scan(&exists); err != nil || !exists {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `SELECT journal_json FROM go_remote_journals ORDER BY operation_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []executor.RemoteJournal
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var j executor.RemoteJournal
		if err := jUnmarshal(raw, &j); err == nil {
			out = append(out, j)
		}
	}
	return out, rows.Err()
}

// LoadSchemaMigrations reads recorded expand-only Go migrations from `go_schema_migrations`.
func (r *PostgresDatasetReader) LoadSchemaMigrations(ctx context.Context) ([]struct {
	Name       string
	ExpandOnly bool
}, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'go_schema_migrations')
	`).Scan(&exists); err != nil || !exists {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `SELECT name, expand_only FROM go_schema_migrations ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		Name       string
		ExpandOnly bool
	}
	for rows.Next() {
		var item struct {
			Name       string
			ExpandOnly bool
		}
		if err := rows.Scan(&item.Name, &item.ExpandOnly); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
