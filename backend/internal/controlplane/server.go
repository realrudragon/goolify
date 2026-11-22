package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/applications"
	"github.com/coollabsio/coolify/backend/internal/domains/backups"
	"github.com/coollabsio/coolify/backend/internal/domains/compose"
	"github.com/coollabsio/coolify/backend/internal/domains/databases"
	"github.com/coollabsio/coolify/backend/internal/domains/notifications"
	"github.com/coollabsio/coolify/backend/internal/domains/projects"
	"github.com/coollabsio/coolify/backend/internal/domains/proxy"
	"github.com/coollabsio/coolify/backend/internal/domains/servers"
	"github.com/coollabsio/coolify/backend/internal/domains/services"
	"github.com/coollabsio/coolify/backend/internal/domains/teams"
	"github.com/coollabsio/coolify/backend/internal/domains/transfer"
	"github.com/coollabsio/coolify/backend/internal/domains/webhooks"
	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/coollabsio/coolify/backend/internal/realtime"
	goredis "github.com/coollabsio/coolify/backend/internal/store/redis"
	"github.com/coollabsio/coolify/backend/internal/web"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Pool          *pgxpool.Pool
	Redis         *goredis.Client // Optional; cache/pubsub degrade fail-closed if nil/down
	Cipher        *compat.Cipher
	StaticFS      fs.FS
	BackupDir     string
	BackupDumper  backups.Dumper
	Executor      *executor.Client
	AllowedOrigin string
}

// App is the real production standalone Go Control Plane HTTP server.
type App struct {
	Pool          *pgxpool.Pool
	Redis         *goredis.Client
	Cipher        *compat.Cipher
	Authz         *access.Authorizer
	Hooks         *compat.EloquentHooks
	Sessions      *auth.PostgresSessionStore
	Lookup        *auth.PostgresPrincipalLookup
	Authenticator *auth.Authenticator
	AuthRepo      *auth.PostgresAuthRepository

	Outbox     *realtime.PostgresOutboxStore
	Cache      *realtime.RedisCacheCoherence
	Hub        *realtime.RealtimeHub
	Dispatcher *realtime.PostgresDispatcher
	LogStore   *realtime.PostgresLogStore
	Tickets    *realtime.PostgresTerminalTicketStore
	JobCoord   *jobs.PostgresJobCoordinator
	Executor   *executor.Client

	ServerRepo   *servers.PostgresRepository
	ServerSvc    *servers.ServerService
	ProjectSvc   *projects.PostgresProjectService
	AppSvc       *applications.PostgresService
	ServiceSvc   *services.PostgresServiceManager
	DatabaseSvc  *databases.PostgresDatabaseManager
	BackupSvc    *backups.PostgresBackupService
	TeamSvc      *teams.PostgresTeamService
	WebhookSvc   *webhooks.PostgresWebhookService
	NotifySvc    *notifications.PostgresNotificationService
	TransferSvc  *transfer.PostgresTransferService
	ProxyGen     *proxy.ProxyConfigGenerator
	ComposeParse *compose.ComposeParser

	allowedOrigin string
	mux           *http.ServeMux
	uiShell       http.Handler
}

func NewApp(cfg Config) (*App, error) {
	if cfg.Pool == nil {
		return nil, errors.New("controlplane requires a PostgreSQL connection pool")
	}
	authz := access.NewAuthorizer()
	hooks := compat.NewEloquentHooks()
	sessions := auth.NewPostgresSessionStore(cfg.Pool)
	lookup := auth.NewPostgresPrincipalLookup(cfg.Pool)
	authenticator := auth.NewAuthenticator(lookup, sessions)
	authRepo := auth.NewPostgresAuthRepository(cfg.Pool, cfg.Cipher)

	outbox := realtime.NewPostgresOutboxStore(cfg.Pool)
	var cache *realtime.RedisCacheCoherence
	if cfg.Redis != nil {
		cache = realtime.NewRedisCacheCoherence(cfg.Redis)
	}
	hub := realtime.NewRealtimeHub(128)
	disp := realtime.NewPostgresDispatcher(outbox, cache, cfg.Redis, hub)
	logStore := realtime.NewPostgresLogStore(cfg.Pool)
	tickets := realtime.NewPostgresTerminalTicketStore(cfg.Pool, authz)
	jobCoord := jobs.NewPostgresJobCoordinator(cfg.Pool, 4)

	execClient := cfg.Executor
	if execClient == nil {
		execClient = executor.NewClient(executor.NewLocalCommandTransport())
	}

	srvRepo := servers.NewPostgresRepository(cfg.Pool, outbox)
	srvSvc := servers.NewServerService(srvRepo, authz, hooks, nil, nil)
	projSvc := projects.NewPostgresProjectService(cfg.Pool, authz, hooks, cfg.Cipher, outbox)
	appSvc := applications.NewPostgresService(cfg.Pool, authz, hooks, cfg.Cipher, outbox, jobCoord)
	compParser := compose.NewComposeParser()
	svcMgr := services.NewPostgresServiceManager(cfg.Pool, authz, hooks, compParser, outbox, jobCoord)
	dbMgr := databases.NewPostgresDatabaseManager(cfg.Pool, authz, hooks, cfg.Cipher, outbox, jobCoord)
	bkSvc := backups.NewPostgresBackupService(cfg.Pool, authz, hooks, cfg.BackupDumper, cfg.BackupDir)
	teamSvc := teams.NewPostgresTeamService(cfg.Pool, authz, outbox)
	whSvc := webhooks.NewPostgresWebhookService(cfg.Pool)
	notifySvc := notifications.NewPostgresNotificationService(cfg.Pool, authz, cfg.Cipher)
	transferSvc := transfer.NewPostgresTransferService(cfg.Pool, authz, cfg.Cipher, hooks)

	app := &App{
		Pool:          cfg.Pool,
		Redis:         cfg.Redis,
		Cipher:        cfg.Cipher,
		Authz:         authz,
		Hooks:         hooks,
		Sessions:      sessions,
		Lookup:        lookup,
		Authenticator: authenticator,
		AuthRepo:      authRepo,
		Outbox:        outbox,
		Cache:         cache,
		Hub:           hub,
		Dispatcher:    disp,
		LogStore:      logStore,
		Tickets:       tickets,
		JobCoord:      jobCoord,
		Executor:      execClient,
		ServerRepo:    srvRepo,
		ServerSvc:     srvSvc,
		ProjectSvc:    projSvc,
		AppSvc:        appSvc,
		ServiceSvc:    svcMgr,
		DatabaseSvc:   dbMgr,
		BackupSvc:     bkSvc,
		TeamSvc:       teamSvc,
		WebhookSvc:    whSvc,
		NotifySvc:     notifySvc,
		TransferSvc:   transferSvc,
		ProxyGen:      proxy.NewProxyConfigGenerator(),
		ComposeParse:  compParser,
		allowedOrigin: cfg.AllowedOrigin,
		mux:           http.NewServeMux(),
	}

	app.registerRoutes(cfg.StaticFS)
	return app, nil
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mux.ServeHTTP(w, r)
}

func (a *App) registerRoutes(staticFS fs.FS) {
	// Public health & version
	a.mux.HandleFunc("/api/health", a.handleHealth)
	a.mux.HandleFunc("/api/v1/health", a.handleHealth)
	a.mux.HandleFunc("/api/v1/version", a.handleVersion)

	// Browser Auth routes (native Go session + CSRF)
	a.mux.HandleFunc("/app/auth/login", a.handleLogin)
	a.mux.HandleFunc("/app/auth/register", a.handleRegister)
	a.mux.HandleFunc("/app/auth/logout", a.requireSession(a.handleLogout))
	a.mux.HandleFunc("/app/auth/me", a.requireSession(a.handleMe))
	a.mux.HandleFunc("/app/auth/password/force-reset", a.requireSession(a.handleForcePasswordReset))
	a.mux.HandleFunc("/app/auth/2fa/enable", a.requireSession(a.handleTwoFactorEnable))
	a.mux.HandleFunc("/app/auth/2fa/confirm", a.requireSession(a.handleTwoFactorConfirm))
	a.mux.HandleFunc("/app/auth/2fa/disable", a.requireSession(a.handleTwoFactorDisable))
	a.mux.HandleFunc("/app/auth/switch-team", a.requireSession(a.handleSwitchTeam))
	a.mux.HandleFunc("/app/auth/invitations/accept", a.requireSession(a.handleAcceptInvitation))
	a.mux.HandleFunc("/app/auth/tokens", a.requireSession(a.handleTokens))
	a.mux.HandleFunc("/app/auth/tokens/", a.requireSession(a.handleTokens))

	// Terminal ticket & live WebSocket
	a.mux.HandleFunc("/app/terminal/ticket", a.requireSession(a.handleTerminalTicket))
	a.mux.HandleFunc("/terminal/ws", a.handleTerminalWS)

	// Authenticated REST API v1 (Bearer token OR browser session+CSRF; NEVER default root!)
	a.mux.HandleFunc("/api/v1/disable", a.requireAPIAuth(a.handleInstanceToggle("is_api_enabled", false)))
	a.mux.HandleFunc("/api/v1/mcp/enable", a.requireAPIAuth(a.handleInstanceToggle("is_mcp_server_enabled", true)))
	a.mux.HandleFunc("/api/v1/mcp/disable", a.requireAPIAuth(a.handleInstanceToggle("is_mcp_server_enabled", false)))
	a.mux.HandleFunc("/api/v1/servers", a.requireAPIAuth(a.handleServers))
	a.mux.HandleFunc("/api/v1/servers/", a.requireAPIAuth(a.withExtendedServerRoutes(a.handleServers)))
	a.mux.HandleFunc("/api/v1/destinations", a.requireAPIAuth(a.handleDestinations))
	a.mux.HandleFunc("/api/v1/destinations/", a.requireAPIAuth(a.handleDestinations))
	a.mux.HandleFunc("/api/v1/resource-permissions", a.requireAPIAuth(a.handleUIResourcePermissions))
	a.mux.HandleFunc("/api/v1/security/keys", a.requireAPIAuth(a.handlePrivateKeys))
	a.mux.HandleFunc("/api/v1/security/keys/", a.requireAPIAuth(a.handlePrivateKeys))
	a.mux.HandleFunc("/api/v1/projects", a.requireAPIAuth(a.handleProjects))
	a.mux.HandleFunc("/api/v1/projects/", a.requireAPIAuth(a.withExtendedProjectRoutes(a.handleProjects)))
	a.mux.HandleFunc("/api/v1/shared-variables", a.requireAPIAuth(a.handleSharedVariables))
	a.mux.HandleFunc("/api/v1/resources", a.requireAPIAuth(a.handleResources))
	a.mux.HandleFunc("/api/v1/applications", a.requireAPIAuth(a.handleApplications))
	a.mux.HandleFunc("/api/v1/applications/", a.requireAPIAuth(a.withExtendedAppRoutes(a.handleApplications)))
	a.mux.HandleFunc("/api/v1/deploy", a.requireAPIAuth(a.handleDeploy))
	a.mux.HandleFunc("/api/v1/deployments", a.requireAPIAuth(a.handleDeployments))
	a.mux.HandleFunc("/api/v1/deployments/", a.requireAPIAuth(a.handleDeployments))
	a.mux.HandleFunc("/api/v1/services", a.requireAPIAuth(a.handleServices))
	a.mux.HandleFunc("/api/v1/services/", a.requireAPIAuth(a.withExtendedServiceRoutes(a.handleServices)))
	a.mux.HandleFunc("/api/v1/databases", a.requireAPIAuth(a.handleDatabases))
	a.mux.HandleFunc("/api/v1/databases/", a.requireAPIAuth(a.withExtendedDatabaseRoutes(a.handleDatabases)))
	a.mux.HandleFunc("/api/v1/team", a.requireAPIAuth(a.handleCurrentTeam))
	a.mux.HandleFunc("/api/v1/team/", a.requireAPIAuth(a.handleCurrentTeam))
	a.mux.HandleFunc("/api/v1/teams", a.requireAPIAuth(a.handleTeams))
	a.mux.HandleFunc("/api/v1/teams/", a.requireAPIAuth(a.handleTeams))
	a.mux.HandleFunc("/api/v1/audit", a.requireAPIAuth(a.handleAudit))
	a.mux.HandleFunc("/api/v1/audit-events", a.requireAPIAuth(a.handleAudit))
	a.mux.HandleFunc("/api/v1/search", a.requireAPIAuth(a.handleSearch))
	a.mux.HandleFunc("/api/v1/notifications", a.requireAPIAuth(a.handleNotifications))
	a.mux.HandleFunc("/api/v1/notifications/", a.requireAPIAuth(a.handleNotifications))
	a.mux.HandleFunc("/api/v1/notifications/test", a.requireAPIAuth(a.handleNotificationTest))
	a.mux.HandleFunc("/api/v1/s3-storages", a.requireAPIAuth(a.handleS3Storages))
	a.mux.HandleFunc("/api/v1/s3-storages/", a.requireAPIAuth(a.withExtendedS3Routes(a.handleS3Storages)))
	a.mux.HandleFunc("/api/v1/tags", a.requireAPIAuth(a.handleTags))
	a.mux.HandleFunc("/api/v1/tags/", a.requireAPIAuth(a.withExtendedTagRoutes(a.handleTags)))
	a.mux.HandleFunc("/api/v1/settings", a.requireAPIAuth(a.handleSettings))
	a.registerExtendedRoutes()

	// Webhooks (real HMAC/token signature verification + Postgres replay protection)
	a.mux.HandleFunc("/webhooks/", a.withExtendedWebhookRoutes(a.handleWebhook))

	// MCP endpoint (works independently of REST is_api_enabled per SECURITY.md)
	a.mux.HandleFunc("/mcp", a.handleMCP)

	// Realtime SSE streams (real auth + durable replay + live grant revocation)
	a.mux.HandleFunc("/realtime/logs", a.requireAPIAuth(a.handleRealtimeLogs))
	a.mux.HandleFunc("/realtime/events", a.requireAPIAuth(a.handleRealtimeEvents))

	// Static Astro + Vue UI shell (strict allowlist; /login serves login/index.html)
	if staticFS != nil {
		a.uiShell = web.ServeUIShell(web.DefaultUIAllowlist(), staticFS)
		a.mux.Handle("/", a.uiShell)
	}
}

