package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/controlplane"
	"github.com/coollabsio/coolify/backend/internal/domains/backups"
	"github.com/coollabsio/coolify/backend/internal/domains/schedules"
	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/coollabsio/coolify/backend/internal/store/postgres"
	goredis "github.com/coollabsio/coolify/backend/internal/store/redis"
	"github.com/coollabsio/coolify/backend/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

func setupRealControlPlane(t *testing.T) (*controlplane.App, *httptest.Server, *pgxpool.Pool, *goredis.Client, *compat.Cipher) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://coolify:coolify_test_pw@127.0.0.1:15432/coolify_test?sslmode=disable"
	}
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://127.0.0.1:16379/0"
	}

	ctx := context.Background()
	pool, err := postgres.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("isolated test postgres unavailable: %v", err)
	}
	t.Cleanup(func() { pool.Close() })

	if _, err := pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`); err != nil {
		t.Fatalf("reset test schema: %v", err)
	}
	if err := postgres.EnsureBaselineSchema(ctx, pool); err != nil {
		t.Fatalf("EnsureBaselineSchema: %v", err)
	}

	appKey := "base64:" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	ciph, err := compat.NewCipher(appKey)
	if err != nil {
		t.Fatal(err)
	}
	encKey0, _ := ciph.EncryptLaravelPayload([]byte("-----BEGIN OPENSSH PRIVATE KEY-----\ntest-key-0\n-----END OPENSSH PRIVATE KEY-----"), false)
	rootHashBytes, _ := bcrypt.GenerateFromPassword([]byte("RootSecret123!"), bcrypt.MinCost)
	laravelRootHash := "$2y$" + string(rootHashBytes[4:])
	if err := postgres.SeedInstanceSentinels(ctx, pool, "root@coolify.test", laravelRootHash, encKey0); err != nil {
		t.Fatalf("SeedInstanceSentinels: %v", err)
	}

	rc, err := goredis.ParseAndNew(redisURL, "coolify:go:v1:cp:")
	if err != nil {
		t.Fatal(err)
	}
	if err := rc.Ping(ctx); err != nil {
		t.Skipf("isolated test redis unavailable: %v", err)
	}
	_, _ = rc.FlushNamespace(ctx)
	t.Cleanup(func() {
		_, _ = rc.FlushNamespace(context.Background())
		_ = rc.Close()
	})

	staticFS := fstest.MapFS{
		"index.html":       {Data: []byte("<!doctype html><html><body>Dashboard Home</body></html>")},
		"app/index.html":   {Data: []byte("<!doctype html><html><body>Coolify App Shell</body></html>")},
		"login/index.html": {Data: []byte("<!doctype html><html><body>Sign in to Coolify</body></html>")},
	}

	app, err := controlplane.NewApp(controlplane.Config{
		Pool:      pool,
		Redis:     rc,
		Cipher:    ciph,
		StaticFS:  staticFS,
		BackupDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewApp: %v", err)
	}
	ts := httptest.NewServer(app)
	t.Cleanup(ts.Close)
	return app, ts, pool, rc, ciph
}

func loginClient(t *testing.T, baseURL, email, password string) (cookies []*http.Cookie, csrf string) {
	t.Helper()
	payload, _ := json.Marshal(map[string]string{"email": email, "password": password})
	resp, err := http.Post(baseURL+"/app/auth/login", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("login POST failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("login failed (%d): %s", resp.StatusCode, string(body))
	}
	var out struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.Cookies(), out.CSRFToken
}

func doJSON(t *testing.T, method, fullURL string, cookies []*http.Cookie, csrf string, bearer string, body any) *http.Response {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, fullURL, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		req.Header.Set("X-CSRF-TOKEN", csrf)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, fullURL, err)
	}
	return resp
}

func TestProductionControlPlane_AuthCSRFLoginRoutingAndCrossTeamIsolation(t *testing.T) {
	_, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()

	// 1. Unauthenticated requests MUST return 401 (never default root!)
	for _, path := range []string{"/api/v1/servers", "/api/v1/projects", "/app/auth/me", "/realtime/logs?resource=localhost"} {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 unauthenticated for %s, got %d", path, resp.StatusCode)
		}
	}

	// 2. /login MUST serve login/index.html, NOT dashboard shell
	respLogin, err := http.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	loginBody, _ := io.ReadAll(respLogin.Body)
	respLogin.Body.Close()
	if respLogin.StatusCode != http.StatusOK || !strings.Contains(string(loginBody), "Sign in to Coolify") {
		t.Fatalf("expected /login page, got %d %s", respLogin.StatusCode, string(loginBody))
	}

	// 3. Login as root (verifying real $2y$ bcrypt hash in PostgreSQL)
	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	// 4. GET /api/v1/servers/localhost returns sentinel id=0
	respSrv := doJSON(t, http.MethodGet, ts.URL+"/api/v1/servers/localhost", rootCookies, "", "", nil)
	srvBody, _ := io.ReadAll(respSrv.Body)
	respSrv.Body.Close()
	if respSrv.StatusCode != http.StatusOK || !strings.Contains(string(srvBody), `"id":0`) {
		t.Fatalf("expected localhost server id=0, got %d %s", respSrv.StatusCode, string(srvBody))
	}

	// 5. Mutating request WITHOUT CSRF MUST return 403
	respNoCSRF := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", rootCookies, "", "", map[string]any{"name": "FailNoCSRF"})
	respNoCSRF.Body.Close()
	if respNoCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 CSRF mismatch without header, got %d", respNoCSRF.StatusCode)
	}

	// 6. Switched-session resource-team isolation (Review Focus #1) via REAL HTTP endpoints!
	// Register Bob (owns personal team > 0) and add Bob as read-only member of Team 0
	regResp := doJSON(t, http.MethodPost, ts.URL+"/app/auth/register", nil, "", "", map[string]string{
		"name": "Bob", "email": "bob@coolify.test", "password": "BobPassword123!", "team_name": "Bob Team",
	})
	var regData struct {
		UserID        int64  `json:"user_id"`
		SessionTeamID int64  `json:"session_team_id"`
		CSRFToken     string `json:"csrf_token"`
	}
	_ = json.NewDecoder(regResp.Body).Decode(&regData)
	bobCookies := regResp.Cookies()
	regResp.Body.Close()
	if regResp.StatusCode != http.StatusCreated || regData.UserID <= 0 {
		t.Fatalf("register Bob failed: %d", regResp.StatusCode)
	}
	_, err = pool.Exec(ctx, `INSERT INTO team_user (team_id, user_id, role, access) VALUES (0, $1, 'member', NULL)`, regData.UserID)
	if err != nil {
		t.Fatal(err)
	}
	// Bob's session is on Bob's own team where Bob is OWNER. Attempting PATCH on Team 0's localhost server MUST fail 403!
	patchResp := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/servers/localhost", bobCookies, regData.CSRFToken, "", map[string]any{"name": "pwned"})
	patchResp.Body.Close()
	if patchResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 when switched-session owner attempts update on team 0 server, got %d", patchResp.StatusCode)
	}

	// 7. MCP vs REST API switch (SECURITY.md): disable REST API, enable MCP -> Bearer fails REST 403, succeeds MCP 200!
	tokResp := doJSON(t, http.MethodPost, ts.URL+"/app/auth/tokens", rootCookies, rootCSRF, "", map[string]any{
		"name": "ops-tok", "abilities": []string{"read"},
	})
	var tokOut struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(tokResp.Body).Decode(&tokOut)
	tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusCreated || tokOut.Token == "" {
		t.Fatalf("create token failed: %d", tokResp.StatusCode)
	}
	setResp := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/settings", rootCookies, rootCSRF, "", map[string]any{
		"is_api_enabled": false, "is_mcp_server_enabled": true,
	})
	setResp.Body.Close()
	if setResp.StatusCode != http.StatusOK {
		t.Fatalf("update settings failed: %d", setResp.StatusCode)
	}
	restBlocked := doJSON(t, http.MethodGet, ts.URL+"/api/v1/servers", nil, "", tokOut.Token, nil)
	restBlocked.Body.Close()
	if restBlocked.StatusCode != http.StatusForbidden {
		t.Fatalf("expected REST 403 when is_api_enabled=false, got %d", restBlocked.StatusCode)
	}
	mcpOK := doJSON(t, http.MethodPost, ts.URL+"/mcp", nil, "", tokOut.Token, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "list_servers",
	})
	mcpBody, _ := io.ReadAll(mcpOK.Body)
	mcpOK.Body.Close()
	if mcpOK.StatusCode != http.StatusOK || !strings.Contains(string(mcpBody), "localhost") {
		t.Fatalf("expected MCP 200 with localhost when REST disabled, got %d %s", mcpOK.StatusCode, string(mcpBody))
	}
}

func TestProductionEndToEnd_DeployWorkerWebhookLogsTerminalNotifyBackupTransfer(t *testing.T) {
	app, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()
	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	// 1. Create Project + Application with GitHub webhook secret
	pResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", rootCookies, rootCSRF, "", map[string]any{
		"name": "Prod Project", "description": "E2E test",
	})
	var pOut struct {
		Project struct {
			UUID string `json:"uuid"`
		} `json:"project"`
		DefaultEnv struct {
			UUID string `json:"uuid"`
		} `json:"default_environment"`
	}
	_ = json.NewDecoder(pResp.Body).Decode(&pOut)
	pResp.Body.Close()
	if pResp.StatusCode != http.StatusCreated || pOut.DefaultEnv.UUID == "" {
		t.Fatalf("create project failed: %d %+v", pResp.StatusCode, pOut)
	}

	appResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications", rootCookies, rootCSRF, "", map[string]any{
		"name": "web-api", "environment_uuid": pOut.DefaultEnv.UUID, "server_uuid": "localhost",
		"build_pack": "dockerimage", "docker_registry_image_name": "nginx", "docker_registry_image_tag": "alpine",
		"webhook_secret_github": "gh-super-secret-42",
	})
	var appOut struct {
		UUID string `json:"uuid"`
	}
	_ = json.NewDecoder(appResp.Body).Decode(&appOut)
	appResp.Body.Close()
	if appResp.StatusCode != http.StatusCreated || appOut.UUID == "" {
		t.Fatalf("create application failed: %d", appResp.StatusCode)
	}

	// Set locked + whitespace-preserved env var on app
	evResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/envs", rootCookies, rootCSRF, "", map[string]any{
		"key": "SPACED_SECRET", "value": "  keep-spaces  ", "is_runtime": true, "is_shown_once": true,
	})
	evResp.Body.Close()
	if evResp.StatusCode != http.StatusCreated {
		t.Fatalf("set env var failed: %d", evResp.StatusCode)
	}

	// 2. Real Webhook HMAC verification + replay rejection + real deploy enqueue
	whBody := []byte(`{"ref":"refs/heads/main"}`)
	badWhReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/github/"+appOut.UUID, bytes.NewReader(whBody))
	badWhReq.Header.Set("X-Hub-Signature-256", "sha256=deadbeef")
	badWhResp, _ := http.DefaultClient.Do(badWhReq)
	badWhResp.Body.Close()
	if badWhResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on invalid webhook signature, got %d", badWhResp.StatusCode)
	}

	mac := hmac.New(sha256.New, []byte("gh-super-secret-42"))
	mac.Write(whBody)
	validSig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	goodWhReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/github/"+appOut.UUID, bytes.NewReader(whBody))
	goodWhReq.Header.Set("X-Hub-Signature-256", validSig)
	goodWhReq.Header.Set("X-GitHub-Delivery", "deliv-unique-1")
	goodWhResp, _ := http.DefaultClient.Do(goodWhReq)
	goodWhResp.Body.Close()
	if goodWhResp.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 on valid signed webhook, got %d", goodWhResp.StatusCode)
	}
	// Replay of same delivery ID MUST be rejected 409!
	replayReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/webhooks/github/"+appOut.UUID, bytes.NewReader(whBody))
	replayReq.Header.Set("X-Hub-Signature-256", validSig)
	replayReq.Header.Set("X-GitHub-Delivery", "deliv-unique-1")
	replayResp, _ := http.DefaultClient.Do(replayReq)
	replayResp.Body.Close()
	if replayResp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on replayed webhook delivery, got %d", replayResp.StatusCode)
	}

	// 3. Real Go Worker claims the queued webhook deployment, executes a real OS command, streams logs to Postgres, updates status!
	w := worker.NewRunner(worker.Config{
		WorkerID:      "e2e-worker-1",
		Pool:          pool,
		JobCoord:      app.JobCoord,
		ScheduleCoord: schedules.NewPostgresScheduleCoordinator(pool),
		Outbox:        app.Outbox,
		Dispatcher:    app.Dispatcher,
		LogStore:      app.LogStore,
		Executor:      executor.NewClient(executor.NewLocalCommandTransport()),
		Probe:         executor.NewPostgresRemoteProbe(pool, false),
		CommandOverride: func(spec jobs.DurableJobSpec) (string, string) {
			return "local_exec", "echo DEPLOY_STEP_1_OK; echo DEPLOY_COMPLETE_" + spec.ResourceUUID
		},
	})
	worked, err := w.ProcessOneJob(ctx)
	if err != nil || !worked {
		t.Fatalf("worker ProcessOneJob failed: worked=%v err=%v", worked, err)
	}

	// Verify real deployment status updated to "finished" AND real streamed logs available via SSE!
	histResp := doJSON(t, http.MethodGet, ts.URL+"/api/v1/deployments/applications/"+appOut.UUID, rootCookies, "", "", nil)
	histBody, _ := io.ReadAll(histResp.Body)
	histResp.Body.Close()
	if histResp.StatusCode != http.StatusOK || !strings.Contains(string(histBody), `"status":"finished"`) {
		t.Fatalf("expected finished deployment in history, got %d %s", histResp.StatusCode, string(histBody))
	}
	sseResp := doJSON(t, http.MethodGet, ts.URL+"/realtime/logs?resource="+appOut.UUID+"&once=1", rootCookies, "", "", nil)
	sseBody, _ := io.ReadAll(sseResp.Body)
	sseResp.Body.Close()
	if sseResp.StatusCode != http.StatusOK || !strings.Contains(string(sseBody), "DEPLOY_COMPLETE_"+appOut.UUID) {
		t.Fatalf("expected real worker output in SSE stream, got %d %s", sseResp.StatusCode, string(sseBody))
	}

	// 4. Real WebSocket Terminal (single-use ticket + live command execution + replay rejection!)
	tktResp := doJSON(t, http.MethodPost, ts.URL+"/app/terminal/ticket", rootCookies, rootCSRF, "", map[string]string{
		"server_uuid": "localhost",
	})
	var tktOut struct {
		Ticket string `json:"ticket"`
	}
	_ = json.NewDecoder(tktResp.Body).Decode(&tktOut)
	tktResp.Body.Close()
	if tktResp.StatusCode != http.StatusCreated || tktOut.Ticket == "" {
		t.Fatalf("issue terminal ticket failed: %d", tktResp.StatusCode)
	}

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/terminal/ws?ticket=" + tktOut.Ticket
	hdr := http.Header{}
	for _, c := range rootCookies {
		hdr.Add("Cookie", c.Name+"="+c.Value)
	}
	wsConn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	_, banner, err := wsConn.Read(ctx)
	if err != nil || !strings.Contains(string(banner), "Connected to localhost") {
		t.Fatalf("unexpected WS banner: %q err=%v", string(banner), err)
	}
	if err := wsConn.Write(ctx, websocket.MessageText, []byte(`{"type":"stdin","data":"echo TERMINAL_LIVE_OK\n"}`)); err != nil {
		t.Fatalf("ws write failed: %v", err)
	}
	_, outMsg, err := wsConn.Read(ctx)
	if err != nil || !strings.Contains(string(outMsg), "TERMINAL_LIVE_OK") {
		t.Fatalf("expected live command output over WS, got %q err=%v", string(outMsg), err)
	}
	_ = wsConn.Close(websocket.StatusNormalClosure, "done")

	// Replaying the SAME terminal ticket MUST be rejected!
	if _, replayHTTP, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr}); err == nil {
		t.Fatal("CRITICAL: terminal ticket replay succeeded!")
	} else if replayHTTP != nil && replayHTTP.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 on terminal ticket replay, got %d", replayHTTP.StatusCode)
	}

	// 5. Real HTTP Notification delivery + failure propagation
	var receivedNotify []byte
	var failNotify bool
	notifySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if failNotify {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		receivedNotify, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer notifySrv.Close()

	saveN := doJSON(t, http.MethodPut, ts.URL+"/api/v1/notifications", rootCookies, rootCSRF, "", map[string]any{
		"channel": "webhook", "enabled": true, "webhook_url": notifySrv.URL,
	})
	saveN.Body.Close()
	if saveN.StatusCode != http.StatusOK {
		t.Fatalf("save notification config failed: %d", saveN.StatusCode)
	}
	sendN := doJSON(t, http.MethodPost, ts.URL+"/api/v1/notifications/test", rootCookies, rootCSRF, "", map[string]any{
		"channel": "webhook", "event": "deployment.succeeded", "summary": "web-api deployed",
	})
	sendN.Body.Close()
	if sendN.StatusCode != http.StatusOK || !strings.Contains(string(receivedNotify), "web-api deployed") {
		t.Fatalf("real HTTP notification delivery failed: status=%d received=%q", sendN.StatusCode, string(receivedNotify))
	}
	failNotify = true
	failN := doJSON(t, http.MethodPost, ts.URL+"/api/v1/notifications/test", rootCookies, rootCSRF, "", map[string]any{
		"channel": "webhook", "event": "deployment.failed", "summary": "should fail",
	})
	failN.Body.Close()
	if failN.StatusCode != http.StatusBadGateway {
		t.Fatalf("expected 502 when remote webhook endpoint fails, got %d", failN.StatusCode)
	}

	// 6. REAL PostgreSQL Backup -> SigV4 S3 Upload -> Retention Prune -> Wipe DB -> Restore -> Verify Restored Data!
	// Create an isolated source table inside a separate database on our disposable test Postgres container!
	_, _ = pool.Exec(ctx, `DROP DATABASE IF EXISTS coolify_backup_e2e`)
	_, err = pool.Exec(ctx, `CREATE DATABASE coolify_backup_e2e`)
	if err != nil {
		t.Fatalf("create backup test db: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DROP DATABASE IF EXISTS coolify_backup_e2e`) })

	bkPool, err := postgres.Connect(ctx, "postgres://coolify:coolify_test_pw@127.0.0.1:15432/coolify_backup_e2e?sslmode=disable")
	if err != nil {
		t.Fatalf("connect backup test db: %v", err)
	}
	defer bkPool.Close()
	if _, err := bkPool.Exec(ctx, `CREATE TABLE widgets (id INT PRIMARY KEY, label TEXT NOT NULL); INSERT INTO widgets VALUES (1, '  exact-whitespace-widget  '), (2, 'alpha');`); err != nil {
		t.Fatal(err)
	}

	// Real S3-compatible HTTP object store mock checking SigV4 Authorization header + storing/serving/deleting real bytes
	var s3Mu sync.Mutex
	s3Objects := make(map[string][]byte)
	s3Srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		s3Mu.Lock()
		defer s3Mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			s3Objects[r.URL.Path] = b
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			b, ok := s3Objects[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(b)
		case http.MethodDelete:
			delete(s3Objects, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer s3Srv.Close()

	s3Cfg := &backups.S3Target{
		Endpoint: s3Srv.URL, Bucket: "coolify-bk", Region: "us-east-1", AccessKey: "AKIA_TEST", SecretKey: "SECRET_TEST",
	}
	dumpTarget := backups.DumpTarget{
		DatabaseType: "postgresql", DockerContainer: "coolify-mig-test-pg",
		DBUser: "coolify", DBPassword: "coolify_test_pw", DBName: "coolify_backup_e2e",
	}

	// Trigger REAL backup #1 on coolify-db sentinel (id=0) targeting coolify_backup_e2e
	bkResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/databases/coolify-db/backups", rootCookies, rootCSRF, "", map[string]any{
		"retention_count": 2, "target": dumpTarget, "s3": s3Cfg,
	})
	var bkOut struct {
		ExecutionUUID string `json:"execution_uuid"`
		IsInstanceDB  bool   `json:"is_instance_db"`
		ChecksumSHA   string `json:"checksum_sha256"`
		Size          int64  `json:"size"`
	}
	_ = json.NewDecoder(bkResp.Body).Decode(&bkOut)
	bkResp.Body.Close()
	if bkResp.StatusCode != http.StatusCreated || !bkOut.IsInstanceDB || bkOut.Size <= 0 || len(bkOut.ChecksumSHA) != 64 {
		t.Fatalf("real pg_dump backup failed: status=%d out=%+v", bkResp.StatusCode, bkOut)
	}
	s3Mu.Lock()
	s3Count := len(s3Objects)
	s3Mu.Unlock()
	if s3Count != 1 {
		t.Fatalf("expected 1 uploaded S3 object, got %d", s3Count)
	}

	// Wipe the widgets table completely, then restore via API, and query PostgreSQL to PROVE real restore!
	if _, err := bkPool.Exec(ctx, `DROP TABLE widgets;`); err != nil {
		t.Fatal(err)
	}
	restResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/databases/coolify-db/backups/"+bkOut.ExecutionUUID+"/restore", rootCookies, rootCSRF, "", map[string]any{
		"target": dumpTarget, "s3": s3Cfg,
	})
	restBody, _ := io.ReadAll(restResp.Body)
	restResp.Body.Close()
	if restResp.StatusCode != http.StatusOK {
		t.Fatalf("real restore failed: %d %s", restResp.StatusCode, string(restBody))
	}
	var restoredLabel string
	if err := bkPool.QueryRow(ctx, `SELECT label FROM widgets WHERE id = 1`).Scan(&restoredLabel); err != nil || restoredLabel != "  exact-whitespace-widget  " {
		t.Fatalf("restored row mismatch: got %q err=%v", restoredLabel, err)
	}

	// 7. REAL Server Transfer Export -> Import into Team 10 -> Verify encrypted env whitespace preserved & archive read-only!
	_, _ = pool.Exec(ctx, `INSERT INTO teams (id, name) VALUES (10, 'Target Team')`)
	_, _ = pool.Exec(ctx, `INSERT INTO team_user (team_id, user_id, role) VALUES (10, 0, 'owner')`)

	expResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/localhost/transfer", rootCookies, rootCSRF, "", map[string]any{
		"mark_transferred_away": true,
	})
	var exportedBundle map[string]any
	_ = json.NewDecoder(expResp.Body).Decode(&exportedBundle)
	expResp.Body.Close()
	if expResp.StatusCode != http.StatusOK {
		t.Fatalf("transfer export failed: %d", expResp.StatusCode)
	}
	// Archived transferred-away server MUST reject updates!
	updArchived := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/servers/localhost", rootCookies, rootCSRF, "", map[string]any{"name": "mutated"})
	updArchived.Body.Close()
	if updArchived.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 updating transferred-away archived server, got %d", updArchived.StatusCode)
	}
	impResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/import", rootCookies, rootCSRF, "", map[string]any{
		"target_team_id": 10, "bundle": exportedBundle,
	})
	var impOut struct {
		NewServerUUID       string `json:"NewServerUUID"`
		DecryptedRoundTrips int    `json:"DecryptedRoundTrips"`
	}
	_ = json.NewDecoder(impResp.Body).Decode(&impOut)
	impResp.Body.Close()
	if impResp.StatusCode != http.StatusCreated || impOut.NewServerUUID == "" || impOut.DecryptedRoundTrips < 2 {
		t.Fatalf("transfer import failed: status=%d out=%+v", impResp.StatusCode, impOut)
	}
	// Verify imported SPACED_SECRET decrypted byte-for-byte in Target Team 10!
	var encImported string
	if err := pool.QueryRow(ctx, `
		SELECT ev.value FROM environment_variables ev
		JOIN applications a ON a.id = ev.resourceable_id
		JOIN environments e ON e.id = a.environment_id
		JOIN projects p ON p.id = e.project_id
		WHERE p.team_id = 10 AND ev.key = 'SPACED_SECRET'
	`).Scan(&encImported); err != nil {
		t.Fatalf("expected imported SPACED_SECRET in team 10: %v", err)
	}
	plainImported, err := app.Cipher.DecryptLaravelPayload(encImported, false)
	if err != nil || string(plainImported) != "  keep-spaces  " {
		t.Fatalf("imported secret mismatch: got %q err=%v", string(plainImported), err)
	}
}

