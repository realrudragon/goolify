package compose

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	ErrMissingRequiredVar = errors.New("required compose environment variable is not set")
	ErrInvalidComposeYAML = errors.New("invalid docker compose YAML")
	ErrUnsafeServiceName  = errors.New("invalid Docker Compose service name: shell metacharacters are not allowed")
	ErrUnsafeVolumePath   = errors.New("invalid Docker volume definition: shell metacharacters are not allowed")
)

type ComposeMode string

const (
	ModeService     ComposeMode = "service"
	ModeApplication ComposeMode = "application"
)

type ComposeParseInput struct {
	Mode                      ComposeMode // service (default) | application
	ParserVersion             string
	RawCompose                string
	ResourceUUID              string
	ResourceName              string
	ProjectName               string
	EnvironmentName           string
	ServerIP                  string
	TeamID                    int64
	ProxyType                 string // traefik | caddy | none
	GenerateExactLabels       bool
	EscapeContainerLabels     *bool // nil defaults to true (matching Coolify default)
	IsGzipEnabled             *bool // nil defaults to true
	IsStripPrefixEnabled      *bool // nil defaults to true
	FQDN                      string
	CustomDomains             map[string]string // serviceName -> custom FQDN (Step 2)
	MigratedTypes             map[string]string // serviceName -> "application" | "database"
	PullRequestID             int64
	PreviewURLTemplate        string
	Env                       map[string]string
	SharedEnv                 map[string]string // e.g., "environment.SHARED_KEY" -> "shared-value"
	DeterministicSeed         string
}

type ParsedVolume struct {
	Raw         string
	Type        string // volume | bind
	Source      string
	Rewritten   string // prefixed volume or /data/coolify/... bind path
	Target      string
	ReadOnly    bool
	IsDirectory bool
	Content     string
	External    bool
}

type ParsedService struct {
	Name              string
	Image             string
	Kind              string // application | database
	FQDN              string
	RequiredPort      int
	ExcludeFromStatus bool
	Environment       map[string]string
	Labels            map[string]string
	Volumes           []ParsedVolume
	Ports             []string
	Networks          []string
}

type ComposeParseOutput struct {
	Services        map[string]ParsedService
	Applications    map[string]ParsedService
	Databases       map[string]ParsedService
	GeneratedEnv    map[string]string
	RequiredVars    map[string]string // varName -> error message from ${VAR:?msg}
	DeclaredDomains map[string]string // serviceName -> generated/custom FQDN
	Networks        []string
	Volumes         []string
	RenderedYAML    string
}

type ComposeParser struct{}

func NewComposeParser() *ComposeParser {
	return &ComposeParser{}
}

var (
	unbracedVarPat   = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*)`)
	sharedScopePat   = regexp.MustCompile(`\{\{(team|project|environment)\.([A-Za-z0-9_]+)\}\}`)
	shellMetaPattern = regexp.MustCompile("[;|&`><\\n\\r]|\\$\\(")
	serviceDomainPat = regexp.MustCompile(`^SERVICE_(URL|FQDN)_([A-Z0-9_]+?)(?:_([0-9]{1,5}))?$`)
)

// knownDatabaseImages mirrors Coolify's database image detection for ServiceDatabase classification.
var knownDatabaseImages = []string{
	"postgres", "postgis", "timescale", "pgvector", "supabase/postgres",
	"mysql", "mariadb", "percona",
	"mongo", "mongodb", "ferretdb",
	"redis", "keydb", "dragonfly", "valkey",
	"clickhouse", "couchdb", "neo4j", "cassandra", "cockroachdb", "surrealdb", "qdrant", "meilisearch", "milvus",
}

func validateServiceName(name string) error {
	if name == "" || shellMetaPattern.MatchString(name) {
		return fmt.Errorf("%w: %q", ErrUnsafeServiceName, name)
	}
	return nil
}

func validateVolumeSource(src string) error {
	if src != "" && shellMetaPattern.MatchString(src) {
		return fmt.Errorf("%w: %q", ErrUnsafeVolumePath, src)
	}
	return nil
}

func isDatabaseImage(image string) bool {
	low := strings.ToLower(strings.TrimSpace(image))
	if low == "" {
		return false
	}
	// Strip registry prefix and tag
	if idx := strings.IndexByte(low, ':'); idx >= 0 {
		low = low[:idx]
	}
	parts := strings.Split(low, "/")
	base := parts[len(parts)-1]
	for _, dbImg := range knownDatabaseImages {
		if base == dbImg || low == dbImg || strings.HasPrefix(base, dbImg+"-") {
			return true
		}
	}
	return false
}

