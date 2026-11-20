package realtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/realtime"
)

func TestSSE_SlowConsumerBoundedAndCursorGap(t *testing.T) {
	// Review Focus #4: bounded SSE buffer, cursor gap notification, secret redaction, and non-blocking slow consumer
	hub := realtime.NewRealtimeHub(2) // small buffer
	ch, cancel := hub.Subscribe(10)
	defer cancel()

	for i := int64(1); i <= 5; i++ {
		hub.Publish(context.Background(), realtime.DomainEvent{
			EventID: "ev", TeamID: 10, AggregateRevision: i,
		})
	}
	if len(ch) != 2 {
		t.Fatalf("expected bounded buffer cap of 2, got %d", len(ch))
	}

	sse := realtime.NewSSEHandler(access.NewAuthorizer(), 3, "db-password-999")
	sse.AppendLog("app-1", "stdout", "line 1 secret=db-password-999")
	sse.AppendLog("app-1", "stdout", "line 2 <script>alert(1)</script>")
	sse.AppendLog("app-1", "stdout", "line 3")
	sse.AppendLog("app-1", "stdout", "line 4")
	sse.AppendLog("app-1", "stdout", "line 5") // evicts cursors 1 and 2; retained [3,4,5]

	entries, gap := sse.ReplayFrom("app-1", 1, 10)
	if !gap || len(entries) != 3 {
		t.Fatalf("expected cursor gap=true and 3 entries, got gap=%v len=%d", gap, len(entries))
	}

	entriesAll, _ := sse.ReplayFrom("app-1", 0, 10)
	if entriesAll[0].Text != "line 3" {
		t.Fatalf("unexpected oldest retained log text: %q", entriesAll[0].Text)
	}
	sseXSS := realtime.NewSSEHandler(access.NewAuthorizer(), 10)
	xssEntry := sseXSS.AppendLog("app-xss", "stdout", "line <script>alert(1)</script>")
	if xssEntry.Text != "line <script>alert(1)</script>" {
		t.Fatalf("unexpected raw text preservation: %q", xssEntry.Text)
	}
	// Secret redaction check on first log (tested separately with larger capacity)
	sse2 := realtime.NewSSEHandler(access.NewAuthorizer(), 10, "db-password-999")
	e := sse2.AppendLog("app-1", "stdout", "connecting with db-password-999")
	if e.Text != "connecting with [REDACTED]" {
		t.Fatalf("secret not redacted from log stream: %q", e.Text)
	}
}

func TestTerminal_ExpiredReplayedTicketAndTransferredServer(t *testing.T) {
	// Review Focus #4: expired ticket, replay protection, origin mismatch, transferred-away block, and PTY cleanup
	tsvc := realtime.NewTerminalService(access.NewAuthorizer())
	now := time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)
	tsvc.SetNowForTest(func() time.Time { return now })

	admin := auth.Principal{UserID: 1, AuthMethod: "session", TeamRoles: map[int64]auth.Role{10: auth.RoleAdmin}}
	target := realtime.TerminalTarget{
		ServerID: 5, ServerUUID: "srv-5", TeamID: 10, Container: "web-1", Mode: "shell",
		TerminalEnabled: true, PrivateKeyTeamID: 10, AllowedOrigin: "https://coolify.local",
	}

	ticket, err := tsvc.IssueTicket(admin, target)
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}

	// Origin mismatch rejected without consuming ticket
	if _, err := tsvc.RedeemTicket(admin, ticket.Token, "https://evil.example"); !errors.Is(err, realtime.ErrOriginMismatch) {
		t.Fatalf("expected ErrOriginMismatch, got %v", err)
	}

	// Valid redeem succeeds
	sess, err := tsvc.RedeemTicket(admin, ticket.Token, "https://coolify.local")
	if err != nil || !sess.Active {
		t.Fatalf("RedeemTicket failed: %v", err)
	}

	// Replay of same ticket fails
	if _, err := tsvc.RedeemTicket(admin, ticket.Token, "https://coolify.local"); !errors.Is(err, realtime.ErrTicketInvalidOrReplayed) {
		t.Fatalf("expected replay ErrTicketInvalidOrReplayed, got %v", err)
	}

	// Expired ticket fails
	t2, _ := tsvc.IssueTicket(admin, target)
	now = now.Add(61 * time.Second)
	if _, err := tsvc.RedeemTicket(admin, t2.Token, "https://coolify.local"); !errors.Is(err, realtime.ErrTicketInvalidOrReplayed) {
		t.Fatalf("expected expired ErrTicketInvalidOrReplayed, got %v", err)
	}

	// Transferred-away server cannot issue terminal ticket
	target.TransferredAway = true
	if _, err := tsvc.IssueTicket(admin, target); !errors.Is(err, realtime.ErrTerminalNotAllowed) {
		t.Fatalf("expected ErrTerminalNotAllowed for transferred-away server, got %v", err)
	}

	tsvc.CloseSession(sess.SessionID)
	if !sess.PTYClosed {
		t.Fatal("expected PTYClosed=true on CloseSession")
	}
}

func TestRealtime_StreamRevokesOnSessionSwitchOrGrantLoss(t *testing.T) {
	// Review Focus #1: member loses grant or switches session team without holding team admin role
	tsvc := realtime.NewTerminalService(access.NewAuthorizer())
	p := auth.Principal{
		UserID: 9, AuthMethod: "session", SessionTeamID: 20,
		TeamRoles: map[int64]auth.Role{10: auth.RoleAdmin, 20: auth.RoleOwner},
	}
	target := realtime.TerminalTarget{
		ServerID: 5, ServerUUID: "srv-5", TeamID: 10,
		TerminalEnabled: true, PrivateKeyTeamID: 10, AllowedOrigin: "https://coolify.local",
	}
	ticket, err := tsvc.IssueTicket(p, target)
	if err != nil {
		t.Fatalf("IssueTicket: %v", err)
	}
	sess, err := tsvc.RedeemTicket(p, ticket.Token, "https://coolify.local")
	if err != nil {
		t.Fatalf("RedeemTicket: %v", err)
	}

	// Demote user in resource team 10 (even though still owner in session team 20!) -> closes PTY session!
	p.TeamRoles[10] = auth.RoleMember
	if err := tsvc.ReauthorizeSession(p, sess.SessionID); err == nil {
		t.Fatal("expected terminal session to fail reauthorization when resource team role demoted")
	}
	if !sess.PTYClosed || sess.Active {
		t.Fatal("expected active PTY session to be forcefully closed on grant revocation")
	}
}
