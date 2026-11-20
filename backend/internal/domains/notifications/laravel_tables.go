package notifications

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func (s *PostgresNotificationService) encryptField(plain string) (*string, error) {
	if plain == "" {
		return nil, nil
	}
	if s.cipher == nil {
		v := plain
		return &v, nil
	}
	enc, err := s.cipher.EncryptLaravelPayload([]byte(plain), false)
	if err != nil {
		return nil, err
	}
	return &enc, nil
}

func (s *PostgresNotificationService) decryptField(raw sql.NullString) string {
	if !raw.Valid || raw.String == "" {
		return ""
	}
	if s.cipher == nil {
		return raw.String
	}
	if plain, err := s.cipher.DecryptLaravelPayload(raw.String, false); err == nil {
		return string(plain)
	}
	if plain, err := s.cipher.DecryptLaravelPayload(raw.String, true); err == nil {
		return string(plain)
	}
	return raw.String
}

// syncToLaravelTable persists channel configuration into Coolify's exact Laravel
// `<channel>_notification_settings` table with APP_KEY-encrypted secret columns so both
// upgrades from Laravel and rollbacks to Laravel preserve notification settings seamlessly.
func (s *PostgresNotificationService) syncToLaravelTable(ctx context.Context, teamID int64, cfg ChannelConfig) error {
	switch cfg.Channel {
	case ChannelEmail:
		fromAddr, err := s.encryptField(cfg.SMTPFrom)
		if err != nil {
			return err
		}
		recips, err := s.encryptField(cfg.SMTPRecipient)
		if err != nil {
			return err
		}
		host, err := s.encryptField(cfg.SMTPHost)
		if err != nil {
			return err
		}
		user, err := s.encryptField(cfg.SMTPUsername)
		if err != nil {
			return err
		}
		pass, err := s.encryptField(cfg.SMTPPassword)
		if err != nil {
			return err
		}
		resendKey, err := s.encryptField(cfg.ResendAPIKey)
		if err != nil {
			return err
		}
		resendFrom, err := s.encryptField(cfg.ResendFromAddress)
		if err != nil {
			return err
		}
		_, err = s.pool.Exec(ctx, `
			INSERT INTO email_notification_settings (
				team_id, smtp_enabled, smtp_from_address, smtp_recipients, smtp_host, smtp_port,
				smtp_username, smtp_password, resend_enabled, resend_api_key, resend_from_address,
				use_instance_email_settings, updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,NOW())
			ON CONFLICT (team_id) DO UPDATE SET
				smtp_enabled = EXCLUDED.smtp_enabled,
				smtp_from_address = COALESCE(EXCLUDED.smtp_from_address, email_notification_settings.smtp_from_address),
				smtp_recipients = COALESCE(EXCLUDED.smtp_recipients, email_notification_settings.smtp_recipients),
				smtp_host = COALESCE(EXCLUDED.smtp_host, email_notification_settings.smtp_host),
				smtp_port = COALESCE(EXCLUDED.smtp_port, email_notification_settings.smtp_port),
				smtp_username = COALESCE(EXCLUDED.smtp_username, email_notification_settings.smtp_username),
				smtp_password = COALESCE(EXCLUDED.smtp_password, email_notification_settings.smtp_password),
				resend_enabled = EXCLUDED.resend_enabled,
				resend_api_key = COALESCE(EXCLUDED.resend_api_key, email_notification_settings.resend_api_key),
				resend_from_address = COALESCE(EXCLUDED.resend_from_address, email_notification_settings.resend_from_address),
				use_instance_email_settings = EXCLUDED.use_instance_email_settings,
				updated_at = NOW()
		`, teamID, cfg.Enabled, fromAddr, recips, host, nullInt(cfg.SMTPPort), user, pass,
			cfg.ResendEnabled, resendKey, resendFrom, cfg.UseInstanceEmailSettings)
		return err

	case ChannelDiscord:
		urlEnc, err := s.encryptField(cfg.WebhookURL)
		if err != nil {
			return err
		}
		_, err = s.pool.Exec(ctx, `
			INSERT INTO discord_notification_settings (team_id, discord_enabled, discord_webhook_url, discord_ping_enabled, updated_at)
			VALUES ($1, $2, $3, $4, NOW())
			ON CONFLICT (team_id) DO UPDATE SET
				discord_enabled = EXCLUDED.discord_enabled,
				discord_webhook_url = COALESCE(EXCLUDED.discord_webhook_url, discord_notification_settings.discord_webhook_url),
				discord_ping_enabled = EXCLUDED.discord_ping_enabled,
				updated_at = NOW()
		`, teamID, cfg.Enabled, urlEnc, cfg.DiscordPingEnabled)
		return err

	case ChannelSlack:
		urlEnc, err := s.encryptField(cfg.WebhookURL)
		if err != nil {
			return err
		}
		_, err = s.pool.Exec(ctx, `
			INSERT INTO slack_notification_settings (team_id, slack_enabled, slack_webhook_url, updated_at)
			VALUES ($1, $2, $3, NOW())
			ON CONFLICT (team_id) DO UPDATE SET
				slack_enabled = EXCLUDED.slack_enabled,
				slack_webhook_url = COALESCE(EXCLUDED.slack_webhook_url, slack_notification_settings.slack_webhook_url),
				updated_at = NOW()
		`, teamID, cfg.Enabled, urlEnc)
		return err

	case ChannelTelegram:
		tokEnc, err := s.encryptField(cfg.BotToken)
		if err != nil {
			return err
		}
		chatEnc, err := s.encryptField(cfg.ChatID)
		if err != nil {
			return err
		}
		_, err = s.pool.Exec(ctx, `
			INSERT INTO telegram_notification_settings (team_id, telegram_enabled, telegram_token, telegram_chat_id, updated_at)
			VALUES ($1, $2, $3, $4, NOW())
			ON CONFLICT (team_id) DO UPDATE SET
				telegram_enabled = EXCLUDED.telegram_enabled,
				telegram_token = COALESCE(EXCLUDED.telegram_token, telegram_notification_settings.telegram_token),
				telegram_chat_id = COALESCE(EXCLUDED.telegram_chat_id, telegram_notification_settings.telegram_chat_id),
				updated_at = NOW()
		`, teamID, cfg.Enabled, tokEnc, chatEnc)
		return err

	case ChannelPushover:
		userEnc, err := s.encryptField(cfg.PushoverUserKey)
		if err != nil {
			return err
		}
		tokEnc, err := s.encryptField(cfg.PushoverAPIToken)
		if err != nil {
			return err
		}
		_, err = s.pool.Exec(ctx, `
			INSERT INTO pushover_notification_settings (team_id, pushover_enabled, pushover_user_key, pushover_api_token, updated_at)
			VALUES ($1, $2, $3, $4, NOW())
			ON CONFLICT (team_id) DO UPDATE SET
				pushover_enabled = EXCLUDED.pushover_enabled,
				pushover_user_key = COALESCE(EXCLUDED.pushover_user_key, pushover_notification_settings.pushover_user_key),
				pushover_api_token = COALESCE(EXCLUDED.pushover_api_token, pushover_notification_settings.pushover_api_token),
				updated_at = NOW()
		`, teamID, cfg.Enabled, userEnc, tokEnc)
		return err

	case ChannelWebhook:
		urlEnc, err := s.encryptField(cfg.WebhookURL)
		if err != nil {
			return err
		}
		_, err = s.pool.Exec(ctx, `
			INSERT INTO webhook_notification_settings (team_id, webhook_enabled, webhook_url, updated_at)
			VALUES ($1, $2, $3, NOW())
			ON CONFLICT (team_id) DO UPDATE SET
				webhook_enabled = EXCLUDED.webhook_enabled,
				webhook_url = COALESCE(EXCLUDED.webhook_url, webhook_notification_settings.webhook_url),
				updated_at = NOW()
		`, teamID, cfg.Enabled, urlEnc)
		return err
	}
	return nil
}

