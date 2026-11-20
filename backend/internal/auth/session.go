package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrUnauthenticated = errors.New("unauthenticated")
	ErrTokenRevoked    = errors.New("token revoked or expired")
	ErrCSRFMismatch    = errors.New("csrf token mismatch")
	ErrAPIDisabled     = errors.New("api access is disabled")
)

type Role string

const (
	RoleNone   Role = ""
	RoleMember Role = "member"
	RoleAdmin  Role = "admin"
	RoleOwner  Role = "owner"
)

// Rank mirrors Coolify's Role::MEMBER < Role::ADMIN < Role::OWNER comparison.
func (r Role) Rank() int {
	switch r {
	case RoleOwner:
		return 3
	case RoleAdmin:
		return 2
	case RoleMember:
		return 1
	default:
		return 0
	}
}

func (r Role) AtLeast(other Role) bool {
	return r.Rank() >= other.Rank()
}

type AccessLevel string

const (
	AccessRead          AccessLevel = "read"
	AccessReadSensitive AccessLevel = "read:sensitive"
	AccessDeploy        AccessLevel = "deploy"
	AccessWrite         AccessLevel = "write"
	AccessDelete        AccessLevel = "delete"
	AccessRoot          AccessLevel = "root"
)

// ExpandAccessLevels expands stored grant/ability strings into implied access levels
// matching Coolify's App\Enums\AccessLevel::grants() hierarchy.
func ExpandAccessLevels(levels []string) map[AccessLevel]bool {
	out := make(map[AccessLevel]bool)
	for _, raw := range levels {
		switch AccessLevel(raw) {
		case AccessRoot:
			out[AccessRoot] = true
			out[AccessWrite] = true
			out[AccessDelete] = true
			out[AccessDeploy] = true
			out[AccessReadSensitive] = true
			out[AccessRead] = true
		case AccessWrite:
			out[AccessWrite] = true
			out[AccessDeploy] = true
			out[AccessReadSensitive] = true
			out[AccessRead] = true
		case AccessDelete:
			out[AccessDelete] = true
			out[AccessRead] = true
		case AccessDeploy:
			out[AccessDeploy] = true
			out[AccessRead] = true
		case AccessReadSensitive:
			out[AccessReadSensitive] = true
			out[AccessRead] = true
		case AccessRead:
			out[AccessRead] = true
		}
	}
	return out
}

// MergeAccessLevels copies implied access levels from src into dst.
func MergeAccessLevels(dst map[AccessLevel]bool, levels []string) map[AccessLevel]bool {
	if dst == nil {
		dst = make(map[AccessLevel]bool)
	}
	for k, v := range ExpandAccessLevels(levels) {
		if v {
			dst[k] = true
		}
	}
	return dst
}

type ResourceGrant struct {
	Scope        string      `json:"scope,omitempty"` // team | project | environment | resource
	ResourceType string      `json:"resource_type,omitempty"`
	ResourceID   int64       `json:"resource_id,omitempty"`
	ResourceUUID string      `json:"resource_uuid,omitempty"`
	TeamID       int64       `json:"team_id"`
	Level        AccessLevel `json:"level"`
	Levels       []string    `json:"levels,omitempty"`
	ServerIDs    []int64     `json:"server_ids,omitempty"`
}

type Principal struct {
	UserID            int64                       `json:"user_id"`
	Email             string                      `json:"email"`
	SessionTeamID     int64                       `json:"session_team_id"`
	IsInstanceAdmin   bool                        `json:"is_instance_admin"`
	AuthMethod        string                      `json:"auth_method"` // session | bearer | mcp
	TokenID           int64                       `json:"token_id,omitempty"`
	TokenTeamID       *int64                      `json:"token_team_id,omitempty"`
	TokenAbilities    []string                    `json:"token_abilities,omitempty"`
	TokenWholeTeam    bool                        `json:"token_whole_team,omitempty"`
	TokenScoped       bool                        `json:"token_scoped,omitempty"`
	TokenScopes       []ResourceGrant             `json:"token_scopes,omitempty"`
	TeamRoles         map[int64]Role              `json:"team_roles"`
	TeamWideAccess    map[int64]bool              `json:"team_wide_access"`
	TeamWideLevels    map[int64]map[AccessLevel]bool `json:"team_wide_levels,omitempty"`
	ResourceGrants    []ResourceGrant             `json:"resource_grants"`
	ForceReset        bool                        `json:"force_password_reset,omitempty"`
	TwoFactorVerified bool                        `json:"two_factor_verified,omitempty"`
}

