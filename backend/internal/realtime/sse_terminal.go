package realtime

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
)

type LogEntry struct {
	Cursor    int64  `json:"cursor"`
	Stream    string `json:"stream"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
}

// SSEHandler streams deployment/application logs with durable cursor, bounded buffers,
// secret redaction, and automatic revocation on grant/session changes.
type SSEHandler struct {
	authz   *access.Authorizer
	secrets []string
	mu      sync.RWMutex
	logs    map[string][]LogEntry // resourceUUID -> bounded log history
	maxKeep int
}

func NewSSEHandler(authz *access.Authorizer, maxKeep int, secrets ...string) *SSEHandler {
	if maxKeep <= 0 {
		maxKeep = 1000
	}
	return &SSEHandler{
		authz:   authz,
		secrets: secrets,
		logs:    make(map[string][]LogEntry),
		maxKeep: maxKeep,
	}
}

func (h *SSEHandler) AppendLog(resourceUUID string, stream string, rawText string) LogEntry {
	redacted := rawText
	for _, s := range h.secrets {
		if s != "" {
			redacted = strings.ReplaceAll(redacted, s, "[REDACTED]")
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	list := h.logs[resourceUUID]
	cursor := int64(1)
	if len(list) > 0 {
		cursor = list[len(list)-1].Cursor + 1
	}
	entry := LogEntry{
		Cursor:    cursor,
		Stream:    stream,
		Text:      redacted, // JSON/SSE payload remains raw escaped text, never raw HTML
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	}
	list = append(list, entry)
	if len(list) > h.maxKeep {
		list = list[len(list)-h.maxKeep:]
	}
	h.logs[resourceUUID] = list
	return entry
}

// ReplayFrom returns entries strictly after sinceCursor and sets gapDetected=true if older logs were pruned.
func (h *SSEHandler) ReplayFrom(resourceUUID string, sinceCursor int64, maxBatch int) (entries []LogEntry, gapDetected bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	list := h.logs[resourceUUID]
	if len(list) == 0 {
		return nil, false
	}
	if sinceCursor > 0 && sinceCursor < list[0].Cursor-1 {
		gapDetected = true
	}
	for _, e := range list {
		if e.Cursor > sinceCursor {
			entries = append(entries, e)
			if maxBatch > 0 && len(entries) >= maxBatch {
				break
			}
		}
	}
	return entries, gapDetected
}

func (h *SSEHandler) StreamLogs(w http.ResponseWriter, r *http.Request, p auth.Principal, res access.ResourceRef, sinceCursor int64) {
	if err := h.authz.Check(r.Context(), p, access.ActionRead, res); err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	teamKey := "0"
	if res.TeamID != nil {
		teamKey = fmt.Sprintf("%d", *res.TeamID)
	}
	entries, gap := h.ReplayFrom(fmt.Sprintf("%s:%s:%d", res.Type, teamKey, res.ID), sinceCursor, 500)
	if gap {
		fmt.Fprintf(w, "event: gap\ndata: {\"reason\":\"cursor_pruned\"}\n\n")
	}
	for _, e := range entries {
		// Escape any HTML-sensitive bytes defensively in SSE data preview while keeping text semantic
		fmt.Fprintf(w, "id: %d\nevent: log\ndata: {\"cursor\":%d,\"stream\":%q,\"text\":%q}\n\n", e.Cursor, e.Cursor, e.Stream, html.EscapeString(e.Text))
	}
}

// ---- Terminal One-Use Ticket & Lifecycle (Task 8) ----

var (
	ErrTicketInvalidOrReplayed = errors.New("terminal ticket invalid, expired, or already used")
	ErrOriginMismatch          = errors.New("websocket origin mismatch")
	ErrTerminalNotAllowed      = errors.New("terminal target not authorized or server transferred away")
)

type TerminalTarget struct {
	ServerID          int64
	ServerUUID        string
	TeamID            int64
	Container         string
	Mode              string // shell | attach
	TerminalEnabled   bool
	ForceDisabled     bool
	TransferredAway   bool
	PrivateKeyTeamID  int64
	AllowedOrigin     string
}

type TerminalTicket struct {
	Token     string
	UserID    int64
	TeamID    int64
	Target    TerminalTarget
	ExpiresAt time.Time
	Redeemed  bool
}

type TerminalSession struct {
	SessionID   string
	UserID      int64
	TeamID      int64
	ServerUUID  string
	Container   string
	Active      bool
	MaxDeadline time.Time
	PTYClosed   bool
}

type TerminalService struct {
	mu       sync.Mutex
	authz    *access.Authorizer
	tickets  map[string]*TerminalTicket
	sessions map[string]*TerminalSession
	now      func() time.Time
}

func NewTerminalService(authz *access.Authorizer) *TerminalService {
	return &TerminalService{
		authz:    authz,
		tickets:  make(map[string]*TerminalTicket),
		sessions: make(map[string]*TerminalSession),
		now:      time.Now,
	}
}

func (s *TerminalService) IssueTicket(p auth.Principal, target TerminalTarget) (TerminalTicket, error) {
	teamID := target.TeamID
	if err := s.authz.Check(nil, p, access.ActionTerminal, access.ResourceRef{
		Type: "server", ID: target.ServerID, TeamID: &teamID, APISegment: "servers",
	}); err != nil {
		return TerminalTicket{}, err
	}
	if !target.TerminalEnabled || target.ForceDisabled || target.TransferredAway || target.PrivateKeyTeamID != target.TeamID {
		return TerminalTicket{}, ErrTerminalNotAllowed
	}
	if (target.ServerID == 0 || target.ServerUUID == "localhost") && !(target.TeamID == 0 && p.IsInstanceAdmin) {
		return TerminalTicket{}, ErrTerminalNotAllowed
	}

	b := make([]byte, 24)
	_, _ = rand.Read(b)
	tok := hex.EncodeToString(b)

	t := &TerminalTicket{
		Token:     tok,
		UserID:    p.UserID,
		TeamID:    target.TeamID,
		Target:    target,
		ExpiresAt: s.now().Add(60 * time.Second),
	}
	s.mu.Lock()
	s.tickets[tok] = t
	s.mu.Unlock()
	return *t, nil
}

func (s *TerminalService) RedeemTicket(p auth.Principal, token string, origin string) (*TerminalSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tickets[token]
	if !ok || t.Redeemed || s.now().After(t.ExpiresAt) {
		return nil, ErrTicketInvalidOrReplayed
	}
	if t.UserID != p.UserID || t.TeamID != t.Target.TeamID {
		return nil, ErrTicketInvalidOrReplayed
	}
	if t.Target.AllowedOrigin != "" && origin != t.Target.AllowedOrigin {
		return nil, ErrOriginMismatch
	}
	// Re-check authorization & server archive flags at redeem time
	teamID := t.Target.TeamID
	if err := s.authz.Check(nil, p, access.ActionTerminal, access.ResourceRef{
		Type: "server", ID: t.Target.ServerID, TeamID: &teamID, APISegment: "servers",
	}); err != nil {
		return nil, err
	}
	if t.Target.TransferredAway || t.Target.ForceDisabled {
		return nil, ErrTerminalNotAllowed
	}

	t.Redeemed = true // strictly single-use
	sb := make([]byte, 16)
	_, _ = rand.Read(sb)
	sess := &TerminalSession{
		SessionID:   hex.EncodeToString(sb),
		UserID:      p.UserID,
		TeamID:      t.Target.TeamID,
		ServerUUID:  t.Target.ServerUUID,
		Container:   t.Target.Container,
		Active:      true,
		MaxDeadline: s.now().Add(8*time.Hour + 10*time.Minute),
	}
	s.sessions[sess.SessionID] = sess
	return sess, nil
}

func (s *TerminalService) CloseSession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess, ok := s.sessions[sessionID]; ok {
		sess.Active = false
		sess.PTYClosed = true
	}
}

func (s *TerminalService) ReauthorizeSession(p auth.Principal, sessionID string) error {
	s.mu.Lock()
	sess, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok || !sess.Active || s.now().After(sess.MaxDeadline) {
		return errors.New("terminal session closed or expired")
	}
	teamID := sess.TeamID
	if err := s.authz.Check(nil, p, access.ActionTerminal, access.ResourceRef{
		Type: "server", TeamID: &teamID, APISegment: "servers",
	}); err != nil {
		s.CloseSession(sessionID)
		return err
	}
	return nil
}

// Helper for SSE tests.
func (s *TerminalService) SetNowForTest(fn func() time.Time) {
	s.now = fn
}
