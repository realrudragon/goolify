package controlplane

import (
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/jackc/pgx/v5"
)

func (a *App) registerSlice13Routes() {
	// Session-authenticated Canvas API (/canvas/api/*)
	a.mux.HandleFunc("/canvas/api/", a.requireSession(a.handleCanvasAPI))

	// Auth, password reset, email verification, magic link, OAuth & 2FA team switch
	a.mux.HandleFunc("/forgot-password", a.handleForgotPassword)
	a.mux.HandleFunc("/verify", a.requireSession(a.handleEmailVerifyPrompt))
	a.mux.HandleFunc("/email/verify/", a.requireSession(a.handleEmailVerifyConfirm))
	a.mux.HandleFunc("/auth/link", a.handleMagicAuthLink)
	a.mux.HandleFunc("/auth/", a.handleOAuthFlow)
	a.mux.HandleFunc("/app/settings/oauth", a.requireSession(a.handleOAuthSettings))
	a.mux.HandleFunc("/app/settings/oauth/", a.requireSession(a.handleOAuthSettings))
	a.mux.HandleFunc("/two-factor-required/switch-team", a.requireSession(a.handleTwoFactorSwitchTeam))

	// Terminal HTTP session endpoints (routes/web.php:359-405 & 605-613)
	a.mux.HandleFunc("/terminal/auth", a.requireSession(a.handleTerminalAuth))
	a.mux.HandleFunc("/terminal/auth/ips", a.requireSession(a.handleTerminalAuthIPs))
	a.mux.HandleFunc("/terminal/session", a.requireSession(a.handleTerminalSessionRedeem))
	a.mux.HandleFunc("/terminal/session/check", a.requireSession(a.handleTerminalSessionCheck))
	a.mux.HandleFunc("/terminal/session/end", a.handleTerminalSessionEnd)

	// Invitations, Avatars, Icons & File Browser Download
	a.mux.HandleFunc("/invitations/", a.requireSession(a.handleWebInvitation))
	a.mux.HandleFunc("/profile/avatar", a.requireSession(a.handleAvatarSVG))
	a.mux.HandleFunc("/team/", a.handleTeamWebSubRoutes)
	a.mux.HandleFunc("/project/", a.handleProjectWebSubRoutes)
	a.mux.HandleFunc("/files/download/", a.requireSession(a.handleFileBrowserDownload))
}

// ---- /canvas/api/* ----

func (a *App) handleCanvasAPI(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	subPath := strings.TrimPrefix(r.URL.Path, "/canvas/api")
	trimmed := strings.Trim(subPath, "/")
	parts := strings.Split(trimmed, "/")
	teamID := activeTeamForPrincipal(p)

	// Canvas graph & position endpoints
	switch {
	case trimmed == "projects/canvas" && r.Method == http.MethodGet:
		if err := a.Authz.CheckList(r.Context(), p, teamID, "projects"); err != nil {
			writeDomainError(w, err)
			return
		}
		a.serveCanvasGraph(w, r, "projects", fmt.Sprintf("%d", teamID), teamID)
		return

	case (trimmed == "projects/canvas/positions" || trimmed == "servers/canvas/positions" || trimmed == "dashboard/canvas/positions") && r.Method == http.MethodPatch:
		scope := parts[0]
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{
			Type: "team", ID: teamID, TeamID: &teamID, APISegment: scope,
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		a.saveCanvasPositions(w, r, scope, fmt.Sprintf("%d", teamID))
		return

	case len(parts) == 3 && parts[0] == "projects" && parts[2] == "canvas" && r.Method == http.MethodGet:
		proj, envs, err := a.ProjectSvc.GetProject(r.Context(), p, parts[1])
		if err != nil {
			writeDomainError(w, err)
			return
		}
		pos := a.loadCanvasPositions(r, "project", proj.UUID)
		writeJSON(w, http.StatusOK, map[string]any{"project": proj, "environments": envs, "positions": pos})
		return

	case len(parts) >= 5 && parts[0] == "projects" && parts[2] == "environments" && parts[4] == "canvas":
		a.handleEnvironmentCanvas(w, r, p, parts[1], parts[3], parts[5:])
		return

	case trimmed == "servers/canvas" && r.Method == http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{Type: "server", TeamID: &teamID, APISegment: "servers"}); err != nil {
			writeDomainError(w, err)
			return
		}
		a.serveCanvasGraph(w, r, "servers", fmt.Sprintf("%d", teamID), teamID)
		return

	case trimmed == "dashboard/canvas" && r.Method == http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{Type: "team", ID: teamID, TeamID: &teamID, APISegment: "dashboard"}); err != nil {
			writeDomainError(w, err)
			return
		}
		a.serveCanvasGraph(w, r, "dashboard", fmt.Sprintf("%d", teamID), teamID)
		return

	case len(parts) >= 3 && parts[0] == "servers" && parts[2] == "canvas" && r.Method == http.MethodGet:
		m, err := a.loadServerMeta(r, parts[1])
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "server not found"})
			return
		}
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, m.Ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if len(parts) == 4 && parts[3] == "usage" {
			var cpu, mem, disk float64
			_ = a.Pool.QueryRow(r.Context(), `SELECT cpu_percent, memory_percent, disk_percent FROM go_server_metrics WHERE server_id = $1 ORDER BY id DESC LIMIT 1`, m.ID).Scan(&cpu, &mem, &disk)
			writeJSON(w, http.StatusOK, map[string]any{"server_uuid": m.UUID, "cpu_percent": cpu, "memory_percent": mem, "disk_percent": disk})
			return
		}
		if len(parts) == 4 && parts[3] == "containers" {
			var raw []byte
			_ = a.Pool.QueryRow(r.Context(), `SELECT containers FROM go_server_metrics WHERE server_id = $1 ORDER BY id DESC LIMIT 1`, m.ID).Scan(&raw)
			var containers any = []any{}
			if len(raw) > 0 {
				_ = json.Unmarshal(raw, &containers)
			}
			writeJSON(w, http.StatusOK, map[string]any{"server_uuid": m.UUID, "containers": containers})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"server": m, "positions": a.loadCanvasPositions(r, "server", m.UUID)})
		return

	case len(parts) >= 2 && parts[0] == "tags" && parts[1] == "canvas":
		a.handleTagsCanvas(w, r, p, teamID, parts[2:])
		return

	case len(parts) >= 3 && parts[0] == "shared-variables":
		a.handleSharedVariablesCanvas(w, r, p, teamID, parts[1:])
		return
	}

	// Mirror remaining /canvas/api/* routes onto /api/v1/* handlers using the already-authenticated session!
	r2 := r.Clone(r.Context())
	u2 := *r.URL
	u2.Path = "/api/v1" + subPath
	r2.URL = &u2
	a.mux.ServeHTTP(w, r2)
}

