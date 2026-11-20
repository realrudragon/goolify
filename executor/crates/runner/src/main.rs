use coolify_protocol::{
    parse_ndjson_request, EventType, ExecutionEvent, FrameType, PROTOCOL_VERSION_V1,
};
use coolify_runner::{execute_request_streaming, ControlCommand};
use std::collections::HashMap;
use std::sync::Arc;
use tokio::io::{self, AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::sync::{mpsc, Mutex};

#[tokio::main]
async fn main() {
    let stdin = io::stdin();
    let mut stdout = io::stdout();
    let mut lines = BufReader::new(stdin).lines();

    // Dedicated writer task flushes EVERY NDJSON event immediately to stdout (zero end-of-job buffering).
    let (event_tx, mut event_rx) = mpsc::channel::<ExecutionEvent>(256);
    let writer_task = tokio::spawn(async move {
        while let Some(ev) = event_rx.recv().await {
            if let Ok(serialized) = serde_json::to_string(&ev) {
                if stdout.write_all(serialized.as_bytes()).await.is_err() {
                    break;
                }
                if stdout.write_all(b"\n").await.is_err() {
                    break;
                }
                if stdout.flush().await.is_err() {
                    break;
                }
            }
        }
    });

    let active: Arc<Mutex<HashMap<String, mpsc::Sender<ControlCommand>>>> =
        Arc::new(Mutex::new(HashMap::new()));
    let mut join_handles = Vec::new();

    while let Ok(Some(line)) = lines.next_line().await {
        if line.trim().is_empty() {
            continue;
        }
        match parse_ndjson_request(&line) {
            Ok(req) => {
                let op_key = format!("{}:{}", req.operation_id, req.attempt_id);
                match req.frame_type {
                    FrameType::Start => {
                        let (ctrl_tx, ctrl_rx) = mpsc::channel::<ControlCommand>(64);
                        active.lock().await.insert(op_key.clone(), ctrl_tx);
                        let tx_clone = event_tx.clone();
                        let active_clone = Arc::clone(&active);
                        let handle = tokio::spawn(async move {
                            let _ = execute_request_streaming(req, tx_clone, ctrl_rx).await;
                            active_clone.lock().await.remove(&op_key);
                        });
                        join_handles.push(handle);
                    }
                    FrameType::Stdin => {
                        let tx_opt = active.lock().await.get(&op_key).cloned();
                        if let Some(tx) = tx_opt {
                            let _ = tx
                                .send(ControlCommand::Stdin(req.stdin_data.unwrap_or_default()))
                                .await;
                        }
                    }
                    FrameType::Resize => {
                        let tx_opt = active.lock().await.get(&op_key).cloned();
                        if let Some(tx) = tx_opt {
                            let _ = tx
                                .send(ControlCommand::Resize {
                                    cols: req.operation.pty_cols.unwrap_or(80),
                                    rows: req.operation.pty_rows.unwrap_or(24),
                                })
                                .await;
                        }
                    }
                    FrameType::Cancel => {
                        let tx_opt = active.lock().await.get(&op_key).cloned();
                        if let Some(tx) = tx_opt {
                            let _ = tx.send(ControlCommand::Cancel).await;
                        }
                    }
                }
            }
            Err(err) => {
                let _ = event_tx
                    .send(ExecutionEvent {
                        protocol_version: PROTOCOL_VERSION_V1.into(),
                        event_type: EventType::Error,
                        operation_id: "unknown".into(),
                        attempt_id: "unknown".into(),
                        sequence: 0,
                        stream: None,
                        encoding: None,
                        data: None,
                        exit_code: None,
                        error_code: Some("PROTOCOL_ERROR".into()),
                        message: Some(err.to_string()),
                    })
                    .await;
            }
        }
    }

    // Stdin closed (EOF): drop all active control channels so running tasks reap their local child processes immediately.
    active.lock().await.clear();
    for h in join_handles {
        let _ = h.await;
    }
    drop(event_tx);
    let _ = writer_task.await;
}
