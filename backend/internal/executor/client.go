package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

const ProtocolVersionV1 = "v1"
const DefaultMaxFrameBytes = 256 * 1024

var (
	ErrUnsupportedProtocol = errors.New("unsupported execution protocol version")
	ErrFrameTooLarge       = errors.New("ndjson frame exceeds max_frame_bytes")
	ErrSecretInCLIArgs     = errors.New("refusing to pass private key or secret material in CLI args")
	ErrInvalidOpaqueID     = errors.New("invalid opaque path identifier")
)

type FrameType string

const (
	FrameStart  FrameType = "start"
	FrameStdin  FrameType = "stdin"
	FrameResize FrameType = "resize"
	FrameCancel FrameType = "cancel"
)

type OperationSpec struct {
	Kind          string `json:"kind"` // exec | pty | journal_probe | reconcile
	Host          string `json:"host"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	PrivateKeyFD  int    `json:"private_key_fd,omitempty"`
	SSHKeyContent string `json:"ssh_key_content,omitempty"` // Passed strictly via NDJSON stdin, never CLI args
	UseSudo       bool   `json:"use_sudo"`
	Command       string `json:"command"`
	PTYCols       int    `json:"pty_cols,omitempty"`
	PTYRows       int    `json:"pty_rows,omitempty"`
}

type ExecutionRequest struct {
	ProtocolVersion    string        `json:"protocol_version"`
	FrameType          FrameType     `json:"frame_type"`
	JobID              string        `json:"job_id"`
	OperationID        string        `json:"operation_id"`
	AttemptID          string        `json:"attempt_id"`
	OwnershipEpoch     int64         `json:"ownership_epoch"`
	ResourceUUID       string        `json:"resource_uuid"`
	TeamID             int64         `json:"team_id"`
	DeadlineMS         int64         `json:"deadline_ms"`
	MaxFrameBytes      int           `json:"max_frame_bytes"`
	ExpectedCapability string        `json:"expected_capability"`
	Operation          OperationSpec `json:"operation"`
	StdinData          string        `json:"stdin_data,omitempty"`
}

type ExecutionEvent struct {
	ProtocolVersion string `json:"protocol_version"`
	EventType       string `json:"event_type"` // started | output | exited | error
	OperationID     string `json:"operation_id"`
	AttemptID       string `json:"attempt_id"`
	Sequence        int64  `json:"sequence"`
	Stream          string `json:"stream,omitempty"`
	Encoding        string `json:"encoding,omitempty"`
	Data            string `json:"data,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
	Message         string `json:"message,omitempty"`
}

type ExecutionResult struct {
	OperationID string
	AttemptID   string
	ExitCode    int
	Ambiguous   bool // True if pipe dropped/killed without explicit remote exit confirmation
	OutputTail  string
}

type Transport interface {
	StreamNDJSON(ctx context.Context, req ExecutionRequest, emit func(ExecutionEvent) error) (int, error)
}

type Client struct {
	transport Transport
	secrets   []string
}

func NewClient(transport Transport, secretValues ...string) *Client {
	return &Client{transport: transport, secrets: secretValues}
}

var opaqueIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ValidateRequest enforces protocol version, buffer caps, opaque IDs, and zero secret arguments.
func ValidateRequest(req ExecutionRequest) error {
	if req.ProtocolVersion != ProtocolVersionV1 {
		return fmt.Errorf("%w: %q", ErrUnsupportedProtocol, req.ProtocolVersion)
	}
	if !opaqueIDPattern.MatchString(req.OperationID) || !opaqueIDPattern.MatchString(req.AttemptID) {
		return ErrInvalidOpaqueID
	}
	if req.MaxFrameBytes <= 0 || req.MaxFrameBytes > 1048576 {
		return ErrFrameTooLarge
	}
	if strings.Contains(req.Operation.Command, "BEGIN OPENSSH PRIVATE KEY") || strings.Contains(req.Operation.Command, "BEGIN RSA PRIVATE KEY") {
		return ErrSecretInCLIArgs
	}
	return nil
}

