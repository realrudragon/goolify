package servers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/servers"
	"github.com/coollabsio/coolify/backend/internal/realtime"
)

func TestServerService_SessionSwitchIsolation(t *testing.T) {
	// Review Focus #1: member of resource team 10, owner of session team 20
	repo := servers.NewMemoryRepository()
	repo.SeedServer(servers.Server{
		ID: 100, UUID: "srv-team10", Name: "prod-1", IP: "10.0.0.5", Port: 22, User: "root",
		TeamID: 10, PrivateKeyID: 1, PrivateKeyUUID: "pk-10",
	})
	repo.SeedServer(servers.Server{
		ID: 0, UUID: "localhost", Name: "localhost", IP: "host.docker.internal", Port: 22, User: "root",
		TeamID: 0, PrivateKeyID: 0, PrivateKeyUUID: "pk-0",
	})
	repo.SeedPrivateKey(servers.PrivateKeyRef{ID: 1, UUID: "pk-10", TeamID: 10})
	repo.SeedPrivateKey(servers.PrivateKeyRef{ID: 9, UUID: "pk-99", TeamID: 99})

	outbox := realtime.NewOutboxStore()
	cache := realtime.NewCacheCoherence()
	hub := realtime.NewRealtimeHub(8)
	disp := realtime.NewDispatcher(outbox, cache, hub)
	svc := servers.NewServerService(repo, access.NewAuthorizer(), compat.NewEloquentHooks(), outbox, disp)

	memberTeam10OwnerTeam20 := auth.Principal{
		UserID:         7,
		AuthMethod:     "session",
		SessionTeamID:  20,
		TeamRoles:      map[int64]auth.Role{10: auth.RoleMember, 20: auth.RoleOwner, 0: auth.RoleMember},
		TeamWideAccess: map[int64]bool{10: false, 20: true, 0: true},
	}

	ctx := context.Background()
	if _, err := svc.Get(ctx, memberTeam10OwnerTeam20, "srv-team10"); err != nil {
		t.Fatalf("team 10 member should read server: %v", err)
	}
	newName := "renamed"
	if _, err := svc.Update(ctx, memberTeam10OwnerTeam20, "srv-team10", servers.UpdateServerInput{Name: &newName}); !errors.Is(err, access.ErrForbidden) {
		t.Fatalf("expected ErrForbidden on update despite owning session team 20, got %v", err)
	}

	// Foreign private key check (even for team 10 admin)
	adminTeam10 := auth.Principal{
		UserID:    8,
		AuthMethod: "session",
		TeamRoles: map[int64]auth.Role{10: auth.RoleAdmin, 0: auth.RoleOwner},
	}
	foreignKey := "pk-99"
	if _, err := svc.Update(ctx, adminTeam10, "srv-team10", servers.UpdateServerInput{PrivateKeyUUID: &foreignKey}); !errors.Is(err, servers.ErrPrivateKeyTeam) {
		t.Fatalf("expected ErrPrivateKeyTeam, got %v", err)
	}

	// Localhost sentinel id=0 restrictions: non-admin cannot place, owner cannot delete id=0
	if err := svc.ValidatePlacement(ctx, memberTeam10OwnerTeam20, "localhost", 0); !errors.Is(err, access.ErrForbidden) {
		t.Fatalf("expected ErrForbidden placing resource on localhost as member, got %v", err)
	}
	if err := svc.Delete(ctx, adminTeam10, "localhost"); !errors.Is(err, servers.ErrInvalidLocalhost) {
		t.Fatalf("expected ErrInvalidLocalhost deleting id=0 server, got %v", err)
	}
}

func TestServerService_CacheCoherenceOnUpdate(t *testing.T) {
	// Review Focus #5: server mutation invalidates team search cache version without flushing all Redis
	repo := servers.NewMemoryRepository()
	repo.SeedPrivateKey(servers.PrivateKeyRef{ID: 1, UUID: "pk-10", TeamID: 10})
	now := time.Now().UTC()
	repo.SeedServer(servers.Server{
		ID: 101, UUID: "srv-transferred", Name: "archived", TeamID: 10, TransferredAt: &now,
	})

	outbox := realtime.NewOutboxStore()
	cache := realtime.NewCacheCoherence()
	hub := realtime.NewRealtimeHub(8)
	disp := realtime.NewDispatcher(outbox, cache, hub)
	svc := servers.NewServerService(repo, access.NewAuthorizer(), compat.NewEloquentHooks(), outbox, disp)

	admin := auth.Principal{
		UserID:        1,
		AuthMethod:    "session",
		SessionTeamID: 10,
		TeamRoles:     map[int64]auth.Role{10: auth.RoleAdmin},
	}
	ctx := context.Background()

	vkey := "coolify:go:v1:team:10:search_ver"
	vBefore := cache.CurrentVersion(vkey)

	srv, err := svc.Create(ctx, admin, servers.CreateServerInput{
		Name: "  edge-node-1  ", IP: "10.1.1.2", TeamID: 10, PrivateKeyUUID: "pk-10",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if srv.Name != "edge-node-1" || srv.Port != 22 || srv.User != "root" {
		t.Fatalf("unexpected defaults/sanitization: %+v", srv)
	}
	if cache.CurrentVersion(vkey) <= vBefore {
		t.Fatal("expected team 10 search cache version bump on server create")
	}

	// Transferred-away server blocks updates/placement, allows archive delete without remote side effects
	if err := svc.ValidatePlacement(ctx, admin, "srv-transferred", 10); !errors.Is(err, servers.ErrTransferredServer) {
		t.Fatalf("expected ErrTransferredServer on placement, got %v", err)
	}
	if err := svc.Delete(ctx, admin, "srv-transferred"); err != nil {
		t.Fatalf("archive delete should succeed cleanly without SSH side effects: %v", err)
	}
}
