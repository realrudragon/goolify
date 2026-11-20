package controlplane

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/ops"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/jackc/pgx/v5"
)

func (a *App) registerSlice14Routes() {
	// Image scan policies & CVE ignore rules
	a.mux.HandleFunc("/api/v1/security/image-scan-policy", a.requireAPIAuth(a.handleImageScanPolicy))
	a.mux.HandleFunc("/api/v1/security/image-scan-ignores", a.requireAPIAuth(a.handleImageScanIgnores))
	a.mux.HandleFunc("/api/v1/security/image-scan-ignores/", a.requireAPIAuth(a.handleImageScanIgnores))

	// User-scoped stars & changelog reads
	a.mux.HandleFunc("/api/v1/stars", a.requireAPIAuth(a.handleStars))
	a.mux.HandleFunc("/api/v1/stars/", a.requireAPIAuth(a.handleStars))
	a.mux.HandleFunc("/api/v1/changelog/reads", a.requireAPIAuth(a.handleChangelogReads))

	// Team subscription status (Coolify Cloud / Stripe)
	a.mux.HandleFunc("/api/v1/subscription", a.requireAPIAuth(a.handleSubscription))

	// OpenAPI 3.0.3 JSON spec & Fleet Metrics aggregation (real PostgreSQL)
	a.mux.HandleFunc("/api/v1/openapi.json", a.handleOpenAPISpec)
	a.mux.HandleFunc("/api/v1/fleet/metrics", a.requireAPIAuth(a.handleFleetMetrics))

	// Realtime connectivity check endpoint (routes/web.php:160)
	a.mux.HandleFunc("/realtime", a.requireSession(func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		teamID := activeTeamForPrincipal(p)
		writeJSON(w, http.StatusOK, map[string]any{
			"status":    "ok",
			"transport": "sse",
			"channel":   fmt.Sprintf("team.%d", teamID),
			"user_id":   p.UserID,
		})
	}))

	// Dev-only error preview routes (routes/web.php: strictly 404 outside local/testing)
	a.mux.HandleFunc("/__livewire-request-failure", a.handleDevErrorRoute)
	a.mux.HandleFunc("/__exception", a.handleDevErrorRoute)
	a.mux.HandleFunc("/__error/", a.handleDevErrorRoute)

	// Catch-all JSON 404 for unknown /api/v1/* paths (RT-API-0348)
	a.mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	})
}

func (a *App) handleOpenAPISpec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	b, err := ops.GenerateOpenAPISpec("4.0.0-standalone")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b)
}

func (a *App) handleFleetMetrics(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	teamID := activeTeamForPrincipal(p)
	if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
		Type: "server", TeamID: &teamID, APISegment: "servers",
	}); err != nil {
		writeDomainError(w, err)
		return
	}
	summary, err := ops.AggregateFleetMetrics(r.Context(), a.Pool, teamID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, summary)
}

// ---- Dev-only error preview routes (matches routes/web.php environment guard) ----

func (a *App) handleDevErrorRoute(w http.ResponseWriter, r *http.Request) {
	env := strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV")))
	if env != "local" && env != "testing" {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	if r.URL.Path == "/__exception" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "Testing exception page"})
		return
	}
	if r.URL.Path == "/__livewire-request-failure" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Request failure preview"})
		return
	}
	codeStr := strings.Trim(strings.TrimPrefix(r.URL.Path, "/__error"), "/")
	allowed := map[string]int{
		"400": 400, "401": 401, "402": 402, "403": 403, "404": 404, "419": 419, "429": 429, "500": 500, "503": 503,
	}
	code, ok := allowed[codeStr]
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeJSON(w, code, map[string]any{"status": code, "message": fmt.Sprintf("Preview error %d", code)})
}

// ---- Server Extended Sub-Routes: github-runners, certificates, swarm-dockers, host-scan ----

func (a *App) handleSlice14ServerSubRoute(w http.ResponseWriter, r *http.Request, p auth.Principal, m slice12ServerMeta, sub string, rest []string) bool {
	switch sub {
	case "github-runners":
		a.handleServerGithubRunners(w, r, p, m, rest)
		return true
	case "certificates":
		a.handleServerCertificates(w, r, p, m, rest)
		return true
	case "swarm-dockers":
		a.handleServerSwarmDockers(w, r, p, m, rest)
		return true
	case "host-scan":
		a.handleServerHostScan(w, r, p, m)
		return true
	}
	return false
}

// ---- GitHub Self-Hosted Runners (github_runner_configs, github_runner_executions) ----

