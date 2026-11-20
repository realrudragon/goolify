package compat

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"
)

// ValidationRecord represents one exported/scanned database row for dry-run parity checks.
type ValidationRecord struct {
	Table       string
	ID          int64
	UUID        string
	ParentUUID  string
	SecretValue *string // nil = SQL NULL, *"" = empty string, *"  x " = whitespace-preserved
	UpdatedAt   time.Time
}

type ValidationReport struct {
	TotalRows        int
	SentinelZeroIDs  map[string]bool
	NullSecretCount  int
	EmptySecretCount int
	BrokenFKs        []string
	KeyedDigests     map[string]string // Non-secret HMAC digests for cross-instance parity verification
}

// DatasetReader abstracts cloned DB or fixture datasets without mutating production rows.
type DatasetReader interface {
	ListTablesInOrder(ctx context.Context) ([]string, error)
	FetchPageStartingFromZero(ctx context.Context, table string, afterID *int64, limit int) ([]ValidationRecord, error)
	ParentExists(ctx context.Context, parentUUID string) (bool, error)
}

// ValidateDataset iterates from id >= 0 (never skipping sentinel id = 0!), checks FK integrity,
// distinguishes SQL NULL from empty string/whitespace, and computes keyed digests without exposing secrets.
func ValidateDataset(ctx context.Context, reader DatasetReader, digestKey []byte) (ValidationReport, error) {
	tables, err := reader.ListTablesInOrder(ctx)
	if err != nil {
		return ValidationReport{}, err
	}
	report := ValidationReport{
		SentinelZeroIDs: make(map[string]bool),
		KeyedDigests:    make(map[string]string),
	}
	sort.Strings(tables)

	for _, tbl := range tables {
		var cursor *int64 // nil means start at id >= 0 so sentinel id = 0 is included on page 1
		mac := hmac.New(sha256.New, digestKey)
		for {
			rows, err := reader.FetchPageStartingFromZero(ctx, tbl, cursor, 100)
			if err != nil {
				return ValidationReport{}, err
			}
			if len(rows) == 0 {
				break
			}
			for _, r := range rows {
				report.TotalRows++
				if r.ID == 0 {
					report.SentinelZeroIDs[tbl] = true
				}
				if r.ParentUUID != "" {
					ok, err := reader.ParentExists(ctx, r.ParentUUID)
					if err != nil {
						return ValidationReport{}, err
					}
					if !ok {
						report.BrokenFKs = append(report.BrokenFKs, fmt.Sprintf("%s:%d->%s", tbl, r.ID, r.ParentUUID))
					}
				}
				secretTag := "NULL"
				if r.SecretValue == nil {
					report.NullSecretCount++
				} else if *r.SecretValue == "" {
					report.EmptySecretCount++
					secretTag = "EMPTY"
				} else {
					secretTag = fmt.Sprintf("LEN:%d:DATA:%s", len(*r.SecretValue), *r.SecretValue)
				}
				fmt.Fprintf(mac, "%s|%d|%s|%s|%s|", tbl, r.ID, r.UUID, secretTag, r.UpdatedAt.UTC().Format(time.RFC3339Nano))
				lastID := r.ID
				cursor = &lastID
			}
		}
		report.KeyedDigests[tbl] = hex.EncodeToString(mac.Sum(nil))
	}
	return report, nil
}
