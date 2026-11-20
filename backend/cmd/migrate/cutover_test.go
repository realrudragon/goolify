package main

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/coollabsio/coolify/backend/internal/store/postgres"
)

type stubCutoverReader struct{}

func (s *stubCutoverReader) ListTablesInOrder(_ context.Context) ([]string, error) {
	return []string{"teams", "servers", "instance_settings"}, nil
}

func (s *stubCutoverReader) FetchPageStartingFromZero(_ context.Context, table string, afterID *int64, _ int) ([]compat.ValidationRecord, error) {
	if afterID != nil {
		return nil, nil
	}
	now := time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)
	return []compat.ValidationRecord{{Table: table, ID: 0, UUID: table + "-0", UpdatedAt: now}}, nil
}

func (s *stubCutoverReader) ParentExists(_ context.Context, _ string) (bool, error) { return true, nil }

func TestCutover_PreflightAndRollbackAfterPostCutoverWrites(t *testing.T) {
	// Review Focus #5 + Task 13: expand-only schema compatibility & preserving running/unknown remote journals
	journals := []executor.RemoteJournal{
		{OperationID: "op-live-1", State: executor.RemoteRunning},
		{OperationID: "op-unclear-2", State: executor.RemoteUnknown},
	}
	expandChanges := []SchemaChange{
		{Name: "add_go_outbox_events_table", ExpandOnly: true},
		{Name: "add_go_job_operations_table", ExpandOnly: true},
	}
	runner := NewCutoverRunner(&stubCutoverReader{}, []byte("digest-key"), journals, expandChanges)

	receipt, err := runner.Preflight(context.Background())
	if err != nil {
		t.Fatalf("Preflight failed: %v", err)
	}
	if receipt.SentinelZeroTables != 3 || receipt.PreservedRemoteJournals != 2 || !receipt.RollbackCompatible {
		t.Fatalf("unexpected cutover receipt: %+v", receipt)
	}

	// Destructive/non-expand schema change blocks rollback!
	destructive := NewCutoverRunner(&stubCutoverReader{}, []byte("digest-key"), journals, []SchemaChange{
		{Name: "drop_legacy_php_column", ExpandOnly: false},
	})
	if err := destructive.VerifyRollbackCompatibility(context.Background()); !errors.Is(err, ErrNonExpandSchemaBlocksRollback) {
		t.Fatalf("expected ErrNonExpandSchemaBlocksRollback, got %v", err)
	}
}