type authedHandler func(w http.ResponseWriter, r *http.Request, p auth.Principal)

func allowDuringForceReset(path string) bool {
	return path == "/app/auth/logout" || path == "/app/auth/me" || path == "/app/auth/password/force-reset" || path == "/api/health"
}

func (a *App) requireSession(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, err := a.Authenticator.AuthenticateSession(r.Context(), r)
		if err != nil {
			writeAuthError(w, err)
			return
		}
		if p.ForceReset && !allowDuringForceReset(r.URL.Path) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"message":              "Password reset required before continuing.",
				"force_password_reset": true,
			})
			return
		}
		next(w, r, p)
	}
}

func (a *App) requireAPIAuth(next authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if authHeader := strings.TrimSpace(r.Header.Get("Authorization")); authHeader != "" {
			p, err := a.Authenticator.AuthenticateBearer(r.Context(), authHeader)
			if err != nil {
				writeAuthError(w, err)
				return
			}
			next(w, r, p)
			return
		}
		p, err := a.Authenticator.AuthenticateSession(r.Context(), r)
		if err != nil {
			writeAuthError(w, err)
			return
		}
		if p.ForceReset && !allowDuringForceReset(r.URL.Path) {
			writeJSON(w, http.StatusForbidden, map[string]any{
				"message":              "Password reset required before continuing.",
				"force_password_reset": true,
			})
			return
		}
		next(w, r, p)
	}
}

func writeAuthError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrCSRFMismatch):
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "CSRF token mismatch."})
	case errors.Is(err, auth.ErrAPIDisabled):
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "API access is disabled."})
	default:
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Unauthenticated."})
	}
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := a.Pool.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "degraded", "error": "postgres unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "runtime": "go-standalone"})
}

func (a *App) handleVersion(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": "5.0.0-standalone", "runtime": "go-standalone"})
}

// ---- Auth Handlers ----

func (a *App) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
		Code     string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid login payload."})
		return
	}
	user, defaultTeam, err := a.AuthRepo.AuthenticateCredentials(r.Context(), body.Email, body.Password, body.Code)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidTOTP) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid or missing 2FA code."})
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Invalid email or password."})
		return
	}
	sess, err := a.Authenticator.IssueSession(r.Context(), user.ID, defaultTeam, 24*time.Hour)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "Failed to issue session."})
		return
	}
	setSessionCookies(w, sess)
	a.TeamSvc.RecordAudit(r.Context(), &user.ID, &defaultTeam, "auth.login", "user", strconv.FormatInt(user.ID, 10), "info", nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":              user.ID,
		"email":                user.Email,
		"name":                 user.Name,
		"session_team_id":      sess.TeamID,
		"csrf_token":           sess.CSRFToken,
		"force_password_reset": user.ForcePasswordReset,
	})
}

func (a *App) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	var regEnabled bool
	_ = a.Pool.QueryRow(r.Context(), `SELECT COALESCE(is_registration_enabled, TRUE) FROM instance_settings WHERE id = 0`).Scan(&regEnabled)
	if !regEnabled {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "Registration is disabled."})
		return
	}
	var body struct {
		Name     string `json:"name"`
		Email    string `json:"email"`
		Password string `json:"password"`
		TeamName string `json:"team_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" || len(body.Password) < 8 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Valid email and password (>= 8 chars) required."})
		return
	}
	u, teamID, err := a.AuthRepo.CreateUserWithTeam(r.Context(), body.Name, body.Email, body.Password, body.TeamName)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
		return
	}
	sess, err := a.Authenticator.IssueSession(r.Context(), u.ID, teamID, 24*time.Hour)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "Failed to issue session."})
		return
	}
	setSessionCookies(w, sess)
	writeJSON(w, http.StatusCreated, map[string]any{
		"user_id":         u.ID,
		"email":           u.Email,
		"session_team_id": teamID,
		"csrf_token":      sess.CSRFToken,
	})
}

func (a *App) handleLogout(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	if sess, err := a.Authenticator.GetSession(r.Context(), r); err == nil {
		_ = a.Authenticator.RevokeSession(r.Context(), sess.ID)
	}
	clearSessionCookies(w)
	a.TeamSvc.RecordAudit(r.Context(), &p.UserID, &p.SessionTeamID, "auth.logout", "user", strconv.FormatInt(p.UserID, 10), "info", nil)
	writeJSON(w, http.StatusOK, map[string]string{"message": "Logged out."})
}

func (a *App) handleMe(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	sess, _ := a.Authenticator.GetSession(r.Context(), r)
	userTeams, _ := a.TeamSvc.ListUserTeams(r.Context(), p.UserID)
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id":              p.UserID,
		"email":                p.Email,
		"session_team_id":      p.SessionTeamID,
		"is_instance_admin":    p.IsInstanceAdmin,
		"force_password_reset": p.ForceReset,
		"csrf_token":           sess.CSRFToken,
		"teams":                userTeams,
	})
}

func (a *App) handleForcePasswordReset(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	var body struct {
		CurrentPassword      string `json:"current_password"`
		Password             string `json:"password"`
		PasswordConfirmation string `json:"password_confirmation"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Password != body.PasswordConfirmation || len(body.Password) < 8 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Matching password (>= 8 chars) required."})
		return
	}
	if err := a.AuthRepo.ForceResetPassword(r.Context(), p.UserID, body.CurrentPassword, body.Password); err != nil {
		if errors.Is(err, auth.ErrUnauthenticated) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Current password is invalid."})
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
		return
	}
	a.TeamSvc.RecordAudit(r.Context(), &p.UserID, &p.SessionTeamID, "auth.password_reset", "user", strconv.FormatInt(p.UserID, 10), "info", nil)
	writeJSON(w, http.StatusOK, map[string]any{"force_password_reset": false, "message": "Password updated."})
}

func (a *App) handleTwoFactorEnable(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	secret, codes, err := a.AuthRepo.EnableTwoFactor(r.Context(), p.UserID)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
		return
	}
	otpauthURL := fmt.Sprintf("otpauth://totp/Goolify:%s?secret=%s&issuer=Goolify", p.Email, secret)
	writeJSON(w, http.StatusOK, map[string]any{
		"secret":         secret,
		"otpauth_url":    otpauthURL,
		"recovery_codes": codes,
	})
}

func (a *App) handleTwoFactorConfirm(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Code) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "TOTP code is required."})
		return
	}
	if err := a.AuthRepo.ConfirmTwoFactor(r.Context(), p.UserID, body.Code); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid or replayed TOTP code."})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"confirmed": true})
}

func (a *App) handleTwoFactorDisable(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	if err := a.AuthRepo.DisableTwoFactor(r.Context(), p.UserID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"disabled": true})
}

