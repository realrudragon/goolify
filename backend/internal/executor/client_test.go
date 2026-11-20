package executor_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/coollabsio/coolify/backend/internal/jobs"
)

type stubTransport struct {
	events []executor.ExecutionEvent
	err    error
}

func (s *stubTransport) StreamNDJSON(_ context.Context, _ executor.ExecutionRequest, emit func(executor.ExecutionEvent) error) (int, error) {
	for _, ev := range s.events {
		if err := emit(ev); err != nil {
			return 1, err
		}
	}
	if s.err != nil {
		return 1, s.err
	}
	return 0, nil
}

func TestExecutor_ProtocolValidationAndSecretRedaction(t *testing.T) {
	client := executor.NewClient(&stubTransport{
		events: []executor.ExecutionEvent{
			{ProtocolVersion: "v1", EventType: "started", OperationID: "op-1", AttemptID: "att-1", Sequence: 1},
			{ProtocolVersion: "v1", EventType: "output", OperationID: "op-1", AttemptID: "att-1", Sequence: 2, Data: "deploying with token super-secret-xyz"},
			{ProtocolVersion: "v1", EventType: "exited", OperationID: "op-1", AttemptID: "att-1", Sequence: 3, ExitCode: intPtr(0)},
		},
	}, "super-secret-xyz")

	// Reject unknown protocol version
	badReq := executor.ExecutionRequest{
		ProtocolVersion: "v99", OperationID: "op-1", AttemptID: "att-1", MaxFrameBytes: 4096,
	}
	if _, err := client.Run(context.Background(), badReq, nil); !errors.Is(err, executor.ErrUnsupportedProtocol) {
		t.Fatalf("expected ErrUnsupportedProtocol, got %v", err)
	}

	// Reject private key in CLI command string
	secretCmdReq := executor.ExecutionRequest{
		ProtocolVersion: "v1", OperationID: "op-1", AttemptID: "att-1", MaxFrameBytes: 4096,
		Operation: executor.OperationSpec{Command: "echo '-----BEGIN OPENSSH PRIVATE KEY-----'"},
	}
	if _, err := client.Run(context.Background(), secretCmdReq, nil); !errors.Is(err, executor.ErrSecretInCLIArgs) {
		t.Fatalf("expected ErrSecretInCLIArgs, got %v", err)
	}

	// Valid run redacts secret from emitted logs
	var collected []executor.ExecutionEvent
	res, err := client.Run(context.Background(), executor.ExecutionRequest{
		ProtocolVersion: "v1", OperationID: "op-1", AttemptID: "att-1", MaxFrameBytes: 4096,
		Operation: executor.OperationSpec{Kind: "exec", Host: "10.0.0.5", Port: 22, User: "root", Command: "docker compose up -d"},
	}, func(ev executor.ExecutionEvent) error {
		collected = append(collected, ev)
		return nil
	})
	if err != nil || res.Ambiguous || res.ExitCode != 0 {
		t.Fatalf("expected clean exit, res=%+v err=%v", res, err)
	}
	if collected[1].Data != "deploying with token [REDACTED]" {
		t.Fatalf("secret not redacted: %q", collected[1].Data)
	}
}

