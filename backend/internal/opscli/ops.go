package opscli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/compose"
	"github.com/coollabsio/coolify/backend/internal/domains/ops"
	"github.com/coollabsio/coolify/backend/internal/domains/services"
	"github.com/coollabsio/coolify/backend/internal/store/postgres"
	goredis "github.com/coollabsio/coolify/backend/internal/store/redis"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// Runner executes real Coolify operational/maintenance commands against PostgreSQL & Redis
// (replacing legacy `php artisan` admin, cleanup, reencryption, scheduler, and export commands).
type Runner struct {
	Pool   *pgxpool.Pool
	Redis  *goredis.Client
	Cipher *compat.Cipher
}

func NewRunner(pool *pgxpool.Pool, rdb *goredis.Client, ciph *compat.Cipher) *Runner {
	return &Runner{Pool: pool, Redis: rdb, Cipher: ciph}
}

// InitAndSeed ensures baseline + additive schema and id=0 instance sentinels idempotently.
func (r *Runner) InitAndSeed(ctx context.Context, rootEmail, rootPassword string) error {
	if err := postgres.EnsureBaselineSchema(ctx, r.Pool); err != nil {
		return err
	}
	if rootEmail == "" {
		rootEmail = "root@example.com"
	}
	if rootPassword == "" {
		rootPassword = "password"
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(rootPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	localKey := ""
	if r.Cipher != nil {
		if enc, encErr := r.Cipher.EncryptLaravelPayload([]byte("-----BEGIN OPENSSH PRIVATE KEY-----\nLOCALHOST-BOOTSTRAP-KEY\n-----END OPENSSH PRIVATE KEY-----\n"), false); encErr == nil {
			localKey = enc
		}
	}
	return postgres.SeedInstanceSentinels(ctx, r.Pool, rootEmail, string(hash), localKey)
}

// RootResetPassword resets the id=0 root user password and revokes active browser sessions.
func (r *Runner) RootResetPassword(ctx context.Context, newPassword string) error {
	if len(strings.TrimSpace(newPassword)) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tag, err := r.Pool.Exec(ctx, `UPDATE users SET password = $1, force_password_reset = FALSE, updated_at = NOW() WHERE id = 0`, string(hash))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("root user (id=0) not found")
	}
	_, _ = r.Pool.Exec(ctx, `DELETE FROM go_sessions WHERE user_id = 0`)
	return nil
}

// RootChangeEmail updates the id=0 root user's email address in PostgreSQL.
func (r *Runner) RootChangeEmail(ctx context.Context, newEmail string) error {
	email := strings.ToLower(strings.TrimSpace(newEmail))
	if !strings.Contains(email, "@") {
		return errors.New("valid email address is required")
	}
	tag, err := r.Pool.Exec(ctx, `UPDATE users SET email = $1, updated_at = NOW() WHERE id = 0`, email)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("root user (id=0) not found")
	}
	return nil
}

// AdminDeleteUser deletes a non-root user and cleans up team links, sessions, and tokens.
func (r *Runner) AdminDeleteUser(ctx context.Context, email string) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var uid int64
	if err := r.Pool.QueryRow(ctx, `SELECT id FROM users WHERE LOWER(email) = $1`, email).Scan(&uid); err != nil {
		return 0, fmt.Errorf("user not found: %w", err)
	}
	if uid == 0 {
		return 0, errors.New("cannot delete root user (id=0)")
	}
	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	_, _ = tx.Exec(ctx, `DELETE FROM go_sessions WHERE user_id = $1`, uid)
	_, _ = tx.Exec(ctx, `DELETE FROM personal_access_tokens WHERE tokenable_id = $1`, uid)
	_, _ = tx.Exec(ctx, `DELETE FROM team_user WHERE user_id = $1`, uid)
	_, err = tx.Exec(ctx, `DELETE FROM users WHERE id = $1`, uid)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return uid, nil
}

