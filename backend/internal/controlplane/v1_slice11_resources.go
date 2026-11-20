package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/coollabsio/coolify/backend/internal/realtime"
	"github.com/jackc/pgx/v5"
)

type slice11ResourceMeta struct {
	Kind            string // application | service | database
	MorphType       string
	DBTable         string
	DBType          string
	ID              int64
	UUID            string
	Name            string
	TeamID          int64
	ProjectID       int64
	ProjectUUID     string
	EnvironmentID   int64
	EnvironmentUUID string
	DestinationID   int64
	ServerID        int64
	ServerUUID      string
	TeamScopedRef   access.ResourceRef
}

var standaloneDBTables = []struct {
	Type  string
	Morph string
	Table string
}{
	{Type: "postgresql", Morph: `App\Models\StandalonePostgresql`, Table: "standalone_postgresqls"},
	{Type: "mysql", Morph: `App\Models\StandaloneMysql`, Table: "standalone_mysqls"},
	{Type: "mariadb", Morph: `App\Models\StandaloneMariadb`, Table: "standalone_mariadbs"},
	{Type: "mongodb", Morph: `App\Models\StandaloneMongodb`, Table: "standalone_mongodbs"},
	{Type: "redis", Morph: `App\Models\StandaloneRedis`, Table: "standalone_redis"},
	{Type: "keydb", Morph: `App\Models\StandaloneKeydb`, Table: "standalone_keydbs"},
	{Type: "dragonfly", Morph: `App\Models\StandaloneDragonfly`, Table: "standalone_dragonflies"},
	{Type: "clickhouse", Morph: `App\Models\StandaloneClickhouse`, Table: "standalone_clickhouses"},
	{Type: "sqlite", Morph: `App\Models\StandaloneSqlite`, Table: "standalone_sqlites"},
}

func (a *App) registerExtendedRoutes() {
	// Feedback & instance email settings
	a.mux.HandleFunc("/feedback", a.handleFeedback)
	a.mux.HandleFunc("/api/feedback", a.handleFeedback)
	a.mux.HandleFunc("/api/v1/feedback", a.handleFeedback)
	a.mux.HandleFunc("/api/v1/settings/email", a.requireAPIAuth(a.handleInstanceEmailSettings))

	// Backup upload / download (preserving routes/web.php auth boundaries)
	a.mux.HandleFunc("/upload/backup/", a.requireAPIAuth(a.handleUploadDatabaseBackup))
	a.mux.HandleFunc("/download/backup/", a.requireAPIAuth(a.handleDownloadDatabaseBackup))
	a.mux.HandleFunc("/download/volume-backup/", a.requireAPIAuth(a.handleDownloadVolumeBackup))

	a.registerSlice12Routes()
	a.registerSlice13Routes()
	a.registerSlice14Routes()
}

func (a *App) encryptSecretString(plain string) (string, error) {
	if plain == "" || a.Cipher == nil {
		return plain, nil
	}
	return a.Cipher.EncryptLaravelPayload([]byte(plain), false)
}

func (a *App) decryptSecretString(stored string) string {
	if stored == "" || a.Cipher == nil {
		return stored
	}
	if b, err := a.Cipher.DecryptLaravelPayload(stored, false); err == nil {
		return string(b)
	}
	return stored
}

func (a *App) loadResourceMeta(ctx context.Context, kind string, uuid string) (slice11ResourceMeta, error) {
	switch kind {
	case "application":
		var m slice11ResourceMeta
		m.Kind = "application"
		m.MorphType = `App\Models\Application`
		err := a.Pool.QueryRow(ctx, `
			SELECT a.id, a.uuid, a.name, COALESCE(a.destination_id, 0),
			       COALESCE(e.id, 0), COALESCE(e.uuid, ''),
			       COALESCE(p.id, 0), COALESCE(p.uuid, ''),
			       COALESCE(p.team_id, s.team_id, 0),
			       COALESCE(s.id, 0), COALESCE(s.uuid, 'localhost')
			FROM applications a
			LEFT JOIN environments e ON e.id = a.environment_id
			LEFT JOIN projects p ON p.id = e.project_id
			LEFT JOIN standalone_dockers sd ON sd.id = a.destination_id
			LEFT JOIN servers s ON s.id = sd.server_id
			WHERE a.uuid = $1
		`, uuid).Scan(&m.ID, &m.UUID, &m.Name, &m.DestinationID, &m.EnvironmentID, &m.EnvironmentUUID, &m.ProjectID, &m.ProjectUUID, &m.TeamID, &m.ServerID, &m.ServerUUID)
		if err != nil {
			return m, err
		}
		tid := m.TeamID
		m.TeamScopedRef = access.ResourceRef{
			Type: "application", ID: m.ID, UUID: m.UUID,
			ProjectUUID: m.ProjectUUID, EnvironmentUUID: m.EnvironmentUUID,
			TeamID: &tid, APISegment: "applications",
		}
		return m, nil

	case "service":
		var m slice11ResourceMeta
		m.Kind = "service"
		m.MorphType = `App\Models\Service`
		err := a.Pool.QueryRow(ctx, `
			SELECT sv.id, sv.uuid, sv.name, COALESCE(sv.destination_id, 0),
			       COALESCE(e.id, 0), COALESCE(e.uuid, ''),
			       COALESCE(p.id, 0), COALESCE(p.uuid, ''),
			       COALESCE(p.team_id, sr.team_id, 0),
			       COALESCE(sr.id, sv.server_id, 0), COALESCE(sr.uuid, 'localhost')
			FROM services sv
			LEFT JOIN environments e ON e.id = sv.environment_id
			LEFT JOIN projects p ON p.id = e.project_id
			LEFT JOIN standalone_dockers sd ON sd.id = sv.destination_id
			LEFT JOIN servers sr ON sr.id = COALESCE(sd.server_id, sv.server_id)
			WHERE sv.uuid = $1
		`, uuid).Scan(&m.ID, &m.UUID, &m.Name, &m.DestinationID, &m.EnvironmentID, &m.EnvironmentUUID, &m.ProjectID, &m.ProjectUUID, &m.TeamID, &m.ServerID, &m.ServerUUID)
		if err != nil {
			return m, err
		}
		tid := m.TeamID
		m.TeamScopedRef = access.ResourceRef{
			Type: "service", ID: m.ID, UUID: m.UUID,
			ProjectUUID: m.ProjectUUID, EnvironmentUUID: m.EnvironmentUUID,
			TeamID: &tid, APISegment: "services",
		}
		return m, nil

	case "database":
		for _, spec := range standaloneDBTables {
			var m slice11ResourceMeta
			m.Kind = "database"
			m.DBType = spec.Type
			m.DBTable = spec.Table
			m.MorphType = `App\Models\StandaloneDatabase`
			q := fmt.Sprintf(`
				SELECT d.id, d.uuid, d.name, COALESCE(d.destination_id, 0),
				       COALESCE(e.id, 0), COALESCE(e.uuid, ''),
				       COALESCE(p.id, 0), COALESCE(p.uuid, ''),
				       COALESCE(p.team_id, s.team_id, 0),
				       COALESCE(s.id, 0), COALESCE(s.uuid, 'localhost')
				FROM %s d
				LEFT JOIN environments e ON e.id = d.environment_id
				LEFT JOIN projects p ON p.id = e.project_id
				LEFT JOIN standalone_dockers sd ON sd.id = d.destination_id
				LEFT JOIN servers s ON s.id = sd.server_id
				WHERE d.uuid = $1
			`, spec.Table)
			err := a.Pool.QueryRow(ctx, q, uuid).Scan(&m.ID, &m.UUID, &m.Name, &m.DestinationID, &m.EnvironmentID, &m.EnvironmentUUID, &m.ProjectID, &m.ProjectUUID, &m.TeamID, &m.ServerID, &m.ServerUUID)
			if err == nil {
				tid := m.TeamID
				m.TeamScopedRef = access.ResourceRef{
					Type: "database", ID: m.ID, UUID: m.UUID,
					ProjectUUID: m.ProjectUUID, EnvironmentUUID: m.EnvironmentUUID,
					TeamID: &tid, APISegment: "databases",
				}
				return m, nil
			}
		}
		return slice11ResourceMeta{}, pgx.ErrNoRows
	}
	return slice11ResourceMeta{}, pgx.ErrNoRows
}

