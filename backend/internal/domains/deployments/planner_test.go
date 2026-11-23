package deployments

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/domains/deploymentcontext"
)

func TestBuildPlan_DockerImage_FreshHostGeneratesComposeAndEnv(t *testing.T) {
	p := NewDeploymentPlanner()
	dctx := deploymentcontext.DeploymentContext{
		ApplicationUUID: "app-fresh-1",
		ServerUUID:      "localhost",
		DockerNetwork:   "coolify",
		BuildPack:       "dockerimage",
		InputRevision:   "abcdef1234567890",
		Settings: map[string]string{
			"docker_registry_image_name": "ghcr.io/example/web",
			"docker_registry_image_tag":  "v2.1",
			"fqdn":                       "https://web.example.com",
			"ports_exposes":              "3000",
			"health_check_enabled":       "true",
			"health_check_path":          "/healthz",
			"limits_memory":              "512m",
			"limits_cpus":                "1.5",
		},
		EnvEntries: []deploymentcontext.EnvVarRecord{
			{Key: "NODE_ENV", Value: "production", IsRuntime: true},
		},
		PersistentVolumes: []deploymentcontext.VolumeMount{
			{Name: "app-data", MountPath: "/data"},
		},
	}

	plan, err := p.BuildPlan(dctx, "op-1", "")
	if err != nil {
		t.Fatalf("BuildPlan failed: %v", err)
	}
	joined := strings.Join(plan.Commands, "\n")
	if !strings.Contains(joined, "> '.env'") {
		t.Fatalf("expected .env write command, got:\n%s", joined)
	}
	if !strings.Contains(joined, "docker network create --attachable 'coolify'") {
		t.Fatalf("expected network ensure command, got:\n%s", joined)
	}
	if !strings.Contains(joined, "> 'docker-compose.yaml'") {
		t.Fatalf("expected docker-compose.yaml write command, got:\n%s", joined)
	}
	if !strings.Contains(joined, "docker pull 'ghcr.io/example/web:v2.1'") {
		t.Fatalf("expected docker pull for configured image:tag, got:\n%s", joined)
	}

	// Decode the generated docker-compose.yaml and verify Traefik, healthcheck, limits, and volumes.
	composeYAML := buildGeneratedComposeYAML(dctx, "ghcr.io/example/web:v2.1", "coolify", plan.Labels)
	for _, want := range []string{
		`image: "ghcr.io/example/web:v2.1"`,
		`traefik.enable=true`,
		"Host(`web.example.com`)",
		`loadbalancer.server.port=3000`,
		`mem_limit: "512m"`,
		`cpus: "1.5"`,
		`healthcheck:`,
		`/healthz`,
		`app-data:/data`,
	} {
		if !strings.Contains(composeYAML, want) {
			t.Fatalf("generated compose missing %q:\n%s", want, composeYAML)
		}
	}
}

func TestBuildPlan_Dockerfile_InlineAndPrivateRepoDeployKey(t *testing.T) {
	p := NewDeploymentPlanner()

	// 1. Inline Dockerfile without git repo
	inlineCtx := deploymentcontext.DeploymentContext{
		ApplicationUUID: "app-inline-df",
		ServerUUID:      "localhost",
		BuildPack:       "dockerfile",
		InputRevision:   "1234567890abcdef",
		Settings: map[string]string{
			"dockerfile": "FROM alpine:3.21\nCMD [\"echo\", \"ok\"]\n",
		},
		EnvEntries: []deploymentcontext.EnvVarRecord{
			{Key: "BUILD_TOKEN", Value: "secret-val", IsBuildtime: true, IsRuntime: true},
		},
	}
	plan, err := p.BuildPlan(inlineCtx, "op-inline", "")
	if err != nil {
		t.Fatalf("inline dockerfile plan failed: %v", err)
	}
	joined := strings.Join(plan.Commands, "\n")
	wantDFB64 := base64.StdEncoding.EncodeToString([]byte(inlineCtx.Settings["dockerfile"]))
	if !strings.Contains(joined, wantDFB64) {
		t.Fatalf("expected inline Dockerfile base64 payload in commands:\n%s", joined)
	}
	if !strings.Contains(joined, "--build-arg 'BUILD_TOKEN=secret-val'") {
		t.Fatalf("expected build-arg in docker build command:\n%s", joined)
	}

	// 2. Private Git repo with SSH deploy key
	privCtx := deploymentcontext.DeploymentContext{
		ApplicationUUID:  "app-priv-git",
		ServerUUID:       "localhost",
		BuildPack:        "dockerfile",
		InputRevision:    "fedcba0987654321",
		DeployPrivateKey: "-----BEGIN OPENSSH PRIVATE KEY-----\ntestkey\n-----END OPENSSH PRIVATE KEY-----",
		Settings: map[string]string{
			"git_repository": "git@github.com:org/private-repo.git",
			"git_branch":     "main",
		},
	}
	privPlan, err := p.BuildPlan(privCtx, "op-priv", "")
	if err != nil {
		t.Fatalf("private repo plan failed: %v", err)
	}
	privJoined := strings.Join(privPlan.Commands, "\n")
	if !strings.Contains(privJoined, "GIT_SSH_COMMAND=") || !strings.Contains(privJoined, "chmod 600") {
		t.Fatalf("expected ephemeral GIT_SSH_COMMAND with chmod 600, got:\n%s", privJoined)
	}
}

