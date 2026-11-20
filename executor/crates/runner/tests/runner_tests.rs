use coolify_protocol::{
    EventType, ExecutionEvent, ExecutionRequest, FrameType, OperationKind, OperationSpec,
    RemoteJournal, RemoteState, PROTOCOL_VERSION_V1,
};
use coolify_runner::{
    build_atomic_journal_write_cmd, build_remote_probe_cmd, build_ssh_args,
    execute_request_streaming, remote_journal_path, ControlCommand,
};
use std::process::Stdio;
use std::time::{Duration, Instant};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::Command;
use tokio::sync::mpsc;

fn make_local_req(op_id: &str, cmd: &str, deadline_ms: u64) -> ExecutionRequest {
    ExecutionRequest {
        protocol_version: PROTOCOL_VERSION_V1.into(),
        frame_type: FrameType::Start,
        job_id: "job-1".into(),
        operation_id: op_id.into(),
        attempt_id: "att-1".into(),
        ownership_epoch: 1,
        resource_uuid: "app-1".into(),
        team_id: 0,
        deadline_ms,
        max_frame_bytes: 65536,
        expected_capability: "local_exec".into(),
        operation: OperationSpec {
            kind: OperationKind::Exec,
            host: "127.0.0.1".into(),
            port: 22,
            user: "root".into(),
            private_key_fd: None,
            ssh_key_content: None,
            use_sudo: false,
            command: cmd.into(),
            pty_cols: None,
            pty_rows: None,
        },
        stdin_data: None,
    }
}

#[tokio::test]
async fn streams_stdout_and_stderr_immediately_before_process_exits() {
    let req = make_local_req(
        "op-stream-1",
        "echo first-line; echo err-line >&2; sleep 0.35; echo second-line",
        5000,
    );
    let (ev_tx, mut ev_rx) = mpsc::channel::<ExecutionEvent>(32);
    let (_ctrl_tx, ctrl_rx) = mpsc::channel::<ControlCommand>(8);

    let started_at = Instant::now();
    let task = tokio::spawn(execute_request_streaming(req, ev_tx, ctrl_rx));

    let mut saw_first_ms: Option<u128> = None;
    let mut saw_stderr = false;
    let mut saw_second = false;
    let mut exit_code = None;

    while let Some(ev) = ev_rx.recv().await {
        if ev.event_type == EventType::Output {
            if ev.stream.as_deref() == Some("stdout") && ev.data.as_deref() == Some("first-line") {
                saw_first_ms = Some(started_at.elapsed().as_millis());
            }
            if ev.stream.as_deref() == Some("stderr") && ev.data.as_deref() == Some("err-line") {
                saw_stderr = true;
            }
            if ev.stream.as_deref() == Some("stdout") && ev.data.as_deref() == Some("second-line") {
                saw_second = true;
            }
        }
        if ev.event_type == EventType::Exited {
            exit_code = ev.exit_code;
        }
    }

    let code = task.await.unwrap();
    assert_eq!(code, 0);
    assert_eq!(exit_code, Some(0));
    assert!(saw_stderr, "expected stderr line to be streamed");
    assert!(saw_second, "expected second stdout line");
    let first_ms = saw_first_ms.expect("expected first-line");
    assert!(
        first_ms < 250,
        "expected first-line streamed immediately (<250ms), took {first_ms}ms (buffered!)"
    );
}

#[tokio::test]
async fn enforces_deadline_across_entire_lifecycle_even_with_open_stdout() {
    let req = make_local_req("op-timeout-1", "echo starting; sleep 30", 250);
    let (ev_tx, mut ev_rx) = mpsc::channel::<ExecutionEvent>(32);
    let (_ctrl_tx, ctrl_rx) = mpsc::channel::<ControlCommand>(8);

    let start = Instant::now();
    let code = execute_request_streaming(req, ev_tx, ctrl_rx).await;
    let elapsed = start.elapsed();

    assert_eq!(code, 124, "expected timeout exit code 124");
    assert!(
        elapsed < Duration::from_secs(2),
        "deadline failed to interrupt open stdout; elapsed={elapsed:?}"
    );

    let mut saw_timeout_err = false;
    while let Some(ev) = ev_rx.recv().await {
        if ev.event_type == EventType::Error && ev.error_code.as_deref() == Some("DEADLINE_EXCEEDED")
        {
            saw_timeout_err = true;
        }
    }
    assert!(saw_timeout_err, "expected DEADLINE_EXCEEDED error event");
}

