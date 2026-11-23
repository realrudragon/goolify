package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ChannelConfig struct {
	Channel                  ChannelKind `json:"channel"`
	Enabled                  bool        `json:"enabled"`
	WebhookURL               string      `json:"webhook_url,omitempty"`
	SMTPHost                 string      `json:"smtp_host,omitempty"`
	SMTPPort                 int         `json:"smtp_port,omitempty"`
	SMTPFrom                 string      `json:"smtp_from,omitempty"`
	SMTPRecipient            string      `json:"smtp_recipient,omitempty"`
	SMTPUsername             string      `json:"smtp_username,omitempty"`
	SMTPPassword             string      `json:"smtp_password,omitempty"`
	ResendEnabled            bool        `json:"resend_enabled,omitempty"`
	ResendAPIKey             string      `json:"resend_api_key,omitempty"`
	ResendFromAddress        string      `json:"resend_from_address,omitempty"`
	UseInstanceEmailSettings bool        `json:"use_instance_email_settings,omitempty"`
	ChatID                   string      `json:"chat_id,omitempty"`
	BotToken                 string      `json:"bot_token,omitempty"`
	PushoverUserKey          string      `json:"pushover_user_key,omitempty"`
	PushoverAPIToken         string      `json:"pushover_api_token,omitempty"`
	DiscordPingEnabled       bool        `json:"discord_ping_enabled,omitempty"`
}

type DeliveryRecord struct {
	ID          int64     `json:"id"`
	TeamID      int64     `json:"team_id"`
	Channel     string    `json:"channel"`
	Event       string    `json:"event"`
	Summary     string    `json:"summary"`
	Status      string    `json:"status"`
	HTTPStatus  int       `json:"http_status"`
	ErrorText   string    `json:"error_text,omitempty"`
	DeliveredAt time.Time `json:"delivered_at"`
}

// PostgresNotificationService stores encrypted team channel configurations in PostgreSQL
// and executes REAL HTTP/SMTP deliveries, recording delivery outcomes in `go_notification_deliveries`.
type PostgresNotificationService struct {
	pool   *pgxpool.Pool
	authz  *access.Authorizer
	cipher *compat.Cipher
	http   *http.Client
}

