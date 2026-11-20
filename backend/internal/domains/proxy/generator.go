package proxy

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type ProxyResource struct {
	UUID     string
	FQDN     string
	Upstream string
	TLS      bool
}

type ProxyConfigBundle struct {
	ProxyType string
	MainYAML  string
	Dynamic   map[string]string
}

type ProxyConfigGenerator struct{}

func NewProxyConfigGenerator() *ProxyConfigGenerator {
	return &ProxyConfigGenerator{}
}

func (g *ProxyConfigGenerator) Render(_ context.Context, proxyType string, resources []ProxyResource) (ProxyConfigBundle, error) {
	sorted := append([]ProxyResource(nil), resources...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].UUID < sorted[j].UUID })

	dynamic := make(map[string]string, len(sorted))
	switch strings.ToLower(proxyType) {
	case "traefik", "":
		var main strings.Builder
		main.WriteString("entryPoints:\n  http:\n    address: ':80'\n  https:\n    address: ':443'\n")
		for _, r := range sorted {
			dynamic[r.UUID+".yaml"] = fmt.Sprintf(
				"http:\n  routers:\n    %s:\n      rule: Host(`%s`)\n      service: svc-%s\n  services:\n    svc-%s:\n      loadBalancer:\n        servers:\n          - url: %q\n",
				r.UUID, strings.TrimPrefix(strings.TrimPrefix(r.FQDN, "https://"), "http://"), r.UUID, r.UUID, r.Upstream,
			)
		}
		return ProxyConfigBundle{ProxyType: "traefik", MainYAML: main.String(), Dynamic: dynamic}, nil
	case "caddy":
		var caddyfile strings.Builder
		for _, r := range sorted {
			caddyfile.WriteString(fmt.Sprintf("%s {\n    reverse_proxy %s\n}\n", r.FQDN, r.Upstream))
		}
		return ProxyConfigBundle{ProxyType: "caddy", MainYAML: caddyfile.String(), Dynamic: dynamic}, nil
	default:
		return ProxyConfigBundle{}, fmt.Errorf("unsupported proxy type %q", proxyType)
	}
}
