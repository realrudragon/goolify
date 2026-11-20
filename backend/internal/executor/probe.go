package executor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// LocalCommandTransport runs real local shell or OpenSSH subprocesses with immediate line streaming,
// concurrent stdout/stderr draining, and full-lifecycle context deadline enforcement.
// Production Linux images run the Rust `SubprocessTransport` (`coolify-executor`); LocalCommandTransport
// provides a real OS-backed transport when executing directly on a host without the Linux Rust binary.
type LocalCommandTransport struct {
	SSHBinary string
}

func NewLocalCommandTransport() *LocalCommandTransport {
	return &LocalCommandTransport{SSHBinary: "ssh"}
}

func (l *LocalCommandTransport) StreamNDJSON(ctx context.Context, req ExecutionRequest, emit func(ExecutionEvent) error) (int, error) {
	runCtx := ctx
	var cancel context.CancelFunc
	if req.DeadlineMS > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(req.DeadlineMS)*time.Millisecond)
		defer cancel()
	}

	var cmd *exec.Cmd
	if req.ExpectedCapability == "local_exec" {
		if req.TeamID != 0 {
			return 1, errors.New("host boundary denied: local_exec is strictly restricted to root team (team_id=0)")
		}
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(runCtx, "powershell", "-NoProfile", "-NonInteractive", "-Command", req.Operation.Command)
		} else {
			cmd = exec.CommandContext(runCtx, "sh", "-c", req.Operation.Command)
		}
	} else {
		sshBin := l.SSHBinary
		if sshBin == "" {
			sshBin = "ssh"
		}
		args := []string{
			"-o", "BatchMode=yes",
			"-o", "StrictHostKeyChecking=accept-new",
			"-o", "ConnectTimeout=15",
			"-p", fmt.Sprintf("%d", maxPort(req.Operation.Port)),
		}
		if req.Operation.SSHKeyContent != "" {
			keyFile, kErr := os.CreateTemp("", "coolify-ssh-*")
			if kErr != nil {
				return 1, fmt.Errorf("create ephemeral ssh key: %w", kErr)
			}
			keyPath := keyFile.Name()
			defer func() { _ = os.Remove(keyPath) }()
			_ = keyFile.Chmod(0o600)
			keyBody := strings.TrimRight(req.Operation.SSHKeyContent, "\r\n") + "\n"
			if _, wErr := keyFile.WriteString(keyBody); wErr != nil {
				_ = keyFile.Close()
				return 1, fmt.Errorf("write ephemeral ssh key: %w", wErr)
			}
			_ = keyFile.Close()
			args = append(args, "-i", keyPath, "-o", "IdentitiesOnly=yes")
		}
		if req.Operation.Kind == "pty" {
			args = append(args, "-tt")
		} else {
			args = append(args, "-T")
		}
		args = append(args, fmt.Sprintf("%s@%s", req.Operation.User, req.Operation.Host))
		innerCmd := req.Operation.Command
		if req.Operation.PTYCols > 0 || req.Operation.PTYRows > 0 {
			cols := req.Operation.PTYCols
			if cols <= 0 {
				cols = 80
			}
			rows := req.Operation.PTYRows
			if rows <= 0 {
				rows = 24
			}
			innerCmd = fmt.Sprintf("COLUMNS=%d LINES=%d %s", cols, rows, innerCmd)
		}
		remoteCmd := fmt.Sprintf("sh -c '%s'", strings.ReplaceAll(innerCmd, "'", `'\''`))
		if req.Operation.UseSudo && req.Operation.User != "root" {
			remoteCmd = "sudo -n " + remoteCmd
		}
		args = append(args, remoteCmd)
		cmd = exec.CommandContext(runCtx, sshBin, args...)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return -1, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return -1, err
	}
	if req.StdinData != "" {
		cmd.Stdin = strings.NewReader(req.StdinData)
	}

	var seq int64 = 1
	var emitMu sync.Mutex
	safeEmit := func(ev ExecutionEvent) error {
		emitMu.Lock()
		defer emitMu.Unlock()
		ev.ProtocolVersion = ProtocolVersionV1
		ev.OperationID = req.OperationID
		ev.AttemptID = req.AttemptID
		ev.Sequence = seq
		seq++
		return emit(ev)
	}

	if err := safeEmit(ExecutionEvent{EventType: "started"}); err != nil {
		return -1, err
	}
	if err := cmd.Start(); err != nil {
		_ = safeEmit(ExecutionEvent{EventType: "error", ErrorCode: "SPAWN_FAILED", Message: err.Error()})
		return 1, err
	}

	var wg sync.WaitGroup
	streamPipe := func(r io.Reader, name string) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 4096), DefaultMaxFrameBytes)
		for sc.Scan() {
			_ = safeEmit(ExecutionEvent{
				EventType: "output",
				Stream:    name,
				Encoding:  "utf8",
				Data:      sc.Text(),
			})
		}
	}
	wg.Add(2)
	go streamPipe(stdout, "stdout")
	go streamPipe(stderr, "stderr")
	wg.Wait()

	waitErr := cmd.Wait()
	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			exitCode = 124
		} else {
			exitCode = 1
		}
	}
	_ = safeEmit(ExecutionEvent{EventType: "exited", ExitCode: &exitCode})
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return 124, context.DeadlineExceeded
	}
	return exitCode, nil
}