// ---- Feedback & Instance Email Settings ----

func (a *App) handleFeedback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	var body struct {
		Subject     string `json:"subject"`
		Description string `json:"description"`
		Content     string `json:"content"`
		Message     string `json:"message"`
		Email       string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid feedback payload"})
		return
	}
	content := strings.TrimSpace(body.Content)
	if content == "" {
		content = strings.TrimSpace(body.Message)
	}
	if content == "" && strings.TrimSpace(body.Description) != "" {
		subj := strings.TrimSpace(body.Subject)
		if subj != "" {
			content = "[" + subj + "] " + strings.TrimSpace(body.Description)
		} else {
			content = strings.TrimSpace(body.Description)
		}
	}
	if content == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "content is required."})
		return
	}
	var userID, teamID *int64
	if a.Authenticator != nil {
		if authHdr := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(authHdr), "bearer ") {
			if p, err := a.Authenticator.AuthenticateBearer(r.Context(), strings.TrimSpace(authHdr[7:])); err == nil {
				u := p.UserID
				t := activeTeamForPrincipal(p)
				userID = &u
				teamID = &t
			}
		} else if p, err := a.Authenticator.AuthenticateSession(r.Context(), r); err == nil {
			u := p.UserID
			t := activeTeamForPrincipal(p)
			userID = &u
			teamID = &t
		}
	}
	u := a.Hooks.NewCUID2()
	if _, err := a.Pool.Exec(r.Context(), `
		INSERT INTO go_feedbacks (uuid, user_id, team_id, email, content)
		VALUES ($1, $2, $3, $4, $5)
	`, u, userID, teamID, strings.TrimSpace(body.Email), content); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "Feedback sent.", "uuid": u})
}

func (a *App) handleInstanceEmailSettings(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	rootTeam := int64(0)
	if activeTeamForPrincipal(p) != 0 {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "Instance email settings require a root-team token or session."})
		return
	}
	ref := access.ResourceRef{Type: "instance_settings", ID: 0, TeamID: &rootTeam, APISegment: "settings"}

	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionManageInstance, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		a.serveInstanceEmailSettings(w, r, p)

	case http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionManageInstance, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			SMTPEnabled             *bool   `json:"smtp_enabled"`
			SMTPFromAddress         *string `json:"smtp_from_address"`
			SMTPFromName            *string `json:"smtp_from_name"`
			NotificationFromAddress *string `json:"notification_from_address"`
			NotificationFromName    *string `json:"notification_from_name"`
			SMTPHost                *string `json:"smtp_host"`
			SMTPPort                *int    `json:"smtp_port"`
			SMTPEncryption          *string `json:"smtp_encryption"`
			SMTPUsername            *string `json:"smtp_username"`
			SMTPPassword            *string `json:"smtp_password"`
			SMTPTimeout             *int    `json:"smtp_timeout"`
			SMTPEhloDomain          *string `json:"smtp_ehlo_domain"`
			ResendEnabled           *bool   `json:"resend_enabled"`
			ResendAPIKey            *string `json:"resend_api_key"`
			ResendFromAddress       *string `json:"resend_from_address"`
			ResendFromName          *string `json:"resend_from_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid email settings payload"})
			return
		}
		if body.SMTPEncryption != nil {
			switch *body.SMTPEncryption {
			case "", "starttls", "tls", "none":
			default:
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid smtp_encryption"})
				return
			}
		}
		if body.SMTPPort != nil && (*body.SMTPPort < 1 || *body.SMTPPort > 65535) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid smtp_port"})
			return
		}
		if body.SMTPEnabled != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET smtp_enabled = $1 WHERE id = 0`, *body.SMTPEnabled)
		}
		if body.SMTPFromAddress != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET smtp_from_address = $1 WHERE id = 0`, strings.TrimSpace(*body.SMTPFromAddress))
		}
		if body.SMTPFromName != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET smtp_from_name = $1 WHERE id = 0`, strings.TrimSpace(*body.SMTPFromName))
		}
		if body.NotificationFromAddress != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET notification_from_address = $1 WHERE id = 0`, strings.TrimSpace(*body.NotificationFromAddress))
		}
		if body.NotificationFromName != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET notification_from_name = $1 WHERE id = 0`, strings.TrimSpace(*body.NotificationFromName))
		}
		if body.SMTPHost != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET smtp_host = $1 WHERE id = 0`, strings.TrimSpace(*body.SMTPHost))
		}
		if body.SMTPPort != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET smtp_port = $1 WHERE id = 0`, *body.SMTPPort)
		}
		if body.SMTPEncryption != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET smtp_encryption = $1 WHERE id = 0`, *body.SMTPEncryption)
		}
		if body.SMTPUsername != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET smtp_username = $1 WHERE id = 0`, *body.SMTPUsername)
		}
		if body.SMTPPassword != nil {
			enc, err := a.encryptSecretString(*body.SMTPPassword)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET smtp_password = $1 WHERE id = 0`, enc)
		}
		if body.SMTPTimeout != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET smtp_timeout = $1 WHERE id = 0`, *body.SMTPTimeout)
		}
		if body.SMTPEhloDomain != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET smtp_ehlo_domain = $1 WHERE id = 0`, strings.TrimSpace(*body.SMTPEhloDomain))
		}
		if body.ResendEnabled != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET resend_enabled = $1 WHERE id = 0`, *body.ResendEnabled)
		}
		if body.ResendAPIKey != nil {
			enc, err := a.encryptSecretString(*body.ResendAPIKey)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET resend_api_key = $1 WHERE id = 0`, enc)
		}
		if body.ResendFromAddress != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET resend_from_address = $1 WHERE id = 0`, strings.TrimSpace(*body.ResendFromAddress))
		}
		if body.ResendFromName != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET resend_from_name = $1 WHERE id = 0`, strings.TrimSpace(*body.ResendFromName))
		}
		_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET updated_at = NOW() WHERE id = 0`)
		a.serveInstanceEmailSettings(w, r, p)

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (a *App) serveInstanceEmailSettings(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var smtpOn, resendOn bool
	var fromAddr, fromName, notifAddr, notifName, host, enc, user, pass, ehlo, resendKey, resendAddr, resendName string
	var port, timeout int
	err := a.Pool.QueryRow(r.Context(), `
		SELECT COALESCE(smtp_enabled, FALSE), COALESCE(smtp_from_address,''), COALESCE(smtp_from_name,''),
		       COALESCE(notification_from_address,''), COALESCE(notification_from_name,''),
		       COALESCE(smtp_host,''), COALESCE(smtp_port, 587), COALESCE(smtp_encryption,'starttls'),
		       COALESCE(smtp_username,''), COALESCE(smtp_password,''), COALESCE(smtp_timeout, 30),
		       COALESCE(smtp_ehlo_domain,''), COALESCE(resend_enabled, FALSE),
		       COALESCE(resend_api_key,''), COALESCE(resend_from_address,''), COALESCE(resend_from_name,'')
		FROM instance_settings WHERE id = 0
	`).Scan(&smtpOn, &fromAddr, &fromName, &notifAddr, &notifName, &host, &port, &enc, &user, &pass, &timeout, &ehlo, &resendOn, &resendKey, &resendAddr, &resendName)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	rootTeam := int64(0)
	canSensitive := a.Authz.Check(r.Context(), p, access.ActionReadSensitive, access.ResourceRef{
		Type: "instance_settings", ID: 0, TeamID: &rootTeam, APISegment: "settings",
	}) == nil
	resp := map[string]any{
		"smtp_enabled":              smtpOn,
		"smtp_from_address":         fromAddr,
		"smtp_from_name":            fromName,
		"notification_from_address": notifAddr,
		"notification_from_name":    notifName,
		"smtp_host":                 host,
		"smtp_port":                 port,
		"smtp_encryption":           enc,
		"smtp_username":             user,
		"smtp_timeout":              timeout,
		"smtp_ehlo_domain":          ehlo,
		"resend_enabled":            resendOn,
		"resend_from_address":       resendAddr,
		"resend_from_name":          resendName,
	}
	if canSensitive {
		resp["smtp_password"] = a.decryptSecretString(pass)
		resp["resend_api_key"] = a.decryptSecretString(resendKey)
	}
	writeJSON(w, http.StatusOK, resp)
}

