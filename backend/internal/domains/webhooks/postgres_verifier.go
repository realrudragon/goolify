package webhooks

import (
	"context"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresWebhookService validates Git provider signatures fail-closed against persisted application secrets
// and records delivery IDs in PostgreSQL `go_webhook_deliveries` so replays are rejected across restarts.
type PostgresWebhookService struct {
	pool *pgxpool.Pool
}

func NewPostgresWebhookService(pool *pgxpool.Pool) *PostgresWebhookService {
	return &PostgresWebhookService{pool: pool}
}

type VerifiedWebhook struct {
	ApplicationUUID string
	TeamID          int64
	Provider        string
	DeliveryID      string
}

func (s *PostgresWebhookService) VerifyForApplication(ctx context.Context, provider string, appUUID string, headers http.Header, body []byte) (VerifiedWebhook, error) {
	prov := strings.ToLower(strings.TrimSpace(provider))
	col := ""
	switch prov {
	case "github":
		col = "manual_webhook_secret_github"
	case "gitlab":
		col = "manual_webhook_secret_gitlab"
	case "bitbucket":
		col = "manual_webhook_secret_bitbucket"
	case "gitea":
		col = "manual_webhook_secret_gitea"
	default:
		return VerifiedWebhook{}, ErrInvalidWebhookSignature
	}

	var secret *string
	var teamID int64
	err := s.pool.QueryRow(ctx, `
		SELECT a.`+col+`, p.team_id
		FROM applications a
		JOIN environments e ON e.id = a.environment_id
		JOIN projects p ON p.id = e.project_id
		WHERE a.uuid = $1
	`, appUUID).Scan(&secret, &teamID)
	if err != nil || secret == nil || strings.TrimSpace(*secret) == "" {
		// Fail closed: missing or blank secret rejects before signature verification.
		return VerifiedWebhook{}, ErrInvalidWebhookSignature
	}

	// Verify HMAC / token using constant-time comparison
	v := NewWebhookVerifier(map[string]string{prov: *secret})
	if err := v.VerifyAndDispatch(ctx, prov, headers, body); err != nil {
		return VerifiedWebhook{}, err
	}

	deliveryID := extractDeliveryID(prov, headers)
	if deliveryID != "" {
		tag, err := s.pool.Exec(ctx, `
			INSERT INTO go_webhook_deliveries (provider, delivery_id)
			VALUES ($1, $2)
			ON CONFLICT (provider, delivery_id) DO NOTHING
		`, prov, deliveryID)
		if err != nil {
			return VerifiedWebhook{}, err
		}
		if tag.RowsAffected() == 0 {
			return VerifiedWebhook{}, ErrWebhookReplay
		}
	}

	return VerifiedWebhook{
		ApplicationUUID: appUUID,
		TeamID:          teamID,
		Provider:        prov,
		DeliveryID:      deliveryID,
	}, nil
}

func extractDeliveryID(prov string, headers http.Header) string {
	switch prov {
	case "github":
		return headers.Get("X-GitHub-Delivery")
	case "gitea":
		return headers.Get("X-Gitea-Delivery")
	case "gitlab":
		return headers.Get("X-Gitlab-Event-UUID")
	case "bitbucket":
		return headers.Get("X-Request-UUID")
	default:
		return ""
	}
}