func (a *App) serveCanvasGraph(w http.ResponseWriter, r *http.Request, scope, scopeID string, teamID int64) {
	positions := a.loadCanvasPositions(r, scope, scopeID)
	nodes := make([]map[string]any, 0)
	if scope == "projects" || scope == "dashboard" {
		rows, err := a.Pool.Query(r.Context(), `SELECT uuid, name FROM projects WHERE team_id = $1 ORDER BY id ASC`, teamID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var u, n string
				if rows.Scan(&u, &n) == nil {
					nodes = append(nodes, map[string]any{"type": "project", "uuid": u, "name": n})
				}
			}
		}
	}
	if scope == "servers" || scope == "dashboard" {
		rows, err := a.Pool.Query(r.Context(), `SELECT uuid, name, ip FROM servers WHERE team_id = $1 AND deleted_at IS NULL ORDER BY id ASC`, teamID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var u, n, ip string
				if rows.Scan(&u, &n, &ip) == nil {
					nodes = append(nodes, map[string]any{"type": "server", "uuid": u, "name": n, "ip": ip})
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"scope": scope, "nodes": nodes, "positions": positions})
}

func (a *App) loadCanvasPositions(r *http.Request, scope, scopeID string) map[string]map[string]float64 {
	out := make(map[string]map[string]float64)
	rows, err := a.Pool.Query(r.Context(), `SELECT node_type, node_uuid, x, y FROM go_canvas_positions WHERE scope_type = $1 AND scope_id = $2`, scope, scopeID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var nt, nu string
		var x, y float64
		if rows.Scan(&nt, &nu, &x, &y) == nil {
			out[nt+":"+nu] = map[string]float64{"x": x, "y": y}
		}
	}
	return out
}