// ---- Tags & S3 Storages Extensions ----

func (a *App) withExtendedTagRoutes(next authedHandler) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/tags"), "/")
		if trimmed != "" && r.Method == http.MethodPatch {
			var ownerTeam, tagID int64
			var err error
			if idNum, parseErr := strconv.ParseInt(trimmed, 10, 64); parseErr == nil {
				err = a.Pool.QueryRow(r.Context(), `SELECT id, team_id FROM tags WHERE id = $1`, idNum).Scan(&tagID, &ownerTeam)
			} else {
				err = a.Pool.QueryRow(r.Context(), `SELECT id, team_id FROM tags WHERE uuid = $1 OR (team_id = $2 AND LOWER(name) = LOWER($1))`, trimmed, activeTeamForPrincipal(p)).Scan(&tagID, &ownerTeam)
			}
			if errors.Is(err, pgx.ErrNoRows) {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "tag not found"})
				return
			}
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			ref := access.ResourceRef{Type: "tag", ID: tagID, TeamID: &ownerTeam, APISegment: "tags"}
			if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(strings.TrimSpace(body.Name)) < 2 {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "name must be at least 2 characters."})
				return
			}
			cleanName := strings.ToLower(compat.SanitizeString(body.Name))
			if _, err := a.Pool.Exec(r.Context(), `UPDATE tags SET name = $1, updated_at = NOW() WHERE id = $2`, cleanName, tagID); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": tagID, "name": cleanName, "team_id": ownerTeam})
			return
		}
		next(w, r, p)
	}
}

func (a *App) withExtendedS3Routes(next authedHandler) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/s3-storages"), "/")
		parts := strings.Split(trimmed, "/")
		if len(parts) == 1 && parts[0] != "" && r.Method == http.MethodPatch {
			a.handleS3UpdateOrValidate(w, r, p, parts[0], false)
			return
		}
		if len(parts) == 2 && parts[1] == "validate" && r.Method == http.MethodPost {
			a.handleS3UpdateOrValidate(w, r, p, parts[0], true)
			return
		}
		next(w, r, p)
	}
}

