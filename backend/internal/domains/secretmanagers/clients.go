package secretmanagers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client fetches real secret key-value maps from external secret managers
// (HashiCorp Vault KV v2, Infisical v3, Doppler v3) over real HTTP APIs.
type Client struct {
	HTTP *http.Client
}

func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 10 * time.Second}}
}

type FetchRequest struct {
	Provider string         // vault | infisical | doppler
	Token    string         // Decrypted secret manager token
	Settings map[string]any // base_url, project, config, environment, mount, path
}

func (c *Client) FetchSecrets(ctx context.Context, req FetchRequest) (map[string]string, error) {
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	prov := strings.ToLower(strings.TrimSpace(req.Provider))
	switch prov {
	case "vault", "hashicorp_vault":
		return c.fetchVault(ctx, req)
	case "infisical":
		return c.fetchInfisical(ctx, req)
	case "doppler":
		return c.fetchDoppler(ctx, req)
	default:
		return nil, fmt.Errorf("unsupported secret manager provider %q", req.Provider)
	}
}

func strSetting(m map[string]any, key, def string) string {
	if m == nil {
		return def
	}
	if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func (c *Client) fetchVault(ctx context.Context, req FetchRequest) (map[string]string, error) {
	base := strings.TrimRight(strSetting(req.Settings, "base_url", "http://127.0.0.1:8200"), "/")
	mount := strings.Trim(strSetting(req.Settings, "mount", "secret"), "/")
	secretPath := strings.Trim(strSetting(req.Settings, "path", "app"), "/")
	endpoint := fmt.Sprintf("%s/v1/%s/data/%s", base, url.PathEscape(mount), secretPath)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("X-Vault-Token", req.Token)
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("vault HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Data struct {
			Data map[string]any `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(parsed.Data.Data))
	for k, v := range parsed.Data.Data {
		out[k] = fmt.Sprint(v)
	}
	return out, nil
}

func (c *Client) fetchInfisical(ctx context.Context, req FetchRequest) (map[string]string, error) {
	base := strings.TrimRight(strSetting(req.Settings, "base_url", "https://app.infisical.com"), "/")
	proj := strSetting(req.Settings, "project_id", strSetting(req.Settings, "project", ""))
	env := strSetting(req.Settings, "environment", "prod")
	secretPath := strSetting(req.Settings, "secret_path", "/")

	q := url.Values{}
	if proj != "" {
		q.Set("workspaceId", proj)
	}
	q.Set("environment", env)
	q.Set("secretPath", secretPath)
	endpoint := fmt.Sprintf("%s/api/v3/secrets/raw?%s", base, q.Encode())

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+req.Token)
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("infisical HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Secrets []struct {
			SecretKey   string `json:"secretKey"`
			SecretValue string `json:"secretValue"`
		} `json:"secrets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(parsed.Secrets))
	for _, s := range parsed.Secrets {
		if s.SecretKey != "" {
			out[s.SecretKey] = s.SecretValue
		}
	}
	return out, nil
}

func (c *Client) fetchDoppler(ctx context.Context, req FetchRequest) (map[string]string, error) {
	base := strings.TrimRight(strSetting(req.Settings, "base_url", "https://api.doppler.com"), "/")
	proj := strSetting(req.Settings, "project", "")
	cfg := strSetting(req.Settings, "config", "prd")
	q := url.Values{}
	if proj != "" {
		q.Set("project", proj)
	}
	if cfg != "" {
		q.Set("config", cfg)
	}
	endpoint := fmt.Sprintf("%s/v3/configs/config/secrets?%s", base, q.Encode())

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Authorization", "Bearer "+req.Token)
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("doppler HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Secrets map[string]struct {
			Computed string `json:"computed"`
			Raw      string `json:"raw"`
		} `json:"secrets"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(parsed.Secrets))
	for k, v := range parsed.Secrets {
		val := v.Computed
		if val == "" {
			val = v.Raw
		}
		out[k] = val
	}
	return out, nil
}