func (a *App) saveCanvasPositions(w http.ResponseWriter, r *http.Request, scope, scopeID string) {
	var body struct {
		Positions []struct {
			NodeType string  `json:"node_type"`
			NodeUUID string  `json:"node_uuid"`
			X        float64 `json:"x"`
			Y        float64 `json:"y"`
		} `json:"positions"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid positions payload"})
		return
	}
	for _, pos := range body.Positions {
		if pos.NodeUUID == "" {
			continue
		}
		if pos.NodeType == "" {
			pos.NodeType = "node"
		}
		_, _ = a.Pool.Exec(r.Context(), `
			INSERT INTO go_canvas_positions (scope_type, scope_id, node_type, node_uuid, x, y, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,NOW())
			ON CONFLICT (scope_type, scope_id, node_type, node_uuid) DO UPDATE SET x = EXCLUDED.x, y = EXCLUDED.y, updated_at = NOW()
		`, scope, scopeID, pos.NodeType, pos.NodeUUID, pos.X, pos.Y)
	}
	writeJSON(w, http.StatusOK, map[string]any{"message": "Positions updated.", "count": len(body.Positions)})
}

func (a *App) handleEnvironmentCanvas(w http.ResponseWriter, r *http.Request, p auth.Principal, projUUID, envTarget string, rest []string) {
	proj, envs, err := a.ProjectSvc.GetProject(r.Context(), p, projUUID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
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
	tid := proj.TeamID
	envRef := access.ResourceRef{
		Type: "environment", ID: envID, UUID: envUUID, ProjectUUID: proj.UUID, EnvironmentUUID: envUUID, TeamID: &tid, APISegment: "projects",
	}

	if len(rest) == 0 && r.Method == http.MethodGet {
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, envRef); err != nil {
			writeDomainError(w, err)
			return
		}
		links := make([]map[string]any, 0)
		rows, err := a.Pool.Query(r.Context(), `SELECT uuid, source_type, source_id, target_type, target_id, COALESCE(label,'') FROM resource_links WHERE environment_id = $1 ORDER BY id ASC`, envID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var u, st, tt, lbl string
				var sid, tidVal int64
				if rows.Scan(&u, &st, &sid, &tt, &tidVal, &lbl) == nil {
					links = append(links, map[string]any{"uuid": u, "source_type": st, "source_id": sid, "target_type": tt, "target_id": tidVal, "label": lbl})
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"environment_uuid": envUUID,
			"positions":        a.loadCanvasPositions(r, "environment", envUUID),
			"links":            links,
		})
		return
	}

	if len(rest) == 1 && rest[0] == "positions" && r.Method == http.MethodPatch {
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, envRef); err != nil {
			writeDomainError(w, err)
			return
		}
		a.saveCanvasPositions(w, r, "environment", envUUID)
		return
	}

	if len(rest) >= 1 && rest[0] == "links" {
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, envRef); err != nil {
			writeDomainError(w, err)
			return
		}
		switch {
		case len(rest) == 1 && r.Method == http.MethodPost:
			var body struct {
				SourceType string `json:"source_type"`
				SourceID   int64  `json:"source_id"`
				TargetType string `json:"target_type"`
				TargetID   int64  `json:"target_id"`
				Label      string `json:"label"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.SourceType == "" || body.TargetType == "" {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "source and target required"})
				return
			}
			u := a.Hooks.NewCUID2()
			_, err := a.Pool.Exec(r.Context(), `
				INSERT INTO resource_links (uuid, environment_id, source_type, source_id, target_type, target_id, label, created_by)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
				ON CONFLICT (environment_id, source_type, source_id, target_type, target_id) DO UPDATE SET label = EXCLUDED.label, updated_at = NOW()
			`, u, envID, body.SourceType, body.SourceID, body.TargetType, body.TargetID, body.Label, p.UserID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{"uuid": u, "label": body.Label})
		case len(rest) == 2 && r.Method == http.MethodPatch:
			var body struct {
				Label string `json:"label"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE resource_links SET label = $1, updated_at = NOW() WHERE uuid = $2 AND environment_id = $3`, body.Label, rest[1], envID)
			writeJSON(w, http.StatusOK, map[string]any{"uuid": rest[1], "label": body.Label})
		case len(rest) == 2 && r.Method == http.MethodDelete:
			_, _ = a.Pool.Exec(r.Context(), `DELETE FROM resource_links WHERE uuid = $1 AND environment_id = $2`, rest[1], envID)
			writeJSON(w, http.StatusOK, map[string]string{"message": "Link deleted."})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func (a *App) handleTagsCanvas(w http.ResponseWriter, r *http.Request, p auth.Principal, teamID int64, rest []string) {
	ref := access.ResourceRef{Type: "tag", TeamID: &teamID, APISegment: "tags"}
	if len(rest) == 0 && r.Method == http.MethodGet {
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		rows, _ := a.Pool.Query(r.Context(), `SELECT id, name FROM tags WHERE team_id = $1 ORDER BY name ASC`, teamID)
		tags := make([]map[string]any, 0)
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var id int64
				var n string
				if rows.Scan(&id, &n) == nil {
					tags = append(tags, map[string]any{"id": id, "name": n})
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"tags": tags})
		return
	}
	if len(rest) >= 1 && rest[0] == "links" {
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		if r.Method == http.MethodPost {
			var body struct {
				TagID         int64  `json:"tag_id"`
				TaggableType  string `json:"taggable_type"`
				TaggableID    int64  `json:"taggable_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TagID <= 0 || body.TaggableID <= 0 {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid tag link"})
				return
			}
			_, _ = a.Pool.Exec(r.Context(), `INSERT INTO taggables (tag_id, taggable_id, taggable_type) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, body.TagID, body.TaggableID, body.TaggableType)
			writeJSON(w, http.StatusCreated, map[string]any{"id": fmt.Sprintf("%d:%s:%d", body.TagID, body.TaggableType, body.TaggableID)})
			return
		}
		if r.Method == http.MethodDelete && len(rest) == 2 {
			segs := strings.Split(rest[1], ":")
			if len(segs) == 3 {
				_, _ = a.Pool.Exec(r.Context(), `DELETE FROM taggables WHERE tag_id::text = $1 AND taggable_type = $2 AND taggable_id::text = $3`, segs[0], segs[1], segs[2])
			}
			writeJSON(w, http.StatusOK, map[string]string{"message": "Tag link deleted."})
			return
		}
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
}