// TokenRecord represents a persisted Sanctum personal_access_tokens row.
type TokenRecord struct {
	ID         int64
	UserID     int64
	TeamID     *int64
	HashSHA256 string
	Abilities  []string
	WholeTeam  bool
	HasScopes  bool
	Scopes     []ResourceGrant
	ExpiresAt  *time.Time
	Revoked    bool
}

type PrincipalLookup interface {
	LoadPrincipalByUserID(ctx context.Context, userID int64, sessionTeamID int64) (Principal, error)
	FindTokenByID(ctx context.Context, tokenID int64) (TokenRecord, error)
	IsAPIDisabled(ctx context.Context) (bool, error)
}

type SessionData struct {
	ID         string
	UserID     int64
	TeamID     int64
	CSRFToken  string
	ExpiresAt  time.Time
	Revoked    bool
	TwoFAValid bool
}

type SessionStore interface {
	Get(ctx context.Context, sessionID string) (SessionData, error)
	Save(ctx context.Context, sess SessionData) error
	Revoke(ctx context.Context, sessionID string) error
	RevokeAllForUser(ctx context.Context, userID int64) error
}

// MemorySessionStore is a thread-safe in-memory session store for tests and local runtime.
type MemorySessionStore struct {
	mu       sync.RWMutex
	sessions map[string]SessionData
}

func NewMemorySessionStore() *MemorySessionStore {
	return &MemorySessionStore{sessions: make(map[string]SessionData)}
}

func (m *MemorySessionStore) Get(_ context.Context, sessionID string) (SessionData, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[sessionID]
	if !ok {
		return SessionData{}, ErrUnauthenticated
	}
	return s, nil
}

func (m *MemorySessionStore) Save(_ context.Context, sess SessionData) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[sess.ID] = sess
	return nil
}

func (m *MemorySessionStore) Revoke(_ context.Context, sessionID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[sessionID]; ok {
		s.Revoked = true
		m.sessions[sessionID] = s
	}
	return nil
}

func (m *MemorySessionStore) RevokeAllForUser(_ context.Context, userID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, s := range m.sessions {
		if s.UserID == userID {
			s.Revoked = true
			m.sessions[k] = s
		}
	}
	return nil
}

const SessionCookieName = "coolify_session"

type Authenticator struct {
	lookup   PrincipalLookup
	sessions SessionStore
	now      func() time.Time
}

func NewAuthenticator(lookup PrincipalLookup, sessions SessionStore) *Authenticator {
	return &Authenticator{
		lookup:   lookup,
		sessions: sessions,
		now:      time.Now,
	}
}

func (a *Authenticator) IssueSession(ctx context.Context, userID int64, teamID int64, ttl time.Duration) (SessionData, error) {
	id, err := randomHex(32)
	if err != nil {
		return SessionData{}, err
	}
	csrf, err := randomHex(32)
	if err != nil {
		return SessionData{}, err
	}
	sess := SessionData{
		ID:         id,
		UserID:     userID,
		TeamID:     teamID,
		CSRFToken:  csrf,
		ExpiresAt:  a.now().Add(ttl),
		TwoFAValid: true,
	}
	if err := a.sessions.Save(ctx, sess); err != nil {
		return SessionData{}, err
	}
	return sess, nil
}

// UpdateSessionTeam switches the active session team (never changes resource-team authorization on already-loaded resources).
func (a *Authenticator) UpdateSessionTeam(ctx context.Context, sessionID string, newTeamID int64) (SessionData, error) {
	sess, err := a.sessions.Get(ctx, sessionID)
	if err != nil || sess.Revoked || a.now().After(sess.ExpiresAt) {
		return SessionData{}, ErrUnauthenticated
	}
	sess.TeamID = newTeamID
	if err := a.sessions.Save(ctx, sess); err != nil {
		return SessionData{}, err
	}
	return sess, nil
}

// GetSession retrieves the current active session record from the request cookie.
func (a *Authenticator) GetSession(ctx context.Context, r *http.Request) (SessionData, error) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return SessionData{}, ErrUnauthenticated
	}
	sess, err := a.sessions.Get(ctx, cookie.Value)
	if err != nil || sess.Revoked || a.now().After(sess.ExpiresAt) {
		return SessionData{}, ErrUnauthenticated
	}
	return sess, nil
}