func TestExecutor_WorkerCrashAndRemoteJournalRecovery(t *testing.T) {
	// Review Focus #3:
	// Worker crashes / NDJSON EOF happens after remote command started.
	// Fresh worker must NOT treat EOF as remote failure/cancellation and must reconcile using remote journal + boot ID.
	client := executor.NewClient(&stubTransport{
		events: []executor.ExecutionEvent{
			{ProtocolVersion: "v1", EventType: "started", OperationID: "op-deploy-1", AttemptID: "att-1", Sequence: 1},
		},
		err: errors.New("unexpected NDJSON EOF: local worker killed"),
	})

	ctx := context.Background()
	coord := jobs.NewJobCoordinator(2)
	_ = coord.AdmitOperation(ctx, jobs.JobOperation{
		JobID: "job-1", OperationID: "op-deploy-1", AttemptID: "att-1",
		ResourceUUID: "app-1", ServerUUID: "srv-1", TeamID: 10,
	})
	lease1, err := coord.AcquireLease(ctx, "op-deploy-1", "worker-1", 1, time.Minute)
	if err != nil {
		t.Fatalf("AcquireLease worker-1: %v", err)
	}

	probe := executor.NewMemoryRemoteProbe("boot-aaa")
	probe.ProcAlive = true
	tracker := executor.NewRemoteTracker(probe)
	_, err = tracker.EnsureOperationJournal(ctx, executor.OperationIntent{
		JobID: "job-1", OperationID: "op-deploy-1", AttemptID: "att-1",
		OwnershipEpoch: lease1.OwnershipEpoch, ResourceUUID: "app-1", ServerUUID: "srv-1",
		InputFingerprint: executor.ComputeInputFingerprint("rev-1"),
	})
	if err != nil {
		t.Fatalf("EnsureOperationJournal: %v", err)
	}

	res, _ := client.Run(ctx, executor.ExecutionRequest{
		ProtocolVersion: "v1", OperationID: "op-deploy-1", AttemptID: "att-1", MaxFrameBytes: 4096,
	}, nil)
	if !res.Ambiguous {
		t.Fatal("expected ambiguous result when local SSH/NDJSON drops without exited frame")
	}

	// Reconcile on fresh worker sees remote process still running with same boot ID
	rec, err := tracker.ReconcileRemote(ctx, "srv-1", "op-deploy-1")
	if err != nil || rec.State != executor.RemoteRunning {
		t.Fatalf("expected RemoteRunning on reconcile while process alive, got %+v err=%v", rec, err)
	}

	// Simulate remote reboot (PID reuse hazard) without running container -> must yield RemoteUnknown
	probe.BootID = "boot-bbb"
	probe.ProcAlive = true // potentially reused PID on new boot!
	probe.ContRunning = false
	recAfterReboot, _ := tracker.ReconcileRemote(ctx, "srv-1", "op-deploy-1")
	if recAfterReboot.State != executor.RemoteUnknown {
		t.Fatalf("expected RemoteUnknown after boot ID change (PID reuse guard), got %s", recAfterReboot.State)
	}

	// Corrupt journal must also yield RemoteUnknown
	j := probe.Journals["op-deploy-1"]
	j.Corrupt = true
	probe.Journals["op-deploy-1"] = j
	recCorrupt, _ := tracker.ReconcileRemote(ctx, "srv-1", "op-deploy-1")
	if recCorrupt.State != executor.RemoteUnknown {
		t.Fatalf("expected RemoteUnknown for corrupt journal, got %s", recCorrupt.State)
	}
}

func intPtr(v int) *int { return &v }