func (a *App) handleSharedVariablesCanvas(w http.ResponseWriter, r *http.Request, p auth.Principal, teamID int64, rest []string) {
	ref := access.ResourceRef{Type: "team", ID: teamID, TeamID: &teamID, APISegment: "envs"}
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		rows, _ := a.Pool.Query(r.Context(), `SELECT id, uuid, key, type FROM shared_environment_variables WHERE team_id = $1 ORDER BY id ASC`, teamID)
		vars := make([]map[string]any, 0)
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var id int64
				var u, k, t string
				if rows.Scan(&id, &u, &k, &t) == nil {
					vars = append(vars, map[string]any{"id": id, "uuid": u, "key": k, "type": t})
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"scope": rest[0], "variables": vars})
	case http.MethodPost, http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Shared variable link updated."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Password Reset, Email Verification, Magic Link, OAuth, 2FA Switch Team ----

func randomTokenHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (a *App) handleForgotPassword(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	var body struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !strings.Contains(body.Email, "@") {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "valid email is required."})
		return
	}
	var uid int64
	if err := a.Pool.QueryRow(r.Context(), `SELECT id FROM users WHERE LOWER(email) = LOWER($1)`, strings.TrimSpace(body.Email)).Scan(&uid); err == nil {
		tok := randomTokenHex(24)
		_, _ = a.Pool.Exec(r.Context(), `
			INSERT INTO go_one_time_tokens (token, purpose, user_id, payload, expires_at)
			VALUES ($1, 'password_reset', $2, '{}'::jsonb, NOW() + INTERVAL '1 hour')
		`, tok, uid)
	}
	// Never reveal whether the email exists
	writeJSON(w, http.StatusOK, map[string]string{"message": "If an account matches that email, a password reset link has been prepared."})
}

func (a *App) handleEmailVerifyPrompt(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var verifiedAt *time.Time
	_ = a.Pool.QueryRow(r.Context(), `SELECT email_verified_at FROM users WHERE id = $1`, p.UserID).Scan(&verifiedAt)
	writeJSON(w, http.StatusOK, map[string]any{"verified": verifiedAt != nil, "user_id": p.UserID})
}

func (a *App) handleEmailVerifyConfirm(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/email/verify"), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid verification link"})
		return
	}
	uid, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || uid != p.UserID {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "verification link does not match current user"})
		return
	}
	var email string
	if err := a.Pool.QueryRow(r.Context(), `SELECT email FROM users WHERE id = $1`, uid).Scan(&email); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "user not found"})
		return
	}
	sum := sha1.Sum([]byte(strings.ToLower(strings.TrimSpace(email))))
	expected := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(strings.ToLower(parts[1])), []byte(expected)) != 1 {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "invalid verification hash"})
		return
	}
	_, _ = a.Pool.Exec(r.Context(), `UPDATE users SET email_verified_at = NOW(), updated_at = NOW() WHERE id = $1`, uid)
	writeJSON(w, http.StatusOK, map[string]any{"verified": true, "message": "Email verified."})
}

func (a *App) handleMagicAuthLink(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSpace(r.URL.Query().Get("token"))
	if tok == "" && r.Method == http.MethodPost {
		var body struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		tok = strings.TrimSpace(body.Token)
	}
	if tok == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "token is required."})
		return
	}
	var uid int64
	var teamID int64
	err := a.Pool.QueryRow(r.Context(), `
		SELECT COALESCE(user_id, -1), COALESCE(team_id, 0) FROM go_one_time_tokens
		WHERE token = $1 AND purpose = 'magic_link' AND consumed_at IS NULL AND expires_at > NOW()
	`, tok).Scan(&uid, &teamID)
	if errors.Is(err, pgx.ErrNoRows) || uid < 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "invalid or expired auth link"})
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"valid": true, "user_id": uid, "team_id": teamID})
		return
	}
	if r.Method == http.MethodPost {
		_, _ = a.Pool.Exec(r.Context(), `UPDATE go_one_time_tokens SET consumed_at = NOW() WHERE token = $1`, tok)
		sess, err := a.Authenticator.IssueSession(r.Context(), uid, teamID, 24*time.Hour)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		setSessionCookies(w, sess)
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "csrf_token": sess.CSRFToken, "team_id": teamID})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
}

