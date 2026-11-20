package controlplane

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/applications"
	"github.com/coollabsio/coolify/backend/internal/domains/projects"
	"github.com/coollabsio/coolify/backend/internal/domains/servers"
	"github.com/jackc/pgx/v5"
)

// ---- Destinations (/api/v1/destinations, /api/v1/servers/{uuid}/destinations) ----

func (a *App) handleDestinations(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/destinations"), "/")
	if trimmed == "" {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
			return
		}
		teamID := activeTeamForPrincipal(p)
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
			Type: "server", TeamID: &teamID, APISegment: "destinations",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		rows, err := a.Pool.Query(r.Context(), `
			SELECT sd.id, sd.uuid, sd.name, sd.network, sd.server_id, s.uuid, s.team_id
			FROM standalone_dockers sd
			JOIN servers s ON s.id = sd.server_id
			WHERE s.team_id = $1 AND s.deleted_at IS NULL
			ORDER BY sd.id ASC
		`, teamID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id, srvID, tid int64
			var u, n, netw, srvUUID string
			if rows.Scan(&id, &u, &n, &netw, &srvID, &srvUUID, &tid) == nil {
				out = append(out, map[string]any{
					"id": id, "uuid": u, "name": n, "network": netw, "type": "standalone", "server_id": srvID, "server_uuid": srvUUID, "team_id": tid,
				})
			}
		}
		if srows, serr := a.Pool.Query(r.Context(), `
			SELECT sw.id, sw.uuid, sw.name, sw.server_id, s.uuid, s.team_id
			FROM swarm_dockers sw
			JOIN servers s ON s.id = sw.server_id
			WHERE s.team_id = $1 AND s.deleted_at IS NULL
			ORDER BY sw.id ASC
		`, teamID); serr == nil {
			for srows.Next() {
				var id, srvID, tid int64
				var u, n, srvUUID string
				if srows.Scan(&id, &u, &n, &srvID, &srvUUID, &tid) == nil {
					out = append(out, map[string]any{
						"id": id, "uuid": u, "name": n, "network": "coolify-overlay", "type": "swarm", "server_id": srvID, "server_uuid": srvUUID, "team_id": tid,
					})
				}
			}
			srows.Close()
		}
		writeJSON(w, http.StatusOK, out)
		return
	}

	destUUID := strings.Split(trimmed, "/")[0]
	var id, srvID, teamID int64
	var name, network, srvUUID string
	err := a.Pool.QueryRow(r.Context(), `
		SELECT sd.id, sd.name, sd.network, sd.server_id, s.uuid, s.team_id
		FROM standalone_dockers sd
		JOIN servers s ON s.id = sd.server_id
		WHERE sd.uuid = $1 AND s.deleted_at IS NULL
	`, destUUID).Scan(&id, &name, &network, &srvID, &srvUUID, &teamID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "destination not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	ref := access.ResourceRef{Type: "server", ID: srvID, UUID: srvUUID, TeamID: &teamID, APISegment: "destinations"}
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "uuid": destUUID, "name": name, "network": network, "server_id": srvID, "server_uuid": srvUUID, "team_id": teamID,
		})
	case http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Name    *string `json:"name"`
			Network *string `json:"network"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid destination update"})
			return
		}
		if body.Name != nil && strings.TrimSpace(*body.Name) != "" {
			name = compat.SanitizeString(*body.Name)
		}
		if body.Network != nil && strings.TrimSpace(*body.Network) != "" {
			network = compat.SanitizeString(*body.Network)
		}
		_, _ = a.Pool.Exec(r.Context(), `UPDATE standalone_dockers SET name = $1, network = $2, updated_at = NOW() WHERE id = $3`, name, network, id)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "uuid": destUUID, "name": name, "network": network, "server_id": srvID})
	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if id == 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "cannot delete sentinel standalone_dockers id=0"})
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM standalone_dockers WHERE id = $1`, id)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Destination deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (a *App) handleServerCreateDestination(w http.ResponseWriter, r *http.Request, p auth.Principal, srv servers.Server) {
	if srv.IsTransferredAway() {
		writeDomainError(w, servers.ErrTransferredServer)
		return
	}
	tid := srv.TeamID
	if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{
		Type: "server", ID: srv.ID, UUID: srv.UUID, TeamID: &tid, APISegment: "destinations",
	}); err != nil {
		writeDomainError(w, err)
		return
	}
	var body struct {
		Name    string `json:"name"`
		Network string `json:"network"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "destination name is required."})
		return
	}
	if strings.TrimSpace(body.Network) == "" {
		body.Network = "coolify"
	}
	destUUID := a.Hooks.NewCUID2()
	var id int64
	err := a.Pool.QueryRow(r.Context(), `
		INSERT INTO standalone_dockers (uuid, name, network, server_id)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, destUUID, compat.SanitizeString(body.Name), compat.SanitizeString(body.Network), srv.ID).Scan(&id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"id": id, "uuid": destUUID, "name": compat.SanitizeString(body.Name), "network": compat.SanitizeString(body.Network), "server_id": srv.ID,
	})
}

