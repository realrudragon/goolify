package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/executor"
)

var (
	ErrActiveRemoteJobsMustDrainOrJournal = errors.New("in-flight operations must have durable remote journals before cutover/rollback")
	ErrNonExpandSchemaBlocksRollback      = errors.New("destructive/non-expand schema change blocks rollback to frozen Laravel baseline")
)

type CutoverReceipt struct {
	ValidatedAt             time.Time
	TotalRows               int
	SentinelZeroTables      int
	BrokenFKCount           int
	KeyedDigests            map[string]string
	PreservedRemoteJournals int
	RollbackCompatible      bool
}

type SchemaChange struct {
	Name       string
	ExpandOnly bool // true = additive column/table safe for frozen Laravel baseline
}

type CutoverRunner struct {
	reader        compat.DatasetReader
	digestKey     []byte
	journals      []executor.RemoteJournal
	schemaChanges []SchemaChange
}

func NewCutoverRunner(reader compat.DatasetReader, digestKey []byte, journals []executor.RemoteJournal, changes []SchemaChange) *CutoverRunner {
	return &CutoverRunner{
		reader:        reader,
		digestKey:     digestKey,
		journals:      journals,
		schemaChanges: changes,
	}
}

func (r *CutoverRunner) Preflight(ctx context.Context) (CutoverReceipt, error) {
	rep, err := compat.ValidateDataset(ctx, r.reader, r.digestKey)
	if err != nil {
		return CutoverReceipt{}, err
	}
	if len(rep.BrokenFKs) > 0 {
		return CutoverReceipt{}, fmt.Errorf("preflight detected broken FKs: %v", rep.BrokenFKs)
	}
	preserved := 0
	for _, j := range r.journals {
		if j.State == executor.RemoteRunning || j.State == executor.RemoteUnknown || j.State == executor.RemoteStarted {
			if j.OperationID == "" {
				return CutoverReceipt{}, ErrActiveRemoteJobsMustDrainOrJournal
			}
			preserved++
		}
	}
	rollbackOK := r.VerifyRollbackCompatibility(ctx) == nil
	return CutoverReceipt{
		ValidatedAt:             time.Now().UTC(),
		TotalRows:               rep.TotalRows,
		SentinelZeroTables:      len(rep.SentinelZeroIDs),
		BrokenFKCount:           len(rep.BrokenFKs),
		KeyedDigests:            rep.KeyedDigests,
		PreservedRemoteJournals: preserved,
		RollbackCompatible:      rollbackOK,
	}, nil
}

func (r *CutoverRunner) VerifyRollbackCompatibility(_ context.Context) error {
	for _, ch := range r.schemaChanges {
		if !ch.ExpandOnly {
			return fmt.Errorf("%w: %s", ErrNonExpandSchemaBlocksRollback, ch.Name)
		}
	}
	for _, j := range r.journals {
		if (j.State == executor.RemoteRunning || j.State == executor.RemoteUnknown) && j.OperationID == "" {
			return ErrActiveRemoteJobsMustDrainOrJournal
		}
	}
	return nil
}