func maxPort(p int) int {
	if p <= 0 {
		return 22
	}
	return p
}

// PostgresRemoteProbe stores and reconciles durable operation journals in PostgreSQL `go_remote_journals`
// and optionally probes real Docker container liveness by `coolify.operation_id` label.
type PostgresRemoteProbe struct {
	pool        *pgxpool.Pool
	execClient  *Client
	decryptKey  func(string) string
	checkDocker bool
	bootID      string
}

func NewPostgresRemoteProbe(pool *pgxpool.Pool, checkDocker bool) *PostgresRemoteProbe {
	host, _ := os.Hostname()
	if host == "" {
		host = "coolify-host"
	}
	return &PostgresRemoteProbe{
		pool:        pool,
		checkDocker: checkDocker,
		bootID:      "host-boot-" + host,
	}
}

func NewSSHAwareRemoteProbe(pool *pgxpool.Pool, execClient *Client, decryptKey func(string) string, checkDocker bool) *PostgresRemoteProbe {
	p := NewPostgresRemoteProbe(pool, checkDocker)
	p.execClient = execClient
	p.decryptKey = decryptKey
	return p
}

func (p *PostgresRemoteProbe) loadSSHServerTarget(ctx context.Context, serverUUID string) (srvID int64, host string, port int, user string, decKey string, useSSH bool) {
	if p.pool == nil || p.execClient == nil || serverUUID == "" || serverUUID == "localhost" {
		return 0, "", 0, "", "", false
	}
	var encKey string
	err := p.pool.QueryRow(ctx, `
		SELECT s.id, s.ip, s.port, s."user", COALESCE(pk.private_key, '')
		FROM servers s
		LEFT JOIN private_keys pk ON pk.id = s.private_key_id
		WHERE s.uuid = $1 AND s.deleted_at IS NULL
	`, serverUUID).Scan(&srvID, &host, &port, &user, &encKey)
	if err != nil || srvID == 0 {
		return 0, "", 0, "", "", false
	}
	if port <= 0 {
		port = 22
	}
	if strings.TrimSpace(user) == "" {
		user = "root"
	}
	decKey = encKey
	if encKey != "" && p.decryptKey != nil {
		if plain := p.decryptKey(encKey); plain != "" {
			decKey = plain
		}
	}
	return srvID, host, port, user, decKey, true
}

func probeShellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (p *PostgresRemoteProbe) FetchJournal(ctx context.Context, serverUUID string, operationID string) (RemoteJournal, bool, error) {
	if _, err := RemoteJournalPath(operationID); err != nil {
		return RemoteJournal{}, false, err
	}
	if srvID, host, port, user, key, useSSH := p.loadSSHServerTarget(ctx, serverUUID); useSSH && srvID != 0 {
		jPath, _ := RemoteJournalPath(operationID)
		cmd := fmt.Sprintf("if [ -f %s ]; then echo 'COOLIFY_JOURNAL_FOUND=1'; cat %s; else echo 'COOLIFY_JOURNAL_FOUND=0'; fi", probeShellQuote(jPath), probeShellQuote(jPath))
		var lines []string
		res, err := p.execClient.Run(ctx, ExecutionRequest{
			ProtocolVersion:    ProtocolVersionV1,
			FrameType:          FrameStart,
			JobID:              "probe-fetch-" + operationID,
			OperationID:        "probe-fetch-" + operationID,
			AttemptID:          "att-0",
			OwnershipEpoch:     1,
			ResourceUUID:       serverUUID,
			DeadlineMS:         15000,
			MaxFrameBytes:      DefaultMaxFrameBytes,
			ExpectedCapability: "ssh_exec",
			Operation: OperationSpec{
				Kind: "exec", Host: host, Port: port, User: user, SSHKeyContent: key, UseSudo: user != "root", Command: cmd,
			},
		}, func(ev ExecutionEvent) error {
			if ev.EventType == "output" {
				lines = append(lines, ev.Data)
			}
			return nil
		})
		if err != nil || res.Ambiguous {
			return RemoteJournal{}, false, fmt.Errorf("remote journal fetch failed: %w", err)
		}
		joined := strings.Join(lines, "\n")
		if strings.Contains(joined, "COOLIFY_JOURNAL_FOUND=1") {
			idx := strings.Index(joined, "COOLIFY_JOURNAL_FOUND=1")
			payload := strings.TrimSpace(joined[idx+len("COOLIFY_JOURNAL_FOUND=1"):])
			var j RemoteJournal
			if err := json.Unmarshal([]byte(payload), &j); err != nil {
				return RemoteJournal{OperationID: operationID, Corrupt: true, State: RemoteUnknown}, true, nil
			}
			return j, true, nil
		}
		if strings.Contains(joined, "COOLIFY_JOURNAL_FOUND=0") {
			return RemoteJournal{}, false, nil
		}
	}

	var raw []byte
	err := p.pool.QueryRow(ctx, `SELECT journal_json FROM go_remote_journals WHERE operation_id = $1`, operationID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return RemoteJournal{}, false, nil
	}
	if err != nil {
		return RemoteJournal{}, false, err
	}
	var j RemoteJournal
	if err := json.Unmarshal(raw, &j); err != nil {
		return RemoteJournal{OperationID: operationID, Corrupt: true, State: RemoteUnknown}, true, nil
	}
	return j, true, nil
}