#[tokio::test]
async fn supports_live_cancel_and_stdin_forwarding() {
    // 1. Live Cancel frame stops sleeping child promptly
    let req = make_local_req("op-cancel-1", "echo ready; sleep 30", 10000);
    let (ev_tx, mut ev_rx) = mpsc::channel::<ExecutionEvent>(32);
    let (ctrl_tx, ctrl_rx) = mpsc::channel::<ControlCommand>(8);

    let start = Instant::now();
    let handle = tokio::spawn(execute_request_streaming(req, ev_tx, ctrl_rx));

    // Wait for "ready" output before sending Cancel
    while let Some(ev) = ev_rx.recv().await {
        if ev.event_type == EventType::Output && ev.data.as_deref() == Some("ready") {
            ctrl_tx.send(ControlCommand::Cancel).await.unwrap();
            break;
        }
    }
    let code = handle.await.unwrap();
    assert_eq!(code, 130);
    assert!(start.elapsed() < Duration::from_secs(2));

    // 2. Live Stdin forwarding to `cat`
    let cat_req = make_local_req("op-stdin-1", "cat", 5000);
    let (ev_tx2, mut ev_rx2) = mpsc::channel::<ExecutionEvent>(32);
    let (ctrl_tx2, ctrl_rx2) = mpsc::channel::<ControlCommand>(8);
    let h2 = tokio::spawn(execute_request_streaming(cat_req, ev_tx2, ctrl_rx2));

    ctrl_tx2
        .send(ControlCommand::Stdin("hello-from-control\n".into()))
        .await
        .unwrap();
    ctrl_tx2.send(ControlCommand::Stdin(String::new())).await.unwrap(); // close child stdin

    let mut echoed = false;
    while let Some(ev) = ev_rx2.recv().await {
        if ev.data.as_deref() == Some("hello-from-control") {
            echoed = true;
        }
    }
    assert_eq!(h2.await.unwrap(), 0);
    assert!(echoed, "expected cat to echo forwarded stdin");
}

#[tokio::test]
async fn executor_binary_streams_immediately_and_reaps_child_on_stdin_eof() {
    let bin = env!("CARGO_BIN_EXE_coolify-executor");
    let mut child = Command::new(bin)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .expect("spawn coolify-executor binary");

    let mut stdin = child.stdin.take().unwrap();
    let stdout = child.stdout.take().unwrap();
    let mut lines = BufReader::new(stdout).lines();

    let req = make_local_req("op-bin-eof", "echo bin-live; sleep 30", 15000);
    let req_line = serde_json::to_string(&req).unwrap();
    let start = Instant::now();
    stdin
        .write_all(format!("{req_line}\n").as_bytes())
        .await
        .unwrap();
    stdin.flush().await.unwrap();

    // Expect `started` then `bin-live` immediately (well before sleep 30!)
    let l1 = tokio::time::timeout(Duration::from_secs(2), lines.next_line())
        .await
        .expect("timed out waiting for started frame")
        .unwrap()
        .unwrap();
    let ev1: ExecutionEvent = serde_json::from_str(&l1).unwrap();
    assert_eq!(ev1.event_type, EventType::Started);

    let l2 = tokio::time::timeout(Duration::from_secs(2), lines.next_line())
        .await
        .expect("timed out waiting for live streamed stdout line")
        .unwrap()
        .unwrap();
    let ev2: ExecutionEvent = serde_json::from_str(&l2).unwrap();
    assert_eq!(ev2.event_type, EventType::Output);
    assert_eq!(ev2.data.as_deref(), Some("bin-live"));
    assert!(start.elapsed() < Duration::from_secs(2));

    // Close stdin (simulating Go worker crash/EOF) -> executor must reap child and exit WITHOUT emitting Exited!
    drop(stdin);
    let mut saw_false_exited = false;
    while let Ok(Some(l)) = lines.next_line().await {
        if let Ok(ev) = serde_json::from_str::<ExecutionEvent>(&l) {
            if ev.event_type == EventType::Exited {
                saw_false_exited = true;
            }
        }
    }
    let status = tokio::time::timeout(Duration::from_secs(2), child.wait())
        .await
        .expect("executor hung after stdin EOF")
        .unwrap();
    assert!(status.success());
    assert!(
        !saw_false_exited,
        "stdin EOF must NOT emit a confirmed remote Exited event"
    );
}

