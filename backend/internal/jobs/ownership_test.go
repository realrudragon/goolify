package jobs_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/jobs"
)

func TestJobCoordinator_EpochFencingAndConcurrencyLimit(t *testing.T) {
	coord := jobs.NewJobCoordinator(1)
	ctx := context.Background()

	if err := coord.AdmitOperation(ctx, jobs.JobOperation{
		JobID: "j1", OperationID: "op-1", ResourceUUID: "app-1", ServerUUID: "srv-1", TeamID: 10, MaxAttempts: 2,
	}); err != nil {
		t.Fatalf("AdmitOperation 1: %v", err)
	}
	lease, err := coord.AcquireLease(ctx, "op-1", "worker-1", 1, time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease: %v", err)
	}

	// Second active operation on same server hits concurrency limit 1
	if err := coord.AdmitOperation(ctx, jobs.JobOperation{
		JobID: "j2", OperationID: "op-2", ResourceUUID: "app-2", ServerUUID: "srv-1", TeamID: 10,
	}); !errors.Is(err, jobs.ErrAdmissionCapReached) {
		t.Fatalf("expected ErrAdmissionCapReached, got %v", err)
	}

	// Stale epoch cannot acquire or mark terminal
	if _, err := coord.AcquireLease(ctx, "op-1", "worker-2", lease.OwnershipEpoch, time.Minute); !errors.Is(err, jobs.ErrLeaseHeldByOtherEpoch) {
		t.Fatalf("expected ErrLeaseHeldByOtherEpoch, got %v", err)
	}
	if err := coord.MarkTerminal(ctx, "op-1", lease.OwnershipEpoch-1, jobs.StatusFailed, "stale"); !errors.Is(err, jobs.ErrLeaseHeldByOtherEpoch) {
		t.Fatalf("expected stale terminal update rejected, got %v", err)
	}

	if err := coord.MarkTerminal(ctx, "op-1", lease.OwnershipEpoch, jobs.StatusSucceeded, ""); err != nil {
		t.Fatalf("MarkTerminal: %v", err)
	}
	// Completed operation cannot be re-admitted
	if err := coord.AdmitOperation(ctx, jobs.JobOperation{
		JobID: "j1-dup", OperationID: "op-1", ResourceUUID: "app-1", ServerUUID: "srv-1", TeamID: 10,
	}); !errors.Is(err, jobs.ErrAlreadyCompleted) {
		t.Fatalf("expected ErrAlreadyCompleted, got %v", err)
	}
}
