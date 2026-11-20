package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
)

var (
	ErrInvalidWebhookSignature = errors.New("webhook HMAC signature verification failed")
	ErrWebhookReplay           = errors.New("webhook delivery ID already processed")
)

type WebhookVerifier struct {
	mu      sync.Mutex
	secrets map[string]string // provider -> secret
	seen    map[string]bool   // provider:deliveryID -> seen
}

func NewWebhookVerifier(secrets map[string]string) *WebhookVerifier {
	return &WebhookVerifier{
		secrets: secrets,
		seen:    make(map[string]bool),
	}
}

func (v *WebhookVerifier) VerifyAndDispatch(_ context.Context, provider string, headers http.Header, body []byte) error {
	secret := v.secrets[provider]
	if secret == "" {
		return ErrInvalidWebhookSignature
	}

	var deliveryID string
	switch strings.ToLower(provider) {
	case "github", "gitea":
		sigHeader := headers.Get("X-Hub-Signature-256")
		if sigHeader == "" {
			sigHeader = headers.Get("X-Gitea-Signature")
		}
		if !verifyHMAC256(secret, body, strings.TrimPrefix(sigHeader, "sha256=")) {
			return ErrInvalidWebhookSignature
		}
		deliveryID = headers.Get("X-GitHub-Delivery")
		if deliveryID == "" {
			deliveryID = headers.Get("X-Gitea-Delivery")
		}
	case "gitlab":
		if subtleEq(headers.Get("X-Gitlab-Token"), secret) {
			deliveryID = headers.Get("X-Gitlab-Event-UUID")
		} else {
			return ErrInvalidWebhookSignature
		}
	case "bitbucket":
		if !verifyHMAC256(secret, body, strings.TrimPrefix(headers.Get("X-Hub-Signature"), "sha256=")) {
			return ErrInvalidWebhookSignature
		}
		deliveryID = headers.Get("X-Request-UUID")
	default:
		return ErrInvalidWebhookSignature
	}

	if deliveryID != "" {
		key := provider + ":" + deliveryID
		v.mu.Lock()
		if v.seen[key] {
			v.mu.Unlock()
			return ErrWebhookReplay
		}
		v.seen[key] = true
		v.mu.Unlock()
	}
	return nil
}

func verifyHMAC256(secret string, body []byte, hexSig string) bool {
	got, err := hex.DecodeString(strings.TrimSpace(hexSig))
	if err != nil || len(got) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

func subtleEq(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return hmac.Equal([]byte(a), []byte(b))
}
