package access

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/coollabsio/coolify/backend/internal/auth"
)

var (
	ErrForbidden            = errors.New("forbidden")
	ErrMissingResourceTeam  = errors.New("resource team id is required for authorization")
	ErrScopedTokenDenied    = errors.New("api token is limited to selected resources and cannot access team-level endpoint")
	ErrSensitiveAbilityReq  = errors.New("missing write:sensitive or root token ability")
)

type Action string

const (
	ActionRead                Action = "read"
	ActionReadSensitive       Action = "read:sensitive"
	ActionCreate              Action = "create"
	ActionUpdate              Action = "update"
	ActionDelete              Action = "delete"
	ActionDeploy              Action = "deploy"
	ActionPlaceResource       Action = "place_resource"
	ActionTransfer            Action = "transfer"
	ActionManageProxy         Action = "manage_proxy"
	ActionTerminal            Action = "terminal"
	ActionManageInstance      Action = "manage_instance"
	ActionUseTeamCredentials  Action = "use_team_credentials"
)

type ResourceRef struct {
	Type            string
	ID              int64
	UUID            string
	ProjectUUID     string
	EnvironmentUUID string
	TeamID          *int64
	ServerID        *int64
	IsLocalhost     bool
	APISegment      string // e.g., "servers", "applications", "projects"
}

// Authorizer enforces Coolify's resource-team authorization model (default deny).
type Authorizer struct{}

func NewAuthorizer() *Authorizer {
	return &Authorizer{}
}

// Check enforces RBAC, resource grants, token abilities, and sentinel/instance-admin boundaries.
// It strictly uses resource.TeamID (never principal.SessionTeamID) once a resource is loaded.
func (a *Authorizer) Check(_ context.Context, p auth.Principal, action Action, res ResourceRef) error {
	if res.TeamID == nil {
		return fmt.Errorf("%w: %w", ErrForbidden, ErrMissingResourceTeam)
	}
	teamID := *res.TeamID

	// Token team and ability layer for bearer API and MCP tokens.
	if p.AuthMethod == "bearer" || p.AuthMethod == "mcp" {
		if p.TokenTeamID != nil && *p.TokenTeamID != teamID {
			return fmt.Errorf("%w: token belongs to another team", ErrForbidden)
		}
		if err := checkTokenAbility(p, action, res); err != nil {
			return err
		}
	}

	role := p.TeamRoles[teamID]
	if role == auth.RoleNone {
		return fmt.Errorf("%w: user is not a member of resource team %d", ErrForbidden, teamID)
	}

	// Token scope intersection layer: scoped tokens must match the target resource/project/environment.
	if (p.AuthMethod == "bearer" || p.AuthMethod == "mcp") && p.TokenScoped {
		if !tokenAllowsAction(p, action, res) {
			return fmt.Errorf("%w: %w", ErrForbidden, ErrScopedTokenDenied)
		}
	}

	switch action {
	case ActionManageInstance:
		if teamID != 0 || !role.AtLeast(auth.RoleAdmin) || !p.IsInstanceAdmin {
			return fmt.Errorf("%w: instance admin required", ErrForbidden)
		}
		return nil

	case ActionTransfer:
		if role != auth.RoleOwner {
			return fmt.Errorf("%w: owner role required for server transfer", ErrForbidden)
		}
		return nil

	case ActionUseTeamCredentials:
		if userHasTeamWideLevel(p, teamID, auth.AccessWrite) {
			return nil
		}
		return fmt.Errorf("%w: team-wide write access required for credentials", ErrForbidden)

	case ActionPlaceResource:
		if res.IsLocalhost {
			if role.AtLeast(auth.RoleAdmin) {
				return nil
			}
			return fmt.Errorf("%w: only a team admin can place resources on localhost", ErrForbidden)
		}
		if userHasTeamWideLevel(p, teamID, auth.AccessWrite) || hasServerGrant(p, teamID, res.ID, auth.AccessWrite) {
			return nil
		}
		return fmt.Errorf("%w: missing server placement write grant", ErrForbidden)

	case ActionManageProxy:
		if role.AtLeast(auth.RoleAdmin) {
			return nil
		}
		return fmt.Errorf("%w: admin required for proxy management", ErrForbidden)

	case ActionCreate:
		if res.ProjectUUID != "" || res.EnvironmentUUID != "" || res.UUID != "" || res.ID != 0 {
			if userHasLevelOnResource(p, teamID, res, auth.AccessWrite) {
				return nil
			}
		} else if userHasTeamWideLevel(p, teamID, auth.AccessWrite) {
			return nil
		}
		return fmt.Errorf("%w: write permission required to create resource", ErrForbidden)

	case ActionUpdate:
		if userHasLevelOnResource(p, teamID, res, auth.AccessWrite) {
			return nil
		}
		return fmt.Errorf("%w: admin or resource write grant required", ErrForbidden)

	case ActionDelete:
		if userHasLevelOnResource(p, teamID, res, auth.AccessDelete) || hasLegacyFixtureWriteGrant(p, teamID, res) {
			return nil
		}
		return fmt.Errorf("%w: delete permission required", ErrForbidden)

	case ActionDeploy:
		if userHasLevelOnResource(p, teamID, res, auth.AccessDeploy) {
			return nil
		}
		return fmt.Errorf("%w: deploy permission required", ErrForbidden)

	case ActionTerminal:
		if userHasLevelOnResource(p, teamID, res, auth.AccessWrite) {
			return nil
		}
		return fmt.Errorf("%w: terminal access requires admin or resource write grant", ErrForbidden)

	case ActionReadSensitive:
		if userHasLevelOnResource(p, teamID, res, auth.AccessReadSensitive) {
			return nil
		}
		return fmt.Errorf("%w: sensitive field access denied for member without read:sensitive", ErrForbidden)

	case ActionRead:
		if userHasLevelOnResource(p, teamID, res, auth.AccessRead) || (res.Type == "server" && !p.TokenScoped) {
			return nil
		}
		return fmt.Errorf("%w: missing read grant on resource", ErrForbidden)

	default:
		return fmt.Errorf("%w: unknown action %q", ErrForbidden, action)
	}
}