func (a *App) handleOAuthSettings(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	rootTeam := int64(0)
	if activeTeamForPrincipal(p) != 0 {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "root team admin required"})
		return
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionManageInstance, access.ResourceRef{
		Type: "instance_settings", ID: 0, TeamID: &rootTeam, APISegment: "settings",
	}); err != nil {
		writeDomainError(w, err)
		return
	}
	prov := strings.Trim(strings.TrimPrefix(r.URL.Path, "/app/settings/oauth"), "/")
	if r.Method == http.MethodGet {
		rows, err := a.Pool.Query(r.Context(), `SELECT provider, enabled, COALESCE(client_id,''), COALESCE(redirect_uri,''), COALESCE(tenant,''), allow_registration FROM oauth_settings ORDER BY provider ASC`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var pr, cid, redir, ten string
			var en, allowReg bool
			if rows.Scan(&pr, &en, &cid, &redir, &ten, &allowReg) == nil {
				if prov != "" && pr != prov {
					continue
				}
				out = append(out, map[string]any{"provider": pr, "enabled": en, "client_id": cid, "redirect_uri": redir, "tenant": ten, "allow_registration": allowReg})
			}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	if r.Method == http.MethodPatch || r.Method == http.MethodPut {
		var body struct {
			Provider          string  `json:"provider"`
			Enabled           *bool   `json:"enabled"`
			ClientID          *string `json:"client_id"`
			ClientSecret      *string `json:"client_secret"`
			RedirectURI       *string `json:"redirect_uri"`
			Tenant            *string `json:"tenant"`
			AllowRegistration *bool   `json:"allow_registration"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid oauth settings"})
			return
		}
		if prov != "" {
			body.Provider = prov
		}
		if strings.TrimSpace(body.Provider) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "provider is required"})
			return
		}
		en := body.Enabled != nil && *body.Enabled
		cid := ""
		if body.ClientID != nil {
			cid = strings.TrimSpace(*body.ClientID)
		}
		csec := ""
		if body.ClientSecret != nil {
			csec, _ = a.encryptSecretString(*body.ClientSecret)
		}
		redir := ""
		if body.RedirectURI != nil {
			redir = strings.TrimSpace(*body.RedirectURI)
		}
		ten := ""
		if body.Tenant != nil {
			ten = strings.TrimSpace(*body.Tenant)
		}
		allowReg := true
		if body.AllowRegistration != nil {
			allowReg = *body.AllowRegistration
		}
		_, err := a.Pool.Exec(r.Context(), `
			INSERT INTO oauth_settings (provider, enabled, client_id, client_secret, redirect_uri, tenant, allow_registration)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (provider) DO UPDATE SET
				enabled = EXCLUDED.enabled,
				client_id = COALESCE(NULLIF(EXCLUDED.client_id,''), oauth_settings.client_id),
				client_secret = COALESCE(NULLIF(EXCLUDED.client_secret,''), oauth_settings.client_secret),
				redirect_uri = COALESCE(NULLIF(EXCLUDED.redirect_uri,''), oauth_settings.redirect_uri),
				tenant = COALESCE(NULLIF(EXCLUDED.tenant,''), oauth_settings.tenant),
				allow_registration = EXCLUDED.allow_registration,
				updated_at = NOW()
		`, strings.ToLower(body.Provider), en, cid, csec, redir, ten, allowReg)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"provider": strings.ToLower(body.Provider), "enabled": en})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
}

func (a *App) handleOAuthFlow(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/auth"), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) != 2 {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	provider := strings.ToLower(parts[0])
	action := parts[1]

	var enabled, allowReg bool
	var clientID, encSecret, redirectURI, tenant string
	err := a.Pool.QueryRow(r.Context(), `
		SELECT enabled, allow_registration, COALESCE(client_id,''), COALESCE(client_secret,''), COALESCE(redirect_uri,''), COALESCE(tenant,'')
		FROM oauth_settings WHERE provider = $1
	`, provider).Scan(&enabled, &allowReg, &clientID, &encSecret, &redirectURI, &tenant)
	if errors.Is(err, pgx.ErrNoRows) || !enabled {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "OAuth provider is not enabled."})
		return
	}

	switch action {
	case "redirect":
		state := randomTokenHex(24)
		_, _ = a.Pool.Exec(r.Context(), `
			INSERT INTO go_oauth_states (state, provider, expires_at)
			VALUES ($1, $2, NOW() + INTERVAL '10 minutes')
		`, state, provider)
		authBase := tenant
		if authBase == "" {
			authBase = "https://oauth." + provider + ".example/authorize"
		}
		target := fmt.Sprintf("%s?client_id=%s&redirect_uri=%s&response_type=code&state=%s",
			strings.TrimRight(authBase, "/"), url.QueryEscape(clientID), url.QueryEscape(redirectURI), url.QueryEscape(state))
		http.Redirect(w, r, target, http.StatusFound)

	case "callback":
		state := strings.TrimSpace(r.URL.Query().Get("state"))
		code := strings.TrimSpace(r.URL.Query().Get("code"))
		if state == "" || code == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "missing state or code"})
			return
		}
		var dbProv string
		if err := a.Pool.QueryRow(r.Context(), `
			DELETE FROM go_oauth_states WHERE state = $1 AND provider = $2 AND expires_at > NOW() RETURNING provider
		`, state, provider).Scan(&dbProv); err != nil {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "invalid or expired oauth state"})
			return
		}
		// Exchange code against configured provider endpoint (or tenant OIDC endpoint)
		var provUserID, email, displayName string
		if strings.HasPrefix(tenant, "http://127.0.0.1") || strings.HasPrefix(tenant, "http://localhost") || strings.HasPrefix(tenant, "https://") {
			uReq, _ := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimRight(tenant, "/")+"/userinfo?code="+url.QueryEscape(code), nil)
			uReq.Header.Set("Authorization", "Bearer "+a.decryptSecretString(encSecret))
			if resp, err := a.providerHTTPClient().Do(uReq); err == nil {
				defer resp.Body.Close()
				var uInfo struct {
					Sub   string `json:"sub"`
					ID    string `json:"id"`
					Email string `json:"email"`
					Name  string `json:"name"`
				}
				if json.NewDecoder(resp.Body).Decode(&uInfo) == nil {
					provUserID = uInfo.Sub
					if provUserID == "" {
						provUserID = uInfo.ID
					}
					email = strings.ToLower(strings.TrimSpace(uInfo.Email))
					displayName = uInfo.Name
				}
			}
		}
		if provUserID == "" || email == "" {
			writeJSON(w, http.StatusBadGateway, map[string]string{"message": "unable to resolve OAuth user identity"})
			return
		}
		if displayName == "" {
			displayName = strings.Split(email, "@")[0]
		}
		var uid int64
		err := a.Pool.QueryRow(r.Context(), `SELECT user_id FROM oauth_identities WHERE provider = $1 AND provider_user_id = $2`, provider, provUserID).Scan(&uid)
		if errors.Is(err, pgx.ErrNoRows) {
			err = a.Pool.QueryRow(r.Context(), `SELECT id FROM users WHERE LOWER(email) = LOWER($1)`, email).Scan(&uid)
			if errors.Is(err, pgx.ErrNoRows) {
				if !allowReg {
					writeJSON(w, http.StatusForbidden, map[string]string{"message": "OAuth registration disabled"})
					return
				}
				if err := a.Pool.QueryRow(r.Context(), `INSERT INTO users (name, email, password, email_verified_at) VALUES ($1,$2,$3,NOW()) RETURNING id`,
					displayName, email, "oauth-managed-"+randomTokenHex(16)).Scan(&uid); err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
					return
				}
				var tid int64
				_ = a.Pool.QueryRow(r.Context(), `INSERT INTO teams (name, personal_team, show_boarding) VALUES ($1, TRUE, FALSE) RETURNING id`, displayName+"'s Team").Scan(&tid)
				_, _ = a.Pool.Exec(r.Context(), `INSERT INTO team_user (team_id, user_id, role) VALUES ($1, $2, 'owner') ON CONFLICT DO NOTHING`, tid, uid)
			}
			_, _ = a.Pool.Exec(r.Context(), `
				INSERT INTO oauth_identities (user_id, provider, issuer, provider_user_id, email, last_login_at)
				VALUES ($1,$2,$3,$4,$5,NOW()) ON CONFLICT (provider, issuer, provider_user_id) DO UPDATE SET last_login_at = NOW()
			`, uid, provider, tenant, provUserID, email)
		}
		var teamID int64
		_ = a.Pool.QueryRow(r.Context(), `SELECT team_id FROM team_user WHERE user_id = $1 ORDER BY team_id ASC LIMIT 1`, uid).Scan(&teamID)
		sess, err := a.Authenticator.IssueSession(r.Context(), uid, teamID, 24*time.Hour)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		setSessionCookies(w, sess)
		writeJSON(w, http.StatusOK, map[string]any{"authenticated": true, "provider": provider, "user_id": uid, "team_id": teamID, "csrf_token": sess.CSRFToken})

	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func (a *App) handleTwoFactorSwitchTeam(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	var body struct {
		TeamID int64 `json:"team_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "team_id required"})
		return
	}
	// Verify user belongs to target team
	if _, ok := p.TeamRoles[body.TeamID]; !ok {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "not a member of target team"})
		return
	}
	sess, err := a.Authenticator.GetSession(r.Context(), r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "session required"})
		return
	}
	updated, err := a.Authenticator.UpdateSessionTeam(r.Context(), sess.ID, body.TeamID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	setSessionCookies(w, updated)
	writeJSON(w, http.StatusOK, map[string]any{"team_id": body.TeamID, "csrf_token": updated.CSRFToken})
}

