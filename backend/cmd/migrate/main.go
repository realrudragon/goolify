package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/store/postgres"
)

func main() {
	dsnFlag := flag.String("dsn", "", "PostgreSQL DSN (defaults to DATABASE_URL env)")
	appKeyFlag := flag.String("app-key", "", "Laravel APP_KEY for decryptability verification (defaults to APP_KEY env)")
	dryRun := flag.Bool("dry-run", true, "Run read-only validation without mutating database")
	applyAdditive := flag.Bool("apply-additive", false, "Apply expand-only go_* tables (requires --dry-run=false)")
	preflight := flag.Bool("cutover-preflight", false, "Run cutover preflight and rollback compatibility checks")
	receiptOut := flag.String("receipt-out", "", "Optional path to write measured JSON receipt")
	flag.Parse()

	dsn := *dsnFlag
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		fmt.Fprintln(os.Stderr, "error: --dsn or DATABASE_URL is required")
		os.Exit(2)
	}

	appKey := *appKeyFlag
	if appKey == "" {
		appKey = os.Getenv("APP_KEY")
	}
	var ciph *compat.Cipher
	if appKey != "" {
		var err error
		ciph, err = compat.NewCipher(appKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: invalid APP_KEY: %v\n", err)
			os.Exit(2)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := postgres.Connect(ctx, dsn)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: connect postgres: %v\n", err)
		os.Exit(1)
	}
	defer pool.Close()

	if *applyAdditive {
		if *dryRun {
			fmt.Fprintln(os.Stderr, "error: --apply-additive requires --dry-run=false")
			os.Exit(2)
		}
		if err := postgres.EnsureGoAdditiveSchema(ctx, pool); err != nil {
			fmt.Fprintf(os.Stderr, "error: apply additive schema: %v\n", err)
			os.Exit(1)
		}
	}

	digestKey := sha256.Sum256([]byte("coolify-migrate-digest:" + appKey))
	reader := compat.NewPostgresDatasetReader(pool, ciph)
	started := time.Now()

	checkedSecrets, failedSecrets, err := reader.VerifyEncryptedColumns(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: verify encrypted columns: %v\n", err)
		os.Exit(1)
	}
	if failedSecrets > 0 {
		fmt.Fprintf(os.Stderr, "error: %d of %d encrypted environment values failed APP_KEY decryption\n", failedSecrets, checkedSecrets)
		os.Exit(1)
	}

	journals, err := reader.LoadRemoteJournals(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load remote journals: %v\n", err)
		os.Exit(1)
	}
	migs, err := reader.LoadSchemaMigrations(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load schema migrations: %v\n", err)
		os.Exit(1)
	}
	changes := make([]SchemaChange, 0, len(migs))
	for _, m := range migs {
		changes = append(changes, SchemaChange{Name: m.Name, ExpandOnly: m.ExpandOnly})
	}

	runner := NewCutoverRunner(reader, digestKey[:], journals, changes)
	receipt, err := runner.Preflight(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: preflight failed: %v\n", err)
		os.Exit(1)
	}
	if *preflight && !receipt.RollbackCompatible {
		fmt.Fprintln(os.Stderr, "error: rollback compatibility check failed")
		os.Exit(1)
	}

	output := map[string]any{
		"dry_run":                   *dryRun,
		"cutover_preflight":         *preflight,
		"duration_ms":               time.Since(started).Milliseconds(),
		"validated_at":              receipt.ValidatedAt,
		"total_rows":                receipt.TotalRows,
		"sentinel_zero_tables":      receipt.SentinelZeroTables,
		"broken_fk_count":           receipt.BrokenFKCount,
		"encrypted_secrets_checked": checkedSecrets,
		"encrypted_secrets_failed":  failedSecrets,
		"preserved_remote_journals": receipt.PreservedRemoteJournals,
		"rollback_compatible":       receipt.RollbackCompatible,
		"keyed_digests":             receipt.KeyedDigests,
	}
	encoded, _ := json.MarshalIndent(output, "", "  ")
	fmt.Println(string(encoded))

	if *receiptOut != "" {
		if err := os.WriteFile(*receiptOut, append(encoded, '\n'), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "error: write receipt %s: %v\n", *receiptOut, err)
			os.Exit(1)
		}
	}
}