func (s *PostgresNotificationService) loadFromLaravelTable(ctx context.Context, teamID int64, ch ChannelKind) (ChannelConfig, bool) {
	cfg := ChannelConfig{Channel: ch}
	switch ch {
	case ChannelEmail:
		var enabled, resendEn, useInst bool
		var fromAddr, recips, host, user, pass, resendKey, resendFrom sql.NullString
		var port sql.NullInt32
		err := s.pool.QueryRow(ctx, `
			SELECT smtp_enabled, smtp_from_address, smtp_recipients, smtp_host, smtp_port,
			       smtp_username, smtp_password, resend_enabled, resend_api_key, resend_from_address,
			       use_instance_email_settings
			FROM email_notification_settings WHERE team_id = $1
		`, teamID).Scan(&enabled, &fromAddr, &recips, &host, &port, &user, &pass, &resendEn, &resendKey, &resendFrom, &useInst)
		if err != nil {
			return cfg, false
		}
		cfg.Enabled = enabled || resendEn
		cfg.SMTPFrom = s.decryptField(fromAddr)
		cfg.SMTPRecipient = s.decryptField(recips)
		cfg.SMTPHost = s.decryptField(host)
		if port.Valid {
			cfg.SMTPPort = int(port.Int32)
		}
		cfg.SMTPUsername = s.decryptField(user)
		cfg.SMTPPassword = s.decryptField(pass)
		cfg.ResendEnabled = resendEn
		cfg.ResendAPIKey = s.decryptField(resendKey)
		cfg.ResendFromAddress = s.decryptField(resendFrom)
		cfg.UseInstanceEmailSettings = useInst
		return cfg, true

	case ChannelDiscord:
		var enabled, ping bool
		var urlEnc sql.NullString
		err := s.pool.QueryRow(ctx, `
			SELECT discord_enabled, discord_webhook_url, discord_ping_enabled
			FROM discord_notification_settings WHERE team_id = $1
		`, teamID).Scan(&enabled, &urlEnc, &ping)
		if err != nil {
			return cfg, false
		}
		cfg.Enabled = enabled
		cfg.WebhookURL = s.decryptField(urlEnc)
		cfg.DiscordPingEnabled = ping
		return cfg, true

	case ChannelSlack:
		var enabled bool
		var urlEnc sql.NullString
		err := s.pool.QueryRow(ctx, `
			SELECT slack_enabled, slack_webhook_url FROM slack_notification_settings WHERE team_id = $1
		`, teamID).Scan(&enabled, &urlEnc)
		if err != nil {
			return cfg, false
		}
		cfg.Enabled = enabled
		cfg.WebhookURL = s.decryptField(urlEnc)
		return cfg, true

	case ChannelTelegram:
		var enabled bool
		var tokEnc, chatEnc sql.NullString
		err := s.pool.QueryRow(ctx, `
			SELECT telegram_enabled, telegram_token, telegram_chat_id
			FROM telegram_notification_settings WHERE team_id = $1
		`, teamID).Scan(&enabled, &tokEnc, &chatEnc)
		if err != nil {
			return cfg, false
		}
		cfg.Enabled = enabled
		cfg.BotToken = s.decryptField(tokEnc)
		cfg.ChatID = s.decryptField(chatEnc)
		return cfg, true

	case ChannelPushover:
		var enabled bool
		var userEnc, tokEnc sql.NullString
		err := s.pool.QueryRow(ctx, `
			SELECT pushover_enabled, pushover_user_key, pushover_api_token
			FROM pushover_notification_settings WHERE team_id = $1
		`, teamID).Scan(&enabled, &userEnc, &tokEnc)
		if err != nil {
			return cfg, false
		}
		cfg.Enabled = enabled
		cfg.PushoverUserKey = s.decryptField(userEnc)
		cfg.PushoverAPIToken = s.decryptField(tokEnc)
		return cfg, true

	case ChannelWebhook:
		var enabled bool
		var urlEnc sql.NullString
		err := s.pool.QueryRow(ctx, `
			SELECT webhook_enabled, webhook_url FROM webhook_notification_settings WHERE team_id = $1
		`, teamID).Scan(&enabled, &urlEnc)
		if err != nil {
			return cfg, false
		}
		cfg.Enabled = enabled
		cfg.WebhookURL = s.decryptField(urlEnc)
		return cfg, true
	}
	return cfg, false
}

