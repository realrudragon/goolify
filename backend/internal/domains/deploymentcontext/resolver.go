package deploymentcontext

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
)

var (
	ErrBrokenHierarchy    = errors.New("broken project -> environment -> application hierarchy")
	ErrForeignCredential  = errors.New("credential belongs to a different team than resource")
	ErrCyclicEnvReference = errors.New("cyclic shared environment variable reference")
	ErrMissingEnvRef      = errors.New("missing shared environment variable reference")
)

type EnvScope string

const (
	ScopeTeam        EnvScope = "team"
	ScopeProject     EnvScope = "project"
	ScopeEnvironment EnvScope = "environment"
	ScopeResource    EnvScope = "resource"
	ScopePreview     EnvScope = "preview"
)

type EnvVarRecord struct {
	Key         string   `json:"key"`
	Value       string   `json:"value"` // Plaintext resolved strictly server-side
	Scope       EnvScope `json:"scope"`
	IsBuildtime bool     `json:"is_buildtime"`
	IsRuntime   bool     `json:"is_runtime"`
	IsLiteral   bool     `json:"is_literal"`
	IsLocked    bool     `json:"is_locked"`
}

type DeploymentSource struct {
	ApplicationID   int64
	ApplicationUUID string
	EnvironmentID   int64
	EnvironmentUUID string
	ProjectID       int64
	ProjectUUID     string
	ResourceTeamID  int64
	ServerID        int64
	ServerUUID      string
	PrivateKeyID    int64
	PrivateKeyUUID  string
	PrivateKeyTeam  int64
	GitSourceUUID   string
	GitSourceTeam   int64
	BuildPack       string
	Settings        map[string]string
	TeamVars        []EnvVarRecord
	ProjectVars     []EnvVarRecord
	EnvironmentVars []EnvVarRecord
	ResourceVars    []EnvVarRecord
	PreviewVars     []EnvVarRecord
}

type Repository interface {
	LoadApplicationSource(ctx context.Context, appUUID string, previewID *int64) (DeploymentSource, error)
}

type DeploymentContext struct {
	ApplicationUUID string            `json:"application_uuid"`
	ProjectUUID     string            `json:"project_uuid"`
	EnvironmentUUID string            `json:"environment_uuid"`
	ResourceTeamID  int64             `json:"resource_team_id"`
	PreviewID       *int64            `json:"preview_id,omitempty"`
	ServerUUID      string            `json:"server_uuid"`
	PrivateKeyUUID  string            `json:"private_key_uuid"`
	GitSourceUUID   string            `json:"git_source_uuid,omitempty"`
	BuildPack       string            `json:"build_pack"`
	InputRevision   string            `json:"input_revision"`
	Settings        map[string]string `json:"settings,omitempty"`
	EnvEntries      []EnvVarRecord    `json:"-"` // Never expose resolved plaintext secrets in default JSON logs
}

type Resolver struct {
	repo Repository
	auth *access.Authorizer
}

func NewResolver(repo Repository, authz *access.Authorizer) *Resolver {
	return &Resolver{repo: repo, auth: authz}
}

var refPattern = regexp.MustCompile(`\{\{(team|project|environment)\.([A-Za-z0-9_]+)\}\}`)

// Resolve enforces resource-team authorization, checks foreign credentials, resolves Coolify's
// shared variable precedence and nested {{scope.KEY}} references, and produces an immutable InputRevision.
func (r *Resolver) Resolve(ctx context.Context, principal auth.Principal, applicationUUID string, previewID *int64) (DeploymentContext, error) {
	src, err := r.repo.LoadApplicationSource(ctx, applicationUUID, previewID)
	if err != nil {
		return DeploymentContext{}, err
	}
	if src.ProjectUUID == "" || src.EnvironmentUUID == "" || src.ApplicationUUID == "" {
		return DeploymentContext{}, ErrBrokenHierarchy
	}

	teamID := src.ResourceTeamID
	if err := r.auth.Check(ctx, principal, access.ActionDeploy, access.ResourceRef{
		Type:            "application",
		ID:              src.ApplicationID,
		UUID:            src.ApplicationUUID,
		ProjectUUID:     src.ProjectUUID,
		EnvironmentUUID: src.EnvironmentUUID,
		TeamID:          &teamID,
		ServerID:        &src.ServerID,
		APISegment:      "applications",
	}); err != nil {
		return DeploymentContext{}, err
	}

	if src.PrivateKeyUUID != "" && src.PrivateKeyTeam != src.ResourceTeamID {
		return DeploymentContext{}, ErrForeignCredential
	}
	if src.GitSourceUUID != "" && src.GitSourceTeam != src.ResourceTeamID {
		return DeploymentContext{}, ErrForeignCredential
	}

	resolved, err := resolveEnvPrecedence(src, previewID != nil)
	if err != nil {
		return DeploymentContext{}, err
	}

	revHash := computeInputRevision(src, resolved)
	return DeploymentContext{
		ApplicationUUID: src.ApplicationUUID,
		ProjectUUID:     src.ProjectUUID,
		EnvironmentUUID: src.EnvironmentUUID,
		ResourceTeamID:  src.ResourceTeamID,
		PreviewID:       previewID,
		ServerUUID:      src.ServerUUID,
		PrivateKeyUUID:  src.PrivateKeyUUID,
		GitSourceUUID:   src.GitSourceUUID,
		BuildPack:       src.BuildPack,
		InputRevision:   revHash,
		Settings:        src.Settings,
		EnvEntries:      resolved,
	}, nil
}