func TestProductionV1Parity_ResourcesLifecycleKeysS3TagsAndAuthz(t *testing.T) {
	app, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()
	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	// 1. Current team + PATCH team + members + audit-events
	teamResp := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/team", rootCookies, rootCSRF, "", map[string]any{
		"name": "Root Platform Team", "description": "Primary ops",
	})
	teamBody, _ := io.ReadAll(teamResp.Body)
	teamResp.Body.Close()
	if teamResp.StatusCode != http.StatusOK || !strings.Contains(string(teamBody), "Root Platform Team") {
		t.Fatalf("PATCH /api/v1/team failed: %d %s", teamResp.StatusCode, string(teamBody))
	}
	memResp := doJSON(t, http.MethodGet, ts.URL+"/api/v1/teams/current/members", rootCookies, "", "", nil)
	memResp.Body.Close()
	if memResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/teams/current/members failed: %d", memResp.StatusCode)
	}
	auditResp := doJSON(t, http.MethodGet, ts.URL+"/api/v1/audit-events", rootCookies, "", "", nil)
	auditResp.Body.Close()
	if auditResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/audit-events failed: %d", auditResp.StatusCode)
	}

	// 2. Private key sentinel protection (cannot delete pk-0) + create/patch/delete non-sentinel key
	delPK0 := doJSON(t, http.MethodDelete, ts.URL+"/api/v1/security/keys/pk-0", rootCookies, rootCSRF, "", nil)
	delPK0.Body.Close()
	if delPK0.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 deleting sentinel pk-0, got %d", delPK0.StatusCode)
	}
	mkPK := doJSON(t, http.MethodPost, ts.URL+"/api/v1/security/keys", rootCookies, rootCSRF, "", map[string]any{
		"name": "deploy-key", "private_key": "-----BEGIN OPENSSH PRIVATE KEY-----\ntest\n-----END OPENSSH PRIVATE KEY-----",
	})
	var pkOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(mkPK.Body).Decode(&pkOut)
	mkPK.Body.Close()
	if mkPK.StatusCode != http.StatusCreated || pkOut.UUID == "" {
		t.Fatalf("create key failed: %d", mkPK.StatusCode)
	}
	getPK := doJSON(t, http.MethodGet, ts.URL+"/api/v1/security/keys/"+pkOut.UUID, rootCookies, "", "", nil)
	getPKBody, _ := io.ReadAll(getPK.Body)
	getPK.Body.Close()
	if getPK.StatusCode != http.StatusOK || strings.Contains(string(getPKBody), "BEGIN OPENSSH PRIVATE KEY") {
		t.Fatalf("GET private key failed or leaked key material: %d %s", getPK.StatusCode, string(getPKBody))
	}

	// 3. S3 Storage encrypted credentials + Tags CRUD
	mkS3 := doJSON(t, http.MethodPost, ts.URL+"/api/v1/s3-storages", rootCookies, rootCSRF, "", map[string]any{
		"name": "minio-bk", "bucket": "coolify", "endpoint": "http://127.0.0.1:9000", "key": "AKIA_RAW", "secret": "SUPER_S3_SECRET",
	})
	var s3Out struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(mkS3.Body).Decode(&s3Out)
	mkS3.Body.Close()
	if mkS3.StatusCode != http.StatusCreated || s3Out.UUID == "" {
		t.Fatalf("create S3 storage failed: %d", mkS3.StatusCode)
	}
	// Verify S3 secret was stored encrypted in PostgreSQL (not plaintext)
	var storedS3Secret string
	_ = pool.QueryRow(ctx, `SELECT secret FROM s3_storages WHERE uuid = $1`, s3Out.UUID).Scan(&storedS3Secret)
	if storedS3Secret == "SUPER_S3_SECRET" {
		t.Fatal("CRITICAL: S3 secret stored in plaintext!")
	}
	decS3, err := app.Cipher.DecryptLaravelPayload(storedS3Secret, false)
	if err != nil || string(decS3) != "SUPER_S3_SECRET" {
		t.Fatalf("S3 secret decryption mismatch: %q err=%v", string(decS3), err)
	}

	mkTag := doJSON(t, http.MethodPost, ts.URL+"/api/v1/tags", rootCookies, rootCSRF, "", map[string]any{"name": "production"})
	mkTag.Body.Close()
	if mkTag.StatusCode != http.StatusCreated {
		t.Fatalf("create tag failed: %d", mkTag.StatusCode)
	}

	// 4. Project PATCH + Environment GET + Typed Application /dockerimage + Typed Database /redis + Unified /resources + Cancel Queued Deploy + Stop Job
	pResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", rootCookies, rootCSRF, "", map[string]any{"name": "Alpha"})
	var pOut struct {
		Project    struct{ UUID string `json:"uuid"` } `json:"project"`
		DefaultEnv struct{ UUID string `json:"uuid"` } `json:"default_environment"`
	}
	_ = json.NewDecoder(pResp.Body).Decode(&pOut)
	pResp.Body.Close()

	patchProj := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/projects/"+pOut.Project.UUID, rootCookies, rootCSRF, "", map[string]any{"name": "Alpha Renamed"})
	patchProj.Body.Close()
	if patchProj.StatusCode != http.StatusOK {
		t.Fatalf("PATCH project failed: %d", patchProj.StatusCode)
	}

	appResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/dockerimage", rootCookies, rootCSRF, "", map[string]any{
		"name": "typed-nginx", "environment_uuid": pOut.DefaultEnv.UUID, "server_uuid": "localhost",
		"docker_registry_image_name": "nginx", "docker_registry_image_tag": "alpine", "fqdn": "https://typed.example.com",
	})
	var appOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(appResp.Body).Decode(&appOut)
	appResp.Body.Close()
	if appResp.StatusCode != http.StatusCreated || appOut.UUID == "" {
		t.Fatalf("POST /api/v1/applications/dockerimage failed: %d", appResp.StatusCode)
	}

	dbResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/databases/redis", rootCookies, rootCSRF, "", map[string]any{
		"name": "cache-redis", "password": "redis-pass", "server_uuid": "localhost",
	})
	var dbOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(dbResp.Body).Decode(&dbOut)
	dbResp.Body.Close()
	if dbResp.StatusCode != http.StatusCreated || dbOut.UUID == "" {
		t.Fatalf("POST /api/v1/databases/redis failed: %d", dbResp.StatusCode)
	}

	resResp := doJSON(t, http.MethodGet, ts.URL+"/api/v1/resources", rootCookies, "", "", nil)
	resBody, _ := io.ReadAll(resResp.Body)
	resResp.Body.Close()
	if resResp.StatusCode != http.StatusOK || !strings.Contains(string(resBody), "typed-nginx") || !strings.Contains(string(resBody), "cache-redis") {
		t.Fatalf("GET /api/v1/resources missing created resources: %d %s", resResp.StatusCode, string(resBody))
	}

	// Start app (enqueues deployment) -> cancel queued deployment -> stop app (enqueues app_stop) -> worker runs stop -> status='exited'
	startResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/start", rootCookies, rootCSRF, "", nil)
	var depOut struct{ DeploymentUUID string `json:"deployment_uuid"` }
	_ = json.NewDecoder(startResp.Body).Decode(&depOut)
	startResp.Body.Close()
	if startResp.StatusCode != http.StatusAccepted || depOut.DeploymentUUID == "" {
		t.Fatalf("POST start failed: %d", startResp.StatusCode)
	}
	cancelResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/deployments/"+depOut.DeploymentUUID+"/cancel", rootCookies, rootCSRF, "", nil)
	cancelBody, _ := io.ReadAll(cancelResp.Body)
	cancelResp.Body.Close()
	if cancelResp.StatusCode != http.StatusOK || !strings.Contains(string(cancelBody), `"cancelled"`) {
		t.Fatalf("POST cancel queued deploy failed: %d %s", cancelResp.StatusCode, string(cancelBody))
	}

	stopResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/stop", rootCookies, rootCSRF, "", nil)
	stopResp.Body.Close()
	if stopResp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST stop failed: %d", stopResp.StatusCode)
	}
	w := worker.NewRunner(worker.Config{
		WorkerID: "parity-worker", Pool: pool, JobCoord: app.JobCoord,
		ScheduleCoord: schedules.NewPostgresScheduleCoordinator(pool),
		Outbox: app.Outbox, Dispatcher: app.Dispatcher, LogStore: app.LogStore,
		Executor: executor.NewClient(executor.NewLocalCommandTransport()),
		CommandOverride: func(_ jobs.DurableJobSpec) (string, string) { return "local_exec", "echo STOPPED_OK" },
	})
	if worked, err := w.ProcessOneJob(ctx); err != nil || !worked {
		t.Fatalf("worker failed processing app_stop: worked=%v err=%v", worked, err)
	}
	var appStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM applications WHERE uuid = $1`, appOut.UUID).Scan(&appStatus)
	if appStatus != "exited" {
		t.Fatalf("expected application status='exited' after real worker stop job, got %q", appStatus)
	}

	// 5. Server sub-routes (/domains, /destinations, /resources, /validate) + Per-channel notification PATCH/GET + MCP toggle authz
	for _, sub := range []string{"/resources", "/domains", "/destinations"} {
		rSub := doJSON(t, http.MethodGet, ts.URL+"/api/v1/servers/localhost"+sub, rootCookies, "", "", nil)
		rSub.Body.Close()
		if rSub.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/v1/servers/localhost%s failed: %d", sub, rSub.StatusCode)
		}
	}
	notifPatch := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/notifications/slack", rootCookies, rootCSRF, "", map[string]any{
		"enabled": true, "webhook_url": "https://hooks.slack.example/secret",
	})
	notifPatch.Body.Close()
	if notifPatch.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /api/v1/notifications/slack failed: %d", notifPatch.StatusCode)
	}
	notifGet := doJSON(t, http.MethodGet, ts.URL+"/api/v1/notifications/slack", rootCookies, "", "", nil)
	notifBody, _ := io.ReadAll(notifGet.Body)
	notifGet.Body.Close()
	if notifGet.StatusCode != http.StatusOK || !strings.Contains(string(notifBody), "hooks.slack.example") {
		t.Fatalf("authorized notification editor could not reload its configuration: %d", notifGet.StatusCode)
	}

	// Non-root team owner MUST receive 403 on /api/v1/mcp/enable & /api/v1/disable!
	regResp := doJSON(t, http.MethodPost, ts.URL+"/app/auth/register", nil, "", "", map[string]string{
		"name": "Alice", "email": "alice-parity@coolify.test", "password": "AlicePassword123!", "team_name": "Alice Team",
	})
	var alice struct{ CSRFToken string `json:"csrf_token"` }
	_ = json.NewDecoder(regResp.Body).Decode(&alice)
	aliceCookies := regResp.Cookies()
	regResp.Body.Close()
	for _, path := range []string{"/api/v1/mcp/enable", "/api/v1/disable"} {
		forb := doJSON(t, http.MethodPost, ts.URL+path, aliceCookies, alice.CSRFToken, "", nil)
		forb.Body.Close()
		if forb.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 for non-root owner on %s, got %d", path, forb.StatusCode)
		}
	}
	// Alice also MUST NOT access Root Team's S3 storage or private key!
	crossS3 := doJSON(t, http.MethodGet, ts.URL+"/api/v1/s3-storages/"+s3Out.UUID, aliceCookies, "", "", nil)
	crossS3.Body.Close()
	if crossS3.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 cross-team S3 read, got %d", crossS3.StatusCode)
	}
	crossPK := doJSON(t, http.MethodGet, ts.URL+"/api/v1/security/keys/"+pkOut.UUID, aliceCookies, "", "", nil)
	crossPK.Body.Close()
	if crossPK.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 cross-team private key read, got %d", crossPK.StatusCode)
	}
}

func TestProductionSlice9_DestinationsPreviewsCloneAndScopedSharedEnvs(t *testing.T) {
	app, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()
	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	// 1. Destinations CRUD + sentinel id=0 protection
	delDest0 := doJSON(t, http.MethodDelete, ts.URL+"/api/v1/destinations/standalone-docker-0", rootCookies, rootCSRF, "", nil)
	delDest0.Body.Close()
	if delDest0.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422 deleting sentinel standalone-docker-0, got %d", delDest0.StatusCode)
	}
	mkDest := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/localhost/destinations", rootCookies, rootCSRF, "", map[string]any{
		"name": "staging-net", "network": "coolify-staging",
	})
	var destOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(mkDest.Body).Decode(&destOut)
	mkDest.Body.Close()
	if mkDest.StatusCode != http.StatusCreated || destOut.UUID == "" {
		t.Fatalf("POST server destination failed: %d", mkDest.StatusCode)
	}
	patchDest := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/destinations/"+destOut.UUID, rootCookies, rootCSRF, "", map[string]any{
		"network": "coolify-staging-v2",
	})
	patchDest.Body.Close()
	if patchDest.StatusCode != http.StatusOK {
		t.Fatalf("PATCH destination failed: %d", patchDest.StatusCode)
	}
	listDest := doJSON(t, http.MethodGet, ts.URL+"/api/v1/destinations", rootCookies, "", "", nil)
	listDestBody, _ := io.ReadAll(listDest.Body)
	listDest.Body.Close()
	if listDest.StatusCode != http.StatusOK || !strings.Contains(string(listDestBody), "coolify-staging-v2") {
		t.Fatalf("GET /api/v1/destinations missing updated destination: %d %s", listDest.StatusCode, string(listDestBody))
	}

	// 2. Project + App + Bulk Envs + Clone preserving exact encrypted whitespace
	pResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", rootCookies, rootCSRF, "", map[string]any{"name": "Slice9 Proj"})
	var pOut struct {
		Project    struct{ UUID string `json:"uuid"` } `json:"project"`
		DefaultEnv struct{ UUID string `json:"uuid"` } `json:"default_environment"`
	}
	_ = json.NewDecoder(pResp.Body).Decode(&pOut)
	pResp.Body.Close()

	appResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/dockerimage", rootCookies, rootCSRF, "", map[string]any{
		"name": "origin-app", "environment_uuid": pOut.DefaultEnv.UUID, "server_uuid": "localhost",
		"docker_registry_image_name": "nginx", "docker_registry_image_tag": "alpine",
	})
	var appOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(appResp.Body).Decode(&appOut)
	appResp.Body.Close()

	bulkResp := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/applications/"+appOut.UUID+"/envs/bulk", rootCookies, rootCSRF, "", map[string]any{
		"data": []map[string]any{
			{"key": "BULK_WS", "value": "  exact-bulk  ", "is_runtime": true},
			{"key": "BULK_LOCKED", "value": "top-secret", "is_runtime": true, "is_shown_once": true},
		},
	})
	bulkResp.Body.Close()
	if bulkResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH bulk envs failed: %d", bulkResp.StatusCode)
	}

	cloneResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/clone", rootCookies, rootCSRF, "", map[string]any{
		"name": "cloned-app",
	})
	var cloneOut struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.NewDecoder(cloneResp.Body).Decode(&cloneOut)
	cloneResp.Body.Close()
	if cloneResp.StatusCode != http.StatusCreated || cloneOut.UUID == "" || cloneOut.UUID == appOut.UUID {
		t.Fatalf("POST clone failed: %d %+v", cloneResp.StatusCode, cloneOut)
	}
	var encCloned string
	if err := pool.QueryRow(ctx, `SELECT value FROM environment_variables WHERE resourceable_id = $1 AND key = 'BULK_WS'`, cloneOut.ID).Scan(&encCloned); err != nil {
		t.Fatalf("cloned env missing: %v", err)
	}
	decCloned, err := app.Cipher.DecryptLaravelPayload(encCloned, false)
	if err != nil || string(decCloned) != "  exact-bulk  " {
		t.Fatalf("cloned env whitespace mismatch: %q err=%v", string(decCloned), err)
	}

	// 3. Application Previews CRUD
	mkPrev := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/previews", rootCookies, rootCSRF, "", map[string]any{
		"pull_request_id": 42, "pull_request_html_url": "https://github.com/org/repo/pull/42", "fqdn": "https://pr-42.example.com",
	})
	mkPrev.Body.Close()
	if mkPrev.StatusCode != http.StatusCreated {
		t.Fatalf("POST preview failed: %d", mkPrev.StatusCode)
	}
	patchPrev := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/applications/"+appOut.UUID+"/previews/42", rootCookies, rootCSRF, "", map[string]any{
		"fqdn": "https://pr-42-v2.example.com",
	})
	patchPrev.Body.Close()
	if patchPrev.StatusCode != http.StatusOK {
		t.Fatalf("PATCH preview failed: %d", patchPrev.StatusCode)
	}
	getPrev := doJSON(t, http.MethodGet, ts.URL+"/api/v1/applications/"+appOut.UUID+"/previews/42", rootCookies, "", "", nil)
	getPrevBody, _ := io.ReadAll(getPrev.Body)
	getPrev.Body.Close()
	if getPrev.StatusCode != http.StatusOK || !strings.Contains(string(getPrevBody), "pr-42-v2.example.com") {
		t.Fatalf("GET preview failed: %d %s", getPrev.StatusCode, string(getPrevBody))
	}
	delPrev := doJSON(t, http.MethodDelete, ts.URL+"/api/v1/applications/"+appOut.UUID+"/previews/42", rootCookies, rootCSRF, "", nil)
	delPrev.Body.Close()
	if delPrev.StatusCode != http.StatusOK {
		t.Fatalf("DELETE preview failed: %d", delPrev.StatusCode)
	}

	// 4. Scoped Shared Envs (team, project, server) + is_shown_once redaction
	for _, ep := range []string{"/api/v1/team/envs", "/api/v1/projects/" + pOut.Project.UUID + "/envs", "/api/v1/servers/localhost/envs"} {
		postE := doJSON(t, http.MethodPost, ts.URL+ep, rootCookies, rootCSRF, "", map[string]any{
			"key": "SHARED_ONCE", "value": "never-reveal", "is_shown_once": true,
		})
		postE.Body.Close()
		if postE.StatusCode != http.StatusCreated {
			t.Fatalf("POST %s failed: %d", ep, postE.StatusCode)
		}
		getE := doJSON(t, http.MethodGet, ts.URL+ep, rootCookies, "", "", nil)
		bodyE, _ := io.ReadAll(getE.Body)
		getE.Body.Close()
		if getE.StatusCode != http.StatusOK || strings.Contains(string(bodyE), "never-reveal") {
			t.Fatalf("GET %s failed or leaked is_shown_once value: %d %s", ep, getE.StatusCode, string(bodyE))
		}
	}

	// 5. Cross-team isolation (other user MUST receive 403 on destination/preview/clone!)
	regResp := doJSON(t, http.MethodPost, ts.URL+"/app/auth/register", nil, "", "", map[string]string{
		"name": "Eve", "email": "eve-s9@coolify.test", "password": "EvePassword123!", "team_name": "Eve Team",
	})
	var eve struct{ CSRFToken string `json:"csrf_token"` }
	_ = json.NewDecoder(regResp.Body).Decode(&eve)
	eveCookies := regResp.Cookies()
	regResp.Body.Close()
	for _, tc := range []struct{ m, p string }{
		{http.MethodGet, "/api/v1/destinations/" + destOut.UUID},
		{http.MethodPost, "/api/v1/applications/" + appOut.UUID + "/clone"},
		{http.MethodGet, "/api/v1/projects/" + pOut.Project.UUID + "/envs"},
	} {
		resp := doJSON(t, tc.m, ts.URL+tc.p, eveCookies, eve.CSRFToken, "", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("expected 403 cross-team on %s %s, got %d", tc.m, tc.p, resp.StatusCode)
		}
	}
}

func TestProductionSlice10_StoragesPathConfinementAndEncryption(t *testing.T) {
	app, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()
	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	pResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", rootCookies, rootCSRF, "", map[string]any{"name": "Storage Proj"})
	var pOut struct {
		DefaultEnv struct{ UUID string `json:"uuid"` } `json:"default_environment"`
	}
	_ = json.NewDecoder(pResp.Body).Decode(&pOut)
	pResp.Body.Close()

	appResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/dockerimage", rootCookies, rootCSRF, "", map[string]any{
		"name": "vol-app", "environment_uuid": pOut.DefaultEnv.UUID, "server_uuid": "localhost",
		"docker_registry_image_name": "nginx", "docker_registry_image_tag": "alpine",
	})
	var appOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(appResp.Body).Decode(&appOut)
	appResp.Body.Close()

	// 1. Unsafe paths (traversal, shell metacharacters, ~, root /) MUST be rejected 422!
	for _, bad := range []map[string]any{
		{"type": "file", "fs_path": "/etc/../shadow", "mount_path": "/app/x", "content": "x"},
		{"type": "file", "fs_path": "~/secret", "mount_path": "/app/x", "content": "x"},
		{"type": "file", "fs_path": "/data/$(id)", "mount_path": "/app/x", "content": "x"},
		{"type": "persistent", "name": "rootvol", "mount_path": "/"},
	} {
		r := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/storages", rootCookies, rootCSRF, "", bad)
		r.Body.Close()
		if r.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422 for unsafe storage payload %+v, got %d", bad, r.StatusCode)
		}
	}

	// 2. Valid persistent volume + valid encrypted file storage
	pvResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/storages", rootCookies, rootCSRF, "", map[string]any{
		"type": "persistent", "name": "app-data", "mount_path": "/var/lib/data",
	})
	var pvOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(pvResp.Body).Decode(&pvOut)
	pvResp.Body.Close()
	if pvResp.StatusCode != http.StatusCreated || pvOut.UUID == "" {
		t.Fatalf("create persistent storage failed: %d", pvResp.StatusCode)
	}

	fvResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/storages", rootCookies, rootCSRF, "", map[string]any{
		"type": "file", "fs_path": "/data/coolify/app.conf", "mount_path": "/etc/app.conf", "content": "secret_token = abc123\n",
	})
	var fvOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(fvResp.Body).Decode(&fvOut)
	fvResp.Body.Close()
	if fvResp.StatusCode != http.StatusCreated || fvOut.UUID == "" {
		t.Fatalf("create file storage failed: %d", fvResp.StatusCode)
	}

	// Verify content is encrypted in PostgreSQL (never plaintext!)
	var storedContent string
	if err := pool.QueryRow(ctx, `SELECT content FROM local_file_volumes WHERE uuid = $1`, fvOut.UUID).Scan(&storedContent); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(storedContent, "abc123") {
		t.Fatal("CRITICAL: local_file_volumes.content stored in plaintext!")
	}
	plain, err := app.Cipher.DecryptLaravelPayload(storedContent, false)
	if err != nil || string(plain) != "secret_token = abc123\n" {
		t.Fatalf("file volume decrypt mismatch: %q err=%v", string(plain), err)
	}

	// Verify list + delete
	listResp := doJSON(t, http.MethodGet, ts.URL+"/api/v1/applications/"+appOut.UUID+"/storages", rootCookies, "", "", nil)
	listBody, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK || !strings.Contains(string(listBody), "app-data") || !strings.Contains(string(listBody), "/etc/app.conf") {
		t.Fatalf("GET storages failed: %d %s", listResp.StatusCode, string(listBody))
	}

	delResp := doJSON(t, http.MethodDelete, ts.URL+"/api/v1/applications/"+appOut.UUID+"/storages/"+pvOut.UUID, rootCookies, rootCSRF, "", nil)
	delResp.Body.Close()
	if delResp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE storage failed: %d", delResp.StatusCode)
	}
}

func TestFindingB_ReadOnlyMemberCannotDeployPlaceResourceOrUseTeamCredentials(t *testing.T) {
	_, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()
	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	// Seed a non-localhost server on team 0, project/env/app on team 0, and S3 + private key
	srvResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers", rootCookies, rootCSRF, "", map[string]any{
		"name": "worker-node-b", "ip": "10.0.10.2", "port": 22, "user": "root", "private_key_uuid": "pk-0",
	})
	var srvOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(srvResp.Body).Decode(&srvOut)
	srvResp.Body.Close()

	pResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", rootCookies, rootCSRF, "", map[string]any{"name": "Team0AppProj"})
	var pOut struct {
		Project    struct{ UUID string `json:"uuid"` } `json:"project"`
		DefaultEnv struct{ UUID string `json:"uuid"` } `json:"default_environment"`
	}
	_ = json.NewDecoder(pResp.Body).Decode(&pOut)
	pResp.Body.Close()

	appResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/dockerimage", rootCookies, rootCSRF, "", map[string]any{
		"name": "team0-app", "environment_uuid": pOut.DefaultEnv.UUID, "server_uuid": srvOut.UUID,
		"docker_registry_image_name": "nginx", "docker_registry_image_tag": "alpine",
	})
	var appOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(appResp.Body).Decode(&appOut)
	appResp.Body.Close()

	// Register Marge (owner of personal team, added as member of Team 0 with access = NULL)
	regResp := doJSON(t, http.MethodPost, ts.URL+"/app/auth/register", nil, "", "", map[string]string{
		"name": "Marge", "email": "marge@coolify.test", "password": "MargePassword123!", "team_name": "Marge Team",
	})
	var marge struct {
		UserID        int64  `json:"user_id"`
		SessionTeamID int64  `json:"session_team_id"`
		CSRFToken     string `json:"csrf_token"`
	}
	_ = json.NewDecoder(regResp.Body).Decode(&marge)
	margeCookies := regResp.Cookies()
	regResp.Body.Close()
	if _, err := pool.Exec(ctx, `INSERT INTO team_user (team_id, user_id, role, access) VALUES (0, $1, 'member', NULL)`, marge.UserID); err != nil {
		t.Fatal(err)
	}

	// Marge has access = NULL in Team 0 (while owning her own personal team): MUST read app, MUST NOT deploy, place resources, use team credentials, or get terminal!
	readApp := doJSON(t, http.MethodGet, ts.URL+"/api/v1/applications/"+appOut.UUID, margeCookies, "", "", nil)
	readApp.Body.Close()
	if readApp.StatusCode != http.StatusOK {
		t.Fatalf("expected member with access=NULL to read application, got %d", readApp.StatusCode)
	}
	depResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/deploy?uuid="+appOut.UUID, margeCookies, marge.CSRFToken, "", nil)
	depResp.Body.Close()
	if depResp.StatusCode != http.StatusForbidden {
		t.Fatalf("FINDING B: read-only member with access=NULL deployed app (got %d, want 403)", depResp.StatusCode)
	}
	keysResp := doJSON(t, http.MethodGet, ts.URL+"/api/v1/security/keys", margeCookies, "", "", nil)
	keysBody, _ := io.ReadAll(keysResp.Body)
	keysResp.Body.Close()
	if keysResp.StatusCode != http.StatusOK || strings.Contains(string(keysBody), "pk-0") {
		t.Fatalf("FINDING B: personal team keys leaked Team 0 key: status=%d body=%s", keysResp.StatusCode, string(keysBody))
	}
	directKey0 := doJSON(t, http.MethodGet, ts.URL+"/api/v1/security/keys/pk-0", margeCookies, "", "", nil)
	directKey0.Body.Close()
	if directKey0.StatusCode != http.StatusForbidden {
		t.Fatalf("FINDING B: read-only member accessed Team 0 key pk-0 directly (got %d want 403)", directKey0.StatusCode)
	}
	// Switch Marge session explicitly to Team 0 then try UseTeamCredentials + PlaceResource + Terminal + Explicit read-only grant
	swResp := doJSON(t, http.MethodPost, ts.URL+"/app/auth/switch-team", margeCookies, marge.CSRFToken, "", map[string]any{"team_id": 0})
	swResp.Body.Close()
	if swResp.StatusCode != http.StatusOK {
		t.Fatalf("switch team failed: %d", swResp.StatusCode)
	}
	keysTeam0 := doJSON(t, http.MethodGet, ts.URL+"/api/v1/security/keys", margeCookies, "", "", nil)
	keysTeam0.Body.Close()
	if keysTeam0.StatusCode != http.StatusForbidden {
		t.Fatalf("FINDING B: read-only member with access=NULL accessed team credentials, got %d want 403", keysTeam0.StatusCode)
	}
	placeResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/dockerimage", margeCookies, marge.CSRFToken, "", map[string]any{
		"name": "marge-app", "environment_uuid": pOut.DefaultEnv.UUID, "server_uuid": srvOut.UUID,
		"docker_registry_image_name": "nginx", "docker_registry_image_tag": "alpine",
	})
	placeResp.Body.Close()
	if placeResp.StatusCode != http.StatusForbidden {
		t.Fatalf("FINDING B: read-only member created/placed app on server, got %d want 403", placeResp.StatusCode)
	}
	termResp := doJSON(t, http.MethodPost, ts.URL+"/app/terminal/ticket", margeCookies, marge.CSRFToken, "", map[string]string{
		"server_uuid": srvOut.UUID,
	})
	termResp.Body.Close()
	if termResp.StatusCode != http.StatusForbidden {
		t.Fatalf("FINDING B: read-only member obtained terminal ticket, got %d want 403", termResp.StatusCode)
	}

	// Explicit read-only JSON grant on team 0 MUST also deny deploy!
	explicitRead := `[{"scope":"team","uuid":null,"levels":["read"]}]`
	if _, err := pool.Exec(ctx, `UPDATE team_user SET access = $1::jsonb WHERE team_id = 0 AND user_id = $2`, explicitRead, marge.UserID); err != nil {
		t.Fatal(err)
	}
	depExplicit := doJSON(t, http.MethodPost, ts.URL+"/api/v1/deploy?uuid="+appOut.UUID, margeCookies, marge.CSRFToken, "", nil)
	depExplicit.Body.Close()
	if depExplicit.StatusCode != http.StatusForbidden {
		t.Fatalf("FINDING B: explicit team read-only member deployed app (got %d, want 403)", depExplicit.StatusCode)
	}
}

func TestFindingC_ScopedTokenIntersectionEnforcedAcrossReadWriteListSearchMCP(t *testing.T) {
	_, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()
	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	// Enable MCP + REST API
	setResp := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/settings", rootCookies, rootCSRF, "", map[string]any{
		"is_api_enabled": true, "is_mcp_server_enabled": true,
	})
	setResp.Body.Close()

	// Create Project A (with App A) and Project B (with App B)
	mkProj := func(name string) (projUUID, envUUID, appUUID string) {
		pr := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", rootCookies, rootCSRF, "", map[string]any{"name": name})
		var pOut struct {
			Project    struct{ UUID string `json:"uuid"` } `json:"project"`
			DefaultEnv struct{ UUID string `json:"uuid"` } `json:"default_environment"`
		}
		_ = json.NewDecoder(pr.Body).Decode(&pOut)
		pr.Body.Close()
		ar := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/dockerimage", rootCookies, rootCSRF, "", map[string]any{
			"name": name + "-app", "environment_uuid": pOut.DefaultEnv.UUID, "server_uuid": "localhost",
			"docker_registry_image_name": "nginx", "docker_registry_image_tag": "alpine",
		})
		var aOut struct{ UUID string `json:"uuid"` }
		_ = json.NewDecoder(ar.Body).Decode(&aOut)
		ar.Body.Close()
		return pOut.Project.UUID, pOut.DefaultEnv.UUID, aOut.UUID
	}
	projA, _, appA := mkProj("ScopedAlpha")
	projB, _, appB := mkProj("ScopedBeta")

	// Owner issues an API token with "root" abilities, BUT scoped ONLY to Project A!
	scopesJSON, _ := json.Marshal([]map[string]any{{"scope": "project", "uuid": projA, "levels": []string{"write"}}})
	tokResp := doJSON(t, http.MethodPost, ts.URL+"/app/auth/tokens", rootCookies, rootCSRF, "", map[string]any{
		"name": "proj-a-only-token", "abilities": []string{"read", "deploy", "write", "root"}, "scopes": json.RawMessage(scopesJSON),
	})
	var tokOut struct{ Token string `json:"token"` }
	_ = json.NewDecoder(tokResp.Body).Decode(&tokOut)
	tokResp.Body.Close()
	if tokResp.StatusCode != http.StatusCreated || tokOut.Token == "" {
		t.Fatalf("create scoped token failed: %d", tokResp.StatusCode)
	}
	// Verify DB really stored non-null scopes for Team 0
	var scopesCount int
	_ = pool.QueryRow(ctx, `SELECT jsonb_array_length(scopes) FROM personal_access_tokens WHERE name = 'proj-a-only-token'`).Scan(&scopesCount)
	if scopesCount != 1 {
		t.Fatalf("expected 1 stored scope, got %d", scopesCount)
	}

	// Token MUST be allowed to read and deploy App A (inside Project A)
	okRead := doJSON(t, http.MethodGet, ts.URL+"/api/v1/applications/"+appA, nil, "", tokOut.Token, nil)
	okRead.Body.Close()
	if okRead.StatusCode != http.StatusOK {
		t.Fatalf("expected scoped token to read App A, got %d", okRead.StatusCode)
	}
	okDeploy := doJSON(t, http.MethodPost, ts.URL+"/api/v1/deploy?uuid="+appA, nil, "", tokOut.Token, nil)
	okDeploy.Body.Close()
	if okDeploy.StatusCode != http.StatusAccepted {
		t.Fatalf("expected scoped token to deploy App A, got %d", okDeploy.StatusCode)
	}

	// Token MUST BE DENIED reading, deploying, streaming logs, or patching App B / Project B!
	for _, tc := range []struct{ m, p string }{
		{http.MethodGet, "/api/v1/applications/" + appB},
		{http.MethodPost, "/api/v1/deploy?uuid=" + appB},
		{http.MethodGet, "/api/v1/projects/" + projB},
		{http.MethodGet, "/realtime/logs?resource=" + appB + "&once=1"},
	} {
		r := doJSON(t, tc.m, ts.URL+tc.p, nil, "", tokOut.Token, nil)
		r.Body.Close()
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("FINDING C: scoped token bypassed scope boundary on %s %s (got %d want 403)", tc.m, tc.p, r.StatusCode)
		}
	}

	// List, Search, Resources, and MCP MUST NOT leak App B or Project B!
	listApps := doJSON(t, http.MethodGet, ts.URL+"/api/v1/applications", nil, "", tokOut.Token, nil)
	listBody, _ := io.ReadAll(listApps.Body)
	listApps.Body.Close()
	if listApps.StatusCode != http.StatusOK || !strings.Contains(string(listBody), appA) || strings.Contains(string(listBody), appB) {
		t.Fatalf("FINDING C: /api/v1/applications leaked out-of-scope appB: %s", string(listBody))
	}

	searchResp := doJSON(t, http.MethodGet, ts.URL+"/api/v1/search?q=Scoped", nil, "", tokOut.Token, nil)
	searchBody, _ := io.ReadAll(searchResp.Body)
	searchResp.Body.Close()
	if strings.Contains(string(searchBody), "ScopedBeta") {
		t.Fatalf("FINDING C: /api/v1/search leaked out-of-scope Project B: %s", string(searchBody))
	}

	mcpResp := doJSON(t, http.MethodPost, ts.URL+"/mcp", nil, "", tokOut.Token, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "list_applications",
	})
	mcpBody, _ := io.ReadAll(mcpResp.Body)
	mcpResp.Body.Close()
	if mcpResp.StatusCode != http.StatusOK || strings.Contains(string(mcpBody), appB) {
		t.Fatalf("FINDING C: /mcp list_applications leaked out-of-scope appB: %d %s", mcpResp.StatusCode, string(mcpBody))
	}
}

type captureTerminalTransport struct {
	mu       sync.Mutex
	captured []executor.ExecutionRequest
}

func (c *captureTerminalTransport) StreamNDJSON(_ context.Context, req executor.ExecutionRequest, emit func(executor.ExecutionEvent) error) (int, error) {
	c.mu.Lock()
	c.captured = append(c.captured, req)
	c.mu.Unlock()
	exitZero := 0
	_ = emit(executor.ExecutionEvent{ProtocolVersion: executor.ProtocolVersionV1, EventType: "started", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 1})
	_ = emit(executor.ExecutionEvent{ProtocolVersion: executor.ProtocolVersionV1, EventType: "output", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 2, Stream: "stdout", Data: "SSH_TERMINAL_OK host=" + req.Operation.Host})
	_ = emit(executor.ExecutionEvent{ProtocolVersion: executor.ProtocolVersionV1, EventType: "exited", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 3, ExitCode: &exitZero})
	return 0, nil
}

func TestFindingA_TerminalRemoteSSHBoundaryAndNonRootLocalhostBlock(t *testing.T) {
	app, ts, _, _, ciph := setupRealControlPlane(t)
	ctx := context.Background()
	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	// 1. Register Alice (owner of personal Team != 0) and confirm she CANNOT obtain a terminal ticket for localhost
	regResp := doJSON(t, http.MethodPost, ts.URL+"/app/auth/register", nil, "", "", map[string]string{
		"name": "Alice", "email": "alice-term@coolify.test", "password": "AlicePassword123!", "team_name": "Alice Team",
	})
	var alice struct {
		CSRFToken string `json:"csrf_token"`
	}
	_ = json.NewDecoder(regResp.Body).Decode(&alice)
	aliceCookies := regResp.Cookies()
	regResp.Body.Close()

	denyLocal := doJSON(t, http.MethodPost, ts.URL+"/app/terminal/ticket", aliceCookies, alice.CSRFToken, "", map[string]string{
		"server_uuid": "localhost",
	})
	denyLocal.Body.Close()
	if denyLocal.StatusCode != http.StatusForbidden {
		t.Fatalf("FINDING A: non-root admin accessed localhost terminal ticket (got %d want 403)", denyLocal.StatusCode)
	}

	// Verify LocalCommandTransport blocks local_exec when TeamID != 0
	_, localErr := executor.NewLocalCommandTransport().StreamNDJSON(ctx, executor.ExecutionRequest{
		ProtocolVersion:    executor.ProtocolVersionV1,
		FrameType:          executor.FrameStart,
		JobID:              "job-team-boundary",
		OperationID:        "op-team-boundary",
		AttemptID:          "att-0",
		TeamID:             99,
		DeadlineMS:         5000,
		MaxFrameBytes:      executor.DefaultMaxFrameBytes,
		ExpectedCapability: "local_exec",
		Operation:          executor.OperationSpec{Kind: "exec", Command: "echo should_fail"},
	}, func(executor.ExecutionEvent) error { return nil })
	if localErr == nil {
		t.Fatal("FINDING A: LocalCommandTransport permitted local_exec for non-root team 99")
	}

	// 2. Seed remote SSH key & remote server on Team 0 and verify WS terminal dispatches remote ssh_exec with real host/port/user/key
	remotePEM := "-----BEGIN OPENSSH PRIVATE KEY-----\nremote-terminal-secret-material\n-----END OPENSSH PRIVATE KEY-----"
	keyResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/security/keys", rootCookies, rootCSRF, "", map[string]any{
		"name": "remote-term-key", "description": "ssh key", "private_key": remotePEM,
	})
	var keyOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(keyResp.Body).Decode(&keyOut)
	keyResp.Body.Close()
	if keyResp.StatusCode != http.StatusCreated {
		t.Fatalf("create key failed: %d", keyResp.StatusCode)
	}
	// Sanity-check key is encrypted at rest in DB before continuing
	_ = ciph

	srvResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers", rootCookies, rootCSRF, "", map[string]any{
		"name": "remote-ssh-node", "ip": "10.0.99.5", "port": 2222, "user": "deploy", "private_key_uuid": keyOut.UUID,
	})
	var srvOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(srvResp.Body).Decode(&srvOut)
	srvResp.Body.Close()
	if srvResp.StatusCode != http.StatusCreated {
		t.Fatalf("create remote server failed: %d", srvResp.StatusCode)
	}

	capTrans := &captureTerminalTransport{}
	app.Executor = executor.NewClient(capTrans, remotePEM)

	tktResp := doJSON(t, http.MethodPost, ts.URL+"/app/terminal/ticket", rootCookies, rootCSRF, "", map[string]string{
		"server_uuid": srvOut.UUID,
		"container":   "app-web-1",
	})
	var tktOut struct{ Ticket string `json:"ticket"` }
	_ = json.NewDecoder(tktResp.Body).Decode(&tktOut)
	tktResp.Body.Close()
	if tktResp.StatusCode != http.StatusCreated || tktOut.Ticket == "" {
		t.Fatalf("remote terminal ticket failed: %d", tktResp.StatusCode)
	}

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/terminal/ws?ticket=" + tktOut.Ticket
	hdr := http.Header{}
	for _, c := range rootCookies {
		hdr.Add("Cookie", c.Name+"="+c.Value)
	}
	wsConn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		t.Fatalf("ws dial failed: %v", err)
	}
	defer wsConn.Close(websocket.StatusNormalClosure, "done")
	_, _, _ = wsConn.Read(ctx) // banner

	if err := wsConn.Write(ctx, websocket.MessageText, []byte(`{"type":"resize","cols":132,"rows":42}`)); err != nil {
		t.Fatal(err)
	}
	if err := wsConn.Write(ctx, websocket.MessageText, []byte(`{"type":"stdin","data":"id -u\n"}`)); err != nil {
		t.Fatal(err)
	}
	_, outMsg, err := wsConn.Read(ctx)
	if err != nil || !strings.Contains(string(outMsg), "SSH_TERMINAL_OK") {
		t.Fatalf("expected SSH_TERMINAL_OK output, got %q err=%v", string(outMsg), err)
	}

	capTrans.mu.Lock()
	defer capTrans.mu.Unlock()
	if len(capTrans.captured) != 1 {
		t.Fatalf("expected 1 executor call, got %d", len(capTrans.captured))
	}
	got := capTrans.captured[0]
	if got.ExpectedCapability != "ssh_exec" {
		t.Fatalf("FINDING A: remote server used capability %q instead of ssh_exec", got.ExpectedCapability)
	}
	if got.Operation.Host != "10.0.99.5" || got.Operation.Port != 2222 || got.Operation.User != "deploy" || !got.Operation.UseSudo {
		t.Fatalf("FINDING A: wrong SSH target metadata: %+v", got.Operation)
	}
	if got.Operation.SSHKeyContent != remotePEM {
		t.Fatalf("FINDING A: expected decrypted SSH private key on NDJSON request, got len=%d", len(got.Operation.SSHKeyContent))
	}
	if got.Operation.PTYCols != 132 || got.Operation.PTYRows != 42 || got.Operation.Kind != "pty" {
		t.Fatalf("FINDING A: expected PTY cols=132 rows=42 kind=pty, got cols=%d rows=%d kind=%s", got.Operation.PTYCols, got.Operation.PTYRows, got.Operation.Kind)
	}
	if !strings.Contains(got.Operation.Command, "docker exec") || !strings.Contains(got.Operation.Command, "app-web-1") {
		t.Fatalf("FINDING A: expected container exec wrapping for app-web-1, got %q", got.Operation.Command)
	}
}

func TestFindingD_LaravelFortify2FADecryptRecoveryReEncryptReplayAndForceReset(t *testing.T) {
	_, ts, pool, _, ciph := setupRealControlPlane(t)
	ctx := context.Background()

	// Seed Laravel Fortify user with APP_KEY-encrypted (PHP-serialized) secret & recovery codes + force_password_reset=true
	totpSecret := "JBSWY3DPEHPK3PXP"
	encSecret, err := ciph.EncryptLaravelPayload([]byte(totpSecret), true)
	if err != nil {
		t.Fatal(err)
	}
	recCodesJSON, _ := json.Marshal([]string{"REC-1111-AAAA", "REC-2222-BBBB"})
	encCodes, err := ciph.EncryptLaravelPayload(recCodesJSON, true)
	if err != nil {
		t.Fatal(err)
	}
	pwBytes, _ := bcrypt.GenerateFromPassword([]byte("TempResetPw123!"), bcrypt.MinCost)
	laravelHash := "$2y$" + string(pwBytes[4:])

	var uid int64
	err = pool.QueryRow(ctx, `
		INSERT INTO users (name, email, password, email_verified_at, force_password_reset, two_factor_secret, two_factor_recovery_codes, two_factor_confirmed_at)
		VALUES ('FortifyUser', 'fortify@coolify.test', $1, NOW(), TRUE, $2, $3, NOW())
		RETURNING id
	`, laravelHash, encSecret, encCodes).Scan(&uid)
	if err != nil {
		t.Fatal(err)
	}
	var tid int64
	if err := pool.QueryRow(ctx, `INSERT INTO teams (name, personal_team, show_boarding) VALUES ('Fortify Team', TRUE, FALSE) RETURNING id`).Scan(&tid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO team_user (team_id, user_id, role) VALUES ($1, $2, 'owner')`, tid, uid); err != nil {
		t.Fatal(err)
	}

	// 1. Login without TOTP fails
	bad1 := doJSON(t, http.MethodPost, ts.URL+"/app/auth/login", nil, "", "", map[string]string{
		"email": "fortify@coolify.test", "password": "TempResetPw123!",
	})
	bad1.Body.Close()
	if bad1.StatusCode == http.StatusOK {
		t.Fatal("expected 2FA login without TOTP code to be rejected")
	}

	// 2. Login with valid TOTP against Laravel-encrypted Fortify secret succeeds
	totpNow, err := auth.ComputeTOTP(totpSecret, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	okLogin := doJSON(t, http.MethodPost, ts.URL+"/app/auth/login", nil, "", "", map[string]string{
		"email": "fortify@coolify.test", "password": "TempResetPw123!", "code": totpNow,
	})
	var loginOut struct {
		CSRFToken          string `json:"csrf_token"`
		ForcePasswordReset bool   `json:"force_password_reset"`
	}
	_ = json.NewDecoder(okLogin.Body).Decode(&loginOut)
	userCookies := okLogin.Cookies()
	okLogin.Body.Close()
	if okLogin.StatusCode != http.StatusOK {
		t.Fatalf("FINDING D: login with valid TOTP against encrypted Fortify secret failed (%d)", okLogin.StatusCode)
	}
	if !loginOut.ForcePasswordReset {
		t.Fatalf("FINDING D: expected force_password_reset=true on login response")
	}

	// 3. Replaying the exact same TOTP step MUST fail!
	replayLogin := doJSON(t, http.MethodPost, ts.URL+"/app/auth/login", nil, "", "", map[string]string{
		"email": "fortify@coolify.test", "password": "TempResetPw123!", "code": totpNow,
	})
	replayLogin.Body.Close()
	if replayLogin.StatusCode == http.StatusOK {
		t.Fatalf("FINDING D: replayed TOTP code succeeded")
	}

	// 4. Forced password reset blocks normal app/API routes until reset, then allows them after reset!
	blockedProj := doJSON(t, http.MethodGet, ts.URL+"/api/v1/projects", userCookies, "", "", nil)
	blockedProj.Body.Close()
	if blockedProj.StatusCode != http.StatusForbidden {
		t.Fatalf("FINDING D: user with force_password_reset=true accessed /api/v1/projects (got %d want 403)", blockedProj.StatusCode)
	}

	resetResp := doJSON(t, http.MethodPost, ts.URL+"/app/auth/password/force-reset", userCookies, loginOut.CSRFToken, "", map[string]string{
		"current_password": "TempResetPw123!", "password": "NewStrongPassword99!", "password_confirmation": "NewStrongPassword99!",
	})
	resetResp.Body.Close()
	if resetResp.StatusCode != http.StatusOK {
		t.Fatalf("FINDING D: force password reset failed (%d)", resetResp.StatusCode)
	}

	allowedProj := doJSON(t, http.MethodGet, ts.URL+"/api/v1/projects", userCookies, "", "", nil)
	allowedProj.Body.Close()
	if allowedProj.StatusCode != http.StatusOK {
		t.Fatalf("expected /api/v1/projects allowed after force password reset, got %d", allowedProj.StatusCode)
	}

	// 5. Recovery code consumes once AND remaining recovery codes in DB stay APP_KEY-encrypted (NOT plaintext JSON)!
	recLogin := doJSON(t, http.MethodPost, ts.URL+"/app/auth/login", nil, "", "", map[string]string{
		"email": "fortify@coolify.test", "password": "NewStrongPassword99!", "code": "REC-1111-AAAA",
	})
	recLogin.Body.Close()
	if recLogin.StatusCode != http.StatusOK {
		t.Fatalf("FINDING D: recovery code login failed (%d)", recLogin.StatusCode)
	}
	var storedRecovery string
	if err := pool.QueryRow(ctx, `SELECT two_factor_recovery_codes FROM users WHERE id = $1`, uid).Scan(&storedRecovery); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(storedRecovery, "REC-2222-BBBB") {
		t.Fatalf("FINDING D: two_factor_recovery_codes was written as plaintext instead of APP_KEY encrypted: %s", storedRecovery)
	}
	decRemaining, decErr := ciph.DecryptLaravelPayload(storedRecovery, true)
	if decErr != nil {
		t.Fatalf("FINDING D: failed to decrypt updated two_factor_recovery_codes: %v", decErr)
	}
	if strings.Contains(string(decRemaining), "REC-1111-AAAA") || !strings.Contains(string(decRemaining), "REC-2222-BBBB") {
		t.Fatalf("FINDING D: expected consumed code removed and remaining code preserved, got %s", string(decRemaining))
	}

	// Reusing already-consumed recovery code MUST fail
	recReplay := doJSON(t, http.MethodPost, ts.URL+"/app/auth/login", nil, "", "", map[string]string{
		"email": "fortify@coolify.test", "password": "NewStrongPassword99!", "code": "REC-1111-AAAA",
	})
	recReplay.Body.Close()
	if recReplay.StatusCode == http.StatusOK {
		t.Fatal("consumed recovery code was allowed to replay")
	}
}

