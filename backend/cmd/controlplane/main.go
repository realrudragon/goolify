package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/controlplane"
	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/coollabsio/coolify/backend/internal/store/postgres"
	goredis "github.com/coollabsio/coolify/backend/internal/store/redis"
	"golang.org/x/crypto/bcrypt"
)

func main() {
	addr := flag.String("addr", ":8000", "HTTP listen address")
	staticDir := flag.String("static-dir", "../apps/web/dist", "Path to built Astro + Vue static assets")
	executorBin := flag.String("executor-bin", "", "Optional path to Rust coolify-executor binary")
	backupDir := flag.String("backup-dir", "", "Directory for local database backups")
	bootstrap := flag.Bool("bootstrap-schema", true, "Ensure additive Go schema & baseline tables if absent")
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is required (production controlplane refuses to run without PostgreSQL)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Connect(ctx, dsn)
	if err != nil {
		log.Fatalf("connect postgres: %v", err)
	}
	defer pool.Close()

	if *bootstrap {
		if err := postgres.EnsureBaselineSchema(ctx, pool); err != nil {
			log.Fatalf("ensure schema: %v", err)
		}
		rootEmail := os.Getenv("ROOT_USER_EMAIL")
		rootPass := os.Getenv("ROOT_USER_PASSWORD")
		var rootHash string
		if rootEmail != "" && rootPass != "" {
			if h, err := bcrypt.GenerateFromPassword([]byte(rootPass), bcrypt.DefaultCost); err == nil {
				rootHash = string(h)
			}
		}
		if err := postgres.SeedInstanceSentinels(ctx, pool, rootEmail, rootHash, ""); err != nil {
			log.Fatalf("seed sentinels: %v", err)
		}
	}

	var redisClient *goredis.Client
	if redisURL := os.Getenv("REDIS_URL"); redisURL != "" {
		prefix := os.Getenv("REDIS_PREFIX")
		if prefix == "" {
			prefix = "coolify:go:v1:"
		}
		if rc, err := goredis.ParseAndNew(redisURL, prefix); err == nil {
			if pErr := rc.Ping(ctx); pErr == nil {
				redisClient = rc
				defer func() { _ = rc.Close() }()
			} else {
				log.Printf("warning: redis ping failed (%v); cache coherence running fail-closed", pErr)
			}
		}
	}

	var cipher *compat.Cipher
	if appKey := os.Getenv("APP_KEY"); appKey != "" {
		c, err := compat.NewCipher(appKey)
		if err != nil {
			log.Fatalf("invalid APP_KEY: %v", err)
		}
		cipher = c
	}

	var execClient *executor.Client
	if *executorBin != "" {
		execClient = executor.NewClient(&executor.SubprocessTransport{BinaryPath: *executorBin})
	} else {
		execClient = executor.NewClient(executor.NewLocalCommandTransport())
	}

	app, err := controlplane.NewApp(controlplane.Config{
		Pool:          pool,
		Redis:         redisClient,
		Cipher:        cipher,
		StaticFS:      os.DirFS(*staticDir),
		BackupDir:     *backupDir,
		Executor:      execClient,
		AllowedOrigin: os.Getenv("APP_URL"),
	})
	if err != nil {
		log.Fatalf("init controlplane: %v", err)
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           app,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()

	log.Printf("coolify standalone controlplane listening on %s (static=%s)", *addr, *staticDir)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