#[test]
fn ssh_args_handle_non_root_sudo_and_atomic_journal_commands() {
    let mut req = make_local_req("op-ssh-1", "echo 'hi' | tee /tmp/a && ls", 5000);
    req.expected_capability = "ssh_exec".into();
    req.operation.user = "deploy".into();
    req.operation.use_sudo = true;
    let args = build_ssh_args(&req, Some("/tmp/key_0600"), Some("/tmp/mux_%C"));
    assert!(args.contains(&"-i".to_string()));
    assert!(args.contains(&"/tmp/key_0600".to_string()));
    assert!(args.contains(&"IdentitiesOnly=yes".to_string()));
    let last = args.last().unwrap();
    assert!(last.starts_with("sudo -n sh -c '"));
    assert!(remote_journal_path("../bad").is_none());

    let j = RemoteJournal {
        protocol_version: PROTOCOL_VERSION_V1.into(),
        job_id: "job-1".into(),
        operation_id: "op-ssh-1".into(),
        attempt_id: "att-1".into(),
        ownership_epoch: 2,
        resource_uuid: "app-1".into(),
        input_fingerprint: "sha256:abc".into(),
        state: RemoteState::Running,
        boot_id: "boot-1".into(),
        pid: Some(42),
        container_ids: vec![],
        exit_code: None,
        updated_at: "2026-11-19T00:00:00Z".into(),
    };
    let write_cmd = build_atomic_journal_write_cmd(&j).unwrap();
    assert!(write_cmd.contains("mv -f"));
    assert!(write_cmd.contains("/data/coolify/operations/op-ssh-1/journal.json"));

    let probe_cmd = build_remote_probe_cmd("op-ssh-1", Some(42)).unwrap();
    assert!(probe_cmd.contains("/proc/sys/kernel/random/boot_id"));
    assert!(probe_cmd.contains("kill -0 42"));
    assert!(probe_cmd.contains("coolify.operation_id"));
}

#[tokio::test]
async fn refuses_local_exec_for_non_root_team_and_cleans_ephemeral_ssh_key() {
    let mut non_root_local = make_local_req("op-boundary-1", "echo should_fail", 2000);
    non_root_local.team_id = 42;
    let (ev_tx, mut ev_rx) = mpsc::channel::<ExecutionEvent>(16);
    let (_ctrl_tx, ctrl_rx) = mpsc::channel::<ControlCommand>(8);
    let code = execute_request_streaming(non_root_local, ev_tx, ctrl_rx).await;
    assert_eq!(code, 1);
    let mut saw_boundary_err = false;
    while let Some(ev) = ev_rx.recv().await {
        if ev.error_code.as_deref() == Some("HOST_BOUNDARY_DENIED") {
            saw_boundary_err = true;
        }
    }
    assert!(saw_boundary_err, "expected HOST_BOUNDARY_DENIED for non-root team local_exec");

    let mut ssh_req = make_local_req("op-ephemeral-key-1", "echo hi", 500);
    ssh_req.expected_capability = "ssh_exec".into();
    ssh_req.operation.kind = OperationKind::Pty;
    ssh_req.operation.host = "127.0.0.1".into();
    ssh_req.operation.port = 1; // guaranteed fast fail connect refused
    ssh_req.operation.ssh_key_content = Some("-----BEGIN OPENSSH PRIVATE KEY-----\ndummy\n-----END OPENSSH PRIVATE KEY-----".into());
    let (ev_tx2, _ev_rx2) = mpsc::channel::<ExecutionEvent>(16);
    let (_ctrl_tx2, ctrl_rx2) = mpsc::channel::<ControlCommand>(8);
    let _ = execute_request_streaming(ssh_req, ev_tx2, ctrl_rx2).await;
    let leftover = std::env::temp_dir().join(format!("coolify-ssh-op-ephemeral-key-1-att-1-{}.key", std::process::id()));
    assert!(!leftover.exists(), "ephemeral SSH key file must be removed after execution");
}