func TestFindingE_BaselineApplicationSettingsAndNineStandaloneDatabaseTables(t *testing.T) {
	_, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()
	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	// Simulate an authentic frozen Laravel baseline PostgreSQL schema WITHOUT synthetic columns/tables:
	if _, err := pool.Exec(ctx, `
		ALTER TABLE applications DROP COLUMN IF EXISTS settings_json;
		DROP TABLE IF EXISTS standalone_databases_generic;
	`); err != nil {
		t.Fatalf("simulate pure Laravel baseline schema: %v", err)
	}

	pResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", rootCookies, rootCSRF, "", map[string]any{"name": "BaselineSchemaProj"})
	var pOut struct {
		DefaultEnv struct{ UUID string `json:"uuid"` } `json:"default_environment"`
	}
	_ = json.NewDecoder(pResp.Body).Decode(&pOut)
	pResp.Body.Close()

	// 1. Application creation + application_settings persistence + GET without applications.settings_json
	appResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/dockerimage", rootCookies, rootCSRF, "", map[string]any{
		"name": "schema-app", "environment_uuid": pOut.DefaultEnv.UUID, "server_uuid": "localhost",
		"docker_registry_image_name": "nginx", "docker_registry_image_tag": "alpine",
		"settings": map[string]string{"is_static": "true", "is_spa": "true", "is_build_server_enabled": "false"},
	})
	var appOut struct {
		ID       int64             `json:"id"`
		UUID     string            `json:"uuid"`
		Settings map[string]string `json:"settings"`
	}
	appBody, _ := io.ReadAll(appResp.Body)
	appResp.Body.Close()
	if appResp.StatusCode != http.StatusCreated {
		t.Fatalf("FINDING E: create app without settings_json failed (%d): %s", appResp.StatusCode, string(appBody))
	}
	_ = json.Unmarshal(appBody, &appOut)
	var isStatic, isSPA bool
	if err := pool.QueryRow(ctx, `SELECT is_static, is_spa FROM application_settings WHERE application_id = $1`, appOut.ID).Scan(&isStatic, &isSPA); err != nil {
		t.Fatalf("FINDING E: expected row in application_settings, got err=%v", err)
	}
	if !isStatic || !isSPA {
		t.Fatalf("FINDING E: application_settings not persisted: is_static=%v is_spa=%v", isStatic, isSPA)
	}

	// 2. All 9 standalone database types stored in Laravel's real standalone_* tables!
	expectedTables := map[string]string{
		"postgresql": "standalone_postgresqls",
		"mysql":      "standalone_mysqls",
		"mariadb":    "standalone_mariadbs",
		"mongodb":    "standalone_mongodbs",
		"redis":      "standalone_redis",
		"keydb":      "standalone_keydbs",
		"dragonfly":  "standalone_dragonflies",
		"clickhouse": "standalone_clickhouses",
		"sqlite":     "standalone_sqlites",
	}
	createdUUIDs := make(map[string]string)
	for dbType, tableName := range expectedTables {
		resp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/databases/"+dbType, rootCookies, rootCSRF, "", map[string]any{
			"name": "db-" + dbType, "password": "secret-" + dbType, "server_uuid": "localhost",
			"environment_uuid": pOut.DefaultEnv.UUID,
		})
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("FINDING E: create %s in %s failed (%d): %s", dbType, tableName, resp.StatusCode, string(body))
		}
		var out struct{ UUID string `json:"uuid"` }
		_ = json.Unmarshal(body, &out)
		createdUUIDs[dbType] = out.UUID

		var count int
		if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM "+tableName+" WHERE uuid = $1", out.UUID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("FINDING E: expected row in %s for uuid %s (count=%d err=%v)", tableName, out.UUID, count, err)
		}

		// Verify GET decrypts/returns metadata + redaction boundary doesn't break
		getResp := doJSON(t, http.MethodGet, ts.URL+"/api/v1/databases/"+out.UUID, rootCookies, "", "", nil)
		getResp.Body.Close()
		if getResp.StatusCode != http.StatusOK {
			t.Fatalf("FINDING E: GET %s (%s) failed: %d", dbType, out.UUID, getResp.StatusCode)
		}
	}

	// 3. List endpoint returns all 9 newly created DBs plus coolify-db sentinel
	listResp := doJSON(t, http.MethodGet, ts.URL+"/api/v1/databases", rootCookies, "", "", nil)
	listBody, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("FINDING E: GET /api/v1/databases failed: %d", listResp.StatusCode)
	}
	for dbType, u := range createdUUIDs {
		if !strings.Contains(string(listBody), u) {
			t.Fatalf("FINDING E: missing %s (%s) in /api/v1/databases list", dbType, u)
		}
	}
}

