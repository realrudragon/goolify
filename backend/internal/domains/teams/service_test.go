package teams_test

import (
	"context"
	"errors"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/domains/teams"
	"github.com/coollabsio/coolify/backend/internal/realtime"
)

func TestTeamService_GrantAccessAndCacheBump(t *testing.T) {
	outbox := realtime.NewOutboxStore()
	cache := realtime.NewCacheCoherence()
	hub := realtime.NewRealtimeHub(8)
	disp := realtime.NewDispatcher(outbox, cache, hub)
	svc := teams.NewTeamService(access.NewAuthorizer(), outbox, disp)

	nonAdmin := auth.Principal{UserID: 3, AuthMethod: "session", TeamRoles: map[int64]auth.Role{10: auth.RoleMember}}
	if err := svc.GrantAccess(context.Background(), nonAdmin, 10, 5, auth.ResourceGrant{ResourceType: "application", ResourceID: 9, Level: auth.AccessWrite}); !errors.Is(err, access.ErrForbidden) {
		t.Fatalf("expected non-admin grant attempt denied, got %v", err)
	}

	owner := auth.Principal{UserID: 1, AuthMethod: "session", TeamRoles: map[int64]auth.Role{10: auth.RoleOwner}}
	if err := svc.GrantAccess(context.Background(), owner, 10, 5, auth.ResourceGrant{ResourceType: "application", ResourceID: 9, Level: auth.AccessWrite}); err != nil {
		t.Fatalf("GrantAccess failed: %v", err)
	}
	if cache.CurrentVersion("coolify:go:v1:team:10:auth_ver") == 0 {
		t.Fatal("expected grant change to bump team auth cache version")
	}
}
