package cloud

import "context"

// ProviderRegistry covers Hetzner, DigitalOcean, Vultr, Hostinger, and DNS integrations.
type ProviderClient interface {
	ProviderName() string
	ValidateToken(ctx context.Context, token string) error
}

type Registry struct {
	providers map[string]bool
}

func NewRegistry() *Registry {
	return &Registry{
		providers: map[string]bool{
			"hetzner":      true,
			"digitalocean": true,
			"vultr":        true,
			"hostinger":    true,
			"cloudflare":   true,
		},
	}
}

func (r *Registry) Supports(provider string) bool {
	return r.providers[provider]
}
