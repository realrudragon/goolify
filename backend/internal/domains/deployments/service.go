package deployments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/domains/deploymentcontext"
	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/coollabsio/coolify/backend/internal/realtime"
)

var (
	ErrDuplicateOperationBlocked = errors.New("concurrent or unresolved operation already active for deployment")
	ErrPriorHealthyPreserved     = errors.New("deployment failed; prior healthy container preserved")
)

type DeployRequest struct {
	ApplicationUUID string
	PreviewID       *int64
	OperationID     string
	TriggerSource   string // manual | webhook | api
	WebhookDelivery string // optional deduplication key
	ForceRebuild    bool
}

type DeploymentReceipt struct {
	DeploymentUUID string
	OperationID    string
	Status         string // queued | running | finished | failed | unknown
	InputRevision  string
	PreservedPrior bool
}

type DeploymentService struct {
	mu           sync.Mutex
	resolver     *deploymentcontext.Resolver
	planner      *DeploymentPlanner
	coord        *jobs.JobCoordinator
	exec         *executor.Client
	tracker      *executor.RemoteTracker
	reconciler   *Reconciler
	outbox       *realtime.OutboxStore
	disp         *realtime.Dispatcher
	activeByApp  map[string]string // appUUID -> operationID
	seenWebhooks map[string]string // deliveryKey -> operationID
}

func NewDeploymentService(
	resolver *deploymentcontext.Resolver,
	planner *DeploymentPlanner,
	coord *jobs.JobCoordinator,
	exec *executor.Client,
	tracker *executor.RemoteTracker,
	reconciler *Reconciler,
	outbox *realtime.OutboxStore,
	disp *realtime.Dispatcher,
) *DeploymentService {
	return &DeploymentService{
		resolver:     resolver,
		planner:      planner,
		coord:        coord,
		exec:         exec,
		tracker:      tracker,
		reconciler:   reconciler,
		outbox:       outbox,
		disp:         disp,
		activeByApp:  make(map[string]string),
		seenWebhooks: make(map[string]string),
	}
}