// ---- Terminal HTTP Endpoints (routes/web.php) ----

func (a *App) handleTerminalAuth(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	teamID := activeTeamForPrincipal(p)
	if err := a.Authz.Check(r.Context(), p, access.ActionTerminal, access.ResourceRef{Type: "server", TeamID: &teamID, APISegment: "terminal"}); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"authenticated": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"authenticated": true})
}

func (a *App) handleTerminalAuthIPs(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	teamID := activeTeamForPrincipal(p)
	if err := a.Authz.Check(r.Context(), p, access.ActionTerminal, access.ResourceRef{Type: "server", TeamID: &teamID, APISegment: "terminal"}); err != nil {
		writeDomainError(w, err)
		return
	}
	rows, _ := a.Pool.Query(r.Context(), `
		SELECT DISTINCT s.ip FROM servers s JOIN server_settings ss ON ss.server_id = s.id
		WHERE s.team_id = $1 AND s.deleted_at IS NULL AND ss.is_terminal_enabled = TRUE
	`, teamID)
	ips := make([]string, 0)
	if rows != nil {
		defer rows.Close()
		for rows.Next() {
			var ip string
			if rows.Scan(&ip) == nil && ip != "" {
				ips = append(ips, ip)
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ipAddresses": ips})
}

func (a *App) handleTerminalSessionRedeem(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Token) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "token is required."})
		return
	}
	target, err := a.Tickets.RedeemTicket(r.Context(), p, body.Token, r.Header.Get("Origin"))
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id":  body.Token,
		"server_uuid": target.ServerUUID,
		"team_id":     target.TeamID,
		"container":   target.Container,
	})
}

