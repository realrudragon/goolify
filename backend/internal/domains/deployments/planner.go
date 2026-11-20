package deployments

import (
	"fmt"
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

// BuildPlan compiles a deterministic command plan for Docker Image, Dockerfile, Railpack,
// Compose, Static, or legacy Nixpacks (per frozen baseline compatibility policy).
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

	var cmds []string
	switch dctx.BuildPack {
	case "dockerimage":
		img := dctx.Settings["docker_registry_image_name"]
		if img == "" {
			img = "nginx:alpine"
		}
		tag := dctx.Settings["docker_registry_image_tag"]
		if tag != "" {
			img = img + ":" + tag
		}
		cmds = []string{
			fmt.Sprintf("docker pull %s", shellQuote(img)),
			fmt.Sprintf("docker compose --project-name %s up -d --remove-orphans", shellQuote(dctx.ApplicationUUID)),
		}
	case "dockerfile":
		cmds = []string{
			gitCloneOrFetchCmd(dctx),
			fmt.Sprintf("docker build -f %s -t %s .", shellQuote(orDefault(dctx.Settings["dockerfile_location"], "Dockerfile")), shellQuote("coolify/"+dctx.ApplicationUUID+":"+dctx.InputRevision[:12])),
			fmt.Sprintf("docker compose --project-name %s up -d --remove-orphans", shellQuote(dctx.ApplicationUUID)),
		}
	case "railpack":
		cmds = []string{
			gitCloneOrFetchCmd(dctx),
			fmt.Sprintf("railpack build --name %s .", shellQuote("coolify/"+dctx.ApplicationUUID)),
			fmt.Sprintf("docker compose --project-name %s up -d --remove-orphans", shellQuote(dctx.ApplicationUUID)),
		}
	case "dockercompose":
		if strings.TrimSpace(dctx.Settings["git_repository"]) != "" {
			cmds = append(cmds, gitCloneOrFetchCmd(dctx))
		}
		cmds = append(cmds, fmt.Sprintf("docker compose --project-name %s -f docker-compose.yaml up -d --remove-orphans", shellQuote(dctx.ApplicationUUID)))
	case "static":
		if strings.TrimSpace(dctx.Settings["git_repository"]) != "" {
			cmds = append(cmds, gitCloneOrFetchCmd(dctx))
		}
		cmds = append(cmds,
			fmt.Sprintf("docker build -f Dockerfile.static -t %s .", shellQuote("coolify/"+dctx.ApplicationUUID)),
			fmt.Sprintf("docker compose --project-name %s up -d --remove-orphans", shellQuote(dctx.ApplicationUUID)),
		)
	case "nixpacks":
		// Explicit compatibility policy from Task 1: allow read/legacy build only when flagged
		if dctx.Settings["legacy_nixpacks_confirmed"] != "true" {
			return ExecutionPlan{}, fmt.Errorf("legacy nixpacks deployment requires explicit compatibility confirmation before cutover")
		}
		cmds = []string{
			fmt.Sprintf("nixpacks build . --name %s", shellQuote("coolify/"+dctx.ApplicationUUID)),
		}
	default:
		return ExecutionPlan{}, fmt.Errorf("unsupported build pack %q", dctx.BuildPack)
	}

	if pullSecretRef != "" {
		labels["coolify.registry_ref"] = pullSecretRef
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
	return fmt.Sprintf(
		"if [ -d .git ]; then git fetch --depth=1 origin %s && git reset --hard FETCH_HEAD; else git clone --depth=1 --branch %s %s .; fi",
		shellQuote(branch), shellQuote(branch), shellQuote(repo),
	)
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
