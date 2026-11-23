package deployments

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/coollabsio/coolify/backend/internal/domains/deploymentcontext"
)

type ExecutionPlan struct {
	ApplicationUUID string
	BuildPack       string
	InputRevision   string
	ServerUUID      string
	Commands        []string
	Labels          map[string]string
	PreserveHealthy bool
}

type DeploymentPlanner struct{}

func NewDeploymentPlanner() *DeploymentPlanner {
	return &DeploymentPlanner{}
}

// BuildPlan compiles a deterministic, fresh-host-ready command plan for Docker Image, Dockerfile,
// Railpack, Compose, Static, or legacy Nixpacks (per frozen baseline compatibility policy).
func (p *DeploymentPlanner) BuildPlan(dctx deploymentcontext.DeploymentContext, operationID string, pullSecretRef string) (ExecutionPlan, error) {
	if dctx.ApplicationUUID == "" || dctx.ServerUUID == "" {
		return ExecutionPlan{}, fmt.Errorf("incomplete deployment context")
	}

	labels := map[string]string{
		"coolify.managed":          "true",
		"coolify.application_uuid": dctx.ApplicationUUID,
		"coolify.operation_id":     operationID,
		"coolify.input_revision":   dctx.InputRevision,
	}
	if pullSecretRef != "" {
		labels["coolify.registry_ref"] = pullSecretRef
	}

	revTag := dctx.InputRevision
	if len(revTag) > 12 {
		revTag = revTag[:12]
	}
	if revTag == "" {
		revTag = "latest"
	}

	network := strings.TrimSpace(dctx.DockerNetwork)
	if network == "" {
		network = "coolify"
	}

	var cmds []string
	// Always write .env for runtime variables and materialize any attached file mounts.
	cmds = append(cmds, writeFileB64Cmd(".env", buildDotEnvContent(dctx.EnvEntries)))
	for _, fm := range dctx.FileVolumes {
		hostPath := strings.TrimSpace(fm.FSPath)
		if hostPath == "" {
			continue
		}
		if fm.IsDirectory {
			cmds = append(cmds, fmt.Sprintf("mkdir -p %s", shellQuote(hostPath)))
		} else {
			cmds = append(cmds, writeFileB64Cmd(hostPath, fm.Content))
		}
	}

	ensureNetCmd := fmt.Sprintf("docker network inspect %s >/dev/null 2>&1 || docker network create --attachable %s", shellQuote(network), shellQuote(network))

	switch dctx.BuildPack {
	case "dockerimage":
		img := strings.TrimSpace(dctx.Settings["docker_registry_image_name"])
		if img == "" {
			img = "nginx:alpine"
		}
		tag := strings.TrimSpace(dctx.Settings["docker_registry_image_tag"])
		if tag != "" && !strings.Contains(img, ":") {
			img = img + ":" + tag
		}
		composeYAML := buildGeneratedComposeYAML(dctx, img, network, labels)
		cmds = append(cmds,
			ensureNetCmd,
			writeFileB64Cmd("docker-compose.yaml", composeYAML),
			fmt.Sprintf("docker pull %s", shellQuote(img)),
			fmt.Sprintf("docker compose --project-name %s -f docker-compose.yaml up -d --remove-orphans", shellQuote(dctx.ApplicationUUID)),
		)
	case "dockerfile":
		imageRef := "coolify/" + dctx.ApplicationUUID + ":" + revTag
		dfLoc := strings.TrimPrefix(orDefault(dctx.Settings["dockerfile_location"], "Dockerfile"), "/")
		baseDir := normalizeBaseDir(dctx.Settings["base_directory"])
		dfPath := path.Join(baseDir, dfLoc)
		inlineDF := strings.TrimSpace(dctx.Settings["dockerfile"])
		primaryPort := orDefault(firstExposedPort(dctx.Settings["ports_exposes"]), "80")
		if inlineDF != "" && strings.TrimSpace(dctx.Settings["git_repository"]) == "" {
			cmds = append(cmds, writeFileB64Cmd(dfLoc, dctx.Settings["dockerfile"]))
		} else {
			cmds = append(cmds,
				gitCloneOrFetchCmd(dctx),
				buildAutoDetectDockerfileCmd(dfPath, baseDir, primaryPort),
			)
		}
		buildArgs := buildDockerBuildArgs(dctx)
		noCache := ""
		if dctx.Settings["disable_build_cache"] == "true" {
			noCache = "--no-cache "
		}
		composeYAML := buildGeneratedComposeYAML(dctx, imageRef, network, labels)
		cmds = append(cmds,
			ensureNetCmd,
			fmt.Sprintf("docker build %s-f %s%s -t %s %s", noCache, shellQuote(dfPath), buildArgs, shellQuote(imageRef), shellQuote(baseDir)),
			writeFileB64Cmd("docker-compose.yaml", composeYAML),
			fmt.Sprintf("docker compose --project-name %s -f docker-compose.yaml up -d --remove-orphans", shellQuote(dctx.ApplicationUUID)),
		)
	case "railpack":
		imageRef := "coolify/" + dctx.ApplicationUUID + ":" + revTag
		baseDir := normalizeBaseDir(dctx.Settings["base_directory"])
		dfPath := path.Join(baseDir, "Dockerfile")
		primaryPort := orDefault(firstExposedPort(dctx.Settings["ports_exposes"]), "80")
		buildArgs := buildDockerBuildArgs(dctx)
		composeYAML := buildGeneratedComposeYAML(dctx, imageRef, network, labels)
		cmds = append(cmds,
			gitCloneOrFetchCmd(dctx),
			ensureNetCmd,
			fmt.Sprintf(
				"if command -v railpack >/dev/null 2>&1; then railpack build --name %s %s; else %s && docker build -f %s%s -t %s %s; fi",
				shellQuote(imageRef), shellQuote(baseDir),
				buildAutoDetectDockerfileCmd(dfPath, baseDir, primaryPort),
				shellQuote(dfPath), buildArgs, shellQuote(imageRef), shellQuote(baseDir),
			),
			writeFileB64Cmd("docker-compose.yaml", composeYAML),
			fmt.Sprintf("docker compose --project-name %s -f docker-compose.yaml up -d --remove-orphans", shellQuote(dctx.ApplicationUUID)),
		)
	case "dockercompose":
		composeLoc := strings.TrimPrefix(orDefault(dctx.Settings["docker_compose_location"], "docker-compose.yaml"), "/")
		if strings.TrimSpace(dctx.Settings["git_repository"]) != "" {
			cmds = append(cmds, gitCloneOrFetchCmd(dctx))
		}
		if strings.TrimSpace(dctx.Settings["docker_compose_raw"]) != "" {
			cmds = append(cmds, writeFileB64Cmd(composeLoc, dctx.Settings["docker_compose_raw"]))
		}
		cmds = append(cmds,
			ensureNetCmd,
			fmt.Sprintf("docker compose --project-name %s -f %s up -d --remove-orphans", shellQuote(dctx.ApplicationUUID), shellQuote(composeLoc)),
		)
	case "static":
		imageRef := "coolify/" + dctx.ApplicationUUID + ":" + revTag
		if strings.TrimSpace(dctx.Settings["git_repository"]) != "" {
			cmds = append(cmds, gitCloneOrFetchCmd(dctx))
		}
		staticDF := buildStaticDockerfile(dctx.Settings["publish_directory"], dctx.Settings["is_spa"] == "true")
		composeYAML := buildGeneratedComposeYAML(dctx, imageRef, network, labels)
		cmds = append(cmds,
			ensureNetCmd,
			writeFileB64Cmd("Dockerfile.static", staticDF),
			fmt.Sprintf("docker build -f Dockerfile.static -t %s .", shellQuote(imageRef)),
			writeFileB64Cmd("docker-compose.yaml", composeYAML),
			fmt.Sprintf("docker compose --project-name %s -f docker-compose.yaml up -d --remove-orphans", shellQuote(dctx.ApplicationUUID)),
		)
	case "nixpacks":
		// Explicit compatibility policy from Task 1: allow read/legacy build only when flagged
		if dctx.Settings["legacy_nixpacks_confirmed"] != "true" {
			return ExecutionPlan{}, fmt.Errorf("legacy nixpacks deployment requires explicit compatibility confirmation before cutover")
		}
		cmds = append(cmds,
			fmt.Sprintf("nixpacks build . --name %s", shellQuote("coolify/"+dctx.ApplicationUUID)),
		)
	default:
		return ExecutionPlan{}, fmt.Errorf("unsupported build pack %q", dctx.BuildPack)
	}

	if strings.TrimSpace(dctx.Settings["fqdn"]) != "" {
		cmds = append(cmds, buildEnsureTraefikProxyCmd(network))
	}

	return ExecutionPlan{
		ApplicationUUID: dctx.ApplicationUUID,
		BuildPack:       dctx.BuildPack,
		InputRevision:   dctx.InputRevision,
		ServerUUID:      dctx.ServerUUID,
		Commands:        cmds,
		Labels:          labels,
		PreserveHealthy: true,
	}, nil
}

