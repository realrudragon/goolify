package worker

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/notifications"
	"github.com/coollabsio/coolify/backend/internal/store/postgres"
)

func TestWorkerMaintenance_SupersededDeploysTokenExpiryAndMissingBackups(t *testing.T) {
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
	if err := postgres.EnsureBaselineSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := postgres.SeedInstanceSentinels(ctx, pool, "root@coolify.test", "$2y$10$h", ""); err != nil {
		t.Fatal(err)
	}

	appKey := "base64:" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	ciph, _ := compat.NewCipher(appKey)
	authz := access.NewAuthorizer()
	notifySvc := notifications.NewPostgresNotificationService(pool, authz, ciph)

	var hits atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	ownerP := auth.Principal{UserID: 0, AuthMethod: "session", SessionTeamID: 0, IsInstanceAdmin: true, TeamRoles: map[int64]auth.Role{0: auth.RoleOwner}}
	if err := notifySvc.SaveConfig(ctx, ownerP, 0, notifications.ChannelConfig{
		Channel: notifications.ChannelWebhook, Enabled: true, WebhookURL: sink.URL,
	}); err != nil {
		t.Fatal(err)
	}

	// Seed superseded queued deployments
	var projID, envID, appID int64
	_ = pool.QueryRow(ctx, `INSERT INTO projects (uuid, name, team_id) VALUES ('p-m', 'M', 0) RETURNING id`).Scan(&projID)
	_ = pool.QueryRow(ctx, `INSERT INTO environments (uuid, name, project_id) VALUES ('e-m', 'prod', $1) RETURNING id`, projID).Scan(&envID)
	_ = pool.QueryRow(ctx, `INSERT INTO applications (uuid, name, environment_id, destination_id) VALUES ('a-m', 'app', $1, 0) RETURNING id`, envID).Scan(&appID)
	_, _ = pool.Exec(ctx, `INSERT INTO application_deployment_queues (application_id, deployment_uuid, status) VALUES ($1, 'dep-old-1', 'queued'), ($1, 'dep-old-2', 'queued'), ($1, 'dep-new', 'queued')`, appID)

	// Seed expiring API token (expires in 2 days) & missing volume backup (1 day window, 0 executions)
	_, _ = pool.Exec(ctx, `
		INSERT INTO personal_access_tokens (tokenable_type, tokenable_id, team_id, name, token, abilities, expires_at)
		VALUES ('App\Models\User', 0, 0, 'ExpiringCI', 'hash-exp-1', '["read"]', NOW() + INTERVAL '2 days')
	`)
	_, _ = pool.Exec(ctx, `
		INSERT INTO scheduled_volume_backups (uuid, backupable_type, backupable_id, team_id, frequency, enabled, missing_backup_notification_days)
		VALUES ('svb-miss-1', 'App\Models\LocalPersistentVolume', 1, 0, '0 0 * * *', TRUE, 1)
	`)

	r := NewRunner(Config{WorkerID: "w-maint", Pool: pool, Cipher: ciph, NotifySvc: notifySvc})

	cancelled, err := r.CancelSupersededDeployments(ctx, appID, "dep-new")
	if err != nil || cancelled != 2 {
		t.Fatalf("expected 2 superseded deployments cancelled, got %d err=%v", cancelled, err)
	}

	tokAlerts, err := r.SweepApiTokenExpirations(ctx, 7)
	if err != nil || tokAlerts != 1 {
		t.Fatalf("expected 1 expiring token alert, got %d err=%v", tokAlerts, err)
	}
	// Second run immediately must be throttled (0 new alerts)
	tokAlerts2, _ := r.SweepApiTokenExpirations(ctx, 7)
	if tokAlerts2 != 0 {
		t.Fatalf("expected throttled second sweep to send 0 alerts, got %d", tokAlerts2)
	}

	bkAlerts, err := r.SweepMissingBackups(ctx)
	if err != nil || bkAlerts != 1 {
		t.Fatalf("expected 1 missing backup alert, got %d err=%v", bkAlerts, err)
	}
	if hits.Load() != 2 {
		t.Fatalf("expected 2 real HTTP webhook alert deliveries, got %d", hits.Load())
	}
}
