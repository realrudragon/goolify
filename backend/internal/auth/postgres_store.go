package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

// PostgresSessionStore persists native Go HTTP sessions in the additive `go_sessions` table.
type PostgresSessionStore struct {
	pool *pgxpool.Pool
}

func NewPostgresSessionStore(pool *pgxpool.Pool) *PostgresSessionStore {
	return &PostgresSessionStore{pool: pool}
}

func (s *PostgresSessionStore) Get(ctx context.Context, sessionID string) (SessionData, error) {
	var sess SessionData
	err := s.pool.QueryRow(ctx, `
		SELECT id, user_id, team_id, csrf_token, expires_at, revoked, two_fa_valid
		FROM go_sessions
		WHERE id = $1
	`, sessionID).Scan(
		&sess.ID, &sess.UserID, &sess.TeamID, &sess.CSRFToken,
		&sess.ExpiresAt, &sess.Revoked, &sess.TwoFAValid,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SessionData{}, ErrUnauthenticated
	}
	if err != nil {
		return SessionData{}, fmt.Errorf("load session: %w", err)
	}
	return sess, nil
}

func (s *PostgresSessionStore) Save(ctx context.Context, sess SessionData) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO go_sessions (id, user_id, team_id, csrf_token, expires_at, revoked, two_fa_valid, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (id) DO UPDATE SET
			team_id = EXCLUDED.team_id,
			csrf_token = EXCLUDED.csrf_token,
			expires_at = EXCLUDED.expires_at,
			revoked = EXCLUDED.revoked,
			two_fa_valid = EXCLUDED.two_fa_valid,
			updated_at = NOW()
	`, sess.ID, sess.UserID, sess.TeamID, sess.CSRFToken, sess.ExpiresAt, sess.Revoked, sess.TwoFAValid)
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}
	return nil
}

func (s *PostgresSessionStore) Revoke(ctx context.Context, sessionID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE go_sessions SET revoked = TRUE, updated_at = NOW() WHERE id = $1`, sessionID)
	return err
}

