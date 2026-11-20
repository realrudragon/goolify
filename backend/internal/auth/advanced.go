package auth

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalidTOTP          = errors.New("invalid TOTP code or recovery code")
	ErrInvalidWebAuthn      = errors.New("webauthn origin, RP ID, or challenge verification failed")
	ErrInvalidOAuthState    = errors.New("oauth state mismatch or replay")
	ErrInvalidInvitation    = errors.New("invitation invalid or expired")
	ErrForcePasswordReset   = errors.New("password reset required before continuing")
)

type UserAuthRecord struct {
	UserID              int64
	Email               string
	PasswordHash        string
	TOTPSecretBase32    string
	RecoveryCodes       []string
	TwoFactorConfirmed  bool
	EmailVerified       bool
	ForcePasswordReset  bool
	WebAuthnCredentialID string
	WebAuthnOrigin      string
	WebAuthnRPID        string
}

type InvitationRecord struct {
	Token     string
	TeamID    int64
	Email     string
	Role      Role
	ExpiresAt time.Time
	Used      bool
}

type AuthFlowService struct {
	mu          sync.Mutex
	users       map[int64]*UserAuthRecord
	oauthStates map[string]string // state -> provider
	challenges  map[string]int64  // challengeID -> userID
	invites     map[string]*InvitationRecord
	now         func() time.Time
}

func NewAuthFlowService() *AuthFlowService {
	return &AuthFlowService{
		users:       make(map[int64]*UserAuthRecord),
		oauthStates: make(map[string]string),
		challenges:  make(map[string]int64),
		invites:     make(map[string]*InvitationRecord),
		now:         time.Now,
	}
}

func (s *AuthFlowService) RegisterUser(rec UserAuthRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec.Email = strings.ToLower(strings.TrimSpace(rec.Email))
	s.users[rec.UserID] = &rec
}

func (s *AuthFlowService) EnsureRootBootstrap(email string, passwordHash string) UserAuthRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, exists := s.users[0]; exists {
		return *u
	}
	root := &UserAuthRecord{
		UserID:        0, // Instance sentinel id = 0
		Email:         strings.ToLower(strings.TrimSpace(email)),
		PasswordHash:  passwordHash,
		EmailVerified: true,
	}
	s.users[0] = root
	return *root
}

// ComputeTOTP generates an RFC 6238 6-digit TOTP code for testing and verification.
func ComputeTOTP(secretBase32 string, ts time.Time) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimSpace(secretBase32)))
	if err != nil {
		return "", err
	}
	counter := uint64(ts.Unix() / 30)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], counter)
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := (uint32(sum[offset]&0x7f) << 24) |
		(uint32(sum[offset+1]) << 16) |
		(uint32(sum[offset+2]) << 8) |
		uint32(sum[offset+3])
	return fmt.Sprintf("%06d", code%1000000), nil
}

func (s *AuthFlowService) VerifyTOTP(userID int64, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[userID]
	if !ok {
		return ErrInvalidTOTP
	}
	if u.ForcePasswordReset {
		return ErrForcePasswordReset
	}
	now := s.now().UTC()
	for _, delta := range []time.Duration{0, -30 * time.Second, 30 * time.Second} {
		expected, err := ComputeTOTP(u.TOTPSecretBase32, now.Add(delta))
		if err == nil && hmac.Equal([]byte(expected), []byte(strings.TrimSpace(code))) {
			return nil
		}
	}
	for i, rc := range u.RecoveryCodes {
		if rc != "" && hmac.Equal([]byte(rc), []byte(strings.TrimSpace(code))) {
			u.RecoveryCodes[i] = "" // consume one-time recovery code
			return nil
		}
	}
	return ErrInvalidTOTP
}

func (s *AuthFlowService) BeginWebAuthn(challengeID string, userID int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.challenges[challengeID] = userID
}

func (s *AuthFlowService) FinishWebAuthnLogin(challengeID string, origin string, rpID string, credentialID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	uid, ok := s.challenges[challengeID]
	if !ok {
		return 0, ErrInvalidWebAuthn
	}
	delete(s.challenges, challengeID)
	u, ok := s.users[uid]
	if !ok || u.WebAuthnOrigin != origin || u.WebAuthnRPID != rpID || u.WebAuthnCredentialID != credentialID {
		return 0, ErrInvalidWebAuthn
	}
	return uid, nil
}

func (s *AuthFlowService) RegisterOAuthState(state string, provider string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.oauthStates[state] = provider
}

func (s *AuthFlowService) HandleOAuthCallback(provider string, state string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	expected, ok := s.oauthStates[state]
	if !ok || expected != provider {
		return ErrInvalidOAuthState
	}
	delete(s.oauthStates, state) // single-use state
	return nil
}

func (s *AuthFlowService) CreateInvitation(inv InvitationRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv.Email = strings.ToLower(strings.TrimSpace(inv.Email))
	s.invites[inv.Token] = &inv
}

func (s *AuthFlowService) AcceptInvitation(token string, userEmail string) (InvitationRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, ok := s.invites[token]
	if !ok || inv.Used || s.now().After(inv.ExpiresAt) || inv.Email != strings.ToLower(strings.TrimSpace(userEmail)) {
		return InvitationRecord{}, ErrInvalidInvitation
	}
	inv.Used = true
	return *inv, nil
}
