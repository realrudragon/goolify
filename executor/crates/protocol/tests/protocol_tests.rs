use coolify_protocol::{
    parse_ndjson_request, redact_secrets, EventType, ExecutionEvent, ProtocolError, RemoteJournal,
    RemoteState,
};

#[test]
fn rejects_unknown_protocol_version() {
    let raw = r#"{"protocol_version":"v99","job_id":"j1","operation_id":"op-1","attempt_id":"att-1","ownership_epoch":1,"resource_uuid":"app-1","team_id":10,"deadline_ms":5000,"expected_capability":"ssh_exec","operation":{"kind":"exec","host":"10.0.0.1","port":22,"user":"root","command":"echo ok"}}"#;
    let err = parse_ndjson_request(raw).unwrap_err();
    assert!(matches!(err, ProtocolError::UnsupportedVersion(_)));
}

#[test]
fn rejects_path_traversal_operation_id() {
    let raw = r#"{"protocol_version":"v1","job_id":"j1","operation_id":"../etc/passwd","attempt_id":"att-1","ownership_epoch":1,"resource_uuid":"app-1","team_id":10,"deadline_ms":5000,"expected_capability":"ssh_exec","operation":{"kind":"exec","host":"10.0.0.1","port":22,"user":"root","command":"echo ok"}}"#;
    let err = parse_ndjson_request(raw).unwrap_err();
    assert!(matches!(err, ProtocolError::InvalidOpaqueId(_)));
}

#[test]
fn rejects_private_key_in_command_and_redacts_secrets() {
    let raw = r#"{"protocol_version":"v1","job_id":"j1","operation_id":"op-1","attempt_id":"att-1","ownership_epoch":1,"resource_uuid":"app-1","team_id":10,"deadline_ms":5000,"expected_capability":"ssh_exec","operation":{"kind":"exec","host":"10.0.0.1","port":22,"user":"root","command":"echo -----BEGIN OPENSSH PRIVATE KEY-----"}}"#;
    assert!(matches!(
        parse_ndjson_request(raw).unwrap_err(),
        ProtocolError::SecretInCommand
    ));
    assert_eq!(redact_secrets("token=abc123", &["abc123"]), "token=[REDACTED]");
}

#[test]
fn serializes_event_and_remote_journal() {
    let ev = ExecutionEvent {
        protocol_version: "v1".into(),
        event_type: EventType::Started,
        operation_id: "op-1".into(),
        attempt_id: "att-1".into(),
        sequence: 1,
        stream: None,
        encoding: None,
        data: None,
        exit_code: None,
        error_code: None,
        message: None,
    };
    let line = serde_json::to_string(&ev).unwrap();
    assert!(line.contains("\"event_type\":\"started\""));

    let journal = RemoteJournal {
        protocol_version: "v1".into(),
        job_id: "job-1".into(),
        operation_id: "op-1".into(),
        attempt_id: "att-1".into(),
        ownership_epoch: 1,
        resource_uuid: "app-1".into(),
        input_fingerprint: "sha256:abc".into(),
        state: RemoteState::Running,
        boot_id: "boot-xyz".into(),
        pid: Some(1234),
        container_ids: vec!["c1".into()],
        exit_code: None,
        updated_at: "2026-11-19T00:00:00Z".into(),
    };
    let jline = serde_json::to_string(&journal).unwrap();
    assert!(jline.contains("\"state\":\"running\""));
}