func TestGoSubprocessTransport_RealRustExecutorBinary(t *testing.T) {
	bin := os.Getenv("COOLIFY_EXECUTOR_BIN")
	if bin == "" {
		t.Skip("COOLIFY_EXECUTOR_BIN not set; run inside Linux container with compiled coolify-executor")
	}
	client := executor.NewClient(&executor.SubprocessTransport{BinaryPath: bin})

	// 1. Live streaming: first line MUST arrive before second line after sleep!
	var firstAt, secondAt time.Time
	res, err := client.Run(context.Background(), executor.ExecutionRequest{
		ProtocolVersion:    executor.ProtocolVersionV1,
		FrameType:          executor.FrameStart,
		JobID:              "job-stream",
		OperationID:        "op-stream",
		AttemptID:          "att-1",
		OwnershipEpoch:     1,
		ResourceUUID:       "srv-0",
		TeamID:             0,
		DeadlineMS:         5000,
		MaxFrameBytes:      executor.DefaultMaxFrameBytes,
		ExpectedCapability: "local_exec",
		Operation: executor.OperationSpec{
			Kind:    "exec",
			Host:    "localhost",
			Port:    22,
			User:    "root",
			Command: "echo STREAM_ONE; sleep 0.4; echo STREAM_TWO",
		},
	}, func(ev executor.ExecutionEvent) error {
		if ev.EventType == "output" {
			if strings.Contains(ev.Data, "STREAM_ONE") && firstAt.IsZero() {
				firstAt = time.Now()
			}
			if strings.Contains(ev.Data, "STREAM_TWO") && secondAt.IsZero() {
				secondAt = time.Now()
			}
		}
		return nil
	})
	if err != nil || res.Ambiguous || res.ExitCode != 0 {
		t.Fatalf("streaming run failed: res=%+v err=%v", res, err)
	}
	if firstAt.IsZero() || secondAt.IsZero() || secondAt.Sub(firstAt) < 200*time.Millisecond {
		t.Fatalf("expected immediate line streaming (>200ms separation), got delta=%v", secondAt.Sub(firstAt))
	}

	// 2. Whole-lifecycle deadline_ms timeout MUST kill hung child and emit exit 124
	startTimeout := time.Now()
	timeoutRes, _ := client.Run(context.Background(), executor.ExecutionRequest{
		ProtocolVersion:    executor.ProtocolVersionV1,
		FrameType:          executor.FrameStart,
		JobID:              "job-timeout",
		OperationID:        "op-timeout",
		AttemptID:          "att-1",
		OwnershipEpoch:     1,
		ResourceUUID:       "srv-0",
		TeamID:             0,
		DeadlineMS:         300,
		MaxFrameBytes:      executor.DefaultMaxFrameBytes,
		ExpectedCapability: "local_exec",
		Operation: executor.OperationSpec{
			Kind:    "exec",
			Host:    "localhost",
			Port:    22,
			User:    "root",
			Command: "echo BEFORE_HANG; sleep 10",
		},
	}, nil)
	if time.Since(startTimeout) > 3*time.Second || timeoutRes.ExitCode != 124 {
		t.Fatalf("expected deadline timeout exit=124 in <3s, got exit=%d elapsed=%v", timeoutRes.ExitCode, time.Since(startTimeout))
	}

	// 3. Context cancel MUST send Cancel frame to Rust executor and terminate quickly
	cancelCtx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(250 * time.Millisecond)
		cancel()
	}()
	startCancel := time.Now()
	_, _ = client.Run(cancelCtx, executor.ExecutionRequest{
		ProtocolVersion:    executor.ProtocolVersionV1,
		FrameType:          executor.FrameStart,
		JobID:              "job-cancel",
		OperationID:        "op-cancel",
		AttemptID:          "att-1",
		OwnershipEpoch:     1,
		ResourceUUID:       "srv-0",
		TeamID:             0,
		DeadlineMS:         10000,
		MaxFrameBytes:      executor.DefaultMaxFrameBytes,
		ExpectedCapability: "local_exec",
		Operation: executor.OperationSpec{
			Kind:    "exec",
			Host:    "localhost",
			Port:    22,
			User:    "root",
			Command: "sleep 10",
		},
	}, nil)
	if time.Since(startCancel) > 3*time.Second {
		t.Fatalf("context cancel took too long: %v", time.Since(startCancel))
	}

	// 4. Non-zero exit code propagation
	nzRes, _ := client.Run(context.Background(), executor.ExecutionRequest{
		ProtocolVersion:    executor.ProtocolVersionV1,
		FrameType:          executor.FrameStart,
		JobID:              "job-exit42",
		OperationID:        "op-exit42",
		AttemptID:          "att-1",
		OwnershipEpoch:     1,
		ResourceUUID:       "srv-0",
		TeamID:             0,
		DeadlineMS:         5000,
		MaxFrameBytes:      executor.DefaultMaxFrameBytes,
		ExpectedCapability: "local_exec",
		Operation: executor.OperationSpec{
			Kind:    "exec",
			Host:    "localhost",
			Port:    22,
			User:    "root",
			Command: "exit 42",
		},
	}, nil)
	if nzRes.Ambiguous || nzRes.ExitCode != 42 {
		t.Fatalf("expected exit 42, got res=%+v", nzRes)
	}
}
