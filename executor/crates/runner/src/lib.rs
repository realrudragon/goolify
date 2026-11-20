use coolify_protocol::{
    is_valid_opaque_id, EventType, ExecutionEvent, ExecutionRequest, OperationKind, RemoteJournal,
    PROTOCOL_VERSION_V1,
};
use std::process::Stdio;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::Arc;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::Command;
use tokio::sync::mpsc;
use tokio::time::{Instant, Duration};

/// Control commands forwarded concurrently to a running operation.
#[derive(Debug, Clone)]
pub enum ControlCommand {
    Stdin(String),
    Resize { cols: u16, rows: u16 },
    Cancel,
}

/// Builds OpenSSH arguments cleanly without ever exposing private keys or passwords on CLI args.
/// Preserves explicit shell scripts for pipelines/redirects and handles non-root sudo wrapping.
pub fn build_ssh_args(req: &ExecutionRequest, key_path: Option<&str>, mux_path: Option<&str>) -> Vec<String> {
    let mut args = vec![
        "-o".to_string(),
        "BatchMode=yes".to_string(),
        "-o".to_string(),
        "StrictHostKeyChecking=accept-new".to_string(),
        "-o".to_string(),
        "ConnectTimeout=15".to_string(),
        "-o".to_string(),
        "ServerAliveInterval=15".to_string(),
        "-o".to_string(),
        "ServerAliveCountMax=3".to_string(),
        "-p".to_string(),
        req.operation.port.to_string(),
    ];
    if let Some(mp) = mux_path {
        args.push("-o".to_string());
        args.push("ControlMaster=auto".to_string());
        args.push("-o".to_string());
        args.push(format!("ControlPath={mp}"));
        args.push("-o".to_string());
        args.push("ControlPersist=60s".to_string());
    }
    if let Some(kp) = key_path {
        args.push("-i".to_string());
        args.push(kp.to_string());
        args.push("-o".to_string());
        args.push("IdentitiesOnly=yes".to_string());
    }
    if req.operation.kind == OperationKind::Pty {
        args.push("-tt".to_string());
    } else {
        args.push("-T".to_string());
    }
    args.push(format!("{}@{}", req.operation.user, req.operation.host));

    let remote_cmd = if req.operation.use_sudo && req.operation.user != "root" {
        format!(
            "sudo -n sh -c {}",
            shell_single_quote(&req.operation.command)
        )
    } else {
        format!("sh -c {}", shell_single_quote(&req.operation.command))
    };
    args.push(remote_cmd);
    args
}

pub fn remote_journal_path(operation_id: &str) -> Option<String> {
    if !is_valid_opaque_id(operation_id) {
        return None;
    }
    Some(format!("/data/coolify/operations/{operation_id}/journal.json"))
}

/// Generates an atomic shell command (`mkdir -p`, write `.tmp`, `mv` rename) for persisting RemoteJournal.
pub fn build_atomic_journal_write_cmd(journal: &RemoteJournal) -> Option<String> {
    let path = remote_journal_path(&journal.operation_id)?;
    let dir = format!("/data/coolify/operations/{}", journal.operation_id);
    let tmp = format!("{path}.tmp.{}", journal.attempt_id);
    let json = serde_json::to_string(journal).ok()?;
    Some(format!(
        "mkdir -p {dir} && chmod 700 {dir} && printf '%s' {payload} > {tmp} && chmod 600 {tmp} && mv -f {tmp} {path}",
        dir = shell_single_quote(&dir),
        payload = shell_single_quote(&json),
        tmp = shell_single_quote(&tmp),
        path = shell_single_quote(&path),
    ))
}

/// Generates a probe command reading host boot_id, journal JSON, PID liveness, and container labels.
pub fn build_remote_probe_cmd(operation_id: &str, pid: Option<u32>) -> Option<String> {
    let path = remote_journal_path(operation_id)?;
    let pid_check = match pid {
        Some(p) if p > 0 => format!("(kill -0 {p} 2>/dev/null && echo ALIVE || echo DEAD)"),
        _ => "echo DEAD".to_string(),
    };
    Some(format!(
        "BOOT=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null || hostname); \
         PROC=$({pid_check}); \
         CONT=$(docker ps -q --filter label=coolify.operation_id={op} 2>/dev/null | tr '\\n' ',' || true); \
         echo \"COOLIFY_BOOT=$BOOT\"; \
         echo \"COOLIFY_PROC=$PROC\"; \
         echo \"COOLIFY_CONT=$CONT\"; \
         if [ -f {path} ]; then echo \"COOLIFY_JOURNAL_FOUND=1\"; cat {path}; else echo \"COOLIFY_JOURNAL_FOUND=0\"; fi",
        pid_check = pid_check,
        op = shell_single_quote(operation_id),
        path = shell_single_quote(&path),
    ))
}

