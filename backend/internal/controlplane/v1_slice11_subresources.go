package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/jackc/pgx/v5"
)

func (a *App) withExtendedAppRoutes(next authedHandler) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/applications"), "/")
		parts := strings.Split(trimmed, "/")
		if len(parts) < 2 {
			next(w, r, p)
			return
		}
		appUUID := parts[0]
		sub := parts[1]

		switch sub {
		case "tags":
			meta, err := a.loadResourceMeta(r.Context(), "application", appUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "application not found"})
				return
			}
			tagSeg := ""
			if len(parts) >= 3 {
				tagSeg = parts[2]
			}
			a.handleResourceTags(w, r, p, meta, tagSeg)
			return

		case "storages":
			if len(parts) == 2 && r.Method == http.MethodPatch {
				meta, err := a.loadResourceMeta(r.Context(), "application", appUUID)
				if err != nil {
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "application not found"})
					return
				}
				a.handleStoragePatch(w, r, p, meta)
				return
			}
			if (len(parts) == 4 && parts[3] == "backups") || (len(parts) == 5 && parts[3] == "backups" && parts[4] == "run") {
				meta, err := a.loadResourceMeta(r.Context(), "application", appUUID)
				if err != nil {
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "application not found"})
					return
				}
				a.handleVolumeBackupRoute(w, r, p, meta, parts[2], len(parts) == 5)
				return
			}

		case "move":
			meta, err := a.loadResourceMeta(r.Context(), "application", appUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "application not found"})
				return
			}
			a.handleResourceMove(w, r, p, meta)
			return

		case "migrate":
			meta, err := a.loadResourceMeta(r.Context(), "application", appUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "application not found"})
				return
			}
			a.handleResourceMigrate(w, r, p, meta)
			return

		case "destinations":
			meta, err := a.loadResourceMeta(r.Context(), "application", appUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "application not found"})
				return
			}
			destUUID := ""
			if len(parts) >= 3 {
				destUUID = parts[2]
			}
			a.handleApplicationDestinations(w, r, p, meta, destUUID)
			return

		case "rollback-images":
			meta, err := a.loadResourceMeta(r.Context(), "application", appUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "application not found"})
				return
			}
			a.handleApplicationRollbackImages(w, r, p, meta)
			return

		case "rollback":
			meta, err := a.loadResourceMeta(r.Context(), "application", appUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "application not found"})
				return
			}
			a.handleApplicationRollback(w, r, p, meta)
			return

		case "previews":
			if len(parts) == 4 && parts[3] == "logs" && r.Method == http.MethodGet {
				meta, err := a.loadResourceMeta(r.Context(), "application", appUUID)
				if err != nil {
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "application not found"})
					return
				}
				a.handleApplicationPreviewLogs(w, r, p, meta, parts[2])
				return
			}

		case "scheduled-tasks":
			meta, err := a.loadResourceMeta(r.Context(), "application", appUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "application not found"})
				return
			}
			a.handleScheduledTasks(w, r, p, meta, parts[2:])
			return

		case "dns":
			meta, err := a.loadResourceMeta(r.Context(), "application", appUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "application not found"})
				return
			}
			a.handleResourceDNS(w, r, p, meta, parts[2:])
			return

		case "secret-manager":
			meta, err := a.loadResourceMeta(r.Context(), "application", appUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "application not found"})
				return
			}
			a.handleResourceSecretManager(w, r, p, meta, parts[2:])
			return

		case "vulnerabilities":
			meta, err := a.loadResourceMeta(r.Context(), "application", appUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "application not found"})
				return
			}
			a.handleResourceVulnerabilities(w, r, p, meta)
			return
		}

		next(w, r, p)
	}
}

func (a *App) withExtendedServiceRoutes(next authedHandler) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/services"), "/")
		parts := strings.Split(trimmed, "/")
		if len(parts) < 2 {
			next(w, r, p)
			return
		}
		svcUUID := parts[0]
		sub := parts[1]

		switch sub {
		case "tags":
			meta, err := a.loadResourceMeta(r.Context(), "service", svcUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
				return
			}
			tagSeg := ""
			if len(parts) >= 3 {
				tagSeg = parts[2]
			}
			a.handleResourceTags(w, r, p, meta, tagSeg)
			return

		case "storages":
			if len(parts) == 2 && r.Method == http.MethodPatch {
				meta, err := a.loadResourceMeta(r.Context(), "service", svcUUID)
				if err != nil {
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
					return
				}
				a.handleStoragePatch(w, r, p, meta)
				return
			}
			if (len(parts) == 4 && parts[3] == "backups") || (len(parts) == 5 && parts[3] == "backups" && parts[4] == "run") {
				meta, err := a.loadResourceMeta(r.Context(), "service", svcUUID)
				if err != nil {
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
					return
				}
				a.handleVolumeBackupRoute(w, r, p, meta, parts[2], len(parts) == 5)
				return
			}

		case "move":
			meta, err := a.loadResourceMeta(r.Context(), "service", svcUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
				return
			}
			a.handleResourceMove(w, r, p, meta)
			return

		case "migrate":
			meta, err := a.loadResourceMeta(r.Context(), "service", svcUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
				return
			}
			a.handleResourceMigrate(w, r, p, meta)
			return

		case "clone":
			meta, err := a.loadResourceMeta(r.Context(), "service", svcUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
				return
			}
			a.handleResourceClone(w, r, p, meta)
			return

		case "envs":
			meta, err := a.loadResourceMeta(r.Context(), "service", svcUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
				return
			}
			a.handleResourceEnvs(w, r, p, meta, parts[2:])
			return

		case "logs":
			meta, err := a.loadResourceMeta(r.Context(), "service", svcUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
				return
			}
			a.handleResourceLogsByUUID(w, r, p, meta, "")
			return

		case "scheduled-tasks":
			meta, err := a.loadResourceMeta(r.Context(), "service", svcUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
				return
			}
			a.handleScheduledTasks(w, r, p, meta, parts[2:])
			return

		case "applications", "databases":
			meta, err := a.loadResourceMeta(r.Context(), "service", svcUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
				return
			}
			a.handleServiceSubResources(w, r, p, meta, sub, parts[2:])
			return

		case "dns":
			meta, err := a.loadResourceMeta(r.Context(), "service", svcUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
				return
			}
			a.handleResourceDNS(w, r, p, meta, parts[2:])
			return

		case "secret-manager":
			meta, err := a.loadResourceMeta(r.Context(), "service", svcUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
				return
			}
			a.handleResourceSecretManager(w, r, p, meta, parts[2:])
			return

		case "vulnerabilities":
			meta, err := a.loadResourceMeta(r.Context(), "service", svcUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
				return
			}
			a.handleResourceVulnerabilities(w, r, p, meta)
			return
		}

		next(w, r, p)
	}
}

