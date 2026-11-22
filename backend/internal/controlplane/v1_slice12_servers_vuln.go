package controlplane

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/jackc/pgx/v5"
)

type slice12ServerMeta struct {
	ID            int64
	UUID          string
	Name          string
	IP            string
	TeamID        int64
	TransferredAt *time.Time
	Ref           access.ResourceRef
}

func (a *App) loadServerMeta(r *http.Request, srvUUID string) (slice12ServerMeta, error) {
	var m slice12ServerMeta
	err := a.Pool.QueryRow(r.Context(), `
		SELECT id, uuid, name, ip, team_id, transferred_at
		FROM servers WHERE uuid = $1 AND deleted_at IS NULL
	`, srvUUID).Scan(&m.ID, &m.UUID, &m.Name, &m.IP, &m.TeamID, &m.TransferredAt)
	if err != nil {
		return m, err
	}
	tid := m.TeamID
	m.Ref = access.ResourceRef{Type: "server", ID: m.ID, UUID: m.UUID, TeamID: &tid, APISegment: "servers"}
	return m, nil
}

func (a *App) withExtendedServerRoutes(next authedHandler) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/servers"), "/")
		parts := strings.Split(trimmed, "/")
		if trimmed == "" || len(parts) == 0 {
			next(w, r, p)
			return
		}

		// Cloud provider server creation: POST /api/v1/servers/{hetzner|vultr|digitalocean|hostinger}
		if len(parts) == 1 && r.Method == http.MethodPost {
			switch parts[0] {
			case "hetzner", "vultr", "digitalocean", "hostinger":
				a.handleCloudServerCreate(w, r, p, parts[0])
				return
			}
		}

		// Transfer preflight check: GET /api/v1/servers/import/check
		if len(parts) == 2 && parts[0] == "import" && parts[1] == "check" && r.Method == http.MethodGet {
			teamID := activeTeamForPrincipal(p)
			if err := a.Authz.Check(r.Context(), p, access.ActionTransfer, access.ResourceRef{
				Type: "server", TeamID: &teamID, APISegment: "servers",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ready": true, "team_id": teamID, "format_version": "v1"})
			return
		}

		if len(parts) < 2 {
			next(w, r, p)
			return
		}
		srvUUID := parts[0]
		sub := parts[1]

		switch sub {
		case "docker-cleanup":
			m, err := a.loadServerMeta(r, srvUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "server not found"})
				return
			}
			a.handleServerDockerCleanup(w, r, p, m, parts[2:])
			return

		case "log-drains":
			m, err := a.loadServerMeta(r, srvUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "server not found"})
				return
			}
			a.handleServerLogDrains(w, r, p, m)
			return

		case "sentinel":
			m, err := a.loadServerMeta(r, srvUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "server not found"})
				return
			}
			a.handleServerSentinel(w, r, p, m)
			return

		case "image-scanning":
			m, err := a.loadServerMeta(r, srvUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "server not found"})
				return
			}
			a.handleServerImageScanning(w, r, p, m)
			return

		case "cloudflare-tunnel":
			m, err := a.loadServerMeta(r, srvUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "server not found"})
				return
			}
			a.handleServerCloudflareTunnel(w, r, p, m, parts[2:])
			return

		case "host-key":
			if len(parts) == 3 && parts[2] == "reset" && r.Method == http.MethodPost {
				m, err := a.loadServerMeta(r, srvUUID)
				if err != nil {
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "server not found"})
					return
				}
				if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, m.Ref); err != nil {
					writeDomainError(w, err)
					return
				}
				if m.TransferredAt != nil {
					writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Server has been transferred away; remote commands are disabled."})
					return
				}
				_, _ = a.Pool.Exec(r.Context(), `UPDATE servers SET server_metadata = COALESCE(server_metadata,'{}'::jsonb) - 'ssh_host_key', updated_at = NOW() WHERE id = $1`, m.ID)
				writeJSON(w, http.StatusOK, map[string]any{"message": "SSH host key reset.", "server_uuid": m.UUID})
				return
			}

		case "registries":
			m, err := a.loadServerMeta(r, srvUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "server not found"})
				return
			}
			a.handleServerRegistries(w, r, p, m, parts[2:])
			return

		case "proxy":
			if r.Method == http.MethodPatch || len(parts) >= 3 {
				m, err := a.loadServerMeta(r, srvUUID)
				if err != nil {
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "server not found"})
					return
				}
				a.handleServerProxyExtended(w, r, p, m, parts[2:])
				return
			}

		case "transfer":
			if r.Method == http.MethodGet || (len(parts) == 3 && parts[2] == "complete" && r.Method == http.MethodPost) {
				m, err := a.loadServerMeta(r, srvUUID)
				if err != nil {
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "server not found"})
					return
				}
				a.handleServerTransferExtended(w, r, p, m, parts[2:])
				return
			}

		case "migrate":
			if r.Method == http.MethodPost {
				m, err := a.loadServerMeta(r, srvUUID)
				if err != nil {
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "server not found"})
					return
				}
				if err := a.Authz.Check(r.Context(), p, access.ActionTransfer, m.Ref); err != nil {
					writeDomainError(w, err)
					return
				}
				var body struct {
					TargetURL string `json:"target_url"`
				}
				_ = json.NewDecoder(r.Body).Decode(&body)
				_, _ = a.Pool.Exec(r.Context(), `UPDATE servers SET transfer_status = 'migrating', transferred_to_url = NULLIF($1,''), updated_at = NOW() WHERE id = $2`, strings.TrimSpace(body.TargetURL), m.ID)
				writeJSON(w, http.StatusOK, map[string]any{"message": "Server migration initiated.", "server_uuid": m.UUID, "transfer_status": "migrating"})
				return
			}
		case "github-runners", "certificates", "swarm-dockers", "host-scan":
			m, err := a.loadServerMeta(r, srvUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "server not found"})
				return
			}
			if a.handleSlice14ServerSubRoute(w, r, p, m, sub, parts[2:]) {
				return
			}
		}

		next(w, r, p)
	}
}

