package secretmanagers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSecretManagers_RealHTTPVaultInfisicalDoppler(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/kv/data/prod":
			if r.Header.Get("X-Vault-Token") != "vault-tok" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_, _ = w.Write([]byte(`{"data":{"data":{"DB_PASS":"v-secret","API_KEY":"v-key"}}}`))
		case r.URL.Path == "/api/v3/secrets/raw":
			if r.Header.Get("Authorization") != "Bearer inf-tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"secrets":[{"secretKey":"JWT_SECRET","secretValue":"inf-secret"}]}`))
		case r.URL.Path == "/v3/configs/config/secrets":
			if r.Header.Get("Authorization") != "Bearer dop-tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"secrets":{"STRIPE_KEY":{"computed":"sk_live_doppler"}}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := NewClient()
	ctx := context.Background()

	vMap, err := c.FetchSecrets(ctx, FetchRequest{Provider: "vault", Token: "vault-tok", Settings: map[string]any{"base_url": srv.URL, "mount": "kv", "path": "prod"}})
	if err != nil || vMap["DB_PASS"] != "v-secret" {
		t.Fatalf("vault fetch failed: %+v err=%v", vMap, err)
	}
	iMap, err := c.FetchSecrets(ctx, FetchRequest{Provider: "infisical", Token: "inf-tok", Settings: map[string]any{"base_url": srv.URL, "project_id": "p1", "environment": "prod"}})
	if err != nil || iMap["JWT_SECRET"] != "inf-secret" {
		t.Fatalf("infisical fetch failed: %+v err=%v", iMap, err)
	}
	dMap, err := c.FetchSecrets(ctx, FetchRequest{Provider: "doppler", Token: "dop-tok", Settings: map[string]any{"base_url": srv.URL, "project": "app", "config": "prd"}})
	if err != nil || dMap["STRIPE_KEY"] != "sk_live_doppler" {
		t.Fatalf("doppler fetch failed: %+v err=%v", dMap, err)
	}
}
