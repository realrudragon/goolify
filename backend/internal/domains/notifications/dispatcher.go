package notifications

import (
	"context"
	"fmt"
)

type ChannelKind string

const (
	ChannelEmail    ChannelKind = "email"
	ChannelDiscord  ChannelKind = "discord"
	ChannelSlack    ChannelKind = "slack"
	ChannelTelegram ChannelKind = "telegram"
	ChannelPushover ChannelKind = "pushover"
	ChannelWebhook  ChannelKind = "webhook"
)

type NotificationDispatcher struct {
	sent []string
}

func NewNotificationDispatcher() *NotificationDispatcher {
	return &NotificationDispatcher{}
}

func (n *NotificationDispatcher) Dispatch(_ context.Context, kind ChannelKind, teamID int64, event string, summary string) error {
	switch kind {
	case ChannelEmail, ChannelDiscord, ChannelSlack, ChannelTelegram, ChannelPushover, ChannelWebhook:
		n.sent = append(n.sent, fmt.Sprintf("%s:%d:%s:%s", kind, teamID, event, summary))
		return nil
	default:
		return fmt.Errorf("unsupported notification channel %q", kind)
	}
}
