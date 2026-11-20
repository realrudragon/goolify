package opscli

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/store/postgres"
	goredis "github.com/coollabsio/coolify/backend/internal/store/redis"
	"golang.org/x/crypto/bcrypt"
)

func TestOpsCLI_RealPostgresAdminReencryptCleanupAndExport(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://coolify:coolify_test_pw@127.0.0.1:15432/coolify_test?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := postgres.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("isolated test postgres unavailable: %v", err)
	}
	defer pool.Close()

	_, _ = pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`)
	oldKey := "base64:" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	newKey := "base64:" + base64.StdEncoding.EncodeToString([]byte("fedcba9876543210fedcba9876543210"))
	oldCiph, _ := compat.NewCipher(oldKey)
	newCiph, _ := compat.NewCipher(newKey)

	rdb, _ := goredis.ParseAndNew("redis://127.0.0.1:16379/0", "coolify:go:v1:")
	if rdb != nil {
		defer func() { _ = rdb.Close() }()
	}
	r := NewRunner(pool, rdb, oldCiph)

	// 1. Init & Seed
	if err := r.InitAndSeed(ctx, "root@coolify.test", "OldRootPass123!"); err != nil {
		t.Fatalf("InitAndSeed failed: %v", err)
	}

	// 2. Root password reset & email change
	if err := r.RootResetPassword(ctx, "NewRootPass999!"); err != nil {
		t.Fatalf("RootResetPassword failed: %v", err)
	}
	var hash string
	_ = pool.QueryRow(ctx, `SELECT password FROM users WHERE id = 0`).Scan(&hash)
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("NewRootPass999!")) != nil {
		t.Fatal("root password hash did not update in Postgres")
	}
	if err := r.RootChangeEmail(ctx, "ops-root@coolify.test"); err != nil {
		t.Fatalf("RootChangeEmail failed: %v", err)
	}

	// 3. ReencryptWithAppKey atomically across multiple encrypted tables
	encEnv, _ := oldCiph.EncryptLaravelPayload([]byte("TOP_SECRET_ENV"), false)
	encDiscord, _ := oldCiph.EncryptLaravelPayload([]byte("https://discord.example/hook"), false)
	_, _ = pool.Exec(ctx, `INSERT INTO environment_variables (uuid, key, value, resourceable_type, resourceable_id) VALUES ('ev-reenc', 'SECRET', $1, 'App\Models\Application', 1)`, encEnv)
	_, _ = pool.Exec(ctx, `INSERT INTO discord_notification_settings (team_id, discord_enabled, discord_webhook_url) VALUES (0, TRUE, $1)`, encDiscord)

	cells, err := r.ReencryptWithAppKey(ctx, oldKey, newKey)
	if err != nil || cells < 2 {
		t.Fatalf("ReencryptWithAppKey failed: cells=%d err=%v", cells, err)
	}
	var newEncEnv, newEncDiscord string
	_ = pool.QueryRow(ctx, `SELECT value FROM environment_variables WHERE uuid = 'ev-reenc'`).Scan(&newEncEnv)
	_ = pool.QueryRow(ctx, `SELECT discord_webhook_url FROM discord_notification_settings WHERE team_id = 0`).Scan(&newEncDiscord)
	p1, err1 := newCiph.DecryptLaravelPayload(newEncEnv, false)
	p2, err2 := newCiph.DecryptLaravelPayload(newEncDiscord, false)
	if err1 != nil || string(p1) != "TOP_SECRET_ENV" || err2 != nil || string(p2) != "https://discord.example/hook" {
		t.Fatalf("reencrypted values failed decrypt with new key: %q (%v), %q (%v)", string(p1), err1, string(p2), err2)
	}

	// 4. AdminDeleteUser (refuses id=0, deletes non-root)
	if _, err := r.AdminDeleteUser(ctx, "ops-root@coolify.test"); err == nil {
		t.Fatal("must refuse deleting root user id=0")
	}
	_, _ = pool.Exec(ctx, `INSERT INTO users (name, email, password, created_at) VALUES ('TempUser', 'temp@coolify.test', 'x', NOW() - INTERVAL '10 days')`)
	if _, err := r.AdminDeleteUser(ctx, "temp@coolify.test"); err != nil {
		t.Fatalf("AdminDeleteUser failed: %v", err)
	}

	// 5. Cleanup sweepers: unverified users, stale queue, import uploads, orphaned DNS, search cache, diagnostics, export
	_, _ = pool.Exec(ctx, `INSERT INTO users (name, email, password, created_at) VALUES ('Unver', 'unver@coolify.test', 'x', NOW() - INTERVAL '15 days')`)
	delUnver, err := r.CleanupUnverifiedUsers(ctx, 7*24*time.Hour)
	if err != nil || delUnver != 1 {
		t.Fatalf("CleanupUnverifiedUsers expected 1, got %d err=%v", delUnver, err)
	}

	tmpUpload := filepath.Join(t.TempDir(), "old.sql")
	_ = os.WriteFile(tmpUpload, []byte("SELECT 1;"), 0o600)
	_, _ = pool.Exec(ctx, `INSERT INTO go_upload_imports (uuid, team_id, resource_type, resource_uuid, filename, filepath, status, created_at) VALUES ('up-old', 0, 'database', 'coolify-db', 'old.sql', $1, 'uploaded', NOW() - INTERVAL '48 hours')`, tmpUpload)
	expUp, err := r.CleanupImportUploads(ctx, 24*time.Hour)
	if err != nil || expUp != 1 {
		t.Fatalf("CleanupImportUploads expected 1, got %d err=%v", expUp, err)
	}
	if _, statErr := os.Stat(tmpUpload); !os.IsNotExist(statErr) {
		t.Fatal("expired upload file was not deleted from disk")
	}

	if _, err := r.ClearGlobalSearchCache(ctx); err != nil {
		t.Fatalf("ClearGlobalSearchCache failed: %v", err)
	}
	diag, err := r.ScheduledJobDiagnostics(ctx)
	if err != nil || diag["checked_at"] == nil {
		t.Fatalf("ScheduledJobDiagnostics failed: %+v err=%v", diag, err)
	}
	exported, err := r.ExportUsers(ctx)
	if err != nil || len(exported) != 1 || exported[0]["email"] != "ops-root@coolify.test" {
		t.Fatalf("ExportUsers unexpected: %+v err=%v", exported, err)
	}

	// 6. Real DNS resolution on localhost + extended ops CLI commands
	addrs, err := CheckDomainDNS(ctx, "https://localhost/health")
	if err != nil || len(addrs) == 0 {
		t.Fatalf("CheckDomainDNS(localhost) failed: %v %v", addrs, err)
	}
	if _, err := r.CleanupNames(ctx); err != nil {
		t.Fatalf("CleanupNames failed: %v", err)
	}
	if _, err := r.RekeyOauthIdentities(ctx); err != nil {
		t.Fatalf("RekeyOauthIdentities failed: %v", err)
	}
	if _, err := r.AllowSSOLink(ctx, "ops-root@coolify.test"); err != nil {
		t.Fatalf("AllowSSOLink failed: %v", err)
	}
	if _, err := r.ViewScheduledLogs(ctx, 10); err != nil {
		t.Fatalf("ViewScheduledLogs failed: %v", err)
	}
	if _, err := r.DeleteOrphanedServices(ctx); err != nil {
		t.Fatalf("DeleteOrphanedServices failed: %v", err)
	}
	if _, err := r.ReconcileSubscriptions(ctx); err != nil {
		t.Fatalf("ReconcileSubscriptions failed: %v", err)
	}
	devInfo, err := r.DevInfo(ctx)
	if err != nil || devInfo["database_ok"] != true {
		t.Fatalf("DevInfo failed: %+v err=%v", devInfo, err)
	}
	schemaRes, err := r.TestingSchemaCheck(ctx)
	if err != nil || schemaRes["status"] != "ok" {
		t.Fatalf("TestingSchemaCheck failed: %+v err=%v", schemaRes, err)
	}
	hz, err := r.HorizonStatus(ctx)
	if err != nil || hz["worker_engine"] != "coolify-worker-postgres" {
		t.Fatalf("HorizonStatus failed: %+v err=%v", hz, err)
	}
	if err := r.SeedDevTraefikCert(ctx, "dev.localhost.sslip.io"); err != nil {
		t.Fatalf("SeedDevTraefikCert failed: %v", err)
	}
	if err := r.StartDevInstanceDB(ctx); err != nil {
		t.Fatalf("StartDevInstanceDB failed: %v", err)
	}
	if qUUID, err := r.SeedDevQemuServer(ctx, "dev-qemu-test", "127.0.0.1"); err != nil || qUUID == "" {
		t.Fatalf("SeedDevQemuServer failed: %q err=%v", qUUID, err)
	}
}