type captureWorkerDeployTransport struct {
	mu       sync.Mutex
	captured []executor.ExecutionRequest
	echoText string
}

func (c *captureWorkerDeployTransport) StreamNDJSON(_ context.Context, req executor.ExecutionRequest, emit func(executor.ExecutionEvent) error) (int, error) {
	c.mu.Lock()
	c.captured = append(c.captured, req)
	c.mu.Unlock()
	exitZero := 0
	_ = emit(executor.ExecutionEvent{ProtocolVersion: executor.ProtocolVersionV1, EventType: "started", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 1})
	if c.echoText != "" {
		_ = emit(executor.ExecutionEvent{ProtocolVersion: executor.ProtocolVersionV1, EventType: "output", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 2, Stream: "stdout", Data: c.echoText})
	}
	_ = emit(executor.ExecutionEvent{ProtocolVersion: executor.ProtocolVersionV1, EventType: "exited", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 3, ExitCode: &exitZero})
	return 0, nil
}

func TestFindingF_WorkerRunsRealDeploymentContextAndPlannerPipeline(t *testing.T) {
	app, ts, pool, _, ciph := setupRealControlPlane(t)
	ctx := context.Background()
	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	remotePEM := "-----BEGIN OPENSSH PRIVATE KEY-----\nfinding-f-remote-key\n-----END OPENSSH PRIVATE KEY-----"
	kResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/security/keys", rootCookies, rootCSRF, "", map[string]any{
		"name": "deploy-ssh-key", "description": "for remote", "private_key": remotePEM,
	})
	var kOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(kResp.Body).Decode(&kOut)
	kResp.Body.Close()

	sResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers", rootCookies, rootCSRF, "", map[string]any{
		"name": "remote-deploy-box", "ip": "10.0.55.2", "port": 2222, "user": "deployer", "private_key_uuid": kOut.UUID,
	})
	var sOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(sResp.Body).Decode(&sOut)
	sResp.Body.Close()

	pResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", rootCookies, rootCSRF, "", map[string]any{"name": "PlannerProj"})
	var pOut struct {
		DefaultEnv struct{ UUID string `json:"uuid"` } `json:"default_environment"`
	}
	_ = json.NewDecoder(pResp.Body).Decode(&pOut)
	pResp.Body.Close()

	svResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/shared-variables", rootCookies, rootCSRF, "", map[string]any{
		"key": "DB_PASS", "value": "super-secret-db-pass-999", "type": "team", "is_shown_once": true,
	})
	svResp.Body.Close()
	if svResp.StatusCode != http.StatusCreated {
		t.Fatalf("create shared var failed: %d", svResp.StatusCode)
	}

	// 1. Dockerfile build_pack app on remote server
	aResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/dockerfile", rootCookies, rootCSRF, "", map[string]any{
		"name": "dockerfile-app", "environment_uuid": pOut.DefaultEnv.UUID, "server_uuid": sOut.UUID,
		"git_repository": "https://github.com/coollabsio/example.git", "git_branch": "main",
		"dockerfile_location": "Dockerfile.prod",
	})
	var aOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(aResp.Body).Decode(&aOut)
	aResp.Body.Close()

	evResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+aOut.UUID+"/envs", rootCookies, rootCSRF, "", map[string]any{
		"key": "APP_DSN", "value": "postgres://app:{{team.DB_PASS}}@db:5432", "is_runtime": true, "is_buildtime": true,
	})
	evResp.Body.Close()

	depResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/deploy?uuid="+aOut.UUID, rootCookies, rootCSRF, "", nil)
	var depOut struct{ DeploymentUUID string `json:"deployment_uuid"` }
	_ = json.NewDecoder(depResp.Body).Decode(&depOut)
	depResp.Body.Close()
	if depResp.StatusCode != http.StatusAccepted {
		t.Fatalf("deploy enqueue failed: %d", depResp.StatusCode)
	}

	capTrans := &captureWorkerDeployTransport{echoText: "building with secret super-secret-db-pass-999 done"}
	w := worker.NewRunner(worker.Config{
		WorkerID:      "finding-f-worker",
		Pool:          pool,
		Cipher:        ciph,
		JobCoord:      app.JobCoord,
		ScheduleCoord: schedules.NewPostgresScheduleCoordinator(pool),
		Outbox:        app.Outbox,
		Dispatcher:    app.Dispatcher,
		LogStore:      app.LogStore,
		Executor:      executor.NewClient(capTrans),
		Probe:         executor.NewPostgresRemoteProbe(pool, false),
	})
	worked, err := w.ProcessOneJob(ctx)
	if err != nil || !worked {
		t.Fatalf("ProcessOneJob failed: worked=%v err=%v", worked, err)
	}

	capTrans.mu.Lock()
	if len(capTrans.captured) != 1 {
		capTrans.mu.Unlock()
		t.Fatalf("expected 1 deploy request captured, got %d", len(capTrans.captured))
	}
	req := capTrans.captured[0]
	capTrans.mu.Unlock()

	if req.ExpectedCapability != "ssh_exec" || req.Operation.Host != "10.0.55.2" || req.Operation.Port != 2222 || req.Operation.User != "deployer" {
		t.Fatalf("FINDING F: wrong SSH target in worker: cap=%s op=%+v", req.ExpectedCapability, req.Operation)
	}
	if req.Operation.SSHKeyContent != remotePEM {
		t.Fatalf("FINDING F: worker failed to decrypt/attach server SSH private key (len=%d)", len(req.Operation.SSHKeyContent))
	}
	if !strings.Contains(req.Operation.Command, "git fetch") || !strings.Contains(req.Operation.Command, "Dockerfile.prod") || !strings.Contains(req.Operation.Command, "docker build") {
		t.Fatalf("FINDING F: worker bypassed DeploymentPlanner (dockerfile): cmd=%q", req.Operation.Command)
	}

	// Verify logs NEVER leak resolved/locked secrets in go_operation_logs or application_deployment_queues!
	sseResp := doJSON(t, http.MethodGet, ts.URL+"/realtime/logs?resource="+aOut.UUID+"&once=1", rootCookies, "", "", nil)
	sseBody, _ := io.ReadAll(sseResp.Body)
	sseResp.Body.Close()
	if strings.Contains(string(sseBody), "super-secret-db-pass-999") {
		t.Fatalf("FINDING F: plaintext locked secret leaked in SSE log stream: %s", string(sseBody))
	}
	if !strings.Contains(string(sseBody), "[REDACTED]") {
		t.Fatalf("FINDING F: expected [REDACTED] marker in log stream, got: %s", string(sseBody))
	}

	// 2. Unconfirmed nixpacks build_pack MUST fail via DeploymentPlanner without spawning executor!
	nixResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications", rootCookies, rootCSRF, "", map[string]any{
		"name": "nix-app", "environment_uuid": pOut.DefaultEnv.UUID, "server_uuid": sOut.UUID,
		"build_pack": "nixpacks", "git_repository": "https://github.com/coollabsio/example.git",
	})
	var nixOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(nixResp.Body).Decode(&nixOut)
	nixResp.Body.Close()
	nixDep := doJSON(t, http.MethodPost, ts.URL+"/api/v1/deploy?uuid="+nixOut.UUID, rootCookies, rootCSRF, "", nil)
	var nixDepOut struct{ DeploymentUUID string `json:"deployment_uuid"` }
	_ = json.NewDecoder(nixDep.Body).Decode(&nixDepOut)
	nixDep.Body.Close()

	capTrans.mu.Lock()
	beforeCount := len(capTrans.captured)
	capTrans.mu.Unlock()

	_, _ = w.ProcessOneJob(ctx)

	capTrans.mu.Lock()
	afterCount := len(capTrans.captured)
	capTrans.mu.Unlock()
	if afterCount != beforeCount {
		t.Fatalf("FINDING F: unconfirmed nixpacks still invoked executor instead of failing in DeploymentPlanner")
	}
	var st string
	_ = pool.QueryRow(ctx, `SELECT status FROM application_deployment_queues WHERE deployment_uuid = $1`, nixDepOut.DeploymentUUID).Scan(&st)
	if st != "failed" {
		t.Fatalf("FINDING F: expected failed status for blocked nixpacks deploy, got %q", st)
	}
}