func (a *App) handleServerGithubRunners(w http.ResponseWriter, r *http.Request, p auth.Principal, m slice12ServerMeta, rest []string) {
	if len(rest) == 1 && rest[0] == "executions" && r.Method == http.MethodGet {
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		rows, err := a.Pool.Query(r.Context(), `
			SELECT id, uuid, github_app_id, status, trigger_workflow_job_id, COALESCE(workflow_name,''),
			       COALESCE(job_name,''), COALESCE(repository_full_name,''), COALESCE(runner_name,''),
			       COALESCE(conclusion,''), COALESCE(error_message,''), provision_attempts, created_at
			FROM github_runner_executions
			WHERE server_id = $1
			ORDER BY id DESC LIMIT 50
		`, m.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id, appID, trigID int64
			var attempts int
			var u, st, wf, job, repo, rname, concl, emsg string
			var ca time.Time
			if rows.Scan(&id, &u, &appID, &st, &trigID, &wf, &job, &repo, &rname, &concl, &emsg, &attempts, &ca) == nil {
				out = append(out, map[string]any{
					"id": id, "uuid": u, "github_app_id": appID, "status": st,
					"trigger_workflow_job_id": trigID, "workflow_name": wf, "job_name": job,
					"repository_full_name": repo, "runner_name": rname, "conclusion": concl,
					"error_message": emsg, "provision_attempts": attempts, "created_at": ca,
				})
			}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var id, ghAppID int64
		var u, dockerMode, runnerImg string
		var labelsRaw []byte
		var enabled, dedicated, allowPR bool
		var maxRunners int
		err := a.Pool.QueryRow(r.Context(), `
			SELECT id, uuid, github_app_id, labels, is_enabled, is_dedicated, allow_pull_requests,
			       max_runners, docker_mode, COALESCE(runner_image,'')
			FROM github_runner_configs WHERE server_id = $1
		`, m.ID).Scan(&id, &u, &ghAppID, &labelsRaw, &enabled, &dedicated, &allowPR, &maxRunners, &dockerMode, &runnerImg)
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, map[string]any{"configured": false, "server_uuid": m.UUID})
			return
		}
		var labels any
		_ = json.Unmarshal(labelsRaw, &labels)
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": true, "id": id, "uuid": u, "server_id": m.ID, "github_app_id": ghAppID,
			"labels": labels, "is_enabled": enabled, "is_dedicated": dedicated,
			"allow_pull_requests": allowPR, "max_runners": maxRunners, "docker_mode": dockerMode, "runner_image": runnerImg,
		})

	case http.MethodPost, http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if m.TransferredAt != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "server has been transferred away"})
			return
		}
		var body struct {
			GithubAppUUID     string   `json:"github_app_uuid"`
			GithubAppID       *int64   `json:"github_app_id"`
			Labels            []string `json:"labels"`
			IsEnabled         *bool    `json:"is_enabled"`
			IsDedicated       *bool    `json:"is_dedicated"`
			AllowPullRequests *bool    `json:"allow_pull_requests"`
			MaxRunners        *int     `json:"max_runners"`
			DockerMode        string   `json:"docker_mode"`
			RunnerImage       string   `json:"runner_image"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid runner config payload"})
			return
		}
		// Resolve GitHub App strictly scoped to the server's team (or system-wide)
		var ghAppID int64
		var err error
		if body.GithubAppUUID != "" {
			err = a.Pool.QueryRow(r.Context(), `
				SELECT id FROM github_apps WHERE uuid = $1 AND (team_id = $2 OR is_system_wide = TRUE)
			`, body.GithubAppUUID, m.TeamID).Scan(&ghAppID)
		} else if body.GithubAppID != nil {
			err = a.Pool.QueryRow(r.Context(), `
				SELECT id FROM github_apps WHERE id = $1 AND (team_id = $2 OR is_system_wide = TRUE)
			`, *body.GithubAppID, m.TeamID).Scan(&ghAppID)
		} else {
			err = a.Pool.QueryRow(r.Context(), `SELECT github_app_id FROM github_runner_configs WHERE server_id = $1`, m.ID).Scan(&ghAppID)
		}
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "github_app not found in server team"})
			return
		}
		labelsJSON, _ := json.Marshal(body.Labels)
		if body.Labels == nil {
			labelsJSON = []byte(`["self-hosted","coolify"]`)
		}
		enabled := true
		if body.IsEnabled != nil {
			enabled = *body.IsEnabled
		}
		dedicated := false
		if body.IsDedicated != nil {
			dedicated = *body.IsDedicated
		}
		allowPR := false
		if body.AllowPullRequests != nil {
			allowPR = *body.AllowPullRequests
		}
		maxRunners := 2
		if body.MaxRunners != nil && *body.MaxRunners > 0 {
			maxRunners = *body.MaxRunners
		}
		dockerMode := body.DockerMode
		if dockerMode == "" {
			dockerMode = "none"
		}
		u := a.Hooks.NewCUID2()
		var id int64
		var outUUID string
		err = a.Pool.QueryRow(r.Context(), `
			INSERT INTO github_runner_configs (
				uuid, server_id, github_app_id, labels, is_enabled, is_dedicated, allow_pull_requests,
				max_runners, docker_mode, runner_image
			) VALUES ($1,$2,$3,$4::jsonb,$5,$6,$7,$8,$9,NULLIF($10,''))
			ON CONFLICT (server_id) DO UPDATE SET
				github_app_id = EXCLUDED.github_app_id,
				labels = EXCLUDED.labels,
				is_enabled = EXCLUDED.is_enabled,
				is_dedicated = EXCLUDED.is_dedicated,
				allow_pull_requests = EXCLUDED.allow_pull_requests,
				max_runners = EXCLUDED.max_runners,
				docker_mode = EXCLUDED.docker_mode,
				runner_image = COALESCE(EXCLUDED.runner_image, github_runner_configs.runner_image),
				updated_at = NOW()
			RETURNING id, uuid
		`, u, m.ID, ghAppID, string(labelsJSON), enabled, dedicated, allowPR, maxRunners, dockerMode, body.RunnerImage).Scan(&id, &outUUID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "uuid": outUUID, "server_id": m.ID, "github_app_id": ghAppID, "is_enabled": enabled})

	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM github_runner_configs WHERE server_id = $1`, m.ID)
		writeJSON(w, http.StatusOK, map[string]string{"message": "GitHub runner config deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- SSL Certificates (ssl_certificates with APP_KEY encryption) ----

func (a *App) handleServerCertificates(w http.ResponseWriter, r *http.Request, p auth.Principal, m slice12ServerMeta, rest []string) {
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		rows, err := a.Pool.Query(r.Context(), `
			SELECT id, common_name, COALESCE(configuration_dir,''), COALESCE(mount_path,''),
			       valid_until, is_ca_certificate, created_at
			FROM ssl_certificates WHERE server_id = $1 ORDER BY id DESC
		`, m.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id int64
			var cn, cfgDir, mpath string
			var validUntil, createdAt time.Time
			var isCA bool
			if rows.Scan(&id, &cn, &cfgDir, &mpath, &validUntil, &isCA, &createdAt) == nil {
				out = append(out, map[string]any{
					"id": id, "server_id": m.ID, "common_name": cn, "configuration_dir": cfgDir,
					"mount_path": mpath, "valid_until": validUntil, "is_ca_certificate": isCA,
					"private_key_redacted": true, "created_at": createdAt,
				})
			}
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if m.TransferredAt != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "server has been transferred away"})
			return
		}
		var body struct {
			CommonName       string   `json:"common_name"`
			SSLCertificate   string   `json:"ssl_certificate"`
			SSLPrivateKey    string   `json:"ssl_private_key"`
			ConfigurationDir string   `json:"configuration_dir"`
			MountPath        string   `json:"mount_path"`
			SANs             []string `json:"subject_alternative_names"`
			ValidUntil       string   `json:"valid_until"`
			IsCACertificate  bool     `json:"is_ca_certificate"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.CommonName) == "" || strings.TrimSpace(body.SSLCertificate) == "" || strings.TrimSpace(body.SSLPrivateKey) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "common_name, ssl_certificate, and ssl_private_key are required"})
			return
		}
		validUntil := time.Now().UTC().Add(365 * 24 * time.Hour)
		if body.ValidUntil != "" {
			if parsed, err := time.Parse(time.RFC3339, body.ValidUntil); err == nil {
				validUntil = parsed
			}
		}
		certEnc, err := a.encryptSecretString(body.SSLCertificate)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		keyEnc, err := a.encryptSecretString(body.SSLPrivateKey)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		sansJSON, _ := json.Marshal(body.SANs)
		var id int64
		err = a.Pool.QueryRow(r.Context(), `
			INSERT INTO ssl_certificates (
				ssl_certificate, ssl_private_key, configuration_dir, mount_path, server_id,
				common_name, subject_alternative_names, valid_until, is_ca_certificate
			) VALUES ($1,$2,NULLIF($3,''),NULLIF($4,''),$5,$6,$7::jsonb,$8,$9)
			RETURNING id
		`, certEnc, keyEnc, body.ConfigurationDir, body.MountPath, m.ID, compat.SanitizeString(body.CommonName), string(sansJSON), validUntil, body.IsCACertificate).Scan(&id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": id, "server_id": m.ID, "common_name": body.CommonName, "valid_until": validUntil, "is_ca_certificate": body.IsCACertificate,
		})

	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if len(rest) == 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "certificate id is required"})
			return
		}
		certID, err := strconv.ParseInt(rest[0], 10, 64)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid certificate id"})
			return
		}
		tag, _ := a.Pool.Exec(r.Context(), `DELETE FROM ssl_certificates WHERE id = $1 AND server_id = $2`, certID, m.ID)
		if tag.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "certificate not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "SSL certificate deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Swarm Docker Destinations (swarm_dockers) ----

func (a *App) handleServerSwarmDockers(w http.ResponseWriter, r *http.Request, p auth.Principal, m slice12ServerMeta, rest []string) {
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		rows, err := a.Pool.Query(r.Context(), `SELECT id, uuid, name, server_id, created_at FROM swarm_dockers WHERE server_id = $1 ORDER BY id ASC`, m.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id, srvID int64
			var u, name string
			var ca time.Time
			if rows.Scan(&id, &u, &name, &srvID, &ca) == nil {
				out = append(out, map[string]any{"id": id, "uuid": u, "name": name, "server_id": srvID, "type": "swarm", "created_at": ca})
			}
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if m.TransferredAt != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "server has been transferred away"})
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "name is required"})
			return
		}
		u := a.Hooks.NewCUID2()
		var id int64
		err := a.Pool.QueryRow(r.Context(), `
			INSERT INTO swarm_dockers (uuid, name, server_id) VALUES ($1, $2, $3) RETURNING id
		`, u, compat.SanitizeString(body.Name), m.ID).Scan(&id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "uuid": u, "name": body.Name, "server_id": m.ID, "type": "swarm"})

	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if len(rest) == 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "swarm docker uuid is required"})
			return
		}
		tag, _ := a.Pool.Exec(r.Context(), `DELETE FROM swarm_dockers WHERE uuid = $1 AND server_id = $2`, rest[0], m.ID)
		if tag.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "swarm destination not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Swarm destination deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Server Host Security Scan (server_host_scans & image_scan_notification_states) ----

func (a *App) handleServerHostScan(w http.ResponseWriter, r *http.Request, p auth.Principal, m slice12ServerMeta) {
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var id int64
		var errCode *string
		var summaryRaw, findingsRaw, eolRaw []byte
		var scannedAt *time.Time
		err := a.Pool.QueryRow(r.Context(), `
			SELECT id, error_code, COALESCE(summary,'{}'::jsonb), COALESCE(findings,'[]'::jsonb),
			       COALESCE(eol,'{}'::jsonb), scanned_at
			FROM server_host_scans WHERE server_id = $1
		`, m.ID).Scan(&id, &errCode, &summaryRaw, &findingsRaw, &eolRaw, &scannedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, map[string]any{"server_uuid": m.UUID, "scanned": false})
			return
		}
		var summary, findings, eol any
		_ = json.Unmarshal(summaryRaw, &summary)
		_ = json.Unmarshal(findingsRaw, &findings)
		_ = json.Unmarshal(eolRaw, &eol)
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "server_uuid": m.UUID, "scanned": true, "error_code": errCode,
			"summary": summary, "findings": findings, "eol": eol, "scanned_at": scannedAt,
		})

	case http.MethodPost:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if m.TransferredAt != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "server has been transferred away"})
			return
		}
		var body struct {
			Summary  map[string]any   `json:"summary"`
			Findings []map[string]any `json:"findings"`
			EOL      map[string]any   `json:"eol"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Summary == nil {
			body.Summary = map[string]any{"critical": 0, "high": 0, "medium": 0, "low": 0}
		}
		if body.Findings == nil {
			body.Findings = []map[string]any{}
		}
		if body.EOL == nil {
			body.EOL = map[string]any{"status": "supported"}
		}
		sumJSON, _ := json.Marshal(body.Summary)
		findJSON, _ := json.Marshal(body.Findings)
		eolJSON, _ := json.Marshal(body.EOL)
		var id int64
		err := a.Pool.QueryRow(r.Context(), `
			INSERT INTO server_host_scans (server_id, summary, findings, eol, scanned_at, updated_at)
			VALUES ($1, $2::jsonb, $3::jsonb, $4::jsonb, NOW(), NOW())
			ON CONFLICT (server_id) DO UPDATE SET
				summary = EXCLUDED.summary,
				findings = EXCLUDED.findings,
				eol = EXCLUDED.eol,
				scanned_at = NOW(),
				updated_at = NOW()
			RETURNING id
		`, m.ID, string(sumJSON), string(findJSON), string(eolJSON)).Scan(&id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		// Maintain notification diff state in image_scan_notification_states
		var cveIDs []string
		for _, f := range body.Findings {
			if cve, ok := f["id"].(string); ok && cve != "" {
				cveIDs = append(cveIDs, cve)
			}
		}
		cveJSON, _ := json.Marshal(cveIDs)
		_, _ = a.Pool.Exec(r.Context(), `
			INSERT INTO image_scan_notification_states (resource_type, resource_id, team_id, notified_ids, is_baseline, check_reported, updated_at)
			VALUES ('App\Models\Server', $1, $2, $3::jsonb, FALSE, TRUE, NOW())
			ON CONFLICT (resource_type, resource_id) DO UPDATE SET
				notified_ids = EXCLUDED.notified_ids,
				check_reported = TRUE,
				updated_at = NOW()
		`, m.ID, m.TeamID, string(cveJSON))

		writeJSON(w, http.StatusOK, map[string]any{"id": id, "server_uuid": m.UUID, "summary": body.Summary, "findings_count": len(body.Findings)})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Image Scan Policies & Ignores (image_scan_policies, image_scan_ignores) ----

func (a *App) handleImageScanPolicy(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	teamID := activeTeamForPrincipal(p)
	scopeType := "team"
	scopeID := teamID
	ref := access.ResourceRef{Type: "team", ID: teamID, TeamID: &teamID, APISegment: "security"}

	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var id int64
		var u, gateMode, onErr string
		var countMed bool
		err := a.Pool.QueryRow(r.Context(), `
			SELECT id, uuid, COALESCE(gate_mode,'warn'), COALESCE(on_scan_error,'allow'), COALESCE(count_medium,FALSE)
			FROM image_scan_policies WHERE scope_type = $1 AND scope_id = $2
		`, scopeType, scopeID).Scan(&id, &u, &gateMode, &onErr, &countMed)
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusOK, map[string]any{"scope_type": scopeType, "scope_id": scopeID, "gate_mode": "warn", "on_scan_error": "allow", "count_medium": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "uuid": u, "scope_type": scopeType, "scope_id": scopeID, "gate_mode": gateMode, "on_scan_error": onErr, "count_medium": countMed})

	case http.MethodPatch, http.MethodPut:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			GateMode    string `json:"gate_mode"`
			OnScanError string `json:"on_scan_error"`
			CountMedium bool   `json:"count_medium"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid policy payload"})
			return
		}
		if body.GateMode == "" {
			body.GateMode = "warn"
		}
		if body.OnScanError == "" {
			body.OnScanError = "allow"
		}
		u := a.Hooks.NewCUID2()
		var id int64
		var outUUID string
		err := a.Pool.QueryRow(r.Context(), `
			INSERT INTO image_scan_policies (uuid, scope_type, scope_id, gate_mode, on_scan_error, count_medium)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (scope_type, scope_id) DO UPDATE SET
				gate_mode = EXCLUDED.gate_mode,
				on_scan_error = EXCLUDED.on_scan_error,
				count_medium = EXCLUDED.count_medium,
				updated_at = NOW()
			RETURNING id, uuid
		`, u, scopeType, scopeID, body.GateMode, body.OnScanError, body.CountMedium).Scan(&id, &outUUID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "uuid": outUUID, "gate_mode": body.GateMode, "on_scan_error": body.OnScanError, "count_medium": body.CountMedium})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (a *App) handleImageScanIgnores(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	teamID := activeTeamForPrincipal(p)
	ref := access.ResourceRef{Type: "team", ID: teamID, TeamID: &teamID, APISegment: "security"}
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/security/image-scan-ignores"), "/")

	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		rows, err := a.Pool.Query(r.Context(), `
			SELECT id, uuid, COALESCE(vulnerability_id,''), COALESCE(package,''), reason, expires_at, created_at
			FROM image_scan_ignores WHERE team_id = $1 ORDER BY id DESC
		`, teamID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id int64
			var u, vid, pkg, reason string
			var exp *time.Time
			var ca time.Time
			if rows.Scan(&id, &u, &vid, &pkg, &reason, &exp, &ca) == nil {
				out = append(out, map[string]any{"id": id, "uuid": u, "vulnerability_id": vid, "package": pkg, "reason": reason, "expires_at": exp, "created_at": ca})
			}
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			VulnerabilityID string `json:"vulnerability_id"`
			Package         string `json:"package"`
			Reason          string `json:"reason"`
			ExpiresAt       string `json:"expires_at"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Reason) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "reason is required"})
			return
		}
		var exp *time.Time
		if body.ExpiresAt != "" {
			if t, err := time.Parse(time.RFC3339, body.ExpiresAt); err == nil {
				exp = &t
			}
		}
		u := a.Hooks.NewCUID2()
		var id int64
		err := a.Pool.QueryRow(r.Context(), `
			INSERT INTO image_scan_ignores (uuid, team_id, vulnerability_id, package, reason, expires_at, created_by)
			VALUES ($1, $2, NULLIF($3,''), NULLIF($4,''), $5, $6, $7)
			RETURNING id
		`, u, teamID, body.VulnerabilityID, body.Package, body.Reason, exp, p.UserID).Scan(&id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "uuid": u, "vulnerability_id": body.VulnerabilityID, "package": body.Package, "reason": body.Reason})

	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if trimmed == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "ignore uuid is required"})
			return
		}
		tag, _ := a.Pool.Exec(r.Context(), `DELETE FROM image_scan_ignores WHERE uuid = $1 AND team_id = $2`, trimmed, teamID)
		if tag.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "ignore rule not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Ignore rule deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Project Settings (project_settings) ----