// ---- Application Previews (/api/v1/applications/{uuid}/previews[/{pr_id}]) ----

func (a *App) handleApplicationPreviews(w http.ResponseWriter, r *http.Request, p auth.Principal, appUUID string, prSegment string) {
	appRec, err := a.AppSvc.GetByUUID(r.Context(), p, appUUID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	tid := appRec.TeamID
	ref := access.ResourceRef{
		Type: "application", ID: appRec.ID, UUID: appRec.UUID, ProjectUUID: appRec.ProjectUUID, EnvironmentUUID: appRec.EnvironmentUUID,
		TeamID: &tid, APISegment: "applications",
	}

	if prSegment == "" {
		switch r.Method {
		case http.MethodGet:
			rows, err := a.Pool.Query(r.Context(), `
				SELECT id, uuid, pull_request_id, COALESCE(pull_request_html_url,''), COALESCE(fqdn,''), status
				FROM application_previews WHERE application_id = $1 ORDER BY pull_request_id ASC
			`, appRec.ID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			defer rows.Close()
			out := make([]map[string]any, 0)
			for rows.Next() {
				var id, prID int64
				var u, url, fqdn, st string
				if rows.Scan(&id, &u, &prID, &url, &fqdn, &st) == nil {
					out = append(out, map[string]any{
						"id": id, "uuid": u, "pull_request_id": prID, "pull_request_html_url": url, "fqdn": fqdn, "status": st,
					})
				}
			}
			writeJSON(w, http.StatusOK, out)
		case http.MethodPost:
			if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, ref); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				PullRequestID      int64  `json:"pull_request_id"`
				PullRequestHTMLURL string `json:"pull_request_html_url"`
				FQDN               string `json:"fqdn"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PullRequestID <= 0 {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "pull_request_id (>0) is required."})
				return
			}
			prevUUID := a.Hooks.NewCUID2()
			var id int64
			err := a.Pool.QueryRow(r.Context(), `
				INSERT INTO application_previews (uuid, application_id, pull_request_id, pull_request_html_url, fqdn, status)
				VALUES ($1, $2, $3, $4, $5, 'queued')
				RETURNING id
			`, prevUUID, appRec.ID, body.PullRequestID, strings.TrimSpace(body.PullRequestHTMLURL), strings.TrimSpace(body.FQDN)).Scan(&id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{
				"id": id, "uuid": prevUUID, "pull_request_id": body.PullRequestID, "fqdn": body.FQDN, "status": "queued",
			})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}

	prID, err := strconv.ParseInt(prSegment, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid pull_request_id"})
		return
	}
	var id int64
	var prevUUID, prURL, fqdn, status string
	err = a.Pool.QueryRow(r.Context(), `
		SELECT id, uuid, COALESCE(pull_request_html_url,''), COALESCE(fqdn,''), status
		FROM application_previews WHERE application_id = $1 AND pull_request_id = $2
	`, appRec.ID, prID).Scan(&id, &prevUUID, &prURL, &fqdn, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "preview not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "uuid": prevUUID, "pull_request_id": prID, "pull_request_html_url": prURL, "fqdn": fqdn, "status": status,
		})
	case http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			FQDN *string `json:"fqdn"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid preview update"})
			return
		}
		if body.FQDN != nil {
			fqdn = strings.TrimSpace(*body.FQDN)
		}
		_, _ = a.Pool.Exec(r.Context(), `UPDATE application_previews SET fqdn = $1, updated_at = NOW() WHERE id = $2`, fqdn, id)
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "uuid": prevUUID, "pull_request_id": prID, "fqdn": fqdn, "status": status})
	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM application_previews WHERE id = $1`, id)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Preview deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Application Bulk Envs & Clone ----

func (a *App) handleApplicationBulkEnvs(w http.ResponseWriter, r *http.Request, p auth.Principal, appUUID string) {
	if r.Method != http.MethodPatch && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	var body struct {
		Data []applications.EnvVarInput `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid bulk env payload"})
		return
	}
	saved := make([]applications.EnvVarDTO, 0, len(body.Data))
	for _, item := range body.Data {
		ev, err := a.AppSvc.SetEnvVar(r.Context(), p, appUUID, item)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		saved = append(saved, ev)
	}
	writeJSON(w, http.StatusOK, saved)
}

