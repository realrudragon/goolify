package worker_test

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/domains/schedules"
	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/coollabsio/coolify/backend/internal/realtime"
	"github.com/coollabsio/coolify/backend/internal/store/postgres"
	"github.com/coollabsio/coolify/backend/internal/worker"
	"github.com/jackc/pgx/v5/pgxpool"
)

type ambiguousTransport struct{}

func (a *ambiguousTransport) StreamNDJSON(_ context.Context, req executor.ExecutionRequest, emit func(executor.ExecutionEvent) error) (int, error) {
	_ = emit(executor.ExecutionEvent{
		ProtocolVersion: executor.ProtocolVersionV1,
		EventType:       "started",
		OperationID:     req.OperationID,
		AttemptID:       req.AttemptID,
		Sequence:        1,
	})
	return 1, errors.New("unexpected NDJSON EOF: local worker killed before remote exit")
}

func openWorkerTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://coolify:coolify_test_pw@127.0.0.1:15432/coolify_test?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := postgres.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("isolated test postgres unavailable: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	_, _ = pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`)
	if err := postgres.EnsureBaselineSchema(ctx, pool); err != nil {
		t.Fatalf("EnsureBaselineSchema: %v", err)
	}
	if err := postgres.SeedInstanceSentinels(ctx, pool, "root@coolify.test", "hash", "key"); err != nil {
		t.Fatalf("SeedInstanceSentinels: %v", err)
	}
	return pool
}

func TestWorker_AmbiguousEOFAndReclaimReconcileBlocksDuplicateDeploy(t *testing.T) {
	pool := openWorkerTestDB(t)
	ctx := context.Background()

	jobCoord := jobs.NewPostgresJobCoordinator(pool, 4)
	outbox := realtime.NewPostgresOutboxStore(pool)
	logStore := realtime.NewPostgresLogStore(pool)

	// Seed a real project/environment/application row so DeploymentContextResolver + DeploymentPlanner run end-to-end
	_, _ = pool.Exec(ctx, `INSERT INTO projects (id, uuid, name, team_id) VALUES (1, 'proj-amb', 'AmbProj', 0)`)
	_, _ = pool.Exec(ctx, `INSERT INTO environments (id, uuid, name, project_id) VALUES (1, 'env-amb', 'production', 1)`)
	_, _ = pool.Exec(ctx, `
		INSERT INTO applications (id, uuid, name, build_pack, docker_registry_image_name, docker_registry_image_tag, environment_id, destination_id)
		VALUES (1, 'app-amb', 'amb-app', 'dockerimage', 'nginx', 'alpine', 1, 0)
	`)

	// 1. Ambiguous EOF without Exited frame -> MUST mark StatusUnknown, NEVER failed or succeeded!
	if err := jobCoord.AdmitOperation(ctx, jobs.DurableJobSpec{
		JobOperation: jobs.JobOperation{
			JobID: "job-amb-1", OperationID: "op-amb-1", AttemptID: "att-0",
			ResourceUUID: "app-amb", ServerUUID: "localhost", TeamID: 0,
		},
		JobType: "deploy",
	}); err != nil {
		t.Fatal(err)
	}

	probe := executor.NewMemoryRemoteProbe("boot-1")
	w1 := worker.NewRunner(worker.Config{
		WorkerID: "w-eof",
		Pool:     pool,
		JobCoord: jobCoord,
		Outbox:   outbox,
		LogStore: logStore,
		Executor: executor.NewClient(&ambiguousTransport{}),
		Probe:    probe,
	})
	worked, err := w1.ProcessOneJob(ctx)
	if err != nil || !worked {
		t.Fatalf("ProcessOneJob failed: worked=%v err=%v", worked, err)
	}
	got, found, err := jobCoord.Get(ctx, "op-amb-1")
	if err != nil || !found || got.Status != jobs.StatusUnknown {
		t.Fatalf("expected StatusUnknown on ambiguous EOF, got %+v err=%v", got, err)
	}

	// 2. Worker crash mid-job + lease expiry reclaim while remote process is STILL alive -> MUST NOT execute a duplicate!
	if err := jobCoord.AdmitOperation(ctx, jobs.DurableJobSpec{
		JobOperation: jobs.JobOperation{
			JobID: "job-rec-1", OperationID: "op-rec-1", AttemptID: "att-0",
			ResourceUUID: "app-rec", ServerUUID: "localhost", TeamID: 0,
		},
		JobType: "deploy",
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate worker A claiming with short lease, writing remote journal, and dying before finishing
	leaseA, err := jobCoord.AcquireLease(ctx, "op-rec-1", "dead-worker", 1, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	probe.ProcAlive = true
	_ = probe.WriteJournalAtomic(ctx, "localhost", executor.RemoteJournal{
		ProtocolVersion: executor.ProtocolVersionV1,
		JobID:           "job-rec-1",
		OperationID:     "op-rec-1",
		AttemptID:       "att-1",
		OwnershipEpoch:  leaseA.OwnershipEpoch,
		ResourceUUID:    "app-rec",
		State:           executor.RemoteRunning,
		BootID:          "boot-1",
		PID:             999,
		UpdatedAt:       time.Now().UTC(),
	})

	time.Sleep(250 * time.Millisecond)

	var execCalls atomic.Int32
	w2 := worker.NewRunner(worker.Config{
		WorkerID: "fresh-worker",
		Pool:     pool,
		JobCoord: jobCoord,
		Outbox:   outbox,
		LogStore: logStore,
		Executor: executor.NewClient(executor.NewLocalCommandTransport()),
		Probe:    probe,
		CommandOverride: func(_ jobs.DurableJobSpec) (string, string) {
			execCalls.Add(1)
			return "local_exec", "echo SHOULD_NOT_RUN"
		},
	})
	worked2, err := w2.ProcessOneJob(ctx)
	if err != nil || !worked2 {
		t.Fatalf("reclaim ProcessOneJob failed: worked=%v err=%v", worked2, err)
	}
	if execCalls.Load() != 0 {
		t.Fatalf("CRITICAL: fresh worker executed duplicate side effect despite remote process running! calls=%d", execCalls.Load())
	}
	gotRec, _, _ := jobCoord.Get(ctx, "op-rec-1")
	if gotRec.Status != jobs.StatusUnknown {
		t.Fatalf("expected StatusUnknown when reclaimed operation is still running remotely, got %s", gotRec.Status)
	}
}

func TestWorker_ConcurrentTickSchedulesNeverDuplicatesBackupJob(t *testing.T) {
	pool := openWorkerTestDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `
		INSERT INTO scheduled_database_backups (uuid, enabled, frequency, timezone, database_id, database_type, team_id)
		VALUES ('sched-every-min', TRUE, '* * * * *', 'UTC', 0, 'postgresql', 0)
	`)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 19, 10, 15, 0, 0, time.UTC)
	var totalEnqueued atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			w := worker.NewRunner(worker.Config{
				WorkerID:      "sched-worker-" + string(rune('0'+idx)),
				Pool:          pool,
				JobCoord:      jobs.NewPostgresJobCoordinator(pool, 4),
				ScheduleCoord: schedules.NewPostgresScheduleCoordinator(pool),
			})
			n, err := w.TickSchedules(ctx, now)
			if err == nil {
				totalEnqueued.Add(int32(n))
			}
		}(i)
	}
	wg.Wait()
	if totalEnqueued.Load() != 1 {
		t.Fatalf("expected strictly 1 backup job admitted across 6 concurrent workers, got %d", totalEnqueued.Load())
	}
}