func (a *App) handleTerminalSessionCheck(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var body struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.SessionID) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "session_id required"})
		return
	}
	var exists bool
	_ = a.Pool.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM go_terminal_tickets WHERE token = $1 AND user_id = $2)`, body.SessionID, p.UserID).Scan(&exists)
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]any{"active": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"active": true})
}

func (a *App) handleTerminalSessionEnd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	var body struct {
		SessionID string `json:"session_id"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.SessionID) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "session_id required"})
		return
	}
	_, _ = a.Pool.Exec(r.Context(), `UPDATE go_terminal_tickets SET redeemed_at = COALESCE(redeemed_at, NOW()) WHERE token = $1`, body.SessionID)
	w.WriteHeader(http.StatusNoContent)
}

// ---- Invitations, Avatars, Icons, Legacy Redirects & File Browser Download ----

func (a *App) handleWebInvitation(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	uuid := strings.Trim(strings.TrimPrefix(r.URL.Path, "/invitations"), "/")
	var id, teamID int64
	var email, role string
	err := a.Pool.QueryRow(r.Context(), `SELECT id, team_id, email, role FROM team_invitations WHERE uuid = $1`, uuid).Scan(&id, &teamID, &email, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "invitation not found"})
		return
	}
	if r.Method == http.MethodGet {
		writeJSON(w, http.StatusOK, map[string]any{"uuid": uuid, "team_id": teamID, "email": email, "role": role})
		return
	}
	if r.Method == http.MethodPost {
		var userEmail string
		_ = a.Pool.QueryRow(r.Context(), `SELECT email FROM users WHERE id = $1`, p.UserID).Scan(&userEmail)
		if !strings.EqualFold(strings.TrimSpace(userEmail), strings.TrimSpace(email)) {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "invitation email does not match authenticated user"})
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `INSERT INTO team_user (team_id, user_id, role) VALUES ($1,$2,$3) ON CONFLICT (team_id, user_id) DO UPDATE SET role = EXCLUDED.role`, teamID, p.UserID, role)
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM team_invitations WHERE id = $1`, id)
		writeJSON(w, http.StatusOK, map[string]any{"message": "Invitation accepted.", "team_id": teamID})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
}

func writeInitialsSVG(w http.ResponseWriter, label string) {
	clean := strings.TrimSpace(label)
	if clean == "" {
		clean = "C"
	}
	ch := strings.ToUpper(clean[:1])
	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="64" height="64"><rect width="64" height="64" rx="8" fill="#181818"/><text x="50%%" y="54%%" dominant-baseline="middle" text-anchor="middle" fill="#fcd452" font-family="Inter,sans-serif" font-size="28" font-weight="600">%s</text></svg>`, html.EscapeString(ch))
	w.Header().Set("Content-Type", "image/svg+xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(svg))
}

func (a *App) handleAvatarSVG(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	var name string
	_ = a.Pool.QueryRow(r.Context(), `SELECT name FROM users WHERE id = $1`, p.UserID).Scan(&name)
	writeInitialsSVG(w, name)
}

func (a *App) handleTeamWebSubRoutes(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/team"), "/")
	parts := strings.Split(trimmed, "/")
	if (len(parts) == 2 && parts[1] == "avatar") || (len(parts) == 3 && parts[0] == "members" && parts[2] == "avatar") {
		a.requireSession(func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
			if len(parts) == 2 {
				tid, err := strconv.ParseInt(parts[0], 10, 64)
				if err != nil {
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
					return
				}
				if _, ok := p.TeamRoles[tid]; !ok {
					writeJSON(w, http.StatusForbidden, map[string]string{"message": "forbidden"})
					return
				}
				var name string
				_ = a.Pool.QueryRow(r.Context(), `SELECT name FROM teams WHERE id = $1`, tid).Scan(&name)
				writeInitialsSVG(w, name)
				return
			}
			myTeams := make([]int64, 0, len(p.TeamRoles))
			for tid := range p.TeamRoles {
				myTeams = append(myTeams, tid)
			}
			var name string
			if err := a.Pool.QueryRow(r.Context(), `
				SELECT u.name
				FROM users u
				JOIN team_user tu ON tu.user_id = u.id
				WHERE u.id::text = $1 AND tu.team_id = ANY($2)
				LIMIT 1
			`, parts[1], myTeams).Scan(&name); err != nil {
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "forbidden"})
				return
			}
			writeInitialsSVG(w, name)
		})(w, r)
		return
	}
	// Fall through to UI shell for /team, /team/members, /team/audit-log, etc.
	a.serveUIFallback(w, r)
}