func gitCloneOrFetchCmd(dctx deploymentcontext.DeploymentContext) string {
	repo := strings.TrimSpace(dctx.Settings["git_repository"])
	if repo == "" {
		return "git fetch --depth=1"
	}
	branch := orDefault(dctx.Settings["git_branch"], "main")
	depthFlag := ""
	if dctx.Settings["is_git_shallow_clone_enabled"] != "false" {
		depthFlag = " --depth=1"
	}
	submodFlag := ""
	if dctx.Settings["is_git_submodules_enabled"] == "true" {
		submodFlag = " --recurse-submodules"
	}
	gitCore := fmt.Sprintf(
		"if [ -d .git ]; then git fetch%s origin %s && git reset --hard FETCH_HEAD; else git clone%s%s --branch %s %s .; fi",
		depthFlag, shellQuote(branch), depthFlag, submodFlag, shellQuote(branch), shellQuote(repo),
	)
	if strings.TrimSpace(dctx.DeployPrivateKey) == "" {
		return gitCore
	}
	// Bind private repository SSH deploy key via an ephemeral 0600 temp file cleaned up immediately.
	keyB64 := base64.StdEncoding.EncodeToString([]byte(strings.TrimRight(dctx.DeployPrivateKey, "\r\n") + "\n"))
	return fmt.Sprintf(
		`(k=$(mktemp); trap 'rm -f "$k"' EXIT; printf %%s %s | base64 -d > "$k"; chmod 600 "$k"; GIT_SSH_COMMAND="ssh -i $k -o StrictHostKeyChecking=accept-new -o IdentitiesOnly=yes" sh -c %s)`,
		shellQuote(keyB64), shellQuote(gitCore),
	)
}