// Run invokes the Rust executor NDJSON transport and returns whether completion was confirmed or ambiguous.
func (c *Client) Run(ctx context.Context, req ExecutionRequest, emit func(ExecutionEvent) error) (ExecutionResult, error) {
	if req.MaxFrameBytes == 0 {
		req.MaxFrameBytes = DefaultMaxFrameBytes
	}
	if err := ValidateRequest(req); err != nil {
		return ExecutionResult{OperationID: req.OperationID, AttemptID: req.AttemptID, Ambiguous: true}, err
	}
	var tail strings.Builder
	var gotExited bool
	var confirmedExit int

	exitCode, err := c.transport.StreamNDJSON(ctx, req, func(ev ExecutionEvent) error {
		if ev.ProtocolVersion != ProtocolVersionV1 {
			return fmt.Errorf("%w: %q", ErrUnsupportedProtocol, ev.ProtocolVersion)
		}
		ev.Data = c.Redact(ev.Data)
		ev.Message = c.Redact(ev.Message)
		if ev.EventType == "output" && ev.Data != "" {
			tail.WriteString(ev.Data)
		}
		if ev.EventType == "exited" && ev.ExitCode != nil {
			gotExited = true
			confirmedExit = *ev.ExitCode
		}
		if emit != nil {
			return emit(ev)
		}
		return nil
	})
	if err != nil || !gotExited {
		// Local pipe EOF or process kill never implies remote process stopped!
		return ExecutionResult{
			OperationID: req.OperationID,
			AttemptID:   req.AttemptID,
			ExitCode:    exitCode,
			Ambiguous:   true,
			OutputTail:  tail.String(),
		}, err
	}
	return ExecutionResult{
		OperationID: req.OperationID,
		AttemptID:   req.AttemptID,
		ExitCode:    confirmedExit,
		Ambiguous:   false,
		OutputTail:  tail.String(),
	}, nil
}

func (c *Client) Redact(input string) string {
	out := input
	for _, s := range c.secrets {
		if s != "" {
			out = strings.ReplaceAll(out, s, "[REDACTED]")
		}
	}
	return out
}

// SubprocessTransport spawns the Rust executor binary over stdin/stdout NDJSON pipes.
type SubprocessTransport struct {
	BinaryPath string
}