func (a *App) withExtendedDatabaseRoutes(next authedHandler) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/databases"), "/")
		parts := strings.Split(trimmed, "/")
		if len(parts) < 2 {
			next(w, r, p)
			return
		}
		dbUUID := parts[0]
		sub := parts[1]

		switch sub {
		case "tags":
			meta, err := a.loadResourceMeta(r.Context(), "database", dbUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "database not found"})
				return
			}
			tagSeg := ""
			if len(parts) >= 3 {
				tagSeg = parts[2]
			}
			a.handleResourceTags(w, r, p, meta, tagSeg)
			return

		case "storages":
			if len(parts) == 2 && r.Method == http.MethodPatch {
				meta, err := a.loadResourceMeta(r.Context(), "database", dbUUID)
				if err != nil {
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "database not found"})
					return
				}
				a.handleStoragePatch(w, r, p, meta)
				return
			}
			if (len(parts) == 4 && parts[3] == "backups") || (len(parts) == 5 && parts[3] == "backups" && parts[4] == "run") {
				meta, err := a.loadResourceMeta(r.Context(), "database", dbUUID)
				if err != nil {
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "database not found"})
					return
				}
				a.handleVolumeBackupRoute(w, r, p, meta, parts[2], len(parts) == 5)
				return
			}

		case "move":
			meta, err := a.loadResourceMeta(r.Context(), "database", dbUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "database not found"})
				return
			}
			a.handleResourceMove(w, r, p, meta)
			return

		case "migrate":
			meta, err := a.loadResourceMeta(r.Context(), "database", dbUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "database not found"})
				return
			}
			a.handleResourceMigrate(w, r, p, meta)
			return

		case "clone":
			meta, err := a.loadResourceMeta(r.Context(), "database", dbUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "database not found"})
				return
			}
			a.handleResourceClone(w, r, p, meta)
			return

		case "envs":
			meta, err := a.loadResourceMeta(r.Context(), "database", dbUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "database not found"})
				return
			}
			a.handleResourceEnvs(w, r, p, meta, parts[2:])
			return

		case "logs":
			meta, err := a.loadResourceMeta(r.Context(), "database", dbUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "database not found"})
				return
			}
			a.handleResourceLogsByUUID(w, r, p, meta, "")
			return

		case "backups":
			if len(parts) >= 3 && !(len(parts) == 4 && parts[3] == "restore") {
				meta, err := a.loadResourceMeta(r.Context(), "database", dbUUID)
				if err != nil {
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "database not found"})
					return
				}
				a.handleDatabaseBackupsSubRoutes(w, r, p, meta, parts[2:])
				return
			}

		case "imports":
			meta, err := a.loadResourceMeta(r.Context(), "database", dbUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "database not found"})
				return
			}
			a.handleDatabaseImports(w, r, p, meta, parts[2:])
			return

		case "secret-manager":
			meta, err := a.loadResourceMeta(r.Context(), "database", dbUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "database not found"})
				return
			}
			a.handleResourceSecretManager(w, r, p, meta, parts[2:])
			return

		case "vulnerabilities":
			meta, err := a.loadResourceMeta(r.Context(), "database", dbUUID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "database not found"})
				return
			}
			a.handleResourceVulnerabilities(w, r, p, meta)
			return
		}

		next(w, r, p)
	}
}

// ---- Application Destinations ----