func (a *App) handleProjectSettings(w http.ResponseWriter, r *http.Request, p auth.Principal, projUUID string) {
	proj, _, err := a.ProjectSvc.GetProject(r.Context(), p, projUUID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	teamID := proj.TeamID
	ref := access.ResourceRef{Type: "project", ID: proj.ID, UUID: proj.UUID, ProjectUUID: proj.UUID, TeamID: &teamID, APISegment: "projects"}
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var wd *string
		_ = a.Pool.QueryRow(r.Context(), `SELECT wildcard_domain FROM project_settings WHERE project_id = $1`, proj.ID).Scan(&wd)
		writeJSON(w, http.StatusOK, map[string]any{"project_id": proj.ID, "project_uuid": proj.UUID, "wildcard_domain": wd})

	case http.MethodPatch, http.MethodPut:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			WildcardDomain *string `json:"wildcard_domain"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid project settings payload"})
			return
		}
		var existingID int64
		err := a.Pool.QueryRow(r.Context(), `SELECT id FROM project_settings WHERE project_id = $1`, proj.ID).Scan(&existingID)
		if errors.Is(err, pgx.ErrNoRows) {
			_, err = a.Pool.Exec(r.Context(), `INSERT INTO project_settings (project_id, wildcard_domain) VALUES ($1, $2)`, proj.ID, body.WildcardDomain)
		} else if err == nil {
			_, err = a.Pool.Exec(r.Context(), `UPDATE project_settings SET wildcard_domain = $1, updated_at = NOW() WHERE id = $2`, body.WildcardDomain, existingID)
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"project_id": proj.ID, "project_uuid": proj.UUID, "wildcard_domain": body.WildcardDomain})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Stars (stars) ----

func (a *App) handleStars(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/stars"), "/")
	switch r.Method {
	case http.MethodGet:
		rows, err := a.Pool.Query(r.Context(), `
			SELECT id, starrable_type, starrable_id, position, created_at
			FROM stars WHERE user_id = $1 ORDER BY position ASC, id ASC
		`, p.UserID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id, sid int64
			var stype string
			var pos int
			var ca time.Time
			if rows.Scan(&id, &stype, &sid, &pos, &ca) == nil {
				out = append(out, map[string]any{"id": id, "starrable_type": stype, "starrable_id": sid, "position": pos, "created_at": ca})
			}
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		var body struct {
			StarrableType string `json:"starrable_type"`
			StarrableID   int64  `json:"starrable_id"`
			Position      int    `json:"position"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.StarrableType == "" || body.StarrableID < 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "starrable_type and starrable_id are required"})
			return
		}
		// Verify caller belongs to the target resource's team before starring
		var targetTeam int64
		var checkErr error
		switch strings.ToLower(body.StarrableType) {
		case "project", `app\models\project`:
			checkErr = a.Pool.QueryRow(r.Context(), `SELECT team_id FROM projects WHERE id = $1`, body.StarrableID).Scan(&targetTeam)
		case "server", `app\models\server`:
			checkErr = a.Pool.QueryRow(r.Context(), `SELECT team_id FROM servers WHERE id = $1 AND deleted_at IS NULL`, body.StarrableID).Scan(&targetTeam)
		case "application", `app\models\application`:
			checkErr = a.Pool.QueryRow(r.Context(), `SELECT p.team_id FROM applications a JOIN environments e ON e.id = a.environment_id JOIN projects p ON p.id = e.project_id WHERE a.id = $1`, body.StarrableID).Scan(&targetTeam)
		default:
			targetTeam = activeTeamForPrincipal(p)
		}
		if checkErr != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "starrable resource not found"})
			return
		}
		if _, ok := p.TeamRoles[targetTeam]; !ok {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "forbidden"})
			return
		}
		var id int64
		err := a.Pool.QueryRow(r.Context(), `
			INSERT INTO stars (user_id, starrable_type, starrable_id, position)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id, starrable_type, starrable_id) DO UPDATE SET position = EXCLUDED.position, updated_at = NOW()
			RETURNING id
		`, p.UserID, body.StarrableType, body.StarrableID, body.Position).Scan(&id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "starrable_type": body.StarrableType, "starrable_id": body.StarrableID, "position": body.Position})

	case http.MethodDelete:
		starID, err := strconv.ParseInt(trimmed, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "star id is required"})
			return
		}
		tag, _ := a.Pool.Exec(r.Context(), `DELETE FROM stars WHERE id = $1 AND user_id = $2`, starID, p.UserID)
		if tag.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "star not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Star removed."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- User Changelog Reads (user_changelog_reads) ----