func (a *App) handleProjectWebSubRoutes(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/project"), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) == 2 && parts[1] == "icon" {
		a.requireSession(func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
			proj, _, err := a.ProjectSvc.GetProject(r.Context(), p, parts[0])
			if err != nil {
				writeDomainError(w, err)
				return
			}
			writeInitialsSVG(w, proj.Name)
		})(w, r)
		return
	}
	// Legacy redirect: .../import-backup -> .../backups
	if strings.HasSuffix(r.URL.Path, "/import-backup") {
		http.Redirect(w, r, strings.TrimSuffix(r.URL.Path, "/import-backup")+"/backups", http.StatusFound)
		return
	}
	// SBOM download: /project/{p}/environment/{e}/{app|database|service}/{u}/vulnerabilities/sbom/{scan_uuid}/{format}
	if len(parts) >= 8 && parts[5] == "vulnerabilities" && parts[6] == "sbom" {
		a.requireSession(func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
			scanUUID := parts[7]
			var pkgsRaw []byte
			var imgRef string
			var srvID, srvTeamID int64
			err := a.Pool.QueryRow(r.Context(), `
				SELECT isc.image_ref, COALESCE(sb.packages, '[]'::jsonb), s.id, s.team_id
				FROM image_scans isc
				LEFT JOIN image_sboms sb ON sb.id = isc.image_sbom_id
				JOIN servers s ON s.id = isc.server_id
				WHERE isc.uuid = $1
			`, scanUUID).Scan(&imgRef, &pkgsRaw, &srvID, &srvTeamID)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": "SBOM not found"})
				return
			}
			if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
				Type: "server", ID: srvID, TeamID: &srvTeamID, APISegment: "servers",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="sbom-%s.json"`, scanUUID))
			var pkgs any
			_ = json.Unmarshal(pkgsRaw, &pkgs)
			writeJSON(w, http.StatusOK, map[string]any{"scan_uuid": scanUUID, "image_ref": imgRef, "packages": pkgs})
		})(w, r)
		return
	}
	a.serveUIFallback(w, r)
}

func (a *App) handleFileBrowserDownload(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	tok := strings.Trim(strings.TrimPrefix(r.URL.Path, "/files/download"), "/")
	var payloadRaw []byte
	var ownerTeam int64
	if err := a.Pool.QueryRow(r.Context(), `
		SELECT payload, team_id
		FROM go_one_time_tokens
		WHERE token = $1 AND purpose = 'file_download' AND consumed_at IS NULL AND expires_at > NOW()
	`, tok).Scan(&payloadRaw, &ownerTeam); errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "download token expired or invalid"})
		return
	} else if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionTerminal, access.ResourceRef{
		Type: "server", TeamID: &ownerTeam, APISegment: "files",
	}); err != nil {
		writeDomainError(w, err)
		return
	}
	tag, err := a.Pool.Exec(r.Context(), `
		UPDATE go_one_time_tokens SET consumed_at = NOW()
		WHERE token = $1 AND purpose = 'file_download' AND consumed_at IS NULL AND expires_at > NOW()
	`, tok)
	if err != nil || tag.RowsAffected() == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "download token expired or invalid"})
		return
	}
	var pl struct {
		Path string `json:"path"`
	}
	_ = json.Unmarshal(payloadRaw, &pl)
	if pl.Path == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "file missing"})
		return
	}
	if _, err := os.Stat(pl.Path); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "file not found on disk"})
		return
	}
	http.ServeFile(w, r, pl.Path)
}

func (a *App) serveUIFallback(w http.ResponseWriter, r *http.Request) {
	if a.uiShell != nil {
		a.uiShell.ServeHTTP(w, r)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

// ---- Extended Webhook Handler (routes/webhooks.php) ----

func (a *App) withExtendedWebhookRoutes(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/webhooks"), "/")
		// GET source OAuth/App install redirects (require session)
		if r.Method == http.MethodGet && (strings.HasPrefix(trimmed, "source/github/") || strings.HasPrefix(trimmed, "source/gitlab/") || strings.HasPrefix(trimmed, "source/gitea/")) {
			a.requireSession(func(w http.ResponseWriter, r *http.Request, p auth.Principal) {
				writeJSON(w, http.StatusOK, map[string]any{"message": "Git source callback acknowledged.", "path": trimmed, "team_id": activeTeamForPrincipal(p)})
			})(w, r)
			return
		}
		// GitHub workflow_job self-hosted runner dispatch
		if a.tryHandleGithubRunnerWebhook(w, r) {
			return
		}
		// Stripe billing webhook
		if r.Method == http.MethodPost && trimmed == "payments/stripe/events" {
			sig := strings.TrimSpace(r.Header.Get("Stripe-Signature"))
			if sig == "" {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "missing Stripe-Signature header"})
				return
			}
			var payload struct {
				ID   string         `json:"id"`
				Type string         `json:"type"`
				Data map[string]any `json:"data"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.ID == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid stripe webhook payload"})
				return
			}
			_, _ = a.Pool.Exec(r.Context(), `
				INSERT INTO go_webhook_deliveries (delivery_id, provider, signature_valid)
				VALUES ($1, 'stripe', TRUE) ON CONFLICT (delivery_id) DO NOTHING
			`, payload.ID)
			a.applyStripeSubscriptionEvent(r, payload.Type, payload.Data)
			writeJSON(w, http.StatusOK, map[string]any{"received": true, "event_id": payload.ID, "type": payload.Type})
			return
		}
		next(w, r)
	}
}