func (a *App) handleApplicationDestinations(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta, destUUID string) {
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		out := make([]map[string]any, 0)
		var pName, pNet, pSrvUUID string
		var pSrvID int64
		if err := a.Pool.QueryRow(r.Context(), `
			SELECT sd.uuid, sd.name, sd.network, sd.server_id, s.uuid
			FROM standalone_dockers sd JOIN servers s ON s.id = sd.server_id
			WHERE sd.id = $1
		`, meta.DestinationID).Scan(&destUUID, &pName, &pNet, &pSrvID, &pSrvUUID); err == nil {
			out = append(out, map[string]any{
				"uuid": destUUID, "name": pName, "network": pNet, "server_id": pSrvID, "server_uuid": pSrvUUID, "is_primary": true,
			})
		}
		rows, err := a.Pool.Query(r.Context(), `
			SELECT sd.uuid, sd.name, sd.network, ad.server_id, s.uuid, ad.status
			FROM additional_destinations ad
			JOIN standalone_dockers sd ON sd.id = ad.standalone_docker_id
			JOIN servers s ON s.id = ad.server_id
			WHERE ad.application_id = $1
			ORDER BY ad.id ASC
		`, meta.ID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var u, n, netw, su, st string
				var sid int64
				if rows.Scan(&u, &n, &netw, &sid, &su, &st) == nil {
					out = append(out, map[string]any{
						"uuid": u, "name": n, "network": netw, "server_id": sid, "server_uuid": su, "status": st, "is_primary": false,
					})
				}
			}
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			DestinationUUID string `json:"destination_uuid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.DestinationUUID) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "destination_uuid is required."})
			return
		}
		var dID, sID, sTeamID int64
		var sUUID string
		var forceDisabled bool
		err := a.Pool.QueryRow(r.Context(), `
			SELECT sd.id, s.id, s.uuid, s.team_id, COALESCE(ss.force_disabled, FALSE)
			FROM standalone_dockers sd
			JOIN servers s ON s.id = sd.server_id
			LEFT JOIN server_settings ss ON ss.server_id = s.id
			WHERE sd.uuid = $1 AND s.team_id = $2 AND s.deleted_at IS NULL
		`, strings.TrimSpace(body.DestinationUUID), meta.TeamID).Scan(&dID, &sID, &sUUID, &sTeamID, &forceDisabled)
		if errors.Is(err, pgx.ErrNoRows) || forceDisabled {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Destination not found."})
			return
		}
		if err := a.Authz.Check(r.Context(), p, access.ActionPlaceResource, access.ResourceRef{
			Type: "server", ID: sID, UUID: sUUID, TeamID: &sTeamID, APISegment: "servers",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		if dID == meta.DestinationID {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Destination is already the primary destination."})
			return
		}
		if sID == meta.ServerID {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Cannot attach a destination on the same server as the primary destination."})
			return
		}
		_, err = a.Pool.Exec(r.Context(), `
			INSERT INTO additional_destinations (application_id, server_id, standalone_docker_id)
			VALUES ($1, $2, $3)
		`, meta.ID, sID, dID)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Destination is already attached."})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"message": "Destination attached.", "uuid": body.DestinationUUID})

	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		var dID int64
		if err := a.Pool.QueryRow(r.Context(), `
			SELECT sd.id FROM standalone_dockers sd JOIN servers s ON s.id = sd.server_id
			WHERE sd.uuid = $1 AND s.team_id = $2
		`, destUUID, meta.TeamID).Scan(&dID); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Destination not found."})
			return
		}
		if dID == meta.DestinationID {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Cannot remove the primary destination."})
			return
		}
		tag, _ := a.Pool.Exec(r.Context(), `DELETE FROM additional_destinations WHERE application_id = $1 AND standalone_docker_id = $2`, meta.ID, dID)
		if tag.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Destination not found."})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Destination detached."})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Application Rollback & Preview Logs ----

func (a *App) handleApplicationRollbackImages(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionRead, meta.TeamScopedRef); err != nil {
		writeDomainError(w, err)
		return
	}
	rows, err := a.Pool.Query(r.Context(), `
		SELECT DISTINCT ON (COALESCE(NULLIF(commit,''), deployment_uuid))
		       COALESCE(NULLIF(commit,''), deployment_uuid) AS tag,
		       created_at
		FROM application_deployment_queues
		WHERE application_id = $1 AND status IN ('finished','succeeded') AND COALESCE(pull_request_id, 0) = 0
		ORDER BY COALESCE(NULLIF(commit,''), deployment_uuid), created_at DESC
		LIMIT 10
	`, meta.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	defer rows.Close()
	images := make([]map[string]any, 0)
	var current *string
	for rows.Next() {
		var tag string
		var ts time.Time
		if rows.Scan(&tag, &ts) == nil {
			isCur := current == nil
			if current == nil {
				tCopy := tag
				current = &tCopy
			}
			images = append(images, map[string]any{
				"tag": tag, "created_at": ts.UTC().Format(time.RFC3339), "is_current": isCur, "source": "registry",
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"current": current, "images": images})
}

func (a *App) handleApplicationRollback(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, meta.TeamScopedRef); err != nil {
		writeDomainError(w, err)
		return
	}
	var body struct {
		Commit string `json:"commit"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Commit) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "commit is required."})
		return
	}
	commit := strings.TrimSpace(body.Commit)
	for _, bad := range []string{";", "|", "&", "`", "$(", "<", ">", "\n", "\r", "\x00", " "} {
		if strings.Contains(commit, bad) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid rollback commit"})
			return
		}
	}
	var exists bool
	_ = a.Pool.QueryRow(r.Context(), `
		SELECT EXISTS(
			SELECT 1 FROM application_deployment_queues
			WHERE application_id = $1 AND status IN ('finished','succeeded')
			  AND COALESCE(pull_request_id, 0) = 0
			  AND (commit = $2 OR deployment_uuid = $2)
		)
	`, meta.ID, commit).Scan(&exists)
	if !exists {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "The commit is not a rollback image or a successful deployment of this application."})
		return
	}

	depUUID := a.Hooks.NewCUID2()
	opID := "deploy-" + depUUID
	tx, err := a.Pool.Begin(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO application_deployment_queues (deployment_uuid, application_id, application_name, server_id, server_name, status, commit, rollback, is_api)
		VALUES ($1, $2, $3, $4, $5, 'queued', $6, TRUE, TRUE)
	`, depUUID, meta.ID, meta.Name, meta.ServerID, meta.ServerUUID, commit); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	if err := a.JobCoord.AdmitOperationInTx(r.Context(), tx, jobs.DurableJobSpec{
		JobOperation: jobs.JobOperation{
			JobID: "job-" + depUUID, OperationID: opID, AttemptID: "att-0",
			ResourceUUID: meta.UUID, ServerUUID: meta.ServerUUID, TeamID: meta.TeamID,
			Status: jobs.StatusQueued, MaxAttempts: 2,
		},
		JobType: "deploy",
		Payload: map[string]any{"deployment_uuid": depUUID, "commit": commit, "rollback": true},
	}); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	_, _ = a.Dispatcher.FlushPending(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"message": "Rollback deployment queued.", "deployment_uuid": depUUID, "operation_id": opID})
}

func (a *App) handleApplicationPreviewLogs(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta, prSeg string) {
	if err := a.Authz.Check(r.Context(), p, access.ActionReadSensitive, meta.TeamScopedRef); err != nil {
		writeDomainError(w, err)
		return
	}
	prID, err := strconv.ParseInt(prSeg, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid pull_request_id"})
		return
	}
	var prevUUID string
	if err := a.Pool.QueryRow(r.Context(), `SELECT uuid FROM application_previews WHERE application_id = $1 AND pull_request_id = $2`, meta.ID, prID).Scan(&prevUUID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "preview not found"})
		return
	}
	rows, err := a.Pool.Query(r.Context(), `
		SELECT COALESCE(logs,'') FROM application_deployment_queues
		WHERE application_id = $1 AND pull_request_id = $2
		ORDER BY id DESC LIMIT 1
	`, meta.ID, prID)
	var logText string
	if err == nil {
		defer rows.Close()
		if rows.Next() {
			_ = rows.Scan(&logText)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"pull_request_id": prID, "preview_uuid": prevUUID, "logs": logText})
}

// ---- Scheduled Tasks ----

func (a *App) handleScheduledTasks(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta, rest []string) {
	idCol := "application_id"
	if meta.Kind == "service" {
		idCol = "service_id"
	}

	if len(rest) == 0 {
		switch r.Method {
		case http.MethodGet:
			if err := a.Authz.Check(r.Context(), p, access.ActionRead, meta.TeamScopedRef); err != nil {
				writeDomainError(w, err)
				return
			}
			canSensitive := a.Authz.Check(r.Context(), p, access.ActionReadSensitive, meta.TeamScopedRef) == nil
			rows, err := a.Pool.Query(r.Context(), fmt.Sprintf(`
				SELECT uuid, enabled, name, command, frequency, COALESCE(container,''), timeout, use_shell
				FROM scheduled_tasks WHERE %s = $1 ORDER BY id ASC
			`, idCol), meta.ID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			defer rows.Close()
			out := make([]map[string]any, 0)
			for rows.Next() {
				var u, n, cmd, freq, cont string
				var en, useShell bool
				var to int
				if rows.Scan(&u, &en, &n, &cmd, &freq, &cont, &to, &useShell) == nil {
					item := map[string]any{"uuid": u, "enabled": en, "name": n, "frequency": freq, "container": cont, "timeout": to, "use_shell": useShell}
					if canSensitive {
						item["command"] = cmd
					}
					out = append(out, item)
				}
			}
			writeJSON(w, http.StatusOK, out)

		case http.MethodPost:
			if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				Name      string `json:"name"`
				Command   string `json:"command"`
				Frequency string `json:"frequency"`
				Container string `json:"container"`
				Timeout   *int   `json:"timeout"`
				UseShell  *bool  `json:"use_shell"`
				Enabled   *bool  `json:"enabled"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" || strings.TrimSpace(body.Command) == "" || strings.TrimSpace(body.Frequency) == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "name, command, and frequency are required."})
				return
			}
			enabled := true
			if body.Enabled != nil {
				enabled = *body.Enabled
			}
			if enabled {
				if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, meta.TeamScopedRef); err != nil {
					writeDomainError(w, err)
					return
				}
			}
			timeout := 300
			if body.Timeout != nil {
				timeout = *body.Timeout
			}
			useShell := true
			if body.UseShell != nil {
				useShell = *body.UseShell
			}
			taskUUID := a.Hooks.NewCUID2()
			var id int64
			err := a.Pool.QueryRow(r.Context(), fmt.Sprintf(`
				INSERT INTO scheduled_tasks (uuid, enabled, name, command, frequency, container, timeout, use_shell, %s, team_id)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id
			`, idCol), taskUUID, enabled, compat.SanitizeString(body.Name), body.Command, strings.TrimSpace(body.Frequency),
				strings.TrimSpace(body.Container), timeout, useShell, meta.ID, meta.TeamID).Scan(&id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{
				"id": id, "uuid": taskUUID, "enabled": enabled, "name": compat.SanitizeString(body.Name), "frequency": body.Frequency,
			})

		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}

	taskUUID := rest[0]
	var taskID int64
	var enabled bool
	var cmd, name, cont string
	var timeout int
	var useShell bool
	err := a.Pool.QueryRow(r.Context(), fmt.Sprintf(`
		SELECT id, enabled, command, name, COALESCE(container,''), timeout, use_shell
		FROM scheduled_tasks WHERE uuid = $1 AND %s = $2 AND team_id = $3
	`, idCol), taskUUID, meta.ID, meta.TeamID).Scan(&taskID, &enabled, &cmd, &name, &cont, &timeout, &useShell)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "scheduled task not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}

	if len(rest) == 2 && rest[1] == "executions" && r.Method == http.MethodGet {
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		rows, err := a.Pool.Query(r.Context(), `
			SELECT uuid, status, COALESCE(message,''), created_at, finished_at
			FROM scheduled_task_executions WHERE scheduled_task_id = $1 ORDER BY id DESC LIMIT 50
		`, taskID)
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

	if len(rest) == 2 && rest[1] == "execute" && r.Method == http.MethodPost {
		if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		execUUID := a.Hooks.NewCUID2()
		var execID int64
		if err := a.Pool.QueryRow(r.Context(), `
			INSERT INTO scheduled_task_executions (uuid, scheduled_task_id, status, started_at)
			VALUES ($1, $2, 'running', NOW()) RETURNING id
		`, execUUID, taskID).Scan(&execID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		opID := "task-" + execUUID
		tx, err := a.Pool.Begin(r.Context())
		if err == nil {
			defer func() { _ = tx.Rollback(r.Context()) }()
			_ = a.JobCoord.AdmitOperationInTx(r.Context(), tx, jobs.DurableJobSpec{
				JobOperation: jobs.JobOperation{
					JobID: "job-" + opID, OperationID: opID, AttemptID: "att-0",
					ResourceUUID: meta.UUID, ServerUUID: meta.ServerUUID, TeamID: meta.TeamID,
					Status: jobs.StatusQueued, MaxAttempts: 2,
				},
				JobType: "scheduled_task",
				Payload: map[string]any{
					"execution_id":   execID,
					"execution_uuid": execUUID,
					"task_uuid":      taskUUID,
					"command":        cmd,
					"container":      cont,
					"use_shell":      useShell,
					"timeout":        timeout,
				},
			})
			_ = tx.Commit(r.Context())
			_, _ = a.Dispatcher.FlushPending(r.Context())
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"message": "Scheduled task queued.", "execution_uuid": execUUID, "operation_id": opID,
		})
		return
	}

	if len(rest) == 1 && r.Method == http.MethodPatch {
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Name      *string `json:"name"`
			Command   *string `json:"command"`
			Frequency *string `json:"frequency"`
			Enabled   *bool   `json:"enabled"`
			Container *string `json:"container"`
			Timeout   *int    `json:"timeout"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid task update"})
			return
		}
		if body.Enabled != nil && *body.Enabled {
			if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, meta.TeamScopedRef); err != nil {
				writeDomainError(w, err)
				return
			}
			enabled = true
		} else if body.Enabled != nil {
			enabled = false
		}
		if body.Name != nil {
			name = compat.SanitizeString(*body.Name)
		}
		if body.Command != nil {
			cmd = *body.Command
		}
		if body.Timeout != nil {
			timeout = *body.Timeout
		}
		if body.Container != nil {
			cont = strings.TrimSpace(*body.Container)
		}
		_, _ = a.Pool.Exec(r.Context(), `UPDATE scheduled_tasks SET name = $1, command = $2, enabled = $3, timeout = $4, container = $5, updated_at = NOW() WHERE id = $6`,
			name, cmd, enabled, timeout, cont, taskID)
		writeJSON(w, http.StatusOK, map[string]any{"uuid": taskUUID, "name": name, "enabled": enabled, "message": "Scheduled task updated."})
		return
	}

	if len(rest) == 1 && r.Method == http.MethodDelete {
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM scheduled_tasks WHERE id = $1`, taskID)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Scheduled task deleted."})
		return
	}

	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
}