// CheckAndRecordThrottle enforces per-notifiable throttling in `notification_throttles`.
// Returns throttled=true if an identical notification was already sent within `window`.
func (s *PostgresNotificationService) CheckAndRecordThrottle(
	ctx context.Context,
	notifiableType string,
	notifiableID int64,
	notification string,
	window time.Duration,
) (bool, error) {
	if window <= 0 {
		window = 5 * time.Minute
	}
	var lastSent time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT sent_at FROM notification_throttles
		WHERE notifiable_type = $1 AND notifiable_id = $2 AND notification = $3
	`, notifiableType, notifiableID, notification).Scan(&lastSent)
	if err == nil && time.Since(lastSent) < window {
		return true, nil
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO notification_throttles (notifiable_type, notifiable_id, notification, sent_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (notifiable_type, notifiable_id, notification) DO UPDATE SET sent_at = NOW()
	`, notifiableType, notifiableID, notification)
	if err != nil {
		return false, fmt.Errorf("record notification throttle: %w", err)
	}
	return false, nil
}

func nullInt(v int) any {
	if v <= 0 {
		return nil
	}
	return v
}

func (s *PostgresNotificationService) listLaravelChannels(ctx context.Context, teamID int64) map[string]bool {
	out := make(map[string]bool)
	queries := []struct {
		ch string
		q  string
	}{
		{"email", `SELECT (smtp_enabled OR resend_enabled) FROM email_notification_settings WHERE team_id = $1`},
		{"discord", `SELECT discord_enabled FROM discord_notification_settings WHERE team_id = $1`},
		{"slack", `SELECT slack_enabled FROM slack_notification_settings WHERE team_id = $1`},
		{"telegram", `SELECT telegram_enabled FROM telegram_notification_settings WHERE team_id = $1`},
		{"pushover", `SELECT pushover_enabled FROM pushover_notification_settings WHERE team_id = $1`},
		{"webhook", `SELECT webhook_enabled FROM webhook_notification_settings WHERE team_id = $1`},
	}
	for _, item := range queries {
		var en bool
		if err := s.pool.QueryRow(ctx, item.q, teamID).Scan(&en); err == nil {
			out[item.ch] = en
		}
	}
	return out
}

func resolveTelegramEndpoint(cfg ChannelConfig) string {
	if strings.TrimSpace(cfg.WebhookURL) != "" {
		return strings.TrimSpace(cfg.WebhookURL)
	}
	if strings.TrimSpace(cfg.BotToken) != "" {
		return "https://api.telegram.org/bot" + strings.TrimSpace(cfg.BotToken) + "/sendMessage"
	}
	return ""
}

func resolvePushoverEndpoint(cfg ChannelConfig) string {
	if strings.TrimSpace(cfg.WebhookURL) != "" {
		return strings.TrimSpace(cfg.WebhookURL)
	}
	if strings.TrimSpace(cfg.PushoverAPIToken) != "" && strings.TrimSpace(cfg.PushoverUserKey) != "" {
		return "https://api.pushover.net/1/messages.json"
	}
	return ""
}