pub fn shell_single_quote(s: &str) -> String {
    format!("'{}'", s.replace('\'', "'\\''"))
}

struct EphemeralKeyFile {
    path: std::path::PathBuf,
}

impl Drop for EphemeralKeyFile {
    fn drop(&mut self) {
        let _ = std::fs::remove_file(&self.path);
    }
}

fn create_ephemeral_ssh_key(op_id: &str, att_id: &str, key_content: &str) -> std::io::Result<EphemeralKeyFile> {
    let fname = format!("coolify-ssh-{op_id}-{att_id}-{}.key", std::process::id());
    let path = std::env::temp_dir().join(fname);
    let normalized = format!("{}\n", key_content.trim_end_matches(&['\r', '\n'][..]));
    #[cfg(unix)]
    {
        use std::io::Write;
        use std::os::unix::fs::OpenOptionsExt;
        let mut f = std::fs::OpenOptions::new()
            .create(true)
            .write(true)
            .truncate(true)
            .mode(0o600)
            .open(&path)?;
        f.write_all(normalized.as_bytes())?;
    }
    #[cfg(not(unix))]
    {
        std::fs::write(&path, normalized.as_bytes())?;
    }
    Ok(EphemeralKeyFile { path })
}

/// Runs a single ExecutionRequest with immediate async event streaming, concurrent stdout/stderr draining,
/// whole-lifecycle deadline enforcement, live stdin forwarding, and explicit cancellation.
pub async fn execute_request_streaming(
    req: ExecutionRequest,
    event_tx: mpsc::Sender<ExecutionEvent>,
    mut ctrl_rx: mpsc::Receiver<ControlCommand>,
) -> i32 {
    let seq = Arc::new(AtomicU64::new(0));
    let next_seq = |s: &Arc<AtomicU64>| s.fetch_add(1, Ordering::SeqCst) + 1;

    if req.expected_capability == "local_exec" && req.team_id != 0 {
        let _ = event_tx
            .send(ExecutionEvent {
                protocol_version: PROTOCOL_VERSION_V1.into(),
                event_type: EventType::Error,
                operation_id: req.operation_id,
                attempt_id: req.attempt_id,
                sequence: next_seq(&seq),
                stream: None,
                encoding: None,
                data: None,
                exit_code: None,
                error_code: Some("HOST_BOUNDARY_DENIED".into()),
                message: Some("local_exec is restricted to root team (team_id=0)".into()),
            })
            .await;
        return 1;
    }

    let max_chunk = req.max_frame_bytes.clamp(1024, 256 * 1024);

    let _ = event_tx
        .send(ExecutionEvent {
            protocol_version: PROTOCOL_VERSION_V1.into(),
            event_type: EventType::Started,
            operation_id: req.operation_id.clone(),
            attempt_id: req.attempt_id.clone(),
            sequence: next_seq(&seq),
            stream: None,
            encoding: None,
            data: None,
            exit_code: None,
            error_code: None,
            message: None,
        })
        .await;

    let mut _ephemeral_key: Option<EphemeralKeyFile> = None;
    let mut cmd = if req.expected_capability == "local_exec" {
        let mut c = Command::new("sh");
        c.arg("-c").arg(&req.operation.command);
        c
    } else {
        if let Some(key_content) = req.operation.ssh_key_content.as_deref() {
            if !key_content.trim().is_empty() {
                match create_ephemeral_ssh_key(&req.operation_id, &req.attempt_id, key_content) {
                    Ok(ek) => _ephemeral_key = Some(ek),
                    Err(e) => {
                        let _ = event_tx
                            .send(ExecutionEvent {
                                protocol_version: PROTOCOL_VERSION_V1.into(),
                                event_type: EventType::Error,
                                operation_id: req.operation_id,
                                attempt_id: req.attempt_id,
                                sequence: next_seq(&seq),
                                stream: None,
                                encoding: None,
                                data: None,
                                exit_code: None,
                                error_code: Some("KEY_MATERIAL_ERROR".into()),
                                message: Some(format!("failed to prepare ssh key: {e}")),
                            })
                            .await;
                        return 1;
                    }
                }
            }
        }
        let key_path_str = _ephemeral_key
            .as_ref()
            .and_then(|k| k.path.to_str())
            .map(str::to_string);
        let args = build_ssh_args(&req, key_path_str.as_deref(), None);
        let mut c = Command::new("ssh");
        c.args(&args);
        c
    };

    cmd.stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .kill_on_drop(true);

    let mut child = match cmd.spawn() {
        Ok(ch) => ch,
        Err(e) => {
            let _ = event_tx
                .send(ExecutionEvent {
                    protocol_version: PROTOCOL_VERSION_V1.into(),
                    event_type: EventType::Error,
                    operation_id: req.operation_id,
                    attempt_id: req.attempt_id,
                    sequence: next_seq(&seq),
                    stream: None,
                    encoding: None,
                    data: None,
                    exit_code: None,
                    error_code: Some("SPAWN_FAILED".into()),
                    message: Some(format!("failed to spawn subprocess: {e}")),
                })
                .await;
            return 1;
        }
    };

    let mut child_stdin = child.stdin.take();
    if let (Some(data), Some(stdin)) = (&req.stdin_data, child_stdin.as_mut()) {
        if !data.is_empty() {
            let _ = stdin.write_all(data.as_bytes()).await;
            let _ = stdin.flush().await;
        }
    }

    // Spawn concurrent stdout & stderr streaming tasks so neither pipe can ever block or buffer until exit.
    let stdout_handle = child.stdout.take().map(|out| {
        let tx = event_tx.clone();
        let op_id = req.operation_id.clone();
        let att_id = req.attempt_id.clone();
        let seq_ref = Arc::clone(&seq);
        tokio::spawn(async move {
            stream_pipe(out, "stdout", max_chunk, op_id, att_id, seq_ref, tx).await;
        })
    });

    let stderr_handle = child.stderr.take().map(|err_pipe| {
        let tx = event_tx.clone();
        let op_id = req.operation_id.clone();
        let att_id = req.attempt_id.clone();
        let seq_ref = Arc::clone(&seq);
        tokio::spawn(async move {
            stream_pipe(err_pipe, "stderr", max_chunk, op_id, att_id, seq_ref, tx).await;
        })
    });

    // Deadline covers the ENTIRE execution lifecycle (stdout/stderr streaming + process exit)!
    let deadline_dur = Duration::from_millis(req.deadline_ms.max(10));
    let deadline = tokio::time::sleep_until(Instant::now() + deadline_dur);
    tokio::pin!(deadline);

    let mut cancelled = false;
    let mut timed_out = false;
    let exit_code: i32;

    loop {
        tokio::select! {
            biased;
            _ = &mut deadline => {
                timed_out = true;
                let _ = child.start_kill();
                let _ = child.wait().await;
                exit_code = 124;
                break;
            }
            ctrl = ctrl_rx.recv() => {
                match ctrl {
                    Some(ControlCommand::Cancel) => {
                        cancelled = true;
                        let _ = child.start_kill();
                        let _ = child.wait().await;
                        exit_code = 130;
                        break;
                    }
                    Some(ControlCommand::Stdin(data)) => {
                        if let Some(stdin) = child_stdin.as_mut() {
                            if data.is_empty() {
                                // Empty stdin frame signals EOF to child stdin
                                drop(child_stdin.take());
                            } else {
                                let _ = stdin.write_all(data.as_bytes()).await;
                                let _ = stdin.flush().await;
                            }
                        }
                    }
                    Some(ControlCommand::Resize { .. }) => {
                        // Tracked for PTY sessions; no-op for plain pipe exec
                    }
                    None => {
                        // Control channel closed (parent stdin EOF / worker crash):
                        // Reap local child immediately and DO NOT emit a confirmed remote Exited event!
                        let _ = child.start_kill();
                        let _ = child.wait().await;
                        if let Some(h) = stdout_handle { h.abort(); }
                        if let Some(h) = stderr_handle { h.abort(); }
                        return 137;
                    }
                }
            }
            status = child.wait() => {
                exit_code = match status {
                    Ok(st) => st.code().unwrap_or(1),
                    Err(_) => 1,
                };
                break;
            }
        }
    }

    // Drain remaining buffered lines briefly when process finished normally (not aborted by EOF).
    if timed_out || cancelled {
        if let Some(h) = stdout_handle {
            let _ = tokio::time::timeout(Duration::from_millis(100), h).await;
        }
        if let Some(h) = stderr_handle {
            let _ = tokio::time::timeout(Duration::from_millis(100), h).await;
        }
    } else {
        if let Some(h) = stdout_handle {
            let _ = h.await;
        }
        if let Some(h) = stderr_handle {
            let _ = h.await;
        }
    }

    if timed_out {
        let _ = event_tx
            .send(ExecutionEvent {
                protocol_version: PROTOCOL_VERSION_V1.into(),
                event_type: EventType::Error,
                operation_id: req.operation_id.clone(),
                attempt_id: req.attempt_id.clone(),
                sequence: next_seq(&seq),
                stream: None,
                encoding: None,
                data: None,
                exit_code: Some(124),
                error_code: Some("DEADLINE_EXCEEDED".into()),
                message: Some("execution deadline exceeded; local subprocess terminated".into()),
            })
            .await;
    } else if cancelled {
        let _ = event_tx
            .send(ExecutionEvent {
                protocol_version: PROTOCOL_VERSION_V1.into(),
                event_type: EventType::Error,
                operation_id: req.operation_id.clone(),
                attempt_id: req.attempt_id.clone(),
                sequence: next_seq(&seq),
                stream: None,
                encoding: None,
                data: None,
                exit_code: Some(130),
                error_code: Some("CANCELLED".into()),
                message: Some("execution cancelled by control frame".into()),
            })
            .await;
    }

    let _ = event_tx
        .send(ExecutionEvent {
            protocol_version: PROTOCOL_VERSION_V1.into(),
            event_type: EventType::Exited,
            operation_id: req.operation_id,
            attempt_id: req.attempt_id,
            sequence: next_seq(&seq),
            stream: None,
            encoding: None,
            data: None,
            exit_code: Some(exit_code),
            error_code: None,
            message: None,
        })
        .await;

    exit_code
}