func (s *DeploymentService) Deploy(ctx context.Context, principal auth.Principal, req DeployRequest) (DeploymentReceipt, error) {
	dctx, err := s.resolver.Resolve(ctx, principal, req.ApplicationUUID, req.PreviewID)
	if err != nil {
		return DeploymentReceipt{}, err
	}

	s.mu.Lock()
	if req.WebhookDelivery != "" {
		if existingOp, ok := s.seenWebhooks[req.WebhookDelivery]; ok {
			s.mu.Unlock()
			return DeploymentReceipt{
				DeploymentUUID: "dep-" + existingOp,
				OperationID:    existingOp,
				Status:         "deduplicated",
				InputRevision:  dctx.InputRevision,
			}, nil
		}
	}
	if activeOp, busy := s.activeByApp[req.ApplicationUUID]; busy && activeOp != req.OperationID {
		s.mu.Unlock()
		rec, _ := s.reconciler.Reconcile(ctx, activeOp)
		if rec.Outcome == OutcomeRunning || rec.Outcome == OutcomeUnknown {
			return DeploymentReceipt{}, fmt.Errorf("%w: active=%s outcome=%s", ErrDuplicateOperationBlocked, activeOp, rec.Outcome)
		}
		s.mu.Lock()
	}
	s.activeByApp[req.ApplicationUUID] = req.OperationID
	if req.WebhookDelivery != "" {
		s.seenWebhooks[req.WebhookDelivery] = req.OperationID
	}
	s.mu.Unlock()

	plan, err := s.planner.BuildPlan(dctx, req.OperationID, "")
	if err != nil {
		return DeploymentReceipt{}, err
	}

	s.reconciler.RegisterOperationServer(req.OperationID, dctx.ServerUUID)
	if err := s.coord.AdmitOperation(ctx, jobs.JobOperation{
		JobID:        "job-" + req.OperationID,
		OperationID:  req.OperationID,
		AttemptID:    "att-1",
		ResourceUUID: dctx.ApplicationUUID,
		ServerUUID:   dctx.ServerUUID,
		TeamID:       dctx.ResourceTeamID,
	}); err != nil {
		return DeploymentReceipt{}, err
	}

	lease, err := s.coord.AcquireLease(ctx, req.OperationID, "worker-primary", 1, 5*time.Minute)
	if err != nil {
		return DeploymentReceipt{}, err
	}

	if _, err := s.tracker.EnsureOperationJournal(ctx, executor.OperationIntent{
		JobID:            "job-" + req.OperationID,
		OperationID:      req.OperationID,
		AttemptID:        "att-1",
		OwnershipEpoch:   lease.OwnershipEpoch,
		ResourceUUID:     dctx.ApplicationUUID,
		ServerUUID:       dctx.ServerUUID,
		InputFingerprint: dctx.InputRevision,
	}); err != nil {
		_ = s.coord.MarkTerminal(ctx, req.OperationID, lease.OwnershipEpoch, jobs.StatusUnknown, err.Error())
		return DeploymentReceipt{OperationID: req.OperationID, Status: "unknown", InputRevision: dctx.InputRevision}, err
	}

	res, runErr := s.exec.Run(ctx, executor.ExecutionRequest{
		ProtocolVersion:    executor.ProtocolVersionV1,
		FrameType:          executor.FrameStart,
		JobID:              "job-" + req.OperationID,
		OperationID:        req.OperationID,
		AttemptID:          "att-1",
		OwnershipEpoch:     lease.OwnershipEpoch,
		ResourceUUID:       dctx.ApplicationUUID,
		TeamID:             dctx.ResourceTeamID,
		DeadlineMS:         300000,
		MaxFrameBytes:      executor.DefaultMaxFrameBytes,
		ExpectedCapability: "ssh_exec",
		Operation: executor.OperationSpec{
			Kind:    "exec",
			Host:    dctx.ServerUUID,
			Port:    22,
			User:    "root",
			Command: strings.Join(plan.Commands, " && "),
		},
	}, nil)

	if runErr != nil || res.Ambiguous {
		_ = s.coord.MarkTerminal(ctx, req.OperationID, lease.OwnershipEpoch, jobs.StatusUnknown, "ambiguous remote state")
		return DeploymentReceipt{
			DeploymentUUID: "dep-" + req.OperationID,
			OperationID:    req.OperationID,
			Status:         "unknown",
			InputRevision:  dctx.InputRevision,
			PreservedPrior: plan.PreserveHealthy,
		}, runErr
	}

	if res.ExitCode != 0 {
		_ = s.coord.MarkTerminal(ctx, req.OperationID, lease.OwnershipEpoch, jobs.StatusFailed, "build/deploy failed")
		return DeploymentReceipt{
			DeploymentUUID: "dep-" + req.OperationID,
			OperationID:    req.OperationID,
			Status:         "failed",
			InputRevision:  dctx.InputRevision,
			PreservedPrior: plan.PreserveHealthy,
		}, ErrPriorHealthyPreserved
	}

	_ = s.coord.MarkTerminal(ctx, req.OperationID, lease.OwnershipEpoch, jobs.StatusSucceeded, "")
	s.mu.Lock()
	delete(s.activeByApp, req.ApplicationUUID)
	s.mu.Unlock()

	if s.outbox != nil {
		_ = s.outbox.AppendInTx(ctx, true, realtime.DomainEvent{
			EventID: "dep-done-" + req.OperationID, SchemaVersion: "v1", Producer: "go-worker",
			TeamID: dctx.ResourceTeamID, ResourceType: "application", ResourceUUID: dctx.ApplicationUUID,
			AggregateRevision: 1, OwnershipEpoch: lease.OwnershipEpoch, MutationType: "application.status_changed",
			CommittedAt: time.Now().UTC(), Payload: map[string]any{"status": "running", "operation_id": req.OperationID},
		})
		if s.disp != nil {
			_ = s.disp.FlushPending(ctx)
		}
	}

	return DeploymentReceipt{
		DeploymentUUID: "dep-" + req.OperationID,
		OperationID:    req.OperationID,
		Status:         "finished",
		InputRevision:  dctx.InputRevision,
		PreservedPrior: true,
	}, nil
}
