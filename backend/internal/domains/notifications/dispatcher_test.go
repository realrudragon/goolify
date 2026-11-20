package notifications_test

import (
	"context"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/domains/notifications"
)

func TestNotificationDispatcher_Channels(t *testing.T) {
	d := notifications.NewNotificationDispatcher()
	for _, ch := range []notifications.ChannelKind{
		notifications.ChannelEmail,
		notifications.ChannelDiscord,
		notifications.ChannelSlack,
		notifications.ChannelTelegram,
		notifications.ChannelPushover,
		notifications.ChannelWebhook,
	} {
		if err := d.Dispatch(context.Background(), ch, 10, "deployment.succeeded", "app deployed"); err != nil {
			t.Fatalf("channel %s failed: %v", ch, err)
		}
	}
	if err := d.Dispatch(context.Background(), "unknown", 10, "x", "y"); err == nil {
		t.Fatal("expected error for unsupported channel")
	}
}
