package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/opscli"
	"github.com/coollabsio/coolify/backend/internal/store/postgres"
	goredis "github.com/coollabsio/coolify/backend/internal/store/redis"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: coolify-cli <command> [flags]")
		fmt.Fprintln(os.Stderr, "commands: init | root:reset-password | root:change-email | admin:delete-user | app:reencrypt | queue:cleanup | cleanup:stuck-resources | cleanup:import-uploads | cleanup:orphaned-dns | cleanup:unverified-users | cache:clear-search | schedule:diagnostics | users:export | services:validate-templates | openapi:generate")
		os.Exit(2)
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dsnFlag := fs.String("dsn", os.Getenv("DATABASE_URL"), "PostgreSQL DSN")
	redisFlag := fs.String("redis", os.Getenv("REDIS_URL"), "Redis host:port")
	appKeyFlag := fs.String("app-key", os.Getenv("APP_KEY"), "Laravel APP_KEY")
	emailFlag := fs.String("email", "", "User email")
	passFlag := fs.String("password", "", "Password")
	oldKeyFlag := fs.String("old-key", "", "Old APP_KEY for app:reencrypt")
	newKeyFlag := fs.String("new-key", "", "New APP_KEY for app:reencrypt")
	templatesFlag := fs.String("templates-dir", "templates/compose", "Templates directory")
	ageMinutes := fs.Int("age-minutes", 120, "Age threshold in minutes")
	_ = fs.Parse(os.Args[2:])

	if cmd == "openapi:generate" {
		b, err := opscli.GenerateOpenAPIJSON("4.0.0-standalone")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(string(b))
		return
	}
	if cmd == "services:validate-templates" {
		n, err := opscli.ValidateComposeTemplates(*templatesFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(map[string]any{"validated_templates": n}))
		return
	}

	if *dsnFlag == "" {
		fmt.Fprintln(os.Stderr, "error: --dsn or DATABASE_URL is required")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := postgres.Connect(ctx, *dsnFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: connect postgres: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	var rdb *goredis.Client
	if *redisFlag != "" {
		if rc, rerr := goredis.ParseAndNew(*redisFlag, "coolify:go:v1:"); rerr == nil {
			rdb = rc
			defer func() { _ = rdb.Close() }()
		}
	}
	var ciph *compat.Cipher
	if *appKeyFlag != "" {
		ciph, _ = compat.NewCipher(*appKeyFlag)
	}
	runner := opscli.NewRunner(pool, rdb, ciph)

	switch cmd {
	case "init", "seed":
		if err := runner.InitAndSeed(ctx, *emailFlag, *passFlag); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(`{"status":"ok","command":"init"}`)
	case "root:reset-password":
		if err := runner.RootResetPassword(ctx, *passFlag); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(`{"status":"ok","command":"root:reset-password"}`)
	case "root:change-email":
		if err := runner.RootChangeEmail(ctx, *emailFlag); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(`{"status":"ok","command":"root:change-email"}`)
	case "admin:delete-user":
		uid, err := runner.AdminDeleteUser(ctx, *emailFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(map[string]any{"status": "ok", "deleted_user_id": uid}))
	case "app:reencrypt":
		n, err := runner.ReencryptWithAppKey(ctx, *oldKeyFlag, *newKeyFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(map[string]any{"status": "ok", "reencrypted_cells": n}))
	case "queue:cleanup":
		n, err := runner.CleanupDeploymentQueue(ctx, time.Duration(*ageMinutes)*time.Minute)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(map[string]any{"status": "ok", "cleaned_deployments": n}))
	case "cleanup:stuck-resources":
		n, err := runner.CleanupStuckResources(ctx, time.Duration(*ageMinutes)*time.Minute)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(map[string]any{"status": "ok", "reset_resources": n}))
	case "cleanup:import-uploads":
		n, err := runner.CleanupImportUploads(ctx, time.Duration(*ageMinutes)*time.Minute)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(map[string]any{"status": "ok", "expired_uploads": n}))
	case "cleanup:orphaned-dns":
		n, err := runner.ReleaseOrphanedManagedDNS(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(map[string]any{"status": "ok", "deleted_records": n}))
	case "cleanup:unverified-users":
		n, err := runner.CleanupUnverifiedUsers(ctx, time.Duration(*ageMinutes)*time.Minute)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(map[string]any{"status": "ok", "deleted_users": n}))
	case "cache:clear-search":
		n, err := runner.ClearGlobalSearchCache(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(map[string]any{"status": "ok", "teams_invalidated": n}))
	case "schedule:diagnostics":
		d, err := runner.ScheduledJobDiagnostics(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(d))
	case "users:export":
		u, err := runner.ExportUsers(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(u))
	case "cleanup:names":
		n, err := runner.CleanupNames(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(map[string]any{"status": "ok", "cleaned_names": n}))
	case "oauth:rekey":
		n, err := runner.RekeyOauthIdentities(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(map[string]any{"status": "ok", "rekeyed_identities": n}))
	case "user:allow-sso-link":
		n, err := runner.AllowSSOLink(ctx, *emailFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(map[string]any{"status": "ok", "cleared_bindings": n}))
	case "schedule:logs":
		logs, err := runner.ViewScheduledLogs(ctx, 25)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(logs))
	case "services:delete-orphans":
		n, err := runner.DeleteOrphanedServices(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(map[string]any{"status": "ok", "deleted_services": n}))
	case "cloud:reconcile-subscriptions":
		n, err := runner.ReconcileSubscriptions(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(map[string]any{"status": "ok", "reconciled": n}))
	case "dev", "dev:info":
		info, err := runner.DevInfo(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(info))
	case "testing:schema":
		res, err := runner.TestingSchemaCheck(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(res))
	case "horizon", "horizon:status":
		st, err := runner.HorizonStatus(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(st))
	case "dev:seed-traefik-certs":
		if err := runner.SeedDevTraefikCert(ctx, "localhost.sslip.io"); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(`{"status":"ok","command":"dev:seed-traefik-certs"}`)
	case "dev:start-instance-db":
		if err := runner.StartDevInstanceDB(ctx); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(`{"status":"ok","command":"dev:start-instance-db"}`)
	case "dev:qemu", "dev:seed-qemu", "dev:gpu-driver":
		u, err := runner.SeedDevQemuServer(ctx, "dev-qemu-vm", "127.0.0.1")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(opscli.MustJSON(map[string]any{"status": "ok", "server_uuid": u}))
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		os.Exit(2)
	}
}