// ---- Docker Cleanup ----

func (a *App) handleServerDockerCleanup(w http.ResponseWriter, r *http.Request, p auth.Principal, m slice12ServerMeta, rest []string) {
	if len(rest) == 1 && rest[0] == "executions" && r.Method == http.MethodGet {
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		rows, err := a.Pool.Query(r.Context(), `
			SELECT uuid, status, COALESCE(message,''), created_at, finished_at
			FROM docker_cleanup_executions WHERE server_id = $1 ORDER BY id DESC LIMIT 50
		`, m.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var u, st, msg string
			var ca time.Time
			var fa *time.Time
			if rows.Scan(&u, &st, &msg, &ca, &fa) == nil {
				out = append(out, map[string]any{"uuid": u, "status": st, "message": msg, "created_at": ca, "finished_at": fa})
			}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	if len(rest) == 1 && rest[0] == "run" && r.Method == http.MethodPost {
		if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if m.TransferredAt != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Server has been transferred away; remote commands are disabled."})
			return
		}
		var delVols bool
		_ = a.Pool.QueryRow(r.Context(), `SELECT COALESCE(delete_unused_volumes, FALSE) FROM server_settings WHERE server_id = $1`, m.ID).Scan(&delVols)
		execUUID := a.Hooks.NewCUID2()
		var execID int64
		if err := a.Pool.QueryRow(r.Context(), `
			INSERT INTO docker_cleanup_executions (uuid, status, server_id) VALUES ($1, 'running', $2) RETURNING id
		`, execUUID, m.ID).Scan(&execID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		opID := "cleanup-" + execUUID
		tx, err := a.Pool.Begin(r.Context())
		if err == nil {
			defer func() { _ = tx.Rollback(r.Context()) }()
			_ = a.JobCoord.AdmitOperationInTx(r.Context(), tx, jobs.DurableJobSpec{
				JobOperation: jobs.JobOperation{
					JobID: "job-" + opID, OperationID: opID, AttemptID: "att-0",
					ResourceUUID: m.UUID, ServerUUID: m.UUID, TeamID: m.TeamID,
					Status: jobs.StatusQueued, MaxAttempts: 2,
				},
				JobType: "docker_cleanup",
				Payload: map[string]any{"execution_id": execID, "execution_uuid": execUUID, "delete_unused_volumes": delVols},
			})
			_ = tx.Commit(r.Context())
			_, _ = a.Dispatcher.FlushPending(r.Context())
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"message": "Docker cleanup queued.", "execution_uuid": execUUID, "operation_id": opID})
		return
	}

	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		a.serveDockerCleanupSettings(w, r, m.ID)
	case http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Frequency       *string `json:"docker_cleanup_frequency"`
			Threshold       *int    `json:"docker_cleanup_threshold"`
			ForceCleanup    *bool   `json:"force_docker_cleanup"`
			DeleteVolumes   *bool   `json:"delete_unused_volumes"`
			DeleteNetworks  *bool   `json:"delete_unused_networks"`
			DisableImageRet *bool   `json:"disable_application_image_retention"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid docker cleanup payload"})
			return
		}
		if body.DeleteVolumes != nil && *body.DeleteVolumes {
			if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, m.Ref); err != nil {
				writeDomainError(w, err)
				return
			}
		}
		if body.Threshold != nil && (*body.Threshold < 1 || *body.Threshold > 99) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "docker_cleanup_threshold must be 1..99"})
			return
		}
		if body.Frequency != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET docker_cleanup_frequency = $1 WHERE server_id = $2`, strings.TrimSpace(*body.Frequency), m.ID)
		}
		if body.Threshold != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET docker_cleanup_threshold = $1 WHERE server_id = $2`, *body.Threshold, m.ID)
		}
		if body.ForceCleanup != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET force_docker_cleanup = $1 WHERE server_id = $2`, *body.ForceCleanup, m.ID)
		}
		if body.DeleteVolumes != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET delete_unused_volumes = $1 WHERE server_id = $2`, *body.DeleteVolumes, m.ID)
		}
		if body.DeleteNetworks != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET delete_unused_networks = $1 WHERE server_id = $2`, *body.DeleteNetworks, m.ID)
		}
		if body.DisableImageRet != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET disable_application_image_retention = $1 WHERE server_id = $2`, *body.DisableImageRet, m.ID)
		}
		_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET updated_at = NOW() WHERE server_id = $1`, m.ID)
		a.serveDockerCleanupSettings(w, r, m.ID)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (a *App) serveDockerCleanupSettings(w http.ResponseWriter, r *http.Request, serverID int64) {
	var freq string
	var thresh int
	var force, delVol, delNet, disRet bool
	_ = a.Pool.QueryRow(r.Context(), `
		SELECT COALESCE(docker_cleanup_frequency,'0 0 * * *'), COALESCE(docker_cleanup_threshold,80),
		       COALESCE(force_docker_cleanup,FALSE), COALESCE(delete_unused_volumes,FALSE),
		       COALESCE(delete_unused_networks,FALSE), COALESCE(disable_application_image_retention,FALSE)
		FROM server_settings WHERE server_id = $1
	`, serverID).Scan(&freq, &thresh, &force, &delVol, &delNet, &disRet)
	writeJSON(w, http.StatusOK, map[string]any{
		"docker_cleanup_frequency":            freq,
		"docker_cleanup_threshold":            thresh,
		"force_docker_cleanup":                force,
		"delete_unused_volumes":               delVol,
		"delete_unused_networks":              delNet,
		"disable_application_image_retention": disRet,
	})
}

