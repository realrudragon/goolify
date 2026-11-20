package ops

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// ---- ProxyPortParser (parity with app/Services/ProxyPortParser.php) ----

type PortMapping struct {
	HostPort      int `json:"host_port"`
	ContainerPort int `json:"container_port"`
}

func ParseProxyPortMappings(raw string) []PortMapping {
	var out []PortMapping
	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		parts := strings.Split(entry, ":")
		if len(parts) == 1 {
			if p, err := strconv.Atoi(strings.TrimSpace(parts[0])); err == nil && p > 0 && p <= 65535 {
				out = append(out, PortMapping{HostPort: p, ContainerPort: p})
			}
			continue
		}
		if len(parts) >= 2 {
			hp, err1 := strconv.Atoi(strings.TrimSpace(parts[len(parts)-2]))
			cp, err2 := strconv.Atoi(strings.TrimSpace(parts[len(parts)-1]))
			if err1 == nil && err2 == nil && hp > 0 && hp <= 65535 && cp > 0 && cp <= 65535 {
				out = append(out, PortMapping{HostPort: hp, ContainerPort: cp})
			}
		}
	}
	return out
}

// ---- RepositoryDetector (parity with app/Services/RepositoryDetector.php) ----

type DetectedRepository struct {
	Provider string `json:"provider"` // github | gitlab | bitbucket | gitea | custom
	Host     string `json:"host"`
	Owner    string `json:"owner"`
	Repo     string `json:"repo"`
	FullName string `json:"full_name"`
}

func DetectRepositoryInfo(rawURL string) DetectedRepository {
	s := strings.TrimSpace(rawURL)
	s = strings.TrimSuffix(s, ".git")
	var host, pathPart string
	if strings.HasPrefix(s, "git@") {
		rest := strings.TrimPrefix(s, "git@")
		if idx := strings.Index(rest, ":"); idx > 0 {
			host = rest[:idx]
			pathPart = rest[idx+1:]
		}
	} else if u, err := url.Parse(s); err == nil && u.Host != "" {
		host = u.Hostname()
		pathPart = strings.Trim(u.Path, "/")
	}
	parts := strings.Split(strings.Trim(pathPart, "/"), "/")
	owner, repo := "", ""
	if len(parts) >= 2 {
		owner = parts[0]
		repo = strings.Join(parts[1:], "/")
	}
	prov := "custom"
	lh := strings.ToLower(host)
	switch {
	case strings.Contains(lh, "github"):
		prov = "github"
	case strings.Contains(lh, "gitlab"):
		prov = "gitlab"
	case strings.Contains(lh, "bitbucket"):
		prov = "bitbucket"
	case strings.Contains(lh, "gitea") || strings.Contains(lh, "codeberg"):
		prov = "gitea"
	}
	full := ""
	if owner != "" && repo != "" {
		full = owner + "/" + repo
	}
	return DetectedRepository{Provider: prov, Host: host, Owner: owner, Repo: repo, FullName: full}
}

// ---- SensitiveDataRedactor (parity with app/Services/Security/SensitiveDataRedactor.php) ----

var bearerPattern = regexp.MustCompile(`(?i)(authorization:\s*bearer\s+|token=|password=|secret=)[^\s&"']+`)

func RedactSensitiveData(input string, secrets ...string) string {
	out := input
	for _, s := range secrets {
		if len(s) >= 4 {
			out = strings.ReplaceAll(out, s, "[REDACTED]")
		}
	}
	out = bearerPattern.ReplaceAllString(out, "${1}[REDACTED]")
	return out
}
