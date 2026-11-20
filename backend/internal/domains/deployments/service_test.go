package deployments_test

import (
	"context"
	"errors"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/domains/deploymentcontext"
	"github.com/coollabsio/coolify/backend/internal/domains/deployments"
	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/coollabsio/coolify/backend/internal/realtime"
)

type stubAppRepo struct {
	src deploymentcontext.DeploymentSource
}

func (s *stubAppRepo) LoadApplicationSource(_ context.Context, _ string, _ *int64) (deploymentcontext.DeploymentSource, error) {
	return s.src, nil
}

type mockTransport struct {
	exitCode int
	dropEOF  bool
}

func (m *mockTransport) StreamNDJSON(_ context.Context, req executor.ExecutionRequest, emit func(executor.ExecutionEvent) error) (int, error) {
	_ = emit(executor.ExecutionEvent{ProtocolVersion: "v1", EventType: "started", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 1})
	if m.dropEOF {
		return -1, errors.New("worker killed before ack")
	}
	code := m.exitCode
	_ = emit(executor.ExecutionEvent{ProtocolVersion: "v1", EventType: "exited", OperationID: req.OperationID, AttemptID: req.AttemptID, Sequence: 2, ExitCode: &code})
	return code, nil
}

func baseContext() deploymentcontext.DeploymentSource {
	return deploymentcontext.DeploymentSource{
		ApplicationID: 1, ApplicationUUID: "app-nginx",
		EnvironmentID: 1, EnvironmentUUID: "env-prod",
		ProjectID: 1, ProjectUUID: "proj-main",
		ResourceTeamID: 10, ServerID: 1, ServerUUID: "srv-1",
		PrivateKeyUUID: "pk-10", PrivateKeyTeam: 10,
		BuildPack: "dockerimage",
		Settings:  map[string]string{"docker_registry_image_name": "nginx", "docker_registry_image_tag": "alpine"},
	}
}

func TestReconciler_UnknownAndRunningStatesBlockDuplicateDeploy(t *testing.T) {
	// Review Focus #3: worker dies mid-deploy -> active operation must be reconciled; running/unknown blocks duplicate deploy
	ctx := context.Background()
	probe := executor.NewMemoryRemoteProbe("boot-1")
	probe.ProcAlive = true
	tracker := executor.NewRemoteTracker(probe)
	reconciler := deployments.NewReconciler(tracker)

	resolver := deploymentcontext.NewResolver(&stubAppRepo{src: baseContext()}, access.NewAuthorizer())
	transport := &mockTransport{dropEOF: true}
	svc := deployments.NewDeploymentService(
		resolver,
		deployments.NewDeploymentPlanner(),
		jobs.NewJobCoordinator(2),
		executor.NewClient(transport),
		tracker,
		reconciler,
		realtime.NewOutboxStore(),
		nil,
	)

	admin := auth.Principal{UserID: 1, AuthMethod: "session", TeamRoles: map[int64]auth.Role{10: auth.RoleAdmin}}
	rec, err := svc.Deploy(ctx, admin, deployments.DeployRequest{
		ApplicationUUID: "app-nginx", OperationID: "op-100", TriggerSource: "manual",
	})
	if err == nil || rec.Status != "unknown" {
		t.Fatalf("expected unknown status when worker killed mid-exec, rec=%+v err=%v", rec, err)
	}

	// Second deploy while op-100 is still running on remote must be blocked
	_, err2 := svc.Deploy(ctx, admin, deployments.DeployRequest{
		ApplicationUUID: "app-nginx", OperationID: "op-101", TriggerSource: "manual",
	})
	if !errors.Is(err2, deployments.ErrDuplicateOperationBlocked) {
		t.Fatalf("expected ErrDuplicateOperationBlocked while remote still running, got %v", err2)
	}
}

func TestDeployment_ConcurrentWebhookAndManualFencing(t *testing.T) {
	// Review Focus #5 + failing build preserves healthy prior deployment + build pack coverage
	ctx := context.Background()
	probe := executor.NewMemoryRemoteProbe("boot-1")
	tracker := executor.NewRemoteTracker(probe)
	reconciler := deployments.NewReconciler(tracker)
	outbox := realtime.NewOutboxStore()
	cache := realtime.NewCacheCoherence()
	hub := realtime.NewRealtimeHub(8)
	disp := realtime.NewDispatcher(outbox, cache, hub)

	src := baseContext()
	resolver := deploymentcontext.NewResolver(&stubAppRepo{src: src}, access.NewAuthorizer())
	transport := &mockTransport{exitCode: 0}
	svc := deployments.NewDeploymentService(
		resolver,
		deployments.NewDeploymentPlanner(),
		jobs.NewJobCoordinator(1),
		executor.NewClient(transport),
		tracker,
		reconciler,
		outbox,
		disp,
	)

	admin := auth.Principal{UserID: 1, AuthMethod: "session", TeamRoles: map[int64]auth.Role{10: auth.RoleAdmin}}

	// Webhook delivery deduplication
	r1, err := svc.Deploy(ctx, admin, deployments.DeployRequest{
		ApplicationUUID: "app-nginx", OperationID: "op-wh-1", TriggerSource: "webhook", WebhookDelivery: "gh-deliv-999",
	})
	if err != nil || r1.Status != "finished" {
		t.Fatalf("initial webhook deploy failed: %+v err=%v", r1, err)
	}
	rDup, err := svc.Deploy(ctx, admin, deployments.DeployRequest{
		ApplicationUUID: "app-nginx", OperationID: "op-wh-2", TriggerSource: "webhook", WebhookDelivery: "gh-deliv-999",
	})
	if err != nil || rDup.Status != "deduplicated" || rDup.OperationID != "op-wh-1" {
		t.Fatalf("expected webhook delivery deduplication, got %+v err=%v", rDup, err)
	}

	// Failing build preserves prior healthy container
	transport.exitCode = 1
	rFail, err := svc.Deploy(ctx, admin, deployments.DeployRequest{
		ApplicationUUID: "app-nginx", OperationID: "op-fail-1", TriggerSource: "manual",
	})
	if !errors.Is(err, deployments.ErrPriorHealthyPreserved) || !rFail.PreservedPrior {
		t.Fatalf("expected ErrPriorHealthyPreserved with PreservedPrior=true, rec=%+v err=%v", rFail, err)
	}
}