// ---- Log Drains ----

func (a *App) handleServerLogDrains(w http.ResponseWriter, r *http.Request, p auth.Principal, m slice12ServerMeta) {
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		a.serveLogDrains(w, r, p, m)
	case http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			AxiomEnabled    *bool   `json:"is_logdrain_axiom_enabled"`
			AxiomDataset    *string `json:"logdrain_axiom_dataset_name"`
			AxiomAPIKey     *string `json:"logdrain_axiom_api_key"`
			NewRelicEnabled *bool   `json:"is_logdrain_newrelic_enabled"`
			NewRelicKey     *string `json:"logdrain_newrelic_license_key"`
			NewRelicURI     *string `json:"logdrain_newrelic_base_uri"`
			CustomEnabled   *bool   `json:"is_logdrain_custom_enabled"`
			CustomConfig    *string `json:"logdrain_custom_config"`
			CustomParser    *string `json:"logdrain_custom_config_parser"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid log drains payload"})
			return
		}
		if body.AxiomEnabled != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET is_logdrain_axiom_enabled = $1 WHERE server_id = $2`, *body.AxiomEnabled, m.ID)
		}
		if body.AxiomDataset != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET logdrain_axiom_dataset_name = $1 WHERE server_id = $2`, strings.TrimSpace(*body.AxiomDataset), m.ID)
		}
		if body.AxiomAPIKey != nil {
			enc, _ := a.encryptSecretString(*body.AxiomAPIKey)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET logdrain_axiom_api_key = $1 WHERE server_id = $2`, enc, m.ID)
		}
		if body.NewRelicEnabled != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET is_logdrain_newrelic_enabled = $1 WHERE server_id = $2`, *body.NewRelicEnabled, m.ID)
		}
		if body.NewRelicKey != nil {
			enc, _ := a.encryptSecretString(*body.NewRelicKey)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET logdrain_newrelic_license_key = $1 WHERE server_id = $2`, enc, m.ID)
		}
		if body.NewRelicURI != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET logdrain_newrelic_base_uri = $1 WHERE server_id = $2`, strings.TrimSpace(*body.NewRelicURI), m.ID)
		}
		if body.CustomEnabled != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET is_logdrain_custom_enabled = $1 WHERE server_id = $2`, *body.CustomEnabled, m.ID)
		}
		if body.CustomConfig != nil {
			enc, _ := a.encryptSecretString(*body.CustomConfig)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET logdrain_custom_config = $1 WHERE server_id = $2`, enc, m.ID)
		}
		if body.CustomParser != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET logdrain_custom_config_parser = $1 WHERE server_id = $2`, *body.CustomParser, m.ID)
		}
		a.serveLogDrains(w, r, p, m)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (a *App) serveLogDrains(w http.ResponseWriter, r *http.Request, p auth.Principal, m slice12ServerMeta) {
	var axOn, nrOn, cuOn bool
	var axDS, axKey, nrKey, nrURI, cuCfg, cuParser string
	_ = a.Pool.QueryRow(r.Context(), `
		SELECT COALESCE(is_logdrain_axiom_enabled,FALSE), COALESCE(logdrain_axiom_dataset_name,''), COALESCE(logdrain_axiom_api_key,''),
		       COALESCE(is_logdrain_newrelic_enabled,FALSE), COALESCE(logdrain_newrelic_license_key,''), COALESCE(logdrain_newrelic_base_uri,''),
		       COALESCE(is_logdrain_custom_enabled,FALSE), COALESCE(logdrain_custom_config,''), COALESCE(logdrain_custom_config_parser,'')
		FROM server_settings WHERE server_id = $1
	`, m.ID).Scan(&axOn, &axDS, &axKey, &nrOn, &nrKey, &nrURI, &cuOn, &cuCfg, &cuParser)
	resp := map[string]any{
		"is_logdrain_axiom_enabled":     axOn,
		"logdrain_axiom_dataset_name":   axDS,
		"is_logdrain_newrelic_enabled":  nrOn,
		"logdrain_newrelic_base_uri":    nrURI,
		"is_logdrain_custom_enabled":    cuOn,
		"logdrain_custom_config_parser": cuParser,
	}
	if a.Authz.Check(r.Context(), p, access.ActionReadSensitive, m.Ref) == nil {
		resp["logdrain_axiom_api_key"] = a.decryptSecretString(axKey)
		resp["logdrain_newrelic_license_key"] = a.decryptSecretString(nrKey)
		resp["logdrain_custom_config"] = a.decryptSecretString(cuCfg)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- Sentinel & Metrics Push ----

func (a *App) handleServerSentinel(w http.ResponseWriter, r *http.Request, p auth.Principal, m slice12ServerMeta) {
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		a.serveServerSentinel(w, r, p, m)
	case http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			IsSentinelEnabled *bool   `json:"is_sentinel_enabled"`
			IsMetricsEnabled  *bool   `json:"is_metrics_enabled"`
			SentinelToken     *string `json:"sentinel_token"`
			SentinelCustomURL *string `json:"sentinel_custom_url"`
			PushInterval      *int    `json:"sentinel_push_interval_seconds"`
			MetricsRefresh    *int    `json:"sentinel_metrics_refresh_rate_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid sentinel payload"})
			return
		}
		if body.IsSentinelEnabled != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET is_sentinel_enabled = $1 WHERE server_id = $2`, *body.IsSentinelEnabled, m.ID)
		}
		if body.IsMetricsEnabled != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET is_metrics_enabled = $1 WHERE server_id = $2`, *body.IsMetricsEnabled, m.ID)
		}
		if body.SentinelToken != nil {
			enc, _ := a.encryptSecretString(*body.SentinelToken)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET sentinel_token = $1 WHERE server_id = $2`, enc, m.ID)
		}
		if body.SentinelCustomURL != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET sentinel_custom_url = $1 WHERE server_id = $2`, strings.TrimSpace(*body.SentinelCustomURL), m.ID)
		}
		if body.PushInterval != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET sentinel_push_interval_seconds = $1 WHERE server_id = $2`, *body.PushInterval, m.ID)
		}
		if body.MetricsRefresh != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET sentinel_metrics_refresh_rate_seconds = $1 WHERE server_id = $2`, *body.MetricsRefresh, m.ID)
		}
		a.serveServerSentinel(w, r, p, m)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (a *App) serveServerSentinel(w http.ResponseWriter, r *http.Request, p auth.Principal, m slice12ServerMeta) {
	var sentOn, metOn bool
	var tok, urlStr string
	var pushInt, refRate int
	_ = a.Pool.QueryRow(r.Context(), `
		SELECT COALESCE(is_sentinel_enabled,FALSE), COALESCE(is_metrics_enabled,FALSE),
		       COALESCE(sentinel_token,''), COALESCE(sentinel_custom_url,''),
		       COALESCE(sentinel_push_interval_seconds,60), COALESCE(sentinel_metrics_refresh_rate_seconds,5)
		FROM server_settings WHERE server_id = $1
	`, m.ID).Scan(&sentOn, &metOn, &tok, &urlStr, &pushInt, &refRate)
	resp := map[string]any{
		"is_sentinel_enabled":                   sentOn,
		"is_metrics_enabled":                    metOn,
		"sentinel_custom_url":                   urlStr,
		"sentinel_push_interval_seconds":        pushInt,
		"sentinel_metrics_refresh_rate_seconds": refRate,
	}
	if a.Authz.Check(r.Context(), p, access.ActionReadSensitive, m.Ref) == nil {
		resp["sentinel_token"] = a.decryptSecretString(tok)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) handleSentinelPush(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Coolify-Token"))
	}
	var body struct {
		ServerUUID    string  `json:"server_uuid"`
		CPUPercent    float64 `json:"cpu_percent"`
		MemoryPercent float64 `json:"memory_percent"`
		DiskPercent   float64 `json:"disk_percent"`
		Containers    []any   `json:"containers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ServerUUID) == "" || token == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "invalid sentinel push payload or token"})
		return
	}
	var srvID int64
	var encTok string
	if err := a.Pool.QueryRow(r.Context(), `
		SELECT s.id, COALESCE(ss.sentinel_token,'')
		FROM servers s JOIN server_settings ss ON ss.server_id = s.id
		WHERE s.uuid = $1 AND s.deleted_at IS NULL AND s.transferred_at IS NULL
	`, strings.TrimSpace(body.ServerUUID)).Scan(&srvID, &encTok); err != nil || encTok == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "invalid sentinel credentials"})
		return
	}
	expected := a.decryptSecretString(encTok)
	if subtle.ConstantTimeCompare([]byte(token), []byte(expected)) != 1 {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "invalid sentinel token"})
		return
	}
	contJSON, _ := json.Marshal(body.Containers)
	if string(contJSON) == "null" {
		contJSON = []byte("[]")
	}
	_, _ = a.Pool.Exec(r.Context(), `
		INSERT INTO go_server_metrics (server_id, cpu_percent, memory_percent, disk_percent, containers)
		VALUES ($1, $2, $3, $4, $5::jsonb)
	`, srvID, body.CPUPercent, body.MemoryPercent, body.DiskPercent, string(contJSON))
	writeJSON(w, http.StatusOK, map[string]string{"message": "Metrics accepted."})
}