func (a *App) handleS3UpdateOrValidate(w http.ResponseWriter, r *http.Request, p auth.Principal, s3UUID string, isValidate bool) {
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
	if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
		writeDomainError(w, err)
		return
	}
	if isValidate {
		if strings.TrimSpace(bucket) == "" && strings.TrimSpace(endpoint) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "storage endpoint/bucket missing"})
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `UPDATE s3_storages SET is_usable = TRUE, updated_at = NOW() WHERE id = $1`, id)
		writeJSON(w, http.StatusOK, map[string]any{"message": "S3 storage validated.", "uuid": s3UUID, "is_usable": true})
		return
	}
	var body struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Region      *string `json:"region"`
		Bucket      *string `json:"bucket"`
		Endpoint    *string `json:"endpoint"`
		Key         *string `json:"key"`
		Secret      *string `json:"secret"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid s3 storage update payload"})
		return
	}
	if body.Name != nil {
		name = compat.SanitizeString(*body.Name)
	}
	if body.Description != nil {
		desc = compat.SanitizeString(*body.Description)
	}
	if body.Region != nil {
		region = strings.TrimSpace(*body.Region)
	}
	if body.Bucket != nil {
		bucket = strings.TrimSpace(*body.Bucket)
	}
	if body.Endpoint != nil {
		endpoint = strings.TrimSpace(*body.Endpoint)
	}
	_, _ = a.Pool.Exec(r.Context(), `
		UPDATE s3_storages SET name = $1, description = $2, region = $3, bucket = $4, endpoint = $5, updated_at = NOW() WHERE id = $6
	`, name, desc, region, bucket, endpoint, id)
	if body.Key != nil {
		enc, _ := a.encryptSecretString(*body.Key)
		_, _ = a.Pool.Exec(r.Context(), `UPDATE s3_storages SET key = $1 WHERE id = $2`, enc, id)
	}
	if body.Secret != nil {
		enc, _ := a.encryptSecretString(*body.Secret)
		_, _ = a.Pool.Exec(r.Context(), `UPDATE s3_storages SET secret = $1 WHERE id = $2`, enc, id)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": id, "uuid": s3UUID, "name": name, "description": desc,
		"region": region, "bucket": bucket, "endpoint": endpoint, "is_usable": usable, "team_id": teamID,
	})
}

// ---- Extended Project Environment Routes ----

func (a *App) withExtendedProjectRoutes(next authedHandler) authedHandler {
	return func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
		trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/projects"), "/")
		parts := strings.Split(trimmed, "/")
		if len(parts) < 2 {
			next(w, r, p)
			return
		}
		projUUID := parts[0]

		if len(parts) == 2 && parts[1] == "settings" {
			a.handleProjectSettings(w, r, p, projUUID)
			return
		}

		// Environment shared envs: /projects/{uuid}/environments/{env}/envs(/env_id)
		if (len(parts) == 4 || len(parts) == 5) && parts[1] == "environments" && parts[3] == "envs" {
			proj, envs, err := a.ProjectSvc.GetProject(r.Context(), p, projUUID)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			envTarget := parts[2]
			var envID int64
			var envUUID string
			for _, e := range envs {
				if e.UUID == envTarget || e.Name == envTarget {
					envID = e.ID
					envUUID = e.UUID
					break
				}
			}
			if envUUID == "" {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "environment not found"})
				return
			}
			envVarID := ""
			if len(parts) == 5 {
				envVarID = parts[4]
			}
			a.handleEnvironmentSharedEnvs(w, r, p, proj.TeamID, proj.UUID, envID, envUUID, envVarID)
			return
		}

		// Clone environment: POST /projects/{uuid}/environments/{env}/clone
		if len(parts) == 4 && parts[1] == "environments" && parts[3] == "clone" {
			if r.Method != http.MethodPost {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
				return
			}
			proj, envs, err := a.ProjectSvc.GetProject(r.Context(), p, projUUID)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			envTarget := parts[2]
			var srcEnvID int64
			var srcEnvUUID, srcEnvName string
			for _, e := range envs {
				if e.UUID == envTarget || e.Name == envTarget {
					srcEnvID = e.ID
					srcEnvUUID = e.UUID
					srcEnvName = e.Name
					break
				}
			}
			if srcEnvUUID == "" {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "environment not found"})
				return
			}
			tid := proj.TeamID
			if err := a.Authz.Check(r.Context(), p, access.ActionCreate, access.ResourceRef{
				Type: "environment", ID: srcEnvID, UUID: srcEnvUUID, ProjectUUID: proj.UUID, EnvironmentUUID: srcEnvUUID, TeamID: &tid, APISegment: "projects",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			newName := compat.SanitizeString(body.Name)
			if newName == "" {
				newName = srcEnvName + "-clone"
			}
			newEnvUUID := a.Hooks.NewCUID2()
			var newEnvID int64
			if err := a.Pool.QueryRow(r.Context(), `
				INSERT INTO environments (uuid, name, project_id, description)
				SELECT $1, $2, project_id, description FROM environments WHERE id = $3
				RETURNING id
			`, newEnvUUID, newName, srcEnvID).Scan(&newEnvID); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
				return
			}
			// Clone environment shared variables
			_, _ = a.Pool.Exec(r.Context(), `
				INSERT INTO shared_environment_variables (uuid, key, value, is_shown_once, type, team_id, project_id, environment_id)
				SELECT md5(random()::text || clock_timestamp()::text || id::text), key, value, is_shown_once, 'environment', team_id, project_id, $2
				FROM shared_environment_variables
				WHERE type = 'environment' AND environment_id = $1
			`, srcEnvID, newEnvID)
			// Clone applications in this environment (+ settings + env vars)
			appRows, _ := a.Pool.Query(r.Context(), `SELECT id FROM applications WHERE environment_id = $1`, srcEnvID)
			if appRows != nil {
				var srcAppIDs []int64
				for appRows.Next() {
					var aid int64
					if appRows.Scan(&aid) == nil {
						srcAppIDs = append(srcAppIDs, aid)
					}
				}
				appRows.Close()
				for _, aid := range srcAppIDs {
					var newAppID int64
					newAppUUID := a.Hooks.NewCUID2()
					if a.Pool.QueryRow(r.Context(), `
						INSERT INTO applications (uuid, name, description, fqdn, git_repository, git_branch, build_pack, docker_registry_image_name, docker_registry_image_tag, dockerfile_location, docker_compose_raw, ports_exposes, status, environment_id, destination_id, destination_type, private_key_id)
						SELECT $1, name, description, NULL, git_repository, git_branch, build_pack, docker_registry_image_name, docker_registry_image_tag, dockerfile_location, docker_compose_raw, ports_exposes, 'exited', $2, destination_id, destination_type, private_key_id
						FROM applications WHERE id = $3 RETURNING id
					`, newAppUUID, newEnvID, aid).Scan(&newAppID) == nil {
						_, _ = a.Pool.Exec(r.Context(), `INSERT INTO application_settings (application_id) VALUES ($1) ON CONFLICT DO NOTHING`, newAppID)
						_, _ = a.Pool.Exec(r.Context(), `
							INSERT INTO environment_variables (uuid, key, value, is_preview, is_buildtime, is_runtime, is_literal, is_shown_once, resourceable_type, resourceable_id)
							SELECT md5(random()::text || clock_timestamp()::text || id::text), key, value, is_preview, is_buildtime, is_runtime, is_literal, is_shown_once, resourceable_type, $1
							FROM environment_variables WHERE resourceable_type = 'App\Models\Application' AND resourceable_id = $2
						`, newAppID, aid)
					}
				}
			}
			// Clone services in this environment (+ sub-resources + env vars)
			svcRows, _ := a.Pool.Query(r.Context(), `SELECT id FROM services WHERE environment_id = $1`, srcEnvID)
			if svcRows != nil {
				var srcSvcIDs []int64
				for svcRows.Next() {
					var sid int64
					if svcRows.Scan(&sid) == nil {
						srcSvcIDs = append(srcSvcIDs, sid)
					}
				}
				svcRows.Close()
				for _, sid := range srcSvcIDs {
					var newSvcID int64
					newSvcUUID := a.Hooks.NewCUID2()
					if a.Pool.QueryRow(r.Context(), `
						INSERT INTO services (uuid, name, description, docker_compose_raw, docker_compose, fqdn, status, environment_id, server_id, destination_id, destination_type)
						SELECT $1, name, description, docker_compose_raw, docker_compose, NULL, 'exited', $2, server_id, destination_id, destination_type
						FROM services WHERE id = $3 RETURNING id
					`, newSvcUUID, newEnvID, sid).Scan(&newSvcID) == nil {
						_, _ = a.Pool.Exec(r.Context(), `
							INSERT INTO service_applications (uuid, name, human_name, fqdn, ports, status, service_id, exclude_from_status)
							SELECT md5(random()::text || clock_timestamp()::text || id::text), name, human_name, NULL, ports, 'exited', $1, exclude_from_status
							FROM service_applications WHERE service_id = $2 AND deleted_at IS NULL
						`, newSvcID, sid)
						_, _ = a.Pool.Exec(r.Context(), `
							INSERT INTO service_databases (uuid, name, human_name, ports, status, service_id, exclude_from_status)
							SELECT md5(random()::text || clock_timestamp()::text || id::text), name, human_name, ports, 'exited', $1, exclude_from_status
							FROM service_databases WHERE service_id = $2 AND deleted_at IS NULL
						`, newSvcID, sid)
						_, _ = a.Pool.Exec(r.Context(), `
							INSERT INTO environment_variables (uuid, key, value, is_preview, is_buildtime, is_runtime, is_literal, is_shown_once, resourceable_type, resourceable_id)
							SELECT md5(random()::text || clock_timestamp()::text || id::text), key, value, is_preview, is_buildtime, is_runtime, is_literal, is_shown_once, resourceable_type, $1
							FROM environment_variables WHERE resourceable_type = 'App\Models\Service' AND resourceable_id = $2
						`, newSvcID, sid)
					}
				}
			}
			writeJSON(w, http.StatusCreated, map[string]any{
				"id": newEnvID, "uuid": newEnvUUID, "name": newName, "project_id": proj.ID,
			})
			return
		}

		// Update environment: PATCH /projects/{uuid}/environments/{env}
		if len(parts) == 3 && parts[1] == "environments" && r.Method == http.MethodPatch {
			proj, envs, err := a.ProjectSvc.GetProject(r.Context(), p, projUUID)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			target := parts[2]
			var envID int64
			var envUUID, envName, envDesc string
			for _, e := range envs {
				if e.UUID == target || e.Name == target {
					envID = e.ID
					envUUID = e.UUID
					envName = e.Name
					_ = a.Pool.QueryRow(r.Context(), `SELECT COALESCE(description,'') FROM environments WHERE id = $1`, envID).Scan(&envDesc)
					break
				}
			}
			if envUUID == "" {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "environment not found"})
				return
			}
			tid := proj.TeamID
			if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{
				Type: "environment", ID: envID, UUID: envUUID, ProjectUUID: proj.UUID, EnvironmentUUID: envUUID, TeamID: &tid, APISegment: "projects",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			var body struct {
				Name        *string `json:"name"`
				Description *string `json:"description"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid environment update"})
				return
			}
			if body.Name != nil && strings.TrimSpace(*body.Name) != "" {
				envName = compat.SanitizeString(*body.Name)
			}
			if body.Description != nil {
				envDesc = compat.SanitizeString(*body.Description)
			}
			_, err = a.Pool.Exec(r.Context(), `UPDATE environments SET name = $1, description = $2, updated_at = NOW() WHERE id = $3`, envName, envDesc, envID)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"id": envID, "uuid": envUUID, "name": envName, "description": envDesc, "project_id": proj.ID,
			})
			return
		}

		// Get environment details: GET /projects/{uuid}/{environment_name_or_uuid}
		if len(parts) == 2 && parts[1] != "environments" && parts[1] != "envs" && r.Method == http.MethodGet {
			proj, envs, err := a.ProjectSvc.GetProject(r.Context(), p, projUUID)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			target := parts[1]
			for _, e := range envs {
				if e.UUID == target || e.Name == target {
					apps := make([]map[string]any, 0)
					rows, qErr := a.Pool.Query(r.Context(), `SELECT id, uuid, name, status, COALESCE(fqdn,'') FROM applications WHERE environment_id = $1 ORDER BY id ASC`, e.ID)
					if qErr == nil {
						defer rows.Close()
						for rows.Next() {
							var id int64
							var u, n, st, f string
							if rows.Scan(&id, &u, &n, &st, &f) == nil {
								apps = append(apps, map[string]any{"id": id, "uuid": u, "name": n, "status": st, "fqdn": f})
							}
						}
					}
					svcs := make([]map[string]any, 0)
					sRows, sErr := a.Pool.Query(r.Context(), `SELECT id, uuid, name, status FROM services WHERE environment_id = $1 ORDER BY id ASC`, e.ID)
					if sErr == nil {
						defer sRows.Close()
						for sRows.Next() {
							var id int64
							var u, n, st string
							if sRows.Scan(&id, &u, &n, &st) == nil {
								svcs = append(svcs, map[string]any{"id": id, "uuid": u, "name": n, "status": st})
							}
						}
					}
					var envDesc string
					_ = a.Pool.QueryRow(r.Context(), `SELECT COALESCE(description,'') FROM environments WHERE id = $1`, e.ID).Scan(&envDesc)
					writeJSON(w, http.StatusOK, map[string]any{
						"id": e.ID, "uuid": e.UUID, "name": e.Name, "description": envDesc,
						"project_uuid": proj.UUID, "applications": apps, "services": svcs,
					})
					return
				}
			}
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "environment not found"})
			return
		}

		next(w, r, p)
	}
}

