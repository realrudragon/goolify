package proxy

import (
	"context"
	"encoding/base64"
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
	ProxyType   string            `json:"proxy_type"`
	Status      string            `json:"status,omitempty"`
	ComposeYAML string            `json:"compose_yaml"`
	MainYAML    string            `json:"main_yaml"`
	Dynamic     map[string]string `json:"dynamic"`
}

type ProxyConfigGenerator struct{}

func NewProxyConfigGenerator() *ProxyConfigGenerator {
	return &ProxyConfigGenerator{}
}

// DefaultComposeYAML generates the exact Coolify default docker-compose.yml for coolify-proxy
// (parity with generateDefaultProxyConfiguration in bootstrap/helpers/proxy.php).
func (g *ProxyConfigGenerator) DefaultComposeYAML(proxyType string, networks []string) string {
	nets := sanitizeNetworks(networks)
	switch strings.ToLower(strings.TrimSpace(proxyType)) {
	case "caddy":
		var b strings.Builder
		b.WriteString("networks:\n")
		for _, n := range nets {
			fmt.Fprintf(&b, "  %s:\n    external: true\n", n)
		}
		b.WriteString("services:\n  caddy:\n    container_name: coolify-proxy\n    image: lucaslorentz/caddy-docker-proxy:2.8-alpine\n    restart: unless-stopped\n")
		b.WriteString("    extra_hosts:\n      - 'host.docker.internal:host-gateway'\n")
		b.WriteString("    environment:\n      - CADDY_DOCKER_POLLING_INTERVAL=5s\n      - CADDY_DOCKER_CADDYFILE_PATH=/dynamic/Caddyfile\n")
		b.WriteString("    networks:\n")
		for _, n := range nets {
			fmt.Fprintf(&b, "      - %s\n", n)
		}
		b.WriteString("    ports:\n      - '80:80'\n      - '443:443'\n      - '443:443/udp'\n")
		b.WriteString("    labels:\n      - coolify.managed=true\n      - coolify.proxy=true\n")
		b.WriteString("    volumes:\n      - '/var/run/docker.sock:/var/run/docker.sock:ro'\n      - '/data/coolify/proxy/dynamic:/dynamic'\n      - '/data/coolify/proxy/config:/config'\n      - '/data/coolify/proxy/data:/data'\n")
		return b.String()
	default:
		var b strings.Builder
		b.WriteString("name: coolify-proxy\nnetworks:\n")
		for _, n := range nets {
			fmt.Fprintf(&b, "  %s:\n    external: true\n", n)
		}
		b.WriteString("services:\n  traefik:\n    container_name: coolify-proxy\n    image: traefik:v3.7\n    restart: unless-stopped\n")
		b.WriteString("    extra_hosts:\n      - 'host.docker.internal:host-gateway'\n")
		b.WriteString("    networks:\n")
		for _, n := range nets {
			fmt.Fprintf(&b, "      - %s\n", n)
		}
		b.WriteString("    ports:\n      - '80:80'\n      - '443:443'\n      - '443:443/udp'\n      - '8080:8080'\n")
		b.WriteString("    healthcheck:\n      test: 'wget -qO- http://localhost:80/ping || exit 1'\n      interval: 4s\n      timeout: 2s\n      retries: 5\n")
		b.WriteString("    volumes:\n      - '/var/run/docker.sock:/var/run/docker.sock:ro'\n      - '/data/coolify/proxy:/traefik'\n")
		b.WriteString("    command:\n")
		for _, flag := range []string{
			"--ping=true",
			"--ping.entrypoint=http",
			"--api.dashboard=true",
			"--api.insecure=false",
			"--entrypoints.http.address=:80",
			"--entrypoints.https.address=:443",
			"--entrypoints.http.http.encodequerysemicolons=true",
			"--entryPoints.http.http2.maxConcurrentStreams=250",
			"--entrypoints.https.http.encodequerysemicolons=true",
			"--entryPoints.https.http2.maxConcurrentStreams=250",
			"--entrypoints.https.http3",
			"--providers.file.directory=/traefik/dynamic/",
			"--providers.file.watch=true",
			"--certificatesresolvers.letsencrypt.acme.httpchallenge=true",
			"--certificatesresolvers.letsencrypt.acme.httpchallenge.entrypoint=http",
			"--certificatesresolvers.letsencrypt.acme.storage=/traefik/acme.json",
			"--providers.docker=true",
			"--providers.docker.exposedbydefault=false",
		} {
			fmt.Fprintf(&b, "      - '%s'\n", flag)
		}
		b.WriteString("    labels:\n      - traefik.enable=false\n      - coolify.managed=true\n      - coolify.proxy=true\n")
		return b.String()
	}
}

func (g *ProxyConfigGenerator) Render(_ context.Context, proxyType string, resources []ProxyResource) (ProxyConfigBundle, error) {
	return g.RenderWithNetworks(proxyType, []string{"coolify"}, resources)
}