func NewPostgresNotificationService(pool *pgxpool.Pool, authz *access.Authorizer, cipher *compat.Cipher) *PostgresNotificationService {
	return &PostgresNotificationService{
		pool:   pool,
		authz:  authz,
		cipher: cipher,
		http:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (s *PostgresNotificationService) SaveConfig(ctx context.Context, principal auth.Principal, teamID int64, cfg ChannelConfig) error {
	if err := s.authz.Check(ctx, principal, access.ActionUpdate, access.ResourceRef{
		Type: "team", ID: teamID, TeamID: &teamID, APISegment: "notifications",
	}); err != nil {
		return err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	stored := string(raw)
	if s.cipher != nil {
		enc, err := s.cipher.EncryptLaravelPayload(raw, false)
		if err != nil {
			return err
		}
		stored = enc
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO go_notification_configs (team_id, channel, enabled, config_enc, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (team_id, channel) DO UPDATE SET
			enabled = EXCLUDED.enabled,
			config_enc = EXCLUDED.config_enc,
			updated_at = NOW()
	`, teamID, string(cfg.Channel), cfg.Enabled, stored)
	if err != nil {
		return err
	}
	return s.syncToLaravelTable(ctx, teamID, cfg)
}

func (s *PostgresNotificationService) ListConfigs(ctx context.Context, principal auth.Principal, teamID int64) ([]map[string]any, error) {
	if err := s.authz.Check(ctx, principal, access.ActionRead, access.ResourceRef{
		Type: "team", ID: teamID, TeamID: &teamID, APISegment: "notifications",
	}); err != nil {
		return nil, err
	}
	merged := s.listLaravelChannels(ctx, teamID)
	rows, err := s.pool.Query(ctx, `
		SELECT channel, enabled FROM go_notification_configs WHERE team_id = $1 ORDER BY channel ASC
	`, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ch string
		var en bool
		if err := rows.Scan(&ch, &en); err != nil {
			return nil, err
		}
		if _, exists := merged[ch]; !exists {
			merged[ch] = en
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	order := []string{"discord", "email", "pushover", "slack", "telegram", "webhook"}
	out := make([]map[string]any, 0, len(merged))
	seen := make(map[string]bool)
	for _, ch := range order {
		if en, ok := merged[ch]; ok {
			out = append(out, map[string]any{"channel": ch, "enabled": en})
			seen[ch] = true
		}
	}
	for ch, en := range merged {
		if !seen[ch] {
			out = append(out, map[string]any{"channel": ch, "enabled": en})
		}
	}
	return out, nil
}

func (s *PostgresNotificationService) LoadConfig(ctx context.Context, teamID int64, ch ChannelKind) (ChannelConfig, error) {
	laravelCfg, foundLaravel := s.loadFromLaravelTable(ctx, teamID, ch)

	var enabled bool
	var enc string
	err := s.pool.QueryRow(ctx, `
		SELECT enabled, config_enc FROM go_notification_configs WHERE team_id = $1 AND channel = $2
	`, teamID, string(ch)).Scan(&enabled, &enc)
	if err != nil {
		if foundLaravel {
			return laravelCfg, nil
		}
		return ChannelConfig{}, err
	}
	raw := []byte(enc)
	if s.cipher != nil && enc != "" {
		if plain, decErr := s.cipher.DecryptLaravelPayload(enc, false); decErr == nil {
			raw = plain
		}
	}
	var cfg ChannelConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		if foundLaravel {
			return laravelCfg, nil
		}
		return ChannelConfig{}, err
	}
	cfg.Enabled = enabled
	cfg.Channel = ch
	if foundLaravel {
		// Laravel table is authoritative for upgraded/rolled-back installations; retain custom WebhookURL override if set.
		if laravelCfg.WebhookURL == "" && cfg.WebhookURL != "" {
			laravelCfg.WebhookURL = cfg.WebhookURL
		}
		return laravelCfg, nil
	}
	return cfg, nil
}

// SendReal executes an actual HTTP POST or SMTP send according to the team's stored channel config
// and persists the delivery result (never returning fake success when the endpoint fails!).
func (s *PostgresNotificationService) SendReal(ctx context.Context, teamID int64, ch ChannelKind, event string, summary string) (DeliveryRecord, error) {
	cfg, err := s.LoadConfig(ctx, teamID, ch)
	if err != nil {
		return DeliveryRecord{}, fmt.Errorf("notification channel %s not configured for team %d: %w", ch, teamID, err)
	}
	if !cfg.Enabled {
		return DeliveryRecord{}, fmt.Errorf("notification channel %s is disabled for team %d", ch, teamID)
	}

	var httpStatus int
	var sendErr error

	switch ch {
	case ChannelWebhook, ChannelDiscord, ChannelSlack, ChannelPushover, ChannelTelegram:
		targetURL := strings.TrimSpace(cfg.WebhookURL)
		payload := map[string]any{
			"event":   event,
			"summary": summary,
			"content": fmt.Sprintf("[Coolify] %s: %s", event, summary),
			"text":    fmt.Sprintf("[Coolify] %s: %s", event, summary),
			"team_id": teamID,
		}
		if ch == ChannelTelegram {
			targetURL = resolveTelegramEndpoint(cfg)
			payload["chat_id"] = cfg.ChatID
		} else if ch == ChannelPushover {
			targetURL = resolvePushoverEndpoint(cfg)
			payload["token"] = cfg.PushoverAPIToken
			payload["user"] = cfg.PushoverUserKey
			payload["title"] = fmt.Sprintf("[Coolify] %s", event)
			payload["message"] = summary
		}
		if targetURL == "" {
			sendErr = errors.New("webhook_url is empty")
			break
		}
		body, _ := json.Marshal(payload)
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
		if reqErr != nil {
			sendErr = reqErr
			break
		}
		req.Header.Set("Content-Type", "application/json")
		resp, doErr := s.http.Do(req)
		if doErr != nil {
			sendErr = doErr
			break
		}
		httpStatus = resp.StatusCode
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		if httpStatus < 200 || httpStatus >= 300 {
			sendErr = fmt.Errorf("notification HTTP endpoint returned %d", httpStatus)
		}

	case ChannelEmail:
		eff := cfg
		if eff.UseInstanceEmailSettings && s.pool != nil {
			var instSMTPOn, instResendOn bool
			var instHost, instUser, instPass, instFrom, instResKey, instResFrom string
			var instPort int
			if err := s.pool.QueryRow(ctx, `
				SELECT COALESCE(smtp_enabled,FALSE), COALESCE(smtp_host,''), COALESCE(smtp_port,587),
				       COALESCE(smtp_username,''), COALESCE(smtp_password,''), COALESCE(smtp_from_address,''),
				       COALESCE(resend_enabled,FALSE), COALESCE(resend_api_key,''), COALESCE(resend_from_address,'')
				FROM instance_settings WHERE id = 0
			`).Scan(&instSMTPOn, &instHost, &instPort, &instUser, &instPass, &instFrom, &instResendOn, &instResKey, &instResFrom); err == nil {
				if instSMTPOn && strings.TrimSpace(instHost) != "" {
					eff.SMTPHost = strings.TrimSpace(instHost)
					eff.SMTPPort = instPort
					eff.SMTPUsername = strings.TrimSpace(instUser)
					eff.SMTPPassword = s.decryptString(instPass)
					if eff.SMTPFrom == "" {
						eff.SMTPFrom = strings.TrimSpace(instFrom)
					}
				}
				if instResendOn && strings.TrimSpace(instResKey) != "" {
					eff.ResendEnabled = true
					eff.ResendAPIKey = s.decryptString(instResKey)
					if eff.ResendFromAddress == "" {
						eff.ResendFromAddress = strings.TrimSpace(instResFrom)
					}
				}
			}
		}

		var recips []string
		for _, r := range strings.Split(eff.SMTPRecipient, ",") {
			if tr := strings.TrimSpace(r); tr != "" {
				recips = append(recips, tr)
			}
		}
		if len(recips) == 0 {
			sendErr = errors.New("smtp_recipient is required")
			break
		}

		if eff.ResendEnabled && strings.TrimSpace(eff.ResendAPIKey) != "" {
			from := strings.TrimSpace(eff.ResendFromAddress)
			if from == "" {
				from = strings.TrimSpace(eff.SMTPFrom)
			}
			if from == "" {
				from = "coolify@localhost"
			}
			resendURL := "https://api.resend.com/emails"
			if strings.HasPrefix(eff.SMTPHost, "http://") || strings.HasPrefix(eff.SMTPHost, "https://") {
				resendURL = strings.TrimRight(eff.SMTPHost, "/") + "/emails"
			}
			bodyBytes, _ := json.Marshal(map[string]any{
				"from":    from,
				"to":      recips,
				"subject": fmt.Sprintf("[Coolify] %s", event),
				"text":    summary,
			})
			req, rErr := http.NewRequestWithContext(ctx, http.MethodPost, resendURL, bytes.NewReader(bodyBytes))
			if rErr != nil {
				sendErr = rErr
				break
			}
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(eff.ResendAPIKey))
			req.Header.Set("Content-Type", "application/json")
			resp, doErr := s.http.Do(req)
			if doErr != nil {
				sendErr = doErr
				break
			}
			httpStatus = resp.StatusCode
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			if httpStatus < 200 || httpStatus >= 300 {
				sendErr = fmt.Errorf("resend API returned HTTP %d", httpStatus)
			}
			break
		}

		if eff.SMTPHost == "" {
			sendErr = errors.New("smtp_host and smtp_recipient are required")
			break
		}
		port := eff.SMTPPort
		if port <= 0 {
			port = 25
		}
		addr := fmt.Sprintf("%s:%d", eff.SMTPHost, port)
		from := eff.SMTPFrom
		if from == "" {
			from = "coolify@localhost"
		}
		msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: [Coolify] %s\r\n\r\n%s\r\n", from, strings.Join(recips, ", "), event, summary)
		var authObj smtp.Auth
		if eff.SMTPUsername != "" {
			authObj = smtp.PlainAuth("", eff.SMTPUsername, eff.SMTPPassword, eff.SMTPHost)
		}
		sendErr = smtp.SendMail(addr, authObj, from, recips, []byte(msg))

	default:
		sendErr = fmt.Errorf("unsupported notification channel %q", ch)
	}

	status := "delivered"
	errText := ""
	if sendErr != nil {
		status = "failed"
		errText = sendErr.Error()
	}

	var rec DeliveryRecord
	_ = s.pool.QueryRow(ctx, `
		INSERT INTO go_notification_deliveries (team_id, channel, event, summary, status, http_status, error_text)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id, team_id, channel, event, summary, status, http_status, error_text, delivered_at
	`, teamID, string(ch), event, summary, status, httpStatus, errText).Scan(
		&rec.ID, &rec.TeamID, &rec.Channel, &rec.Event, &rec.Summary,
		&rec.Status, &rec.HTTPStatus, &rec.ErrorText, &rec.DeliveredAt,
	)
	return rec, sendErr
}

func (s *PostgresNotificationService) decryptString(raw string) string {
	if raw == "" || s.cipher == nil {
		return raw
	}
	if plain, err := s.cipher.DecryptLaravelPayload(raw, false); err == nil {
		return string(plain)
	}
	return raw
}