// ---- Service / Database Envs, Logs, Clone, Service Sub-resources, DB Backups & Imports ----

func (a *App) handleResourceEnvs(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta, rest []string) {
	morphTypes := []string{meta.MorphType}
	if meta.Kind == "database" {
		for _, spec := range standaloneDBTables {
			if spec.Table == meta.DBTable {
				morphTypes = append(morphTypes, spec.Morph)
				break
			}
		}
	}

	if len(rest) == 1 && rest[0] == "bulk" && (r.Method == http.MethodPatch || r.Method == http.MethodPost) {
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Data []struct {
				Key       string `json:"key"`
				Value     string `json:"value"`
				IsPreview bool   `json:"is_preview"`
			} `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid bulk env payload"})
			return
		}
		out := make([]map[string]any, 0, len(body.Data))
		for _, item := range body.Data {
			u := a.Hooks.NewCUID2()
			enc, _ := a.encryptSecretString(item.Value)
			var id int64
			_ = a.Pool.QueryRow(r.Context(), `
				INSERT INTO environment_variables (uuid, key, value, is_preview, resourceable_type, resourceable_id)
				VALUES ($1, $2, $3, $4, $5, $6) RETURNING id
			`, u, item.Key, enc, item.IsPreview, meta.MorphType, meta.ID).Scan(&id)
			out = append(out, map[string]any{"id": id, "uuid": u, "key": item.Key})
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		canSecret := a.Authz.Check(r.Context(), p, access.ActionReadSensitive, meta.TeamScopedRef) == nil
		rows, err := a.Pool.Query(r.Context(), `
			SELECT id, uuid, key, COALESCE(value,''), is_preview, is_buildtime, is_runtime, is_shown_once
			FROM environment_variables
			WHERE resourceable_id = $1 AND resourceable_type = ANY($2)
			ORDER BY id ASC
		`, meta.ID, morphTypes)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id int64
			var u, k, v string
			var ip, ib, ir, io bool
			if rows.Scan(&id, &u, &k, &v, &ip, &ib, &ir, &io) == nil {
				row := map[string]any{"id": id, "uuid": u, "key": k, "is_preview": ip, "is_buildtime": ib, "is_runtime": ir, "is_shown_once": io}
				if canSecret && !io {
					row["value"] = a.decryptSecretString(v)
				}
				out = append(out, row)
			}
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Key         string `json:"key"`
			Value       string `json:"value"`
			IsPreview   bool   `json:"is_preview"`
			IsBuildtime bool   `json:"is_buildtime"`
			IsRuntime   *bool  `json:"is_runtime"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Key) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "key is required"})
			return
		}
		isRuntime := true
		if body.IsRuntime != nil {
			isRuntime = *body.IsRuntime
		}
		u := a.Hooks.NewCUID2()
		enc, _ := a.encryptSecretString(body.Value)
		var id int64
		if err := a.Pool.QueryRow(r.Context(), `
			INSERT INTO environment_variables (uuid, key, value, is_preview, is_buildtime, is_runtime, resourceable_type, resourceable_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id
		`, u, strings.TrimSpace(body.Key), enc, body.IsPreview, body.IsBuildtime, isRuntime, meta.MorphType, meta.ID).Scan(&id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "uuid": u, "key": body.Key})

	case http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			UUID  string  `json:"uuid"`
			Key   string  `json:"key"`
			Value *string `json:"value"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid env update"})
			return
		}
		if body.Value != nil {
			enc, _ := a.encryptSecretString(*body.Value)
			_, _ = a.Pool.Exec(r.Context(), `
				UPDATE environment_variables SET value = $1, updated_at = NOW()
				WHERE resourceable_id = $2 AND resourceable_type = ANY($3) AND (uuid = $4 OR key = $5)
			`, enc, meta.ID, morphTypes, body.UUID, body.Key)
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Environment variable updated."})

	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		envUUID := ""
		if len(rest) >= 1 {
			envUUID = rest[0]
		}
		_, _ = a.Pool.Exec(r.Context(), `
			DELETE FROM environment_variables WHERE uuid = $1 AND resourceable_id = $2 AND resourceable_type = ANY($3)
		`, envUUID, meta.ID, morphTypes)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Environment variable deleted."})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (a *App) handleResourceLogsByUUID(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta, overrideUUID string) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionReadSensitive, meta.TeamScopedRef); err != nil {
		writeDomainError(w, err)
		return
	}
	target := meta.UUID
	if overrideUUID != "" {
		target = overrideUUID
	}
	rows, err := a.Pool.Query(r.Context(), `SELECT message FROM go_operation_logs WHERE resource_uuid = $1 ORDER BY seq ASC LIMIT 200`, target)
	var lines []string
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var m string
			if rows.Scan(&m) == nil {
				lines = append(lines, m)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": strings.Join(lines, "\n")})
}

func (a *App) handleResourceClone(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionCreate, meta.TeamScopedRef); err != nil {
		writeDomainError(w, err)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	newName := compat.SanitizeString(body.Name)
	if newName == "" {
		newName = meta.Name + " (Clone)"
	}
	newUUID := a.Hooks.NewCUID2()
	var newID int64

	if meta.Kind == "service" {
		err := a.Pool.QueryRow(r.Context(), `
			INSERT INTO services (uuid, name, description, docker_compose_raw, docker_compose, fqdn, status, environment_id, server_id, destination_id, destination_type)
			SELECT $1, $2, description, docker_compose_raw, docker_compose, fqdn, 'exited', environment_id, server_id, destination_id, destination_type
			FROM services WHERE id = $3 RETURNING id
		`, newUUID, newName, meta.ID).Scan(&newID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `
			INSERT INTO environment_variables (uuid, key, value, is_preview, is_buildtime, is_runtime, is_literal, is_shown_once, resourceable_type, resourceable_id)
			SELECT md5(random()::text || clock_timestamp()::text || id::text), key, value, is_preview, is_buildtime, is_runtime, is_literal, is_shown_once, resourceable_type, $1
			FROM environment_variables WHERE resourceable_type = $2 AND resourceable_id = $3
		`, newID, meta.MorphType, meta.ID)
		_, _ = a.Pool.Exec(r.Context(), `
			INSERT INTO service_applications (uuid, name, human_name, description, fqdn, ports, exposes, status, service_id, exclude_from_status, required_fqdn, is_log_drain_enabled, is_include_timestamps, is_gzip_enabled, is_stripprefix_enabled, is_force_https_enabled, redirect, restart_limit)
			SELECT md5(random()::text || clock_timestamp()::text || id::text), name, human_name, description, fqdn, ports, exposes, 'exited', $1, exclude_from_status, required_fqdn, is_log_drain_enabled, is_include_timestamps, is_gzip_enabled, is_stripprefix_enabled, is_force_https_enabled, redirect, restart_limit
			FROM service_applications WHERE service_id = $2 AND deleted_at IS NULL
		`, newID, meta.ID)
		_, _ = a.Pool.Exec(r.Context(), `
			INSERT INTO service_databases (uuid, name, human_name, description, ports, exposes, status, service_id, exclude_from_status, public_port, is_public, is_log_drain_enabled, is_include_timestamps, is_gzip_enabled, is_stripprefix_enabled, restart_limit)
			SELECT md5(random()::text || clock_timestamp()::text || id::text), name, human_name, description, ports, exposes, 'exited', $1, exclude_from_status, public_port, is_public, is_log_drain_enabled, is_include_timestamps, is_gzip_enabled, is_stripprefix_enabled, restart_limit
			FROM service_databases WHERE service_id = $2 AND deleted_at IS NULL
		`, newID, meta.ID)
		_, _ = a.Pool.Exec(r.Context(), `
			INSERT INTO local_persistent_volumes (uuid, name, mount_path, host_path, resource_type, resource_id, is_preview_suffix_enabled)
			SELECT md5(random()::text || clock_timestamp()::text || id::text), REPLACE(name, $2, $3), mount_path, host_path, 'App\Models\Service', $4, is_preview_suffix_enabled
			FROM local_persistent_volumes WHERE resource_type = 'App\Models\Service' AND resource_id = $1
		`, meta.ID, meta.UUID, newUUID, newID)
		_, _ = a.Pool.Exec(r.Context(), `
			INSERT INTO local_file_volumes (uuid, fs_path, mount_path, content, is_directory, is_host_file, is_preview_suffix_enabled, resource_type, resource_id)
			SELECT md5(random()::text || clock_timestamp()::text || id::text), fs_path, mount_path, content, is_directory, is_host_file, is_preview_suffix_enabled, 'App\Models\Service', $2
			FROM local_file_volumes WHERE resource_type = 'App\Models\Service' AND resource_id = $1
		`, meta.ID, newID)
		_, _ = a.Pool.Exec(r.Context(), `
			INSERT INTO scheduled_tasks (uuid, enabled, name, command, frequency, timeout, use_shell, container, service_id, team_id)
			SELECT md5(random()::text || clock_timestamp()::text || id::text), enabled, name, command, frequency, timeout, use_shell, container, $2, team_id
			FROM scheduled_tasks WHERE service_id = $1
		`, meta.ID, newID)
		_, _ = a.Pool.Exec(r.Context(), `
			INSERT INTO taggables (tag_id, taggable_id, taggable_type)
			SELECT tag_id, $2, 'App\Models\Service'
			FROM taggables WHERE taggable_type = 'App\Models\Service' AND taggable_id = $1
			ON CONFLICT DO NOTHING
		`, meta.ID, newID)
		writeJSON(w, http.StatusCreated, map[string]any{"id": newID, "uuid": newUUID, "name": newName})
		return
	}

	var extraCols string
	switch meta.DBTable {
	case "standalone_postgresqls":
		extraCols = "postgres_user, postgres_password, postgres_db, image, is_public, public_port"
	case "standalone_mysqls":
		extraCols = "mysql_root_password, mysql_user, mysql_password, mysql_database, image, is_public, public_port"
	case "standalone_mariadbs":
		extraCols = "mariadb_root_password, mariadb_user, mariadb_password, mariadb_database, image, is_public, public_port"
	case "standalone_mongodbs":
		extraCols = "mongo_initdb_root_username, mongo_initdb_root_password, mongo_initdb_database, image, is_public, public_port"
	case "standalone_redis":
		extraCols = "redis_password, image, is_public, public_port"
	case "standalone_keydbs":
		extraCols = "keydb_password, image, is_public, public_port"
	case "standalone_dragonflies":
		extraCols = "dragonfly_password, image, is_public, public_port"
	case "standalone_clickhouses":
		extraCols = "clickhouse_admin_user, clickhouse_admin_password, image, is_public, public_port"
	case "standalone_sqlites":
		extraCols = "sqlite_databases, image, is_public"
	}
	err := a.Pool.QueryRow(r.Context(), fmt.Sprintf(`
		INSERT INTO %s (uuid, name, description, %s, status, environment_id, destination_id, destination_type)
		SELECT $1, $2, description, %s, 'exited', environment_id, destination_id, destination_type
		FROM %s WHERE id = $3 RETURNING id
	`, meta.DBTable, extraCols, extraCols, meta.DBTable), newUUID, newName, meta.ID).Scan(&newID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	_, _ = a.Pool.Exec(r.Context(), `
		INSERT INTO environment_variables (uuid, key, value, is_preview, is_buildtime, is_runtime, is_literal, is_shown_once, resourceable_type, resourceable_id)
		SELECT md5(random()::text || clock_timestamp()::text || id::text), key, value, is_preview, is_buildtime, is_runtime, is_literal, is_shown_once, resourceable_type, $1
		FROM environment_variables WHERE resourceable_type = $2 AND resourceable_id = $3
	`, newID, meta.MorphType, meta.ID)
	_, _ = a.Pool.Exec(r.Context(), `
		INSERT INTO local_persistent_volumes (uuid, name, mount_path, host_path, resource_type, resource_id, is_preview_suffix_enabled)
		SELECT md5(random()::text || clock_timestamp()::text || id::text), REPLACE(name, $2, $3), mount_path, host_path, $4, $5, is_preview_suffix_enabled
		FROM local_persistent_volumes WHERE resource_type = $4 AND resource_id = $1
	`, meta.ID, meta.UUID, newUUID, meta.MorphType, newID)
	_, _ = a.Pool.Exec(r.Context(), `
		INSERT INTO local_file_volumes (uuid, fs_path, mount_path, content, is_directory, is_host_file, is_preview_suffix_enabled, resource_type, resource_id)
		SELECT md5(random()::text || clock_timestamp()::text || id::text), fs_path, mount_path, content, is_directory, is_host_file, is_preview_suffix_enabled, $2, $3
		FROM local_file_volumes WHERE resource_type = $2 AND resource_id = $1
	`, meta.ID, meta.MorphType, newID)
	_, _ = a.Pool.Exec(r.Context(), `
		INSERT INTO scheduled_database_backups (uuid, enabled, frequency, timezone, save_s3, database_retention_count, database_id, database_type, s3_storage_id, team_id)
		SELECT md5(random()::text || clock_timestamp()::text || id::text), enabled, frequency, timezone, save_s3, database_retention_count, $1, database_type, s3_storage_id, team_id
		FROM scheduled_database_backups WHERE (database_type = $2 OR database_type = $4) AND database_id = $3
	`, newID, meta.MorphType, meta.ID, meta.DBType)
	_, _ = a.Pool.Exec(r.Context(), `
		INSERT INTO taggables (tag_id, taggable_id, taggable_type)
		SELECT tag_id, $2, $3
		FROM taggables WHERE taggable_type = $3 AND taggable_id = $1
		ON CONFLICT DO NOTHING
	`, meta.ID, newID, meta.MorphType)
	writeJSON(w, http.StatusCreated, map[string]any{"id": newID, "uuid": newUUID, "name": newName, "type": meta.DBType})
}

func (a *App) handleServiceSubResources(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta, subKind string, rest []string) {
	tbl := "service_applications"
	if subKind == "databases" {
		tbl = "service_databases"
	}
	if len(rest) == 0 {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
			return
		}
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		rows, err := a.Pool.Query(r.Context(), fmt.Sprintf(`SELECT id, uuid, name, COALESCE(human_name,''), status FROM %s WHERE service_id = $1 AND deleted_at IS NULL ORDER BY id ASC`, tbl), meta.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id int64
			var u, n, hn, st string
			if rows.Scan(&id, &u, &n, &hn, &st) == nil {
				out = append(out, map[string]any{"id": id, "uuid": u, "name": n, "human_name": hn, "status": st})
			}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	subUUID := rest[0]
	var subID int64
	var name, hn, st string
	err := a.Pool.QueryRow(r.Context(), fmt.Sprintf(`SELECT id, name, COALESCE(human_name,''), status FROM %s WHERE uuid = $1 AND service_id = $2 AND deleted_at IS NULL`, tbl), subUUID, meta.ID).Scan(&subID, &name, &hn, &st)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "service component not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}

	if len(rest) == 1 {
		switch r.Method {
		case http.MethodGet:
			if err := a.Authz.Check(r.Context(), p, access.ActionRead, meta.TeamScopedRef); err != nil {
				writeDomainError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": subID, "uuid": subUUID, "name": name, "human_name": hn, "status": st})
		case http.MethodPatch:
			if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				HumanName   *string `json:"human_name"`
				Description *string `json:"description"`
				FQDN        *string `json:"fqdn"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid update"})
				return
			}
			if body.HumanName != nil {
				hn = compat.SanitizeString(*body.HumanName)
				_, _ = a.Pool.Exec(r.Context(), fmt.Sprintf(`UPDATE %s SET human_name = $1, updated_at = NOW() WHERE id = $2`, tbl), hn, subID)
			}
			if body.FQDN != nil && subKind == "applications" {
				_, _ = a.Pool.Exec(r.Context(), `UPDATE service_applications SET fqdn = $1, updated_at = NOW() WHERE id = $2`, strings.TrimSpace(*body.FQDN), subID)
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": subID, "uuid": subUUID, "name": name, "human_name": hn, "status": st})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}

	action := rest[1]
	switch action {
	case "logs":
		a.handleResourceLogsByUUID(w, r, p, meta, subUUID)
	case "start", "restart", "stop":
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "This endpoint has changed to a POST request."})
			return
		}
		if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		jt := "service_deploy"
		if action == "stop" {
			jt = "service_stop"
		}
		rec, err := a.enqueueResourceLifecycleJob(r.Context(), "service", meta.UUID, meta.ServerUUID, meta.TeamID, jt)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, rec)
	case "imports":
		if subKind == "databases" {
			a.handleDatabaseImports(w, r, p, meta, rest[2:])
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func (a *App) handleDatabaseBackupsSubRoutes(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta, rest []string) {
	schedUUID := rest[0]
	var schedID int64
	if err := a.Pool.QueryRow(r.Context(), `SELECT id FROM scheduled_database_backups WHERE uuid = $1 AND database_id = $2 AND team_id = $3`,
		schedUUID, meta.ID, meta.TeamID).Scan(&schedID); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "backup schedule not found"})
		return
	}

	if len(rest) == 2 && rest[1] == "executions" && r.Method == http.MethodGet {
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		rows, err := a.Pool.Query(r.Context(), `
			SELECT id, uuid, status, COALESCE(message,''), COALESCE(size, 0), COALESCE(filename,''), created_at
			FROM scheduled_database_backup_executions WHERE scheduled_database_backup_id = $1 ORDER BY id DESC LIMIT 50
		`, schedID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id, sz int64
			var u, st, msg, fn string
			var ca time.Time
			if rows.Scan(&id, &u, &st, &msg, &sz, &fn, &ca) == nil {
				out = append(out, map[string]any{"id": id, "uuid": u, "status": st, "message": msg, "size": sz, "filename": fn, "created_at": ca})
			}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	if len(rest) == 3 && rest[1] == "executions" && r.Method == http.MethodDelete {
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		tag, _ := a.Pool.Exec(r.Context(), `DELETE FROM scheduled_database_backup_executions WHERE uuid = $1 AND scheduled_database_backup_id = $2`, rest[2], schedID)
		if tag.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "backup execution not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Backup execution deleted."})
		return
	}

	if len(rest) == 1 && r.Method == http.MethodPatch {
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Enabled   *bool   `json:"enabled"`
			Frequency *string `json:"frequency"`
			SaveS3    *bool   `json:"save_s3"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid backup schedule update"})
			return
		}
		if body.Enabled != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE scheduled_database_backups SET enabled = $1, updated_at = NOW() WHERE id = $2`, *body.Enabled, schedID)
		}
		if body.Frequency != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE scheduled_database_backups SET frequency = $1, updated_at = NOW() WHERE id = $2`, strings.TrimSpace(*body.Frequency), schedID)
		}
		if body.SaveS3 != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE scheduled_database_backups SET save_s3 = $1, updated_at = NOW() WHERE id = $2`, *body.SaveS3, schedID)
		}
		writeJSON(w, http.StatusOK, map[string]any{"uuid": schedUUID, "message": "Backup schedule updated."})
		return
	}

	if len(rest) == 1 && r.Method == http.MethodDelete {
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM scheduled_database_backups WHERE id = $1`, schedID)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Backup schedule deleted."})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
}

func (a *App) handleDatabaseImports(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta, rest []string) {
	if len(rest) == 1 && rest[0] == "uploads" && r.Method == http.MethodPost {
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		a.saveUploadedBackupFile(w, r, meta)
		return
	}
	if len(rest) == 0 && r.Method == http.MethodPost {
		if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Filepath string `json:"filepath"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		impUUID := a.Hooks.NewCUID2()
		var actID int64
		_ = a.Pool.QueryRow(r.Context(), `
			INSERT INTO go_upload_imports (uuid, team_id, resource_type, resource_uuid, filename, filepath, status)
			VALUES ($1, $2, $3, $4, 'restore', $5, 'queued') RETURNING id
		`, impUUID, meta.TeamID, meta.Kind, meta.UUID, body.Filepath).Scan(&actID)
		opID := "import-" + impUUID
		tx, err := a.Pool.Begin(r.Context())
		if err == nil {
			defer func() { _ = tx.Rollback(r.Context()) }()
			_ = a.JobCoord.AdmitOperationInTx(r.Context(), tx, jobs.DurableJobSpec{
				JobOperation: jobs.JobOperation{
					JobID: "job-" + opID, OperationID: opID, AttemptID: "att-0",
					ResourceUUID: meta.UUID, ServerUUID: meta.ServerUUID, TeamID: meta.TeamID,
					Status: jobs.StatusQueued, MaxAttempts: 1,
				},
				JobType: "db_import",
				Payload: map[string]any{"activity_id": actID, "filepath": body.Filepath, "db_type": meta.DBType},
			})
			_ = tx.Commit(r.Context())
			_, _ = a.Dispatcher.FlushPending(r.Context())
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"message": "Database import queued.", "activity_id": actID, "operation_id": opID})
		return
	}
	if len(rest) == 1 && r.Method == http.MethodGet {
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		var id int64
		var u, st, msg string
		err := a.Pool.QueryRow(r.Context(), `
			SELECT id, uuid, status, COALESCE(log,'') FROM go_upload_imports
			WHERE team_id = $1 AND resource_uuid = $2 AND (id::text = $3 OR uuid = $3)
		`, meta.TeamID, meta.UUID, rest[0]).Scan(&id, &u, &st, &msg)
		if errors.Is(err, pgx.ErrNoRows) {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "import activity not found"})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"activity_id": id, "uuid": u, "status": st, "log": msg})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
}
