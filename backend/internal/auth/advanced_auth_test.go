package auth_test

import (
	"errors"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/auth"
)

func TestAuthFlowService_2FAPasskeyOAuthInvitations(t *testing.T) {
	svc := auth.NewAuthFlowService()
	root := svc.EnsureRootBootstrap("Admin@Example.com", "$2y$10$hash")
	if root.UserID != 0 || root.Email != "admin@example.com" {
		t.Fatalf("unexpected root bootstrap sentinel: %+v", root)
	}

	secret := "JBSWY3DPEHPK3PXP"
	svc.RegisterUser(auth.UserAuthRecord{
		UserID:               42,
		Email:                "dev@example.com",
		TOTPSecretBase32:     secret,
		RecoveryCodes:        []string{"rec-code-1", "rec-code-2"},
		TwoFactorConfirmed:   true,
		WebAuthnCredentialID: "cred-abc",
		WebAuthnOrigin:       "https://coolify.local",
		WebAuthnRPID:         "coolify.local",
	})

	// TOTP code verifies and recovery code is single-use
	code, err := auth.ComputeTOTP(secret, time.Now().UTC())
	if err != nil {
		t.Fatalf("ComputeTOTP: %v", err)
	}
	if err := svc.VerifyTOTP(42, code); err != nil {
		t.Fatalf("VerifyTOTP: %v", err)
	}
	if err := svc.VerifyTOTP(42, "rec-code-1"); err != nil {
		t.Fatalf("VerifyTOTP recovery code: %v", err)
	}
	if err := svc.VerifyTOTP(42, "rec-code-1"); !errors.Is(err, auth.ErrInvalidTOTP) {
		t.Fatalf("expected recovery code replay rejection, got %v", err)
	}

	// WebAuthn origin/RP-ID enforcement
	svc.BeginWebAuthn("ch-1", 42)
	if _, err := svc.FinishWebAuthnLogin("ch-1", "https://phishing.example", "coolify.local", "cred-abc"); !errors.Is(err, auth.ErrInvalidWebAuthn) {
		t.Fatalf("expected WebAuthn origin mismatch rejection, got %v", err)
	}
	svc.BeginWebAuthn("ch-2", 42)
	uid, err := svc.FinishWebAuthnLogin("ch-2", "https://coolify.local", "coolify.local", "cred-abc")
	if err != nil || uid != 42 {
		t.Fatalf("expected WebAuthn login pass, uid=%d err=%v", uid, err)
	}

	// OAuth state single-use replay check
	svc.RegisterOAuthState("state-1", "github")
	if err := svc.HandleOAuthCallback("github", "state-1"); err != nil {
		t.Fatalf("HandleOAuthCallback: %v", err)
	}
	if err := svc.HandleOAuthCallback("github", "state-1"); !errors.Is(err, auth.ErrInvalidOAuthState) {
		t.Fatalf("expected OAuth state replay fail, got %v", err)
	}

	// Invitation check
	svc.CreateInvitation(auth.InvitationRecord{
		Token: "inv-1", TeamID: 10, Email: "invitee@example.com", Role: auth.RoleMember, ExpiresAt: time.Now().Add(time.Hour),
	})
	if _, err := svc.AcceptInvitation("inv-1", "other@example.com"); !errors.Is(err, auth.ErrInvalidInvitation) {
		t.Fatalf("expected email mismatch rejection, got %v", err)
	}
	if _, err := svc.AcceptInvitation("inv-1", "invitee@example.com"); err != nil {
		t.Fatalf("AcceptInvitation: %v", err)
	}
}