func resolveEnvPrecedence(src DeploymentSource, isPreview bool) ([]EnvVarRecord, error) {
	scopeMaps := map[EnvScope]map[string]EnvVarRecord{
		ScopeTeam:        toMap(src.TeamVars, ScopeTeam),
		ScopeProject:     toMap(src.ProjectVars, ScopeProject),
		ScopeEnvironment: toMap(src.EnvironmentVars, ScopeEnvironment),
	}

	merged := make(map[string]EnvVarRecord)
	order := make([]string, 0)
	addLayer := func(vars []EnvVarRecord, scope EnvScope) {
		for _, v := range vars {
			v.Scope = scope
			if _, exists := merged[v.Key]; !exists {
				order = append(order, v.Key)
			}
			// Preserve locked/literal semantics and override lower-precedence entries
			merged[v.Key] = v
		}
	}

	// Precedence: team < project < environment < resource < preview
	addLayer(src.TeamVars, ScopeTeam)
	addLayer(src.ProjectVars, ScopeProject)
	addLayer(src.EnvironmentVars, ScopeEnvironment)
	addLayer(src.ResourceVars, ScopeResource)
	if isPreview {
		addLayer(src.PreviewVars, ScopePreview)
	}

	visiting := make(map[string]bool)
	resolvedCache := make(map[string]string)

	var resolveVal func(scope EnvScope, key string, rec EnvVarRecord) (string, error)
	resolveVal = func(scope EnvScope, key string, rec EnvVarRecord) (string, error) {
		cacheKey := string(scope) + ":" + key
		if val, ok := resolvedCache[cacheKey]; ok {
			return val, nil
		}
		if rec.IsLiteral {
			return rec.Value, nil
		}
		if visiting[cacheKey] {
			return "", fmt.Errorf("%w: %s", ErrCyclicEnvReference, cacheKey)
		}
		visiting[cacheKey] = true
		defer delete(visiting, cacheKey)

		var resolveErr error
		out := refPattern.ReplaceAllStringFunc(rec.Value, func(match string) string {
			if resolveErr != nil {
				return match
			}
			sub := refPattern.FindStringSubmatch(match)
			targetScope := EnvScope(sub[1])
			targetKey := sub[2]
			targetMap, ok := scopeMaps[targetScope]
			if !ok {
				resolveErr = fmt.Errorf("%w: %s.%s", ErrMissingEnvRef, targetScope, targetKey)
				return match
			}
			targetRec, exists := targetMap[targetKey]
			if !exists {
				resolveErr = fmt.Errorf("%w: %s.%s", ErrMissingEnvRef, targetScope, targetKey)
				return match
			}
			val, err := resolveVal(targetScope, targetKey, targetRec)
			if err != nil {
				resolveErr = err
				return match
			}
			return val
		})
		if resolveErr != nil {
			return "", resolveErr
		}
		resolvedCache[cacheKey] = out
		return out, nil
	}

	out := make([]EnvVarRecord, 0, len(order))
	for _, k := range order {
		rec := merged[k]
		val, err := resolveVal(rec.Scope, rec.Key, rec)
		if err != nil {
			return nil, err
		}
		rec.Value = val
		out = append(out, rec)
	}
	return out, nil
}

func toMap(vars []EnvVarRecord, scope EnvScope) map[string]EnvVarRecord {
	m := make(map[string]EnvVarRecord, len(vars))
	for _, v := range vars {
		v.Scope = scope
		m[v.Key] = v
	}
	return m
}

func computeInputRevision(src DeploymentSource, envs []EnvVarRecord) string {
	h := sha256.New()
	fmt.Fprintf(h, "%s|%s|%s|%d|%s|%s|", src.ApplicationUUID, src.EnvironmentUUID, src.ProjectUUID, src.ResourceTeamID, src.BuildPack, src.ServerUUID)
	for _, e := range envs {
		fmt.Fprintf(h, "%s=%d:%t:%t:%t:%s|", e.Key, len(e.Value), e.IsBuildtime, e.IsRuntime, e.IsLocked, e.Value)
	}
	return hex.EncodeToString(h.Sum(nil))
}
