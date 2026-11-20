package services

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var ErrTemplateNotFound = errors.New("service template not found")

type ServiceTemplateInfo struct {
	Type          string   `json:"type"`
	Name          string   `json:"name"`
	Slogan        string   `json:"slogan"`
	Category      string   `json:"category"`
	Tags          []string `json:"tags"`
	Logo          string   `json:"logo"`
	Documentation string   `json:"documentation"`
	Port          string   `json:"port,omitempty"`
	ComposeRaw    string   `json:"docker_compose_raw,omitempty"`
}

type rawTemplateJSON struct {
	Documentation string   `json:"documentation"`
	Slogan        string   `json:"slogan"`
	Compose       string   `json:"compose"`
	Tags          []string `json:"tags"`
	Category      string   `json:"category"`
	Logo          string   `json:"logo"`
	Port          string   `json:"port"`
}

// ResolveTemplatesDir locates Coolify's bundled templates/ directory on host or inside container.
func ResolveTemplatesDir() string {
	candidates := []string{
		os.Getenv("COOLIFY_TEMPLATES_DIR"),
		"templates",
		filepath.Join("..", "templates"),
		filepath.Join("..", "..", "templates"),
		filepath.Join("..", "..", "..", "templates"),
		filepath.Join("..", "..", "..", "..", "templates"),
		"/app/templates",
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c
		}
	}
	return ""
}

// DecodeComposeInput decodes base64-encoded Compose YAML (matching Coolify REST API v1) or returns raw YAML untouched.
func DecodeComposeInput(rawOrBase64 string) string {
	trimmed := strings.TrimSpace(rawOrBase64)
	if trimmed == "" || strings.Contains(trimmed, "services:") || strings.Contains(trimmed, "\n") {
		return rawOrBase64
	}
	if decoded, err := base64.StdEncoding.DecodeString(trimmed); err == nil {
		s := string(decoded)
		if strings.Contains(s, "services:") || strings.Contains(s, "version:") {
			return s
		}
	}
	return rawOrBase64
}

func loadAllTemplates() (map[string]ServiceTemplateInfo, error) {
	dir := ResolveTemplatesDir()
	if dir == "" {
		return nil, fmt.Errorf("templates directory not found")
	}

	out := make(map[string]ServiceTemplateInfo)
	jsonPath := filepath.Join(dir, "service-templates-latest.json")
	if rawBytes, err := os.ReadFile(jsonPath); err == nil {
		var entries map[string]rawTemplateJSON
		if err := json.Unmarshal(rawBytes, &entries); err == nil {
			for slug, meta := range entries {
				composeYAML := ""
				if meta.Compose != "" {
					if dec, err := base64.StdEncoding.DecodeString(meta.Compose); err == nil {
						composeYAML = string(dec)
					}
				}
				if composeYAML == "" {
					if b, err := os.ReadFile(filepath.Join(dir, "compose", slug+".yaml")); err == nil {
						composeYAML = string(b)
					}
				}
				name := strings.ReplaceAll(strings.ToUpper(slug[:1])+slug[1:], "-", " ")
				if len(slug) <= 1 {
					name = strings.ToUpper(slug)
				}
				out[slug] = ServiceTemplateInfo{
					Type:          slug,
					Name:          name,
					Slogan:        meta.Slogan,
					Category:      meta.Category,
					Tags:          meta.Tags,
					Logo:          meta.Logo,
					Documentation: meta.Documentation,
					Port:          meta.Port,
					ComposeRaw:    composeYAML,
				}
			}
		}
	}

	// Merge any additional YAML files under templates/compose/*.yaml so every template is available.
	composeDir := filepath.Join(dir, "compose")
	if files, err := os.ReadDir(composeDir); err == nil {
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".yaml") {
				continue
			}
			slug := strings.TrimSuffix(f.Name(), ".yaml")
			if existing, ok := out[slug]; ok {
				if existing.ComposeRaw == "" {
					if b, err := os.ReadFile(filepath.Join(composeDir, f.Name())); err == nil {
						existing.ComposeRaw = string(b)
						out[slug] = existing
					}
				}
				continue
			}
			b, err := os.ReadFile(filepath.Join(composeDir, f.Name()))
			if err != nil {
				continue
			}
			out[slug] = ServiceTemplateInfo{
				Type:       slug,
				Name:       strings.ReplaceAll(strings.ToUpper(slug[:1])+slug[1:], "-", " "),
				Category:   "utilities",
				Tags:       []string{slug},
				Logo:       "svgs/" + slug + ".svg",
				ComposeRaw: string(b),
			}
		}
	}

	return out, nil
}

// ListServiceTemplates returns sorted Coolify one-click service templates.
func ListServiceTemplates(includeCompose bool) ([]ServiceTemplateInfo, error) {
	all, err := loadAllTemplates()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(all))
	for k := range all {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	res := make([]ServiceTemplateInfo, 0, len(keys))
	for _, k := range keys {
		item := all[k]
		if !includeCompose {
			item.ComposeRaw = ""
		}
		res = append(res, item)
	}
	return res, nil
}

// LookupServiceTemplate finds a one-click service template by exact or normalized slug.
func LookupServiceTemplate(slug string) (ServiceTemplateInfo, error) {
	norm := strings.ToLower(strings.TrimSpace(slug))
	if norm == "" {
		return ServiceTemplateInfo{}, ErrTemplateNotFound
	}
	all, err := loadAllTemplates()
	if err != nil {
		return ServiceTemplateInfo{}, err
	}
	if item, ok := all[norm]; ok {
		return item, nil
	}
	return ServiceTemplateInfo{}, fmt.Errorf("%w: %s", ErrTemplateNotFound, norm)
}
