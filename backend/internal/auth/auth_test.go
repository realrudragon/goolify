package auth_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/auth"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
	"encoding/base64"
	"fmt"
)

type stubLookup struct {
	principals map[int64]auth.Principal
	tokens     map[int64]auth.TokenRecord
	apiOff     bool
}

func (s *stubLookup) LoadPrincipalByUserID(_ context.Context, userID int64, sessionTeamID int64) (auth.Principal, error) {
	p, ok := s.principals[userID]
	if !ok {
		return auth.Principal{}, auth.ErrUnauthenticated
	}
	p.SessionTeamID = sessionTeamID
	return p, nil
}

func (s *stubLookup) FindTokenByID(_ context.Context, tokenID int64) (auth.TokenRecord, error) {
	rec, ok := s.tokens[tokenID]
	if !ok {
		return auth.TokenRecord{}, auth.ErrTokenRevoked
	}
	return rec, nil
}

func (s *stubLookup) IsAPIDisabled(_ context.Context) (bool, error) {
	return s.apiOff, nil
}

func TestPasswordVerifier_LaravelBcryptAndArgon2id(t *testing.T) {
	v := auth.NewPasswordVerifier()

	rawHash, err := bcrypt.GenerateFromPassword([]byte("synthetic-secret-pw"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt gen: %v", err)
	}
	// Laravel stores bcrypt with $2y$ prefix.
	laravelBcrypt := "$2y$" + string(rawHash[4:])
	ok, err := v.Verify(laravelBcrypt, "synthetic-secret-pw")
	if err != nil || !ok {
		t.Fatalf("expected Laravel $2y$ bcrypt hash to verify, ok=%v err=%v", ok, err)
	}
	ok, err = v.Verify(laravelBcrypt, "wrong-pw")
	if err != nil || ok {
		t.Fatalf("expected wrong password to fail cleanly, ok=%v err=%v", ok, err)
	}

	// Synthetic Argon2id hash
	salt := []byte("0123456789abcdef")
	dk := argon2.IDKey([]byte("argon-pw"), salt, 1, 65536, 1, 32)
	argonHash := fmt.Sprintf("$argon2id$v=19$m=65536,t=1,p=1$%s$%s",
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk))
	ok, err = v.Verify(argonHash, "argon-pw")
	if err != nil || !ok {
		t.Fatalf("expected Argon2id hash to verify, ok=%v err=%v", ok, err)
	}
}

func TestAuthenticator_SanctumBearerAbilitiesAndAPIDisabled(t *testing.T) {
	team10 := int64(10)
	secret := "synthetic-token-secret"
	lookup := &stubLookup{
		principals: map[int64]auth.Principal{
			5: {UserID: 5, TeamRoles: map[int64]auth.Role{10: auth.RoleAdmin}},
		},
		tokens: map[int64]auth.TokenRecord{
			1: {
				ID:         1,
				UserID:     5,
				TeamID:     &team10,
				HashSHA256: auth.HashSanctumToken(secret),
				Abilities:  []string{"read", "deploy"},
				WholeTeam:  false, // scoped access token
			},
		},
	}
	a := auth.NewAuthenticator(lookup, auth.NewMemorySessionStore())
	ctx := context.Background()

	p, err := a.AuthenticateBearer(ctx, "Bearer "+auth.FormatSanctumToken(1, secret))
	if err != nil {
		t.Fatalf("AuthenticateBearer failed: %v", err)
	}
	if !auth.HasAbility(p, "applications", "deploy") {
		t.Fatal("expected scoped token to have deploy ability on applications")
	}
	if auth.HasAbility(p, "servers", "read") {
		t.Fatal("expected scoped token to be denied on team-level servers endpoint")
	}

	// API disabled blocks REST bearer auth, while MCP still works per SECURITY.md
	lookup.apiOff = true
	if _, err := a.AuthenticateBearer(ctx, auth.FormatSanctumToken(1, secret)); !errors.Is(err, auth.ErrAPIDisabled) {
		t.Fatalf("expected ErrAPIDisabled, got %v", err)
	}
	if _, err := a.AuthenticateMCP(ctx, auth.FormatSanctumToken(1, secret)); err != nil {
		t.Fatalf("expected MCP auth to work when REST API disabled, got %v", err)
	}
}

func TestSession_CSRFAndRevocation(t *testing.T) {
	lookup := &stubLookup{
		principals: map[int64]auth.Principal{
			1: {UserID: 1, TeamRoles: map[int64]auth.Role{10: auth.RoleOwner}},
		},
	}
	store := auth.NewMemorySessionStore()
	a := auth.NewAuthenticator(lookup, store)
	ctx := context.Background()

	sess, err := a.IssueSession(ctx, 1, 10, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	// POST without CSRF fails
	reqBad := httptest.NewRequest(http.MethodPost, "/app/servers", nil)
	reqBad.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.ID})
	if _, err := a.AuthenticateSession(ctx, reqBad); !errors.Is(err, auth.ErrCSRFMismatch) {
		t.Fatalf("expected ErrCSRFMismatch, got %v", err)
	}

	// POST with valid CSRF passes
	reqOK := httptest.NewRequest(http.MethodPost, "/app/servers", nil)
	reqOK.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.ID})
	reqOK.Header.Set("X-CSRF-TOKEN", sess.CSRFToken)
	if _, err := a.AuthenticateSession(ctx, reqOK); err != nil {
		t.Fatalf("expected valid session+CSRF to pass, got %v", err)
	}

	// Revocation fails closed
	_ = store.RevokeAllForUser(ctx, 1)
	if _, err := a.AuthenticateSession(ctx, reqOK); !errors.Is(err, auth.ErrUnauthenticated) {
		t.Fatalf("expected revoked session to fail closed, got %v", err)
	}
}