// RevokeSession revokes a single session by ID.
func (a *Authenticator) RevokeSession(ctx context.Context, sessionID string) error {
	return a.sessions.Revoke(ctx, sessionID)
}

// RevokeAllSessionsForUser revokes every session belonging to userID.
func (a *Authenticator) RevokeAllSessionsForUser(ctx context.Context, userID int64) error {
	return a.sessions.RevokeAllForUser(ctx, userID)
}

// AuthenticateSession validates the HTTP session cookie (and CSRF token on state-changing requests).
func (a *Authenticator) AuthenticateSession(ctx context.Context, r *http.Request) (Principal, error) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil || cookie.Value == "" {
		return Principal{}, ErrUnauthenticated
	}
	sess, err := a.sessions.Get(ctx, cookie.Value)
	if err != nil || sess.Revoked || a.now().After(sess.ExpiresAt) {
		return Principal{}, ErrUnauthenticated
	}
	if isMutatingMethod(r.Method) {
		gotCSRF := r.Header.Get("X-CSRF-TOKEN")
		if subtle.ConstantTimeCompare([]byte(gotCSRF), []byte(sess.CSRFToken)) != 1 {
			return Principal{}, ErrCSRFMismatch
		}
	}
	p, err := a.lookup.LoadPrincipalByUserID(ctx, sess.UserID, sess.TeamID)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	p.AuthMethod = "session"
	p.SessionTeamID = sess.TeamID
	p.TwoFactorVerified = sess.TwoFAValid
	return p, nil
}

// AuthenticateBearer validates a Sanctum-compatible bearer token ("<id>|<secret>" or raw token hash).
func (a *Authenticator) AuthenticateBearer(ctx context.Context, rawToken string) (Principal, error) {
	return a.authenticateToken(ctx, rawToken, false)
}

// AuthenticateMCP validates an MCP request token even when REST API access is disabled (per SECURITY.md intended behavior).
func (a *Authenticator) AuthenticateMCP(ctx context.Context, rawToken string) (Principal, error) {
	return a.authenticateToken(ctx, rawToken, true)
}

func (a *Authenticator) authenticateToken(ctx context.Context, rawToken string, isMCP bool) (Principal, error) {
	rawToken = strings.TrimSpace(strings.TrimPrefix(rawToken, "Bearer "))
	if rawToken == "" {
		return Principal{}, ErrUnauthenticated
	}
	if !isMCP {
		disabled, err := a.lookup.IsAPIDisabled(ctx)
		if err != nil || disabled {
			return Principal{}, ErrAPIDisabled
		}
	}

	parts := strings.SplitN(rawToken, "|", 2)
	if len(parts) != 2 {
		return Principal{}, ErrUnauthenticated
	}
	tokenID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || tokenID <= 0 || parts[1] == "" {
		return Principal{}, ErrUnauthenticated
	}
	rec, err := a.lookup.FindTokenByID(ctx, tokenID)
	if err != nil || rec.Revoked {
		return Principal{}, ErrTokenRevoked
	}
	if rec.ExpiresAt != nil && a.now().After(*rec.ExpiresAt) {
		return Principal{}, ErrTokenRevoked
	}
	sum := sha256.Sum256([]byte(parts[1]))
	actualHex := hex.EncodeToString(sum[:])
	if subtle.ConstantTimeCompare([]byte(actualHex), []byte(strings.ToLower(rec.HashSHA256))) != 1 {
		return Principal{}, ErrTokenRevoked
	}

	sessionTeam := int64(0)
	if rec.TeamID != nil {
		sessionTeam = *rec.TeamID
	}
	p, err := a.lookup.LoadPrincipalByUserID(ctx, rec.UserID, sessionTeam)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	if isMCP {
		p.AuthMethod = "mcp"
	} else {
		p.AuthMethod = "bearer"
	}
	p.TokenID = rec.ID
	p.TokenTeamID = rec.TeamID
	p.TokenAbilities = append([]string(nil), rec.Abilities...)
	p.TokenWholeTeam = rec.WholeTeam
	p.TokenScoped = rec.HasScopes && !rec.WholeTeam
	p.TokenScopes = append([]ResourceGrant(nil), rec.Scopes...)
	return p, nil
}

func isMutatingMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}

func randomHex(nBytes int) (string, error) {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("crypto rand: %w", err)
	}
	return hex.EncodeToString(b), nil
}
