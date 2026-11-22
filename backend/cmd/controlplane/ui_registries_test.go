package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/executor"
)

type registryTransport struct {
	requests []executor.ExecutionRequest
	code     int
}

func (tr *registryTransport) StreamNDJSON(_ context.Context, req executor.ExecutionRequest, emit func(executor.ExecutionEvent) error) (int, error) {
	tr.requests = append(tr.requests, req)
	code := tr.code
	return code, emit(executor.ExecutionEvent{ProtocolVersion: executor.ProtocolVersionV1, EventType: "exited", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 1, ExitCode: &code})
}

func TestUIRegistriesRequireConfirmedRemoteExecution(t *testing.T) {
	app, ts, pool, _, _ := setupRealControlPlane(t)
	tr := &registryTransport{code: 1}
	app.Executor = executor.NewClient(tr)
	cookies, csrf := loginClient(t, ts.URL, "root@coolify.test", "RootSecret123!")
	endpoint := ts.URL + "/api/v1/servers/localhost/registries"
	payload := map[string]string{"registry": "ghcr.io", "username": "user'$(touch /tmp/injected)", "password": "registry-private-password"}
	response := doJSON(t, http.MethodPost, endpoint, cookies, csrf, "", payload)
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed remote login returned %d", response.StatusCode)
	}
	var count int
	pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM go_server_registries`).Scan(&count)
	if count != 0 {
		t.Fatal("failed login saved verified credentials")
	}
	tr.code = 0
	response = doJSON(t, http.MethodPost, endpoint, cookies, csrf, "", payload)
	var saved struct {
		UUID string `json:"uuid"`
	}
	json.NewDecoder(response.Body).Decode(&saved)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated || saved.UUID == "" {
		t.Fatalf("confirmed login returned %d", response.StatusCode)
	}
	request := tr.requests[len(tr.requests)-1]
	if request.ExpectedCapability != "ssh_exec" || request.Operation.Host == "" || request.StdinData != base64.StdEncoding.EncodeToString([]byte(payload["password"]))+"\n" || strings.Contains(request.Operation.Command, payload["password"]) || !strings.Contains(request.Operation.Command, "--password-stdin") {
		t.Fatal("login did not securely target the managed server")
	}
	if !strings.Contains(request.Operation.Command, `'user'\''$(touch /tmp/injected)'`) {
		t.Fatal("username not shell-quoted")
	}
	tr.code = 1
	response = doJSON(t, http.MethodPost, endpoint+"/"+saved.UUID+"/check", cookies, csrf, "", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed recheck returned %d", response.StatusCode)
	}
	var verified bool
	pool.QueryRow(context.Background(), `SELECT verified FROM go_server_registries WHERE uuid=$1`, saved.UUID).Scan(&verified)
	if verified {
		t.Fatal("failed recheck retained verified status")
	}
	response = doJSON(t, http.MethodDelete, endpoint+"/"+saved.UUID, cookies, csrf, "", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("failed logout returned %d", response.StatusCode)
	}
	pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM go_server_registries WHERE uuid=$1`, saved.UUID).Scan(&count)
	if count != 1 {
		t.Fatal("failed logout discarded credentials")
	}
	tr.code = 0
	response = doJSON(t, http.MethodDelete, endpoint+"/"+saved.UUID, cookies, csrf, "", nil)
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("confirmed logout returned %d", response.StatusCode)
	}
	if !strings.HasPrefix(tr.requests[len(tr.requests)-1].Operation.Command, "docker logout ") {
		t.Fatal("logout not executed")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE servers SET "user"='deploy' WHERE uuid='localhost'`); err != nil {
		t.Fatal(err)
	}
	response = doJSON(t, http.MethodPost, endpoint, cookies, csrf, "", payload)
	json.NewDecoder(response.Body).Decode(&saved)
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("non-root login returned %d", response.StatusCode)
	}
	request = tr.requests[len(tr.requests)-1]
	if request.Operation.User != "deploy" || !request.Operation.UseSudo {
		t.Fatal("non-root SSH login omitted sudo")
	}
	if _, err := pool.Exec(context.Background(), `UPDATE servers SET transferred_at=NOW() WHERE uuid='localhost'; INSERT INTO teams(id,name) VALUES(32,'Other registry team'); INSERT INTO servers(uuid,name,ip,team_id) VALUES('other-registry-host','Other host','192.0.2.5',32)`); err != nil {
		t.Fatal(err)
	}
	calls := len(tr.requests)
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		url := endpoint
		if method == http.MethodDelete {
			url += "/" + saved.UUID
		}
		response = doJSON(t, method, url, cookies, csrf, "", payload)
		response.Body.Close()
		if response.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("transferred server %s returned %d", method, response.StatusCode)
		}
	}
	response = doJSON(t, http.MethodPost, ts.URL+"/api/v1/servers/other-registry-host/registries", cookies, csrf, "", payload)
	response.Body.Close()
	if response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-team login returned %d", response.StatusCode)
	}
	if len(tr.requests) != calls {
		t.Fatal("unauthorized or transferred server executed a command")
	}
}