func (s *PostgresSessionStore) RevokeAllForUser(ctx context.Context, userID int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE go_sessions SET revoked = TRUE, updated_at = NOW() WHERE user_id = $1`, userID)
	return err
}

// PostgresPrincipalLookup reads authoritative user/team/grant/token/settings state directly from Coolify's PostgreSQL schema.
type PostgresPrincipalLookup struct {
	pool *pgxpool.Pool
}

func NewPostgresPrincipalLookup(pool *pgxpool.Pool) *PostgresPrincipalLookup {
	return &PostgresPrincipalLookup{pool: pool}
}

type rawAccessGrant struct {
	Scope  string   `json:"scope"`
	UUID   *string  `json:"uuid"`
	Levels []string `json:"levels"`
}

func (l *PostgresPrincipalLookup) LoadPrincipalByUserID(ctx context.Context, userID int64, sessionTeamID int64) (Principal, error) {
	var email string
	var forceReset bool
	err := l.pool.QueryRow(ctx, `
		SELECT email, COALESCE(force_password_reset, FALSE)
		FROM users
		WHERE id = $1
	`, userID).Scan(&email, &forceReset)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}

	p := Principal{
		UserID:         userID,
		Email:          email,
		SessionTeamID:  sessionTeamID,
		TeamRoles:      make(map[int64]Role),
		TeamWideAccess: make(map[int64]bool),
		TeamWideLevels: make(map[int64]map[AccessLevel]bool),
		ForceReset:     forceReset,
	}

	rows, err := l.pool.Query(ctx, `
		SELECT team_id, role, access
		FROM team_user
		WHERE user_id = $1
		ORDER BY team_id ASC
	`, userID)
	if err != nil {
		return Principal{}, fmt.Errorf("load team memberships: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var tid int64
		var roleStr string
		var accessJSON []byte
		if err := rows.Scan(&tid, &roleStr, &accessJSON); err != nil {
			return Principal{}, err
		}
		role := Role(strings.ToLower(strings.TrimSpace(roleStr)))
		p.TeamRoles[tid] = role
		if tid == 0 && role.AtLeast(RoleAdmin) {
			p.IsInstanceAdmin = true
		}
		if role.AtLeast(RoleAdmin) {
			p.TeamWideAccess[tid] = true
			p.TeamWideLevels[tid] = ExpandAccessLevels([]string{string(AccessRoot)})
			continue
		}
		// Match App\Services\Access\AccessResolver:
		// A member with NULL access gets default team-wide read ONLY (NOT write/deploy/credentials).
		if len(accessJSON) == 0 || string(accessJSON) == "null" {
			p.TeamWideLevels[tid] = ExpandAccessLevels([]string{string(AccessRead)})
			continue
		}
		var stored []rawAccessGrant
		if err := json.Unmarshal(accessJSON, &stored); err != nil {
			continue // fail closed on corrupt JSON
		}
		for _, g := range stored {
			expanded := ExpandAccessLevels(g.Levels)
			lvl := AccessRead
			switch {
			case expanded[AccessRoot]:
				lvl = AccessRoot
			case expanded[AccessWrite]:
				lvl = AccessWrite
			case expanded[AccessDelete]:
				lvl = AccessDelete
			case expanded[AccessDeploy]:
				lvl = AccessDeploy
			case expanded[AccessReadSensitive]:
				lvl = AccessReadSensitive
			}
			if g.Scope == "team" {
				p.TeamWideLevels[tid] = MergeAccessLevels(p.TeamWideLevels[tid], g.Levels)
				if p.TeamWideLevels[tid][AccessWrite] {
					p.TeamWideAccess[tid] = true
				}
			}
			uuidVal := ""
			if g.UUID != nil {
				uuidVal = *g.UUID
			}
			p.ResourceGrants = append(p.ResourceGrants, ResourceGrant{
				Scope:        g.Scope,
				ResourceUUID: uuidVal,
				TeamID:       tid,
				Level:        lvl,
				Levels:       g.Levels,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return Principal{}, err
	}
	return p, nil
}

func (l *PostgresPrincipalLookup) FindTokenByID(ctx context.Context, tokenID int64) (TokenRecord, error) {
	var rec TokenRecord
	var teamIDStr string
	var abilitiesRaw *string
	var scopesRaw []byte
	var expiresAt *time.Time
	var pausedAt *time.Time
	var lastUsedAt *time.Time

	err := l.pool.QueryRow(ctx, `
		SELECT id, tokenable_id, token, COALESCE(team_id, '0'), abilities, scopes, expires_at, paused_at, last_used_at
		FROM personal_access_tokens
		WHERE id = $1
	`, tokenID).Scan(
		&rec.ID, &rec.UserID, &rec.HashSHA256, &teamIDStr,
		&abilitiesRaw, &scopesRaw, &expiresAt, &pausedAt, &lastUsedAt,
	)
	if err != nil {
		return TokenRecord{}, ErrTokenRevoked
	}
	if tid, err := strconv.ParseInt(strings.TrimSpace(teamIDStr), 10, 64); err == nil {
		rec.TeamID = &tid
	}
	if abilitiesRaw != nil && *abilitiesRaw != "" {
		_ = json.Unmarshal([]byte(*abilitiesRaw), &rec.Abilities)
	}
	rec.ExpiresAt = expiresAt
	rec.Revoked = pausedAt != nil
	// A token with NULL scopes covers the whole team; non-null scopes restrict it to selected resources unless a team scope is present.
	if len(scopesRaw) == 0 || string(scopesRaw) == "null" {
		rec.WholeTeam = true
	} else {
		rec.HasScopes = true
		var scopes []rawAccessGrant
		if err := json.Unmarshal(scopesRaw, &scopes); err == nil {
			for _, sc := range scopes {
				if sc.Scope == "team" {
					rec.WholeTeam = true
				}
				uuidVal := ""
				if sc.UUID != nil {
					uuidVal = *sc.UUID
				}
				rec.Scopes = append(rec.Scopes, ResourceGrant{
					Scope:        sc.Scope,
					ResourceUUID: uuidVal,
					Levels:       append([]string(nil), rec.Abilities...),
				})
			}
		}
	}

	// Best-effort last_used_at update (throttled to once per minute to eliminate write amplification on read-heavy API traffic)
	if lastUsedAt == nil || time.Since(*lastUsedAt) >= time.Minute {
		_, _ = l.pool.Exec(ctx, `UPDATE personal_access_tokens SET last_used_at = NOW() WHERE id = $1`, tokenID)
	}
	return rec, nil
}

func (l *PostgresPrincipalLookup) IsAPIDisabled(ctx context.Context) (bool, error) {
	var enabled bool
	err := l.pool.QueryRow(ctx, `SELECT COALESCE(is_api_enabled, TRUE) FROM instance_settings WHERE id = 0`).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return true, err // fail closed
	}
	return !enabled, nil
}

// IsMCPServerEnabled checks instance_settings.is_mcp_server_enabled independently from REST API access (per SECURITY.md).
func (l *PostgresPrincipalLookup) IsMCPServerEnabled(ctx context.Context) (bool, error) {
	var enabled bool
	err := l.pool.QueryRow(ctx, `SELECT COALESCE(is_mcp_server_enabled, FALSE) FROM instance_settings WHERE id = 0`).Scan(&enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return enabled, nil
}

// PostgresAuthRepository handles user credential verification, registration, 2FA, passkeys, OAuth, invitations, and API tokens.
type PostgresAuthRepository struct {
	pool     *pgxpool.Pool
	verifier *PasswordVerifier
	cipher   *compat.Cipher
}

func NewPostgresAuthRepository(pool *pgxpool.Pool, cipher ...*compat.Cipher) *PostgresAuthRepository {
	var c *compat.Cipher
	if len(cipher) > 0 {
		c = cipher[0]
	}
	return &PostgresAuthRepository{
		pool:     pool,
		verifier: NewPasswordVerifier(),
		cipher:   c,
	}
}

type StoredUser struct {
	ID                 int64
	Name               string
	Email              string
	PasswordHash       string
	ForcePasswordReset bool
	TOTPSecret         string
	RecoveryCodes      []string
	TwoFactorConfirmed bool
}

func looksLikeLaravelEnvelope(raw string) bool {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	var env struct {
		IV    string `json:"iv"`
		Value string `json:"value"`
		MAC   string `json:"mac"`
	}
	return json.Unmarshal(decoded, &env) == nil && env.IV != "" && env.Value != "" && env.MAC != ""
}

func (r *PostgresAuthRepository) decryptFortifyField(raw string) (string, bool, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false, nil
	}
	if looksLikeLaravelEnvelope(trimmed) {
		if r.cipher == nil {
			return "", true, ErrInvalidTOTP
		}
		plain, err := r.cipher.DecryptLaravelPayload(trimmed, true)
		if errors.Is(err, compat.ErrInvalidPHPSerialize) {
			plain, err = r.cipher.DecryptLaravelPayload(trimmed, false)
		}
		if err != nil {
			return "", true, ErrInvalidTOTP
		}
		return string(plain), true, nil
	}
	return trimmed, false, nil
}

func (r *PostgresAuthRepository) AuthenticateCredentials(ctx context.Context, email string, password string, totpCode string) (StoredUser, int64, error) {
	normalized := strings.ToLower(strings.TrimSpace(email))
	var u StoredUser
	var totpSecret *string
	var recoveryRaw *string
	var confirmedAt *time.Time

	err := r.pool.QueryRow(ctx, `
		SELECT id, name, email, password, COALESCE(force_password_reset, FALSE),
		       two_factor_secret, two_factor_recovery_codes, two_factor_confirmed_at
		FROM users
		WHERE LOWER(email) = $1
	`, normalized).Scan(
		&u.ID, &u.Name, &u.Email, &u.PasswordHash, &u.ForcePasswordReset,
		&totpSecret, &recoveryRaw, &confirmedAt,
	)
	if err != nil {
		return StoredUser{}, 0, ErrUnauthenticated
	}

	ok, err := r.verifier.Verify(u.PasswordHash, password)
	if err != nil || !ok {
		return StoredUser{}, 0, ErrUnauthenticated
	}

	var recoveryEncrypted bool
	if totpSecret != nil && strings.TrimSpace(*totpSecret) != "" {
		decSecret, _, decErr := r.decryptFortifyField(*totpSecret)
		if decErr != nil {
			return StoredUser{}, 0, ErrInvalidTOTP
		}
		u.TOTPSecret = decSecret
	}
	u.TwoFactorConfirmed = confirmedAt != nil && totpSecret != nil && strings.TrimSpace(*totpSecret) != ""
	if recoveryRaw != nil && strings.TrimSpace(*recoveryRaw) != "" {
		decCodes, wasEnc, decErr := r.decryptFortifyField(*recoveryRaw)
		if decErr != nil {
			if u.TwoFactorConfirmed {
				return StoredUser{}, 0, ErrInvalidTOTP
			}
		} else {
			recoveryEncrypted = wasEnc
			_ = json.Unmarshal([]byte(decCodes), &u.RecoveryCodes)
		}
	}

	if u.TwoFactorConfirmed {
		if strings.TrimSpace(totpCode) == "" {
			return StoredUser{}, 0, ErrInvalidTOTP
		}
		if err := r.verifyAndConsumeTOTP(ctx, &u, totpCode, recoveryEncrypted); err != nil {
			return StoredUser{}, 0, err
		}
	}

	// Pick default team (prefer root 0 if member, else lowest team_id)
	var defaultTeam int64
	err = r.pool.QueryRow(ctx, `SELECT team_id FROM team_user WHERE user_id = $1 ORDER BY team_id ASC LIMIT 1`, u.ID).Scan(&defaultTeam)
	if err != nil {
		defaultTeam = 0
	}
	return u, defaultTeam, nil
}

func (r *PostgresAuthRepository) verifyAndConsumeTOTP(ctx context.Context, u *StoredUser, code string, recoveryEncrypted bool) error {
	now := time.Now().UTC()
	trimmed := strings.TrimSpace(code)
	for _, offset := range []int64{0, -1, 1} {
		step := (now.Unix() / 30) + offset
		expected, err := ComputeTOTP(u.TOTPSecret, time.Unix(step*30, 0).UTC())
		if err == nil && hmac.Equal([]byte(expected), []byte(trimmed)) {
			tag, execErr := r.pool.Exec(ctx, `
				INSERT INTO go_totp_used_steps (user_id, time_step)
				VALUES ($1, $2)
				ON CONFLICT DO NOTHING
			`, u.ID, step)
			if execErr != nil || tag.RowsAffected() == 0 {
				return ErrInvalidTOTP
			}
			return nil
		}
	}

	// Atomic recovery code consume + re-encrypt with APP_KEY
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ErrInvalidTOTP
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var rawCodes *string
	if err := tx.QueryRow(ctx, `SELECT two_factor_recovery_codes FROM users WHERE id = $1 FOR UPDATE`, u.ID).Scan(&rawCodes); err != nil || rawCodes == nil {
		return ErrInvalidTOTP
	}
	decCodes, wasEnc, err := r.decryptFortifyField(*rawCodes)
	if err != nil {
		return ErrInvalidTOTP
	}
	var currentCodes []string
	if err := json.Unmarshal([]byte(decCodes), &currentCodes); err != nil {
		return ErrInvalidTOTP
	}
	matchIdx := -1
	for i, rc := range currentCodes {
		if rc != "" && hmac.Equal([]byte(rc), []byte(trimmed)) {
			matchIdx = i
			break
		}
	}
	if matchIdx < 0 {
		return ErrInvalidTOTP
	}

	remaining := make([]string, 0, len(currentCodes)-1)
	for i, rc := range currentCodes {
		if i != matchIdx && rc != "" {
			remaining = append(remaining, rc)
		}
	}
	updatedJSON, err := json.Marshal(remaining)
	if err != nil {
		return ErrInvalidTOTP
	}
	storedValue := string(updatedJSON)
	if r.cipher != nil {
		enc, encErr := r.cipher.EncryptLaravelPayload(updatedJSON, true)
		if encErr != nil {
			return ErrInvalidTOTP
		}
		storedValue = enc
	} else if recoveryEncrypted || wasEnc {
		// Never downgrade encrypted recovery codes to plaintext when cipher is missing
		return ErrInvalidTOTP
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET two_factor_recovery_codes = $1, updated_at = NOW() WHERE id = $2`, storedValue, u.ID); err != nil {
		return ErrInvalidTOTP
	}
	if err := tx.Commit(ctx); err != nil {
		return ErrInvalidTOTP
	}
	u.RecoveryCodes = remaining
	return nil
}