func (a *App) handleApplicationClone(w http.ResponseWriter, r *http.Request, p auth.Principal, appUUID string) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	src, err := a.AppSvc.GetByUUID(r.Context(), p, appUUID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	tid := src.TeamID
	if err := a.Authz.Check(r.Context(), p, access.ActionCreate, access.ResourceRef{
		Type: "application", ProjectUUID: src.ProjectUUID, EnvironmentUUID: src.EnvironmentUUID, TeamID: &tid, APISegment: "applications",
	}); err != nil {
		writeDomainError(w, err)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	newName := strings.TrimSpace(body.Name)
	if newName == "" {
		newName = src.Name + " (Clone)"
	}

	cloned, err := a.AppSvc.Create(r.Context(), p, applications.CreateApplicationInput{
		Name:                    newName,
		Description:             src.Description,
		EnvironmentUUID:         src.EnvironmentUUID,
		ServerUUID:              src.ServerUUID,
		GitRepository:           src.GitRepository,
		GitBranch:               src.GitBranch,
		BuildPack:               src.BuildPack,
		DockerRegistryImageName: src.DockerRegistryImageName,
		DockerRegistryImageTag:  src.DockerRegistryImageTag,
		DockerfileLocation:      src.DockerfileLocation,
		DockerComposeRaw:        src.DockerComposeRaw,
		PortsExposes:            src.PortsExposes,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	// Copy exact encrypted environment_variables rows without stripping whitespace or decrypting locked secrets unnecessarily
	rows, err := a.Pool.Query(r.Context(), `
		SELECT key, value, is_preview, is_buildtime, is_runtime, is_literal, is_shown_once
		FROM environment_variables
		WHERE resourceable_id = $1 AND resourceable_type = 'App\Models\Application'
	`, src.ID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var v *string
		var pr, bt, rt, lit, once bool
		if err := rows.Scan(&k, &v, &pr, &bt, &rt, &lit, &once); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		if _, err := a.Pool.Exec(r.Context(), `
			INSERT INTO environment_variables (uuid, key, value, is_preview, is_buildtime, is_runtime, is_literal, is_shown_once, resourceable_type, resourceable_id)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'App\Models\Application', $9)
		`, a.Hooks.NewCUID2(), k, v, pr, bt, rt, lit, once, cloned.ID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
	}

	// Copy application_settings, volumes, scheduled_tasks, and tags for full tech-stack clone fidelity
	_, _ = a.Pool.Exec(r.Context(), `
		UPDATE application_settings dst SET
			is_static = src.is_static,
			is_spa = src.is_spa,
			is_git_submodules_enabled = src.is_git_submodules_enabled,
			is_git_lfs_enabled = src.is_git_lfs_enabled,
			is_git_shallow_clone_enabled = src.is_git_shallow_clone_enabled,
			is_auto_deploy_enabled = src.is_auto_deploy_enabled,
			is_force_https_enabled = src.is_force_https_enabled,
			is_debug_enabled = src.is_debug_enabled,
			is_preview_deployments_enabled = src.is_preview_deployments_enabled,
			is_pr_deployments_public_enabled = src.is_pr_deployments_public_enabled,
			is_log_drain_enabled = src.is_log_drain_enabled,
			is_gpu_enabled = src.is_gpu_enabled,
			gpu_driver = src.gpu_driver,
			gpu_count = src.gpu_count,
			gpu_device_ids = src.gpu_device_ids,
			gpu_options = src.gpu_options,
			is_include_timestamps = src.is_include_timestamps,
			is_swarm_only_worker_nodes = src.is_swarm_only_worker_nodes,
			is_raw_compose_deployment_enabled = src.is_raw_compose_deployment_enabled,
			is_build_server_enabled = src.is_build_server_enabled,
			is_consistent_container_name_enabled = src.is_consistent_container_name_enabled,
			is_gzip_enabled = src.is_gzip_enabled,
			is_stripprefix_enabled = src.is_stripprefix_enabled,
			is_permanent_redirect_enabled = src.is_permanent_redirect_enabled,
			connect_to_docker_network = src.connect_to_docker_network,
			custom_internal_name = src.custom_internal_name,
			custom_container_name_prefix = src.custom_container_name_prefix,
			is_container_label_escape_enabled = src.is_container_label_escape_enabled,
			is_container_label_readonly_enabled = src.is_container_label_readonly_enabled,
			is_preserve_repository_enabled = src.is_preserve_repository_enabled,
			disable_build_cache = src.disable_build_cache,
			use_build_secrets = src.use_build_secrets,
			redact_all_env_values_in_logs = src.redact_all_env_values_in_logs,
			inject_build_args_to_dockerfile = src.inject_build_args_to_dockerfile,
			include_source_commit_in_build = src.include_source_commit_in_build,
			include_network_hosts_in_build = src.include_network_hosts_in_build,
			is_env_sorting_enabled = src.is_env_sorting_enabled,
			include_coolify_default_variables = src.include_coolify_default_variables,
			traefik_certificate_resolver = src.traefik_certificate_resolver,
			docker_images_to_keep = src.docker_images_to_keep,
			stop_grace_period = src.stop_grace_period,
			rolling_update_drain_seconds = src.rolling_update_drain_seconds,
			is_image_scan_enabled = src.is_image_scan_enabled,
			updated_at = NOW()
		FROM application_settings src
		WHERE src.application_id = $1 AND dst.application_id = $2
	`, src.ID, cloned.ID)

	_, _ = a.Pool.Exec(r.Context(), `
		INSERT INTO local_persistent_volumes (uuid, name, mount_path, host_path, resource_type, resource_id, is_preview_suffix_enabled)
		SELECT md5(random()::text || clock_timestamp()::text || id::text),
		       REPLACE(name, $2, $3),
		       mount_path, host_path, 'App\Models\Application', $4, is_preview_suffix_enabled
		FROM local_persistent_volumes
		WHERE resource_type = 'App\Models\Application' AND resource_id = $1
	`, src.ID, src.UUID, cloned.UUID, cloned.ID)

	_, _ = a.Pool.Exec(r.Context(), `
		INSERT INTO local_file_volumes (uuid, fs_path, mount_path, content, is_directory, is_host_file, is_preview_suffix_enabled, resource_type, resource_id)
		SELECT md5(random()::text || clock_timestamp()::text || id::text),
		       fs_path, mount_path, content, is_directory, is_host_file, is_preview_suffix_enabled, 'App\Models\Application', $2
		FROM local_file_volumes
		WHERE resource_type = 'App\Models\Application' AND resource_id = $1
	`, src.ID, cloned.ID)

	_, _ = a.Pool.Exec(r.Context(), `
		INSERT INTO scheduled_tasks (uuid, enabled, name, command, frequency, timeout, use_shell, container, application_id, team_id)
		SELECT md5(random()::text || clock_timestamp()::text || id::text),
		       enabled, name, command, frequency, timeout, use_shell, container, $2, team_id
		FROM scheduled_tasks
		WHERE application_id = $1
	`, src.ID, cloned.ID)

	_, _ = a.Pool.Exec(r.Context(), `
		INSERT INTO taggables (tag_id, taggable_id, taggable_type)
		SELECT tag_id, $2, 'App\Models\Application'
		FROM taggables
		WHERE taggable_type = 'App\Models\Application' AND taggable_id = $1
		ON CONFLICT DO NOTHING
	`, src.ID, cloned.ID)

	writeJSON(w, http.StatusCreated, cloned)
}

// ---- Scoped Shared Environment Variables (team / project / server) ----

func (a *App) handleScopedSharedEnvs(w http.ResponseWriter, r *http.Request, p auth.Principal, scope string, teamID int64, projectID *int64, serverID *int64, envIDSegment string) {
	ref := access.ResourceRef{Type: "team", ID: teamID, TeamID: &teamID, APISegment: "envs"}
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		canReadSecrets := a.Authz.Check(r.Context(), p, access.ActionReadSensitive, ref) == nil
		var rows pgx.Rows
		var err error
		switch scope {
		case "team":
			rows, err = a.Pool.Query(r.Context(), `SELECT id, uuid, key, value, is_shown_once FROM shared_environment_variables WHERE team_id = $1 AND type = 'team' ORDER BY id ASC`, teamID)
		case "project":
			rows, err = a.Pool.Query(r.Context(), `SELECT id, uuid, key, value, is_shown_once FROM shared_environment_variables WHERE team_id = $1 AND type = 'project' AND project_id = $2 ORDER BY id ASC`, teamID, *projectID)
		case "server":
			rows, err = a.Pool.Query(r.Context(), `SELECT id, uuid, key, value, is_shown_once FROM shared_environment_variables WHERE team_id = $1 AND type = 'server' AND server_id = $2 ORDER BY id ASC`, teamID, *serverID)
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]projects.SharedVariable, 0)
		for rows.Next() {
			var sv projects.SharedVariable
			var rawVal *string
			if rows.Scan(&sv.ID, &sv.UUID, &sv.Key, &rawVal, &sv.IsShownOnce) == nil {
				sv.Type = scope
				sv.TeamID = teamID
				sv.ProjectID = projectID
				sv.ServerID = serverID
				if rawVal != nil && canReadSecrets && !sv.IsShownOnce {
					if a.Cipher != nil {
						if dec, decErr := a.Cipher.DecryptLaravelPayload(*rawVal, false); decErr == nil {
							sv.Value = string(dec)
						}
					} else {
						sv.Value = *rawVal
					}
				}
				out = append(out, sv)
			}
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost, http.MethodPatch:
		var in projects.SharedVariable
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid shared variable payload"})
			return
		}
		if r.Method == http.MethodPatch && envIDSegment != "" && strings.TrimSpace(in.Key) == "" {
			var existingKey string
			var lookupErr error
			if idNum, err := strconv.ParseInt(envIDSegment, 10, 64); err == nil {
				lookupErr = a.Pool.QueryRow(r.Context(), `SELECT key FROM shared_environment_variables WHERE id = $1 AND team_id = $2`, idNum, teamID).Scan(&existingKey)
			} else {
				lookupErr = a.Pool.QueryRow(r.Context(), `SELECT key FROM shared_environment_variables WHERE uuid = $1 AND team_id = $2`, envIDSegment, teamID).Scan(&existingKey)
			}
			if lookupErr != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "shared variable not found"})
				return
			}
			in.Key = existingKey
		}
		in.Type = scope
		in.TeamID = teamID
		in.ProjectID = projectID
		in.ServerID = serverID
		saved, err := a.ProjectSvc.UpsertSharedVariable(r.Context(), p, in)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		status := http.StatusCreated
		if r.Method == http.MethodPatch {
			status = http.StatusOK
		}
		writeJSON(w, status, saved)
	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if envIDSegment == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "env_id required"})
			return
		}
		if idNum, err := strconv.ParseInt(envIDSegment, 10, 64); err == nil {
			_, _ = a.Pool.Exec(r.Context(), `DELETE FROM shared_environment_variables WHERE id = $1 AND team_id = $2`, idNum, teamID)
		} else {
			_, _ = a.Pool.Exec(r.Context(), `DELETE FROM shared_environment_variables WHERE uuid = $1 AND team_id = $2`, envIDSegment, teamID)
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Shared environment variable deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}