func (a *App) handleEnvironmentSharedEnvs(w http.ResponseWriter, r *http.Request, p auth.Principal, teamID int64, projUUID string, envID int64, envUUID string, envVarID string) {
	ref := access.ResourceRef{
		Type: "environment", ID: envID, UUID: envUUID, ProjectUUID: projUUID, EnvironmentUUID: envUUID,
		TeamID: &teamID, APISegment: "envs",
	}
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		canSecret := a.Authz.Check(r.Context(), p, access.ActionReadSensitive, ref) == nil
		rows, err := a.Pool.Query(r.Context(), `
			SELECT id, uuid, key, value, is_shown_once FROM shared_environment_variables
			WHERE team_id = $1 AND type = 'environment' AND environment_id = $2 ORDER BY id ASC
		`, teamID, envID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id int64
			var u, k string
			var v *string
			var once bool
			if rows.Scan(&id, &u, &k, &v, &once) == nil {
				item := map[string]any{"id": id, "uuid": u, "key": k, "is_shown_once": once, "environment_id": envID, "type": "environment"}
				if v != nil && canSecret && !once {
					item["value"] = a.decryptSecretString(*v)
				}
				out = append(out, item)
			}
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost, http.MethodPatch:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Key         string `json:"key"`
			Value       string `json:"value"`
			IsShownOnce bool   `json:"is_shown_once"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid env payload"})
			return
		}
		if envVarID != "" && strings.TrimSpace(body.Key) == "" {
			if err := a.Pool.QueryRow(r.Context(), `SELECT key FROM shared_environment_variables WHERE (uuid = $1 OR id::text = $1) AND environment_id = $2 AND team_id = $3`, envVarID, envID, teamID).Scan(&body.Key); err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "shared environment variable not found"})
				return
			}
		}
		if strings.TrimSpace(body.Key) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "key is required"})
			return
		}
		enc, _ := a.encryptSecretString(body.Value)
		u := a.Hooks.NewCUID2()
		if envVarID != "" {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE shared_environment_variables SET key = $1, value = $2, updated_at = NOW() WHERE (uuid = $3 OR id::text = $3) AND environment_id = $4 AND team_id = $5`,
				strings.TrimSpace(body.Key), enc, envVarID, envID, teamID)
			writeJSON(w, http.StatusOK, map[string]any{"key": body.Key, "environment_id": envID})
			return
		}
		var id int64
		if err := a.Pool.QueryRow(r.Context(), `
			INSERT INTO shared_environment_variables (uuid, key, value, type, is_shown_once, team_id, environment_id)
			VALUES ($1, $2, $3, 'environment', $4, $5, $6) RETURNING id
		`, u, strings.TrimSpace(body.Key), enc, body.IsShownOnce, teamID, envID).Scan(&id); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "uuid": u, "key": body.Key, "environment_id": envID})
	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM shared_environment_variables WHERE (uuid = $1 OR id::text = $1) AND environment_id = $2 AND team_id = $3`, envVarID, envID, teamID)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Common Resource Tags, Storage Update, Volume Backups, Move/Migrate/Clone, Scheduled Tasks ----

func (a *App) handleResourceTags(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta, tagSeg string) {
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		rows, err := a.Pool.Query(r.Context(), `
			SELECT t.id, COALESCE(t.uuid,''), t.name, t.team_id
			FROM tags t
			JOIN taggables tg ON tg.tag_id = t.id
			WHERE tg.taggable_type = $1 AND tg.taggable_id = $2 AND t.team_id = $3
			ORDER BY t.name ASC
		`, meta.MorphType, meta.ID, meta.TeamID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var id, tid int64
			var u, n string
			if rows.Scan(&id, &u, &n, &tid) == nil {
				out = append(out, map[string]any{"id": id, "uuid": u, "name": n, "team_id": tid})
			}
		}
		writeJSON(w, http.StatusOK, out)

	case http.MethodPost:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Name string `json:"name"`
			Tag  string `json:"tag"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid tag payload"})
			return
		}
		tagName := strings.ToLower(compat.SanitizeString(body.Name))
		if tagName == "" {
			tagName = strings.ToLower(compat.SanitizeString(body.Tag))
		}
		if len(tagName) < 2 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "tag name must be at least 2 characters"})
			return
		}
		var tagID int64
		var tagUUID string
		err := a.Pool.QueryRow(r.Context(), `SELECT id, COALESCE(uuid,'') FROM tags WHERE team_id = $1 AND LOWER(name) = $2`, meta.TeamID, tagName).Scan(&tagID, &tagUUID)
		if errors.Is(err, pgx.ErrNoRows) {
			tagUUID = a.Hooks.NewCUID2()
			err = a.Pool.QueryRow(r.Context(), `INSERT INTO tags (uuid, name, team_id) VALUES ($1, $2, $3) RETURNING id`, tagUUID, tagName, meta.TeamID).Scan(&tagID)
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		if tagUUID == "" {
			tagUUID = fmt.Sprintf("%d", tagID)
		}
		_, _ = a.Pool.Exec(r.Context(), `
			INSERT INTO taggables (tag_id, taggable_id, taggable_type)
			VALUES ($1, $2, $3)
			ON CONFLICT (tag_id, taggable_id, taggable_type) DO NOTHING
		`, tagID, meta.ID, meta.MorphType)
		writeJSON(w, http.StatusCreated, map[string]any{"id": tagID, "uuid": tagUUID, "name": tagName})

	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		res, err := a.Pool.Exec(r.Context(), `
			DELETE FROM taggables tg
			USING tags t
			WHERE tg.tag_id = t.id
			  AND tg.taggable_type = $1 AND tg.taggable_id = $2 AND t.team_id = $3
			  AND (t.uuid = $4 OR t.id::text = $4 OR LOWER(t.name) = LOWER($4))
		`, meta.MorphType, meta.ID, meta.TeamID, tagSeg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		if res.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "tag not found on resource"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Tag removed."})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (a *App) handleStoragePatch(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta) {
	if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
		writeDomainError(w, err)
		return
	}
	var body struct {
		UUID      string  `json:"uuid"`
		Name      *string `json:"name"`
		MountPath *string `json:"mount_path"`
		HostPath  *string `json:"host_path"`
		FSPath    *string `json:"fs_path"`
		Content   *string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.UUID) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "storage uuid is required"})
		return
	}
	if body.MountPath != nil {
		if _, err := validateStoragePath(*body.MountPath, false); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid mount_path"})
			return
		}
	}
	if body.HostPath != nil && strings.TrimSpace(*body.HostPath) != "" {
		if _, err := validateStoragePath(*body.HostPath, false); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid host_path"})
			return
		}
	}
	if body.FSPath != nil {
		if _, err := validateStoragePath(*body.FSPath, true); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid fs_path"})
			return
		}
	}
	if body.Content != nil && len(*body.Content) > maxFileVolumeContentBytes {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": ErrContentTooLarge.Error()})
		return
	}

	var pvID int64
	if err := a.Pool.QueryRow(r.Context(), `SELECT id FROM local_persistent_volumes WHERE uuid = $1 AND resource_type = $2 AND resource_id = $3`, body.UUID, meta.MorphType, meta.ID).Scan(&pvID); err == nil {
		if body.Name != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE local_persistent_volumes SET name = $1, updated_at = NOW() WHERE id = $2`, compat.SanitizeString(*body.Name), pvID)
		}
		if body.MountPath != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE local_persistent_volumes SET mount_path = $1, updated_at = NOW() WHERE id = $2`, strings.TrimSpace(*body.MountPath), pvID)
		}
		if body.HostPath != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE local_persistent_volumes SET host_path = $1, updated_at = NOW() WHERE id = $2`, strings.TrimSpace(*body.HostPath), pvID)
		}
		writeJSON(w, http.StatusOK, map[string]any{"uuid": body.UUID, "type": "persistent", "message": "Storage updated."})
		return
	}

	var fvID int64
	if err := a.Pool.QueryRow(r.Context(), `SELECT id FROM local_file_volumes WHERE uuid = $1 AND resource_type = $2 AND resource_id = $3`, body.UUID, meta.MorphType, meta.ID).Scan(&fvID); err == nil {
		if body.MountPath != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE local_file_volumes SET mount_path = $1, updated_at = NOW() WHERE id = $2`, strings.TrimSpace(*body.MountPath), fvID)
		}
		if body.FSPath != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE local_file_volumes SET fs_path = $1, updated_at = NOW() WHERE id = $2`, strings.TrimSpace(*body.FSPath), fvID)
		}
		if body.Content != nil {
			enc, _ := a.encryptSecretString(*body.Content)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE local_file_volumes SET content = $1, updated_at = NOW() WHERE id = $2`, enc, fvID)
		}
		writeJSON(w, http.StatusOK, map[string]any{"uuid": body.UUID, "type": "file", "message": "Storage updated."})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "storage not found"})
}

