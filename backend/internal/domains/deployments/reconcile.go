package deployments

import (
	"context"
	"fmt"

	"github.com/coollabsio/coolify/backend/internal/executor"
)

type ReconcileOutcome string

const (
	OutcomeRunning   ReconcileOutcome = "running"
	OutcomeSucceeded ReconcileOutcome = "succeeded"
	OutcomeFailed    ReconcileOutcome = "failed"
	OutcomeUnknown   ReconcileOutcome = "unknown"
)

type ReconcileResult struct {
	OperationID  string           `json:"operation_id"`
	Outcome      ReconcileOutcome `json:"outcome"`
	CanRetry     bool             `json:"can_retry"`
	EvidenceRefs []string         `json:"evidence_refs"`
}

type Reconciler struct {
	tracker *executor.RemoteTracker
	servers map[string]string // operationID -> serverUUID lookup
}

func NewReconciler(tracker *executor.RemoteTracker) *Reconciler {
	return &Reconciler{
		tracker: tracker,
		servers: make(map[string]string),
	}
}

func (r *Reconciler) RegisterOperationServer(operationID string, serverUUID string) {
	r.servers[operationID] = serverUUID
}

// Reconcile inspects the remote journal + host boot ID + container labels before allowing any retry.
// Never allows duplicate admission when remote outcome is running or unknown.
func (r *Reconciler) Reconcile(ctx context.Context, operationID string) (ReconcileResult, error) {
	serverUUID := r.servers[operationID]
	if serverUUID == "" {
		return ReconcileResult{OperationID: operationID, Outcome: OutcomeUnknown, CanRetry: false}, fmt.Errorf("missing server mapping for operation %s", operationID)
	}
	j, err := r.tracker.ReconcileRemote(ctx, serverUUID, operationID)
	ev := []string{
		fmt.Sprintf("journal_boot:%s", j.BootID),
		fmt.Sprintf("journal_state:%s", j.State),
	}
	if len(j.ContainerIDs) > 0 {
		ev = append(ev, j.ContainerIDs...)
	}

	switch j.State {
	case executor.RemoteRunning, executor.RemoteStarted:
		return ReconcileResult{OperationID: operationID, Outcome: OutcomeRunning, CanRetry: false, EvidenceRefs: ev}, err
	case executor.RemoteSucceeded:
		return ReconcileResult{OperationID: operationID, Outcome: OutcomeSucceeded, CanRetry: false, EvidenceRefs: ev}, nil
	case executor.RemoteFailed, executor.RemoteCancelled:
		return ReconcileResult{OperationID: operationID, Outcome: OutcomeFailed, CanRetry: true, EvidenceRefs: ev}, err
	default:
		return ReconcileResult{OperationID: operationID, Outcome: OutcomeUnknown, CanRetry: false, EvidenceRefs: ev}, err
	}
}