func TestCutover_RealPostgresTwoRoundRehearsalAndRollback(t *testing.T) {
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

	appKey := "base64:" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	ciph, err := compat.NewCipher(appKey)
	if err != nil {
		t.Fatal(err)
	}

	// Run full isolated rehearsal TWICE per Task 13 acceptance gate
	for round := 1; round <= 2; round++ {
		if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
			t.Fatalf("round %d reset: %v", round, err)
		}
		if err := postgres.EnsureBaselineSchema(ctx, pool); err != nil {
			t.Fatalf("round %d schema: %v", round, err)
		}
		encKey, _ := ciph.EncryptLaravelPayload([]byte("ssh-ed25519-key"), false)
		if err := postgres.SeedInstanceSentinels(ctx, pool, "root@coolify.test", "$2y$10$hash", encKey); err != nil {
			t.Fatalf("round %d sentinels: %v", round, err)
		}
		// Seed representative frozen Laravel rows including NULL, empty string, and whitespace-padded encrypted env vars
		var projID, envID, appID int64
		_ = pool.QueryRow(ctx, `INSERT INTO projects (uuid, name, team_id) VALUES ('proj-legacy', 'Legacy', 0) RETURNING id`).Scan(&projID)
		_ = pool.QueryRow(ctx, `INSERT INTO environments (uuid, name, project_id) VALUES ('env-legacy', 'production', $1) RETURNING id`, projID).Scan(&envID)
		_ = pool.QueryRow(ctx, `INSERT INTO applications (uuid, name, environment_id, destination_id) VALUES ('app-legacy', 'legacy-app', $1, 0) RETURNING id`, envID).Scan(&appID)
		encEmpty, _ := ciph.EncryptLaravelPayload([]byte(""), false)
		encSpaced, _ := ciph.EncryptLaravelPayload([]byte("  preserve-me  "), false)
		encDiscordURL, _ := ciph.EncryptLaravelPayload([]byte("https://discord.example/api/webhooks/123"), false)
		encCert, _ := ciph.EncryptLaravelPayload([]byte("-----BEGIN CERTIFICATE-----\nTEST\n-----END CERTIFICATE-----"), false)
		encCertKey, _ := ciph.EncryptLaravelPayload([]byte("-----BEGIN PRIVATE KEY-----\nTEST\n-----END PRIVATE KEY-----"), false)
		_, _ = pool.Exec(ctx, `INSERT INTO environment_variables (uuid, key, value, resourceable_type, resourceable_id) VALUES ('ev-null', 'NULL_VAR', NULL, 'App\Models\Application', $1)`, appID)
		_, _ = pool.Exec(ctx, `INSERT INTO environment_variables (uuid, key, value, resourceable_type, resourceable_id) VALUES ('ev-empty', 'EMPTY_VAR', $2, 'App\Models\Application', $1)`, appID, encEmpty)
		_, _ = pool.Exec(ctx, `INSERT INTO environment_variables (uuid, key, value, resourceable_type, resourceable_id) VALUES ('ev-ws', 'WS_VAR', $2, 'App\Models\Application', $1)`, appID, encSpaced)
		_, _ = pool.Exec(ctx, `INSERT INTO discord_notification_settings (team_id, discord_enabled, discord_webhook_url) VALUES (0, TRUE, $1)`, encDiscordURL)
		_, _ = pool.Exec(ctx, `INSERT INTO ssl_certificates (ssl_certificate, ssl_private_key, server_id, common_name, valid_until) VALUES ($1, $2, 0, 'test.local', NOW() + INTERVAL '90 days')`, encCert, encCertKey)

		// 1. Pre-cutover dry-run validation (checks env vars + private_keys + ssl_certificates + discord_notification_settings)
		reader := compat.NewPostgresDatasetReader(pool, ciph)
		checked, failed, err := reader.VerifyEncryptedColumns(ctx)
		if err != nil || checked != 6 || failed != 0 {
			t.Fatalf("round %d encrypted check failed: checked=%d failed=%d err=%v", round, checked, failed, err)
		}

		// 2. Simulate post-cutover Go writes + in-flight remote journal
		_, err = pool.Exec(ctx, `UPDATE servers SET name = 'localhost-updated' WHERE id = 0`)
		if err != nil {
			t.Fatal(err)
		}
		probe := executor.NewPostgresRemoteProbe(pool, false)
		if err := probe.WriteJournalAtomic(ctx, "localhost", executor.RemoteJournal{
			ProtocolVersion: "v1", JobID: "job-1", OperationID: "op-inflight-1", AttemptID: "att-1",
			OwnershipEpoch: 1, ResourceUUID: "app-legacy", State: executor.RemoteRunning, BootID: "boot-1",
			UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}

		// 3. Verify rollback compatibility AFTER Go writes (expand-only migrations + preserved remote journal + id=0 sentinels intact!)
		journals, err := reader.LoadRemoteJournals(ctx)
		if err != nil {
			t.Fatal(err)
		}
		migs, err := reader.LoadSchemaMigrations(ctx)
		if err != nil {
			t.Fatal(err)
		}
		changes := make([]SchemaChange, 0, len(migs))
		for _, m := range migs {
			changes = append(changes, SchemaChange{Name: m.Name, ExpandOnly: m.ExpandOnly})
		}
		runner := NewCutoverRunner(reader, []byte("rehearsal-key"), journals, changes)
		receipt, err := runner.Preflight(ctx)
		if err != nil {
			t.Fatalf("round %d Preflight failed: %v", round, err)
		}
		if receipt.SentinelZeroTables < 6 || receipt.BrokenFKCount != 0 || receipt.PreservedRemoteJournals != 1 || !receipt.RollbackCompatible {
			t.Fatalf("round %d unexpected receipt: %+v", round, receipt)
		}

		// Verify frozen Laravel queries still see intact rows (NULL vs empty vs whitespace, id=0 preserved)
		var nullVal *string
		if err := pool.QueryRow(ctx, `SELECT value FROM environment_variables WHERE uuid = 'ev-null'`).Scan(&nullVal); err != nil || nullVal != nil {
			t.Fatalf("round %d NULL_VAR corrupted: %v", round, nullVal)
		}
		var wsEnc string
		if err := pool.QueryRow(ctx, `SELECT value FROM environment_variables WHERE uuid = 'ev-ws'`).Scan(&wsEnc); err != nil {
			t.Fatal(err)
		}
		plainWS, err := ciph.DecryptLaravelPayload(wsEnc, false)
		if err != nil || string(plainWS) != "  preserve-me  " {
			t.Fatalf("round %d whitespace lost after cutover writes: %q", round, string(plainWS))
		}
	}
}