func writeFileB64Cmd(filePath, content string) string {
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	dir := path.Dir(filePath)
	if dir != "." && dir != "/" && dir != "" {
		return fmt.Sprintf("mkdir -p %s && printf %%s %s | base64 -d > %s", shellQuote(dir), shellQuote(encoded), shellQuote(filePath))
	}
	return fmt.Sprintf("printf %%s %s | base64 -d > %s", shellQuote(encoded), shellQuote(filePath))
}

func buildDotEnvContent(entries []deploymentcontext.EnvVarRecord) string {
	var b strings.Builder
	for _, e := range entries {
		if !e.IsRuntime || strings.TrimSpace(e.Key) == "" {
			continue
		}
		val := strings.ReplaceAll(e.Value, "\r", "")
		if strings.ContainsAny(val, " \t\n#\"'$") {
			escaped := strings.ReplaceAll(val, `\`, `\\`)
			escaped = strings.ReplaceAll(escaped, `"`, `\"`)
			escaped = strings.ReplaceAll(escaped, "\n", `\n`)
			fmt.Fprintf(&b, "%s=\"%s\"\n", e.Key, escaped)
		} else {
			fmt.Fprintf(&b, "%s=%s\n", e.Key, val)
		}
	}
	return b.String()
}

func buildDockerBuildArgs(dctx deploymentcontext.DeploymentContext) string {
	if dctx.Settings["inject_build_args_to_dockerfile"] == "false" {
		return ""
	}
	var b strings.Builder
	for _, e := range dctx.EnvEntries {
		if !e.IsBuildtime || strings.TrimSpace(e.Key) == "" {
			continue
		}
		fmt.Fprintf(&b, " --build-arg %s", shellQuote(e.Key+"="+e.Value))
	}
	return b.String()
}

func buildStaticDockerfile(publishDir string, isSPA bool) string {
	dir := strings.TrimPrefix(strings.TrimSpace(publishDir), "/")
	if dir == "" {
		dir = "."
	}
	var b strings.Builder
	b.WriteString("FROM nginx:alpine\n")
	fmt.Fprintf(&b, "COPY %s /usr/share/nginx/html\n", dir)
	if isSPA {
		b.WriteString("RUN printf 'server { listen 80; root /usr/share/nginx/html; index index.html; location / { try_files $uri $uri/ /index.html; } }\\n' > /etc/nginx/conf.d/default.conf\n")
	}
	b.WriteString("EXPOSE 80\n")
	return b.String()
}

func buildGeneratedComposeYAML(dctx deploymentcontext.DeploymentContext, imageRef, network string, baseLabels map[string]string) string {
	svcName := sanitizeComposeServiceName(dctx.ApplicationUUID)
	allLabels := make(map[string]string, len(baseLabels)+12)
	for k, v := range baseLabels {
		allLabels[k] = v
	}

	primaryPort := firstExposedPort(dctx.Settings["ports_exposes"])
	if primaryPort == "" {
		primaryPort = "80"
	}

	// Traefik proxy labels from FQDN (comma-separated URLs like https://app.example.com or https://app.example.com:3000)
	fqdnRaw := strings.TrimSpace(dctx.Settings["fqdn"])
	if fqdnRaw != "" {
		allLabels["traefik.enable"] = "true"
		allLabels["traefik.http.middlewares.gzip.compress"] = "true"
		certResolver := orDefault(dctx.Settings["traefik_certificate_resolver"], "letsencrypt")
		forceHTTPS := dctx.Settings["is_force_https_enabled"] != "false"
		redirectMode := strings.ToLower(strings.TrimSpace(dctx.Settings["redirect"]))
		for idx, rawEntry := range strings.Split(fqdnRaw, ",") {
			rawEntry = strings.TrimSpace(rawEntry)
			if rawEntry == "" {
				continue
			}
			if !strings.Contains(rawEntry, "://") {
				rawEntry = "http://" + rawEntry
			}
			u, err := url.Parse(rawEntry)
			if err != nil || u.Hostname() == "" {
				continue
			}
			targetPort := primaryPort
			if u.Port() != "" {
				targetPort = u.Port()
			}
			routerBase := fmt.Sprintf("%s-%d", svcName, idx)
			rule := fmt.Sprintf("Host(`%s`)", u.Hostname())
			if u.Path != "" && u.Path != "/" {
				rule += fmt.Sprintf(" && PathPrefix(`%s`)", u.Path)
			}
			allLabels["traefik.http.routers."+routerBase+"-http.entryPoints"] = "http"
			allLabels["traefik.http.routers."+routerBase+"-http.rule"] = rule
			allLabels["traefik.http.services."+routerBase+".loadbalancer.server.port"] = targetPort

			var activeMiddlewares []string
			if dctx.Settings["is_gzip_enabled"] != "false" {
				activeMiddlewares = append(activeMiddlewares, "gzip")
			}
			if redirectMode == "non-www" {
				mwName := routerBase + "-redir-non-www"
				allLabels["traefik.http.middlewares."+mwName+".redirectregex.regex"] = "^(https?://)www\\.(.*)"
				allLabels["traefik.http.middlewares."+mwName+".redirectregex.replacement"] = "${1}${2}"
				activeMiddlewares = append(activeMiddlewares, mwName)
			} else if redirectMode == "www" {
				mwName := routerBase + "-redir-www"
				allLabels["traefik.http.middlewares."+mwName+".redirectregex.regex"] = "^(https?://)(?:www\\.)?(.*)"
				allLabels["traefik.http.middlewares."+mwName+".redirectregex.replacement"] = "${1}www.${2}"
				activeMiddlewares = append(activeMiddlewares, mwName)
			}

			if strings.EqualFold(u.Scheme, "https") {
				allLabels["traefik.http.routers."+routerBase+"-https.entryPoints"] = "https"
				allLabels["traefik.http.routers."+routerBase+"-https.rule"] = rule
				allLabels["traefik.http.routers."+routerBase+"-https.tls"] = "true"
				allLabels["traefik.http.routers."+routerBase+"-https.tls.certresolver"] = certResolver
				allLabels["traefik.http.routers."+routerBase+"-https.service"] = routerBase
				if len(activeMiddlewares) > 0 {
					allLabels["traefik.http.routers."+routerBase+"-https.middlewares"] = strings.Join(activeMiddlewares, ",")
				}
				if forceHTTPS {
					allLabels["traefik.http.middlewares.redirect-to-https.redirectscheme.scheme"] = "https"
					allLabels["traefik.http.routers."+routerBase+"-http.middlewares"] = "redirect-to-https"
					allLabels["traefik.http.routers."+routerBase+"-http.service"] = routerBase
				} else {
					allLabels["traefik.http.routers."+routerBase+"-http.service"] = routerBase
				}
			} else {
				allLabels["traefik.http.routers."+routerBase+"-http.service"] = routerBase
				if len(activeMiddlewares) > 0 {
					allLabels["traefik.http.routers."+routerBase+"-http.middlewares"] = strings.Join(activeMiddlewares, ",")
				}
			}
		}
	}

	// Optional user custom labels (KEY=VALUE per line)
	if custom := strings.TrimSpace(dctx.Settings["custom_labels"]); custom != "" {
		for _, line := range strings.Split(custom, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) != "" {
				allLabels[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
	}

	labelKeys := make([]string, 0, len(allLabels))
	for k := range allLabels {
		labelKeys = append(labelKeys, k)
	}
	sort.Strings(labelKeys)

	var b strings.Builder
	b.WriteString("services:\n")
	fmt.Fprintf(&b, "  %s:\n", svcName)
	fmt.Fprintf(&b, "    image: %s\n", yamlQuote(imageRef))
	b.WriteString("    restart: unless-stopped\n")
	b.WriteString("    env_file:\n      - .env\n")

	// Resource limits
	if v := strings.TrimSpace(dctx.Settings["limits_memory"]); v != "" && v != "0" {
		fmt.Fprintf(&b, "    mem_limit: %s\n", yamlQuote(v))
	}
	if v := strings.TrimSpace(dctx.Settings["limits_memory_swap"]); v != "" && v != "0" {
		fmt.Fprintf(&b, "    memswap_limit: %s\n", yamlQuote(v))
	}
	if v := strings.TrimSpace(dctx.Settings["limits_memory_reservation"]); v != "" && v != "0" {
		fmt.Fprintf(&b, "    mem_reservation: %s\n", yamlQuote(v))
	}
	if v := strings.TrimSpace(dctx.Settings["limits_memory_swappiness"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 100 && n != 60 {
			fmt.Fprintf(&b, "    mem_swappiness: %d\n", n)
		}
	}
	if v := strings.TrimSpace(dctx.Settings["limits_cpus"]); v != "" && v != "0" {
		fmt.Fprintf(&b, "    cpus: %s\n", yamlQuote(v))
	}
	if v := strings.TrimSpace(dctx.Settings["limits_cpuset"]); v != "" {
		fmt.Fprintf(&b, "    cpuset: %s\n", yamlQuote(v))
	}
	if v := strings.TrimSpace(dctx.Settings["limits_cpu_shares"]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n != 1024 {
			fmt.Fprintf(&b, "    cpu_shares: %d\n", n)
		}
	}

	// Healthcheck
	if dctx.Settings["health_check_enabled"] == "true" {
		scheme := orDefault(dctx.Settings["health_check_scheme"], "http")
		host := orDefault(dctx.Settings["health_check_host"], "localhost")
		port := orDefault(dctx.Settings["health_check_port"], primaryPort)
		hcPath := orDefault(dctx.Settings["health_check_path"], "/")
		if !strings.HasPrefix(hcPath, "/") {
			hcPath = "/" + hcPath
		}
		targetURL := fmt.Sprintf("%s://%s:%s%s", scheme, host, port, hcPath)
		interval := orDefault(dctx.Settings["health_check_interval"], "5")
		timeout := orDefault(dctx.Settings["health_check_timeout"], "5")
		retries := orDefault(dctx.Settings["health_check_retries"], "10")
		startPeriod := orDefault(dctx.Settings["health_check_start_period"], "5")
		probeCmd := fmt.Sprintf("curl -fsS %s || wget -q -O /dev/null %s || exit 1", shellQuote(targetURL), shellQuote(targetURL))
		b.WriteString("    healthcheck:\n")
		fmt.Fprintf(&b, "      test: [\"CMD-SHELL\", %s]\n", yamlQuote(probeCmd))
		fmt.Fprintf(&b, "      interval: %ss\n", interval)
		fmt.Fprintf(&b, "      timeout: %ss\n", timeout)
		fmt.Fprintf(&b, "      retries: %s\n", retries)
		fmt.Fprintf(&b, "      start_period: %ss\n", startPeriod)
	}

	// Host port mappings (e.g. "8080:80,3000:3000")
	if pmRaw := strings.TrimSpace(dctx.Settings["ports_mappings"]); pmRaw != "" {
		var mappings []string
		for _, entry := range strings.Split(pmRaw, ",") {
			entry = strings.TrimSpace(entry)
			if entry != "" {
				mappings = append(mappings, entry)
			}
		}
		if len(mappings) > 0 {
			b.WriteString("    ports:\n")
			for _, m := range mappings {
				fmt.Fprintf(&b, "      - %s\n", yamlQuote(m))
			}
		}
	}

	// Volumes (persistent + file mounts)
	var namedVolumes []string
	var volumeSpecs []string
	for _, pv := range dctx.PersistentVolumes {
		if strings.TrimSpace(pv.MountPath) == "" {
			continue
		}
		if strings.TrimSpace(pv.HostPath) != "" {
			volumeSpecs = append(volumeSpecs, pv.HostPath+":"+pv.MountPath)
		} else if strings.TrimSpace(pv.Name) != "" {
			volumeSpecs = append(volumeSpecs, pv.Name+":"+pv.MountPath)
			namedVolumes = append(namedVolumes, pv.Name)
		}
	}
	for _, fv := range dctx.FileVolumes {
		if strings.TrimSpace(fv.FSPath) != "" && strings.TrimSpace(fv.MountPath) != "" {
			volumeSpecs = append(volumeSpecs, fv.FSPath+":"+fv.MountPath)
		}
	}
	if len(volumeSpecs) > 0 {
		b.WriteString("    volumes:\n")
		for _, vs := range volumeSpecs {
			fmt.Fprintf(&b, "      - %s\n", yamlQuote(vs))
		}
	}

	// Labels
	if len(labelKeys) > 0 {
		b.WriteString("    labels:\n")
		for _, k := range labelKeys {
			fmt.Fprintf(&b, "      - %s\n", yamlQuote(k+"="+allLabels[k]))
		}
	}

	// Network attachment
	b.WriteString("    networks:\n")
	fmt.Fprintf(&b, "      - %s\n", network)
	b.WriteString("networks:\n")
	fmt.Fprintf(&b, "  %s:\n", network)
	b.WriteString("    external: true\n")

	if len(namedVolumes) > 0 {
		b.WriteString("volumes:\n")
		for _, nv := range namedVolumes {
			fmt.Fprintf(&b, "  %s:\n", nv)
		}
	}
	return b.String()
}

func firstExposedPort(portsExposes string) string {
	for _, part := range strings.Split(portsExposes, ",") {
		p := strings.TrimSpace(part)
		if p != "" {
			return p
		}
	}
	return ""
}

func normalizeBaseDir(baseDir string) string {
	d := strings.TrimSpace(baseDir)
	d = strings.TrimPrefix(d, "/")
	if d == "" {
		return "."
	}
	return d
}

// buildAutoDetectDockerfileCmd auto-generates a Dockerfile if the cloned repository does not have one at dfPath.
func buildAutoDetectDockerfileCmd(dfPath, baseDir, primaryPort string) string {
	port := strings.TrimSpace(primaryPort)
	if port == "" {
		port = "80"
	}
	nodeDF := fmt.Sprintf("FROM node:22-alpine\nWORKDIR /app\nCOPY package*.json ./\nRUN npm install\nCOPY . .\nRUN if npm run | grep -q ' build'; then npm run build; fi\nENV PORT=%s\nEXPOSE %s\nCMD [\"sh\", \"-c\", \"if [ -d dist ] && ! grep -q '\\\"start\\\"' package.json; then npx --yes serve -s dist -l %s; else npm start; fi\"]\n", port, port, port)
	goDF := fmt.Sprintf("FROM golang:1.24-alpine AS builder\nWORKDIR /app\nCOPY . .\nRUN go build -o /out/app .\nFROM alpine:3.21\nWORKDIR /app\nCOPY --from=builder /out/app /app/app\nENV PORT=%s\nEXPOSE %s\nCMD [\"/app/app\"]\n", port, port)
	pyDF := fmt.Sprintf("FROM python:3.12-alpine\nWORKDIR /app\nCOPY . .\nRUN if [ -f requirements.txt ]; then pip install --no-cache-dir -r requirements.txt; fi\nENV PORT=%s\nEXPOSE %s\nCMD [\"python\", \"main.py\"]\n", port, port)
	staticDF := "FROM nginx:alpine\nCOPY . /usr/share/nginx/html\nEXPOSE 80\n"

	return fmt.Sprintf(
		"if [ ! -f %s ]; then "+
			"if [ -f %s ]; then printf %%s %s | base64 -d > %s; "+
			"elif [ -f %s ]; then printf %%s %s | base64 -d > %s; "+
			"elif [ -f %s ] || [ -f %s ]; then printf %%s %s | base64 -d > %s; "+
			"else printf %%s %s | base64 -d > %s; fi; "+
			"fi",
		shellQuote(dfPath),
		shellQuote(path.Join(baseDir, "package.json")), shellQuote(base64.StdEncoding.EncodeToString([]byte(nodeDF))), shellQuote(dfPath),
		shellQuote(path.Join(baseDir, "go.mod")), shellQuote(base64.StdEncoding.EncodeToString([]byte(goDF))), shellQuote(dfPath),
		shellQuote(path.Join(baseDir, "requirements.txt")), shellQuote(path.Join(baseDir, "pyproject.toml")), shellQuote(base64.StdEncoding.EncodeToString([]byte(pyDF))), shellQuote(dfPath),
		shellQuote(base64.StdEncoding.EncodeToString([]byte(staticDF))), shellQuote(dfPath),
	)
}

func buildEnsureTraefikProxyCmd(network string) string {
	netName := strings.TrimSpace(network)
	if netName == "" {
		netName = "coolify"
	}
	defaultProxyCompose := strings.Join([]string{
		"networks:",
		"  " + netName + ":",
		"    external: true",
		"services:",
		"  traefik:",
		"    container_name: coolify-proxy",
		"    image: traefik:v3.3",
		"    restart: unless-stopped",
		"    ports:",
		"      - \"80:80\"",
		"      - \"443:443\"",
		"      - \"443:443/udp\"",
		"    networks:",
		"      - " + netName,
		"    volumes:",
		"      - /var/run/docker.sock:/var/run/docker.sock:ro",
		"      - /data/coolify/proxy:/traefik",
		"    command:",
		"      - \"--entrypoints.http.address=:80\"",
		"      - \"--entrypoints.https.address=:443\"",
		"      - \"--providers.docker=true\"",
		"      - \"--providers.docker.exposedbydefault=false\"",
		"      - \"--providers.file.directory=/traefik/dynamic\"",
		"      - \"--providers.file.watch=true\"",
		"      - \"--certificatesresolvers.letsencrypt.acme.httpchallenge=true\"",
		"      - \"--certificatesresolvers.letsencrypt.acme.httpchallenge.entrypoint=http\"",
		"      - \"--certificatesresolvers.letsencrypt.acme.storage=/traefik/acme.json\"",
		"",
	}, "\n")
	b64 := base64.StdEncoding.EncodeToString([]byte(defaultProxyCompose))
	return fmt.Sprintf(
		"if ! docker ps --format '{{.Names}}' 2>/dev/null | grep -q '^coolify-proxy$'; then "+
			"mkdir -p /data/coolify/proxy/dynamic && "+
			"if [ ! -f /data/coolify/proxy/docker-compose.yml ]; then printf %%s %s | base64 -d > /data/coolify/proxy/docker-compose.yml; fi && "+
			"touch /data/coolify/proxy/acme.json && chmod 600 /data/coolify/proxy/acme.json && "+
			"(docker compose --project-name coolify-proxy -f /data/coolify/proxy/docker-compose.yml up -d --remove-orphans || true) && "+
			"(docker network connect %s coolify-proxy 2>/dev/null || true); "+
			"fi",
		shellQuote(b64), shellQuote(netName),
	)
}

func sanitizeComposeServiceName(uuid string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(uuid) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "app"
	}
	return b.String()
}

func yamlQuote(v string) string {
	escaped := strings.ReplaceAll(v, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, "\n", `\n`)
	return `"` + escaped + `"`
}

func shellQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

func orDefault(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
