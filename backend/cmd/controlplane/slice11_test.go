package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/coollabsio/coolify/backend/internal/realtime"
	"github.com/coollabsio/coolify/backend/internal/worker"
)

type sliceTestTransport struct {
	seen []executor.ExecutionRequest
}

func (t *sliceTestTransport) StreamNDJSON(_ context.Context, req executor.ExecutionRequest, onEvent func(executor.ExecutionEvent) error) (int, error) {
	t.seen = append(t.seen, req)
	_ = onEvent(executor.ExecutionEvent{
		ProtocolVersion: executor.ProtocolVersionV1,
		EventType:       "output",
		OperationID:     req.OperationID,
		AttemptID:       req.AttemptID,
		Sequence:        1,
		Stream:          "stdout",
		Data:            "executed: " + req.Operation.Command,
	})
	code := 0
	_ = onEvent(executor.ExecutionEvent{
		ProtocolVersion: executor.ProtocolVersionV1,
		EventType:       "exited",
		OperationID:     req.OperationID,
		AttemptID:       req.AttemptID,
		Sequence:        2,
		ExitCode:        &code,
	})
	return 0, nil
}

func TestProductionSlice11_ResourceSubRoutesBackupsAndWorkerExecution(t *testing.T) {
	app, ts, pool, rc, ciph := setupRealControlPlane(t)
	ctx := context.Background()

	rootCookies, rootCSRF := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")

	// Seed a second team + admin user to assert cross-team boundaries
	otherResp := doJSON(t, http.MethodPost, ts.URL+"/app/auth/register", nil, "", "", map[string]string{
		"name": "Other User", "email": "slice11-other@coolify.test", "password": "OtherSecret123!",
	})
	otherResp.Body.Close()
	otherCookies, otherCSRF := loginClient(t, ts.URL, "slice11-other@coolify.test", "OtherSecret123!")

	// 1. Feedback endpoint
	fbResp := doJSON(t, http.MethodPost, ts.URL+"/api/feedback", rootCookies, rootCSRF, "", map[string]string{
		"content": "Migration looks crisp", "email": "root@coolify.test",
	})
	fbResp.Body.Close()
	if fbResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/feedback failed: %d", fbResp.StatusCode)
	}
	var fbCount int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM go_feedbacks WHERE content = 'Migration looks crisp'`).Scan(&fbCount)
	if fbCount != 1 {
		t.Fatalf("expected 1 feedback row, got %d", fbCount)
	}

	// 2. Instance email settings (root team allowed + encrypted; other team forbidden 403)
	deniedEmail := doJSON(t, http.MethodGet, ts.URL+"/api/v1/settings/email", otherCookies, "", "", nil)
	deniedEmail.Body.Close()
	if deniedEmail.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for non-root team on /api/v1/settings/email, got %d", deniedEmail.StatusCode)
	}
	patchEmail := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/settings/email", rootCookies, rootCSRF, "", map[string]any{
		"smtp_enabled": true, "smtp_host": "smtp.mailpit.local", "smtp_port": 1025,
		"smtp_password": "smtp-secret-value", "resend_enabled": true, "resend_api_key": "re_live_secret_999",
	})
	emailBody, _ := io.ReadAll(patchEmail.Body)
	patchEmail.Body.Close()
	if patchEmail.StatusCode != http.StatusOK || !strings.Contains(string(emailBody), "smtp-secret-value") {
		t.Fatalf("PATCH /api/v1/settings/email failed: %d %s", patchEmail.StatusCode, string(emailBody))
	}
	var encSMTP, encResend string
	_ = pool.QueryRow(ctx, `SELECT smtp_password, resend_api_key FROM instance_settings WHERE id = 0`).Scan(&encSMTP, &encResend)
	if strings.Contains(encSMTP, "smtp-secret-value") || strings.Contains(encResend, "re_live_secret_999") {
		t.Fatal("email secrets stored in plaintext!")
	}

	// 3. Tags + S3 Storage patch/validate
	tagResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/tags", rootCookies, rootCSRF, "", map[string]string{"name": "prod-tier"})
	var tagOut struct{ ID int64 `json:"id"` }
	_ = json.NewDecoder(tagResp.Body).Decode(&tagOut)
	tagResp.Body.Close()
	if tagOut.ID <= 0 {
		t.Fatal("failed to create tag")
	}
	patchTag := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/tags/prod-tier", rootCookies, rootCSRF, "", map[string]string{"name": "prod-renamed"})
	patchTag.Body.Close()
	if patchTag.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /api/v1/tags failed: %d", patchTag.StatusCode)
	}

	s3Mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s3Mock.Close()

	s3Resp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/s3-storages", rootCookies, rootCSRF, "", map[string]any{
		"name": "primary-s3", "bucket": "coolify-bkp", "region": "us-east-1", "endpoint": s3Mock.URL, "key": "k1", "secret": "s1",
	})
	var s3Out struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(s3Resp.Body).Decode(&s3Out)
	s3Resp.Body.Close()
	valS3 := doJSON(t, http.MethodPost, ts.URL+"/api/v1/s3-storages/"+s3Out.UUID+"/validate", rootCookies, rootCSRF, "", nil)
	valS3.Body.Close()
	if valS3.StatusCode != http.StatusOK {
		t.Fatalf("POST /s3-storages/validate failed: %d", valS3.StatusCode)
	}

	// 4. Project environments + env shared vars + move/migrate/clone/destinations/rollback/tasks
	projResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects", rootCookies, rootCSRF, "", map[string]any{"name": "Slice11 Project"})
	var projOut struct {
		Project    struct{ UUID string `json:"uuid"` } `json:"project"`
		DefaultEnv struct{ UUID string `json:"uuid"` } `json:"default_environment"`
	}
	_ = json.NewDecoder(projResp.Body).Decode(&projOut)
	projResp.Body.Close()

	env2Resp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects/"+projOut.Project.UUID+"/environments", rootCookies, rootCSRF, "", map[string]string{"name": "staging"})
	var env2Out struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(env2Resp.Body).Decode(&env2Out)
	env2Resp.Body.Close()

	patchEnv := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/projects/"+projOut.Project.UUID+"/environments/"+env2Out.UUID, rootCookies, rootCSRF, "", map[string]string{"description": "Staging Env"})
	patchEnv.Body.Close()
	if patchEnv.StatusCode != http.StatusOK {
		t.Fatalf("PATCH project environment failed: %d", patchEnv.StatusCode)
	}
	envDet := doJSON(t, http.MethodGet, ts.URL+"/api/v1/projects/"+projOut.Project.UUID+"/"+env2Out.UUID, rootCookies, "", "", nil)
	envDetBody, _ := io.ReadAll(envDet.Body)
	envDet.Body.Close()
	if envDet.StatusCode != http.StatusOK || !strings.Contains(string(envDetBody), "Staging Env") {
		t.Fatalf("GET /projects/{uuid}/{env} failed: %d %s", envDet.StatusCode, string(envDetBody))
	}

	envSharedPost := doJSON(t, http.MethodPost, ts.URL+"/api/v1/projects/"+projOut.Project.UUID+"/environments/"+env2Out.UUID+"/envs", rootCookies, rootCSRF, "", map[string]any{
		"key": "ENV_SHARED", "value": "secret-env-val",
	})
	envSharedPost.Body.Close()
	if envSharedPost.StatusCode != http.StatusCreated {
		t.Fatalf("POST environment shared env failed: %d", envSharedPost.StatusCode)
	}

	// Create application + extra server/destination for migrate/destinations test
	appResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/dockerimage", rootCookies, rootCSRF, "", map[string]any{
		"name": "s11-app", "environment_uuid": projOut.DefaultEnv.UUID, "server_uuid": "localhost",
		"docker_registry_image_name": "nginx", "docker_registry_image_tag": "1.27",
	})
	var appOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(appResp.Body).Decode(&appOut)
	appResp.Body.Close()

	// Create secondary server + destination in root team
	srv2Resp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers", rootCookies, rootCSRF, "", map[string]any{
		"name": "worker-node-2", "ip": "127.0.0.2", "port": 22, "user": "root", "private_key_uuid": "pk-0",
	})
	var srv2Out struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(srv2Resp.Body).Decode(&srv2Out)
	srv2Resp.Body.Close()
	dest2Resp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/"+srv2Out.UUID+"/destinations", rootCookies, rootCSRF, "", map[string]any{
		"name": "net-node-2", "network": "coolify-node2",
	})
	var dest2Out struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(dest2Resp.Body).Decode(&dest2Out)
	dest2Resp.Body.Close()

	// App destinations attach + list + delete
	addDest := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/destinations", rootCookies, rootCSRF, "", map[string]string{"destination_uuid": dest2Out.UUID})
	addDest.Body.Close()
	if addDest.StatusCode != http.StatusCreated {
		t.Fatalf("POST /applications/destinations failed: %d", addDest.StatusCode)
	}
	listDest := doJSON(t, http.MethodGet, ts.URL+"/api/v1/applications/"+appOut.UUID+"/destinations", rootCookies, "", "", nil)
	listDestBody, _ := io.ReadAll(listDest.Body)
	listDest.Body.Close()
	if !strings.Contains(string(listDestBody), dest2Out.UUID) {
		t.Fatalf("GET /applications/destinations missing attached dest: %s", string(listDestBody))
	}
	delDest := doJSON(t, http.MethodDelete, ts.URL+"/api/v1/applications/"+appOut.UUID+"/destinations/"+dest2Out.UUID, rootCookies, rootCSRF, "", nil)
	delDest.Body.Close()
	if delDest.StatusCode != http.StatusOK {
		t.Fatalf("DELETE /applications/destinations failed: %d", delDest.StatusCode)
	}

	// Move application to staging env + migrate to node-2 destination
	mvResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/move", rootCookies, rootCSRF, "", map[string]string{"environment_uuid": env2Out.UUID})
	mvResp.Body.Close()
	if mvResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /applications/move failed: %d", mvResp.StatusCode)
	}
	migResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/migrate", rootCookies, rootCSRF, "", map[string]any{"destination_uuid": dest2Out.UUID, "migrate_volumes": true})
	migResp.Body.Close()
	if migResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /applications/migrate failed: %d", migResp.StatusCode)
	}
	// Migrate back to localhost so worker jobs target localhost
	migBack := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/migrate", rootCookies, rootCSRF, "", map[string]any{"destination_uuid": "standalone-docker-0"})
	migBack.Body.Close()

	// App tags attach + cross-team denial + detach
	addAppTag := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/tags", rootCookies, rootCSRF, "", map[string]string{"name": "prod-renamed"})
	addAppTag.Body.Close()
	if addAppTag.StatusCode != http.StatusCreated {
		t.Fatalf("POST /applications/tags failed: %d", addAppTag.StatusCode)
	}
	deniedTag := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/tags", otherCookies, otherCSRF, "", map[string]string{"name": "evil"})
	deniedTag.Body.Close()
	if deniedTag.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for cross-team POST app tag, got %d", deniedTag.StatusCode)
	}

	// Rollback images + rollback deployment
	var appID int64
	_ = pool.QueryRow(ctx, `SELECT id FROM applications WHERE uuid = $1`, appOut.UUID).Scan(&appID)
	_, err := pool.Exec(ctx, `
		INSERT INTO application_deployment_queues (deployment_uuid, application_id, application_name, server_id, server_name, status, commit, pull_request_id)
		VALUES ('dep-prev-1', $1, 's11-app', 0, 'localhost', 'finished', 'sha-deadbeef', 0)
	`, appID)
	if err != nil {
		t.Fatal(err)
	}
	rbList := doJSON(t, http.MethodGet, ts.URL+"/api/v1/applications/"+appOut.UUID+"/rollback-images", rootCookies, "", "", nil)
	rbBody, _ := io.ReadAll(rbList.Body)
	rbList.Body.Close()
	if rbList.StatusCode != http.StatusOK || !strings.Contains(string(rbBody), "sha-deadbeef") {
		t.Fatalf("GET rollback-images failed: %d %s", rbList.StatusCode, string(rbBody))
	}
	rbRun := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/rollback", rootCookies, rootCSRF, "", map[string]string{"commit": "sha-deadbeef"})
	rbRunBody, _ := io.ReadAll(rbRun.Body)
	rbRun.Body.Close()
	if rbRun.StatusCode != http.StatusOK || !strings.Contains(string(rbRunBody), "Rollback deployment queued") {
		t.Fatalf("POST rollback failed: %d %s", rbRun.StatusCode, string(rbRunBody))
	}

	// Storage patch + volume backup schedule + run + worker execution + download check
	pvResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/storages", rootCookies, rootCSRF, "", map[string]any{
		"type": "persistent", "name": "app-vol", "mount_path": "/var/lib/app",
	})
	var pvOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(pvResp.Body).Decode(&pvOut)
	pvResp.Body.Close()

	patchStorage := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/applications/"+appOut.UUID+"/storages", rootCookies, rootCSRF, "", map[string]any{
		"uuid": pvOut.UUID, "mount_path": "/var/lib/app2",
	})
	patchStorage.Body.Close()
	if patchStorage.StatusCode != http.StatusOK {
		t.Fatalf("PATCH storage failed: %d", patchStorage.StatusCode)
	}
	putVolBkp := doJSON(t, http.MethodPut, ts.URL+"/api/v1/applications/"+appOut.UUID+"/storages/"+pvOut.UUID+"/backups", rootCookies, rootCSRF, "", map[string]any{
		"frequency": "0 3 * * *", "enabled": true, "retention_amount_locally": 5,
	})
	putVolBkp.Body.Close()
	if putVolBkp.StatusCode != http.StatusOK {
		t.Fatalf("PUT volume backup failed: %d", putVolBkp.StatusCode)
	}
	runVolBkp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/storages/"+pvOut.UUID+"/backups/run", rootCookies, rootCSRF, "", nil)
	var volRunOut struct {
		ExecutionUUID string `json:"execution_uuid"`
		OperationID   string `json:"operation_id"`
	}
	_ = json.NewDecoder(runVolBkp.Body).Decode(&volRunOut)
	runVolBkp.Body.Close()
	if runVolBkp.StatusCode != http.StatusAccepted || volRunOut.OperationID == "" {
		t.Fatalf("POST volume backup run failed: %d", runVolBkp.StatusCode)
	}

	// Scheduled task create + execute + worker execution
	taskResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/scheduled-tasks", rootCookies, rootCSRF, "", map[string]any{
		"name": "nightly-cache-clear", "command": "php artisan cache:clear", "frequency": "0 4 * * *", "enabled": true,
	})
	var taskOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(taskResp.Body).Decode(&taskOut)
	taskResp.Body.Close()
	if taskResp.StatusCode != http.StatusCreated || taskOut.UUID == "" {
		t.Fatalf("POST scheduled-task failed: %d", taskResp.StatusCode)
	}
	execTask := doJSON(t, http.MethodPost, ts.URL+"/api/v1/applications/"+appOut.UUID+"/scheduled-tasks/"+taskOut.UUID+"/execute", rootCookies, rootCSRF, "", nil)
	var taskExecOut struct {
		ExecutionUUID string `json:"execution_uuid"`
		OperationID   string `json:"operation_id"`
	}
	_ = json.NewDecoder(execTask.Body).Decode(&taskExecOut)
	execTask.Body.Close()
	if execTask.StatusCode != http.StatusAccepted || taskExecOut.OperationID == "" {
		t.Fatalf("POST scheduled-task execute failed: %d", execTask.StatusCode)
	}

	// Drain queued rollback, volume_backup, scheduled_task jobs through real Worker Runner!
	tr := &sliceTestTransport{}
	wr := worker.NewRunner(worker.Config{
		WorkerID:   "worker-slice11",
		Pool:       pool,
		Cipher:     ciph,
		JobCoord:   app.JobCoord,
		Probe:      executor.NewPostgresRemoteProbe(pool, false),
		Executor:   executor.NewClient(tr),
		LogStore:   app.LogStore,
		Outbox:     app.Outbox,
		Dispatcher: app.Dispatcher,
		LeaseTTL:   5 * time.Second,
	})
	for i := 0; i < 6; i++ {
		worked, rErr := wr.ProcessOneJob(ctx)
		if rErr != nil {
			t.Fatalf("worker ProcessOneJob failed: %v", rErr)
		}
		if !worked {
			break
		}
	}
	_ = rc
	var volState, taskState string
	_ = pool.QueryRow(ctx, `SELECT status FROM scheduled_volume_backup_executions WHERE uuid = $1`, volRunOut.ExecutionUUID).Scan(&volState)
	_ = pool.QueryRow(ctx, `SELECT status FROM scheduled_task_executions WHERE uuid = $1`, taskExecOut.ExecutionUUID).Scan(&taskState)
	if volState != "success" || taskState != "success" {
		t.Fatalf("expected worker to mark volume_backup=%q & scheduled_task=%q as success", volState, taskState)
	}

	// Verify /download/volume-backup streams real file bytes and denies cross-team user
	dlFile := filepath.Join(t.TempDir(), "vol-backup.tar.gz")
	_ = os.WriteFile(dlFile, []byte("FAKE_GZIP_ARCHIVE"), 0o644)
	_, _ = pool.Exec(ctx, `UPDATE scheduled_volume_backup_executions SET filename = $1 WHERE uuid = $2`, dlFile, volRunOut.ExecutionUUID)

	deniedDL := doJSON(t, http.MethodGet, ts.URL+"/download/volume-backup/"+volRunOut.ExecutionUUID, otherCookies, "", "", nil)
	deniedDL.Body.Close()
	if deniedDL.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 cross-team volume backup download, got %d", deniedDL.StatusCode)
	}
	okDL := doJSON(t, http.MethodGet, ts.URL+"/download/volume-backup/"+volRunOut.ExecutionUUID, rootCookies, "", "", nil)
	dlBytes, _ := io.ReadAll(okDL.Body)
	okDL.Body.Close()
	if okDL.StatusCode != http.StatusOK || string(dlBytes) != "FAKE_GZIP_ARCHIVE" {
		t.Fatalf("GET /download/volume-backup failed: %d %q", okDL.StatusCode, string(dlBytes))
	}

	// Database clone + backup execution delete + upload import + service sub-resources
	dbResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/databases/postgresql", rootCookies, rootCSRF, "", map[string]any{
		"name": "s11-pg", "environment_uuid": env2Out.UUID, "server_uuid": "localhost", "postgres_password": "pgpass",
	})
	var dbOut struct{ UUID string `json:"uuid"` }
	_ = json.NewDecoder(dbResp.Body).Decode(&dbOut)
	dbResp.Body.Close()

	cloneDB := doJSON(t, http.MethodPost, ts.URL+"/api/v1/databases/"+dbOut.UUID+"/clone", rootCookies, rootCSRF, "", map[string]string{"name": "s11-pg-clone"})
	cloneDB.Body.Close()
	if cloneDB.StatusCode != http.StatusCreated {
		t.Fatalf("POST /databases/clone failed: %d", cloneDB.StatusCode)
	}

	reqUpload, _ := http.NewRequest(http.MethodPost, ts.URL+"/upload/backup/"+dbOut.UUID+"?filename=dump.sql", bytes.NewBufferString("SELECT 1;"))
	for _, c := range rootCookies {
		reqUpload.AddCookie(c)
	}
	reqUpload.Header.Set("X-CSRF-TOKEN", rootCSRF)
	upResp, err := http.DefaultClient.Do(reqUpload)
	if err != nil {
		t.Fatal(err)
	}
	upBody, _ := io.ReadAll(upResp.Body)
	upResp.Body.Close()
	if upResp.StatusCode != http.StatusOK || !strings.Contains(string(upBody), "activity_id") {
		t.Fatalf("POST /upload/backup failed: %d %s", upResp.StatusCode, string(upBody))
	}

	// Service + service sub-component
	svcResp := doJSON(t, http.MethodPost, ts.URL+"/api/v1/services", rootCookies, rootCSRF, "", map[string]any{
		"name": "s11-svc", "environment_uuid": env2Out.UUID, "server_uuid": "localhost",
		"docker_compose_raw": "services:\n  web:\n    image: nginx:alpine\n",
	})
	var svcOut struct {
		ID   int64  `json:"id"`
		UUID string `json:"uuid"`
	}
	_ = json.NewDecoder(svcResp.Body).Decode(&svcOut)
	svcResp.Body.Close()
	_, _ = pool.Exec(ctx, `INSERT INTO service_applications (uuid, name, status, service_id) VALUES ('svcapp-1', 'web', 'exited', $1)`, svcOut.ID)

	svcAppGet := doJSON(t, http.MethodGet, ts.URL+"/api/v1/services/"+svcOut.UUID+"/applications/svcapp-1", rootCookies, "", "", nil)
	svcAppGet.Body.Close()
	if svcAppGet.StatusCode != http.StatusOK {
		t.Fatalf("GET /services/{uuid}/applications/{app_uuid} failed: %d", svcAppGet.StatusCode)
	}
	svcEnvResp := doJSON(t, http.MethodPatch, ts.URL+"/api/v1/services/"+svcOut.UUID+"/envs/bulk", rootCookies, rootCSRF, "", map[string]any{
		"data": []map[string]any{{"key": "SVC_SECRET", "value": "val123"}},
	})
	svcEnvResp.Body.Close()
	if svcEnvResp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH /services/{uuid}/envs/bulk failed: %d", svcEnvResp.StatusCode)
	}

	_ = realtime.DomainEvent{}
}
