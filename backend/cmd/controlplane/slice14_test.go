package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/backups"
	"github.com/coollabsio/coolify/backend/internal/domains/schedules"
	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/coollabsio/coolify/backend/internal/worker"
)

type slice14TestDumper struct{}

func (d *slice14TestDumper) Dump(_ context.Context, _ backups.DumpTarget) ([]byte, error) {
	return []byte("-- SQL backup dump for multi-S3 replica verification\nSELECT 1;\n"), nil
}

func (d *slice14TestDumper) Restore(_ context.Context, _ backups.DumpTarget, _ []byte) error {
	return nil
}

func readRespString(r *http.Response) string {
	if r == nil || r.Body == nil {
		return ""
	}
	b, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	return string(b)
}

func TestProductionSlice14_Remaining23ModelsLaravelNotifsAndRealtimeChannels(t *testing.T) {
	app, ts, pool, _, ciph := setupRealControlPlane(t)
	ctx := context.Background()

	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")
	var rootUserID int64
	_ = pool.QueryRow(ctx, `SELECT id FROM users WHERE email = 'root@coolify.test'`).Scan(&rootUserID)

	// Register read-only member on team 0
	mReg := doJSON(t, http.MethodPost, ts.URL+"/app/auth/register", nil, "", "", map[string]string{
		"name": "Member0", "email": "member0-s14@coolify.test", "password": "MemberSecret123!",
	})
	mReg.Body.Close()
	var memberID int64
	_ = pool.QueryRow(ctx, `SELECT id FROM users WHERE email = 'member0-s14@coolify.test'`).Scan(&memberID)
	_, _ = pool.Exec(ctx, `INSERT INTO team_user (team_id, user_id, role) VALUES (0, $1, 'member') ON CONFLICT DO NOTHING`, memberID)
	memberCookies, memberCSRF := loginClient(t, ts.URL, "member0-s14@coolify.test", "MemberSecret123!")
	sw := doJSON(t, http.MethodPost, ts.URL+"/app/auth/switch-team", memberCookies, memberCSRF, "", map[string]int64{"team_id": 0})
	sw.Body.Close()

	// Register attacker owner on separate team B
	oReg := doJSON(t, http.MethodPost, ts.URL+"/app/auth/register", nil, "", "", map[string]string{
		"name": "Other", "email": "other-s14@coolify.test", "password": "OtherSecret123!",
	})
	oReg.Body.Close()
	otherCookies, otherCSRF := loginClient(t, ts.URL, "other-s14@coolify.test", "OtherSecret123!")

	// 1. All 6 Laravel per-channel notification tables + real HTTP delivery + notification_throttles
	var webhookHits atomic.Int32
	mockSink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webhookHits.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer mockSink.Close()

	notifCases := []struct {
		ch      string
		table   string
		encCol  string
		payload map[string]any
		testSnd bool
	}{
		{"discord", "discord_notification_settings", "discord_webhook_url", map[string]any{"discord_enabled": true, "discord_webhook_url": mockSink.URL + "/discord", "discord_ping_enabled": true}, true},
		{"slack", "slack_notification_settings", "slack_webhook_url", map[string]any{"slack_enabled": true, "slack_webhook_url": mockSink.URL + "/slack"}, true},
		{"telegram", "telegram_notification_settings", "telegram_token", map[string]any{"telegram_enabled": true, "telegram_token": "bot-secret-123", "telegram_chat_id": "-100999", "webhook_url": mockSink.URL + "/telegram"}, true},
		{"pushover", "pushover_notification_settings", "pushover_api_token", map[string]any{"pushover_enabled": true, "pushover_user_key": "u-key-1", "pushover_api_token": "api-tok-1", "webhook_url": mockSink.URL + "/pushover"}, true},
		{"webhook", "webhook_notification_settings", "webhook_url", map[string]any{"webhook_enabled": true, "webhook_url": mockSink.URL + "/webhook"}, true},
		{"email", "email_notification_settings", "smtp_password", map[string]any{"smtp_enabled": true, "smtp_host": "127.0.0.1", "smtp_port": 1025, "smtp_from_address": "coolify@test.local", "smtp_recipients": "ops@test.local", "smtp_password": "smtp-secret-password", "resend_enabled": true, "resend_api_key": "re_test_123"}, false},
	}

	for _, nc := range notifCases {
		res := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/notifications/"+nc.ch, rootCookies, rootCSRF, "", nc.payload)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("PATCH /notifications/%s failed: %d %s", nc.ch, res.StatusCode, readRespString(res))
		}
		res.Body.Close()

		res = doJSON(t, http.MethodGet, ts.URL+"/api/v1/notifications/"+nc.ch, rootCookies, "", "", nil)
		body := readRespString(res)
		if res.StatusCode != http.StatusOK || !strings.Contains(body, `"enabled":true`) || !strings.Contains(body, `"configured":true`) {
			t.Fatalf("GET /notifications/%s unexpected: %d %s", nc.ch, res.StatusCode, body)
		}

		// Verify ciphertext actually persisted in exact Laravel table & decrypts via APP_KEY
		var rawEnc string
		if err := pool.QueryRow(ctx, `SELECT `+nc.encCol+` FROM `+nc.table+` WHERE team_id = 0`).Scan(&rawEnc); err != nil || rawEnc == "" {
			t.Fatalf("expected encrypted row in %s.%s, err=%v", nc.table, nc.encCol, err)
		}
		if _, err := ciph.DecryptLaravelPayload(rawEnc, false); err != nil {
			t.Fatalf("%s.%s failed APP_KEY decrypt: %v", nc.table, nc.encCol, err)
		}
		if nc.testSnd {
			res = doJSON(t, http.MethodPost, ts.URL+"/api/v1/notifications/test", rootCookies, rootCSRF, "", map[string]any{
				"channel": nc.ch, "event": "deployment.succeeded", "summary": "Slice 14 test",
			})
			if res.StatusCode != http.StatusOK {
				t.Fatalf("POST /notifications/test (%s) failed: %d %s", nc.ch, res.StatusCode, readRespString(res))
			}
			res.Body.Close()
		}
	}
	if webhookHits.Load() != 5 {
		t.Fatalf("expected 5 real HTTP notification deliveries, got %d", webhookHits.Load())
	}

	// Verify notification_throttles real throttling behavior
	th1, err := app.NotifySvc.CheckAndRecordThrottle(ctx, `App\Models\Server`, 0, "high_disk_usage", 5*time.Minute)
	if err != nil || th1 {
		t.Fatalf("first notification should not be throttled: th=%v err=%v", th1, err)
	}
	th2, err := app.NotifySvc.CheckAndRecordThrottle(ctx, `App\Models\Server`, 0, "high_disk_usage", 5*time.Minute)
	if err != nil || !th2 {
		t.Fatalf("second notification within window must be throttled: th=%v err=%v", th2, err)
	}

	// 2. SSL Certificates (APP_KEY encrypted, redacted private key)
	res := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/localhost/certificates", rootCookies, rootCSRF, "", map[string]any{
		"common_name":       "app.example.test",
		"ssl_certificate":   "-----BEGIN CERTIFICATE-----\nCERTDATA\n-----END CERTIFICATE-----",
		"ssl_private_key":   "-----BEGIN PRIVATE KEY-----\nSUPERSECRETKEY\n-----END PRIVATE KEY-----",
		"is_ca_certificate": false,
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST certificates failed: %d %s", res.StatusCode, readRespString(res))
	}
	res.Body.Close()

	res = doJSON(t, http.MethodGet, ts.URL+"/api/v1/servers/localhost/certificates", rootCookies, "", "", nil)
	certListBody := readRespString(res)
	if res.StatusCode != http.StatusOK || strings.Contains(certListBody, "SUPERSECRETKEY") || !strings.Contains(certListBody, "app.example.test") {
		t.Fatalf("GET certificates leaked key or missed cert: %d %s", res.StatusCode, certListBody)
	}
	if res = doJSON(t, http.MethodGet, ts.URL+"/api/v1/servers/localhost/certificates", otherCookies, "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 cross-team certificates, got %d", res.StatusCode)
	} else {
		res.Body.Close()
	}

	// 3. Swarm Docker destination (appears in /api/v1/destinations too)
	res = doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/localhost/swarm-dockers", rootCookies, rootCSRF, "", map[string]any{"name": "swarm-prod"})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST swarm-dockers failed: %d %s", res.StatusCode, readRespString(res))
	}
	var swarmOut struct {
		UUID string `json:"uuid"`
	}
	_ = json.NewDecoder(res.Body).Decode(&swarmOut)
	res.Body.Close()

	res = doJSON(t, http.MethodGet, ts.URL+"/api/v1/destinations", rootCookies, "", "", nil)
	destBody := readRespString(res)
	if res.StatusCode != http.StatusOK || !strings.Contains(destBody, swarmOut.UUID) || !strings.Contains(destBody, `"type":"swarm"`) {
		t.Fatalf("GET /destinations did not include swarm_dockers: %d %s", res.StatusCode, destBody)
	}

	// 4. Image Scan Policy, CVE Ignore Rules, Server Host Scan & Image Scan Notification State
	res = doJSON(t, http.MethodPatch, ts.URL+"/api/v1/security/image-scan-policy", rootCookies, rootCSRF, "", map[string]any{
		"gate_mode": "block", "on_scan_error": "block", "count_medium": true,
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PATCH image-scan-policy failed: %d %s", res.StatusCode, readRespString(res))
	}
	res.Body.Close()

	res = doJSON(t, http.MethodPost, ts.URL+"/api/v1/security/image-scan-ignores", rootCookies, rootCSRF, "", map[string]any{
		"vulnerability_id": "CVE-2026-9999", "package": "openssl", "reason": "Mitigated at proxy",
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST image-scan-ignores failed: %d %s", res.StatusCode, readRespString(res))
	}
	res.Body.Close()

	res = doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/localhost/host-scan", rootCookies, rootCSRF, "", map[string]any{
		"summary":  map[string]any{"critical": 1, "high": 2, "medium": 0, "low": 0},
		"findings": []map[string]any{{"id": "CVE-2026-1111", "severity": "CRITICAL", "package": "curl"}},
		"eol":      map[string]any{"status": "supported"},
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST host-scan failed: %d %s", res.StatusCode, readRespString(res))
	}
	res.Body.Close()
	var notifStateCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM image_scan_notification_states WHERE resource_type = 'App\Models\Server' AND resource_id = 0`).Scan(&notifStateCount)
	if notifStateCount != 1 {
		t.Fatalf("expected 1 image_scan_notification_states row, got %d", notifStateCount)
	}

	// 5. Project Settings, Stars (with cross-team guard), User Changelog Reads & Stripe Subscriptions
	res = doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", rootCookies, rootCSRF, "", map[string]any{"name": "Slice14Proj"})
	var projEnvelope struct {
		Project struct {
			ID   int64  `json:"id"`
			UUID string `json:"uuid"`
		} `json:"project"`
	}
	_ = json.NewDecoder(res.Body).Decode(&projEnvelope)
	res.Body.Close()
	projCreated := projEnvelope.Project

	res = doJSON(t, http.MethodPatch, ts.URL+"/api/v1/projects/"+projCreated.UUID+"/settings", rootCookies, rootCSRF, "", map[string]any{
		"wildcard_domain": "https://apps.slice14.local",
	})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("PATCH project settings failed: %d %s", res.StatusCode, readRespString(res))
	}
	res.Body.Close()
	res = doJSON(t, http.MethodGet, ts.URL+"/api/v1/projects/"+projCreated.UUID+"/settings", rootCookies, "", "", nil)
	if !strings.Contains(readRespString(res), "apps.slice14.local") {
		t.Fatalf("GET project settings missed wildcard_domain")
	}

	// Stars: allowed for root, forbidden for Team B user trying to star Team 0 project
	res = doJSON(t, http.MethodPost, ts.URL+"/api/v1/stars", rootCookies, rootCSRF, "", map[string]any{"starrable_type": "project", "starrable_id": projCreated.ID, "position": 1})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST stars failed: %d %s", res.StatusCode, readRespString(res))
	}
	res.Body.Close()
	res = doJSON(t, http.MethodPost, ts.URL+"/api/v1/stars", otherCookies, otherCSRF, "", map[string]any{"starrable_type": "project", "starrable_id": projCreated.ID})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 cross-team star, got %d", res.StatusCode)
	}
	res.Body.Close()

	// User Changelog Reads
	res = doJSON(t, http.MethodPost, ts.URL+"/api/v1/changelog/reads", rootCookies, rootCSRF, "", map[string]any{"release_tag": "v4.0.0-standalone"})
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST changelog/reads failed: %d %s", res.StatusCode, readRespString(res))
	}
	res.Body.Close()
	res = doJSON(t, http.MethodGet, ts.URL+"/api/v1/changelog/reads", rootCookies, "", "", nil)
	if !strings.Contains(readRespString(res), "v4.0.0-standalone") {
		t.Fatalf("GET changelog/reads missed recorded tag")
	}

	// Stripe webhook -> real subscriptions persistence + member 403 guard
	stripeBody, _ := json.Marshal(map[string]any{
		"id": "evt_s14_1", "type": "invoice.paid",
		"data": map[string]any{"object": map[string]any{"id": "sub_s14", "customer": "cus_s14", "metadata": map[string]any{"team_id": 0}}},
	})
	reqStripe, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/payments/stripe/events", bytes.NewReader(stripeBody))
	reqStripe.Header.Set("Content-Type", "application/json")
	reqStripe.Header.Set("Stripe-Signature", "t=123,v1=valid")
	respStripe, err := http.DefaultClient.Do(reqStripe)
	if err != nil || respStripe.StatusCode != http.StatusOK {
		t.Fatalf("stripe webhook failed: err=%v status=%d", err, respStripe.StatusCode)
	}
	_ = respStripe.Body.Close()
	if res = doJSON(t, http.MethodGet, ts.URL+"/api/v1/subscription", memberCookies, "", "", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("read-only member must get 403 on /subscription, got %d", res.StatusCode)
	} else {
		res.Body.Close()
	}
	res = doJSON(t, http.MethodGet, ts.URL+"/api/v1/subscription", rootCookies, "", "", nil)
	if subBody := readRespString(res); res.StatusCode != http.StatusOK || !strings.Contains(subBody, `"subscribed":true`) || !strings.Contains(subBody, `"sub_s14"`) {
		t.Fatalf("GET /subscription unexpected: %d %s", res.StatusCode, subBody)
	}

	// 6. GitHub Runner Config + Signed workflow_job Webhook + Worker Execution + Multi-S3 Backup Replicas + ScheduledJobState/Delivery
	whSecret := "gh-runner-webhook-secret"
	whEnc, _ := ciph.EncryptLaravelPayload([]byte(whSecret), false)
	var ghAppID int64
	_ = pool.QueryRow(ctx, `
		INSERT INTO github_apps (uuid, name, installation_id, webhook_secret, team_id)
		VALUES ('gh-app-s14', 'RunnerApp', 777123, $1, 0) RETURNING id
	`, whEnc).Scan(&ghAppID)

	res = doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/localhost/github-runners", rootCookies, rootCSRF, "", map[string]any{
		"github_app_id": ghAppID, "labels": []string{"self-hosted", "linux"}, "max_runners": 3, "is_enabled": true,
	})
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("POST github-runners failed: %d %s", res.StatusCode, readRespString(res))
	}
	res.Body.Close()

	wfPayload := []byte(`{"action":"queued","installation":{"id":777123},"repository":{"full_name":"org/app"},"workflow_job":{"id":987654321,"html_url":"https://github.com/org/app/runs/1","workflow_name":"CI","name":"build","labels":["self-hosted"]}}`)
	mac := hmac.New(sha256.New, []byte(whSecret))
	mac.Write(wfPayload)
	ghSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	reqGH, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/source/github/events", bytes.NewReader(wfPayload))
	reqGH.Header.Set("Content-Type", "application/json")
	reqGH.Header.Set("X-GitHub-Event", "workflow_job")
	reqGH.Header.Set("X-Hub-Signature-256", ghSig)
	respGH, err := http.DefaultClient.Do(reqGH)
	if err != nil || respGH.StatusCode != http.StatusAccepted {
		t.Fatalf("GitHub workflow_job webhook failed: err=%v status=%d", err, respGH.StatusCode)
	}
	_ = respGH.Body.Close()

	// Multi-S3 replica setup + scheduled task + scheduled volume backup
	var s3Puts atomic.Int32
	mockS3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			s3Puts.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer mockS3.Close()
	kEnc, _ := ciph.EncryptLaravelPayload([]byte("AKIA1"), false)
	sEnc, _ := ciph.EncryptLaravelPayload([]byte("SECRET1"), false)
	var s3ID1, s3ID2 int64
	_ = pool.QueryRow(ctx, `INSERT INTO s3_storages (uuid, name, region, bucket, endpoint, key, secret, team_id) VALUES ('s3-s14-1','Primary','us-east-1','bk1',$1,$2,$3,0) RETURNING id`, mockS3.URL, kEnc, sEnc).Scan(&s3ID1)
	_ = pool.QueryRow(ctx, `INSERT INTO s3_storages (uuid, name, region, bucket, endpoint, key, secret, team_id) VALUES ('s3-s14-2','Replica','us-east-1','bk2',$1,$2,$3,0) RETURNING id`, mockS3.URL, kEnc, sEnc).Scan(&s3ID2)

	var dbSchedID int64
	_ = pool.QueryRow(ctx, `
		INSERT INTO scheduled_database_backups (uuid, frequency, timezone, enabled, save_s3, s3_storage_id, database_id, database_type, team_id, database_retention_count)
		VALUES ('sdb-s14', '* * * * *', 'UTC', TRUE, TRUE, $1, 0, 'postgresql', 0, 5) RETURNING id
	`, s3ID1).Scan(&dbSchedID)
	_, _ = pool.Exec(ctx, `INSERT INTO scheduled_database_backup_s3_storage (scheduled_database_backup_id, s3_storage_id) VALUES ($1, $2), ($1, $3)`, dbSchedID, s3ID1, s3ID2)

	_, _ = pool.Exec(ctx, `INSERT INTO scheduled_tasks (uuid, enabled, name, command, frequency, team_id) VALUES ('st-s14', TRUE, 'Nightly', 'echo hi', '* * * * *', 0)`)
	var volSchedID int64
	_ = pool.QueryRow(ctx, `INSERT INTO scheduled_volume_backups (uuid, backupable_type, backupable_id, team_id, frequency, enabled, save_s3) VALUES ('svb-s14', 'App\Models\LocalPersistentVolume', 1, 0, '* * * * *', TRUE, TRUE) RETURNING id`).Scan(&volSchedID)
	_, _ = pool.Exec(ctx, `INSERT INTO scheduled_volume_backup_s3_storage (scheduled_volume_backup_id, s3_storage_id) VALUES ($1, $2)`, volSchedID, s3ID1)

	backupDir := t.TempDir()
	realBackupSvc := backups.NewPostgresBackupService(pool, app.Authz, compat.NewEloquentHooks(), &slice14TestDumper{}, backupDir)
	wRunner := worker.NewRunner(worker.Config{
		WorkerID:      "worker-s14",
		Pool:          pool,
		Cipher:        ciph,
		JobCoord:      app.JobCoord,
		ScheduleCoord: schedules.NewPostgresScheduleCoordinator(pool),
		Outbox:        app.Outbox,
		Dispatcher:    app.Dispatcher,
		LogStore:      app.LogStore,
		Executor:      executor.NewClient(&sliceTestTransport{}),
		BackupSvc:     realBackupSvc,
		BackupDir:     backupDir,
	})

	now := time.Now().UTC()
	enq1, err := wRunner.TickSchedules(ctx, now)
	if err != nil || enq1 < 3 {
		t.Fatalf("TickSchedules expected >=3 due jobs (db backup + task + vol backup), got %d err=%v", enq1, err)
	}
	enq2, _ := wRunner.TickSchedules(ctx, now)
	if enq2 != 0 {
		t.Fatalf("second TickSchedules in same minute must enqueue 0 duplicates, got %d", enq2)
	}
	for i := 0; i < 10; i++ {
		worked, err := wRunner.ProcessOneJob(ctx)
		if err != nil {
			t.Fatalf("ProcessOneJob error: %v", err)
		}
		if !worked {
			break
		}
	}

	var dbReplicaCount, volReplicaCount, schedStateCount, schedDelivCount, ghDoneCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM database_backup_s3_replicas WHERE s3_uploaded = TRUE`).Scan(&dbReplicaCount)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM volume_backup_s3_replicas WHERE s3_uploaded = TRUE`).Scan(&volReplicaCount)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM scheduled_job_states`).Scan(&schedStateCount)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM scheduled_job_deliveries`).Scan(&schedDelivCount)
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM github_runner_executions WHERE status = 'completed' AND conclusion = 'success'`).Scan(&ghDoneCount)
	if dbReplicaCount != 2 || s3Puts.Load() < 2 {
		t.Fatalf("expected 2 uploaded database_backup_s3_replicas & >=2 S3 PUTs, got replicas=%d puts=%d", dbReplicaCount, s3Puts.Load())
	}
	if volReplicaCount != 1 {
		t.Fatalf("expected 1 volume_backup_s3_replicas row, got %d", volReplicaCount)
	}
	if schedStateCount < 3 || schedDelivCount < 3 {
		t.Fatalf("expected >=3 scheduled_job_states and deliveries, got states=%d deliveries=%d", schedStateCount, schedDelivCount)
	}
	if ghDoneCount != 1 {
		t.Fatalf("expected 1 completed github_runner_executions row, got %d", ghDoneCount)
	}

	res = doJSON(t, http.MethodGet, ts.URL+"/api/v1/servers/localhost/github-runners/executions", rootCookies, "", "", nil)
	if ghBody := readRespString(res); res.StatusCode != http.StatusOK || !strings.Contains(ghBody, `"completed"`) {
		t.Fatalf("GET github-runners/executions unexpected: %d %s", res.StatusCode, ghBody)
	}

	// 7. All 7 Remaining Inventoried Routes: Realtime channels (team.{id}, user.{id}), API 404, Web 404, Dev-only routes
	res = doJSON(t, http.MethodGet, ts.URL+"/realtime/events?channel=team.0&once=1", rootCookies, "", "", nil)
	if body := readRespString(res); res.StatusCode != http.StatusOK || !strings.Contains(body, `"channel":"team.0"`) {
		t.Fatalf("team.0 realtime channel failed: %d %s", res.StatusCode, body)
	}
	res = doJSON(t, http.MethodGet, ts.URL+"/realtime/events?channel=team.0&once=1", otherCookies, "", "", nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-team realtime channel team.0 must return 403, got %d", res.StatusCode)
	}
	res.Body.Close()

	res = doJSON(t, http.MethodGet, fmt.Sprintf("%s/realtime/events?channel=user.%d&once=1", ts.URL, rootUserID), rootCookies, "", "", nil)
	if body := readRespString(res); res.StatusCode != http.StatusOK || !strings.Contains(body, fmt.Sprintf(`"channel":"user.%d"`, rootUserID)) {
		t.Fatalf("user.%d realtime channel failed: %d %s", rootUserID, res.StatusCode, body)
	}
	res = doJSON(t, http.MethodGet, fmt.Sprintf("%s/realtime/events?channel=user.%d&once=1", ts.URL, rootUserID), otherCookies, "", "", nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-user realtime channel must return 403, got %d", res.StatusCode)
	}
	res.Body.Close()

	// RT-API-0348 & RT-WEB-0629
	res = doJSON(t, http.MethodGet, ts.URL+"/api/v1/does-not-exist-xyz", rootCookies, "", "", nil)
	if res.StatusCode != http.StatusNotFound || !strings.Contains(res.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("expected 404 JSON on unknown /api/v1 path, got %d ct=%s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	res.Body.Close()
	res = doJSON(t, http.MethodGet, ts.URL+"/totally-unknown-route-xyz", rootCookies, "", "", nil)
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 on unknown web fallback, got %d", res.StatusCode)
	}
	res.Body.Close()

	// RT-WEB-0357, RT-WEB-0358, RT-WEB-0359: 404 in production, preview status in testing
	t.Setenv("APP_ENV", "production")
	for _, devPath := range []string{"/__livewire-request-failure", "/__exception", "/__error/404"} {
		res = doJSON(t, http.MethodGet, ts.URL+devPath, rootCookies, "", "", nil)
		if res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s must return 404 in production, got %d", devPath, res.StatusCode)
		}
		res.Body.Close()
	}
	t.Setenv("APP_ENV", "testing")
	if res = doJSON(t, http.MethodGet, ts.URL+"/__error/503", rootCookies, "", "", nil); res.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("/__error/503 expected 503 in testing env, got %d", res.StatusCode)
	} else {
		res.Body.Close()
	}
	if res = doJSON(t, http.MethodGet, ts.URL+"/__exception", rootCookies, "", "", nil); res.StatusCode != http.StatusInternalServerError {
		t.Fatalf("/__exception expected 500 in testing env, got %d", res.StatusCode)
	} else {
		res.Body.Close()
	}
	if res = doJSON(t, http.MethodGet, ts.URL+"/__livewire-request-failure", rootCookies, "", "", nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("/__livewire-request-failure expected 400 in testing env, got %d", res.StatusCode)
	} else {
		res.Body.Close()
	}

	// Verify compat.PostgresDatasetReader validates ALL newly added encrypted baseline tables!
	reader := compat.NewPostgresDatasetReader(pool, ciph)
	checked, failed, err := reader.VerifyEncryptedColumns(ctx)
	if err != nil || failed != 0 || checked < 10 {
		t.Fatalf("VerifyEncryptedColumns across all tables failed: checked=%d failed=%d err=%v", checked, failed, err)
	}
}