func (r *PostgresAuthRepository) ForceResetPassword(ctx context.Context, userID int64, currentPassword string, newPassword string) error {
	if len(newPassword) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	var currentHash string
	if err := r.pool.QueryRow(ctx, `SELECT password FROM users WHERE id = $1`, userID).Scan(&currentHash); err != nil {
		return ErrUnauthenticated
	}
	ok, err := r.verifier.Verify(currentHash, currentPassword)
	if err != nil || !ok {
		return ErrUnauthenticated
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = r.pool.Exec(ctx, `
		UPDATE users SET password = $1, force_password_reset = FALSE, updated_at = NOW() WHERE id = $2
	`, string(hash), userID)
	return err
}

func (r *PostgresAuthRepository) EnableTwoFactor(ctx context.Context, userID int64) (secretBase32 string, recoveryCodes []string, err error) {
	if r.cipher == nil {
		return "", nil, errors.New("cipher required to store encrypted 2FA secrets")
	}
	secBytes := make([]byte, 20)
	if _, err := rand.Read(secBytes); err != nil {
		return "", nil, err
	}
	secretBase32 = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secBytes)
	recoveryCodes = make([]string, 8)
	for i := range recoveryCodes {
		tok, err := randomHex(5)
		if err != nil {
			return "", nil, err
		}
		recoveryCodes[i] = strings.ToUpper(tok[:5] + "-" + tok[5:])
	}
	encSecret, err := r.cipher.EncryptLaravelPayload([]byte(secretBase32), true)
	if err != nil {
		return "", nil, err
	}
	codesJSON, _ := json.Marshal(recoveryCodes)
	encCodes, err := r.cipher.EncryptLaravelPayload(codesJSON, true)
	if err != nil {
		return "", nil, err
	}
	_, err = r.pool.Exec(ctx, `
		UPDATE users SET two_factor_secret = $1, two_factor_recovery_codes = $2, two_factor_confirmed_at = NULL, updated_at = NOW()
		WHERE id = $3
	`, encSecret, encCodes, userID)
	return secretBase32, recoveryCodes, err
}