func (s *SubprocessTransport) StreamNDJSON(ctx context.Context, req ExecutionRequest, emit func(ExecutionEvent) error) (int, error) {
	cmd := exec.CommandContext(ctx, s.BinaryPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return -1, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf

	if err := cmd.Start(); err != nil {
		return -1, err
	}

	line, err := json.Marshal(req)
	if err != nil {
		_ = cmd.Process.Kill()
		return -1, err
	}
	if _, err := io.WriteString(stdin, string(line)+"\n"); err != nil {
		_ = cmd.Process.Kill()
		return -1, err
	}

	// Keep stdin open while operation runs so Rust does NOT interpret premature EOF as worker crash!
	// If ctx is cancelled, send a Cancel control frame and then close stdin so Rust reaps the child.
	doneCh := make(chan struct{})
	var closeStdinOnce sync.Once
	closeStdin := func() {
		closeStdinOnce.Do(func() { _ = stdin.Close() })
	}
	defer func() {
		close(doneCh)
		closeStdin()
	}()
	go func() {
		select {
		case <-ctx.Done():
			cancelReq := req
			cancelReq.FrameType = FrameCancel
			if cancelLine, mErr := json.Marshal(cancelReq); mErr == nil {
				_, _ = io.WriteString(stdin, string(cancelLine)+"\n")
			}
			closeStdin()
		case <-doneCh:
		}
	}()

	scanner := bufio.NewScanner(stdout)
	buf := make([]byte, 4096)
	maxBytes := req.MaxFrameBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxFrameBytes
	}
	scanner.Buffer(buf, maxBytes)

	for scanner.Scan() {
		raw := scanner.Bytes()
		if len(raw) > maxBytes {
			_ = cmd.Process.Kill()
			return -1, ErrFrameTooLarge
		}
		var ev ExecutionEvent
		if err := json.Unmarshal(raw, &ev); err != nil {
			_ = cmd.Process.Kill()
			return -1, err
		}
		if err := emit(ev); err != nil {
			_ = cmd.Process.Kill()
			return -1, err
		}
		if ev.EventType == "exited" {
			// Operation confirmed exited; close stdin so executor exits cleanly.
			closeStdin()
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		_ = cmd.Process.Kill()
		return -1, scanErr
	}
	if waitErr := cmd.Wait(); waitErr != nil {
		return 1, fmt.Errorf("executor exited: %w (stderr=%s)", waitErr, strings.TrimSpace(stderrBuf.String()))
	}
	return 0, nil
}

// ---- Remote Journal & Tracking (Review Focus #3) ----

type RemoteState string

const (
	RemoteStarted   RemoteState = "started"
	RemoteRunning   RemoteState = "running"
	RemoteSucceeded RemoteState = "succeeded"
	RemoteFailed    RemoteState = "failed"
	RemoteCancelled RemoteState = "cancelled"
	RemoteUnknown   RemoteState = "unknown"
)

type OperationIntent struct {
	JobID            string
	OperationID      string
	AttemptID        string
	OwnershipEpoch   int64
	ResourceUUID     string
	ServerUUID       string
	InputFingerprint string
	ReplaySafe       bool
}

type RemoteJournal struct {
	ProtocolVersion  string      `json:"protocol_version"`
	JobID            string      `json:"job_id"`
	OperationID      string      `json:"operation_id"`
	AttemptID        string      `json:"attempt_id"`
	OwnershipEpoch   int64       `json:"ownership_epoch"`
	ResourceUUID     string      `json:"resource_uuid"`
	InputFingerprint string      `json:"input_fingerprint"`
	State            RemoteState `json:"state"`
	BootID           string      `json:"boot_id"`
	PID              int         `json:"pid,omitempty"`
	ProcessStartTime string      `json:"process_start_time,omitempty"`
	ContainerIDs     []string    `json:"container_ids,omitempty"`
	ExitCode         *int        `json:"exit_code,omitempty"`
	Corrupt          bool        `json:"-"`
	UpdatedAt        time.Time   `json:"updated_at"`
}

type RemoteHostProbe interface {
	FetchJournal(ctx context.Context, serverUUID string, operationID string) (RemoteJournal, bool, error)
	WriteJournalAtomic(ctx context.Context, serverUUID string, j RemoteJournal) error
	InspectHostBootAndProcess(ctx context.Context, serverUUID string, bootID string, pid int, operationID string) (currentBootID string, processAlive bool, containerRunning bool, err error)
}

type RemoteTracker struct {
	probe RemoteHostProbe
}

func NewRemoteTracker(probe RemoteHostProbe) *RemoteTracker {
	return &RemoteTracker{probe: probe}
}

// RemoteJournalPath confines journal paths to validated opaque IDs under Coolify's private directory.
func RemoteJournalPath(operationID string) (string, error) {
	if !opaqueIDPattern.MatchString(operationID) {
		return "", ErrInvalidOpaqueID
	}
	return "/data/coolify/operations/" + operationID + "/journal.json", nil
}

func ComputeInputFingerprint(inputs ...string) string {
	h := sha256.New()
	for _, in := range inputs {
		h.Write([]byte(in))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// EnsureOperationJournal checks existing remote state before permitting a side effect.
// Missing, corrupt, or boot-mismatched journals return RemoteUnknown rather than allowing blind retry.
func (t *RemoteTracker) EnsureOperationJournal(ctx context.Context, op OperationIntent) (RemoteJournal, error) {
	if _, err := RemoteJournalPath(op.OperationID); err != nil {
		return RemoteJournal{}, err
	}
	existing, found, err := t.probe.FetchJournal(ctx, op.ServerUUID, op.OperationID)
	if err != nil {
		return RemoteJournal{OperationID: op.OperationID, State: RemoteUnknown}, err
	}
	if found {
		if existing.Corrupt {
			existing.State = RemoteUnknown
			return existing, nil
		}
		if existing.OwnershipEpoch > op.OwnershipEpoch {
			return existing, errors.New("stale ownership epoch rejected by remote journal")
		}
		return existing, nil
	}

	bootID, _, _, err := t.probe.InspectHostBootAndProcess(ctx, op.ServerUUID, "", 0, op.OperationID)
	if err != nil {
		return RemoteJournal{OperationID: op.OperationID, State: RemoteUnknown}, err
	}
	j := RemoteJournal{
		ProtocolVersion:  ProtocolVersionV1,
		JobID:            op.JobID,
		OperationID:      op.OperationID,
		AttemptID:        op.AttemptID,
		OwnershipEpoch:   op.OwnershipEpoch,
		ResourceUUID:     op.ResourceUUID,
		InputFingerprint: op.InputFingerprint,
		State:            RemoteStarted,
		BootID:           bootID,
		UpdatedAt:        time.Now().UTC(),
	}
	if err := t.probe.WriteJournalAtomic(ctx, op.ServerUUID, j); err != nil {
		return RemoteJournal{OperationID: op.OperationID, State: RemoteUnknown}, err
	}
	return j, nil
}

// ReconcileRemote verifies boot ID + process liveness + container labels rather than trusting a single PID/file.
func (t *RemoteTracker) ReconcileRemote(ctx context.Context, serverUUID string, operationID string) (RemoteJournal, error) {
	j, found, err := t.probe.FetchJournal(ctx, serverUUID, operationID)
	if err != nil || !found || j.Corrupt {
		return RemoteJournal{OperationID: operationID, State: RemoteUnknown}, err
	}
	if j.State == RemoteSucceeded || j.State == RemoteFailed || j.State == RemoteCancelled {
		return j, nil
	}
	curBoot, procAlive, containerAlive, err := t.probe.InspectHostBootAndProcess(ctx, serverUUID, j.BootID, j.PID, operationID)
	if err != nil {
		j.State = RemoteUnknown
		return j, err
	}
	if curBoot != j.BootID {
		// Remote host rebooted -> PID reuse protection: treat as unknown until postconditions confirm!
		if containerAlive {
			j.State = RemoteSucceeded
			return j, nil
		}
		j.State = RemoteUnknown
		return j, nil
	}
	if procAlive {
		j.State = RemoteRunning
		return j, nil
	}
	if containerAlive {
		j.State = RemoteSucceeded
		return j, nil
	}
	j.State = RemoteUnknown
	return j, nil
}

// MemoryRemoteProbe is an in-memory simulation helper used across recovery tests.
type MemoryRemoteProbe struct {
	mu          sync.RWMutex
	Journals    map[string]RemoteJournal
	BootID      string
	ProcAlive   bool
	ContRunning bool
	Unreachable bool
}

func NewMemoryRemoteProbe(bootID string) *MemoryRemoteProbe {
	return &MemoryRemoteProbe{
		Journals: make(map[string]RemoteJournal),
		BootID:   bootID,
	}
}

func (m *MemoryRemoteProbe) FetchJournal(_ context.Context, _ string, opID string) (RemoteJournal, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Unreachable {
		return RemoteJournal{}, false, errors.New("ssh unreachable")
	}
	j, ok := m.Journals[opID]
	return j, ok, nil
}

func (m *MemoryRemoteProbe) WriteJournalAtomic(_ context.Context, _ string, j RemoteJournal) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Unreachable {
		return errors.New("ssh unreachable")
	}
	m.Journals[j.OperationID] = j
	return nil
}

func (m *MemoryRemoteProbe) InspectHostBootAndProcess(_ context.Context, _ string, _ string, _ int, _ string) (string, bool, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Unreachable {
		return "", false, false, errors.New("ssh unreachable")
	}
	return m.BootID, m.ProcAlive, m.ContRunning, nil
}