func (a *App) handleVolumeBackupRoute(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta, storageUUID string, runNow bool) {
	// Resolve either local_persistent_volumes or directory local_file_volumes
	var storageType, backupableType, volName, mountPath string
	var backupableID int64
	err := a.Pool.QueryRow(r.Context(), `SELECT id, name, mount_path FROM local_persistent_volumes WHERE uuid = $1 AND resource_type = $2 AND resource_id = $3`,
		storageUUID, meta.MorphType, meta.ID).Scan(&backupableID, &volName, &mountPath)
	if err == nil {
		storageType = "persistent"
		backupableType = `App\Models\LocalPersistentVolume`
	} else {
		var isDir bool
		err = a.Pool.QueryRow(r.Context(), `SELECT id, fs_path, mount_path, is_directory FROM local_file_volumes WHERE uuid = $1 AND resource_type = $2 AND resource_id = $3`,
			storageUUID, meta.MorphType, meta.ID).Scan(&backupableID, &volName, &mountPath, &isDir)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "storage not found"})
			return
		}
		if !isDir {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "only persistent volumes and directory mounts support backups"})
			return
		}
		storageType = "directory"
		backupableType = `App\Models\LocalFileVolume`
	}

	if runNow && r.Method == http.MethodPost {
		if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		var schedID int64
		var schedUUID string
		err := a.Pool.QueryRow(r.Context(), `SELECT id, uuid FROM scheduled_volume_backups WHERE backupable_type = $1 AND backupable_id = $2`, backupableType, backupableID).Scan(&schedID, &schedUUID)
		if errors.Is(err, pgx.ErrNoRows) {
			schedUUID = a.Hooks.NewCUID2()
			err = a.Pool.QueryRow(r.Context(), `
				INSERT INTO scheduled_volume_backups (uuid, backupable_type, backupable_id, team_id, frequency, enabled)
				VALUES ($1, $2, $3, $4, '0 2 * * *', TRUE) RETURNING id
			`, schedUUID, backupableType, backupableID, meta.TeamID).Scan(&schedID)
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		execUUID := a.Hooks.NewCUID2()
		var execID int64
		if err := a.Pool.QueryRow(r.Context(), `
			INSERT INTO scheduled_volume_backup_executions (uuid, scheduled_volume_backup_id, status)
			VALUES ($1, $2, 'running') RETURNING id
		`, execUUID, schedID).Scan(&execID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		opID := "volbackup-" + execUUID
		tx, err := a.Pool.Begin(r.Context())
		if err == nil {
			defer func() { _ = tx.Rollback(r.Context()) }()
			_ = a.JobCoord.AdmitOperationInTx(r.Context(), tx, jobs.DurableJobSpec{
				JobOperation: jobs.JobOperation{
					JobID: "job-" + opID, OperationID: opID, AttemptID: "att-0",
					ResourceUUID: meta.UUID, ServerUUID: meta.ServerUUID, TeamID: meta.TeamID,
					Status: jobs.StatusQueued, MaxAttempts: 2,
				},
				JobType: "volume_backup",
				Payload: map[string]any{
					"execution_id":   execID,
					"execution_uuid": execUUID,
					"schedule_uuid":  schedUUID,
					"storage_uuid":   storageUUID,
					"volume_name":    volName,
					"mount_path":     mountPath,
					"storage_type":   storageType,
				},
			})
			_ = tx.Commit(r.Context())
			_, _ = a.Dispatcher.FlushPending(r.Context())
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"message": "Volume backup queued.", "execution_id": execID, "execution_uuid": execUUID, "operation_id": opID,
		})
		return
	}

	switch r.Method {
	case http.MethodPut:
		var body struct {
			Frequency                   string  `json:"frequency"`
			Enabled                     *bool   `json:"enabled"`
			SaveS3                      *bool   `json:"save_s3"`
			DisableLocalBackup          *bool   `json:"disable_local_backup"`
			StopDuringBackup            *bool   `json:"stop_during_backup"`
			S3StorageUUID               *string `json:"s3_storage_uuid"`
			RetentionAmountLocally      *int    `json:"retention_amount_locally"`
			Timeout                     *int    `json:"timeout"`
			MissingBackupNotifyDays     *int    `json:"missing_backup_notification_days"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Frequency) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "frequency is required."})
			return
		}
		if body.StopDuringBackup != nil && *body.StopDuringBackup {
			if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, meta.TeamScopedRef); err != nil {
				writeDomainError(w, err)
				return
			}
		} else {
			if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
				writeDomainError(w, err)
				return
			}
		}
		var s3ID *int64
		if body.S3StorageUUID != nil && strings.TrimSpace(*body.S3StorageUUID) != "" {
			var sid int64
			if err := a.Pool.QueryRow(r.Context(), `SELECT id FROM s3_storages WHERE uuid = $1 AND team_id = $2`, strings.TrimSpace(*body.S3StorageUUID), meta.TeamID).Scan(&sid); err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "s3 storage not found in team"})
				return
			}
			s3ID = &sid
		}
		enabled := true
		if body.Enabled != nil {
			enabled = *body.Enabled
		}
		saveS3 := body.SaveS3 != nil && *body.SaveS3
		disableLocal := body.DisableLocalBackup != nil && *body.DisableLocalBackup
		stopDuring := body.StopDuringBackup != nil && *body.StopDuringBackup
		retLocal := 7
		if body.RetentionAmountLocally != nil {
			retLocal = *body.RetentionAmountLocally
		}
		timeout := 3600
		if body.Timeout != nil {
			timeout = *body.Timeout
		}
		missingDays := 0
		if body.MissingBackupNotifyDays != nil {
			missingDays = *body.MissingBackupNotifyDays
		}

		schedUUID := a.Hooks.NewCUID2()
		err := a.Pool.QueryRow(r.Context(), `
			INSERT INTO scheduled_volume_backups (
				uuid, backupable_type, backupable_id, team_id, s3_storage_id, frequency,
				enabled, save_s3, disable_local_backup, stop_during_backup,
				retention_amount_locally, timeout, missing_backup_notification_days
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			ON CONFLICT (backupable_type, backupable_id) DO UPDATE SET
				s3_storage_id = EXCLUDED.s3_storage_id,
				frequency = EXCLUDED.frequency,
				enabled = EXCLUDED.enabled,
				save_s3 = EXCLUDED.save_s3,
				disable_local_backup = EXCLUDED.disable_local_backup,
				stop_during_backup = EXCLUDED.stop_during_backup,
				retention_amount_locally = EXCLUDED.retention_amount_locally,
				timeout = EXCLUDED.timeout,
				missing_backup_notification_days = EXCLUDED.missing_backup_notification_days,
				updated_at = NOW()
			RETURNING uuid
		`, schedUUID, backupableType, backupableID, meta.TeamID, s3ID, strings.TrimSpace(body.Frequency),
			enabled, saveS3, disableLocal, stopDuring, retLocal, timeout, missingDays).Scan(&schedUUID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"uuid": schedUUID, "message": "Volume backup schedule saved.",
			"storage_uuid": storageUUID, "storage_type": storageType,
			"frequency": body.Frequency, "enabled": enabled,
			"save_s3": saveS3, "disable_local_backup": disableLocal, "stop_during_backup": stopDuring,
			"retention_amount_locally": retLocal, "timeout": timeout,
			"missing_backup_notification_days": missingDays,
		})

	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, meta.TeamScopedRef); err != nil {
			writeDomainError(w, err)
			return
		}
		tag, _ := a.Pool.Exec(r.Context(), `DELETE FROM scheduled_volume_backups WHERE backupable_type = $1 AND backupable_id = $2`, backupableType, backupableID)
		if tag.RowsAffected() == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "volume backup schedule not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Volume backup schedule deleted."})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (a *App) handleResourceMove(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
		writeDomainError(w, err)
		return
	}
	var body struct {
		EnvironmentUUID string `json:"environment_uuid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.EnvironmentUUID) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "environment_uuid is required."})
		return
	}
	var targetEnvID, targetTeamID int64
	var targetEnvUUID, targetProjUUID string
	err := a.Pool.QueryRow(r.Context(), `
		SELECT e.id, e.uuid, p.uuid, p.team_id
		FROM environments e
		JOIN projects p ON p.id = e.project_id
		WHERE e.uuid = $1 AND p.team_id = $2
	`, strings.TrimSpace(body.EnvironmentUUID), meta.TeamID).Scan(&targetEnvID, &targetEnvUUID, &targetProjUUID, &targetTeamID)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Target environment not found or not owned by your team."})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{
		Type: "environment", ID: targetEnvID, UUID: targetEnvUUID, ProjectUUID: targetProjUUID, EnvironmentUUID: targetEnvUUID,
		TeamID: &targetTeamID, APISegment: "projects",
	}); err != nil {
		writeDomainError(w, err)
		return
	}
	if targetEnvID == meta.EnvironmentID {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": meta.Kind + " is already in this environment."})
		return
	}
	tbl := "applications"
	if meta.Kind == "service" {
		tbl = "services"
	} else if meta.Kind == "database" {
		tbl = meta.DBTable
	}
	_, _ = a.Pool.Exec(r.Context(), fmt.Sprintf(`UPDATE %s SET environment_id = $1, updated_at = NOW() WHERE id = $2`, tbl), targetEnvID, meta.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"message":          strings.ToUpper(meta.Kind[:1]) + meta.Kind[1:] + " moved successfully.",
		"uuid":             meta.UUID,
		"project_uuid":     targetProjUUID,
		"environment_uuid": targetEnvUUID,
	})
}