func (a *App) handleChangelogReads(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	switch r.Method {
	case http.MethodGet:
		rows, err := a.Pool.Query(r.Context(), `SELECT release_tag, read_at FROM user_changelog_reads WHERE user_id = $1 ORDER BY read_at DESC`, p.UserID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var tag string
			var ra time.Time
			if rows.Scan(&tag, &ra) == nil {
				out = append(out, map[string]any{"release_tag": tag, "read_at": ra})
			}
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		var body struct {
			ReleaseTag string `json:"release_tag"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ReleaseTag) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "release_tag is required"})
			return
		}
		_, err := a.Pool.Exec(r.Context(), `
			INSERT INTO user_changelog_reads (user_id, release_tag, read_at)
			VALUES ($1, $2, NOW())
			ON CONFLICT (user_id, release_tag) DO UPDATE SET read_at = NOW(), updated_at = NOW()
		`, p.UserID, strings.TrimSpace(body.ReleaseTag))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"release_tag": strings.TrimSpace(body.ReleaseTag), "read": true})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Team Subscription (subscriptions) + Stripe Webhook Sync ----

func (a *App) handleSubscription(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	teamID := activeTeamForPrincipal(p)
	// Billing data is administrative (never exposed to read-only team members per AGENTS.md)
	if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{
		Type: "team", ID: teamID, TeamID: &teamID, APISegment: "subscription",
	}); err != nil {
		writeDomainError(w, err)
		return
	}
	var id int64
	var subID, custID, planID *string
	var paid, cancelEnd, pastDue bool
	err := a.Pool.QueryRow(r.Context(), `
		SELECT id, stripe_subscription_id, stripe_customer_id, stripe_plan_id,
		       stripe_invoice_paid, stripe_cancel_at_period_end, stripe_past_due
		FROM subscriptions WHERE team_id = $1 ORDER BY id DESC LIMIT 1
	`, teamID).Scan(&id, &subID, &custID, &planID, &paid, &cancelEnd, &pastDue)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusOK, map[string]any{"team_id": teamID, "subscribed": false, "stripe_invoice_paid": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "team_id": teamID, "subscribed": paid && !pastDue,
		"stripe_subscription_id": subID, "stripe_customer_id": custID, "stripe_plan_id": planID,
		"stripe_invoice_paid": paid, "stripe_cancel_at_period_end": cancelEnd, "stripe_past_due": pastDue,
	})
}

func (a *App) applyStripeSubscriptionEvent(r *http.Request, eventType string, data map[string]any) {
	obj, _ := data["object"].(map[string]any)
	if obj == nil {
		return
	}
	subID, _ := obj["id"].(string)
	custID, _ := obj["customer"].(string)
	meta, _ := obj["metadata"].(map[string]any)
	var teamID int64 = -1
	if meta != nil {
		if rawTeam, ok := meta["team_id"]; ok {
			if parsed, err := strconv.ParseInt(fmt.Sprint(rawTeam), 10, 64); err == nil {
				teamID = parsed
			}
		}
	}
	if teamID < 0 && subID != "" {
		_ = a.Pool.QueryRow(r.Context(), `SELECT team_id FROM subscriptions WHERE stripe_subscription_id = $1 LIMIT 1`, subID).Scan(&teamID)
	}
	if teamID < 0 {
		return
	}
	paid := eventType == "invoice.paid" || eventType == "customer.subscription.created" || eventType == "customer.subscription.updated"
	pastDue := eventType == "invoice.payment_failed"
	cancelAtEnd, _ := obj["cancel_at_period_end"].(bool)
	var existingID int64
	err := a.Pool.QueryRow(r.Context(), `SELECT id FROM subscriptions WHERE team_id = $1 LIMIT 1`, teamID).Scan(&existingID)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = a.Pool.Exec(r.Context(), `
			INSERT INTO subscriptions (team_id, stripe_subscription_id, stripe_customer_id, stripe_invoice_paid, stripe_cancel_at_period_end, stripe_past_due)
			VALUES ($1, NULLIF($2,''), NULLIF($3,''), $4, $5, $6)
		`, teamID, subID, custID, paid, cancelAtEnd, pastDue)
	} else if err == nil {
		_, _ = a.Pool.Exec(r.Context(), `
			UPDATE subscriptions
			SET stripe_subscription_id = COALESCE(NULLIF($1,''), stripe_subscription_id),
			    stripe_customer_id = COALESCE(NULLIF($2,''), stripe_customer_id),
			    stripe_invoice_paid = $3,
			    stripe_cancel_at_period_end = $4,
			    stripe_past_due = $5,
			    updated_at = NOW()
			WHERE id = $6
		`, subID, custID, paid, cancelAtEnd, pastDue, existingID)
	}
}

// ---- GitHub workflow_job Runner Dispatch Hook ----

func (a *App) tryHandleGithubRunnerWebhook(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost || strings.Trim(strings.TrimPrefix(r.URL.Path, "/webhooks"), "/") != "source/github/events" {
		return false
	}
	if strings.ToLower(strings.TrimSpace(r.Header.Get("X-GitHub-Event"))) != "workflow_job" {
		return false
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "unreadable webhook body"})
		return true
	}
	sig := strings.TrimPrefix(strings.TrimSpace(r.Header.Get("X-Hub-Signature-256")), "sha256=")
	var payload struct {
		Action       string `json:"action"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		WorkflowJob struct {
			ID           int64    `json:"id"`
			HTMLURL      string   `json:"html_url"`
			WorkflowName string   `json:"workflow_name"`
			Name         string   `json:"name"`
			Labels       []string `json:"labels"`
		} `json:"workflow_job"`
	}
	if err := json.Unmarshal(bodyBytes, &payload); err != nil || payload.WorkflowJob.ID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid workflow_job payload"})
		return true
	}
	// Find enabled runner config + verify GitHub App webhook secret HMAC
	var cfgID, serverID, ghAppID, teamID int64
	var serverUUID, runnerImage string
	var whSecretEnc *string
	err = a.Pool.QueryRow(r.Context(), `
		SELECT rc.id, rc.server_id, rc.github_app_id, s.uuid, s.team_id, COALESCE(rc.runner_image,''), ga.webhook_secret
		FROM github_runner_configs rc
		JOIN servers s ON s.id = rc.server_id
		JOIN github_apps ga ON ga.id = rc.github_app_id
		WHERE rc.is_enabled = TRUE AND s.deleted_at IS NULL AND s.transferred_at IS NULL
		  AND ($1 = 0 OR ga.installation_id = $1)
		ORDER BY rc.id ASC LIMIT 1
	`, payload.Installation.ID).Scan(&cfgID, &serverID, &ghAppID, &serverUUID, &teamID, &runnerImage, &whSecretEnc)
	if err != nil {
		writeJSON(w, http.StatusAccepted, map[string]any{"message": "No matching enabled runner config."})
		return true
	}
	if whSecretEnc != nil && *whSecretEnc != "" {
		secret := a.decryptSecretString(*whSecretEnc)
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(bodyBytes)
		expected := hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(sig), []byte(expected)) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "invalid webhook signature"})
			return true
		}
	}
	execUUID := a.Hooks.NewCUID2()
	labelsJSON, _ := json.Marshal(payload.WorkflowJob.Labels)
	runnerName := fmt.Sprintf("coolify-runner-%d", payload.WorkflowJob.ID)
	_, err = a.Pool.Exec(r.Context(), `
		INSERT INTO github_runner_executions (
			uuid, github_app_id, github_runner_config_id, server_id, status, trigger_workflow_job_id,
			workflow_job_id, workflow_job_html_url, workflow_name, job_name, repository_full_name,
			labels, runner_name, queued_at
		) VALUES ($1,$2,$3,$4,'queued',$5,$5,$6,$7,$8,$9,$10::jsonb,$11,NOW())
		ON CONFLICT (github_app_id, trigger_workflow_job_id) DO NOTHING
	`, execUUID, ghAppID, cfgID, serverID, payload.WorkflowJob.ID, payload.WorkflowJob.HTMLURL,
		payload.WorkflowJob.WorkflowName, payload.WorkflowJob.Name, payload.Repository.FullName, string(labelsJSON), runnerName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return true
	}
	opID := "gh-runner-" + execUUID
	_ = a.JobCoord.AdmitOperation(r.Context(), jobs.DurableJobSpec{
		JobOperation: jobs.JobOperation{
			JobID: "job-" + opID, OperationID: opID, AttemptID: "att-0",
			ResourceUUID: serverUUID, ServerUUID: serverUUID, TeamID: teamID, Status: jobs.StatusQueued, MaxAttempts: 1,
		},
		JobType: "github_runner_provision",
		Payload: map[string]any{
			"execution_uuid": execUUID,
			"runner_name":    runnerName,
			"runner_image":   runnerImage,
		},
	})
	writeJSON(w, http.StatusAccepted, map[string]any{"execution_uuid": execUUID, "runner_name": runnerName, "status": "queued"})
	return true
}