// Parse implements Coolify's serviceParser and applicationParser semantics:
// security validation (shell metacharacters in service names & volume sources),
// variable interpolation (${VAR:-def}, ${VAR:?err}, ${VAR-def}, nested refs, {{scope.KEY}}),
// magic variables (SERVICE_URL_*, SERVICE_FQDN_*, SERVICE_PASSWORD_*, SERVICE_BASE64_*, SERVICE_REALBASE64_*, SERVICE_HEX_*, SERVICE_USER_*),
// database vs application classification, volume rewriting, $$ label escaping, and Traefik/Caddy proxy label injection.
func (p *ComposeParser) Parse(_ context.Context, input ComposeParseInput) (ComposeParseOutput, error) {
	mode := input.Mode
	if mode == "" {
		mode = ModeService
	}
	serverIP := strings.TrimSpace(input.ServerIP)
	if serverIP == "" {
		serverIP = "127.0.0.1"
	}
	escapeLabels := true
	if input.EscapeContainerLabels != nil {
		escapeLabels = *input.EscapeContainerLabels
	}
	gzipEnabled := true
	if input.IsGzipEnabled != nil {
		gzipEnabled = *input.IsGzipEnabled
	}

	root, err := decodeComposeYAML([]byte(input.RawCompose))
	if err != nil {
		return ComposeParseOutput{}, fmt.Errorf("%w: %v", ErrInvalidComposeYAML, err)
	}
	rawServices, ok := root["services"].(map[string]any)
	if !ok {
		return ComposeParseOutput{}, fmt.Errorf("%w: missing services map", ErrInvalidComposeYAML)
	}

	// Collect top-level volume definitions (to detect external: true)
	externalVolumes := make(map[string]bool)
	if topVols, ok := root["volumes"].(map[string]any); ok {
		for vn, vdef := range topVols {
			if vm, ok := vdef.(map[string]any); ok {
				if ext, ok := vm["external"].(bool); ok && ext {
					externalVolumes[vn] = true
				}
			}
		}
	}

	// Validate EVERY service name before doing any work (fail-closed security boundary)
	svcNames := make([]string, 0, len(rawServices))
	for name := range rawServices {
		if err := validateServiceName(name); err != nil {
			return ComposeParseOutput{}, err
		}
		svcNames = append(svcNames, name)
	}
	sort.Strings(svcNames)

	envPool := make(map[string]string, len(input.Env))
	for k, v := range input.Env {
		envPool[k] = v
	}
	generated := make(map[string]string)
	requiredVars := make(map[string]string)
	declaredDomains := make(map[string]string)

	// Pass 1: Pre-scan SERVICE_URL_* and SERVICE_FQDN_* declarations to compute domains & ports per service
	serviceNameByNorm := make(map[string]string, len(svcNames))
	for _, sn := range svcNames {
		norm := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(sn, "-", "_"), ".", "_"))
		serviceNameByNorm[norm] = sn
	}

	for _, svcName := range svcNames {
		svcMap, ok := rawServices[svcName].(map[string]any)
		if !ok {
			continue
		}
		img, _ := svcMap["image"].(string)
		kind := "application"
		if isDatabaseImage(img) {
			kind = "database"
		}
		if forced, ok := input.MigratedTypes[svcName]; ok {
			kind = forced
		}
		rawEnv := extractEnvPairs(svcMap["environment"])
		for _, pair := range rawEnv {
			if m := serviceDomainPat.FindStringSubmatch(pair.Key); m != nil {
				kindWord := m[1]
				targetNorm := m[2]
				portStr := m[3]
				pathSuffix := ""
				if pair.HasValue && strings.HasPrefix(pair.Value, "/") {
					if mode == ModeService || kindWord == "URL" {
						pathSuffix = pair.Value
					}
				}
				hostSlug := strings.ToLower(targetNorm)
				if mode == ModeApplication {
					hostSlug = strings.ReplaceAll(hostSlug, "_", "-")
				}
				baseHost := stripScheme(input.FQDN)
				var computedHost string
				if custom, ok := input.CustomDomains[svcName]; ok && custom != "" {
					computedHost = stripScheme(custom)
				} else if baseHost != "" && input.FQDN != "" && len(svcNames) == 1 {
					computedHost = baseHost
				} else {
					computedHost = fmt.Sprintf("%s-%s.%s.sslip.io", hostSlug, input.ResourceUUID, serverIP)
				}
				scheme := "http://"
				if strings.HasPrefix(input.FQDN, "https://") {
					scheme = "https://"
				}
				if custom, ok := input.CustomDomains[svcName]; ok && strings.HasPrefix(custom, "https://") {
					scheme = "https://"
				}
				fullFQDN := computedHost + pathSuffix
				fullURL := scheme + fullFQDN

				baseFQDNKey := "SERVICE_FQDN_" + targetNorm
				baseURLKey := "SERVICE_URL_" + targetNorm
				if _, exists := envPool[baseFQDNKey]; !exists {
					envPool[baseFQDNKey] = fullFQDN
					generated[baseFQDNKey] = fullFQDN
				}
				if _, exists := envPool[baseURLKey]; !exists {
					envPool[baseURLKey] = fullURL
					generated[baseURLKey] = fullURL
				}
				if portStr != "" {
					envPool[baseFQDNKey+"_"+portStr] = fullFQDN + ":" + portStr
					envPool[baseURLKey+"_"+portStr] = fullURL + ":" + portStr
					generated[baseFQDNKey+"_"+portStr] = fullFQDN + ":" + portStr
					generated[baseURLKey+"_"+portStr] = fullURL + ":" + portStr
				}

				if kind == "application" && serviceNameByNorm[targetNorm] == svcName {
					if _, already := declaredDomains[svcName]; !already {
						if portStr != "" && mode == ModeApplication {
							declaredDomains[svcName] = fullURL + ":" + portStr
						} else {
							declaredDomains[svcName] = fullURL
						}
					}
				}
			}
		}
	}

	getOrGenMagic := func(key string) (string, bool) {
		if val, exists := envPool[key]; exists {
			return val, true
		}
		if m := serviceDomainPat.FindStringSubmatch(key); m != nil {
			kindStr, targetNorm, portStr := m[1], m[2], m[3]
			hostSlug := strings.ToLower(targetNorm)
			if mode == ModeApplication {
				hostSlug = strings.ReplaceAll(hostSlug, "_", "-")
			}
			h := fmt.Sprintf("%s-%s.%s.sslip.io", hostSlug, input.ResourceUUID, serverIP)
			if input.FQDN != "" && strings.HasPrefix(input.FQDN, "https://") {
				h = stripScheme(input.FQDN)
			}
			val := h
			if kindStr == "URL" {
				scheme := "http://"
				if strings.HasPrefix(input.FQDN, "https://") {
					scheme = "https://"
				}
				val = scheme + h
			}
			if portStr != "" {
				val = val + ":" + portStr
			}
			envPool[key] = val
			generated[key] = val
			return val, true
		}
		val, ok := generateMagicValue(key, input.FQDN, input.DeterministicSeed)
		if !ok {
			return "", false
		}
		envPool[key] = val
		generated[key] = val
		return val, true
	}

	out := ComposeParseOutput{
		Services:        make(map[string]ParsedService),
		Applications:    make(map[string]ParsedService),
		Databases:       make(map[string]ParsedService),
		GeneratedEnv:    generated,
		RequiredVars:    requiredVars,
		DeclaredDomains: declaredDomains,
		Networks:        []string{input.ResourceUUID},
	}

	for _, svcName := range svcNames {
		svcMap, ok := rawServices[svcName].(map[string]any)
		if !ok {
			continue
		}
		img, _ := svcMap["image"].(string)
		kind := "application"
		if isDatabaseImage(img) {
			kind = "database"
		}
		if forced, ok := input.MigratedTypes[svcName]; ok {
			kind = forced
		}

		excludeHC := false
		if ex, ok := svcMap["exclude_from_hc"].(bool); ok && ex {
			excludeHC = true
		}
		if rst, ok := svcMap["restart"].(string); ok && rst == "no" {
			excludeHC = true
		}

		ps := ParsedService{
			Name:              svcName,
			Image:             img,
			Kind:              kind,
			ExcludeFromStatus: excludeHC,
			Environment:       make(map[string]string),
			Labels:            make(map[string]string),
			Networks:          []string{input.ResourceUUID},
		}

		if dom, ok := declaredDomains[svcName]; ok {
			ps.FQDN = dom
		} else if custom, ok := input.CustomDomains[svcName]; ok && custom != "" {
			ps.FQDN = custom
			declaredDomains[svcName] = custom
		} else if input.FQDN != "" && kind == "application" && len(svcNames) == 1 {
			ps.FQDN = input.FQDN
		}

		// Parse environment (slice or map)
		rawEnv := extractEnvPairs(svcMap["environment"])
		for _, pair := range rawEnv {
			if m := serviceDomainPat.FindStringSubmatch(pair.Key); m != nil && m[3] != "" {
				if pNum, err := strconv.Atoi(m[3]); err == nil && ps.RequiredPort == 0 {
					ps.RequiredPort = pNum
				}
			}
			if !pair.HasValue {
				if val, ok := getOrGenMagic(pair.Key); ok {
					ps.Environment[pair.Key] = val
				} else if existing, ok := envPool[pair.Key]; ok {
					ps.Environment[pair.Key] = existing
				} else {
					ps.Environment[pair.Key] = ""
				}
				continue
			}

			// Resolve {{scope.KEY}} shared references first
			valWithShared := sharedScopePat.ReplaceAllStringFunc(pair.Value, func(match string) string {
				sub := sharedScopePat.FindStringSubmatch(match)
				lookupKey := sub[1] + "." + sub[2]
				if sv, ok := input.SharedEnv[lookupKey]; ok {
					return sv
				}
				return match
			})

			resolved, err := interpolateValue(valWithShared, envPool, getOrGenMagic, requiredVars, 0)
			if err != nil {
				return ComposeParseOutput{}, err
			}
			ps.Environment[pair.Key] = resolved
			envPool[pair.Key] = resolved
		}

		// Inject Coolify standard metadata env vars
		ps.Environment["COOLIFY_RESOURCE_UUID"] = input.ResourceUUID
		ps.Environment["COOLIFY_CONTAINER_NAME"] = fmt.Sprintf("%s-%s", svcName, input.ResourceUUID)
		if ps.FQDN != "" {
			ps.Environment["COOLIFY_URL"] = ps.FQDN
			ps.Environment["COOLIFY_FQDN"] = stripScheme(ps.FQDN)
		}
		if input.ServerIP != "" {
			ps.Environment["COOLIFY_SERVER_IP"] = input.ServerIP
		}
		for _, other := range svcNames {
			norm := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(other, "-", "_"), ".", "_"))
			ps.Environment["SERVICE_NAME_"+norm] = other
		}

		// Parse labels & apply $$ escaping when enabled
		for k, v := range extractStringMap(svcMap["labels"]) {
			resolved, err := interpolateValue(v, envPool, getOrGenMagic, requiredVars, 0)
			if err != nil {
				return ComposeParseOutput{}, err
			}
			if escapeLabels {
				resolved = escapeSingleDollars(resolved)
			}
			ps.Labels[k] = resolved
		}

		// Coolify ownership labels (UUID-based, never numeric IDs per .ai/lessons.md!)
		ps.Labels["coolify.managed"] = "true"
		ps.Labels["coolify.serviceName"] = svcName
		ps.Labels["coolify.pullRequestId"] = strconv.FormatInt(input.PullRequestID, 10)
		if mode == ModeApplication {
			ps.Labels["coolify.type"] = "application"
			ps.Labels["coolify.applicationUuid"] = input.ResourceUUID
		} else {
			ps.Labels["coolify.type"] = "service"
			ps.Labels["coolify.serviceUuid"] = input.ResourceUUID
			ps.Labels["coolify.service.subType"] = kind
			ps.Labels["coolify.service.subName"] = svcName
		}
		// Keep legacy coolify.applicationId label for backwards compatibility with existing checks
		ps.Labels["coolify.applicationId"] = input.ResourceUUID

		// Proxy labels (Traefik and/or Caddy depending on GenerateExactLabels & ProxyType)
		if ps.FQDN != "" && kind == "application" {
			emitTraefik := !input.GenerateExactLabels || strings.EqualFold(input.ProxyType, "traefik") || input.ProxyType == ""
			emitCaddy := !input.GenerateExactLabels || strings.EqualFold(input.ProxyType, "caddy")
			if strings.EqualFold(input.ProxyType, "none") {
				emitTraefik = false
				emitCaddy = false
			}
			hostOnly, pathOnly := splitHostAndPath(ps.FQDN)
			router := sanitizeRouterName(input.ResourceUUID + "-" + svcName)
			if emitTraefik {
				ps.Labels["traefik.enable"] = "true"
				ps.Labels["traefik.docker.network"] = input.ResourceUUID
				if pathOnly != "" && pathOnly != "/" {
					ps.Labels[fmt.Sprintf("traefik.http.routers.%s.rule", router)] = fmt.Sprintf("Host(`%s`) && PathPrefix(`%s`)", hostOnly, pathOnly)
				} else {
					ps.Labels[fmt.Sprintf("traefik.http.routers.%s.rule", router)] = fmt.Sprintf("Host(`%s`)", hostOnly)
				}
				if gzipEnabled {
					ps.Labels["traefik.http.middlewares.gzip-no-media.compress.excludedcontenttypes"] = "text/event-stream,video/mp4,video/webm"
				}
				if ps.RequiredPort > 0 {
					ps.Labels[fmt.Sprintf("traefik.http.services.%s.loadbalancer.server.port", router)] = strconv.Itoa(ps.RequiredPort)
				}
			}
			if emitCaddy {
				ps.Labels["caddy_0"] = ps.FQDN
				ps.Labels["caddy_ingress_network"] = input.ResourceUUID
				if ps.RequiredPort > 0 {
					ps.Labels["caddy_0.handle_path.0_reverse_proxy"] = fmt.Sprintf("{{upstreams %d}}", ps.RequiredPort)
				}
			}
		}

		// Parse ports (both string and object syntax)
		if rawPorts, ok := svcMap["ports"].([]any); ok {
			for _, pr := range rawPorts {
				switch pv := pr.(type) {
				case string:
					ps.Ports = append(ps.Ports, pv)
				case map[string]any:
					pub := fmt.Sprintf("%v", pv["published"])
					tgt := fmt.Sprintf("%v", pv["target"])
					proto, _ := pv["protocol"].(string)
					entry := pub + ":" + tgt
					if proto != "" {
						entry += "/" + proto
					}
					ps.Ports = append(ps.Ports, entry)
				}
			}
		}

		// Parse & validate volumes (reject shell metacharacters in sources!)
		baseDir := "/data/coolify/services/" + input.ResourceUUID
		if mode == ModeApplication {
			baseDir = "/data/coolify/applications/" + input.ResourceUUID
		}
		if vols, ok := svcMap["volumes"].([]any); ok {
			for _, v := range vols {
				pv, err := parseVolume(v, envPool, getOrGenMagic, requiredVars, input.ResourceUUID, baseDir, externalVolumes)
				if err != nil {
					return ComposeParseOutput{}, err
				}
				ps.Volumes = append(ps.Volumes, pv)
				if pv.Type == "volume" && pv.Rewritten != "" {
					out.Volumes = append(out.Volumes, pv.Rewritten)
				}
			}
		}

		out.Services[svcName] = ps
		if kind == "database" {
			out.Databases[svcName] = ps
		} else {
			out.Applications[svcName] = ps
		}
	}

	renderedBytes, err := yaml.Marshal(out.Services)
	if err != nil {
		return ComposeParseOutput{}, err
	}
	out.RenderedYAML = string(renderedBytes)
	return out, nil
}