type stubBackupDumper struct {
	mu    sync.Mutex
	calls []backups.DumpTarget
}

func (s *stubBackupDumper) Dump(_ context.Context, target backups.DumpTarget) ([]byte, error) {
	s.mu.Lock()
	s.calls = append(s.calls, target)
	n := len(s.calls)
	s.mu.Unlock()
	return []byte(fmt.Sprintf("-- dump #%d type=%s db=%s pass=%s\n", n, target.DatabaseType, target.DBName, target.DBPassword)), nil
}

func (s *stubBackupDumper) Restore(_ context.Context, _ backups.DumpTarget, _ []byte) error {
	return nil
}

func TestFindingG_ScheduledDatabaseBackupExecutedByWorkerWithRealDumpAndRetention(t *testing.T) {
	app, ts, pool, _, ciph := setupRealControlPlane(t)
	ctx := context.Background()
	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	remotePEM := "-----BEGIN OPENSSH PRIVATE KEY-----\nfinding-g-key\n-----END OPENSSH PRIVATE KEY-----"
	kResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/security/keys", rootCookies, rootCSRF, "", map[string]any{
		"name": "bk-srv-key", "description": "bk", "private_key": remotePEM,
	})
	var kOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(kResp.Body).Decode(&kOut)
	kResp.Body.Close()

	sResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers", rootCookies, rootCSRF, "", map[string]any{
		"name": "remote-db-host", "ip": "10.0.77.2", "port": 22, "user": "root", "private_key_uuid": kOut.UUID,
	})
	var sOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(sResp.Body).Decode(&sOut)
	sResp.Body.Close()

	dbResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/databases/mysql", rootCookies, rootCSRF, "", map[string]any{
		"name": "orders-mysql", "password": "mysql-root-pw-77", "server_uuid": sOut.UUID,
	})
	var dbOut struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.NewDecoder(dbResp.Body).Decode(&dbOut)
	dbResp.Body.Close()
	if dbResp.StatusCode != http.StatusCreated {
		t.Fatalf("create mysql failed: %d", dbResp.StatusCode)
	}

	var schedID int64
	if err := pool.QueryRow(ctx, `
		INSERT INTO scheduled_database_backups (uuid, enabled, frequency, timezone, database_retention_count, database_id, database_type, team_id)
		VALUES ('sched-mysql-1', TRUE, '* * * * *', 'UTC', 1, $1, 'mysql', 0)
		RETURNING id
	`, dbOut.ID).Scan(&schedID); err != nil {
		t.Fatal(err)
	}

	dumper := &stubBackupDumper{}
	backupDir := t.TempDir()
	bkSvc := backups.NewPostgresBackupService(pool, app.Authz, app.Hooks, dumper, backupDir)
	w := worker.NewRunner(worker.Config{
		WorkerID:      "finding-g-worker",
		Pool:          pool,
		Cipher:        ciph,
		JobCoord:      app.JobCoord,
		ScheduleCoord: schedules.NewPostgresScheduleCoordinator(pool),
		Outbox:        app.Outbox,
		Dispatcher:    app.Dispatcher,
		LogStore:      app.LogStore,
		Executor:      executor.NewClient(&mockRemoteProbeTransport{}),
		BackupSvc:     bkSvc,
	})

	minute1 := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	n, err := w.TickSchedules(ctx, minute1)
	if err != nil || n != 1 {
		t.Fatalf("TickSchedules failed: n=%d err=%v", n, err)
	}

	var enqueuedServerUUID, enqueuedJobType string
	if err := pool.QueryRow(ctx, `SELECT server_uuid, job_type FROM go_job_operations WHERE job_type = 'backup' ORDER BY created_at DESC LIMIT 1`).Scan(&enqueuedServerUUID, &enqueuedJobType); err != nil {
		t.Fatal(err)
	}
	if enqueuedServerUUID != sOut.UUID {
		t.Fatalf("FINDING G: scheduled backup enqueued against %q instead of database server %q", enqueuedServerUUID, sOut.UUID)
	}

	worked, err := w.ProcessOneJob(ctx)
	if err != nil || !worked {
		t.Fatalf("ProcessOneJob(backup) failed: worked=%v err=%v", worked, err)
	}

	var opStatus string
	_ = pool.QueryRow(ctx, `SELECT status FROM go_job_operations WHERE job_type = 'backup' ORDER BY created_at DESC LIMIT 1`).Scan(&opStatus)
	if opStatus != string(jobs.StatusSucceeded) {
		t.Fatalf("FINDING G: expected backup job status succeeded, got %q", opStatus)
	}

	// Second scheduled run 1 minute later MUST prune down to retention=1!
	if n2, err := w.TickSchedules(ctx, minute1.Add(time.Minute)); err != nil || n2 != 1 {
		t.Fatalf("second TickSchedules failed: n=%d err=%v", n2, err)
	}
	_, _ = w.ProcessOneJob(ctx)

	var execCount int
	var lastSHA string
	_ = pool.QueryRow(ctx, `SELECT COUNT(*), COALESCE(MAX(checksum_sha256), '') FROM scheduled_database_backup_executions WHERE scheduled_database_backup_id = $1`, schedID).Scan(&execCount, &lastSHA)
	if execCount != 1 || lastSHA == "" {
		t.Fatalf("FINDING G: expected 1 retained backup execution with real SHA-256, got count=%d sha=%q", execCount, lastSHA)
	}
	dumper.mu.Lock()
	callCount := len(dumper.calls)
	var firstCall backups.DumpTarget
	if callCount > 0 {
		firstCall = dumper.calls[0]
	}
	dumper.mu.Unlock()
	if callCount != 2 || firstCall.DBPassword != "mysql-root-pw-77" {
		t.Fatalf("FINDING G: expected worker to decrypt & pass DB password to dumper cleanly, calls=%d gotPass=%q", callCount, firstCall.DBPassword)
	}
}