func (r *PostgresAuthRepository) ConfirmTwoFactor(ctx context.Context, userID int64, totpCode string) error {
	var rawSecret *string
	if err := r.pool.QueryRow(ctx, `SELECT two_factor_secret FROM users WHERE id = $1`, userID).Scan(&rawSecret); err != nil || rawSecret == nil {
		return ErrInvalidTOTP
	}
	secret, _, err := r.decryptFortifyField(*rawSecret)
	if err != nil || secret == "" {
		return ErrInvalidTOTP
	}
	u := &StoredUser{ID: userID, TOTPSecret: secret}
	now := time.Now().UTC()
	trimmed := strings.TrimSpace(totpCode)
	for _, offset := range []int64{0, -1, 1} {
		step := (now.Unix() / 30) + offset
		expected, err := ComputeTOTP(u.TOTPSecret, time.Unix(step*30, 0).UTC())
		if err == nil && hmac.Equal([]byte(expected), []byte(trimmed)) {
			tag, execErr := r.pool.Exec(ctx, `
				INSERT INTO go_totp_used_steps (user_id, time_step) VALUES ($1, $2) ON CONFLICT DO NOTHING
			`, userID, step)
			if execErr != nil || tag.RowsAffected() == 0 {
				return ErrInvalidTOTP
			}
			_, err = r.pool.Exec(ctx, `UPDATE users SET two_factor_confirmed_at = NOW(), updated_at = NOW() WHERE id = $1`, userID)
			return err
		}
	}
	return ErrInvalidTOTP
}

