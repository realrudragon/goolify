package projects_test

import (
	"context"
	"errors"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/projects"
	"github.com/coollabsio/coolify/backend/internal/realtime"
)

func TestProjectService_CreateAndTeamIsolation(t *testing.T) {
	outbox := realtime.NewOutboxStore()
	cache := realtime.NewCacheCoherence()
	hub := realtime.NewRealtimeHub(8)
	disp := realtime.NewDispatcher(outbox, cache, hub)
	svc := projects.NewProjectService(access.NewAuthorizer(), compat.NewEloquentHooks(), outbox, disp)

	member := auth.Principal{UserID: 2, AuthMethod: "session", SessionTeamID: 10, TeamRoles: map[int64]auth.Role{10: auth.RoleMember}}
	if _, err := svc.CreateProject(context.Background(), member, projects.CreateProjectInput{Name: "app", TeamID: 10}); !errors.Is(err, access.ErrForbidden) {
		t.Fatalf("expected member denied creating project, got %v", err)
	}

	admin := auth.Principal{UserID: 1, AuthMethod: "session", SessionTeamID: 10, TeamRoles: map[int64]auth.Role{10: auth.RoleAdmin}}
	proj, err := svc.CreateProject(context.Background(), admin, projects.CreateProjectInput{Name: "  my-saas  ", TeamID: 10})
	if err != nil || proj.Name != "my-saas" || len(proj.UUID) != 24 {
		t.Fatalf("unexpected project create result: proj=%+v err=%v", proj, err)
	}
	if cache.CurrentVersion("coolify:go:v1:team:10:search_ver") == 0 {
		t.Fatal("expected project creation to bump team 10 search cache version")
	}
}
