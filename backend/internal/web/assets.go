package web

import (
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// ServeUIShell serves static Astro + Vue static assets and returns the appropriate HTML shell ONLY
// for allowlisted UI routes. API, /app/*, /mcp, webhook, realtime, terminal, and missing static assets
// strictly return 404 JSON/text and NEVER fall back to HTML.
func ServeUIShell(allowlist []*regexp.Regexp, staticFS fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(staticFS))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := path.Clean("/" + r.URL.Path)

		// Strict boundary: never fallback API/app/canvas-api/mcp/webhook/realtime/terminal-ws to HTML shell
		if strings.HasPrefix(p, "/api/") || p == "/api" ||
			strings.HasPrefix(p, "/app/") || p == "/app" ||
			strings.HasPrefix(p, "/canvas/api/") || p == "/canvas/api" ||
			strings.HasPrefix(p, "/mcp/") || p == "/mcp" ||
			strings.HasPrefix(p, "/webhooks/") || p == "/webhooks" ||
			strings.HasPrefix(p, "/realtime/") || p == "/realtime" ||
			strings.HasPrefix(p, "/terminal/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			return
		}

		if strings.HasPrefix(p, "/_astro/") || strings.HasPrefix(p, "/assets/") || strings.Contains(path.Base(p), ".") {
			rel := strings.TrimPrefix(p, "/")
			f, err := staticFS.Open(rel)
			if err != nil {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte("asset not found"))
				return
			}
			_ = f.Close()
			fileServer.ServeHTTP(w, r)
			return
		}

		for _, re := range allowlist {
			if re.MatchString(p) {
				shellBytes, err := resolvePageHTML(staticFS, p)
				if err != nil {
					http.Error(w, "ui shell missing", http.StatusInternalServerError)
					return
				}
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("X-Content-Type-Options", "nosniff")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(shellBytes)
				return
			}
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("route not found"))
	})
}

func resolvePageHTML(staticFS fs.FS, routePath string) ([]byte, error) {
	candidates := []string{"app/index.html", "index.html"}
	switch routePath {
	case "/login":
		candidates = []string{"login/index.html", "login.html"}
	case "/":
		candidates = []string{"index.html", "app/index.html"}
	}
	for _, cand := range candidates {
		if b, err := fs.ReadFile(staticFS, cand); err == nil {
			return b, nil
		}
	}
	return nil, fs.ErrNotExist
}

func DefaultUIAllowlist() []*regexp.Regexp {
	patterns := []string{
		`^/$`,
		`^/login$`,
		`^/force-password-reset$`,
		`^/two-factor-required$`,
		`^/vulnerabilities$`,
		`^/dashboard/canvas$`,
		`^/analytics$`,
		`^/metrics$`,
		`^/canvas(?:/tags)?$`,
		`^/admin$`,
		`^/onboarding$`,
		`^/select-team$`,
		`^/registries$`,
		`^/subscription(?:/new)?$`,
		`^/profile(?:/(?:appearance|passkeys/confirm))?$`,
		`^/servers(?:/new(?:/[A-Za-z0-9_-]+(?:/[A-Za-z0-9_-]+)?)?|/canvas|/[A-Za-z0-9_-]+(?:/[a-z-]+)?)?$`,
		`^/server/[A-Za-z0-9_-]+(?:/.*)?$`,
		`^/destinations(?:/[A-Za-z0-9_-]+)?$`,
		`^/destination/[A-Za-z0-9_-]+(?:/(?:danger|resources))?$`,
		`^/projects(?:/canvas|/[A-Za-z0-9_-]+(?:/environment/[A-Za-z0-9_-]+(?:/.*)?)?)?$`,
		`^/project/[A-Za-z0-9_-]+(?:/.*)?$`,
		`^/databases(?:/[A-Za-z0-9_-]+(?:/[a-z-]+)?)?$`,
		`^/services(?:/[A-Za-z0-9_-]+(?:/[a-z-]+)?)?$`,
		`^/deployments(?:/[A-Za-z0-9_-]+)?$`,
		`^/teams(?:/[A-Za-z0-9_-]+)?$`,
		`^/team(?:/(?:members|audit-log|image-scanning|admin|danger))?$`,
		`^/settings(?:/[A-Za-z0-9_-]+(?:/[A-Za-z0-9_-]+)?)?$`,
		`^/notifications(?:/[a-z-]+)?$`,
		`^/storages(?:/[A-Za-z0-9_-]+(?:/(?:danger|resources))?)?$`,
		`^/shared-variables(?:/.*)?$`,
		`^/tags(?:/[A-Za-z0-9_-]+)?$`,
		`^/security(?:/[A-Za-z0-9_-]+(?:/[A-Za-z0-9_-]+)?)?$`,
		`^/sources(?:/[A-Za-z0-9_-]+)?$`,
		`^/source/(?:github|gitlab|gitea)/[A-Za-z0-9_-]+(?:/(?:danger|permissions|resources))?$`,
		`^/transfers(?:/[A-Za-z0-9_-]+)?$`,
		`^/terminal$`,
	}
	out := make([]*regexp.Regexp, 0, len(patterns))
	for _, pat := range patterns {
		out = append(out, regexp.MustCompile(pat))
	}
	return out
}