// ---- Image Scanning & Cloudflare Tunnel ----

func (a *App) handleServerImageScanning(w http.ResponseWriter, r *http.Request, p auth.Principal, m slice12ServerMeta) {
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var enabled bool
		var today int
		_ = a.Pool.QueryRow(r.Context(), `SELECT COALESCE(is_image_scan_enabled,FALSE), COALESCE(image_candidate_scans_today,0) FROM server_settings WHERE server_id = $1`, m.ID).Scan(&enabled, &today)
		writeJSON(w, http.StatusOK, map[string]any{"is_image_scan_enabled": enabled, "image_candidate_scans_today": today})
	case http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Enabled *bool `json:"is_image_scan_enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Enabled == nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "is_image_scan_enabled boolean is required"})
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET is_image_scan_enabled = $1, updated_at = NOW() WHERE server_id = $2`, *body.Enabled, m.ID)
		writeJSON(w, http.StatusOK, map[string]any{"is_image_scan_enabled": *body.Enabled})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (a *App) handleServerCloudflareTunnel(w http.ResponseWriter, r *http.Request, p auth.Principal, m slice12ServerMeta, rest []string) {
	if len(rest) == 1 && (rest[0] == "enable" || rest[0] == "disable") && r.Method == http.MethodPost {
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		enabled := rest[0] == "enable"
		_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET is_cloudflare_tunnel = $1, updated_at = NOW() WHERE server_id = $2`, enabled, m.ID)
		writeJSON(w, http.StatusOK, map[string]any{"is_cloudflare_tunnel": enabled, "message": "Cloudflare tunnel updated."})
		return
	}
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var enabled bool
		var tid, accID string
		_ = a.Pool.QueryRow(r.Context(), `SELECT COALESCE(is_cloudflare_tunnel,FALSE), COALESCE(cloudflare_tunnel_id,''), COALESCE(cloudflare_tunnel_account_id,'') FROM server_settings WHERE server_id = $1`, m.ID).Scan(&enabled, &tid, &accID)
		writeJSON(w, http.StatusOK, map[string]any{"is_cloudflare_tunnel": enabled, "cloudflare_tunnel_id": tid, "cloudflare_tunnel_account_id": accID})
	case http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Enabled              *bool   `json:"is_cloudflare_tunnel"`
			TunnelID             *string `json:"cloudflare_tunnel_id"`
			AccountID            *string `json:"cloudflare_tunnel_account_id"`
			IntegrationTokenUUID *string `json:"integration_token_uuid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid cloudflare tunnel payload"})
			return
		}
		if body.Enabled != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET is_cloudflare_tunnel = $1 WHERE server_id = $2`, *body.Enabled, m.ID)
		}
		if body.TunnelID != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET cloudflare_tunnel_id = $1 WHERE server_id = $2`, strings.TrimSpace(*body.TunnelID), m.ID)
		}
		if body.AccountID != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET cloudflare_tunnel_account_id = $1 WHERE server_id = $2`, strings.TrimSpace(*body.AccountID), m.ID)
		}
		if body.IntegrationTokenUUID != nil && strings.TrimSpace(*body.IntegrationTokenUUID) != "" {
			var tokID int64
			if err := a.Pool.QueryRow(r.Context(), `SELECT id FROM integration_tokens WHERE uuid = $1 AND team_id = $2`, strings.TrimSpace(*body.IntegrationTokenUUID), m.TeamID).Scan(&tokID); err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "integration token not found in team"})
				return
			}
			_, _ = a.Pool.Exec(r.Context(), `UPDATE server_settings SET cloudflare_tunnel_integration_token_id = $1 WHERE server_id = $2`, tokID, m.ID)
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Cloudflare tunnel settings saved."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Docker Registries ----

func (a *App) handleServerRegistries(w http.ResponseWriter, r *http.Request, p auth.Principal, m slice12ServerMeta, rest []string) {
	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			if err := a.Authz.Check(r.Context(), p, access.ActionReadSensitive, m.Ref); err != nil {
				writeDomainError(w, err)
				return
			}
			rows, err := a.Pool.Query(r.Context(), `SELECT uuid, registry, COALESCE(username,''), verified, last_checked_at FROM go_server_registries WHERE server_id = $1 ORDER BY registry ASC`, m.ID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			defer rows.Close()
			out := make([]map[string]any, 0)
			for rows.Next() {
				var u, reg, usr string
				var ver bool
				var lc *time.Time
				if rows.Scan(&u, &reg, &usr, &ver, &lc) == nil {
					out = append(out, map[string]any{"uuid": u, "registry": reg, "username": usr, "verified": ver, "last_checked_at": lc})
				}
			}
			writeJSON(w, http.StatusOK, out)

		case http.MethodPost:
			if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, m.Ref); err != nil {
				writeDomainError(w, err)
				return
			}
			if m.TransferredAt != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Server has been transferred away; remote commands are disabled."})
				return
			}
			var body struct {
				Registry string `json:"registry"`
				Username string `json:"username"`
				Password string `json:"password"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Registry) == "" || strings.TrimSpace(body.Username) == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "registry and username are required."})
				return
			}
			if strings.HasPrefix(strings.TrimSpace(body.Registry), "-") || body.Password == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "registry and password are required"})
				return
			}
			if err := a.runRegistryCommand(r.Context(), m, registryLoginCommand(strings.TrimSpace(body.Registry), body.Username), registryPasswordInput(body.Password)); err != nil {
				writeJSON(w, http.StatusBadGateway, map[string]string{"message": err.Error()})
				return
			}
			encPass, err := a.encryptSecretString(body.Password)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			u := a.Hooks.NewCUID2()
			err = a.Pool.QueryRow(r.Context(), `
				INSERT INTO go_server_registries (uuid, server_id, registry, username, password_encrypted, verified, last_checked_at)
				VALUES ($1,$2,$3,$4,$5,TRUE,NOW())
				ON CONFLICT (server_id, registry) DO UPDATE SET username = EXCLUDED.username, password_encrypted = EXCLUDED.password_encrypted, verified = TRUE, last_checked_at = NOW(), updated_at = NOW()
				RETURNING uuid
			`, u, m.ID, strings.TrimSpace(body.Registry), strings.TrimSpace(body.Username), encPass).Scan(&u)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"uuid": u, "registry": body.Registry, "username": body.Username, "verified": true})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}

	regKey := rest[0]
	if len(rest) == 2 && rest[1] == "check" && r.Method == http.MethodPost {
		if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if m.TransferredAt != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Server has been transferred away; remote commands are disabled."})
			return
		}
		var registry, username, password string
		if err := a.Pool.QueryRow(r.Context(), `SELECT registry,username,password_encrypted FROM go_server_registries WHERE server_id=$1 AND (registry=$2 OR uuid=$2)`, m.ID, regKey).Scan(&registry, &username, &password); err != nil {
			writeDomainError(w, err)
			return
		}
		runErr := a.runRegistryCommand(r.Context(), m, registryLoginCommand(registry, username), registryPasswordInput(a.decryptSecretString(password)))
		tag, err := a.Pool.Exec(r.Context(), `UPDATE go_server_registries SET verified = $3, last_checked_at = NOW() WHERE server_id = $1 AND (registry = $2 OR uuid = $2)`, m.ID, regKey, runErr == nil)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		if tag.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "registry login not found"})
			return
		}
		if runErr != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"message": runErr.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"registry": regKey, "verified": true})
		return
	}

	if len(rest) == 1 && r.Method == http.MethodDelete {
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if m.TransferredAt != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Server has been transferred away; remote commands are disabled."})
			return
		}
		var registry string
		if err := a.Pool.QueryRow(r.Context(), `SELECT registry FROM go_server_registries WHERE server_id=$1 AND (registry=$2 OR uuid=$2)`, m.ID, regKey).Scan(&registry); err != nil {
			writeDomainError(w, err)
			return
		}
		if err := a.runRegistryCommand(r.Context(), m, "docker logout "+shellSingleQuote(registry), ""); err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"message": err.Error()})
			return
		}
		tag, err := a.Pool.Exec(r.Context(), `DELETE FROM go_server_registries WHERE server_id = $1 AND (registry = $2 OR uuid = $2)`, m.ID, regKey)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		if tag.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "registry not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Logged out from registry."})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
}

