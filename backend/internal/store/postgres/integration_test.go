package postgres_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/domains/schedules"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/coollabsio/coolify/backend/internal/realtime"
	"github.com/coollabsio/coolify/backend/internal/store/postgres"
	goredis "github.com/coollabsio/coolify/backend/internal/store/redis"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

func openTestInfra(t *testing.T) (*pgxpool.Pool, *goredis.Client) {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://coolify:coolify_test_pw@127.0.0.1:15432/coolify_test?sslmode=disable"
	}
	redisURL := os.Getenv("TEST_REDIS_URL")
	if redisURL == "" {
		redisURL = "redis://127.0.0.1:16379/0"
	}

	ctx := context.Background()
	pool, err := postgres.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("isolated test postgres unavailable (%s): %v", dsn, err)
	}
	t.Cleanup(func() { pool.Close() })

	// Reset test schema cleanly on isolated test DB only
	_, err = pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`)
	if err != nil {
		t.Fatalf("reset isolated test schema: %v", err)
	}
	if err := postgres.EnsureBaselineSchema(ctx, pool); err != nil {
		t.Fatalf("EnsureBaselineSchema: %v", err)
	}

	rc, err := goredis.ParseAndNew(redisURL, "coolify:go:v1:test:")
	if err != nil {
		t.Fatalf("parse test redis: %v", err)
	}
	if err := rc.Ping(ctx); err != nil {
		t.Skipf("isolated test redis unavailable (%s): %v", redisURL, err)
	}
	_, _ = rc.FlushNamespace(ctx)
	t.Cleanup(func() {
		_, _ = rc.FlushNamespace(context.Background())
		_ = rc.Close()
	})
	return pool, rc
}

func TestPostgres_SentinelsAuthSessionBearerAndSwitchedSession(t *testing.T) {
	pool, _ := openTestInfra(t)
	ctx := context.Background()

	// Seed Laravel $2y$ bcrypt hash for root user id=0
	hashBytes, err := bcrypt.GenerateFromPassword([]byte("RootPass123!"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	laravelHash := "$2y$" + string(hashBytes[4:])

	if err := postgres.SeedInstanceSentinels(ctx, pool, "root@coolify.test", laravelHash, "enc-key-0"); err != nil {
		t.Fatalf("SeedInstanceSentinels: %v", err)
	}

	// Verify sentinel id = 0 rows exist AND ordinary inserts get id >= 1
	var teamCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM teams WHERE id = 0`).Scan(&teamCount); err != nil || teamCount != 1 {
		t.Fatalf("expected sentinel root team id=0, count=%d err=%v", teamCount, err)
	}
	var coolifyDBName string
	if err := pool.QueryRow(ctx, `SELECT name FROM standalone_postgresqls WHERE id = 0`).Scan(&coolifyDBName); err != nil || coolifyDBName != "coolify-db" {
		t.Fatalf("expected sentinel coolify-db id=0, got %q err=%v", coolifyDBName, err)
	}

	authRepo := auth.NewPostgresAuthRepository(pool)
	// Authenticate root with Laravel $2y$ hash
	u, defaultTeam, err := authRepo.AuthenticateCredentials(ctx, "root@coolify.test", "RootPass123!", "")
	if err != nil || u.ID != 0 || defaultTeam != 0 {
		t.Fatalf("root login failed: u=%+v team=%d err=%v", u, defaultTeam, err)
	}

	// Create ordinary user (must get id >= 1) who is member of team 10 (read-only) and owner of their own team
	bob, bobOwnTeam, err := authRepo.CreateUserWithTeam(ctx, "Bob", "bob@example.com", "BobSecret99!", "Bob Personal")
	if err != nil {
		t.Fatalf("CreateUserWithTeam: %v", err)
	}
	if bob.ID <= 0 || bobOwnTeam <= 0 {
		t.Fatalf("ordinary user/team must not receive sentinel id=0: user=%d team=%d", bob.ID, bobOwnTeam)
	}

	// Create Team 10 and make Bob a read-only member there
	_, err = pool.Exec(ctx, `INSERT INTO teams (id, name) VALUES (10, 'Alpha Team')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO team_user (team_id, user_id, role, access) VALUES (10, $1, 'member', NULL)`, bob.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Issue durable PostgreSQL session and verify survival across a brand-new store/authenticator instance (simulating restart)
	sessStore1 := auth.NewPostgresSessionStore(pool)
	lookup1 := auth.NewPostgresPrincipalLookup(pool)
	auth1 := auth.NewAuthenticator(lookup1, sessStore1)
	sess, err := auth1.IssueSession(ctx, bob.ID, 10, time.Hour)
	if err != nil {
		t.Fatalf("IssueSession: %v", err)
	}

	// Simulate process restart with new instances
	sessStore2 := auth.NewPostgresSessionStore(pool)
	lookup2 := auth.NewPostgresPrincipalLookup(pool)
	auth2 := auth.NewAuthenticator(lookup2, sessStore2)

	// Switch session team to Bob's own team (where he is Owner!)
	if _, err := auth2.UpdateSessionTeam(ctx, sess.ID, bobOwnTeam); err != nil {
		t.Fatalf("UpdateSessionTeam: %v", err)
	}

	// Mutating request without CSRF must fail with ErrCSRFMismatch
	mutReq := httptest.NewRequest(http.MethodPost, "/api/v1/servers", nil)
	mutReq.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: sess.ID})
	if _, err := auth2.AuthenticateSession(ctx, mutReq); err != auth.ErrCSRFMismatch {
		t.Fatalf("expected ErrCSRFMismatch without header, got %v", err)
	}

	// With valid CSRF header, session authenticates with SessionTeamID = bobOwnTeam
	mutReq.Header.Set("X-CSRF-TOKEN", sess.CSRFToken)
	p, err := auth2.AuthenticateSession(ctx, mutReq)
	if err != nil || p.SessionTeamID != bobOwnTeam {
		t.Fatalf("AuthenticateSession failed: p=%+v err=%v", p, err)
	}

	// Review Focus #1: Even though SessionTeamID is bobOwnTeam (Owner), updating a resource in Team 10 MUST BE DENIED!
	authz := access.NewAuthorizer()
	team10 := int64(10)
	if err := authz.Check(ctx, p, access.ActionUpdate, access.ResourceRef{
		Type: "server", ID: 99, TeamID: &team10, APISegment: "servers",
	}); err == nil {
		t.Fatal("CRITICAL: switched session owner role leaked write permission onto Team 10 resource!")
	}

	// Test Sanctum bearer token + API disabled vs MCP enabled (SECURITY.md intended behavior)
	tokID, rawTok, err := authRepo.IssueAPIToken(ctx, 0, 0, "ci-token", []string{"read"}, nil, nil)
	if err != nil || tokID <= 0 {
		t.Fatalf("IssueAPIToken: id=%d err=%v", tokID, err)
	}
	if _, err := auth2.AuthenticateBearer(ctx, "Bearer "+rawTok); err != nil {
		t.Fatalf("AuthenticateBearer expected success: %v", err)
	}

	// Disable REST API, enable MCP: REST bearer MUST fail with ErrAPIDisabled, MCP MUST succeed!
	_, err = pool.Exec(ctx, `UPDATE instance_settings SET is_api_enabled = FALSE, is_mcp_server_enabled = TRUE WHERE id = 0`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth2.AuthenticateBearer(ctx, "Bearer "+rawTok); err != auth.ErrAPIDisabled {
		t.Fatalf("expected ErrAPIDisabled when REST API is disabled, got %v", err)
	}
	mcpPrincipal, err := auth2.AuthenticateMCP(ctx, "Bearer "+rawTok)
	if err != nil || mcpPrincipal.AuthMethod != "mcp" {
		t.Fatalf("expected MCP token to work when REST API is disabled, got p=%+v err=%v", mcpPrincipal, err)
	}
}