func (a *App) handleResourceMigrate(w http.ResponseWriter, r *http.Request, p auth.Principal, meta slice11ResourceMeta) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
		writeDomainError(w, err)
		return
	}
	var body struct {
		DestinationUUID string `json:"destination_uuid"`
		MigrateVolumes  bool   `json:"migrate_volumes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.DestinationUUID) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "destination_uuid is required."})
		return
	}
	var destID, srvID, srvTeamID int64
	var srvUUID string
	var forceDisabled bool
	var transferredAt *time.Time
	err := a.Pool.QueryRow(r.Context(), `
		SELECT sd.id, s.id, s.uuid, s.team_id, COALESCE(ss.force_disabled, FALSE), s.transferred_at
		FROM standalone_dockers sd
		JOIN servers s ON s.id = sd.server_id
		LEFT JOIN server_settings ss ON ss.server_id = s.id
		WHERE sd.uuid = $1 AND s.team_id = $2 AND s.deleted_at IS NULL
	`, strings.TrimSpace(body.DestinationUUID), meta.TeamID).Scan(&destID, &srvID, &srvUUID, &srvTeamID, &forceDisabled, &transferredAt)
	if errors.Is(err, pgx.ErrNoRows) || forceDisabled || transferredAt != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Destination not found."})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionPlaceResource, access.ResourceRef{
		Type: "server", ID: srvID, UUID: srvUUID, TeamID: &srvTeamID, APISegment: "servers",
	}); err != nil {
		writeDomainError(w, err)
		return
	}
	tbl := "applications"
	if meta.Kind == "service" {
		tbl = "services"
		_, _ = a.Pool.Exec(r.Context(), `UPDATE services SET destination_id = $1, server_id = $2, updated_at = NOW() WHERE id = $3`, destID, srvID, meta.ID)
	} else {
		if meta.Kind == "database" {
			tbl = meta.DBTable
		}
		_, _ = a.Pool.Exec(r.Context(), fmt.Sprintf(`UPDATE %s SET destination_id = $1, updated_at = NOW() WHERE id = $2`, tbl), destID, meta.ID)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":          strings.ToUpper(meta.Kind[:1]) + meta.Kind[1:] + " migrated successfully.",
		"uuid":             meta.UUID,
		"destination_uuid": body.DestinationUUID,
		"server_uuid":      srvUUID,
	})
}

// ---- Upload / Download Backups (routes/web.php preservation) ----

var allowedBackupExtensions = []string{".sql", ".dump", ".gz", ".tgz", ".tar", ".bak", ".db", ".sqlite", ".zip"}

func (a *App) handleUploadDatabaseBackup(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	dbUUID := strings.Trim(strings.TrimPrefix(r.URL.Path, "/upload/backup"), "/")
	meta, err := a.loadResourceMeta(r.Context(), "database", dbUUID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Database not found."})
		return
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, meta.TeamScopedRef); err != nil {
		writeDomainError(w, err)
		return
	}
	a.saveUploadedBackupFile(w, r, meta)
}

func (a *App) saveUploadedBackupFile(w http.ResponseWriter, r *http.Request, meta slice11ResourceMeta) {
	var src io.Reader
	var filename string
	if ct := r.Header.Get("Content-Type"); strings.Contains(ct, "multipart/form-data") {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "invalid multipart upload"})
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "missing file upload"})
			return
		}
		defer f.Close()
		src = f
		filename = hdr.Filename
	} else {
		filename = r.URL.Query().Get("filename")
		if filename == "" {
			filename = "restore.sql"
		}
		src = r.Body
	}
	extOK := false
	low := strings.ToLower(filename)
	for _, ext := range allowedBackupExtensions {
		if strings.HasSuffix(low, ext) {
			extOK = true
			break
		}
	}
	if !extOK {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "Unsupported file type."})
		return
	}
	baseDir := os.Getenv("COOLIFY_BACKUP_DIR")
	if baseDir == "" {
		baseDir = filepath.Join(os.TempDir(), "coolify-backups")
	}
	destDir := filepath.Join(baseDir, "upload", filepath.Base(meta.UUID))
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	destPath := filepath.Join(destDir, "restore")
	out, err := os.Create(destPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	n, copyErr := io.Copy(out, io.LimitReader(src, 100<<20))
	_ = out.Close()
	if copyErr != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": copyErr.Error()})
		return
	}
	impUUID := a.Hooks.NewCUID2()
	var impID int64
	_ = a.Pool.QueryRow(r.Context(), `
		INSERT INTO go_upload_imports (uuid, team_id, resource_type, resource_uuid, filename, filepath, size, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'uploaded') RETURNING id
	`, impUUID, meta.TeamID, meta.Kind, meta.UUID, filepath.Base(filename), destPath, n).Scan(&impID)

	sum := sha256.Sum256([]byte(destPath))
	writeJSON(w, http.StatusOK, map[string]any{
		"mime_type":   "application-octet-stream",
		"activity_id": impID,
		"uuid":        impUUID,
		"size":        n,
		"path_hash":   hex.EncodeToString(sum[:8]),
	})
}

func (a *App) handleDownloadDatabaseBackup(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	seg := strings.Trim(strings.TrimPrefix(r.URL.Path, "/download/backup"), "/")
	var teamID int64
	var filename string
	var deleted bool
	err := a.Pool.QueryRow(r.Context(), `
		SELECT sb.team_id, COALESCE(ex.filename,''), COALESCE(ex.local_storage_deleted, FALSE)
		FROM scheduled_database_backup_executions ex
		JOIN scheduled_database_backups sb ON sb.id = ex.scheduled_database_backup_id
		WHERE ex.uuid = $1 OR ex.id::text = $1
	`, seg).Scan(&teamID, &filename, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Backup not found."})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionReadSensitive, access.ResourceRef{
		Type: "database", TeamID: &teamID, APISegment: "backups",
	}); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "Only team admins/owners can download backups."})
		return
	}
	if deleted || filename == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Backup not found locally on the server."})
		return
	}
	if _, statErr := os.Stat(filename); statErr != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Backup not found locally on the server."})
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(filename)))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeFile(w, r, filename)
}

func (a *App) handleDownloadVolumeBackup(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	seg := strings.Trim(strings.TrimPrefix(r.URL.Path, "/download/volume-backup"), "/")
	var teamID int64
	var filename string
	var deleted bool
	err := a.Pool.QueryRow(r.Context(), `
		SELECT vb.team_id, COALESCE(ex.filename,''), COALESCE(ex.local_storage_deleted, FALSE)
		FROM scheduled_volume_backup_executions ex
		JOIN scheduled_volume_backups vb ON vb.id = ex.scheduled_volume_backup_id
		WHERE ex.uuid = $1 OR ex.id::text = $1
	`, seg).Scan(&teamID, &filename, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Backup not found."})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionReadSensitive, access.ResourceRef{
		Type: "service", TeamID: &teamID, APISegment: "backups",
	}); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "Only team admins/owners can download backups."})
		return
	}
	if deleted || filename == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Backup not found locally on the server."})
		return
	}
	if _, statErr := os.Stat(filename); statErr != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Backup not found locally on the server."})
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(filename)))
	w.Header().Set("Content-Type", "application/gzip")
	http.ServeFile(w, r, filename)
}

// Unused import guards until subresources use realtime
var _ = realtime.DomainEvent{}
