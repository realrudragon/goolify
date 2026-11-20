package realtime_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/realtime"
)

func TestOutbox_RollbackEmitsZeroEvents(t *testing.T) {
	outbox := realtime.NewOutboxStore()
	evt := realtime.DomainEvent{
		EventID: "evt-1", SchemaVersion: "v1", Producer: "go-controlplane",
		TeamID: 0, ResourceType: "server", ResourceUUID: "srv-0",
		AggregateRevision: 1, OwnershipEpoch: 1, MutationType: "server.updated",
		CommittedAt: time.Now().UTC(),
	}
	if err := outbox.AppendInTx(context.Background(), false, evt); !errors.Is(err, realtime.ErrOutboxRolledBack) {
		t.Fatalf("expected ErrOutboxRolledBack, got %v", err)
	}
	if len(outbox.Pending()) != 0 {
		t.Fatal("rolled-back tx must not append outbox events")
	}
}

func TestOutbox_WarmCacheStaleRefillAndConcurrentWorkers(t *testing.T) {
	// Review Focus #5:
	// Worker A reads cache version V0 before mutation, computes stale list,
	// meanwhile Worker B commits mutation and bumps version to V1 via Outbox.
	// Worker A must fail TryPopulate(V0) so stale data never refills the cache.
	outbox := realtime.NewOutboxStore()
	cache := realtime.NewCacheCoherence()
	hub := realtime.NewRealtimeHub(8)
	disp := realtime.NewDispatcher(outbox, cache, hub)
	ctx := context.Background()

	vkey := "coolify:go:v1:team:10:search_ver"
	v0 := cache.CurrentVersion(vkey)
	if !cache.TryPopulate(vkey, v0, "stale-server-name") {
		t.Fatal("initial cache populate failed")
	}

	// Reader A reads v0 before mutation
	observedVer := cache.CurrentVersion(vkey)

	// Subscriber channels for team 10 and team 20 (cross-team isolation check)
	ch10, cancel10 := hub.Subscribe(10)
	defer cancel10()
	ch20, cancel20 := hub.Subscribe(20)
	defer cancel20()

	evt := realtime.DomainEvent{
		EventID: "evt-srv-rename", SchemaVersion: "v1", Producer: "go-controlplane",
		TeamID: 10, ResourceType: "server", ResourceUUID: "srv-1",
		AggregateRevision: 2, OwnershipEpoch: 1, MutationType: "server.updated",
		CommittedAt: time.Now().UTC(),
	}
	if err := outbox.AppendInTx(ctx, true, evt); err != nil {
		t.Fatalf("AppendInTx: %v", err)
	}
	if err := disp.FlushPending(ctx); err != nil {
		t.Fatalf("FlushPending: %v", err)
	}

	// Stale refill from Worker A at observedVer must be rejected
	if cache.TryPopulate(vkey, observedVer, "stale-server-name") {
		t.Fatal("stale reader refill must be rejected after version bump")
	}

	// Fresh read at new version succeeds
	v1 := cache.CurrentVersion(vkey)
	if !cache.TryPopulate(vkey, v1, "fresh-server-name") {
		t.Fatal("fresh reader populate failed")
	}
	val, _, ok := cache.Read(vkey)
	if !ok || val != "fresh-server-name" {
		t.Fatalf("unexpected cached value: %v ok=%v", val, ok)
	}

	// Team 10 received event, Team 20 did not
	select {
	case got := <-ch10:
		if got.EventID != "evt-srv-rename" {
			t.Fatalf("unexpected event: %+v", got)
		}
	default:
		t.Fatal("expected team 10 subscriber to receive event")
	}
	select {
	case leaked := <-ch20:
		t.Fatalf("cross-team channel leak to team 20: %+v", leaked)
	default:
	}

	// Outage replay check
	cache.SetUnavailable(true)
	evt2 := evt
	evt2.EventID = "evt-during-outage"
	_ = outbox.AppendInTx(ctx, true, evt2)
	if err := disp.FlushPending(ctx); err == nil {
		t.Fatal("expected flush to fail during cache outage")
	}
	cache.SetUnavailable(false)
	if err := disp.FlushPending(ctx); err != nil {
		t.Fatalf("expected replay after outage recovery to succeed: %v", err)
	}
}