type envPair struct {
	Key      string
	Value    string
	HasValue bool
}

func extractEnvPairs(raw any) []envPair {
	var pairs []envPair
	switch t := raw.(type) {
	case []any:
		for _, item := range t {
			s, ok := item.(string)
			if !ok {
				continue
			}
			if idx := strings.IndexByte(s, '='); idx >= 0 {
				pairs = append(pairs, envPair{Key: s[:idx], Value: s[idx+1:], HasValue: true})
			} else {
				pairs = append(pairs, envPair{Key: s, HasValue: false})
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if t[k] == nil {
				pairs = append(pairs, envPair{Key: k, HasValue: false})
			} else {
				pairs = append(pairs, envPair{Key: k, Value: fmt.Sprintf("%v", t[k]), HasValue: true})
			}
		}
	}
	return pairs
}

func extractStringMap(raw any) map[string]string {
	out := make(map[string]string)
	switch t := raw.(type) {
	case map[string]any:
		for k, v := range t {
			out[k] = fmt.Sprintf("%v", v)
		}
	case []any:
		for _, item := range t {
			if s, ok := item.(string); ok {
				if idx := strings.IndexByte(s, '='); idx >= 0 {
					out[s[:idx]] = s[idx+1:]
				}
			}
		}
	}
	return out
}

func parseVolume(raw any, env map[string]string, gen func(string) (string, bool), reqVars map[string]string, resUUID string, baseDir string, externalVols map[string]bool) (ParsedVolume, error) {
	switch v := raw.(type) {
	case string:
		interp, err := interpolateValue(v, env, gen, reqVars, 0)
		if err != nil {
			return ParsedVolume{}, err
		}
		parts := strings.Split(interp, ":")
		pv := ParsedVolume{Raw: interp, Type: "volume"}
		if len(parts) >= 2 {
			pv.Source = parts[0]
			pv.Target = parts[1]
			if err := validateVolumeSource(pv.Source); err != nil {
				return ParsedVolume{}, err
			}
			if strings.HasPrefix(pv.Source, "./") || strings.HasPrefix(pv.Source, "/") {
				pv.Type = "bind"
				pv.IsDirectory = true
				if strings.HasPrefix(pv.Source, "./") {
					pv.Rewritten = baseDir + "/" + strings.TrimPrefix(pv.Source, "./")
				} else {
					pv.Rewritten = pv.Source
				}
			} else {
				pv.External = externalVols[pv.Source]
				if pv.External {
					pv.Rewritten = pv.Source
				} else {
					pv.Rewritten = resUUID + "_" + pv.Source
				}
			}
			if len(parts) >= 3 && strings.Contains(parts[2], "ro") {
				pv.ReadOnly = true
			}
		}
		return pv, nil
	case map[string]any:
		src := ""
		if s, ok := v["source"].(string); ok {
			interp, err := interpolateValue(s, env, gen, reqVars, 0)
			if err != nil {
				return ParsedVolume{}, err
			}
			src = interp
		}
		if err := validateVolumeSource(src); err != nil {
			return ParsedVolume{}, err
		}
		tgt := ""
		if t, ok := v["target"].(string); ok {
			tgt = t
		}
		typ := "volume"
		if ty, ok := v["type"].(string); ok && ty != "" {
			typ = ty
		}
		pv := ParsedVolume{Type: typ, Source: src, Target: tgt}
		if ro, ok := v["read_only"].(bool); ok {
			pv.ReadOnly = ro
		}
		if content, ok := v["content"].(string); ok {
			pv.Content = content
			pv.IsDirectory = false
		} else if typ == "bind" {
			pv.IsDirectory = true
		}
		if typ == "bind" && strings.HasPrefix(src, "./") {
			pv.Rewritten = baseDir + "/" + strings.TrimPrefix(src, "./")
		} else if typ == "volume" && src != "" {
			pv.External = externalVols[src]
			if pv.External {
				pv.Rewritten = src
			} else {
				pv.Rewritten = resUUID + "_" + src
			}
		} else {
			pv.Rewritten = src
		}
		return pv, nil
	default:
		return ParsedVolume{}, nil
	}
}

func interpolateValue(val string, env map[string]string, gen func(string) (string, bool), reqVars map[string]string, depth int) (string, error) {
	if depth > 8 {
		return val, nil
	}
	out := val
	for pass := 0; pass < 4 && strings.Contains(out, "${"); pass++ {
		var passErr error
		next := replaceBalancedBracedVars(out, func(varName, op, rhs string) string {
			if passErr != nil {
				return ""
			}
			if v, ok := env[varName]; ok && (v != "" || op == "-") {
				return v
			}
			if mv, ok := gen(varName); ok {
				return mv
			}
			switch op {
			case ":-", "-":
				sub, err := interpolateValue(rhs, env, gen, reqVars, depth+1)
				if err != nil {
					passErr = err
					return ""
				}
				return sub
			case ":?":
				reqVars[varName] = rhs
				passErr = fmt.Errorf("%w: %s (%s)", ErrMissingRequiredVar, varName, rhs)
				return ""
			default:
				return ""
			}
		})
		if passErr != nil {
			return "", passErr
		}
		if next == out {
			break
		}
		out = next
	}

	out = unbracedVarPat.ReplaceAllStringFunc(out, func(m string) string {
		name := m[1:]
		if v, ok := env[name]; ok {
			return v
		}
		if mv, ok := gen(name); ok {
			return mv
		}
		return m
	})
	return out, nil
}

func replaceBalancedBracedVars(s string, fn func(name, op, rhs string) string) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		// Preserve escaped $${VAR} literal (used in healthchecks like pg_isready -U $${POSTGRES_USER})
		if i+2 < len(s) && s[i] == '$' && s[i+1] == '$' && s[i+2] == '{' {
			b.WriteString("$${")
			i += 3
			continue
		}
		if i+1 < len(s) && s[i] == '$' && s[i+1] == '{' {
			depth := 1
			j := i + 2
			for j < len(s) && depth > 0 {
				if s[j] == '$' && j+1 < len(s) && s[j+1] == '{' {
					depth++
					j += 2
					continue
				}
				if s[j] == '}' {
					depth--
					if depth == 0 {
						break
					}
				}
				j++
			}
			if depth == 0 {
				inner := s[i+2 : j]
				name, op, rhs := splitVarExpr(inner)
				b.WriteString(fn(name, op, rhs))
				i = j + 1
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func splitVarExpr(inner string) (name, op, rhs string) {
	for _, cand := range []string{":-", ":?", "-"} {
		if idx := strings.Index(inner, cand); idx > 0 {
			return inner[:idx], cand, inner[idx+len(cand):]
		}
	}
	return inner, "", ""
}

// generateMagicValue matches Coolify's exact lengths from tests/Fixtures/parser/expected/reference.yaml.
func generateMagicValue(key string, fqdn string, seed string) (string, bool) {
	host := stripScheme(fqdn)
	if host == "" {
		host = "localhost"
	}
	switch {
	case strings.HasPrefix(key, "SERVICE_FQDN_"):
		return host, true
	case strings.HasPrefix(key, "SERVICE_URL_"):
		return "https://" + host, true
	case strings.HasPrefix(key, "SERVICE_USER_"):
		return deterministicToken(key, seed, 16), true
	case strings.HasPrefix(key, "SERVICE_LOWERCASEUSER_"):
		return strings.ToLower(deterministicToken(key, seed, 16)), true
	case strings.HasPrefix(key, "SERVICE_PASSWORD_64_"), strings.HasPrefix(key, "SERVICE_PASSWORDWITHSYMBOLS_64_"):
		return deterministicToken(key, seed, 64), true
	case strings.HasPrefix(key, "SERVICE_PASSWORD_"), strings.HasPrefix(key, "SERVICE_PASSWORDWITHSYMBOLS_"):
		return deterministicToken(key, seed, 32), true
	case strings.HasPrefix(key, "SERVICE_REALBASE64_128_"):
		// base64 of 128 bytes = 172 chars (matches <generated:SERVICE_REALBASE64_128_WEB:172>)
		return base64.StdEncoding.EncodeToString(deterministicBytes(key, seed, 128)), true
	case strings.HasPrefix(key, "SERVICE_REALBASE64_64_"):
		// base64 of 64 bytes = 88 chars (matches <generated:SERVICE_REALBASE64_64_WEB:88>)
		return base64.StdEncoding.EncodeToString(deterministicBytes(key, seed, 64)), true
	case strings.HasPrefix(key, "SERVICE_REALBASE64_32_"):
		return base64.StdEncoding.EncodeToString(deterministicBytes(key, seed, 32)), true
	case strings.HasPrefix(key, "SERVICE_BASE64_128_"):
		return deterministicToken(key, seed, 128), true
	case strings.HasPrefix(key, "SERVICE_BASE64_64_"):
		return deterministicToken(key, seed, 64), true
	case strings.HasPrefix(key, "SERVICE_BASE64_32_"), strings.HasPrefix(key, "SERVICE_BASE64_"):
		return deterministicToken(key, seed, 32), true
	case strings.HasPrefix(key, "SERVICE_HEX_128_"):
		return deterministicHex(key, seed, 128), true
	case strings.HasPrefix(key, "SERVICE_HEX_64_"):
		return deterministicHex(key, seed, 64), true
	case strings.HasPrefix(key, "SERVICE_HEX_32_"):
		return deterministicHex(key, seed, 32), true
	default:
		return "", false
	}
}

func deterministicBytes(key, seed string, n int) []byte {
	if seed == "" {
		buf := make([]byte, n)
		_, _ = rand.Read(buf)
		return buf
	}
	out := make([]byte, 0, n)
	for round := 0; len(out) < n; round++ {
		h := sha256.Sum256([]byte(seed + ":bytes:" + key + ":" + strconv.Itoa(round)))
		out = append(out, h[:]...)
	}
	return out[:n]
}

func deterministicToken(key, seed string, length int) string {
	if seed == "" {
		buf := make([]byte, (length+1)/2)
		_, _ = rand.Read(buf)
		return hex.EncodeToString(buf)[:length]
	}
	var b strings.Builder
	for round := 0; b.Len() < length; round++ {
		h := sha256.Sum256([]byte(seed + ":" + key + ":" + strconv.Itoa(round)))
		b.WriteString(hex.EncodeToString(h[:]))
	}
	return b.String()[:length]
}

func deterministicHex(key, seed string, length int) string {
	return deterministicToken("hex:"+key, seed, length)
}

func escapeSingleDollars(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '$' {
			if i+1 < len(s) && s[i+1] == '$' {
				b.WriteString("$$")
				i++
				continue
			}
			b.WriteString("$$")
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func stripScheme(u string) string {
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	return strings.Split(u, "/")[0]
}

func splitHostAndPath(rawURL string) (string, string) {
	if !strings.Contains(rawURL, "://") {
		rawURL = "http://" + rawURL
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return stripScheme(rawURL), ""
	}
	return u.Hostname(), u.Path
}

func sanitizeRouterName(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, strings.ToLower(s))
}

// decodeComposeYAML parses YAML into Go maps/slices while supporting Symfony-compatible `<<: *seq_anchor`
// merges (used by official Coolify templates like langfuse.yaml and gramps-web.yaml).
func decodeComposeYAML(data []byte) (map[string]any, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, errors.New("empty YAML document")
	}
	val, err := nodeToAny(doc.Content[0])
	if err != nil {
		return nil, err
	}
	m, ok := val.(map[string]any)
	if !ok {
		return nil, errors.New("top-level compose YAML must be a mapping")
	}
	return m, nil
}

func nodeToAny(n *yaml.Node) (any, error) {
	if n == nil {
		return nil, nil
	}
	if n.Kind == yaml.AliasNode {
		return nodeToAny(n.Alias)
	}
	switch n.Kind {
	case yaml.MappingNode:
		// Check if mapping consists solely of `<<: *seq_alias` (e.g., gramps-web.yaml volumes)
		if len(n.Content) == 2 && n.Content[0].Value == "<<" {
			mergedVal, err := nodeToAny(n.Content[1])
			if err != nil {
				return nil, err
			}
			if seq, ok := mergedVal.([]any); ok {
				return seq, nil
			}
		}
		out := make(map[string]any)
		for i := 0; i+1 < len(n.Content); i += 2 {
			kNode := n.Content[i]
			vNode := n.Content[i+1]
			if kNode.Value == "<<" {
				merged, err := nodeToAny(vNode)
				if err != nil {
					return nil, err
				}
				switch mt := merged.(type) {
				case map[string]any:
					for mk, mv := range mt {
						if _, exists := out[mk]; !exists {
							out[mk] = mv
						}
					}
				case []any:
					// Symfony-compatible merge of sequence of maps OR sequence of KEY=VAL env strings (e.g. langfuse.yaml)
					for _, elem := range mt {
						switch et := elem.(type) {
						case map[string]any:
							for mk, mv := range et {
								if _, exists := out[mk]; !exists {
									out[mk] = mv
								}
							}
						case string:
							if idx := strings.IndexByte(et, '='); idx >= 0 {
								ek, ev := et[:idx], et[idx+1:]
								if _, exists := out[ek]; !exists {
									out[ek] = ev
								}
							} else if _, exists := out[et]; !exists {
								out[et] = nil
							}
						}
					}
				}
				continue
			}
			v, err := nodeToAny(vNode)
			if err != nil {
				return nil, err
			}
			out[kNode.Value] = v
		}
		return out, nil
	case yaml.SequenceNode:
		out := make([]any, 0, len(n.Content))
		for _, child := range n.Content {
			v, err := nodeToAny(child)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case yaml.ScalarNode:
		var v any
		if err := n.Decode(&v); err != nil {
			return n.Value, nil
		}
		return v, nil
	default:
		return nil, nil
	}
}
