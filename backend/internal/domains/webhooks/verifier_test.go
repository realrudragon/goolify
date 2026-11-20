package webhooks_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/domains/audit"
	"github.com/coollabsio/coolify/backend/internal/domains/proxy"
	"github.com/coollabsio/coolify/backend/internal/domains/webhooks"
)

func TestWebhookVerifier_SignatureAndReplayProtection(t *testing.T) {
	v := webhooks.NewWebhookVerifier(map[string]string{"github": "wh-secret-1"})
	body := []byte(`{"ref":"refs/heads/main"}`)
	mac := hmac.New(sha256.New, []byte("wh-secret-1"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	h := http.Header{}
	h.Set("X-Hub-Signature-256", sig)
	h.Set("X-GitHub-Delivery", "deliv-1")

	if err := v.VerifyAndDispatch(context.Background(), "github", h, body); err != nil {
		t.Fatalf("expected valid webhook to succeed: %v", err)
	}
	// Replay same delivery ID must fail
	if err := v.VerifyAndDispatch(context.Background(), "github", h, body); !errors.Is(err, webhooks.ErrWebhookReplay) {
		t.Fatalf("expected ErrWebhookReplay, got %v", err)
	}
	// Invalid signature must fail
	h.Set("X-Hub-Signature-256", "sha256=deadbeef")
	if err := v.VerifyAndDispatch(context.Background(), "github", h, body); !errors.Is(err, webhooks.ErrInvalidWebhookSignature) {
		t.Fatalf("expected ErrInvalidWebhookSignature, got %v", err)
	}
}

func TestProxyAndSensitiveMasking(t *testing.T) {
	gen := proxy.NewProxyConfigGenerator()
	bundle, err := gen.Render(context.Background(), "traefik", []proxy.ProxyResource{
		{UUID: "app-1", FQDN: "https://app.example.com", Upstream: "http://app-1:3000"},
	})
	if err != nil || bundle.Dynamic["app-1.yaml"] == "" {
		t.Fatalf("expected traefik dynamic config, got %+v err=%v", bundle, err)
	}

	masked := audit.MaskSensitiveAPIResponse(map[string]any{
		"uuid":        "srv-1",
		"name":        "prod",
		"private_key": "SECRET-KEY-BYTES",
		"password":    "pw",
	})
	if _, exists := masked["private_key"]; exists {
		t.Fatal("private_key should be masked from API response")
	}
	if masked["uuid"] != "srv-1" {
		t.Fatal("expected non-sensitive field preserved")
	}
}
