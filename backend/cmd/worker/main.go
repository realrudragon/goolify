package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/notifications"
	"github.com/coollabsio/coolify/backend/internal/domains/schedules"
	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/coollabsio/coolify/backend/internal/realtime"
	"github.com/coollabsio/coolify/backend/internal/store/postgres"
	goredis "github.com/coollabsio/coolify/backend/internal/store/redis"
	"github.com/coollabsio/coolify/backend/internal/worker"
)

func main() {
	executorBin := flag.String("executor-bin", "coolify-executor", "Path to Rust executor binary")
	concurrency := flag.Int("server-concurrency", 4, "Per-server concurrent job limit")
	workerID := flag.String("worker-id", "", "Unique worker identifier")
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required (production worker refuses to run without PostgreSQL)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()

	if err := postgres.EnsureGoAdditiveSchema(ctx, pool); err != nil {
		log.Fatalf("ensure additive schema: %v", err)
	}

	var redisClient *goredis.Client
	var cache *realtime.RedisCacheCoherence
	if redisURL := os.Getenv("REDIS_URL"); redisURL != "" {
		prefix := os.Getenv("REDIS_PREFIX")
		if prefix == "" {
			prefix = "coolify:go:v1:"
		}
		if rc, err := goredis.ParseAndNew(redisURL, prefix); err == nil {
			if pErr := rc.Ping(ctx); pErr == nil {
				redisClient = rc
				cache = realtime.NewRedisCacheCoherence(rc)
				defer func() { _ = rc.Close() }()
			}
		}
	}

	outbox := realtime.NewPostgresOutboxStore(pool)
	hub := realtime.NewRealtimeHub(128)
	disp := realtime.NewPostgresDispatcher(outbox, cache, redisClient, hub)
	logStore := realtime.NewPostgresLogStore(pool)
	jobCoord := jobs.NewPostgresJobCoordinator(pool, *concurrency)
	schedCoord := schedules.NewPostgresScheduleCoordinator(pool)
	probe := executor.NewSSHAwareRemoteProbe(pool, nil, nil, true)
	tracker := executor.NewRemoteTracker(probe)

	var execTransport executor.Transport
	if _, statErr := os.Stat(*executorBin); statErr == nil {
		execTransport = &executor.SubprocessTransport{BinaryPath: *executorBin}
	} else {
		execTransport = executor.NewLocalCommandTransport()
	}
	execClient := executor.NewClient(execTransport)

	var cipher *compat.Cipher
	if appKey := os.Getenv("APP_KEY"); appKey != "" {
		c, cErr := compat.NewCipher(appKey)
		if cErr != nil {
			log.Fatalf("invalid APP_KEY: %v", cErr)
		}
		cipher = c
	}

	probe = executor.NewSSHAwareRemoteProbe(pool, execClient, func(raw string) string {
		if cipher != nil {
			if plain, decErr := cipher.DecryptLaravelPayload(raw, false); decErr == nil {
				return string(plain)
			}
		}
		return raw
	}, true)
	tracker = executor.NewRemoteTracker(probe)

	authz := access.NewAuthorizer()
	notifySvc := notifications.NewPostgresNotificationService(pool, authz, cipher)

	runner := worker.NewRunner(worker.Config{
		WorkerID:      *workerID,
		Pool:          pool,
		NotifySvc:     notifySvc,
		Cipher:        cipher,
		JobCoord:      jobCoord,
		ScheduleCoord: schedCoord,
		Outbox:        outbox,
		Dispatcher:    disp,
		LogStore:      logStore,
		Executor:      execClient,
		Tracker:       tracker,
		Probe:         probe,
		BackupDir:     os.Getenv("COOLIFY_BACKUP_DIR"),
		LeaseTTL:      2 * time.Minute,
	})

	log.Printf("coolify go worker started (executor=%s, server_concurrency=%d)", *executorBin, *concurrency)
	runner.Run(ctx)
	log.Println("worker shutting down gracefully")
}
