package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
)

// ScopedAccessRoutes mirrors App\Http\Middleware\ApiAbility::SCOPED_ACCESS_ROUTES.
var ScopedAccessRoutes = []string{
	"applications",
	"services",
	"databases",
	"projects",
	"deploy",
	"deployments",
	"resources",
	"version",
	"vulnerabilities",
}

// HashSanctumToken produces the SHA-256 hex digest stored in personal_access_tokens.token.
func HashSanctumToken(plainSecret string) string {
	sum := sha256.Sum256([]byte(plainSecret))
	return hex.EncodeToString(sum[:])
}

// FormatSanctumToken formats a Sanctum bearer string "<id>|<secret>".
func FormatSanctumToken(id int64, plainSecret string) string {
	return fmt.Sprintf("%d|%s", id, plainSecret)
}

// HasAbility checks whether the principal's bearer token holds any required ability (or root).
func HasAbility(p Principal, firstPathSegment string, required ...string) bool {
	if p.AuthMethod == "session" {
		return true
	}
	if p.TokenTeamID != nil && !p.TokenWholeTeam && firstPathSegment != "" {
		if !slices.Contains(ScopedAccessRoutes, firstPathSegment) {
			return false
		}
	}
	if slices.Contains(p.TokenAbilities, "root") {
		return true
	}
	for _, req := range required {
		if slices.Contains(p.TokenAbilities, req) {
			return true
		}
	}
	return false
}