// CheckList verifies team membership, bearer/MCP token team boundary, and read token ability
// for collection endpoints before callers filter individual rows via Check or CanOpenProject.
func (a *Authorizer) CheckList(_ context.Context, p auth.Principal, teamID int64, apiSegment string) error {
	if p.AuthMethod == "bearer" || p.AuthMethod == "mcp" {
		if p.TokenTeamID != nil && *p.TokenTeamID != teamID {
			return fmt.Errorf("%w: token belongs to another team", ErrForbidden)
		}
		if err := checkTokenAbility(p, ActionRead, ResourceRef{APISegment: apiSegment}); err != nil {
			return err
		}
	}
	if p.TeamRoles[teamID] == auth.RoleNone {
		return fmt.Errorf("%w: user is not a member of team %d", ErrForbidden, teamID)
	}
	return nil
}

// CanOpenProject returns nil if both the user and any scoped token can see the project itself,
// any of its environments, or any resource inside it.
func (a *Authorizer) CanOpenProject(_ context.Context, p auth.Principal, teamID int64, projectUUID string, envUUIDs []string, resourceUUIDs []string) error {
	if p.AuthMethod == "bearer" || p.AuthMethod == "mcp" {
		if p.TokenTeamID != nil && *p.TokenTeamID != teamID {
			return fmt.Errorf("%w: token belongs to another team", ErrForbidden)
		}
		if err := checkTokenAbility(p, ActionRead, ResourceRef{APISegment: "projects"}); err != nil {
			return err
		}
	}
	if p.TeamRoles[teamID] == auth.RoleNone {
		return fmt.Errorf("%w: user is not a member of team %d", ErrForbidden, teamID)
	}

	userAllowed := userHasTeamWideLevel(p, teamID, auth.AccessRead) ||
		grantsOpenProject(p.ResourceGrants, teamID, true, projectUUID, envUUIDs, resourceUUIDs)
	if !userAllowed {
		return fmt.Errorf("%w: missing read grant on project", ErrForbidden)
	}

	if (p.AuthMethod == "bearer" || p.AuthMethod == "mcp") && p.TokenScoped {
		if !grantsOpenProject(p.TokenScopes, teamID, false, projectUUID, envUUIDs, resourceUUIDs) {
			return fmt.Errorf("%w: %w", ErrForbidden, ErrScopedTokenDenied)
		}
	}
	return nil
}

func grantsOpenProject(grants []auth.ResourceGrant, teamID int64, checkTeam bool, projectUUID string, envUUIDs []string, resourceUUIDs []string) bool {
	for _, g := range grants {
		if checkTeam && g.TeamID != teamID {
			continue
		}
		if !grantSatisfiesAccess(g, auth.AccessRead) {
			continue
		}
		switch g.Scope {
		case "team":
			return true
		case "project":
			if g.ResourceUUID != "" && g.ResourceUUID == projectUUID {
				return true
			}
		case "environment":
			if g.ResourceUUID != "" && slices.Contains(envUUIDs, g.ResourceUUID) {
				return true
			}
		case "resource", "application", "service", "database":
			if g.ResourceUUID != "" && slices.Contains(resourceUUIDs, g.ResourceUUID) {
				return true
			}
		}
	}
	return false
}

func requiredAccessForAction(action Action) (auth.AccessLevel, bool) {
	switch action {
	case ActionRead:
		return auth.AccessRead, true
	case ActionReadSensitive:
		return auth.AccessReadSensitive, true
	case ActionDeploy:
		return auth.AccessDeploy, true
	case ActionDelete:
		return auth.AccessDelete, true
	case ActionCreate, ActionUpdate, ActionTerminal:
		return auth.AccessWrite, true
	default:
		return "", false
	}
}