func (g *ProxyConfigGenerator) RenderWithNetworks(proxyType string, networks []string, resources []ProxyResource) (ProxyConfigBundle, error) {
	sorted := append([]ProxyResource(nil), resources...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].UUID < sorted[j].UUID })

	dynamic := make(map[string]string, len(sorted)+1)
	switch strings.ToLower(strings.TrimSpace(proxyType)) {
	case "traefik", "":
		composeYAML := g.DefaultComposeYAML("traefik", networks)
		// Base dynamic middleware for HTTP -> HTTPS redirect
		dynamic["coolify.yaml"] = "http:\n  middlewares:\n    redirect-to-https:\n      redirectScheme:\n        scheme: https\n"
		for _, r := range sorted {
			host := strings.TrimPrefix(strings.TrimPrefix(r.FQDN, "https://"), "http://")
			host = strings.Split(host, "/")[0]
			if host == "" {
				continue
			}
			dynamic[r.UUID+".yaml"] = fmt.Sprintf(
				"http:\n  routers:\n    %s:\n      rule: Host(`%s`)\n      service: svc-%s\n  services:\n    svc-%s:\n      loadBalancer:\n        servers:\n          - url: %q\n",
				r.UUID, host, r.UUID, r.UUID, r.Upstream,
			)
		}
		return ProxyConfigBundle{
			ProxyType:   "traefik",
			ComposeYAML: composeYAML,
			MainYAML:    composeYAML,
			Dynamic:     dynamic,
		}, nil
	case "caddy":
		composeYAML := g.DefaultComposeYAML("caddy", networks)
		var caddyfile strings.Builder
		for _, r := range sorted {
			if strings.TrimSpace(r.FQDN) != "" {
				fmt.Fprintf(&caddyfile, "%s {\n    reverse_proxy %s\n}\n", r.FQDN, r.Upstream)
			}
		}
		return ProxyConfigBundle{
			ProxyType:   "caddy",
			ComposeYAML: composeYAML,
			MainYAML:    caddyfile.String(),
			Dynamic:     dynamic,
		}, nil
	default:
		return ProxyConfigBundle{}, fmt.Errorf("unsupported proxy type %q", proxyType)
	}
}

// BuildStartProxyCommand generates the exact remote shell pipeline to write /data/coolify/proxy/docker-compose.yml,
// ensure all Docker networks exist, pull & start coolify-proxy, and connect it to all managed networks
// (parity with StartProxy.php & SaveProxyConfiguration.php).
func BuildStartProxyCommand(proxyType string, composeYAML string, dynamicFiles map[string]string, networks []string) string {
	nets := sanitizeNetworks(networks)
	proxyDir := "/data/coolify/proxy"
	composePath := proxyDir + "/docker-compose.yml"
	composeB64 := base64.StdEncoding.EncodeToString([]byte(composeYAML))

	cmds := []string{
		fmt.Sprintf("mkdir -p '%s/dynamic' '%s/backups'", proxyDir, proxyDir),
		fmt.Sprintf("if [ -f '%s' ]; then cp -f '%s' '%s/backups/docker-compose.prev.yml' 2>/dev/null || true; fi", composePath, composePath, proxyDir),
		fmt.Sprintf("printf %%s '%s' | base64 -d > '%s'", composeB64, composePath),
		fmt.Sprintf("touch '%s/acme.json' && chmod 600 '%s/acme.json' 2>/dev/null || true", proxyDir, proxyDir),
	}
	for name, content := range dynamicFiles {
		safeName := strings.ReplaceAll(strings.TrimSpace(name), "'", "")
		if safeName == "" || strings.Contains(safeName, "/") || strings.Contains(safeName, "..") {
			continue
		}
		b64 := base64.StdEncoding.EncodeToString([]byte(content))
		cmds = append(cmds, fmt.Sprintf("printf %%s '%s' | base64 -d > '%s/dynamic/%s'", b64, proxyDir, safeName))
	}
	for _, n := range nets {
		safeNet := strings.ReplaceAll(n, "'", "")
		cmds = append(cmds, fmt.Sprintf("docker network inspect '%s' >/dev/null 2>&1 || docker network create --attachable '%s'", safeNet, safeNet))
	}
	cmds = append(cmds,
		fmt.Sprintf("docker compose -f '%s' pull || true", composePath),
		"if docker ps -a --format '{{.Names}}' | grep -q '^coolify-proxy$'; then docker stop -t 30 coolify-proxy 2>/dev/null || true; docker rm -f coolify-proxy 2>/dev/null || true; fi",
		fmt.Sprintf("docker compose -f '%s' up -d --remove-orphans", composePath),
	)
	for _, n := range nets {
		safeNet := strings.ReplaceAll(n, "'", "")
		cmds = append(cmds, fmt.Sprintf("docker network connect '%s' coolify-proxy >/dev/null 2>&1 || true", safeNet))
	}
	return strings.Join(cmds, " && ")
}

// BuildStopProxyCommand stops and removes coolify-proxy and its sidecars (parity with StopProxy.php).
func BuildStopProxyCommand() string {
	return "docker stop -t 30 coolify-proxy 2>/dev/null || true; docker rm -f coolify-proxy coolify-proxy-logrotate 2>/dev/null || true"
}

func sanitizeNetworks(networks []string) []string {
	seen := make(map[string]bool)
	var out []string
	for _, n := range networks {
		n = strings.TrimSpace(n)
		if n == "" || n == "bridge" || n == "host" || n == "none" || n == "default" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	if len(out) == 0 {
		out = []string{"coolify"}
	}
	return out
}