func TestBuildPlan_StaticSPAAndDockerComposeRaw(t *testing.T) {
	p := NewDeploymentPlanner()

	staticCtx := deploymentcontext.DeploymentContext{
		ApplicationUUID: "app-static-spa",
		ServerUUID:      "localhost",
		BuildPack:       "static",
		InputRevision:   "1111222233334444",
		Settings: map[string]string{
			"git_repository":    "https://github.com/example/spa.git",
			"publish_directory": "/dist",
			"is_spa":            "true",
		},
	}
	sPlan, err := p.BuildPlan(staticCtx, "op-static", "")
	if err != nil {
		t.Fatalf("static plan failed: %v", err)
	}
	sJoined := strings.Join(sPlan.Commands, "\n")
	if !strings.Contains(sJoined, "> 'Dockerfile.static'") || !strings.Contains(sJoined, "> 'docker-compose.yaml'") {
		t.Fatalf("expected Dockerfile.static and docker-compose.yaml generation:\n%s", sJoined)
	}

	compCtx := deploymentcontext.DeploymentContext{
		ApplicationUUID: "app-raw-compose",
		ServerUUID:      "localhost",
		BuildPack:       "dockercompose",
		InputRevision:   "aaaa222233334444",
		Settings: map[string]string{
			"docker_compose_raw": "services:\n  web:\n    image: nginx:alpine\n",
		},
	}
	cPlan, err := p.BuildPlan(compCtx, "op-compose", "")
	if err != nil {
		t.Fatalf("dockercompose plan failed: %v", err)
	}
	cJoined := strings.Join(cPlan.Commands, "\n")
	if !strings.Contains(cJoined, "> 'docker-compose.yaml'") {
		t.Fatalf("expected raw docker-compose.yaml write command:\n%s", cJoined)
	}
}

func TestBuildPlan_DomainPortOverridePortsMappingsRedirectAndProxyAutoStart(t *testing.T) {
	p := NewDeploymentPlanner()
	dctx := deploymentcontext.DeploymentContext{
		ApplicationUUID: "app-domain-cfg",
		ServerUUID:      "localhost",
		DockerNetwork:   "coolify",
		BuildPack:       "dockerfile",
		InputRevision:   "9999888877776666",
		Settings: map[string]string{
			"git_repository": "https://github.com/example/node-app.git",
			"fqdn":           "https://app.example.com:4000,http://www.example.com",
			"ports_exposes":  "3000",
			"ports_mappings": "18080:3000",
			"redirect":       "non-www",
		},
	}
	plan, err := p.BuildPlan(dctx, "op-domain", "")
	if err != nil {
		t.Fatalf("BuildPlan failed: %v", err)
	}
	joined := strings.Join(plan.Commands, "\n")
	if !strings.Contains(joined, "coolify-proxy") {
		t.Fatalf("expected auto-ensure coolify-proxy command when fqdn is configured:\n%s", joined)
	}
	if !strings.Contains(joined, "package.json") {
		t.Fatalf("expected auto-detect Dockerfile fallback for git repo:\n%s", joined)
	}
	composeYAML := buildGeneratedComposeYAML(dctx, "coolify/app-domain-cfg:999988887777", "coolify", plan.Labels)
	for _, want := range []string{
		`loadbalancer.server.port=4000`,
		`loadbalancer.server.port=3000`,
		`redirect-to-https.redirectscheme.scheme=https`,
		`redir-non-www.redirectregex.regex`,
		`- "18080:3000"`,
	} {
		if !strings.Contains(composeYAML, want) {
			t.Fatalf("generated compose missing %q:\n%s", want, composeYAML)
		}
	}
}

