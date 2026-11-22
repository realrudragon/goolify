package projects

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/realtime"
)

type Project struct {
	ID          int64  `json:"id"`
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	TeamID      int64  `json:"team_id"`
	Revision    int64  `json:"revision"`
}

type Environment struct {
	Description string `json:"description,omitempty"`
	ID          int64  `json:"id"`
	UUID        string `json:"uuid"`
	Name        string `json:"name"`
	ProjectID   int64  `json:"project_id"`
	TeamID      int64  `json:"team_id"`
}

type CreateProjectInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	TeamID      int64  `json:"team_id"`
}

type ProjectService struct {
	mu       sync.RWMutex
	authz    *access.Authorizer
	hooks    *compat.EloquentHooks
	outbox   *realtime.OutboxStore
	disp     *realtime.Dispatcher
	projects map[string]Project
	envs     map[string][]Environment
	tags     map[string][]string
	nextID   int64
}

func NewProjectService(authz *access.Authorizer, hooks *compat.EloquentHooks, outbox *realtime.OutboxStore, disp *realtime.Dispatcher) *ProjectService {
	return &ProjectService{
		authz:    authz,
		hooks:    hooks,
		outbox:   outbox,
		disp:     disp,
		projects: make(map[string]Project),
		envs:     make(map[string][]Environment),
		tags:     make(map[string][]string),
		nextID:   1,
	}
}

func (s *ProjectService) CreateProject(ctx context.Context, principal auth.Principal, input CreateProjectInput) (Project, error) {
	teamID := input.TeamID
	if err := s.authz.Check(ctx, principal, access.ActionCreate, access.ResourceRef{
		Type: "project", TeamID: &teamID, APISegment: "projects",
	}); err != nil {
		return Project{}, err
	}
	row := compat.ModelRow{Name: compat.SanitizeString(input.Name)}
	_ = s.hooks.ApplyModelDefaults("project", &row)

	s.mu.Lock()
	id := s.nextID
	s.nextID++
	p := Project{
		ID:          id,
		UUID:        row.UUID,
		Name:        row.Name,
		Description: compat.SanitizeString(input.Description),
		TeamID:      teamID,
		Revision:    1,
	}
	s.projects[p.UUID] = p
	envRow := compat.ModelRow{}
	_ = s.hooks.ApplyModelDefaults("environment", &envRow)
	s.envs[p.UUID] = []Environment{{
		ID: id, UUID: envRow.UUID, Name: "production", ProjectID: id, TeamID: teamID,
	}}
	s.mu.Unlock()

	if s.outbox != nil {
		_ = s.outbox.AppendInTx(ctx, true, realtime.DomainEvent{
			EventID: fmt.Sprintf("proj-created-%s", p.UUID), SchemaVersion: "v1", Producer: "go-controlplane",
			TeamID: teamID, ResourceType: "project", ResourceUUID: p.UUID, AggregateRevision: 1,
			OwnershipEpoch: 1, MutationType: "project.updated", CommittedAt: time.Now().UTC(),
		})
		if s.disp != nil {
			_ = s.disp.FlushPending(ctx)
		}
	}
	return p, nil
}