// ---- Server Proxy Extended & Transfer Status/Complete ----

func (a *App) handleServerProxyExtended(w http.ResponseWriter, r *http.Request, p auth.Principal, m slice12ServerMeta, rest []string) {
	if len(rest) == 0 && r.Method == http.MethodPatch {
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid proxy payload"})
			return
		}
		raw, _ := json.Marshal(body)
		_, _ = a.Pool.Exec(r.Context(), `UPDATE servers SET proxy = COALESCE(proxy,'{}'::jsonb) || $1::jsonb, updated_at = NOW() WHERE id = $2`, string(raw), m.ID)
		writeJSON(w, http.StatusOK, map[string]any{"message": "Proxy updated.", "proxy": body})
		return
	}
	if len(rest) == 1 && rest[0] == "configuration" && r.Method == http.MethodPut {
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Configuration string `json:"configuration"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Configuration) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "configuration is required."})
			return
		}
		raw, _ := json.Marshal(map[string]any{"custom_configuration": body.Configuration})
		_, _ = a.Pool.Exec(r.Context(), `UPDATE servers SET proxy = COALESCE(proxy,'{}'::jsonb) || $1::jsonb, updated_at = NOW() WHERE id = $2`, string(raw), m.ID)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Proxy configuration saved."})
		return
	}
	if len(rest) == 1 && rest[0] == "restart" && r.Method == http.MethodPost {
		if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if m.TransferredAt != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Server has been transferred away; remote commands are disabled."})
			return
		}
		rec, err := a.enqueueResourceLifecycleJob(r.Context(), "server", m.UUID, m.UUID, m.TeamID, "server_validate")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		rec["message"] = "Proxy restart queued."
		writeJSON(w, http.StatusAccepted, rec)
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
}

func (a *App) handleServerTransferExtended(w http.ResponseWriter, r *http.Request, p auth.Principal, m slice12ServerMeta, rest []string) {
	if len(rest) == 0 && r.Method == http.MethodGet {
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var status, targetURL string
		_ = a.Pool.QueryRow(r.Context(), `SELECT COALESCE(transfer_status,'idle'), COALESCE(transferred_to_url,'') FROM servers WHERE id = $1`, m.ID).Scan(&status, &targetURL)
		writeJSON(w, http.StatusOK, map[string]any{
			"server_uuid":     m.UUID,
			"is_transferred":  m.TransferredAt != nil,
			"transferred_at":  m.TransferredAt,
			"transfer_status": status,
			"transferred_to":  targetURL,
		})
		return
	}
	if len(rest) == 1 && rest[0] == "complete" && r.Method == http.MethodPost {
		if err := a.Authz.Check(r.Context(), p, access.ActionTransfer, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			TargetURL string `json:"target_url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = a.Pool.Exec(r.Context(), `
			UPDATE servers SET transferred_at = NOW(), transfer_status = 'completed', transferred_to_url = NULLIF($1,''), updated_at = NOW()
			WHERE id = $2
		`, strings.TrimSpace(body.TargetURL), m.ID)
		writeJSON(w, http.StatusOK, map[string]any{"message": "Server transfer marked complete.", "server_uuid": m.UUID, "is_transferred": true})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
}

// ---- Fleet & Resource Vulnerabilities ----

func (a *App) handleFleetVulnerabilities(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	teamID := activeTeamForPrincipal(p)
	if err := a.Authz.CheckList(r.Context(), p, teamID, "vulnerabilities"); err != nil {
		writeDomainError(w, err)
		return
	}
	rows, err := a.Pool.Query(r.Context(), `
		SELECT isc.uuid, isc.resource_type, isc.resource_id, isc.image_ref, isc.status, COALESCE(isc.summary,'{}'::jsonb), isc.scanned_at
		FROM image_scans isc
		JOIN servers s ON s.id = isc.server_id
		WHERE s.team_id = $1 AND s.deleted_at IS NULL
		ORDER BY isc.id DESC LIMIT 100
	`, teamID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	defer rows.Close()
	out := make([]map[string]any, 0)
	for rows.Next() {
		var u, rType, imgRef, st string
		var rID int64
		var sumRaw []byte
		var scAt *time.Time
		if rows.Scan(&u, &rType, &rID, &imgRef, &st, &sumRaw, &scAt) == nil {
			resKind := "application"
			if strings.Contains(strings.ToLower(rType), "service") {
				resKind = "service"
			} else if strings.Contains(strings.ToLower(rType), "standalone") || strings.Contains(strings.ToLower(rType), "database") {
				resKind = "database"
			}
			// Enforce per-resource visibility so project-scoped members only see permitted resources
			if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
				Type: resKind, ID: rID, TeamID: &teamID, APISegment: "vulnerabilities",
			}); err != nil {
				continue
			}
			var summary any
			_ = json.Unmarshal(sumRaw, &summary)
			out = append(out, map[string]any{
				"uuid": u, "resource_type": rType, "resource_id": rID, "image_ref": imgRef, "status": st, "summary": summary, "scanned_at": scAt,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *App) handleResourceVulnerabilities(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionRead, meta.TeamScopedRef); err != nil {
		writeDomainError(w, err)
		return
	}
	morphs := []string{meta.MorphType}
	if meta.Kind == "database" {
		for _, spec := range standaloneDBTables {
			if spec.Table == meta.DBTable {
				morphs = append(morphs, spec.Morph)
				break
			}
		}
	}
	rows, err := a.Pool.Query(r.Context(), `
		SELECT uuid, image_ref, status, COALESCE(summary,'{}'::jsonb), COALESCE(findings,'[]'::jsonb), scanned_at
		FROM image_scans
		WHERE resource_id = $1 AND resource_type = ANY($2)
		ORDER BY id DESC LIMIT 20
	`, meta.ID, morphs)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	out := make([]map[string]any, 0)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var u, imgRef, st string
			var sumRaw, findRaw []byte
			var scAt *time.Time
			if rows.Scan(&u, &imgRef, &st, &sumRaw, &findRaw, &scAt) == nil {
				var summary, findings any
				_ = json.Unmarshal(sumRaw, &summary)
				_ = json.Unmarshal(findRaw, &findings)
				out = append(out, map[string]any{
					"uuid": u, "image_ref": imgRef, "status": st, "summary": summary, "findings": findings, "scanned_at": scAt,
				})
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}
