package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/applications"
	"github.com/coollabsio/coolify/backend/internal/domains/notifications"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/coollabsio/coolify/backend/internal/realtime"
	"github.com/jackc/pgx/v5"
)

// ---- Instance API / MCP Toggle Endpoints ----

func (a *App) handleInstanceToggle(field string, value bool) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		if r.Method != http.MethodPost && r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
			return
		}
		rootTeam := int64(0)
		if err := a.Authz.Check(r.Context(), p, access.ActionManageInstance, access.ResourceRef{
			Type: "instance_settings", ID: 0, TeamID: &rootTeam, APISegment: "settings",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		var sqlStmt string
		switch field {
		case "is_api_enabled":
			sqlStmt = `UPDATE instance_settings SET is_api_enabled = $1, updated_at = NOW() WHERE id = 0`
		case "is_mcp_server_enabled":
			sqlStmt = `UPDATE instance_settings SET is_mcp_server_enabled = $1, updated_at = NOW() WHERE id = 0`
		default:
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "unsupported setting"})
			return
		}
		if _, err := a.Pool.Exec(r.Context(), sqlStmt, value); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"message": "Updated.", field: value})
	}
}

// ---- Current Team (/api/v1/team, /api/v1/team/members) ----

func (a *App) handleCurrentTeam(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	teamID := activeTeamForPrincipal(p)
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/team"), "/")
	if trimmed == "members" && r.Method == http.MethodGet {
		members, err := a.TeamSvc.ListMembers(r.Context(), p, teamID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, members)
		return
	}
	if trimmed == "envs" || strings.HasPrefix(trimmed, "envs/") {
		envIDSeg := strings.TrimPrefix(strings.TrimPrefix(trimmed, "envs"), "/")
		a.handleScopedSharedEnvs(w, r, p, "team", teamID, nil, nil, envIDSeg)
		return
	}
	if trimmed == "" {
		switch r.Method {
		case http.MethodGet:
			a.serveTeamByID(w, r, p, teamID)
		case http.MethodPatch:
			if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{
				Type: "team", ID: teamID, TeamID: &teamID, APISegment: "teams",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				Name        *string `json:"name"`
				Description *string `json:"description"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid team update payload"})
				return
			}
			if body.Name != nil && strings.TrimSpace(*body.Name) != "" {
				_, _ = a.Pool.Exec(r.Context(), `UPDATE teams SET name = $1, updated_at = NOW() WHERE id = $2`, compat.SanitizeString(*body.Name), teamID)
			}
			if body.Description != nil {
				_, _ = a.Pool.Exec(r.Context(), `UPDATE teams SET description = $1, updated_at = NOW() WHERE id = $2`, compat.SanitizeString(*body.Description), teamID)
			}
			a.serveTeamByID(w, r, p, teamID)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func (a *App) serveTeamByID(w http.ResponseWriter, r *http.Request, p auth.Principal, teamID int64) {
	if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
		Type: "team", ID: teamID, TeamID: &teamID, APISegment: "teams",
	}); err != nil {
		writeDomainError(w, err)
		return
	}
	var id int64
	var name, desc string
	var personal bool
	err := a.Pool.QueryRow(r.Context(), `SELECT id, name, COALESCE(description,''), personal_team FROM teams WHERE id = $1`, teamID).
		Scan(&id, &name, &desc, &personal)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "team not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "name": name, "description": desc, "personal_team": personal,
	})
}

// ---- Unified Team Resources (/api/v1/resources) ----

func (a *App) handleResources(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	teamID := activeTeamForPrincipal(p)
	if err := a.Authz.CheckList(r.Context(), p, teamID, "resources"); err != nil {
		writeDomainError(w, err)
		return
	}
	out := make([]map[string]any, 0)
	if apps, err := a.AppSvc.ListByTeam(r.Context(), p, teamID); err == nil {
		for _, app := range apps {
			out = append(out, map[string]any{
				"id": app.ID, "uuid": app.UUID, "name": app.Name, "type": "application", "status": app.Status,
			})
		}
	}
	if svcs, err := a.ServiceSvc.ListByTeam(r.Context(), p, teamID); err == nil {
		for _, svc := range svcs {
			out = append(out, map[string]any{
				"id": svc.ID, "uuid": svc.UUID, "name": svc.Name, "type": "service", "status": svc.Status,
			})
		}
	}
	if dbs, err := a.DatabaseSvc.ListByTeam(r.Context(), p, teamID); err == nil {
		for _, db := range dbs {
			out = append(out, map[string]any{
				"id": db.ID, "uuid": db.UUID, "name": db.Name, "type": db.Type, "status": db.Status,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// ---- Private Key Individual Resource (/api/v1/security/keys/{uuid}) ----

func (a *App) handlePrivateKeyByUUID(w http.ResponseWriter, r *http.Request, p auth.Principal, keyUUID string) {
	var id, teamID int64
	var name, desc string
	var isGit bool
	err := a.Pool.QueryRow(r.Context(), `
		SELECT id, team_id, name, COALESCE(description,''), is_git_related FROM private_keys WHERE uuid = $1
	`, keyUUID).Scan(&id, &teamID, &name, &desc, &isGit)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "private key not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	ref := access.ResourceRef{Type: "private_key", ID: id, UUID: keyUUID, TeamID: &teamID, APISegment: "security"}
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionUseTeamCredentials, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "uuid": keyUUID, "name": name, "description": desc, "is_git_related": isGit, "team_id": teamID,
		})
	case http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid key update"})
			return
		}
		if body.Name != nil && strings.TrimSpace(*body.Name) != "" {
			name = compat.SanitizeString(*body.Name)
		}
		if body.Description != nil {
			desc = compat.SanitizeString(*body.Description)
		}
		_, err := a.Pool.Exec(r.Context(), `UPDATE private_keys SET name = $1, description = $2, updated_at = NOW() WHERE id = $3`, name, desc, id)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "uuid": keyUUID, "name": name, "description": desc, "team_id": teamID})
	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if id == 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "cannot delete sentinel localhost private key id=0"})
			return
		}
		if _, err := a.Pool.Exec(r.Context(), `DELETE FROM private_keys WHERE id = $1`, id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Private key deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- S3 Storages (/api/v1/s3-storages) ----

func (a *App) handleS3Storages(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/s3-storages"), "/")
	if trimmed == "" {
		teamID := activeTeamForPrincipal(p)
		switch r.Method {
		case http.MethodGet:
			if err := a.Authz.Check(r.Context(), p, access.ActionUseTeamCredentials, access.ResourceRef{
				Type: "s3_storage", TeamID: &teamID, APISegment: "s3-storages",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			rows, err := a.Pool.Query(r.Context(), `
				SELECT id, uuid, name, COALESCE(description,''), region, bucket, endpoint, is_usable, team_id
				FROM s3_storages WHERE team_id = $1 ORDER BY id ASC
			`, teamID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			defer rows.Close()
			out := make([]map[string]any, 0)
			for rows.Next() {
				var id, tid int64
				var uuid, name, desc, region, bucket, endpoint string
				var usable bool
				if err := rows.Scan(&id, &uuid, &name, &desc, &region, &bucket, &endpoint, &usable, &tid); err == nil {
					out = append(out, map[string]any{
						"id": id, "uuid": uuid, "name": name, "description": desc,
						"region": region, "bucket": bucket, "endpoint": endpoint, "is_usable": usable, "team_id": tid,
					})
				}
			}
			writeJSON(w, http.StatusOK, out)
		case http.MethodPost:
			if err := a.Authz.Check(r.Context(), p, access.ActionCreate, access.ResourceRef{
				Type: "s3_storage", TeamID: &teamID, APISegment: "s3-storages",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				Name        string `json:"name"`
				Description string `json:"description"`
				Region      string `json:"region"`
				Key         string `json:"key"`
				Secret      string `json:"secret"`
				Bucket      string `json:"bucket"`
				Endpoint    string `json:"endpoint"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" || strings.TrimSpace(body.Bucket) == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "name and bucket are required."})
				return
			}
			if body.Region == "" {
				body.Region = "us-east-1"
			}
			encKey, encSecret := body.Key, body.Secret
			if a.Cipher != nil {
				if body.Key != "" {
					if e, err := a.Cipher.EncryptLaravelPayload([]byte(body.Key), false); err == nil {
						encKey = e
					}
				}
				if body.Secret != "" {
					if e, err := a.Cipher.EncryptLaravelPayload([]byte(body.Secret), false); err == nil {
						encSecret = e
					}
				}
			}
			uuid := a.Hooks.NewCUID2()
			var id int64
			err := a.Pool.QueryRow(r.Context(), `
				INSERT INTO s3_storages (uuid, name, description, region, key, secret, bucket, endpoint, team_id)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
				RETURNING id
			`, uuid, compat.SanitizeString(body.Name), compat.SanitizeString(body.Description), body.Region, encKey, encSecret, body.Bucket, body.Endpoint, teamID).Scan(&id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"id": id, "uuid": uuid, "name": body.Name, "bucket": body.Bucket, "team_id": teamID})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}

	s3UUID := strings.Split(trimmed, "/")[0]
	var id, teamID int64
	var name, desc, region, bucket, endpoint string
	var usable bool
	err := a.Pool.QueryRow(r.Context(), `
		SELECT id, team_id, name, COALESCE(description,''), region, bucket, endpoint, is_usable
		FROM s3_storages WHERE uuid = $1
	`, s3UUID).Scan(&id, &teamID, &name, &desc, &region, &bucket, &endpoint, &usable)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "s3 storage not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	ref := access.ResourceRef{Type: "s3_storage", ID: id, UUID: s3UUID, TeamID: &teamID, APISegment: "s3-storages"}
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionUseTeamCredentials, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "uuid": s3UUID, "name": name, "description": desc,
			"region": region, "bucket": bucket, "endpoint": endpoint, "is_usable": usable, "team_id": teamID,
		})
	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM s3_storages WHERE id = $1`, id)
		writeJSON(w, http.StatusOK, map[string]string{"message": "S3 storage deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Tags (/api/v1/tags) ----

func (a *App) handleTags(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/tags"), "/")
	teamID := activeTeamForPrincipal(p)
	if trimmed == "" {
		switch r.Method {
		case http.MethodGet:
			if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
				Type: "tag", TeamID: &teamID, APISegment: "tags",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			rows, err := a.Pool.Query(r.Context(), `SELECT id, name, team_id FROM tags WHERE team_id = $1 ORDER BY id ASC`, teamID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			defer rows.Close()
			out := make([]map[string]any, 0)
			for rows.Next() {
				var id, tid int64
				var name string
				if err := rows.Scan(&id, &name, &tid); err == nil {
					out = append(out, map[string]any{"id": id, "name": name, "team_id": tid})
				}
			}
			writeJSON(w, http.StatusOK, out)
		case http.MethodPost:
			if err := a.Authz.Check(r.Context(), p, access.ActionCreate, access.ResourceRef{
				Type: "tag", TeamID: &teamID, APISegment: "tags",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "tag name is required."})
				return
			}
			var id int64
			err := a.Pool.QueryRow(r.Context(), `INSERT INTO tags (name, team_id) VALUES ($1, $2) RETURNING id`, compat.SanitizeString(body.Name), teamID).Scan(&id)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"id": id, "name": compat.SanitizeString(body.Name), "team_id": teamID})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}

	tagID, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid tag id"})
		return
	}
	var ownerTeam int64
	if err := a.Pool.QueryRow(r.Context(), `SELECT team_id FROM tags WHERE id = $1`, tagID).Scan(&ownerTeam); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "tag not found"})
		return
	}
	ref := access.ResourceRef{Type: "tag", ID: tagID, TeamID: &ownerTeam, APISegment: "tags"}
	if r.Method == http.MethodDelete {
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM tags WHERE id = $1`, tagID)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Tag deleted."})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
}