// ReencryptWithAppKey atomically decrypts all encrypted columns across baseline tables using oldKey
// and re-encrypts them using newKey inside a single PostgreSQL transaction (all-or-nothing!).
func (r *Runner) ReencryptWithAppKey(ctx context.Context, oldKey, newKey string) (int, error) {
	oldCiph, err := compat.NewCipher(oldKey)
	if err != nil {
		return 0, fmt.Errorf("invalid old APP_KEY: %w", err)
	}
	newCiph, err := compat.NewCipher(newKey)
	if err != nil {
		return 0, fmt.Errorf("invalid new APP_KEY: %w", err)
	}
	targets := []struct {
		table string
		cols  []string
	}{
		{"environment_variables", []string{"value"}},
		{"shared_environment_variables", []string{"value"}},
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

	tx, err := r.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	updatedCells := 0
	for _, t := range targets {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema='public' AND table_name=$1)`, t.table).Scan(&exists); err != nil || !exists {
			continue
		}
		for _, col := range t.cols {
			var colExists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='public' AND table_name=$1 AND column_name=$2)`, t.table, col).Scan(&colExists); err != nil || !colExists {
				continue
			}
			rows, err := tx.Query(ctx, `SELECT id, `+col+` FROM `+t.table+` WHERE `+col+` IS NOT NULL AND `+col+` <> ''`)
			if err != nil {
				return 0, err
			}
			type cell struct {
				id  int64
				val string
			}
			var cells []cell
			for rows.Next() {
				var c cell
				if err := rows.Scan(&c.id, &c.val); err != nil {
					rows.Close()
					return 0, err
				}
				cells = append(cells, c)
			}
			rows.Close()

			for _, c := range cells {
				serialized := false
				plain, decErr := oldCiph.DecryptLaravelPayload(c.val, false)
				if decErr != nil {
					plain, decErr = oldCiph.DecryptLaravelPayload(c.val, true)
					if decErr != nil {
						return 0, fmt.Errorf("failed decrypting %s.%s id=%d with old key: %w", t.table, col, c.id, decErr)
					}
					serialized = true
				}
				reEnc, err := newCiph.EncryptLaravelPayload(plain, serialized)
				if err != nil {
					return 0, err
				}
				if _, err := tx.Exec(ctx, `UPDATE `+t.table+` SET `+col+` = $1, updated_at = NOW() WHERE id = $2`, reEnc, c.id); err != nil {
					return 0, err
				}
				updatedCells++
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return updatedCells, nil
}

// CleanupDeploymentQueue marks stale queued/in_progress deployments older than maxAge as failed.
func (r *Runner) CleanupDeploymentQueue(ctx context.Context, maxAge time.Duration) (int64, error) {
	if maxAge <= 0 {
		maxAge = 2 * time.Hour
	}
	cutoff := time.Now().UTC().Add(-maxAge)
	tag, err := r.Pool.Exec(ctx, `
		UPDATE application_deployment_queues
		SET status = 'failed', finished_at = NOW(), updated_at = NOW()
		WHERE status IN ('queued', 'in_progress') AND created_at < $1
	`, cutoff)
	if err != nil {
		return 0, err
	}
	_, _ = r.Pool.Exec(ctx, `
		UPDATE go_job_operations
		SET status = 'dead', last_error = 'cleaned up stale queue entry', updated_at = NOW()
		WHERE status IN ('queued', 'running') AND updated_at < $1
	`, cutoff)
	return tag.RowsAffected(), nil
}

// CleanupStuckResources resets applications/services stuck in restarting/in_progress states.
func (r *Runner) CleanupStuckResources(ctx context.Context, maxAge time.Duration) (int64, error) {
	if maxAge <= 0 {
		maxAge = 30 * time.Minute
	}
	cutoff := time.Now().UTC().Add(-maxAge)
	var total int64
	t1, err := r.Pool.Exec(ctx, `UPDATE applications SET status = 'exited', updated_at = NOW() WHERE status IN ('restarting','in_progress') AND updated_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	total += t1.RowsAffected()
	t2, err := r.Pool.Exec(ctx, `UPDATE services SET status = 'exited', updated_at = NOW() WHERE status IN ('restarting','in_progress') AND updated_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	total += t2.RowsAffected()
	return total, nil
}

// CleanupImportUploads deletes expired upload files on disk and marks rows expired in PostgreSQL.
func (r *Runner) CleanupImportUploads(ctx context.Context, maxAge time.Duration) (int64, error) {
	if maxAge <= 0 {
		maxAge = 24 * time.Hour
	}
	cutoff := time.Now().UTC().Add(-maxAge)
	rows, err := r.Pool.Query(ctx, `SELECT id, filepath FROM go_upload_imports WHERE created_at < $1 AND status <> 'expired'`, cutoff)
	if err != nil {
		return 0, err
	}
	type item struct {
		id int64
		fp string
	}
	var items []item
	for rows.Next() {
		var it item
		if rows.Scan(&it.id, &it.fp) == nil {
			items = append(items, it)
		}
	}
	rows.Close()
	for _, it := range items {
		if it.fp != "" {
			_ = os.Remove(it.fp)
		}
		_, _ = r.Pool.Exec(ctx, `UPDATE go_upload_imports SET status = 'expired', updated_at = NOW() WHERE id = $1`, it.id)
	}
	return int64(len(items)), nil
}

// ReleaseOrphanedManagedDNS deletes `managed_dns_records` with zero rows in `managed_dns_record_references`.
func (r *Runner) ReleaseOrphanedManagedDNS(ctx context.Context) (int64, error) {
	tag, err := r.Pool.Exec(ctx, `
		DELETE FROM managed_dns_records m
		WHERE NOT EXISTS (
			SELECT 1 FROM managed_dns_record_references ref WHERE ref.managed_dns_record_id = m.id
		)
	`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CleanupUnverifiedUsers deletes non-root users without email_verified_at older than maxAge.
func (r *Runner) CleanupUnverifiedUsers(ctx context.Context, maxAge time.Duration) (int64, error) {
	if maxAge <= 0 {
		maxAge = 7 * 24 * time.Hour
	}
	cutoff := time.Now().UTC().Add(-maxAge)
	tag, err := r.Pool.Exec(ctx, `DELETE FROM users WHERE id <> 0 AND email_verified_at IS NULL AND created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ClearGlobalSearchCache bumps all teams' search_ver keys in Redis cleanly.
func (r *Runner) ClearGlobalSearchCache(ctx context.Context) (int, error) {
	if r.Redis == nil {
		return 0, nil
	}
	rows, err := r.Pool.Query(ctx, `SELECT id FROM teams ORDER BY id ASC`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var tid int64
		if rows.Scan(&tid) == nil {
			_, _ = r.Redis.Incr(ctx, fmt.Sprintf("coolify:go:v1:team:%d:search_ver", tid))
			n++
		}
	}
	return n, rows.Err()
}

// ScheduledJobDiagnostics returns real Postgres telemetry from scheduled_job_states and scheduled_job_deliveries.
func (r *Runner) ScheduledJobDiagnostics(ctx context.Context) (map[string]any, error) {
	var states, deliveries, pending int64
	_ = r.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM scheduled_job_states`).Scan(&states)
	_ = r.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM scheduled_job_deliveries`).Scan(&deliveries)
	_ = r.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM go_job_operations WHERE status = 'queued'`).Scan(&pending)
	return map[string]any{
		"scheduled_job_states":     states,
		"scheduled_job_deliveries": deliveries,
		"queued_go_jobs":           pending,
		"checked_at":               time.Now().UTC(),
	}, nil
}

// ExportUsers exports all users from PostgreSQL as JSON (excluding password hashes & 2FA secrets).
func (r *Runner) ExportUsers(ctx context.Context) ([]map[string]any, error) {
	rows, err := r.Pool.Query(ctx, `SELECT id, name, email, email_verified_at, created_at FROM users ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0)
	for rows.Next() {
		var id int64
		var name, email string
		var ver *time.Time
		var ca time.Time
		if rows.Scan(&id, &name, &email, &ver, &ca) == nil {
			out = append(out, map[string]any{"id": id, "name": name, "email": email, "email_verified_at": ver, "created_at": ca})
		}
	}
	return out, rows.Err()
}

// ValidateComposeTemplates parses every YAML template in `templatesDir` using the real Go Compose parser.
func ValidateComposeTemplates(templatesDir string) (int, error) {
	entries, err := os.ReadDir(templatesDir)
	if err != nil {
		return 0, err
	}
	p := compose.NewComposeParser()
	parsed := 0
	for _, ent := range entries {
		if ent.IsDir() || (!strings.HasSuffix(ent.Name(), ".yaml") && !strings.HasSuffix(ent.Name(), ".yml")) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(templatesDir, ent.Name()))
		if err != nil {
			return parsed, err
		}
		if _, err := p.Parse(context.Background(), compose.ComposeParseInput{
			Mode:              compose.ModeService,
			ParserVersion:     "5",
			RawCompose:        string(raw),
			ResourceUUID:      strings.TrimSuffix(ent.Name(), filepath.Ext(ent.Name())),
			ProxyType:         "traefik",
			ServerIP:          "10.0.0.1",
			DeterministicSeed: "tpl-seed",
			Env:               map[string]string{},
		}); err != nil && !errors.Is(err, compose.ErrMissingRequiredVar) {
			return parsed, fmt.Errorf("template %s: %w", ent.Name(), err)
		}
		parsed++
	}
	return parsed, nil
}

// CheckDomainDNS resolves a hostname via real system DNS (`net.DefaultResolver.LookupHost`).
func CheckDomainDNS(ctx context.Context, fqdn string) ([]string, error) {
	host := strings.TrimSpace(fqdn)
	host = strings.TrimPrefix(host, "https://")
	host = strings.TrimPrefix(host, "http://")
	if idx := strings.IndexAny(host, "/:"); idx >= 0 {
		host = host[:idx]
	}
	if host == "" {
		return nil, errors.New("empty hostname")
	}
	return net.DefaultResolver.LookupHost(ctx, host)
}

// CleanupNames sanitizes names across projects, servers, applications, and services in PostgreSQL.
func (r *Runner) CleanupNames(ctx context.Context) (int64, error) {
	var total int64
	for _, tbl := range []string{"projects", "servers", "applications", "services"} {
		tag, err := r.Pool.Exec(ctx, `UPDATE `+tbl+` SET name = BTRIM(name), updated_at = NOW() WHERE name <> BTRIM(name)`)
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
	}
	return total, nil
}

// RekeyOauthIdentities normalizes issuer URLs (trimming trailing slashes) in `oauth_identities`.
func (r *Runner) RekeyOauthIdentities(ctx context.Context) (int64, error) {
	tag, err := r.Pool.Exec(ctx, `
		UPDATE oauth_identities
		SET issuer = RTRIM(issuer, '/'), updated_at = NOW()
		WHERE issuer LIKE '%/'
	`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// AllowSSOLink clears stale provider bindings for a user email so they can re-link an OAuth identity.
func (r *Runner) AllowSSOLink(ctx context.Context, email string) (int64, error) {
	var uid int64
	if err := r.Pool.QueryRow(ctx, `SELECT id FROM users WHERE LOWER(email) = $1`, strings.ToLower(strings.TrimSpace(email))).Scan(&uid); err != nil {
		return 0, fmt.Errorf("user not found: %w", err)
	}
	tag, err := r.Pool.Exec(ctx, `DELETE FROM oauth_identities WHERE user_id = $1`, uid)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ViewScheduledLogs returns recent real scheduled job deliveries & backup executions from PostgreSQL.
func (r *Runner) ViewScheduledLogs(ctx context.Context, limit int) ([]map[string]any, error) {
	if limit <= 0 {
		limit = 25
	}
	rows, err := r.Pool.Query(ctx, `
		SELECT uuid, schedule_key, job_type, status, scheduled_for, created_at
		FROM scheduled_job_deliveries
		ORDER BY id DESC LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0)
	for rows.Next() {
		var u, sk, jt, st string
		var sf, ca time.Time
		if rows.Scan(&u, &sk, &jt, &st, &sf, &ca) == nil {
			out = append(out, map[string]any{"uuid": u, "schedule_key": sk, "job_type": jt, "status": st, "scheduled_for": sf, "created_at": ca})
		}
	}
	return out, rows.Err()
}

// DeleteOrphanedServices removes services whose owning environment no longer exists.
func (r *Runner) DeleteOrphanedServices(ctx context.Context) (int64, error) {
	tag, err := r.Pool.Exec(ctx, `
		DELETE FROM services s
		WHERE NOT EXISTS (SELECT 1 FROM environments e WHERE e.id = s.environment_id)
	`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ReconcileSubscriptions ensures expired/past-due non-root subscriptions have consistent flags.
func (r *Runner) ReconcileSubscriptions(ctx context.Context) (int64, error) {
	tag, err := r.Pool.Exec(ctx, `
		UPDATE subscriptions
		SET stripe_invoice_paid = FALSE, updated_at = NOW()
		WHERE team_id <> 0 AND ended_at IS NOT NULL AND ended_at < NOW() AND stripe_invoice_paid = TRUE
	`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func GenerateOpenAPIJSON(version string) ([]byte, error) {
	return ops.GenerateOpenAPISpec(version)
}

// DevInfo checks database, Redis, service templates catalog, and sentinel id=0 readiness.
func (r *Runner) DevInfo(ctx context.Context) (map[string]any, error) {
	dbOK := r.Pool != nil && r.Pool.Ping(ctx) == nil
	redisOK := r.Redis != nil && r.Redis.Ping(ctx) == nil
	var teamZeroCount, serverZeroCount int64
	if r.Pool != nil {
		_ = r.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM teams WHERE id = 0`).Scan(&teamZeroCount)
		_ = r.Pool.QueryRow(ctx, `SELECT COUNT(*) FROM servers WHERE id = 0`).Scan(&serverZeroCount)
	}
	tpls, _ := services.ListServiceTemplates(false)
	return map[string]any{
		"database_ok":     dbOK,
		"redis_ok":        redisOK,
		"sentinel_team_0": teamZeroCount > 0,
		"sentinel_srv_0":  serverZeroCount > 0,
		"templates_count": len(tpls),
	}, nil
}

// TestingSchemaCheck ensures and validates the full PostgreSQL baseline + additive schema tables.
func (r *Runner) TestingSchemaCheck(ctx context.Context) (map[string]any, error) {
	if err := postgres.EnsureBaselineSchema(ctx, r.Pool); err != nil {
		return nil, err
	}
	var tableCount int64
	err := r.Pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'
	`).Scan(&tableCount)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": "ok", "tables_count": tableCount}, nil
}

// HorizonStatus reports durable PostgreSQL go_job_operations queue counts (replacing Laravel Horizon).
func (r *Runner) HorizonStatus(ctx context.Context) (map[string]any, error) {
	rows, err := r.Pool.Query(ctx, `SELECT status, COUNT(*) FROM go_job_operations GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int64{"queued": 0, "running": 0, "finished": 0, "failed": 0, "unknown": 0}
	for rows.Next() {
		var st string
		var cnt int64
		if rows.Scan(&st, &cnt) == nil {
			counts[st] = cnt
		}
	}
	return map[string]any{"worker_engine": "coolify-worker-postgres", "queues": counts}, rows.Err()
}

// SeedDevTraefikCert idempotently seeds/upserts a development TLS cert row in ssl_certificates for localhost (id=0).
func (r *Runner) SeedDevTraefikCert(ctx context.Context, commonName string) error {
	if strings.TrimSpace(commonName) == "" {
		commonName = "localhost.sslip.io"
	}
	var existingID int64
	err := r.Pool.QueryRow(ctx, `SELECT id FROM ssl_certificates WHERE server_id = 0 AND common_name = $1 LIMIT 1`, commonName).Scan(&existingID)
	if err == nil {
		_, err = r.Pool.Exec(ctx, `UPDATE ssl_certificates SET valid_until = NOW() + INTERVAL '365 days', updated_at = NOW() WHERE id = $1`, existingID)
		return err
	}
	_, err = r.Pool.Exec(ctx, `
		INSERT INTO ssl_certificates (ssl_certificate, ssl_private_key, server_id, common_name, valid_until, is_ca_certificate)
		VALUES ('DEV_CERT_PLACEHOLDER', 'DEV_KEY_PLACEHOLDER', 0, $1, NOW() + INTERVAL '365 days', FALSE)
	`, commonName)
	return err
}

// StartDevInstanceDB verifies standalone_postgresqls id=0 (coolify-db) exists and marks status running.
func (r *Runner) StartDevInstanceDB(ctx context.Context) error {
	tag, err := r.Pool.Exec(ctx, `UPDATE standalone_postgresqls SET status = 'running', updated_at = NOW() WHERE id = 0`)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("instance sentinel coolify-db (id=0) not seeded")
	}
	return nil
}

// SeedDevQemuServer idempotently seeds or verifies a development QEMU server entry for local VM dev testing.
func (r *Runner) SeedDevQemuServer(ctx context.Context, name string, ip string) (string, error) {
	if strings.TrimSpace(name) == "" {
		name = "dev-qemu-vm"
	}
	if strings.TrimSpace(ip) == "" {
		ip = "127.0.0.1"
	}
	u := "dev-qemu-" + strings.ReplaceAll(strings.ToLower(name), " ", "-")
	var id int64
	err := r.Pool.QueryRow(ctx, `SELECT id FROM servers WHERE uuid = $1`, u).Scan(&id)
	if err != nil {
		err = r.Pool.QueryRow(ctx, `
			INSERT INTO servers (uuid, name, ip, port, "user", team_id, private_key_id)
			VALUES ($1, $2, $3, 2222, 'coolify', 0, 0)
			RETURNING id
		`, u, name, ip).Scan(&id)
		if err != nil {
			return "", err
		}
		_, _ = r.Pool.Exec(ctx, `INSERT INTO server_settings (server_id, is_reachable, is_usable) VALUES ($1, TRUE, TRUE) ON CONFLICT DO NOTHING`, id)
	}
	return u, nil
}

func MustJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