type mockRemoteProbeTransport struct {
	mu       sync.Mutex
	requests []executor.ExecutionRequest
}

func (m *mockRemoteProbeTransport) StreamNDJSON(_ context.Context, req executor.ExecutionRequest, emit func(executor.ExecutionEvent) error) (int, error) {
	m.mu.Lock()
	m.requests = append(m.requests, req)
	m.mu.Unlock()
	exitZero := 0
	_ = emit(executor.ExecutionEvent{ProtocolVersion: executor.ProtocolVersionV1, EventType: "started", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 1})
	if strings.Contains(req.Operation.Command, "COOLIFY_BOOT=") {
		_ = emit(executor.ExecutionEvent{ProtocolVersion: executor.ProtocolVersionV1, EventType: "output", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 2, Stream: "stdout", Data: "COOLIFY_BOOT=remote-boot-uuid-888"})
		_ = emit(executor.ExecutionEvent{ProtocolVersion: executor.ProtocolVersionV1, EventType: "output", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 3, Stream: "stdout", Data: "COOLIFY_PROC=ALIVE"})
		_ = emit(executor.ExecutionEvent{ProtocolVersion: executor.ProtocolVersionV1, EventType: "output", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 4, Stream: "stdout", Data: "COOLIFY_CONT=c-remote-123"})
		_ = emit(executor.ExecutionEvent{ProtocolVersion: executor.ProtocolVersionV1, EventType: "output", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 5, Stream: "stdout", Data: "COOLIFY_JOURNAL_FOUND=0"})
	}
	_ = emit(executor.ExecutionEvent{ProtocolVersion: executor.ProtocolVersionV1, EventType: "exited", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 6, ExitCode: &exitZero})
	return 0, nil
}