// ---- Lifecycle Job Enqueue Helper (app_stop / service_stop / db_stop) ----

func (a *App) enqueueResourceLifecycleJob(ctx context.Context, resType, resUUID, serverUUID string, teamID int64, jobType string) (map[string]any, error) {
	opID := jobType + "-" + a.Hooks.NewCUID2()
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := a.JobCoord.AdmitOperationInTx(ctx, tx, jobs.DurableJobSpec{
		JobOperation: jobs.JobOperation{
			JobID:        "job-" + opID,
			OperationID:  opID,
			AttemptID:    "att-0",
			ResourceUUID: resUUID,
			ServerUUID:   serverUUID,
			TeamID:       teamID,
			Status:       jobs.StatusQueued,
			MaxAttempts:  3,
		},
		JobType: jobType,
		Payload: map[string]any{"resource_type": resType, "resource_uuid": resUUID},
	}); err != nil {
		return nil, err
	}
	if a.Outbox != nil {
		_ = a.Outbox.AppendInTx(ctx, tx, realtime.DomainEvent{
			EventID:           jobType + "-" + opID,
			SchemaVersion:     "v1",
			Producer:          "go-controlplane",
			TeamID:            teamID,
			ResourceType:      resType,
			ResourceUUID:      resUUID,
			AggregateRevision: 1,
			OwnershipEpoch:    1,
			MutationType:      resType + ".lifecycle_queued",
			CommittedAt:       time.Now().UTC(),
			Payload:           map[string]any{"job_type": jobType, "operation_id": opID, "status": "queued"},
		})
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	_, _ = a.Dispatcher.FlushPending(ctx)
	return map[string]any{
		"message":      "Lifecycle operation queued.",
		"operation_id": opID,
		"job_type":     jobType,
		"status":       "queued",
	}, nil
}

// ---- Per-Channel Notification Config (/api/v1/notifications/{channel}) ----

func (a *App) handleNotificationChannel(w http.ResponseWriter, r *http.Request, p auth.Principal, ch notifications.ChannelKind) {
	teamID := activeTeamForPrincipal(p)
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
			Type: "team", ID: teamID, TeamID: &teamID, APISegment: "notifications",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		cfg, err := a.NotifySvc.LoadConfig(r.Context(), teamID, ch)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"channel": ch, "enabled": false, "configured": false, "team_id": teamID})
			return
		}
		configured := cfg.WebhookURL != "" || cfg.SMTPHost != "" || cfg.ChatID != "" || cfg.BotToken != "" || cfg.PushoverUserKey != "" || cfg.PushoverAPIToken != "" || cfg.ResendAPIKey != ""
		response := map[string]any{
			"channel":                     cfg.Channel,
			"team_id":                     teamID,
			"enabled":                     cfg.Enabled,
			"configured":                  configured,
			"resend_enabled":              cfg.ResendEnabled,
			"use_instance_email_settings": cfg.UseInstanceEmailSettings,
			"discord_ping_enabled":        cfg.DiscordPingEnabled,
		}
		if a.Authz.Check(r.Context(), p, access.ActionReadSensitive, access.ResourceRef{Type: "team", ID: teamID, TeamID: &teamID, APISegment: "notifications"}) == nil {
			encoded, err := json.Marshal(cfg)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			var fields map[string]any
			if err := json.Unmarshal(encoded, &fields); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			for key, value := range fields {
				response[key] = value
			}
		}
		writeJSON(w, http.StatusOK, response)
	case http.MethodPatch, http.MethodPut:
		var raw map[string]any
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid notification config"})
			return
		}
		if expected, ok := raw["expected_team_id"]; ok {
			id, valid := expected.(float64)
			if !valid || id != float64(teamID) {
				writeJSON(w, http.StatusConflict, map[string]string{"message": "The active team changed. Reload this page before saving."})
				return
			}
		}
		// Start from existing config if present so partial PATCHes preserve secrets
		cfg, _ := a.NotifySvc.LoadConfig(r.Context(), teamID, ch)
		cfg.Channel = ch
		if b, ok := raw["enabled"].(bool); ok {
			cfg.Enabled = b
		}
		if b, ok := raw[string(ch)+"_enabled"].(bool); ok {
			cfg.Enabled = b
		}
		if b, ok := raw["smtp_enabled"].(bool); ok && ch == notifications.ChannelEmail {
			cfg.Enabled = b
		}
		if b, ok := raw["resend_enabled"].(bool); ok {
			cfg.ResendEnabled = b
		}
		if b, ok := raw["use_instance_email_settings"].(bool); ok {
			cfg.UseInstanceEmailSettings = b
		}
		if b, ok := raw["discord_ping_enabled"].(bool); ok {
			cfg.DiscordPingEnabled = b
		}
		for _, k := range []string{"webhook_url", "discord_webhook_url", "slack_webhook_url"} {
			if s, ok := raw[k].(string); ok {
				cfg.WebhookURL = s
			}
		}
		if s, ok := raw["smtp_host"].(string); ok {
			cfg.SMTPHost = s
		}
		if n, ok := raw["smtp_port"].(float64); ok {
			cfg.SMTPPort = int(n)
		}
		for _, k := range []string{"smtp_from", "smtp_from_address"} {
			if s, ok := raw[k].(string); ok {
				cfg.SMTPFrom = s
			}
		}
		for _, k := range []string{"smtp_recipient", "smtp_recipients"} {
			if s, ok := raw[k].(string); ok {
				cfg.SMTPRecipient = s
			}
		}
		if s, ok := raw["smtp_username"].(string); ok {
			cfg.SMTPUsername = s
		}
		if s, ok := raw["smtp_password"].(string); ok {
			cfg.SMTPPassword = s
		}
		if s, ok := raw["resend_api_key"].(string); ok {
			cfg.ResendAPIKey = s
		}
		if s, ok := raw["resend_from_address"].(string); ok {
			cfg.ResendFromAddress = s
		}
		for _, k := range []string{"chat_id", "telegram_chat_id"} {
			if s, ok := raw[k].(string); ok {
				cfg.ChatID = s
			}
		}
		for _, k := range []string{"bot_token", "telegram_token"} {
			if s, ok := raw[k].(string); ok {
				cfg.BotToken = s
			}
		}
		if s, ok := raw["pushover_user_key"].(string); ok {
			cfg.PushoverUserKey = s
		}
		if s, ok := raw["pushover_api_token"].(string); ok {
			cfg.PushoverAPIToken = s
		}
		if err := a.NotifySvc.SaveConfig(r.Context(), p, teamID, cfg); err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"channel": ch, "enabled": cfg.Enabled, "message": "Saved."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Typed Application Create Helper ----

func typedAppBuildPack(segment string) (string, bool) {
	switch segment {
	case "public", "private-github-app", "private-gitea-app", "private-gitlab-app", "private-deploy-key", "railpack":
		return "railpack", true
	case "dockerfile":
		return "dockerfile", true
	case "dockerimage":
		return "dockerimage", true
	case "dockercompose":
		return "dockercompose", true
	case "static":
		return "static", true
	default:
		return "", false
	}
}

func isDatabaseTypeSegment(seg string) bool {
	switch seg {
	case "postgresql", "mysql", "mariadb", "mongodb", "redis", "clickhouse", "dragonfly", "keydb", "sqlite":
		return true
	default:
		return false
	}
}

// Suppress unused import check if applications types are referenced directly
var _ = applications.ErrNotFound