func (a *App) handleSwitchTeam(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	var body struct {
		TeamID int64 `json:"team_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Invalid team_id."})
		return
	}
	if p.TeamRoles[body.TeamID] == auth.RoleNone {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "Not a member of requested team."})
		return
	}
	sess, err := a.Authenticator.GetSession(r.Context(), r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Unauthenticated."})
		return
	}
	updated, err := a.Authenticator.UpdateSessionTeam(r.Context(), sess.ID, body.TeamID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_team_id": updated.TeamID})
}

func (a *App) handleAcceptInvitation(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "token is required."})
		return
	}
	teamID, role, err := a.AuthRepo.AcceptInvitation(r.Context(), body.Token, p.UserID, p.Email)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"team_id": teamID, "role": role})
}

func (a *App) handleTokens(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	teamID := p.SessionTeamID
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{
			Type: "team", ID: teamID, TeamID: &teamID, APISegment: "teams",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		list, err := a.AuthRepo.ListAPITokens(r.Context(), teamID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{
			Type: "team", ID: teamID, TeamID: &teamID, APISegment: "teams",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Name      string          `json:"name"`
			Abilities []string        `json:"abilities"`
			Scopes    json.RawMessage `json:"scopes,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "token name is required."})
			return
		}
		if len(body.Abilities) == 0 {
			body.Abilities = []string{"read"}
		}
		id, plain, err := a.AuthRepo.IssueAPIToken(r.Context(), p.UserID, teamID, body.Name, body.Abilities, body.Scopes, nil)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": id, "token": plain, "abilities": body.Abilities})
	case http.MethodDelete:
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{
			Type: "team", ID: teamID, TeamID: &teamID, APISegment: "teams",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		idStr := strings.Trim(strings.TrimPrefix(r.URL.Path, "/app/auth/tokens"), "/")
		tokID, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid token id"})
			return
		}
		if err := a.AuthRepo.RevokeAPIToken(r.Context(), tokID, teamID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Token revoked."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Servers + Transfer + Proxy ----

func (a *App) handleServers(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/servers"), "/")
	if trimmed == "import" && r.Method == http.MethodPost {
		var body struct {
			TargetTeamID *int64                      `json:"target_team_id"`
			Bundle       transfer.FullTransferBundle `json:"bundle"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid transfer bundle"})
			return
		}
		targetTeam := p.SessionTeamID
		if body.TargetTeamID != nil {
			targetTeam = *body.TargetTeamID
		}
		rec, err := a.TransferSvc.ImportServer(r.Context(), p, targetTeam, body.Bundle)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, rec)
		return
	}

	parts := strings.Split(trimmed, "/")
	if len(parts) == 2 && parts[1] == "transfer" && r.Method == http.MethodPost {
		var body struct {
			MarkTransferredAway bool   `json:"mark_transferred_away"`
			TargetURL           string `json:"target_url"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		bundle, err := a.TransferSvc.ExportServer(r.Context(), p, parts[0], body.MarkTransferredAway)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		if strings.TrimSpace(body.TargetURL) != "" {
			target := strings.TrimSpace(body.TargetURL)
			for i := range bundle.PostTransferChecklist {
				bundle.PostTransferChecklist[i].Target = strings.ReplaceAll(bundle.PostTransferChecklist[i].Target, "{target_url}", target)
				bundle.PostTransferChecklist[i].Message = strings.ReplaceAll(bundle.PostTransferChecklist[i].Message, "{target_url}", target)
			}
		}
		writeJSON(w, http.StatusOK, bundle)
		return
	}
	if len(parts) == 2 && parts[1] == "proxy" && r.Method == http.MethodGet {
		srv, err := a.ServerSvc.Get(r.Context(), p, parts[0])
		if err != nil {
			writeDomainError(w, err)
			return
		}
		proxyType := r.URL.Query().Get("type")
		if proxyType == "" {
			proxyType = "traefik"
		}
		bundle, err := a.ProxyGen.Render(r.Context(), proxyType, []proxy.ProxyResource{
			{UUID: srv.UUID, FQDN: "https://" + srv.Name + ".local", Upstream: "http://127.0.0.1:80"},
		})
		if err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, bundle)
		return
	}
	if len(parts) == 2 && parts[1] == "resources" && r.Method == http.MethodGet {
		srv, err := a.ServerSvc.Get(r.Context(), p, parts[0])
		if err != nil {
			writeDomainError(w, err)
			return
		}
		resList := make([]map[string]any, 0)
		rows, err := a.Pool.Query(r.Context(), `
			SELECT a.uuid, a.name, 'application', a.status FROM applications a
			JOIN standalone_dockers sd ON sd.id = a.destination_id
			WHERE sd.server_id = $1
			UNION ALL
			SELECT s.uuid, s.name, 'service', s.status FROM services s WHERE s.server_id = $1
		`, srv.ID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var u, n, t, st string
				if rows.Scan(&u, &n, &t, &st) == nil {
					resList = append(resList, map[string]any{"uuid": u, "name": n, "type": t, "status": st})
				}
			}
		}
		writeJSON(w, http.StatusOK, resList)
		return
	}
	if len(parts) == 2 && parts[1] == "domains" && r.Method == http.MethodGet {
		srv, err := a.ServerSvc.Get(r.Context(), p, parts[0])
		if err != nil {
			writeDomainError(w, err)
			return
		}
		domains := make([]string, 0)
		rows, err := a.Pool.Query(r.Context(), `
			SELECT COALESCE(a.fqdn,'') FROM applications a
			JOIN standalone_dockers sd ON sd.id = a.destination_id
			WHERE sd.server_id = $1 AND a.fqdn IS NOT NULL AND a.fqdn <> ''
		`, srv.ID)
		if err == nil {
			defer rows.Close()
			for rows.Next() {
				var d string
				if rows.Scan(&d) == nil && d != "" {
					domains = append(domains, d)
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"ip": srv.IP, "domains": domains})
		return
	}
	if len(parts) == 2 && parts[1] == "destinations" {
		srv, err := a.ServerSvc.Get(r.Context(), p, parts[0])
		if err != nil {
			writeDomainError(w, err)
			return
		}
		if r.Method == http.MethodPost {
			a.handleServerCreateDestination(w, r, p, srv)
			return
		}
		if r.Method == http.MethodGet {
			rows, err := a.Pool.Query(r.Context(), `SELECT id, uuid, name, network, server_id FROM standalone_dockers WHERE server_id = $1 ORDER BY id ASC`, srv.ID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			defer rows.Close()
			out := make([]map[string]any, 0)
			for rows.Next() {
				var id, sid int64
				var u, n, netw string
				if rows.Scan(&id, &u, &n, &netw, &sid) == nil {
					out = append(out, map[string]any{"id": id, "uuid": u, "name": n, "network": netw, "server_id": sid})
				}
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	if len(parts) >= 2 && parts[1] == "envs" {
		srv, err := a.ServerSvc.Get(r.Context(), p, parts[0])
		if err != nil {
			writeDomainError(w, err)
			return
		}
		envIDSeg := ""
		if len(parts) >= 3 {
			envIDSeg = parts[2]
		}
		sid := srv.ID
		a.handleScopedSharedEnvs(w, r, p, "server", srv.TeamID, nil, &sid, envIDSeg)
		return
	}
	if len(parts) == 2 && parts[1] == "validate" && (r.Method == http.MethodGet || r.Method == http.MethodPost) {
		srv, err := a.ServerSvc.Get(r.Context(), p, parts[0])
		if err != nil {
			writeDomainError(w, err)
			return
		}
		if srv.IsTransferredAway() {
			writeDomainError(w, servers.ErrTransferredServer)
			return
		}
		teamID := srv.TeamID
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{
			Type: "server", ID: srv.ID, UUID: srv.UUID, TeamID: &teamID, APISegment: "servers",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		rec, err := a.enqueueResourceLifecycleJob(r.Context(), "server", srv.UUID, srv.UUID, srv.TeamID, "server_validate")
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, rec)
		return
	}

	// Standard /api/v1/servers CRUD (with Read check on list!)
	if trimmed == "" && r.Method == http.MethodGet {
		teamID := activeTeamForPrincipal(p)
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
			Type: "server", TeamID: &teamID, APISegment: "servers",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		list, err := a.ServerRepo.ListByTeam(r.Context(), teamID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, list)
		return
	}

	h := servers.NewHTTPHandler(a.ServerSvc, a.ServerRepo)
	h.ServeServers(w, r, p)
	// Flush outbox asynchronously/best-effort after mutation
	if r.Method != http.MethodGet {
		_, _ = a.Dispatcher.FlushPending(r.Context())
	}
}

func (a *App) handlePrivateKeys(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/security/keys"), "/")
	if trimmed != "" {
		a.handlePrivateKeyByUUID(w, r, p, strings.Split(trimmed, "/")[0])
		return
	}
	teamID := activeTeamForPrincipal(p)
	switch r.Method {
	case http.MethodGet:
		if err := a.Authz.Check(r.Context(), p, access.ActionUseTeamCredentials, access.ResourceRef{
			Type: "private_key", TeamID: &teamID, APISegment: "security",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		list, err := a.ServerRepo.ListPrivateKeys(r.Context(), teamID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, list)
	case http.MethodPost:
		if err := a.Authz.Check(r.Context(), p, access.ActionCreate, access.ResourceRef{
			Type: "private_key", TeamID: &teamID, APISegment: "security",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			PrivateKey  string `json:"private_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Name) == "" || strings.TrimSpace(body.PrivateKey) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "name and private_key are required."})
			return
		}
		stored := body.PrivateKey
		if a.Cipher != nil {
			enc, err := a.Cipher.EncryptLaravelPayload([]byte(body.PrivateKey), false)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			stored = enc
		}
		uuid := a.Hooks.NewCUID2()
		pk, err := a.ServerRepo.CreatePrivateKey(r.Context(), uuid, compat.SanitizeString(body.Name), compat.SanitizeString(body.Description), stored, "", teamID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"id": pk.ID, "uuid": pk.UUID, "team_id": pk.TeamID})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Projects, Environments, Shared Variables ----

func (a *App) handleProjects(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/projects"), "/")
	if trimmed == "" {
		switch r.Method {
		case http.MethodGet:
			list, err := a.ProjectSvc.ListProjects(r.Context(), p, activeTeamForPrincipal(p))
			if err != nil {
				writeDomainError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, list)
		case http.MethodPost:
			var in projects.CreateProjectInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid project payload"})
				return
			}
			if in.TeamID == 0 {
				in.TeamID = activeTeamForPrincipal(p)
			}
			proj, env, err := a.ProjectSvc.CreateProject(r.Context(), p, in)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			_, _ = a.Dispatcher.FlushPending(r.Context())
			writeJSON(w, http.StatusCreated, map[string]any{"project": proj, "default_environment": env})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}

	parts := strings.Split(trimmed, "/")
	projUUID := parts[0]
	if len(parts) >= 2 && parts[1] == "envs" {
		proj, _, err := a.ProjectSvc.GetProject(r.Context(), p, projUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		envIDSeg := ""
		if len(parts) >= 3 {
			envIDSeg = parts[2]
		}
		pid := proj.ID
		a.handleScopedSharedEnvs(w, r, p, "project", proj.TeamID, &pid, nil, envIDSeg)
		return
	}
	if len(parts) == 2 && parts[1] == "environments" {
		switch r.Method {
		case http.MethodGet:
			_, envs, err := a.ProjectSvc.GetProject(r.Context(), p, projUUID)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, envs)
		case http.MethodPost:
			var body struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid environment payload"})
				return
			}
			env, err := a.ProjectSvc.CreateEnvironment(r.Context(), p, projUUID, body.Name, body.Description)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			_, _ = a.Dispatcher.FlushPending(r.Context())
			writeJSON(w, http.StatusCreated, env)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}
	if len(parts) == 3 && parts[1] == "environments" {
		proj, envs, err := a.ProjectSvc.GetProject(r.Context(), p, projUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		target := parts[2]
		var found *projects.Environment
		for i := range envs {
			if envs[i].UUID == target || envs[i].Name == target {
				found = &envs[i]
				break
			}
		}
		if found == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "environment not found"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, found)
		case http.MethodDelete:
			tid := proj.TeamID
			if err := a.Authz.Check(r.Context(), p, access.ActionDelete, access.ResourceRef{
				Type: "project", ID: proj.ID, UUID: proj.UUID, ProjectUUID: proj.UUID, TeamID: &tid, APISegment: "projects",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			_, _ = a.Pool.Exec(r.Context(), `DELETE FROM environments WHERE id = $1`, found.ID)
			writeJSON(w, http.StatusOK, map[string]string{"message": "Environment deleted."})
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		proj, envs, err := a.ProjectSvc.GetProject(r.Context(), p, projUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"project": proj, "environments": envs})
	case http.MethodPatch:
		proj, _, err := a.ProjectSvc.GetProject(r.Context(), p, projUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		tid := proj.TeamID
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{
			Type: "project", ID: proj.ID, UUID: proj.UUID, ProjectUUID: proj.UUID, TeamID: &tid, APISegment: "projects",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Name        *string `json:"name"`
			Description *string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid project update"})
			return
		}
		if body.Name != nil && strings.TrimSpace(*body.Name) != "" {
			proj.Name = compat.SanitizeString(*body.Name)
		}
		if body.Description != nil {
			proj.Description = compat.SanitizeString(*body.Description)
		}
		_, _ = a.Pool.Exec(r.Context(), `UPDATE projects SET name = $1, description = $2, updated_at = NOW() WHERE id = $3`, proj.Name, proj.Description, proj.ID)
		writeJSON(w, http.StatusOK, proj)
	case http.MethodDelete:
		if err := a.ProjectSvc.DeleteProject(r.Context(), p, projUUID); err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Dispatcher.FlushPending(r.Context())
		writeJSON(w, http.StatusOK, map[string]string{"message": "Project deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (a *App) handleSharedVariables(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	var in projects.SharedVariable
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid shared variable"})
		return
	}
	if in.TeamID == 0 {
		in.TeamID = activeTeamForPrincipal(p)
	}
	saved, err := a.ProjectSvc.UpsertSharedVariable(r.Context(), p, in)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, saved)
}

// ---- Applications & Deployments ----

func (a *App) handleApplications(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/applications"), "/")
	if trimmed == "" {
		switch r.Method {
		case http.MethodGet:
			list, err := a.AppSvc.ListByTeam(r.Context(), p, activeTeamForPrincipal(p))
			if err != nil {
				writeDomainError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, list)
		case http.MethodPost:
			var in applications.CreateApplicationInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid application payload"})
				return
			}
			appRec, err := a.AppSvc.Create(r.Context(), p, in)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			_, _ = a.Dispatcher.FlushPending(r.Context())
			writeJSON(w, http.StatusCreated, appRec)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) == 1 && r.Method == http.MethodPost {
		if bp, ok := typedAppBuildPack(parts[0]); ok {
			var in applications.CreateApplicationInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid application payload"})
				return
			}
			if in.BuildPack == "" {
				in.BuildPack = bp
			}
			appRec, err := a.AppSvc.Create(r.Context(), p, in)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			_, _ = a.Dispatcher.FlushPending(r.Context())
			writeJSON(w, http.StatusCreated, appRec)
			return
		}
	}
	appUUID := parts[0]
	if len(parts) == 2 && parts[1] == "clone" {
		a.handleApplicationClone(w, r, p, appUUID)
		return
	}
	if len(parts) >= 2 && parts[1] == "storages" {
		appRec, err := a.AppSvc.GetByUUID(r.Context(), p, appUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		stUUID := ""
		if len(parts) >= 3 {
			stUUID = parts[2]
		}
		a.handleResourceStorages(w, r, p, `App\Models\Application`, "application", appRec.ID, appRec.UUID, appRec.TeamID, stUUID)
		return
	}
	if len(parts) >= 2 && parts[1] == "previews" {
		prSeg := ""
		if len(parts) >= 3 {
			prSeg = parts[2]
		}
		a.handleApplicationPreviews(w, r, p, appUUID, prSeg)
		return
	}
	if len(parts) == 3 && parts[1] == "envs" && parts[2] == "bulk" {
		a.handleApplicationBulkEnvs(w, r, p, appUUID)
		return
	}
	if len(parts) == 3 && parts[1] == "envs" && r.Method == http.MethodDelete {
		appRec, err := a.AppSvc.GetByUUID(r.Context(), p, appUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		tid := appRec.TeamID
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{
			Type: "application", ID: appRec.ID, UUID: appRec.UUID, ProjectUUID: appRec.ProjectUUID, EnvironmentUUID: appRec.EnvironmentUUID, ServerID: &appRec.ServerID, TeamID: &tid, APISegment: "applications",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM environment_variables WHERE uuid = $1 AND resourceable_id = $2`, parts[2], appRec.ID)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Environment variable deleted."})
		return
	}
	if len(parts) == 2 && parts[1] == "envs" {
		switch r.Method {
		case http.MethodGet:
			list, err := a.AppSvc.ListEnvVars(r.Context(), p, appUUID)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, list)
		case http.MethodPost, http.MethodPatch:
			var in applications.EnvVarInput
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid env payload"})
				return
			}
			ev, err := a.AppSvc.SetEnvVar(r.Context(), p, appUUID, in)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, ev)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}
	if len(parts) == 2 && parts[1] == "logs" && r.Method == http.MethodGet {
		appRec, err := a.AppSvc.GetByUUID(r.Context(), p, appUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		entries, _, err := a.LogStore.ReplayFrom(r.Context(), appRec.UUID, 0, 200)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		lines := make([]string, 0, len(entries))
		for _, e := range entries {
			lines = append(lines, e.Text)
		}
		writeJSON(w, http.StatusOK, map[string]any{"logs": strings.Join(lines, "\n"), "entries": entries})
		return
	}
	if len(parts) == 2 && (parts[1] == "start" || parts[1] == "restart" || parts[1] == "stop") && (r.Method == http.MethodPost || r.Method == http.MethodGet) {
		appRec, err := a.AppSvc.GetByUUID(r.Context(), p, appUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		tid := appRec.TeamID
		if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, access.ResourceRef{
			Type: "application", ID: appRec.ID, UUID: appRec.UUID, ProjectUUID: appRec.ProjectUUID, EnvironmentUUID: appRec.EnvironmentUUID, ServerID: &appRec.ServerID, TeamID: &tid, APISegment: "applications",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		if parts[1] == "stop" {
			rec, err := a.enqueueResourceLifecycleJob(r.Context(), "application", appRec.UUID, appRec.ServerUUID, appRec.TeamID, "app_stop")
			if err != nil {
				writeDomainError(w, err)
				return
			}
			writeJSON(w, http.StatusAccepted, rec)
			return
		}
		rec, err := a.AppSvc.EnqueueDeployment(r.Context(), p, appRec.UUID, parts[1] == "restart", false, p.AuthMethod == "bearer")
		if err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Dispatcher.FlushPending(r.Context())
		writeJSON(w, http.StatusAccepted, rec)
		return
	}
	switch r.Method {
	case http.MethodGet:
		appRec, err := a.AppSvc.GetByUUID(r.Context(), p, appUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, appRec)
	case http.MethodPatch:
		appRec, err := a.AppSvc.GetByUUID(r.Context(), p, appUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		tid := appRec.TeamID
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{
			Type: "application", ID: appRec.ID, UUID: appRec.UUID, ProjectUUID: appRec.ProjectUUID, EnvironmentUUID: appRec.EnvironmentUUID, ServerID: &appRec.ServerID, TeamID: &tid, APISegment: "applications",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Name                    *string           `json:"name"`
			Description             *string           `json:"description"`
			FQDN                    *string           `json:"fqdn"`
			GitRepository           *string           `json:"git_repository"`
			GitBranch               *string           `json:"git_branch"`
			BuildPack               *string           `json:"build_pack"`
			DockerRegistryImageName *string           `json:"docker_registry_image_name"`
			DockerRegistryImageTag  *string           `json:"docker_registry_image_tag"`
			Dockerfile              *string           `json:"dockerfile"`
			DockerfileLocation      *string           `json:"dockerfile_location"`
			DockerComposeLocation   *string           `json:"docker_compose_location"`
			DockerComposeRaw        *string           `json:"docker_compose_raw"`
			PortsExposes            *string           `json:"ports_exposes"`
			BaseDirectory           *string           `json:"base_directory"`
			PublishDirectory        *string           `json:"publish_directory"`
			InstallCommand          *string           `json:"install_command"`
			BuildCommand            *string           `json:"build_command"`
			StartCommand            *string           `json:"start_command"`
			CustomLabels            *string           `json:"custom_labels"`
			CustomDockerRunOptions  *string           `json:"custom_docker_run_options"`
			PrivateKeyUUID          *string           `json:"private_key_uuid"`
			HealthCheckEnabled      *bool             `json:"health_check_enabled"`
			HealthCheckPath         *string           `json:"health_check_path"`
			HealthCheckPort         *string           `json:"health_check_port"`
			HealthCheckHost         *string           `json:"health_check_host"`
			HealthCheckMethod       *string           `json:"health_check_method"`
			HealthCheckReturnCode   *int              `json:"health_check_return_code"`
			HealthCheckScheme       *string           `json:"health_check_scheme"`
			HealthCheckResponseText *string           `json:"health_check_response_text"`
			HealthCheckInterval     *int              `json:"health_check_interval"`
			HealthCheckTimeout      *int              `json:"health_check_timeout"`
			HealthCheckRetries      *int              `json:"health_check_retries"`
			HealthCheckStartPeriod  *int              `json:"health_check_start_period"`
			LimitsMemory            *string           `json:"limits_memory"`
			LimitsMemorySwap        *string           `json:"limits_memory_swap"`
			LimitsMemorySwappiness  *int              `json:"limits_memory_swappiness"`
			LimitsMemoryReservation *string           `json:"limits_memory_reservation"`
			LimitsCPUs              *string           `json:"limits_cpus"`
			LimitsCPUSet            *string           `json:"limits_cpuset"`
			LimitsCPUShares         *int              `json:"limits_cpu_shares"`
			Settings                map[string]string `json:"settings"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid application update"})
			return
		}
		if body.Name != nil && strings.TrimSpace(*body.Name) != "" {
			appRec.Name = compat.SanitizeString(*body.Name)
		}
		if body.Description != nil {
			appRec.Description = compat.SanitizeString(*body.Description)
		}
		if body.FQDN != nil {
			appRec.FQDN = strings.TrimSpace(*body.FQDN)
		}
		if body.GitRepository != nil {
			appRec.GitRepository = strings.TrimSpace(*body.GitRepository)
		}
		if body.GitBranch != nil && strings.TrimSpace(*body.GitBranch) != "" {
			appRec.GitBranch = strings.TrimSpace(*body.GitBranch)
		}
		if body.BuildPack != nil && strings.TrimSpace(*body.BuildPack) != "" {
			appRec.BuildPack = strings.TrimSpace(*body.BuildPack)
		}
		if body.DockerRegistryImageName != nil {
			appRec.DockerRegistryImageName = strings.TrimSpace(*body.DockerRegistryImageName)
		}
		if body.DockerRegistryImageTag != nil {
			appRec.DockerRegistryImageTag = strings.TrimSpace(*body.DockerRegistryImageTag)
		}
		if body.Dockerfile != nil {
			appRec.Dockerfile = *body.Dockerfile
		}
		if body.DockerfileLocation != nil {
			appRec.DockerfileLocation = strings.TrimSpace(*body.DockerfileLocation)
		}
		if body.DockerComposeLocation != nil {
			appRec.DockerComposeLocation = strings.TrimSpace(*body.DockerComposeLocation)
		}
		if body.DockerComposeRaw != nil {
			appRec.DockerComposeRaw = *body.DockerComposeRaw
		}
		if body.PortsExposes != nil {
			appRec.PortsExposes = strings.TrimSpace(*body.PortsExposes)
		}
		if body.BaseDirectory != nil {
			appRec.BaseDirectory = strings.TrimSpace(*body.BaseDirectory)
		}
		if body.PublishDirectory != nil {
			appRec.PublishDirectory = strings.TrimSpace(*body.PublishDirectory)
		}
		if body.InstallCommand != nil {
			appRec.InstallCommand = strings.TrimSpace(*body.InstallCommand)
		}
		if body.BuildCommand != nil {
			appRec.BuildCommand = strings.TrimSpace(*body.BuildCommand)
		}
		if body.StartCommand != nil {
			appRec.StartCommand = strings.TrimSpace(*body.StartCommand)
		}
		if body.CustomLabels != nil {
			appRec.CustomLabels = *body.CustomLabels
		}
		if body.CustomDockerRunOptions != nil {
			appRec.CustomDockerRunOptions = strings.TrimSpace(*body.CustomDockerRunOptions)
		}
		if body.PrivateKeyUUID != nil {
			pkU := strings.TrimSpace(*body.PrivateKeyUUID)
			if pkU == "" {
				appRec.PrivateKeyID = nil
			} else {
				var pkID, pkTeam int64
				if err := a.Pool.QueryRow(r.Context(), `SELECT id, team_id FROM private_keys WHERE uuid = $1`, pkU).Scan(&pkID, &pkTeam); err != nil {
					writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "private key not found"})
					return
				}
				if pkTeam != appRec.TeamID {
					writeJSON(w, http.StatusForbidden, map[string]string{"message": "private key belongs to another team"})
					return
				}
				appRec.PrivateKeyID = &pkID
			}
		}
		if body.HealthCheckEnabled != nil {
			appRec.HealthCheckEnabled = *body.HealthCheckEnabled
		}
		if body.HealthCheckPath != nil {
			appRec.HealthCheckPath = strings.TrimSpace(*body.HealthCheckPath)
		}
		if body.HealthCheckPort != nil {
			appRec.HealthCheckPort = strings.TrimSpace(*body.HealthCheckPort)
		}
		if body.HealthCheckHost != nil {
			appRec.HealthCheckHost = strings.TrimSpace(*body.HealthCheckHost)
		}
		if body.HealthCheckMethod != nil {
			appRec.HealthCheckMethod = strings.TrimSpace(*body.HealthCheckMethod)
		}
		if body.HealthCheckReturnCode != nil {
			appRec.HealthCheckReturnCode = *body.HealthCheckReturnCode
		}
		if body.HealthCheckScheme != nil {
			appRec.HealthCheckScheme = strings.TrimSpace(*body.HealthCheckScheme)
		}
		if body.HealthCheckResponseText != nil {
			appRec.HealthCheckResponseText = *body.HealthCheckResponseText
		}
		if body.HealthCheckInterval != nil {
			appRec.HealthCheckInterval = *body.HealthCheckInterval
		}
		if body.HealthCheckTimeout != nil {
			appRec.HealthCheckTimeout = *body.HealthCheckTimeout
		}
		if body.HealthCheckRetries != nil {
			appRec.HealthCheckRetries = *body.HealthCheckRetries
		}
		if body.HealthCheckStartPeriod != nil {
			appRec.HealthCheckStartPeriod = *body.HealthCheckStartPeriod
		}
		if body.LimitsMemory != nil {
			appRec.LimitsMemory = strings.TrimSpace(*body.LimitsMemory)
		}
		if body.LimitsMemorySwap != nil {
			appRec.LimitsMemorySwap = strings.TrimSpace(*body.LimitsMemorySwap)
		}
		if body.LimitsMemorySwappiness != nil {
			appRec.LimitsMemorySwappiness = *body.LimitsMemorySwappiness
		}
		if body.LimitsMemoryReservation != nil {
			appRec.LimitsMemoryReservation = strings.TrimSpace(*body.LimitsMemoryReservation)
		}
		if body.LimitsCPUs != nil {
			appRec.LimitsCPUs = strings.TrimSpace(*body.LimitsCPUs)
		}
		if body.LimitsCPUSet != nil {
			appRec.LimitsCPUSet = strings.TrimSpace(*body.LimitsCPUSet)
		}
		if body.LimitsCPUShares != nil {
			appRec.LimitsCPUShares = *body.LimitsCPUShares
		}
		_, _ = a.Pool.Exec(r.Context(), `
			UPDATE applications SET
				name = $1, description = $2, fqdn = $3, git_repository = $4, git_branch = $5, build_pack = $6,
				docker_registry_image_name = $7, docker_registry_image_tag = $8, dockerfile = $9,
				dockerfile_location = $10, docker_compose_location = $11, docker_compose_raw = $12,
				ports_exposes = $13, base_directory = $14, publish_directory = $15,
				install_command = $16, build_command = $17, start_command = $18,
				custom_labels = $19, custom_docker_run_options = $20, private_key_id = $21,
				health_check_enabled = $22, health_check_path = $23, health_check_port = $24,
				health_check_host = $25, health_check_method = $26, health_check_return_code = $27,
				health_check_scheme = $28, health_check_response_text = $29, health_check_interval = $30,
				health_check_timeout = $31, health_check_retries = $32, health_check_start_period = $33,
				limits_memory = $34, limits_memory_swap = $35, limits_memory_swappiness = $36,
				limits_memory_reservation = $37, limits_cpus = $38, limits_cpuset = $39, limits_cpu_shares = $40,
				updated_at = NOW()
			WHERE id = $41
		`,
			appRec.Name, appRec.Description, appRec.FQDN, appRec.GitRepository, appRec.GitBranch, appRec.BuildPack,
			appRec.DockerRegistryImageName, appRec.DockerRegistryImageTag, appRec.Dockerfile,
			appRec.DockerfileLocation, appRec.DockerComposeLocation, appRec.DockerComposeRaw,
			appRec.PortsExposes, appRec.BaseDirectory, appRec.PublishDirectory,
			appRec.InstallCommand, appRec.BuildCommand, appRec.StartCommand,
			appRec.CustomLabels, appRec.CustomDockerRunOptions, appRec.PrivateKeyID,
			appRec.HealthCheckEnabled, appRec.HealthCheckPath, appRec.HealthCheckPort,
			appRec.HealthCheckHost, appRec.HealthCheckMethod, appRec.HealthCheckReturnCode,
			appRec.HealthCheckScheme, appRec.HealthCheckResponseText, appRec.HealthCheckInterval,
			appRec.HealthCheckTimeout, appRec.HealthCheckRetries, appRec.HealthCheckStartPeriod,
			appRec.LimitsMemory, appRec.LimitsMemorySwap, appRec.LimitsMemorySwappiness,
			appRec.LimitsMemoryReservation, appRec.LimitsCPUs, appRec.LimitsCPUSet, appRec.LimitsCPUShares,
			appRec.ID,
		)
		if len(body.Settings) > 0 {
			if err := a.AppSvc.UpdateSettings(r.Context(), appRec.ID, body.Settings); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
				return
			}
		}
		appRec, _ = a.AppSvc.GetByUUID(r.Context(), p, appUUID)
		writeJSON(w, http.StatusOK, appRec)
	case http.MethodDelete:
		appRec, err := a.AppSvc.GetByUUID(r.Context(), p, appUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		tid := appRec.TeamID
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, access.ResourceRef{
			Type: "application", ID: appRec.ID, UUID: appRec.UUID, ProjectUUID: appRec.ProjectUUID, EnvironmentUUID: appRec.EnvironmentUUID, ServerID: &appRec.ServerID, TeamID: &tid, APISegment: "applications",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM applications WHERE id = $1`, appRec.ID)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Application deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

func (a *App) handleDeploy(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	appUUID := strings.TrimSpace(r.URL.Query().Get("uuid"))
	force := r.URL.Query().Get("force") == "true"
	if appUUID == "" && r.Method == http.MethodPost {
		var body struct {
			UUID  string `json:"uuid"`
			Force bool   `json:"force"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		appUUID = body.UUID
		force = body.Force
	}
	if appUUID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "uuid is required."})
		return
	}
	rec, err := a.AppSvc.EnqueueDeployment(r.Context(), p, appUUID, force, false, p.AuthMethod == "bearer")
	if err != nil {
		writeDomainError(w, err)
		return
	}
	_, _ = a.Dispatcher.FlushPending(r.Context())
	writeJSON(w, http.StatusAccepted, rec)
}

func (a *App) handleDeployments(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/deployments"), "/")
	if trimmed == "" && r.Method == http.MethodGet {
		teamID := activeTeamForPrincipal(p)
		if err := a.Authz.CheckList(r.Context(), p, teamID, "deployments"); err != nil {
			writeDomainError(w, err)
			return
		}
		rows, err := a.Pool.Query(r.Context(), `
			SELECT q.deployment_uuid, a.id, a.uuid, e.uuid, p.uuid, p.team_id, q.status, q.created_at, q.updated_at
			FROM application_deployment_queues q
			JOIN applications a ON a.id = q.application_id
			JOIN environments e ON e.id = a.environment_id
			JOIN projects p ON p.id = e.project_id
			WHERE p.team_id = $1
			ORDER BY q.created_at DESC
			LIMIT 50
		`, teamID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		defer rows.Close()
		out := make([]map[string]any, 0)
		for rows.Next() {
			var du, au, eu, pu, st string
			var appID, rowTeamID int64
			var ca, ua time.Time
			if rows.Scan(&du, &appID, &au, &eu, &pu, &rowTeamID, &st, &ca, &ua) == nil {
				if a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
					Type: "application", ID: appID, UUID: au, ProjectUUID: pu, EnvironmentUUID: eu, TeamID: &rowTeamID, APISegment: "deployments",
				}) != nil {
					continue
				}
				out = append(out, map[string]any{"deployment_uuid": du, "application_uuid": au, "status": st, "created_at": ca, "updated_at": ua})
			}
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) == 2 && parts[0] == "applications" && r.Method == http.MethodGet {
		list, err := a.AppSvc.ListDeploymentsByApplication(r.Context(), p, parts[1])
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	if len(parts) == 2 && parts[1] == "cancel" && r.Method == http.MethodPost {
		depUUID := parts[0]
		opID := depUUID
		if !strings.HasPrefix(opID, "deploy-") {
			opID = "deploy-" + depUUID
		}
		spec, found, err := a.JobCoord.Get(r.Context(), opID)
		if err != nil || !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "deployment not found"})
			return
		}
		tid := spec.TeamID
		ref := access.ResourceRef{Type: "application", UUID: spec.ResourceUUID, TeamID: &tid, APISegment: "deployments"}
		if rt, rType, rID, pUUID, eUUID, rerr := a.resolveResourceIdentity(r.Context(), spec.ResourceUUID); rerr == nil {
			ref = access.ResourceRef{
				Type: rType, ID: rID, UUID: spec.ResourceUUID, ProjectUUID: pUUID, EnvironmentUUID: eUUID,
				TeamID: &rt, APISegment: "deployments",
			}
		}
		if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		// Only mark queued jobs immediately cancelled; running jobs require worker/remote verification
		if spec.Status == jobs.StatusQueued {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE go_job_operations SET status = 'cancelled', updated_at = NOW() WHERE operation_id = $1 AND status = 'queued'`, opID)
			_, _ = a.Pool.Exec(r.Context(), `UPDATE application_deployment_queues SET status = 'cancelled', updated_at = NOW() WHERE deployment_uuid = $1 AND status = 'queued'`, depUUID)
			writeJSON(w, http.StatusOK, map[string]any{"deployment_uuid": depUUID, "status": "cancelled"})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"deployment_uuid": depUUID,
			"status":          string(spec.Status),
			"message":         "Cancellation requested; terminal state awaits remote process verification.",
		})
		return
	}
	if len(parts) == 1 && parts[0] != "" && r.Method == http.MethodGet {
		opID := parts[0]
		if !strings.HasPrefix(opID, "deploy-") {
			opID = "deploy-" + opID
		}
		spec, found, err := a.JobCoord.Get(r.Context(), opID)
		if err != nil || !found {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "deployment not found"})
			return
		}
		teamID := spec.TeamID
		ref := access.ResourceRef{Type: "application", UUID: spec.ResourceUUID, TeamID: &teamID, APISegment: "deployments"}
		if rt, rType, rID, pUUID, eUUID, rerr := a.resolveResourceIdentity(r.Context(), spec.ResourceUUID); rerr == nil {
			ref = access.ResourceRef{
				Type: rType, ID: rID, UUID: spec.ResourceUUID, ProjectUUID: pUUID, EnvironmentUUID: eUUID,
				TeamID: &rt, APISegment: "deployments",
			}
		}
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, ref); err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, spec)
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

// ---- Services ----

func (a *App) handleServices(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/services"), "/")
	if trimmed == "" {
		switch r.Method {
		case http.MethodGet:
			list, err := a.ServiceSvc.ListByTeam(r.Context(), p, activeTeamForPrincipal(p))
			if err != nil {
				writeDomainError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, list)
		case http.MethodPost:
			var body struct {
				EnvironmentUUID  string `json:"environment_uuid"`
				ProjectUUID      string `json:"project_uuid"`
				ServerUUID       string `json:"server_uuid"`
				Type             string `json:"type"`
				Name             string `json:"name"`
				DockerComposeRaw string `json:"docker_compose_raw"`
				FQDN             string `json:"fqdn"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid service payload"})
				return
			}
			if strings.TrimSpace(body.EnvironmentUUID) == "" && strings.TrimSpace(body.ProjectUUID) != "" {
				teamID := activeTeamForPrincipal(p)
				_ = a.Pool.QueryRow(r.Context(), `
					SELECT e.uuid
					FROM environments e
					JOIN projects p ON p.id = e.project_id
					WHERE p.uuid = $1 AND p.team_id = $2
					ORDER BY e.id ASC
					LIMIT 1
				`, strings.TrimSpace(body.ProjectUUID), teamID).Scan(&body.EnvironmentUUID)
			}
			if strings.TrimSpace(body.DockerComposeRaw) == "" && strings.TrimSpace(body.Type) != "" {
				tpl, tplErr := services.LookupServiceTemplate(body.Type)
				if tplErr != nil {
					writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "unknown service template type: " + body.Type})
					return
				}
				body.DockerComposeRaw = tpl.ComposeRaw
				if strings.TrimSpace(body.Name) == "" {
					body.Name = tpl.Type
				}
			}
			body.DockerComposeRaw = services.DecodeComposeInput(body.DockerComposeRaw)
			svc, err := a.ServiceSvc.Create(r.Context(), p, body.EnvironmentUUID, body.ServerUUID, body.Name, body.DockerComposeRaw, body.FQDN)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			_, _ = a.Dispatcher.FlushPending(r.Context())
			writeJSON(w, http.StatusCreated, svc)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}
	parts := strings.Split(trimmed, "/")
	if parts[0] == "templates" && len(parts) == 1 {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
			return
		}
		if slug := strings.TrimSpace(r.URL.Query().Get("type")); slug != "" {
			tpl, err := services.LookupServiceTemplate(slug)
			if err != nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"message": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, tpl)
			return
		}
		list, err := services.ListServiceTemplates(r.URL.Query().Get("include_compose") == "true")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	svcUUID := parts[0]
	meta, metaErr := a.loadResourceMeta(r.Context(), "service", svcUUID)
	if metaErr != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
		return
	}
	serviceRef := meta.TeamScopedRef
	serviceRef.ServerID = &meta.ServerID

	if len(parts) >= 2 && parts[1] == "storages" {
		var sid, tid int64
		if err := a.Pool.QueryRow(r.Context(), `
			SELECT s.id, p.team_id FROM services s
			JOIN environments e ON e.id = s.environment_id
			JOIN projects p ON p.id = e.project_id
			WHERE s.uuid = $1
		`, svcUUID).Scan(&sid, &tid); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
			return
		}
		stUUID := ""
		if len(parts) >= 3 {
			stUUID = parts[2]
		}
		a.handleResourceStorages(w, r, p, `App\Models\Service`, "service", sid, svcUUID, tid, stUUID)
		return
	}
	if len(parts) == 2 && (parts[1] == "deploy" || parts[1] == "start" || parts[1] == "restart") && (r.Method == http.MethodPost || r.Method == http.MethodGet) {
		rec, err := a.ServiceSvc.EnqueueDeploy(r.Context(), p, svcUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Dispatcher.FlushPending(r.Context())
		writeJSON(w, http.StatusAccepted, rec)
		return
	}
	if len(parts) == 2 && parts[1] == "stop" && (r.Method == http.MethodPost || r.Method == http.MethodGet) {
		var sid, tid int64
		var srvUUID string
		err := a.Pool.QueryRow(r.Context(), `
			SELECT s.id, p.team_id, COALESCE(srv.uuid,'localhost')
			FROM services s
			JOIN environments e ON e.id = s.environment_id
			JOIN projects p ON p.id = e.project_id
			LEFT JOIN servers srv ON srv.id = s.server_id
			WHERE s.uuid = $1
		`, svcUUID).Scan(&sid, &tid, &srvUUID)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
			return
		}
		if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, serviceRef); err != nil {
			writeDomainError(w, err)
			return
		}
		rec, err := a.enqueueResourceLifecycleJob(r.Context(), "service", svcUUID, srvUUID, tid, "service_stop")
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, rec)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		svc, err := a.ServiceSvc.GetByUUID(r.Context(), p, svcUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, svc)
		return
	}

	if len(parts) == 1 && r.Method == http.MethodDelete {
		var sid, tid int64
		if err := a.Pool.QueryRow(r.Context(), `
			SELECT s.id, p.team_id FROM services s
			JOIN environments e ON e.id = s.environment_id
			JOIN projects p ON p.id = e.project_id
			WHERE s.uuid = $1
		`, svcUUID).Scan(&sid, &tid); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
			return
		}
		if err := a.Authz.Check(r.Context(), p, access.ActionDelete, serviceRef); err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Pool.Exec(r.Context(), `DELETE FROM services WHERE id = $1`, sid)
		writeJSON(w, http.StatusOK, map[string]string{"message": "Service deleted."})
		return
	}
	if r.Method == http.MethodPatch || r.Method == http.MethodPut {
		var body struct {
			Name             *string `json:"name"`
			Description      *string `json:"description"`
			DockerComposeRaw *string `json:"docker_compose_raw"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid service update payload"})
			return
		}
		var sid, tid int64
		var curName, curDesc string
		if err := a.Pool.QueryRow(r.Context(), `
			SELECT s.id, p.team_id, s.name, COALESCE(s.description,'')
			FROM services s
			JOIN environments e ON e.id = s.environment_id
			JOIN projects p ON p.id = e.project_id
			WHERE s.uuid = $1
		`, svcUUID).Scan(&sid, &tid, &curName, &curDesc); err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "service not found"})
			return
		}
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, serviceRef); err != nil {
			writeDomainError(w, err)
			return
		}
		if body.Name != nil && strings.TrimSpace(*body.Name) != "" {
			curName = compat.SanitizeString(*body.Name)
		}
		if body.Description != nil {
			curDesc = compat.SanitizeString(*body.Description)
		}
		if body.Name != nil || body.Description != nil {
			if _, err := a.Pool.Exec(r.Context(), `UPDATE services SET name = $1, description = $2, updated_at = NOW() WHERE id = $3`, curName, curDesc, sid); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
		}
		if body.DockerComposeRaw != nil {
			svc, err := a.ServiceSvc.UpdateCompose(r.Context(), p, svcUUID, *body.DockerComposeRaw)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			_, _ = a.Dispatcher.FlushPending(r.Context())
			writeJSON(w, http.StatusOK, svc)
			return
		}
		_, _ = a.Dispatcher.FlushPending(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{"id": sid, "uuid": svcUUID, "name": curName, "description": curDesc})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
}

// ---- Databases & Backups ----

func (a *App) handleDatabases(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/databases"), "/")
	if trimmed == "" {
		switch r.Method {
		case http.MethodGet:
			list, err := a.DatabaseSvc.ListByTeam(r.Context(), p, activeTeamForPrincipal(p))
			if err != nil {
				writeDomainError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, list)
		case http.MethodPost:
			var body struct {
				Name            string `json:"name"`
				Type            string `json:"type"`
				Image           string `json:"image"`
				Password        string `json:"password"`
				CustomPort      int    `json:"custom_port"`
				PublicPort      int    `json:"public_port"`
				ServerUUID      string `json:"server_uuid"`
				EnvironmentUUID string `json:"environment_uuid"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid database payload"})
				return
			}
			port := body.CustomPort
			if port == 0 {
				port = body.PublicPort
			}
			db, err := a.DatabaseSvc.Create(r.Context(), p, activeTeamForPrincipal(p), body.Name, body.Type, body.Image, body.Password, port, body.ServerUUID, body.EnvironmentUUID)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			_, _ = a.Dispatcher.FlushPending(r.Context())
			writeJSON(w, http.StatusCreated, db)
		default:
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		}
		return
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) == 1 && r.Method == http.MethodPost && isDatabaseTypeSegment(parts[0]) {
		var body struct {
			Name            string `json:"name"`
			Image           string `json:"image"`
			Password        string `json:"password"`
			CustomPort      int    `json:"custom_port"`
			PublicPort      int    `json:"public_port"`
			ServerUUID      string `json:"server_uuid"`
			EnvironmentUUID string `json:"environment_uuid"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid database payload"})
			return
		}
		port := body.CustomPort
		if port == 0 {
			port = body.PublicPort
		}
		db, err := a.DatabaseSvc.Create(r.Context(), p, activeTeamForPrincipal(p), body.Name, parts[0], body.Image, body.Password, port, body.ServerUUID, body.EnvironmentUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Dispatcher.FlushPending(r.Context())
		writeJSON(w, http.StatusCreated, db)
		return
	}
	dbUUID := parts[0]

	if len(parts) == 2 && (parts[1] == "provision" || parts[1] == "start" || parts[1] == "restart") && (r.Method == http.MethodPost || r.Method == http.MethodGet) {
		rec, err := a.DatabaseSvc.EnqueueProvision(r.Context(), p, dbUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Dispatcher.FlushPending(r.Context())
		writeJSON(w, http.StatusAccepted, rec)
		return
	}
	if len(parts) == 2 && parts[1] == "stop" && (r.Method == http.MethodPost || r.Method == http.MethodGet) {
		db, err := a.DatabaseSvc.GetByUUID(r.Context(), p, dbUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		tid := db.TeamID
		if err := a.Authz.Check(r.Context(), p, access.ActionDeploy, access.ResourceRef{
			Type: "database", ID: db.ID, UUID: db.UUID, ProjectUUID: db.ProjectUUID, EnvironmentUUID: db.EnvironmentUUID, TeamID: &tid, APISegment: "databases",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		rec, err := a.enqueueResourceLifecycleJob(r.Context(), "database", db.UUID, db.ServerUUID, db.TeamID, "db_stop")
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusAccepted, rec)
		return
	}

	if len(parts) >= 2 && parts[1] == "storages" {
		db, err := a.DatabaseSvc.GetByUUID(r.Context(), p, dbUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		stUUID := ""
		if len(parts) >= 3 {
			stUUID = parts[2]
		}
		a.handleResourceStorages(w, r, p, `App\Models\StandaloneDatabase`, "database", db.ID, db.UUID, db.TeamID, stUUID)
		return
	}
	if len(parts) >= 2 && parts[1] == "backups" {
		db, err := a.DatabaseSvc.GetByUUID(r.Context(), p, dbUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		if len(parts) == 4 && parts[3] == "restore" && r.Method == http.MethodPost {
			var body struct {
				Target backups.DumpTarget `json:"target"`
				S3     *backups.S3Target  `json:"s3,omitempty"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body.Target.DatabaseType == "" {
				body.Target.DatabaseType = db.Type
			}
			if err := a.BackupSvc.RestoreRealBackup(r.Context(), p, parts[2], body.Target, body.S3); err != nil {
				writeDomainError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "restored"})
			return
		}
		if r.Method == http.MethodGet {
			teamID := db.TeamID
			if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
				Type: "database", ID: db.ID, UUID: db.UUID, TeamID: &teamID, APISegment: "backups",
			}); err != nil {
				writeDomainError(w, err)
				return
			}
			rows, err := a.Pool.Query(r.Context(), `
				SELECT b.id, b.uuid, b.frequency, b.enabled, COALESCE(b.save_s3, FALSE),
				       COALESCE(b.database_retention_count, 7), COALESCE(b.timezone, 'UTC'),
				       (SELECT COUNT(*) FROM scheduled_database_backup_executions e WHERE e.scheduled_database_backup_id = b.id)
				FROM scheduled_database_backups b
				WHERE b.database_id = $1 AND b.team_id = $2
				ORDER BY b.id DESC
			`, db.ID, db.TeamID)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
				return
			}
			defer rows.Close()
			list := make([]map[string]any, 0)
			for rows.Next() {
				var id int64
				var u, freq, tz string
				var en, saveS3 bool
				var ret, execCnt int
				if rows.Scan(&id, &u, &freq, &en, &saveS3, &ret, &tz, &execCnt) == nil {
					list = append(list, map[string]any{
						"id":                       id,
						"uuid":                     u,
						"frequency":                freq,
						"enabled":                  en,
						"save_s3":                  saveS3,
						"database_retention_count": ret,
						"timezone":                 tz,
						"executions_count":         execCnt,
					})
				}
			}
			writeJSON(w, http.StatusOK, list)
			return
		}
		if r.Method == http.MethodPost {
			var body struct {
				Frequency      string             `json:"frequency"`
				Enabled        *bool              `json:"enabled"`
				RetentionCount int                `json:"retention_count"`
				Target         backups.DumpTarget `json:"target"`
				S3             *backups.S3Target  `json:"s3,omitempty"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			freq := strings.TrimSpace(body.Frequency)
			if freq == "" {
				freq = "0 0 * * *"
			}
			enabled := true
			if body.Enabled != nil {
				enabled = *body.Enabled
			}
			sched, err := a.BackupSvc.CreateSchedule(r.Context(), p, backups.BackupSchedule{
				DatabaseID: db.ID, DatabaseUUID: db.UUID, DatabaseType: db.Type,
				TeamID: db.TeamID, RetentionCount: body.RetentionCount, Enabled: enabled,
			}, freq)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			// If caller only supplied schedule configuration (Coolify REST API v1 POST /databases/{uuid}/backups),
			// return the persisted schedule without attempting an immediate dump without target parameters.
			if body.Target.DockerContainer == "" && body.Target.Host == "" && body.Frequency != "" {
				writeJSON(w, http.StatusCreated, map[string]any{
					"id":              sched.ID,
					"uuid":            sched.UUID,
					"frequency":       freq,
					"enabled":         enabled,
					"retention_count": sched.RetentionCount,
					"database_id":     db.ID,
					"database_uuid":   db.UUID,
				})
				return
			}
			if body.Target.DatabaseType == "" {
				body.Target.DatabaseType = db.Type
			}
			opID := "bkop-" + a.Hooks.NewCUID2()
			rec, execRow, err := a.BackupSvc.RunRealBackup(r.Context(), p, sched, databases.DatabaseRecord{
				ID: db.ID, UUID: db.UUID, Name: db.Name, Type: db.Type, TeamID: db.TeamID, ServerUUID: db.ServerUUID,
			}, body.Target, body.S3, opID)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			writeJSON(w, http.StatusCreated, map[string]any{
				"schedule_uuid":   sched.UUID,
				"execution_uuid":  rec.ExecutionUUID,
				"operation_id":    rec.OperationID,
				"is_instance_db":  rec.IsInstanceDB,
				"pruned_count":    rec.PrunedCount,
				"checksum_sha256": execRow.ChecksumSHA,
				"size":            execRow.Size,
			})
			return
		}
	}

	switch r.Method {
	case http.MethodGet:
		db, err := a.DatabaseSvc.GetByUUID(r.Context(), p, dbUUID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, db)
	case http.MethodPatch, http.MethodPut:
		var body struct {
			CustomPort int `json:"custom_port"`
			PublicPort int `json:"public_port"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid database update"})
			return
		}
		port := body.CustomPort
		if port == 0 {
			port = body.PublicPort
		}
		db, err := a.DatabaseSvc.UpdatePort(r.Context(), p, dbUUID, port)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, db)
	case http.MethodDelete:
		if err := a.DatabaseSvc.Delete(r.Context(), p, dbUUID); err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Database deleted."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Teams, Grants, Audit, Search ----

func (a *App) handleTeams(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/teams"), "/")
	if trimmed == "" && r.Method == http.MethodGet {
		list, err := a.TeamSvc.ListUserTeams(r.Context(), p.UserID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	parts := strings.Split(trimmed, "/")
	if parts[0] == "current" {
		curTeam := activeTeamForPrincipal(p)
		if len(parts) == 1 && r.Method == http.MethodGet {
			a.serveTeamByID(w, r, p, curTeam)
			return
		}
		if len(parts) == 2 && parts[1] == "members" && r.Method == http.MethodGet {
			members, err := a.TeamSvc.ListMembers(r.Context(), p, curTeam)
			if err != nil {
				writeDomainError(w, err)
				return
			}
			writeJSON(w, http.StatusOK, members)
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	teamID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid team id"})
		return
	}
	if len(parts) == 1 && r.Method == http.MethodGet {
		a.serveTeamByID(w, r, p, teamID)
		return
	}
	if len(parts) == 1 && r.Method == http.MethodDelete {
		if teamID == 0 {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "cannot delete root team (id=0)"})
			return
		}
		role, ok := p.TeamRoles[teamID]
		if !ok || role != auth.RoleOwner {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "only team owner can delete team"})
			return
		}
		if _, err := a.Pool.Exec(r.Context(), `DELETE FROM teams WHERE id = $1`, teamID); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Team deleted."})
		return
	}
	if len(parts) == 2 && parts[1] == "members" && r.Method == http.MethodGet {
		members, err := a.TeamSvc.ListMembers(r.Context(), p, teamID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, members)
		return
	}
	if len(parts) == 2 && parts[1] == "grants" && r.Method == http.MethodPost {
		var body struct {
			MemberID int64            `json:"member_id"`
			Grants   []map[string]any `json:"grants"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid grants payload"})
			return
		}
		if err := a.TeamSvc.SetMemberAccessGrants(r.Context(), p, teamID, body.MemberID, body.Grants); err != nil {
			writeDomainError(w, err)
			return
		}
		_, _ = a.Dispatcher.FlushPending(r.Context())
		writeJSON(w, http.StatusOK, map[string]string{"message": "Access grants updated."})
		return
	}
	if len(parts) == 2 && parts[1] == "invitations" && r.Method == http.MethodPost {
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{
			Type: "team", ID: teamID, TeamID: &teamID, APISegment: "teams",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		var body struct {
			Email string    `json:"email"`
			Role  auth.Role `json:"role"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "email required"})
			return
		}
		if body.Role == "" {
			body.Role = auth.RoleMember
		}
		invUUID := a.Hooks.NewCUID2()
		if err := a.AuthRepo.CreateInvitation(r.Context(), invUUID, teamID, body.Email, body.Role); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"token": invUUID, "email": body.Email, "role": body.Role})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

func (a *App) handleAudit(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	list, err := a.TeamSvc.ListAudit(r.Context(), p, activeTeamForPrincipal(p), 50)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (a *App) handleSearch(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	teamID := activeTeamForPrincipal(p)
	if err := a.Authz.CheckList(r.Context(), p, teamID, "projects"); err != nil {
		writeDomainError(w, err)
		return
	}
	// Versioned Redis cache is ONLY safe for unscoped principals without per-resource filters.
	unfilteredPrincipal := !p.TokenScoped && len(p.ResourceGrants) == 0 && (p.TeamRoles[teamID].AtLeast(auth.RoleAdmin) || p.TeamWideLevels == nil)
	cacheKey := fmt.Sprintf("coolify:go:v1:team:%d:search_ver", teamID)
	type searchIndex struct {
		Servers  []servers.Server   `json:"servers"`
		Projects []projects.Project `json:"projects"`
	}
	var idx searchIndex
	var curVer int64
	var hit bool
	if unfilteredPrincipal && a.Cache != nil {
		curVer, hit, _ = a.Cache.Read(r.Context(), cacheKey, &idx)
	}
	if !hit {
		srvs, _ := a.ServerRepo.ListByTeam(r.Context(), teamID)
		projs, _ := a.ProjectSvc.ListProjects(r.Context(), p, teamID)
		filteredSrvs := make([]servers.Server, 0, len(srvs))
		for _, srv := range srvs {
			tid := srv.TeamID
			if a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
				Type: "server", ID: srv.ID, UUID: srv.UUID, TeamID: &tid, APISegment: "servers",
			}) == nil {
				filteredSrvs = append(filteredSrvs, srv)
			}
		}
		idx = searchIndex{Servers: filteredSrvs, Projects: projs}
		if unfilteredPrincipal && a.Cache != nil {
			_, _ = a.Cache.TryPopulate(r.Context(), cacheKey, curVer, idx)
		}
	}
	results := make([]map[string]string, 0)
	for _, s := range idx.Servers {
		if q == "" || strings.Contains(strings.ToLower(s.Name), q) || strings.Contains(strings.ToLower(s.UUID), q) {
			results = append(results, map[string]string{"type": "server", "uuid": s.UUID, "name": s.Name})
		}
	}
	for _, pr := range idx.Projects {
		if q == "" || strings.Contains(strings.ToLower(pr.Name), q) || strings.Contains(strings.ToLower(pr.UUID), q) {
			results = append(results, map[string]string{"type": "project", "uuid": pr.UUID, "name": pr.Name})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results, "cache_version": curVer, "cache_hit": hit})
}

// ---- Notifications & Instance Settings ----

func (a *App) handleNotifications(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/v1/notifications"), "/")
	if trimmed != "" && trimmed != "test" {
		a.handleNotificationChannel(w, r, p, notifications.ChannelKind(strings.Split(trimmed, "/")[0]))
		return
	}
	teamID := activeTeamForPrincipal(p)
	if r.Method == http.MethodGet {
		list, err := a.NotifySvc.ListConfigs(r.Context(), p, teamID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
		return
	}
	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid notification config"})
			return
		}
		var body struct {
			Channel        notifications.ChannelKind `json:"channel"`
			ExpectedTeamID *int64                    `json:"expected_team_id"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid notification config"})
			return
		}
		switch body.Channel {
		case notifications.ChannelEmail, notifications.ChannelDiscord, notifications.ChannelSlack, notifications.ChannelTelegram, notifications.ChannelPushover, notifications.ChannelWebhook:
		default:
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "unsupported notification channel"})
			return
		}
		if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{Type: "team", ID: teamID, TeamID: &teamID, APISegment: "notifications"}); err != nil {
			writeDomainError(w, err)
			return
		}
		if body.ExpectedTeamID != nil && *body.ExpectedTeamID != teamID {
			writeJSON(w, http.StatusConflict, map[string]string{"message": "The active team changed. Reload this page before saving."})
			return
		}
		cfg, err := a.NotifySvc.LoadConfig(r.Context(), teamID, body.Channel)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			writeDomainError(w, err)
			return
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid notification config"})
			return
		}
		if err := a.NotifySvc.SaveConfig(r.Context(), p, teamID, cfg); err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Notification settings saved."})
		return
	}
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
}

func (a *App) handleNotificationTest(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	teamID := activeTeamForPrincipal(p)
	if err := a.Authz.Check(r.Context(), p, access.ActionUpdate, access.ResourceRef{
		Type: "team", ID: teamID, TeamID: &teamID, APISegment: "notifications",
	}); err != nil {
		writeDomainError(w, err)
		return
	}
	var body struct {
		Channel        notifications.ChannelKind `json:"channel"`
		Event          string                    `json:"event"`
		Summary        string                    `json:"summary"`
		ExpectedTeamID *int64                    `json:"expected_team_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid test notification payload"})
		return
	}
	if body.ExpectedTeamID != nil && *body.ExpectedTeamID != teamID {
		writeJSON(w, http.StatusConflict, map[string]string{"message": "The active team changed. Reload this page before sending a test notification."})
		return
	}
	if body.Event == "" {
		body.Event = "test"
	}
	if body.Summary == "" {
		body.Summary = "Goolify test notification"
	}
	rec, err := a.NotifySvc.SendReal(r.Context(), teamID, body.Channel, body.Event, body.Summary)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"message": err.Error(), "delivery": rec})
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

func (a *App) handleSettings(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	rootTeam := int64(0)
	if err := a.Authz.Check(r.Context(), p, access.ActionManageInstance, access.ResourceRef{
		Type: "instance_settings", ID: 0, TeamID: &rootTeam, APISegment: "settings",
	}); err != nil {
		writeDomainError(w, err)
		return
	}
	switch r.Method {
	case http.MethodGet:
		var name string
		var apiOn, mcpOn, regOn bool
		err := a.Pool.QueryRow(r.Context(), `
			SELECT instance_name, is_api_enabled, is_mcp_server_enabled, is_registration_enabled
			FROM instance_settings WHERE id = 0
		`).Scan(&name, &apiOn, &mcpOn, &regOn)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":                      0,
			"instance_name":           name,
			"is_api_enabled":          apiOn,
			"is_mcp_server_enabled":   mcpOn,
			"is_registration_enabled": regOn,
		})
	case http.MethodPatch:
		var body struct {
			InstanceName *string `json:"instance_name"`
			IsAPIEnabled *bool   `json:"is_api_enabled"`
			IsMCPEnabled *bool   `json:"is_mcp_server_enabled"`
			IsRegEnabled *bool   `json:"is_registration_enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid settings payload"})
			return
		}
		if body.InstanceName != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET instance_name = $1, updated_at = NOW() WHERE id = 0`, *body.InstanceName)
		}
		if body.IsAPIEnabled != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET is_api_enabled = $1, updated_at = NOW() WHERE id = 0`, *body.IsAPIEnabled)
		}
		if body.IsMCPEnabled != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET is_mcp_server_enabled = $1, updated_at = NOW() WHERE id = 0`, *body.IsMCPEnabled)
		}
		if body.IsRegEnabled != nil {
			_, _ = a.Pool.Exec(r.Context(), `UPDATE instance_settings SET is_registration_enabled = $1, updated_at = NOW() WHERE id = 0`, *body.IsRegEnabled)
		}
		writeJSON(w, http.StatusOK, map[string]string{"message": "Instance settings updated."})
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
	}
}

// ---- Webhooks (Fail-Closed Signature + Replay Protection + Real Enqueue) ----

func (a *App) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	trimmed := strings.Trim(strings.TrimPrefix(r.URL.Path, "/webhooks/"), "/")
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Invalid webhook signature."})
		return
	}
	provider, appUUID := parts[0], parts[1]
	body, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Failed to read payload."})
		return
	}
	verified, err := a.WebhookSvc.VerifyForApplication(r.Context(), provider, appUUID, r.Header, body)
	if err != nil {
		if errors.Is(err, webhooks.ErrWebhookReplay) {
			writeJSON(w, http.StatusConflict, map[string]string{"message": "Duplicate webhook delivery ignored."})
			return
		}
		// Generic error without leaking secret/signature details
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Invalid webhook signature."})
		return
	}

	// Enqueue real deployment job using a webhook system principal strictly scoped to the verified application's team.
	whPrincipal := auth.Principal{
		UserID:         -1,
		AuthMethod:     "session",
		SessionTeamID:  verified.TeamID,
		TeamRoles:      map[int64]auth.Role{verified.TeamID: auth.RoleAdmin},
		TeamWideAccess: map[int64]bool{verified.TeamID: true},
	}
	rec, err := a.AppSvc.EnqueueDeployment(r.Context(), whPrincipal, verified.ApplicationUUID, false, true, false)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	_, _ = a.Dispatcher.FlushPending(r.Context())
	writeJSON(w, http.StatusAccepted, map[string]any{
		"accepted":        true,
		"deployment_uuid": rec.DeploymentUUID,
		"operation_id":    rec.OperationID,
		"status":          rec.Status,
	})
}

// ---- MCP Endpoint (Independent of is_api_enabled per SECURITY.md) ----

func (a *App) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	enabled, err := a.Lookup.IsMCPServerEnabled(r.Context())
	if err != nil || !enabled {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "MCP server is disabled."})
		return
	}
	p, err := a.Authenticator.AuthenticateMCP(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Unauthenticated."})
		return
	}
	var rpc struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      any            `json:"id"`
		Method  string         `json:"method"`
		Params  map[string]any `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&rpc); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "Invalid JSON-RPC payload."})
		return
	}
	teamID := activeTeamForPrincipal(p)
	switch rpc.Method {
	case "tools/list":
		writeJSON(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0", "id": rpc.ID,
			"result": map[string]any{"tools": []string{"list_servers", "list_applications", "list_projects"}},
		})
	case "list_servers":
		if err := a.Authz.Check(r.Context(), p, access.ActionRead, access.ResourceRef{
			Type: "server", TeamID: &teamID, APISegment: "servers",
		}); err != nil {
			writeDomainError(w, err)
			return
		}
		srvs, _ := a.ServerRepo.ListByTeam(r.Context(), teamID)
		writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": srvs})
	case "list_applications":
		apps, err := a.AppSvc.ListByTeam(r.Context(), p, teamID)
		if err != nil {
			writeDomainError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": apps})
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"jsonrpc": "2.0", "id": rpc.ID,
			"error": map[string]any{"code": -32601, "message": "method not found"},
		})
	}
}

// ---- Realtime SSE Logs & Events ----

func (a *App) handleRealtimeLogs(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	resUUID := strings.TrimSpace(r.URL.Query().Get("resource"))
	if resUUID == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "resource query param required."})
		return
	}
	sinceCursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	if sinceCursor == 0 && r.Header.Get("Last-Event-ID") != "" {
		sinceCursor, _ = strconv.ParseInt(r.Header.Get("Last-Event-ID"), 10, 64)
	}

	// Resolve resource team and project/environment hierarchy from PostgreSQL (never trust query param or session team!)
	teamID, resType, resID, projUUID, envUUID, err := a.resolveResourceIdentity(r.Context(), resUUID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "resource not found"})
		return
	}
	resRef := access.ResourceRef{
		Type: resType, ID: resID, UUID: resUUID, ProjectUUID: projUUID, EnvironmentUUID: envUUID,
		TeamID: &teamID, APISegment: resType + "s",
	}
	if err := a.Authz.Check(r.Context(), p, access.ActionRead, resRef); err != nil {
		writeDomainError(w, err)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, _ := w.(http.Flusher)

	entries, gap, err := a.LogStore.ReplayFrom(r.Context(), resUUID, sinceCursor, 500)
	if err == nil {
		if gap {
			_, _ = fmt.Fprintf(w, "event: gap\ndata: {\"reason\":\"cursor_pruned\"}\n\n")
		}
		for _, e := range entries {
			payload, _ := json.Marshal(map[string]any{
				"cursor": e.Cursor,
				"stream": e.Stream,
				"text":   e.Text,
			})
			_, _ = fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", e.Cursor, string(payload))
			sinceCursor = e.Cursor
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	// If ?once=1 (useful for snapshot fetches), return after replay.
	if r.URL.Query().Get("once") == "1" {
		return
	}

	// Subscribe to live team hub events; immediately reauthorize and close on grant revocation!
	ch, cancel := a.Hub.Subscribe(teamID)
	defer cancel()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			if evt.MutationType == "team.grant_revoked" {
				// Authoritative fail-closed re-check against PostgreSQL!
				freshP, loadErr := a.Lookup.LoadPrincipalByUserID(r.Context(), p.UserID, p.SessionTeamID)
				if loadErr != nil || a.Authz.Check(r.Context(), freshP, access.ActionRead, resRef) != nil {
					_, _ = fmt.Fprintf(w, "event: revoked\ndata: {\"reason\":\"access_revoked\"}\n\n")
					if flusher != nil {
						flusher.Flush()
					}
					return
				}
			}
			if evt.ResourceUUID == resUUID {
				newEntries, _, _ := a.LogStore.ReplayFrom(r.Context(), resUUID, sinceCursor, 100)
				for _, e := range newEntries {
					payload, _ := json.Marshal(map[string]any{
						"cursor": e.Cursor,
						"stream": e.Stream,
						"text":   e.Text,
					})
					_, _ = fmt.Fprintf(w, "id: %d\nevent: log\ndata: %s\n\n", e.Cursor, string(payload))
					sinceCursor = e.Cursor
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		case <-ticker.C:
			_, _ = fmt.Fprintf(w, ": heartbeat\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

func (a *App) handleRealtimeEvents(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	channelParam := strings.TrimSpace(r.URL.Query().Get("channel"))
	teamID := activeTeamForPrincipal(p)
	isUserChannel := false
	targetUserID := p.UserID
	channelName := fmt.Sprintf("team.%d", teamID)

	if channelParam != "" {
		switch {
		case strings.HasPrefix(channelParam, "team."):
			reqTeamID, err := strconv.ParseInt(strings.TrimPrefix(channelParam, "team."), 10, 64)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid team channel"})
				return
			}
			if _, ok := p.TeamRoles[reqTeamID]; !ok {
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "forbidden channel subscription"})
				return
			}
			teamID = reqTeamID
			channelName = fmt.Sprintf("team.%d", teamID)
		case strings.HasPrefix(channelParam, "user."):
			reqUserID, err := strconv.ParseInt(strings.TrimPrefix(channelParam, "user."), 10, 64)
			if err != nil {
				writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid user channel"})
				return
			}
			if reqUserID != p.UserID {
				writeJSON(w, http.StatusForbidden, map[string]string{"message": "forbidden user channel subscription"})
				return
			}
			isUserChannel = true
			targetUserID = reqUserID
			channelName = fmt.Sprintf("user.%d", targetUserID)
		default:
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "unsupported channel; expected team.{id} or user.{id}"})
			return
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	flusher, _ := w.(http.Flusher)

	if isUserChannel {
		_, _ = fmt.Fprintf(w, "event: connected\ndata: {\"channel\":%q,\"user_id\":%d}\n\n", channelName, targetUserID)
	} else {
		_, _ = fmt.Fprintf(w, "event: connected\ndata: {\"channel\":%q,\"team_id\":%d}\n\n", channelName, teamID)
	}
	if flusher != nil {
		flusher.Flush()
	}
	if r.URL.Query().Get("once") == "1" {
		return
	}

	var (
		ch     <-chan realtime.DomainEvent
		cancel func()
	)
	if isUserChannel {
		ch, cancel = a.Hub.SubscribeUser(targetUserID)
	} else {
		ch, cancel = a.Hub.Subscribe(teamID)
	}
	defer cancel()
	for {
		select {
		case <-r.Context().Done():
			return
		case evt, ok := <-ch:
			if !ok {
				return
			}
			raw, _ := json.Marshal(evt)
			_, _ = fmt.Fprintf(w, "id: %s\nevent: %s\ndata: %s\n\n", evt.EventID, evt.MutationType, string(raw))
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

// ---- Terminal One-Use Ticket & Live WebSocket ----

func (a *App) handleTerminalTicket(w http.ResponseWriter, r *http.Request, p auth.Principal) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"message": "Method not allowed."})
		return
	}
	var body struct {
		ServerUUID string `json:"server_uuid"`
		Container  string `json:"container"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ServerUUID) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "server_uuid is required."})
		return
	}
	body.Container = strings.TrimSpace(body.Container)
	if body.Container != "" && !isValidContainerName(body.Container) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid container name."})
		return
	}
	srv, err := a.ServerRepo.GetByUUID(r.Context(), body.ServerUUID)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	if (srv.ID == 0 || srv.UUID == "localhost") && !(srv.TeamID == 0 && p.IsInstanceAdmin) {
		writeDomainError(w, realtime.ErrTerminalNotAllowed)
		return
	}
	var termEnabled bool
	_ = a.Pool.QueryRow(r.Context(), `SELECT COALESCE(is_terminal_enabled, TRUE) FROM server_settings WHERE server_id = $1`, srv.ID).Scan(&termEnabled)
	var pkTeamID int64 = srv.TeamID
	if pk, err := a.ServerRepo.FindPrivateKeyByUUID(r.Context(), srv.PrivateKeyUUID); err == nil {
		pkTeamID = pk.TeamID
	}

	origin := a.allowedOrigin
	if origin == "" {
		origin = r.Header.Get("Origin")
	}
	ticket, err := a.Tickets.IssueTicket(r.Context(), p, realtime.TerminalTarget{
		ServerID:         srv.ID,
		ServerUUID:       srv.UUID,
		TeamID:           srv.TeamID,
		Container:        body.Container,
		Mode:             "shell",
		TerminalEnabled:  termEnabled,
		ForceDisabled:    srv.ForceDisabled,
		TransferredAway:  srv.IsTransferredAway(),
		PrivateKeyTeamID: pkTeamID,
		AllowedOrigin:    origin,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"ticket": ticket.Token, "expires_at": ticket.ExpiresAt})
}

func (a *App) handleTerminalWS(w http.ResponseWriter, r *http.Request) {
	p, err := a.Authenticator.AuthenticateSession(r.Context(), r)
	if err != nil {
		writeAuthError(w, err)
		return
	}
	tok := strings.TrimSpace(r.URL.Query().Get("ticket"))
	if tok == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "missing terminal ticket"})
		return
	}
	target, err := a.Tickets.RedeemTicket(r.Context(), p, tok, r.Header.Get("Origin"))
	if err != nil {
		if errors.Is(err, realtime.ErrOriginMismatch) || errors.Is(err, access.ErrForbidden) || errors.Is(err, realtime.ErrTerminalNotAllowed) {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": err.Error()})
		return
	}

	srv, err := a.ServerRepo.GetByUUID(r.Context(), target.ServerUUID)
	if err != nil || srv.IsTransferredAway() || srv.ForceDisabled {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "target server unavailable"})
		return
	}
	isLocalhost := (srv.ID == 0 || srv.UUID == "localhost")
	if isLocalhost && !(srv.TeamID == 0 && p.IsInstanceAdmin) {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "host boundary denied"})
		return
	}

	var decryptedKey string
	if !isLocalhost {
		keyPEM, keyTeamID, kErr := a.loadServerSSHKey(r.Context(), srv.PrivateKeyID)
		if kErr != nil || keyTeamID != srv.TeamID || strings.TrimSpace(keyPEM) == "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "missing or unauthorized server private key"})
			return
		}
		decryptedKey = keyPEM
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: a.allowedOrigin == "",
	})
	if err != nil {
		return
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "closed") }()

	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Hour)
	defer cancel()
	_ = conn.Write(ctx, websocket.MessageText, []byte(fmt.Sprintf("Connected to %s\r\n", target.ServerUUID)))

	cols, rows := 80, 24
	for {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			return
		}
		// Authoritative re-check on every interactive command before running anything!
		freshP, loadErr := a.Lookup.LoadPrincipalByUserID(ctx, p.UserID, p.SessionTeamID)
		tID := srv.TeamID
		if loadErr != nil || a.Authz.Check(ctx, freshP, access.ActionTerminal, access.ResourceRef{
			Type: "server", ID: srv.ID, UUID: srv.UUID, TeamID: &tID, APISegment: "servers",
		}) != nil {
			_ = conn.Close(websocket.StatusPolicyViolation, "access revoked")
			return
		}
		if isLocalhost && !(srv.TeamID == 0 && freshP.IsInstanceAdmin) {
			_ = conn.Close(websocket.StatusPolicyViolation, "host boundary denied")
			return
		}

		cmdText := strings.TrimSpace(string(msg))
		if strings.HasPrefix(cmdText, "{") {
			var frame struct {
				Type string `json:"type"`
				Data string `json:"data"`
				Cols int    `json:"cols"`
				Rows int    `json:"rows"`
			}
			if err := json.Unmarshal(msg, &frame); err == nil && frame.Type != "" {
				if frame.Type == "resize" {
					if frame.Cols > 0 && frame.Cols <= 500 {
						cols = frame.Cols
					}
					if frame.Rows > 0 && frame.Rows <= 200 {
						rows = frame.Rows
					}
					continue
				}
				if frame.Type == "stdin" {
					cmdText = strings.TrimSpace(frame.Data)
				}
			}
		}
		if cmdText == "" {
			continue
		}
		if cmdText == "exit" {
			_ = conn.Close(websocket.StatusNormalClosure, "bye")
			return
		}
		execCmd := cmdText
		if target.Container != "" {
			execCmd = fmt.Sprintf("docker exec -i %s sh -c %s", shellSingleQuote(target.Container), shellSingleQuote(cmdText))
		}
		capability := "ssh_exec"
		if isLocalhost {
			capability = "local_exec"
		}
		port := srv.Port
		if port <= 0 {
			port = 22
		}
		user := strings.TrimSpace(srv.User)
		if user == "" {
			user = "root"
		}
		opID := "term-" + a.Hooks.NewCUID2()
		_, _ = a.Executor.Run(ctx, executor.ExecutionRequest{
			ProtocolVersion:    executor.ProtocolVersionV1,
			FrameType:          executor.FrameStart,
			JobID:              "term-" + opID,
			OperationID:        opID,
			AttemptID:          "att-1",
			OwnershipEpoch:     1,
			ResourceUUID:       srv.UUID,
			TeamID:             srv.TeamID,
			DeadlineMS:         15000,
			MaxFrameBytes:      executor.DefaultMaxFrameBytes,
			ExpectedCapability: capability,
			Operation: executor.OperationSpec{
				Kind:          "pty",
				Host:          srv.IP,
				Port:          port,
				User:          user,
				SSHKeyContent: decryptedKey,
				UseSudo:       user != "root",
				Command:       execCmd,
				PTYCols:       cols,
				PTYRows:       rows,
			},
		}, func(ev executor.ExecutionEvent) error {
			if ev.EventType == "output" {
				return conn.Write(ctx, websocket.MessageText, []byte(ev.Data+"\r\n"))
			}
			return nil
		})
	}
}

func (a *App) loadServerSSHKey(ctx context.Context, pkID int64) (string, int64, error) {
	if pkID == 0 {
		return "", 0, errors.New("server has no private key configured")
	}
	var rawKey string
	var teamID int64
	if err := a.Pool.QueryRow(ctx, `SELECT private_key, team_id FROM private_keys WHERE id = $1`, pkID).Scan(&rawKey, &teamID); err != nil {
		return "", 0, err
	}
	if a.Cipher != nil && rawKey != "" {
		if plain, decErr := a.Cipher.DecryptLaravelPayload(rawKey, false); decErr == nil {
			rawKey = string(plain)
		}
	}
	return rawKey, teamID, nil
}

func isValidContainerName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---- Helpers ----

func (a *App) resolveResourceIdentity(ctx context.Context, uuid string) (teamID int64, resType string, resID int64, projectUUID string, environmentUUID string, err error) {
	if err = a.Pool.QueryRow(ctx, `
		SELECT p.team_id, a.id, p.uuid, e.uuid FROM applications a
		JOIN environments e ON e.id = a.environment_id
		JOIN projects p ON p.id = e.project_id
		WHERE a.uuid = $1
	`, uuid).Scan(&teamID, &resID, &projectUUID, &environmentUUID); err == nil {
		return teamID, "application", resID, projectUUID, environmentUUID, nil
	}
	if err = a.Pool.QueryRow(ctx, `SELECT team_id, id FROM servers WHERE uuid = $1 AND deleted_at IS NULL`, uuid).Scan(&teamID, &resID); err == nil {
		return teamID, "server", resID, "", "", nil
	}
	if err = a.Pool.QueryRow(ctx, `
		SELECT p.team_id, s.id, p.uuid, e.uuid FROM services s
		JOIN environments e ON e.id = s.environment_id
		JOIN projects p ON p.id = e.project_id
		WHERE s.uuid = $1
	`, uuid).Scan(&teamID, &resID, &projectUUID, &environmentUUID); err == nil {
		return teamID, "service", resID, projectUUID, environmentUUID, nil
	}
	for _, tbl := range []string{
		"standalone_postgresqls", "standalone_mysqls", "standalone_mariadbs",
		"standalone_mongodbs", "standalone_redis", "standalone_keydbs",
		"standalone_dragonflies", "standalone_clickhouses", "standalone_sqlites",
	} {
		q := fmt.Sprintf(`
			SELECT COALESCE(p.team_id, srv.team_id, 0), d.id, COALESCE(p.uuid,''), COALESCE(e.uuid,'')
			FROM %s d
			LEFT JOIN environments e ON e.id = d.environment_id
			LEFT JOIN projects p ON p.id = e.project_id
			LEFT JOIN standalone_dockers sd ON sd.id = d.destination_id
			LEFT JOIN servers srv ON srv.id = sd.server_id
			WHERE d.uuid = $1 AND d.deleted_at IS NULL
		`, tbl)
		if err = a.Pool.QueryRow(ctx, q, uuid).Scan(&teamID, &resID, &projectUUID, &environmentUUID); err == nil {
			return teamID, "database", resID, projectUUID, environmentUUID, nil
		}
	}
	return 0, "", 0, "", "", errors.New("resource not found")
}

func activeTeamForPrincipal(p auth.Principal) int64 {
	if p.AuthMethod == "bearer" || p.AuthMethod == "mcp" {
		if p.TokenTeamID != nil {
			return *p.TokenTeamID
		}
	}
	return p.SessionTeamID
}

func setSessionCookies(w http.ResponseWriter, sess auth.SessionData) {
	http.SetCookie(w, &http.Cookie{
		Name:     auth.SessionCookieName,
		Value:    sess.ID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  sess.ExpiresAt,
	})
	http.SetCookie(w, &http.Cookie{
		Name:     "coolify_csrf",
		Value:    sess.CSRFToken,
		Path:     "/",
		HttpOnly: false,
		SameSite: http.SameSiteLaxMode,
		Expires:  sess.ExpiresAt,
	})
}

func clearSessionCookies(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: auth.SessionCookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
	http.SetCookie(w, &http.Cookie{Name: "coolify_csrf", Value: "", Path: "/", MaxAge: -1})
}

func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, access.ErrForbidden), errors.Is(err, realtime.ErrTerminalNotAllowed):
		writeJSON(w, http.StatusForbidden, map[string]string{"message": err.Error()})
	case errors.Is(err, servers.ErrNotFound), errors.Is(err, projects.ErrNotFound),
		errors.Is(err, applications.ErrNotFound), errors.Is(err, services.ErrNotFound),
		errors.Is(err, databases.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"message": err.Error()})
	case errors.Is(err, servers.ErrPrivateKeyTeam), errors.Is(err, servers.ErrTransferredServer),
		errors.Is(err, servers.ErrInvalidLocalhost), errors.Is(err, databases.ErrCannotDeleteRoot),
		errors.Is(err, backups.ErrRestoreChecksumMismatch):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
	default:
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