func TestPostgresOutbox_RollbackStaleRefillAndRedisOutage(t *testing.T) {
	pool, rc := openTestInfra(t)
	ctx := context.Background()

	outbox := realtime.NewPostgresOutboxStore(pool)
	cache := realtime.NewRedisCacheCoherence(rc)
	hub := realtime.NewRealtimeHub(16)
	disp := realtime.NewPostgresDispatcher(outbox, cache, rc, hub)

	// 1. Rolled-back transaction must leave ZERO outbox events
	tx1, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := outbox.AppendInTx(ctx, tx1, realtime.DomainEvent{
		EventID: "evt-rolled-back", Producer: "test", TeamID: 10, ResourceType: "server", ResourceUUID: "srv-1", MutationType: "server.updated",
	}); err != nil {
		t.Fatalf("AppendInTx: %v", err)
	}
	_ = tx1.Rollback(ctx)

	pending, err := outbox.Pending(ctx, 10)
	if err != nil || len(pending) != 0 {
		t.Fatalf("expected 0 outbox rows after rollback, got %d err=%v", len(pending), err)
	}

	// 2. Stale-refill race test on real Redis
	vkey := "coolify:go:v1:team:10:search_ver"
	vBefore, err := cache.CurrentVersion(ctx, vkey)
	if err != nil {
		t.Fatalf("CurrentVersion: %v", err)
	}
	// Worker A reads stale data at vBefore; before Worker A writes back, Worker B commits & dispatches
	tx2, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := outbox.AppendInTx(ctx, tx2, realtime.DomainEvent{
		EventID: "evt-committed-1", Producer: "test", TeamID: 10, ResourceType: "server", ResourceUUID: "srv-1", MutationType: "server.updated",
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := disp.FlushPending(ctx); err != nil || n != 1 {
		t.Fatalf("FlushPending expected 1, got %d err=%v", n, err)
	}

	// Worker A's stale CAS populate MUST fail!
	ok, err := cache.TryPopulate(ctx, vkey, vBefore, []string{"stale-server-name"})
	if err != nil || ok {
		t.Fatalf("stale-refill CAS must be rejected! ok=%v err=%v", ok, err)
	}

	// Fresh reader at current version succeeds
	vAfter, err := cache.CurrentVersion(ctx, vkey)
	if err != nil || vAfter <= vBefore {
		t.Fatalf("expected bumped version > %d, got %d err=%v", vBefore, vAfter, err)
	}
	ok, err = cache.TryPopulate(ctx, vkey, vAfter, []string{"fresh-server-name"})
	if err != nil || !ok {
		t.Fatalf("fresh CAS populate should succeed: ok=%v err=%v", ok, err)
	}
	var cached []string
	_, hit, err := cache.Read(ctx, vkey, &cached)
	if err != nil || !hit || len(cached) != 1 || cached[0] != "fresh-server-name" {
		t.Fatalf("expected fresh cache hit, hit=%v cached=%v err=%v", hit, cached, err)
	}

	// 3. Redis outage: dispatcher fails closed and preserves outbox row until Redis recovers
	deadRedis, _ := goredis.ParseAndNew("redis://127.0.0.1:16399/0", "coolify:go:v1:test:")
	deadDisp := realtime.NewPostgresDispatcher(outbox, realtime.NewRedisCacheCoherence(deadRedis), deadRedis, hub)
	tx3, _ := pool.Begin(ctx)
	_ = outbox.AppendInTx(ctx, tx3, realtime.DomainEvent{
		EventID: "evt-during-outage", Producer: "test", TeamID: 10, ResourceType: "server", ResourceUUID: "srv-2", MutationType: "server.updated",
	})
	_ = tx3.Commit(ctx)
	if _, err := deadDisp.FlushPending(ctx); err == nil {
		t.Fatal("expected error during Redis outage")
	}
	// Once healthy Redis dispatcher runs, durable outbox event replays cleanly
	if n, err := disp.FlushPending(ctx); err != nil || n != 1 {
		t.Fatalf("expected durable replay of 1 event after Redis recovery, got %d err=%v", n, err)
	}
}

func TestPostgresJobsAndSchedules_FencingConcurrencyAndRestart(t *testing.T) {
	pool, _ := openTestInfra(t)
	ctx := context.Background()

	coord1 := jobs.NewPostgresJobCoordinator(pool, 2)
	if err := coord1.AdmitOperation(ctx, jobs.DurableJobSpec{
		JobOperation: jobs.JobOperation{
			JobID: "job-1", OperationID: "op-durable-1", AttemptID: "att-0",
			ResourceUUID: "app-1", ServerUUID: "srv-1", TeamID: 10, MaxAttempts: 3,
		},
		JobType: "deploy",
		Payload: map[string]any{"image": "nginx:alpine"},
	}); err != nil {
		t.Fatalf("AdmitOperation: %v", err)
	}

	// Concurrent workers racing to claim the single queued job -> strictly ONE wins
	var claimed atomic.Int32
	var winningLease jobs.JobLease
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		wid := "worker-" + string(rune('a'+i))
		go func(worker string) {
			defer wg.Done()
			c := jobs.NewPostgresJobCoordinator(pool, 2)
			_, lease, ok, err := c.ClaimNext(ctx, worker, 350*time.Millisecond)
			if err == nil && ok {
				claimed.Add(1)
				mu.Lock()
				winningLease = lease
				mu.Unlock()
			}
		}(wid)
	}
	wg.Wait()
	if claimed.Load() != 1 {
		t.Fatalf("expected exactly 1 worker to claim op-durable-1, got %d", claimed.Load())
	}

	// Wait for lease to expire, then fresh coordinator (restarted worker) reclaims with epoch+1
	time.Sleep(450 * time.Millisecond)
	coord2 := jobs.NewPostgresJobCoordinator(pool, 2)
	reclaimedSpec, lease2, ok, err := coord2.ClaimNext(ctx, "worker-fresh", time.Minute)
	if err != nil || !ok || lease2.OwnershipEpoch <= winningLease.OwnershipEpoch {
		t.Fatalf("expected fresh worker reclaim with higher epoch, ok=%v lease2=%+v err=%v", ok, lease2, err)
	}
	if reclaimedSpec.Payload["_reclaimed_after_lease_expiry"] != true {
		t.Fatalf("expected _reclaimed_after_lease_expiry marker so worker reconciles before side effects")
	}

	// Stale worker from epoch 1 MUST BE FENCED OUT on MarkTerminal!
	if err := coord1.MarkTerminal(ctx, "op-durable-1", winningLease.OwnershipEpoch, jobs.StatusSucceeded, ""); err != jobs.ErrLeaseHeldByOtherEpoch {
		t.Fatalf("expected stale worker fenced with ErrLeaseHeldByOtherEpoch, got %v", err)
	}

	// Current epoch worker succeeds
	if err := coord2.MarkTerminal(ctx, "op-durable-1", lease2.OwnershipEpoch, jobs.StatusSucceeded, ""); err != nil {
		t.Fatalf("MarkTerminal epoch 2 failed: %v", err)
	}

	// Distributed ScheduleCoordinator across 8 concurrent workers (including legacy schedule_id = 0)
	var winners atomic.Int32
	now := time.Date(2026, 11, 1, 1, 30, 0, 0, time.UTC)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			sc := schedules.NewPostgresScheduleCoordinator(pool)
			won, err := sc.AcquireDueLease(ctx, 0, "w", now, "America/New_York")
			if err == nil && won {
				winners.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Fatalf("expected exactly 1 schedule lease winner across 8 workers, got %d", winners.Load())
	}
}
