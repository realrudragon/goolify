package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/coollabsio/coolify/backend/internal/worker"
)

type recordingTransport struct {
	mu       sync.Mutex
	commands []string
}

func (rt *recordingTransport) StreamNDJSON(_ context.Context, req executor.ExecutionRequest, emit func(executor.ExecutionEvent) error) (int, error) {
	rt.mu.Lock()
	rt.commands = append(rt.commands, req.Operation.Command)
	rt.mu.Unlock()

	zero := 0
	_ = emit(executor.ExecutionEvent{ProtocolVersion: "v1", EventType: "started", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 1})
	_ = emit(executor.ExecutionEvent{ProtocolVersion: "v1", EventType: "output", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 2, Stream: "stdout", Data: "executed: " + req.Operation.Command})
	_ = emit(executor.ExecutionEvent{ProtocolVersion: "v1", EventType: "exited", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 3, ExitCode: &zero})
	return 0, nil
}

func (rt *recordingTransport) lastCommand() string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if len(rt.commands) == 0 {
		return ""
	}
	return rt.commands[len(rt.commands)-1]
}

func TestPhase4_EndToEndRuntimeSimulation_TraefikDockerDatabaseAndEmail(t *testing.T) {
	app, ts, pool, _, _ := setupRealControlPlane(t)
	ctx := context.Background()
	cookies, csrf := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	recTrans := &recordingTransport{}
	execClient := executor.NewClient(recTrans)
	wRunner := worker.NewRunner(worker.Config{
		WorkerID:   "sim-worker-1",
		Pool:       pool,
		Cipher:     app.Cipher,
		JobCoord:   app.JobCoord,
		Outbox:     app.Outbox,
		Dispatcher: app.Dispatcher,
		LogStore:   app.LogStore,
		Executor:   execClient,
		LeaseTTL:   30 * time.Second,
	})

	// Clear any pre-queued jobs so our simulation worker processes exact jobs deterministically
	_, _ = pool.Exec(ctx, `DELETE FROM go_job_operations`)

	// 1. Traefik Proxy Default Compose + Real Start/Restart & Stop via Worker
	prxGet := doJSON(t, http.MethodGet, ts.URL+"/api/v1/servers/localhost/proxy?type=traefik", cookies, csrf, "", nil)
	var prxBundle struct {
		ProxyType   string `json:"proxy_type"`
		ComposeYAML string `json:"compose_yaml"`
	}
	_ = json.NewDecoder(prxGet.Body).Decode(&prxBundle)
	prxGet.Body.Close()
	for _, want := range []string{
		"container_name: coolify-proxy",
		"image: traefik:v3.7",
		"--providers.docker=true",
		"--providers.file.directory=/traefik/dynamic/",
		"--certificatesresolvers.letsencrypt.acme.httpchallenge=true",
	} {
		if !strings.Contains(prxBundle.ComposeYAML, want) {
			t.Fatalf("default Traefik compose missing %q:\n%s", want, prxBundle.ComposeYAML)
		}
	}

	prxRestart := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/localhost/proxy/restart", cookies, csrf, "", nil)
	prxRestart.Body.Close()
	if prxRestart.StatusCode != http.StatusAccepted {
		t.Fatalf("expected 202 from proxy/restart, got %d", prxRestart.StatusCode)
	}
	if worked, err := wRunner.ProcessOneJob(ctx); err != nil || !worked {
		t.Fatalf("expected worker to process proxy_restart job: worked=%v err=%v", worked, err)
	}
	prxCmd := recTrans.lastCommand()
	for _, want := range []string{
		"/data/coolify/proxy/docker-compose.yml",
		"/data/coolify/proxy/dynamic/coolify.yaml",
		"docker network create --attachable 'coolify'",
		"docker compose -f '/data/coolify/proxy/docker-compose.yml' up -d --remove-orphans",
		"docker network connect 'coolify' coolify-proxy",
	} {
		if !strings.Contains(prxCmd, want) {
			t.Fatalf("proxy_restart worker command missing %q:\n%s", want, prxCmd)
		}
	}
	var proxyStatus string
	_ = pool.QueryRow(ctx, `SELECT COALESCE(proxy->>'status','') FROM servers WHERE id = 0`).Scan(&proxyStatus)
	if proxyStatus != "running" {
		t.Fatalf("expected servers.proxy status='running' after proxy_restart, got %q", proxyStatus)
	}

	prxStop := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/localhost/proxy/stop", cookies, csrf, "", nil)
	prxStop.Body.Close()
	if worked, err := wRunner.ProcessOneJob(ctx); err != nil || !worked {
		t.Fatalf("expected worker to process proxy_stop job: worked=%v err=%v", worked, err)
	}
	if !strings.Contains(recTrans.lastCommand(), "docker stop -t 30 coolify-proxy") {
		t.Fatalf("unexpected proxy_stop command: %s", recTrans.lastCommand())
	}
	_ = pool.QueryRow(ctx, `SELECT COALESCE(proxy->>'status','') FROM servers WHERE id = 0`).Scan(&proxyStatus)
	if proxyStatus != "exited" {
		t.Fatalf("expected servers.proxy status='exited' after proxy_stop, got %q", proxyStatus)
	}

	// 2. Real Authenticated, Networked, Persistent Database Container Provisioning + Secret Redaction
	projResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", cookies, csrf, "", map[string]string{"name": "Sim Project"})
	var proj struct {
		DefaultEnvironment struct{ UUID string `json:"uuid"` } `json:"default_environment"`
	}
	_ = json.NewDecoder(projResp.Body).Decode(&proj)
	projResp.Body.Close()

	dbResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/databases/postgresql", cookies, csrf, "", map[string]any{
		"name": "Sim PG", "environment_uuid": proj.DefaultEnvironment.UUID, "server_uuid": "localhost",
	})
	var dbCreated struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(dbResp.Body).Decode(&dbCreated)
	dbResp.Body.Close()

	dbPatch := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/databases/"+dbCreated.UUID, cookies, csrf, "", map[string]any{
		"is_public": true, "public_port": 15439,
	})
	var dbMeta struct {
		Password string `json:"password"`
	}
	_ = json.NewDecoder(dbPatch.Body).Decode(&dbMeta)
	dbPatch.Body.Close()

	dbStart := doJSON(t, http.MethodPost, ts.URL+"/api/v1/databases/"+dbCreated.UUID+"/start", cookies, csrf, "", nil)
	dbStart.Body.Close()
	if worked, err := wRunner.ProcessOneJob(ctx); err != nil || !worked {
		t.Fatalf("expected worker to process db_provision job: worked=%v err=%v", worked, err)
	}
	dbCmd := recTrans.lastCommand()
	for _, want := range []string{
		"mkdir -p '/data/coolify/databases/" + dbCreated.UUID + "'",
		"--network 'coolify'",
		"-p 15439:5432",
		"-v '/data/coolify/databases/" + dbCreated.UUID + ":/var/lib/postgresql/data'",
		"-e POSTGRES_PASSWORD='" + dbMeta.Password + "'",
	} {
		if !strings.Contains(dbCmd, want) {
			t.Fatalf("db start command missing %q:\n%s", want, dbCmd)
		}
	}
	// Verify DB password is redacted from go_operation_logs!
	var leakedCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM go_operation_logs WHERE resource_uuid = $1 AND line LIKE '%' || $2 || '%'`, dbCreated.UUID, dbMeta.Password).Scan(&leakedCount)
	if leakedCount > 0 {
		t.Fatalf("expected database password to be redacted in go_operation_logs, found %d raw occurrences", leakedCount)
	}

	// 3. Team Email Notifications via Instance SMTP (use_instance_email_settings) AND via Resend API
	smtpHost, smtpPort, getMail, stopSMTP := startLocalSMTPServer(t)
	defer stopSMTP()

	_, _ = pool.Exec(ctx, `
		UPDATE instance_settings
		SET smtp_enabled = TRUE, smtp_host = $1, smtp_port = $2, smtp_from_address = 'instance@coolify.test'
		WHERE id = 0
	`, smtpHost, smtpPort)

	emPatch := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/notifications/email", cookies, csrf, "", map[string]any{
		"enabled":                     true,
		"use_instance_email_settings": true,
		"smtp_recipients":             "ops@coolify.test",
		"events": map[string]bool{
			"deployment_success": true,
			"backup_failure":     true,
		},
	})
	emPatch.Body.Close()
	if emPatch.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /api/v1/notifications/email failed: %d", emPatch.StatusCode)
	}

	emTest := doJSON(t, http.MethodPost, ts.URL+"/api/v1/notifications/test", cookies, csrf, "", map[string]any{
		"channel": "email", "event": "deployment.finished", "summary": "Simulated deploy finished via instance SMTP",
	})
	emTest.Body.Close()
	if emTest.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/notifications/test (instance SMTP) failed: %d", emTest.StatusCode)
	}
	if !strings.Contains(getMail(), "Simulated deploy finished via instance SMTP") {
		t.Fatalf("expected instance SMTP server to receive notification email, got:\n%s", getMail())
	}

	// Now test Resend API delivery
	var resendReceived bool
	resendMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/emails" && r.Header.Get("Authorization") == "Bearer re_live_12345" {
			resendReceived = true
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"email_1"}`))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer resendMock.Close()

	resPatch := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/notifications/email", cookies, csrf, "", map[string]any{
		"enabled":                     true,
		"use_instance_email_settings": false,
		"resend_enabled":              true,
		"resend_api_key":              "re_live_12345",
		"resend_from_address":         "resend@coolify.test",
		"smtp_host":                   resendMock.URL,
		"smtp_recipients":             "team@coolify.test",
	})
	resPatch.Body.Close()

	resTest := doJSON(t, http.MethodPost, ts.URL+"/api/v1/notifications/test", cookies, csrf, "", map[string]any{
		"channel": "email", "event": "backup.succeeded", "summary": "Simulated backup via Resend API",
	})
	resTest.Body.Close()
	if resTest.StatusCode != http.StatusOK || !resendReceived {
		t.Fatalf("expected Resend API notification delivery to succeed: status=%d received=%v", resTest.StatusCode, resendReceived)
	}
}
