package audit

import (
	"encoding/json"
	"time"
)

type Event struct {
	Timestamp time.Time      `json:"timestamp"`
	ActorID   int64          `json:"actor_id"`
	TeamID    int64          `json:"team_id"`
	Action    string         `json:"action"`
	Target    string         `json:"target"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// MaskSensitiveAPIResponse implements ApiSensitiveData middleware redaction rules.
func MaskSensitiveAPIResponse(fields map[string]any) map[string]any {
	sensitiveKeys := map[string]bool{
		"private_key": true, "password": true, "secret": true, "token": true,
		"smtp_password": true, "resend_api_key": true, "webhook_secret": true,
		"client_secret": true, "two_factor_secret": true, "two_factor_recovery_codes": true,
	}
	out := make(map[string]any, len(fields))
	for k, v := range fields {
		if sensitiveKeys[k] {
			continue // mask/omit sensitive fields per ApiSensitiveData
		}
		out[k] = v
	}
	return out
}

func FormatJSON(ev Event) string {
	b, _ := json.Marshal(ev)
	return string(b)
}