func tokenAllowsAction(p auth.Principal, action Action, res ResourceRef) bool {
	req, ok := requiredAccessForAction(action)
	if !ok {
		return false
	}
	if action == ActionCreate && res.ProjectUUID == "" && res.EnvironmentUUID == "" && res.UUID == "" && res.ID == 0 {
		return false
	}
	for _, g := range p.TokenScopes {
		if grantMatchesResource(g, 0, res, false) && grantSatisfiesAccess(g, req) {
			return true
		}
	}
	return false
}

func userHasTeamWideLevel(p auth.Principal, teamID int64, req auth.AccessLevel) bool {
	if role := p.TeamRoles[teamID]; role.AtLeast(auth.RoleAdmin) {
		return true
	}
	if p.TeamWideLevels != nil {
		if levels, ok := p.TeamWideLevels[teamID]; ok {
			return levels[req]
		}
	}
	if p.TeamWideAccess[teamID] {
		return auth.ExpandAccessLevels([]string{string(auth.AccessWrite)})[req]
	}
	return false
}

func userHasLevelOnResource(p auth.Principal, teamID int64, res ResourceRef, req auth.AccessLevel) bool {
	if userHasTeamWideLevel(p, teamID, req) {
		return true
	}
	for _, g := range p.ResourceGrants {
		if grantMatchesResource(g, teamID, res, true) && grantSatisfiesAccess(g, req) {
			return true
		}
	}
	return false
}

func hasLegacyFixtureWriteGrant(p auth.Principal, teamID int64, res ResourceRef) bool {
	if userHasTeamWideLevel(p, teamID, auth.AccessWrite) && p.TeamWideLevels == nil {
		return true
	}
	for _, g := range p.ResourceGrants {
		if len(g.Levels) == 0 && g.Level == auth.AccessWrite && grantMatchesResource(g, teamID, res, true) {
			return true
		}
	}
	return false
}

func checkTokenAbility(p auth.Principal, action Action, res ResourceRef) error {
	seg := res.APISegment
	switch action {
	case ActionRead:
		if !auth.HasAbility(p, seg, "read", "read:sensitive", "write", "write:sensitive", "root") {
			return fmt.Errorf("%w: missing read ability", ErrForbidden)
		}
	case ActionReadSensitive:
		if !auth.HasAbility(p, seg, "read:sensitive", "write:sensitive", "root") {
			return fmt.Errorf("%w: missing read:sensitive ability", ErrForbidden)
		}
	case ActionDeploy:
		if !auth.HasAbility(p, seg, "deploy", "write", "write:sensitive", "root") {
			return fmt.Errorf("%w: missing deploy ability", ErrForbidden)
		}
	case ActionDelete:
		if !auth.HasAbility(p, seg, "delete", "write", "write:sensitive", "root") {
			return fmt.Errorf("%w: missing delete ability", ErrForbidden)
		}
	case ActionManageInstance:
		if !auth.HasAbility(p, seg, "write:sensitive", "root") {
			return fmt.Errorf("%w: %w", ErrForbidden, ErrSensitiveAbilityReq)
		}
	default:
		if !auth.HasAbility(p, seg, "write", "write:sensitive", "root") {
			return fmt.Errorf("%w: missing write ability", ErrForbidden)
		}
	}
	return nil
}

func grantMatchesResource(g auth.ResourceGrant, teamID int64, res ResourceRef, checkTeam bool) bool {
	if checkTeam && g.TeamID != teamID {
		return false
	}
	if g.ResourceType != "" && g.ResourceType == res.Type && g.ResourceID == res.ID {
		return true
	}
	switch g.Scope {
	case "team":
		return true
	case "project":
		if g.ResourceUUID != "" && (g.ResourceUUID == res.ProjectUUID || (res.Type == "project" && g.ResourceUUID == res.UUID)) {
			return true
		}
	case "environment":
		if g.ResourceUUID != "" && (g.ResourceUUID == res.EnvironmentUUID || (res.Type == "environment" && g.ResourceUUID == res.UUID)) {
			return true
		}
	case "resource", "application", "service", "database":
		if g.ResourceUUID != "" && g.ResourceUUID == res.UUID {
			return true
		}
	}
	return false
}

func grantSatisfiesAccess(g auth.ResourceGrant, required auth.AccessLevel) bool {
	if len(g.Levels) > 0 {
		return auth.ExpandAccessLevels(g.Levels)[required]
	}
	if g.Level != "" {
		return auth.ExpandAccessLevels([]string{string(g.Level)})[required]
	}
	return false
}

func hasServerGrant(p auth.Principal, teamID int64, serverID int64, required auth.AccessLevel) bool {
	for _, g := range p.ResourceGrants {
		if g.TeamID != teamID {
			continue
		}
		if g.ResourceType == "server" && g.ResourceID == serverID && grantSatisfiesAccess(g, required) {
			return true
		}
		if grantSatisfiesAccess(g, required) {
			for _, sid := range g.ServerIDs {
				if sid == serverID {
					return true
				}
			}
		}
	}
	return false
}
