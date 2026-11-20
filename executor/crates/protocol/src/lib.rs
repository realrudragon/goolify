use serde::{Deserialize, Serialize};
use thiserror::Error;

pub const PROTOCOL_VERSION_V1: &str = "v1";
pub const MAX_FRAME_BYTES: usize = 1_048_576;

#[derive(Debug, Error)]
pub enum ProtocolError {
    #[error("unsupported protocol version: {0}")]
    UnsupportedVersion(String),
    #[error("frame exceeds maximum size ({size} > {max})")]
    FrameTooLarge { size: usize, max: usize },
    #[error("invalid opaque identifier: {0}")]
    InvalidOpaqueId(String),
    #[error("refusing secret key material in command arguments")]
    SecretInCommand,
    #[error("json parse error: {0}")]
    Json(#[from] serde_json::Error),
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum FrameType {
    Start,
    Stdin,
    Resize,
    Cancel,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum OperationKind {
    Exec,
    Pty,
    JournalProbe,
    Reconcile,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct OperationSpec {
    pub kind: OperationKind,
    pub host: String,
    pub port: u16,
    pub user: String,
    #[serde(default)]
    pub private_key_fd: Option<i32>,
    #[serde(default)]
    pub ssh_key_content: Option<String>,
    #[serde(default)]
    pub use_sudo: bool,
    pub command: String,
    #[serde(default)]
    pub pty_cols: Option<u16>,
    #[serde(default)]
    pub pty_rows: Option<u16>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ExecutionRequest {
    pub protocol_version: String,
    #[serde(default = "default_frame_type")]
    pub frame_type: FrameType,
    pub job_id: String,
    pub operation_id: String,
    pub attempt_id: String,
    pub ownership_epoch: i64,
    pub resource_uuid: String,
    pub team_id: i64,
    pub deadline_ms: u64,
    #[serde(default = "default_max_frame")]
    pub max_frame_bytes: usize,
    pub expected_capability: String,
    pub operation: OperationSpec,
    #[serde(default)]
    pub stdin_data: Option<String>,
}

fn default_frame_type() -> FrameType {
    FrameType::Start
}

fn default_max_frame() -> usize {
    256 * 1024
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum EventType {
    Started,
    Output,
    Exited,
    Error,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ExecutionEvent {
    pub protocol_version: String,
    pub event_type: EventType,
    pub operation_id: String,
    pub attempt_id: String,
    pub sequence: u64,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub stream: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub encoding: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub data: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub exit_code: Option<i32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub error_code: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub message: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize, PartialEq, Eq)]
#[serde(rename_all = "snake_case")]
pub enum RemoteState {
    Started,
    Running,
    Succeeded,
    Failed,
    Cancelled,
    Unknown,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct RemoteJournal {
    pub protocol_version: String,
    pub job_id: String,
    pub operation_id: String,
    pub attempt_id: String,
    pub ownership_epoch: i64,
    pub resource_uuid: String,
    pub input_fingerprint: String,
    pub state: RemoteState,
    pub boot_id: String,
    #[serde(default)]
    pub pid: Option<u32>,
    #[serde(default)]
    pub container_ids: Vec<String>,
    #[serde(default)]
    pub exit_code: Option<i32>,
    pub updated_at: String,
}

pub fn is_valid_opaque_id(id: &str) -> bool {
    !id.is_empty()
        && id.len() <= 128
        && id
            .chars()
            .all(|c| c.is_ascii_alphanumeric() || c == '_' || c == '-')
}

pub fn parse_ndjson_request(line: &str) -> Result<ExecutionRequest, ProtocolError> {
    if line.len() > MAX_FRAME_BYTES {
        return Err(ProtocolError::FrameTooLarge {
            size: line.len(),
            max: MAX_FRAME_BYTES,
        });
    }
    let req: ExecutionRequest = serde_json::from_str(line)?;
    if req.protocol_version != PROTOCOL_VERSION_V1 {
        return Err(ProtocolError::UnsupportedVersion(req.protocol_version));
    }
    if req.max_frame_bytes == 0 || req.max_frame_bytes > MAX_FRAME_BYTES {
        return Err(ProtocolError::FrameTooLarge {
            size: req.max_frame_bytes,
            max: MAX_FRAME_BYTES,
        });
    }
    if !is_valid_opaque_id(&req.operation_id) {
        return Err(ProtocolError::InvalidOpaqueId(req.operation_id));
    }
    if !is_valid_opaque_id(&req.attempt_id) {
        return Err(ProtocolError::InvalidOpaqueId(req.attempt_id));
    }
    if req.operation.command.contains("BEGIN OPENSSH PRIVATE KEY")
        || req.operation.command.contains("BEGIN RSA PRIVATE KEY")
    {
        return Err(ProtocolError::SecretInCommand);
    }
    Ok(req)
}

pub fn redact_secrets(text: &str, secrets: &[&str]) -> String {
    let mut out = text.to_string();
    for s in secrets {
        if !s.is_empty() {
            out = out.replace(s, "[REDACTED]");
        }
    }
    out
}