func (p *PostgresRemoteProbe) WriteJournalAtomic(ctx context.Context, serverUUID string, j RemoteJournal) error {
	jPath, err := RemoteJournalPath(j.OperationID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if srvID, host, port, user, key, useSSH := p.loadSSHServerTarget(ctx, serverUUID); useSSH && srvID != 0 {
		dir := "/data/coolify/operations/" + j.OperationID
		tmp := jPath + ".tmp." + j.AttemptID
		cmd := fmt.Sprintf("mkdir -p %s && chmod 700 %s && printf '%%s' %s > %s && chmod 600 %s && mv -f %s %s",
			probeShellQuote(dir), probeShellQuote(dir), probeShellQuote(string(raw)),
			probeShellQuote(tmp), probeShellQuote(tmp), probeShellQuote(tmp), probeShellQuote(jPath))
		res, runErr := p.execClient.Run(ctx, ExecutionRequest{
			ProtocolVersion:    ProtocolVersionV1,
			FrameType:          FrameStart,
			JobID:              j.JobID,
			OperationID:        "probe-write-" + j.OperationID,
			AttemptID:          j.AttemptID,
			OwnershipEpoch:     j.OwnershipEpoch,
			ResourceUUID:       j.ResourceUUID,
			DeadlineMS:         15000,
			MaxFrameBytes:      DefaultMaxFrameBytes,
			ExpectedCapability: "ssh_exec",
			Operation: OperationSpec{
				Kind: "exec", Host: host, Port: port, User: user, SSHKeyContent: key, UseSudo: user != "root", Command: cmd,
			},
		}, nil)
		if runErr != nil || res.Ambiguous || res.ExitCode != 0 {
			return fmt.Errorf("remote journal atomic write failed (exit=%d err=%v)", res.ExitCode, runErr)
		}
	}
	_, err = p.pool.Exec(ctx, `
		INSERT INTO go_remote_journals (operation_id, server_uuid, journal_json, updated_at)
		VALUES ($1, $2, $3::jsonb, NOW())
		ON CONFLICT (operation_id) DO UPDATE SET
			server_uuid = EXCLUDED.server_uuid,
			journal_json = EXCLUDED.journal_json,
			updated_at = NOW()
	`, j.OperationID, serverUUID, string(raw))
	return err
}

func (p *PostgresRemoteProbe) InspectHostBootAndProcess(ctx context.Context, serverUUID string, _ string, pid int, operationID string) (string, bool, bool, error) {
	if _, err := RemoteJournalPath(operationID); err != nil {
		return "", false, false, err
	}
	if srvID, host, port, user, key, useSSH := p.loadSSHServerTarget(ctx, serverUUID); useSSH && srvID != 0 {
		pidCheck := "echo DEAD"
		if pid > 0 {
			pidCheck = fmt.Sprintf("(kill -0 %d 2>/dev/null && echo ALIVE || echo DEAD)", pid)
		}
		jPath, _ := RemoteJournalPath(operationID)
		cmd := fmt.Sprintf(
			"BOOT=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null || hostname); PROC=$(%s); CONT=$(docker ps -q --filter label=coolify.operation_id=%s 2>/dev/null | tr '\\n' ',' || true); echo \"COOLIFY_BOOT=$BOOT\"; echo \"COOLIFY_PROC=$PROC\"; echo \"COOLIFY_CONT=$CONT\"; if [ -f %s ]; then echo \"COOLIFY_JOURNAL_FOUND=1\"; else echo \"COOLIFY_JOURNAL_FOUND=0\"; fi",
			pidCheck, probeShellQuote(operationID), probeShellQuote(jPath),
		)
		var lines []string
		res, runErr := p.execClient.Run(ctx, ExecutionRequest{
			ProtocolVersion:    ProtocolVersionV1,
			FrameType:          FrameStart,
			JobID:              "probe-inspect-" + operationID,
			OperationID:        "probe-inspect-" + operationID,
			AttemptID:          "att-0",
			OwnershipEpoch:     1,
			ResourceUUID:       serverUUID,
			DeadlineMS:         15000,
			MaxFrameBytes:      DefaultMaxFrameBytes,
			ExpectedCapability: "ssh_exec",
			Operation: OperationSpec{
				Kind: "exec", Host: host, Port: port, User: user, SSHKeyContent: key, UseSudo: user != "root", Command: cmd,
			},
		}, func(ev ExecutionEvent) error {
			if ev.EventType == "output" {
				lines = append(lines, ev.Data)
			}
			return nil
		})
		if runErr != nil || res.Ambiguous || res.ExitCode != 0 {
			return "", false, false, fmt.Errorf("remote host probe failed: %w", runErr)
		}
		var bootID string
		var procAlive, contAlive bool
		for _, ln := range lines {
			switch {
			case strings.HasPrefix(ln, "COOLIFY_BOOT="):
				bootID = strings.TrimSpace(strings.TrimPrefix(ln, "COOLIFY_BOOT="))
			case strings.HasPrefix(ln, "COOLIFY_PROC="):
				procAlive = strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(ln, "COOLIFY_PROC=")), "ALIVE")
			case strings.HasPrefix(ln, "COOLIFY_CONT="):
				contVal := strings.Trim(strings.TrimSpace(strings.TrimPrefix(ln, "COOLIFY_CONT=")), ",")
				contAlive = contVal != ""
			}
		}
		if bootID == "" {
			bootID = p.bootID
		}
		return bootID, procAlive, contAlive, nil
	}

	containerAlive := false
	if p.checkDocker && operationID != "" {
		checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(checkCtx, "docker", "ps", "-q", "--filter", "label=coolify.operation_id="+operationID).Output()
		if err == nil && strings.TrimSpace(string(out)) != "" {
			containerAlive = true
		}
	}
	return p.bootID, false, containerAlive, nil
}