func TestFindingH_RemoteProbeUsesTargetServerSSHJournalBootIDAndContainerInspection(t *testing.T) {
	_, ts, pool, _, ciph := setupRealControlPlane(t)
	ctx := context.Background()
	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	remotePEM := "-----BEGIN OPENSSH PRIVATE KEY-----\nfinding-h-probe-key\n-----END OPENSSH PRIVATE KEY-----"
	kResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/security/keys", rootCookies, rootCSRF, "", map[string]any{
		"name": "probe-key", "description": "for probe", "private_key": remotePEM,
	})
	var kOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(kResp.Body).Decode(&kOut)
	kResp.Body.Close()

	sResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers", rootCookies, rootCSRF, "", map[string]any{
		"name": "probe-target-srv", "ip": "10.0.88.5", "port": 2222, "user": "ops", "private_key_uuid": kOut.UUID,
	})
	var sOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(sResp.Body).Decode(&sOut)
	sResp.Body.Close()

	sshProbeTrans := &mockRemoteProbeTransport{}
	execClient := executor.NewClient(sshProbeTrans)
	probe := executor.NewSSHAwareRemoteProbe(pool, execClient, func(raw string) string {
		if plain, decErr := ciph.DecryptLaravelPayload(raw, false); decErr == nil {
			return string(plain)
		}
		return raw
	}, true)
	tracker := executor.NewRemoteTracker(probe)

	j, err := tracker.EnsureOperationJournal(ctx, executor.OperationIntent{
		JobID: "job-h-1", OperationID: "op-h-1", AttemptID: "att-1",
		OwnershipEpoch: 1, ResourceUUID: "app-h-1", ServerUUID: sOut.UUID, InputFingerprint: "fp-1",
	})
	if err != nil {
		t.Fatalf("EnsureOperationJournal failed: %v", err)
	}
	if j.BootID != "remote-boot-uuid-888" {
		t.Fatalf("FINDING H: expected remote boot_id 'remote-boot-uuid-888', got %q", j.BootID)
	}

	curBoot, procAlive, contRunning, err := probe.InspectHostBootAndProcess(ctx, sOut.UUID, j.BootID, 4242, "op-h-1")
	if err != nil || curBoot != "remote-boot-uuid-888" || !procAlive || !contRunning {
		t.Fatalf("FINDING H: remote inspection mismatch boot=%q proc=%v cont=%v err=%v", curBoot, procAlive, contRunning, err)
	}

	sshProbeTrans.mu.Lock()
	reqs := append([]executor.ExecutionRequest(nil), sshProbeTrans.requests...)
	sshProbeTrans.mu.Unlock()
	if len(reqs) < 2 {
		t.Fatalf("FINDING H: expected SSH probe and atomic remote journal write calls, got %d", len(reqs))
	}
	sawJournalWrite := false
	for _, r := range reqs {
		if r.ExpectedCapability != "ssh_exec" || r.Operation.Host != "10.0.88.5" || r.Operation.Port != 2222 || r.Operation.User != "ops" || r.Operation.SSHKeyContent != remotePEM {
			t.Fatalf("FINDING H: remote probe used wrong SSH config: cap=%s op=%+v", r.ExpectedCapability, r.Operation)
		}
		if strings.Contains(r.Operation.Command, "/data/coolify/operations/op-h-1/journal.json") && strings.Contains(r.Operation.Command, "mv -f") {
			sawJournalWrite = true
		}
	}
	if !sawJournalWrite {
		t.Fatalf("FINDING H: remote atomic journal write command not executed over SSH")
	}
}