async fn stream_pipe<R>(
    pipe: R,
    stream_name: &str,
    max_chunk: usize,
    op_id: String,
    att_id: String,
    seq: Arc<AtomicU64>,
    tx: mpsc::Sender<ExecutionEvent>,
) where
    R: tokio::io::AsyncRead + Unpin,
{
    let mut reader = BufReader::new(pipe);
    let mut buf = Vec::new();
    loop {
        buf.clear();
        match reader.read_until(b'\n', &mut buf).await {
            Ok(0) => break,
            Ok(_) => {
                // Trim trailing CR/LF while preserving inner whitespace
                while buf.ends_with(b"\n") || buf.ends_with(b"\r") {
                    buf.pop();
                }
                let text = String::from_utf8_lossy(&buf);
                // Split oversized lines into bounded UTF-8 chunks <= max_chunk bytes
                let mut start = 0;
                let s = text.as_ref();
                if s.is_empty() {
                    let cur = seq.fetch_add(1, Ordering::SeqCst) + 1;
                    let _ = tx
                        .send(ExecutionEvent {
                            protocol_version: PROTOCOL_VERSION_V1.into(),
                            event_type: EventType::Output,
                            operation_id: op_id.clone(),
                            attempt_id: att_id.clone(),
                            sequence: cur,
                            stream: Some(stream_name.into()),
                            encoding: Some("utf8".into()),
                            data: Some(String::new()),
                            exit_code: None,
                            error_code: None,
                            message: None,
                        })
                        .await;
                    continue;
                }
                while start < s.len() {
                    let mut end = (start + max_chunk).min(s.len());
                    while end > start && !s.is_char_boundary(end) {
                        end -= 1;
                    }
                    if end == start {
                        end = s.len();
                    }
                    let cur = seq.fetch_add(1, Ordering::SeqCst) + 1;
                    let _ = tx
                        .send(ExecutionEvent {
                            protocol_version: PROTOCOL_VERSION_V1.into(),
                            event_type: EventType::Output,
                            operation_id: op_id.clone(),
                            attempt_id: att_id.clone(),
                            sequence: cur,
                            stream: Some(stream_name.into()),
                            encoding: Some("utf8".into()),
                            data: Some(s[start..end].to_string()),
                            exit_code: None,
                            error_code: None,
                            message: None,
                        })
                        .await;
                    start = end;
                }
            }
            Err(_) => break,
        }
    }
}