func (r *PostgresAuthRepository) DisableTwoFactor(ctx context.Context, userID int64) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE users SET two_factor_secret = NULL, two_factor_recovery_codes = NULL, two_factor_confirmed_at = NULL, updated_at = NOW()
		WHERE id = $1
	`, userID)
	return err
}

func (r *PostgresAuthRepository) CreateUserWithTeam(ctx context.Context, name string, email string, plainPassword string, teamName string) (StoredUser, int64, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(plainPassword), bcrypt.DefaultCost)
	if err != nil {
		return StoredUser{}, 0, err
	}
	normalized := strings.ToLower(strings.TrimSpace(email))
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return StoredUser{}, 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO users (name, email, password, email_verified_at)
		VALUES ($1, $2, $3, NOW())
		RETURNING id
	`, strings.TrimSpace(name), normalized, string(hash)).Scan(&userID)
	if err != nil {
		return StoredUser{}, 0, fmt.Errorf("create user: %w", err)
	}

	if strings.TrimSpace(teamName) == "" {
		teamName = strings.TrimSpace(name) + "'s Team"
	}
	var teamID int64
	err = tx.QueryRow(ctx, `
		INSERT INTO teams (name, personal_team, show_boarding)
		VALUES ($1, TRUE, FALSE)
		RETURNING id
	`, teamName).Scan(&teamID)
	if err != nil {
		return StoredUser{}, 0, fmt.Errorf("create personal team: %w", err)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO team_user (team_id, user_id, role)
		VALUES ($1, $2, 'owner')
	`, teamID, userID); err != nil {
		return StoredUser{}, 0, fmt.Errorf("attach user to team: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return StoredUser{}, 0, err
	}
	return StoredUser{ID: userID, Name: name, Email: normalized, PasswordHash: string(hash)}, teamID, nil
}

func (r *PostgresAuthRepository) IssueAPIToken(ctx context.Context, userID int64, teamID int64, name string, abilities []string, scopesJSON []byte, expiresAt *time.Time) (int64, string, error) {
	secret, err := randomHex(20)
	if err != nil {
		return 0, "", err
	}
	hash := HashSanctumToken(secret)
	abJSON, err := json.Marshal(abilities)
	if err != nil {
		return 0, "", err
	}
	var id int64
	err = r.pool.QueryRow(ctx, `
		INSERT INTO personal_access_tokens (tokenable_type, tokenable_id, name, token, team_id, abilities, scopes, expires_at)
		VALUES ('App\Models\User', $1, $2, $3, $4, $5, $6, $7)
		RETURNING id
	`, userID, name, hash, strconv.FormatInt(teamID, 10), string(abJSON), nullableJSONB(scopesJSON), expiresAt).Scan(&id)
	if err != nil {
		return 0, "", fmt.Errorf("issue api token: %w", err)
	}
	return id, FormatSanctumToken(id, secret), nil
}

func (r *PostgresAuthRepository) RevokeAPIToken(ctx context.Context, tokenID int64, teamID int64) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM personal_access_tokens WHERE id = $1 AND team_id = $2`, tokenID, strconv.FormatInt(teamID, 10))
	return err
}

func (r *PostgresAuthRepository) ListAPITokens(ctx context.Context, teamID int64) ([]map[string]any, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, name, tokenable_id, abilities, last_used_at, expires_at, paused_at, created_at
		FROM personal_access_tokens
		WHERE team_id = $1
		ORDER BY id DESC
	`, strconv.FormatInt(teamID, 10))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id, uid int64
		var name string
		var abRaw *string
		var lastUsed, exp, paused *time.Time
		var created time.Time
		if err := rows.Scan(&id, &name, &uid, &abRaw, &lastUsed, &exp, &paused, &created); err != nil {
			return nil, err
		}
		var abs []string
		if abRaw != nil {
			_ = json.Unmarshal([]byte(*abRaw), &abs)
		}
		out = append(out, map[string]any{
			"id":           id,
			"name":         name,
			"user_id":      uid,
			"abilities":    abs,
			"last_used_at": lastUsed,
			"expires_at":   exp,
			"paused_at":    paused,
			"created_at":   created,
		})
	}
	return out, rows.Err()
}

func (r *PostgresAuthRepository) CreateInvitation(ctx context.Context, uuid string, teamID int64, email string, role Role) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO team_invitations (uuid, team_id, email, role, link, via)
		VALUES ($1, $2, $3, $4, $5, 'link')
	`, uuid, teamID, strings.ToLower(strings.TrimSpace(email)), string(role), "/invitations/"+uuid)
	return err
}

func (r *PostgresAuthRepository) AcceptInvitation(ctx context.Context, inviteUUID string, userID int64, userEmail string) (int64, Role, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, RoleNone, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var teamID int64
	var invEmail, roleStr string
	err = tx.QueryRow(ctx, `
		SELECT team_id, email, role FROM team_invitations WHERE uuid = $1 FOR UPDATE
	`, inviteUUID).Scan(&teamID, &invEmail, &roleStr)
	if err != nil {
		return 0, RoleNone, ErrInvalidInvitation
	}
	if strings.ToLower(strings.TrimSpace(invEmail)) != strings.ToLower(strings.TrimSpace(userEmail)) {
		return 0, RoleNone, ErrInvalidInvitation
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO team_user (team_id, user_id, role)
		VALUES ($1, $2, $3)
		ON CONFLICT (team_id, user_id) DO UPDATE SET role = EXCLUDED.role, updated_at = NOW()
	`, teamID, userID, roleStr); err != nil {
		return 0, RoleNone, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM team_invitations WHERE uuid = $1`, inviteUUID); err != nil {
		return 0, RoleNone, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, RoleNone, err
	}
	return teamID, Role(roleStr), nil
}

func nullableJSONB(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}
