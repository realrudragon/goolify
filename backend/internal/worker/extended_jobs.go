package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/domains/backups"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func buildExtendedExecutionCommand(spec jobs.DurableJobSpec) (string, string, bool) {
	switch spec.JobType {
	case "volume_backup":
		volName, _ := spec.Payload["volume_name"].(string)
		mountPath, _ := spec.Payload["mount_path"].(string)
		execUUID, _ := spec.Payload["execution_uuid"].(string)
		safeVol := strings.ReplaceAll(volName, "'", "")
		if safeVol == "" {
			safeVol = strings.ReplaceAll(spec.ResourceUUID, "'", "")
		}
		safeExec := strings.ReplaceAll(execUUID, "'", "")
		cmd := fmt.Sprintf(
			"mkdir -p /data/coolify/backups/volumes && tar czf '/data/coolify/backups/volumes/%s-%s.tar.gz' -C '/' '%s'",
			safeVol, safeExec, strings.TrimPrefix(strings.ReplaceAll(mountPath, "'", ""), "/"),
		)
		return "ssh_exec", cmd, true

	case "scheduled_task":
		commandStr, _ := spec.Payload["command"].(string)
		containerStr, _ := spec.Payload["container"].(string)
		useShell, _ := spec.Payload["use_shell"].(bool)
		if strings.TrimSpace(commandStr) == "" {
			return "ssh_exec", "echo empty-scheduled-task-command && exit 1", true
		}
		if containerStr != "" {
			safeCont := strings.ReplaceAll(containerStr, "'", "")
			safeCmd := strings.ReplaceAll(commandStr, "'", "'\"'\"'")
			if useShell {
				return "ssh_exec", fmt.Sprintf("docker exec '%s' sh -c '%s'", safeCont, safeCmd), true
			}
			return "ssh_exec", fmt.Sprintf("docker exec '%s' %s", safeCont, commandStr), true
		}
		return "ssh_exec", commandStr, true

	case "docker_cleanup":
		delVols, _ := spec.Payload["delete_unused_volumes"].(bool)
		cmd := "docker system prune -af"
		if delVols {
			cmd = "docker system prune -af --volumes"
		}
		return "ssh_exec", cmd, true

	case "db_import":
		fpath, _ := spec.Payload["filepath"].(string)
		safePath := strings.ReplaceAll(fpath, "'", "")
		safeRes := strings.ReplaceAll(spec.ResourceUUID, "'", "")
		return "ssh_exec", fmt.Sprintf("test -f '%s' && docker exec -i 'coolify-db-%s' sh -c 'cat >/tmp/restore.sql' < '%s'", safePath, safeRes, safePath), true

	case "github_runner_provision":
		runnerName, _ := spec.Payload["runner_name"].(string)
		runnerImage, _ := spec.Payload["runner_image"].(string)
		if runnerName == "" {
			runnerName = "coolify-gh-runner-" + spec.ResourceUUID
		}
		if runnerImage == "" {
			runnerImage = "myoung34/github-runner:latest"
		}
		safeName := strings.ReplaceAll(runnerName, "'", "")
		safeImg := strings.ReplaceAll(runnerImage, "'", "")
		return "ssh_exec", fmt.Sprintf("docker run -d --rm --name '%s' '%s'", safeName, safeImg), true
	}
	return "", "", false
}

func finalizeExtendedJobInTx(
	ctx context.Context,
	tx pgx.Tx,
	spec jobs.DurableJobSpec,
	status jobs.JobStatus,
	tail string,
	errMsg string,
) (resType, mutType string, handled bool) {
	execState := "failed"
	if status == jobs.StatusSucceeded {
		execState = "success"
	} else if status == jobs.StatusUnknown {
		execState = "running"
	}

	msgText := tail
	if errMsg != "" {
		msgText = errMsg
	}

	switch spec.JobType {
	case "volume_backup":
		execUUID, _ := spec.Payload["execution_uuid"].(string)
		if execUUID != "" {
			var execID, schedID int64
			_ = tx.QueryRow(ctx, `
				UPDATE scheduled_volume_backup_executions
				SET status = $1, message = $2, finished_at = CASE WHEN $1 IN ('success','failed') THEN NOW() ELSE finished_at END, updated_at = NOW()
				WHERE uuid = $3
				RETURNING id, scheduled_volume_backup_id
			`, execState, msgText, execUUID).Scan(&execID, &schedID)
			if execID > 0 && schedID > 0 {
				uploaded := status == jobs.StatusSucceeded
				_, _ = tx.Exec(ctx, `
					INSERT INTO volume_backup_s3_replicas (execution_id, s3_storage_id, s3_uploaded, s3_storage_deleted, message)
					SELECT $1, s3_storage_id, $2, FALSE, $3
					FROM scheduled_volume_backup_s3_storage
					WHERE scheduled_volume_backup_id = $4
				`, execID, uploaded, msgText, schedID)
			}
		}
		return "storage", "volume_backup." + execState, true

	case "scheduled_task":
		execUUID, _ := spec.Payload["execution_uuid"].(string)
		taskUUID, _ := spec.Payload["task_uuid"].(string)
		if execUUID != "" {
			_, _ = tx.Exec(ctx, `
				UPDATE scheduled_task_executions
				SET status = $1, message = $2, finished_at = CASE WHEN $1 IN ('success','failed') THEN NOW() ELSE finished_at END, updated_at = NOW()
				WHERE uuid = $3
			`, execState, msgText, execUUID)
		}
		if taskUUID != "" {
			if status == jobs.StatusSucceeded {
				_, _ = tx.Exec(ctx, `UPDATE scheduled_tasks SET consecutive_failures = 0, failing_since = NULL, updated_at = NOW() WHERE uuid = $1`, taskUUID)
			} else if status == jobs.StatusFailed || status == jobs.StatusDead {
				_, _ = tx.Exec(ctx, `UPDATE scheduled_tasks SET consecutive_failures = consecutive_failures + 1, failing_since = COALESCE(failing_since, NOW()), updated_at = NOW() WHERE uuid = $1`, taskUUID)
			}
		}
		return "scheduled_task", "scheduled_task." + execState, true

	case "docker_cleanup":
		execUUID, _ := spec.Payload["execution_uuid"].(string)
		if execUUID != "" {
			rawLog, _ := json.Marshal(map[string]any{"output": msgText})
			_, _ = tx.Exec(ctx, `
				UPDATE docker_cleanup_executions
				SET status = $1, message = $2, cleanup_log = $3::jsonb, finished_at = CASE WHEN $1 IN ('success','failed') THEN NOW() ELSE finished_at END, updated_at = NOW()
				WHERE uuid = $4
			`, execState, msgText, string(rawLog), execUUID)
		}
		return "server", "server.docker_cleanup." + execState, true

	case "db_import":
		if actID, ok := spec.Payload["activity_id"]; ok {
			_, _ = tx.Exec(ctx, `
				UPDATE go_upload_imports SET status = $1, log = $2, updated_at = NOW() WHERE id::text = $3
			`, execState, msgText, fmt.Sprint(actID))
		}
		return "database", "database.import." + execState, true

	case "github_runner_provision":
		execUUID, _ := spec.Payload["execution_uuid"].(string)
		if execUUID != "" {
			runnerStatus := "failed"
			conclusion := "failure"
			if status == jobs.StatusSucceeded {
				runnerStatus = "completed"
				conclusion = "success"
			} else if status == jobs.StatusUnknown {
				runnerStatus = "running"
				conclusion = ""
			}
			_, _ = tx.Exec(ctx, `
				UPDATE github_runner_executions
				SET status = $1, conclusion = NULLIF($2, ''), error_message = NULLIF($3, ''),
				    started_at = COALESCE(started_at, NOW()),
				    completed_at = CASE WHEN $1 IN ('completed','failed') THEN NOW() ELSE completed_at END,
				    provision_attempts = provision_attempts + 1, updated_at = NOW()
				WHERE uuid = $4
			`, runnerStatus, conclusion, errMsg, execUUID)
		}
		return "server", "github_runner." + execState, true

	case "proxy_start", "proxy_restart":
		proxyState := "exited"
		if status == jobs.StatusSucceeded {
			proxyState = "running"
		}
		patch, _ := json.Marshal(map[string]any{"status": proxyState, "force_stop": false})
		_, _ = tx.Exec(ctx, `UPDATE servers SET proxy = COALESCE(proxy,'{}'::jsonb) || $1::jsonb, updated_at = NOW() WHERE uuid = $2`, string(patch), spec.ServerUUID)
		return "server", "server.proxy_status_changed", true

	case "proxy_stop":
		patch, _ := json.Marshal(map[string]any{"status": "exited", "force_stop": true})
		_, _ = tx.Exec(ctx, `UPDATE servers SET proxy = COALESCE(proxy,'{}'::jsonb) || $1::jsonb, updated_at = NOW() WHERE uuid = $2`, string(patch), spec.ServerUUID)
		return "server", "server.proxy_status_changed", true
	}
	return "", "", false
}

// recordScheduledJobDelivery atomically persists scheduler progress in `scheduled_job_states`
// and deduplicates delivery windows in `scheduled_job_deliveries` using UNIQUE(schedule_key, scheduled_for).
// Returns won=true only for the single worker that claims this schedule slot.
func recordScheduledJobDelivery(
	ctx context.Context,
	pool *pgxpool.Pool,
	scheduleKey string,
	scheduledFor time.Time,
	jobType string,
	resourceID int64,
	claimToken string,
	payload map[string]any,
) (bool, error) {
	if pool == nil {
		return false, nil
	}
	slot := scheduledFor.UTC().Truncate(time.Minute)
	rawPayload, _ := json.Marshal(payload)
	if len(rawPayload) == 0 || string(rawPayload) == "null" {
		rawPayload = []byte("{}")
	}
	deliveryUUID := fmt.Sprintf("sjd-%s-%d", strings.ReplaceAll(scheduleKey, ":", "-"), slot.Unix())
	stateUUID := "sjs-" + strings.ReplaceAll(scheduleKey, ":", "-")

	var insertedID int64
	err := pool.QueryRow(ctx, `
		INSERT INTO scheduled_job_deliveries (
			uuid, schedule_key, scheduled_for, job_type, resource_id, payload, status, claim_token, enqueued_at
		) VALUES ($1, $2, $3, $4, $5, $6::jsonb, 'enqueued', $7, NOW())
		ON CONFLICT (schedule_key, scheduled_for) DO NOTHING
		RETURNING id
	`, deliveryUUID, scheduleKey, slot, jobType, resourceID, string(rawPayload), claimToken).Scan(&insertedID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	_, _ = pool.Exec(ctx, `
		INSERT INTO scheduled_job_states (uuid, schedule_key, last_scheduled_for, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (schedule_key) DO UPDATE SET last_scheduled_for = EXCLUDED.last_scheduled_for, updated_at = NOW()
	`, stateUUID, scheduleKey, slot)
	return true, nil
}

// tickExtendedSchedules scans enabled `scheduled_tasks` and `scheduled_volume_backups`
// and enqueues idempotent jobs via `scheduled_job_deliveries` + `scheduled_job_states`.
func (r *Runner) tickExtendedSchedules(ctx context.Context, now time.Time) (int, error) {
	if r.cfg.Pool == nil || r.cfg.JobCoord == nil {
		return 0, nil
	}
	enqueued := 0

	// 1. Scheduled Tasks
	taskRows, err := r.cfg.Pool.Query(ctx, `
		SELECT id, uuid, name, command, frequency, COALESCE(container,''), use_shell, team_id
		FROM scheduled_tasks
		WHERE enabled = TRUE
		ORDER BY id ASC
	`)
	if err == nil {
		type taskDue struct {
			id, teamID              int64
			uuid, name, cmd, freq, container string
			useShell                bool
		}
		var tasks []taskDue
		for taskRows.Next() {
			var t taskDue
			if err := taskRows.Scan(&t.id, &t.uuid, &t.name, &t.cmd, &t.freq, &t.container, &t.useShell, &t.teamID); err == nil {
				if cronDueAt(t.freq, now, "UTC") {
					tasks = append(tasks, t)
				}
			}
		}
		taskRows.Close()
		for _, t := range tasks {
			schedKey := fmt.Sprintf("scheduled_task:%d", t.id)
			execUUID := fmt.Sprintf("ste-%d-%s", t.id, now.UTC().Format("20060102T1504"))
			payload := map[string]any{
				"task_id":        t.id,
				"task_uuid":      t.uuid,
				"execution_uuid": execUUID,
				"command":        t.cmd,
				"container":      t.container,
				"use_shell":      t.useShell,
			}
			won, err := recordScheduledJobDelivery(ctx, r.cfg.Pool, schedKey, now, "scheduled_task", t.id, r.cfg.WorkerID, payload)
			if err != nil || !won {
				continue
			}
			_, _ = r.cfg.Pool.Exec(ctx, `
				INSERT INTO scheduled_task_executions (uuid, status, scheduled_task_id, started_at, worker_host)
				VALUES ($1, 'running', $2, NOW(), $3)
				ON CONFLICT (uuid) DO NOTHING
			`, execUUID, t.id, r.cfg.WorkerID)
			opID := "sched-task-" + execUUID
			if err := r.cfg.JobCoord.AdmitOperation(ctx, jobs.DurableJobSpec{
				JobOperation: jobs.JobOperation{
					JobID: "job-" + opID, OperationID: opID, AttemptID: "att-0",
					ResourceUUID: t.uuid, ServerUUID: "localhost", TeamID: t.teamID, Status: jobs.StatusQueued, MaxAttempts: 1,
				},
				JobType: "scheduled_task",
				Payload: payload,
			}); err == nil {
				enqueued++
			}
		}
	}

	// 2. Scheduled Volume Backups
	volRows, err := r.cfg.Pool.Query(ctx, `
		SELECT id, uuid, frequency, team_id, backupable_id
		FROM scheduled_volume_backups
		WHERE enabled = TRUE
		ORDER BY id ASC
	`)
	if err == nil {
		type volDue struct {
			id, teamID, backupableID int64
			uuid, freq               string
		}
		var vols []volDue
		for volRows.Next() {
			var v volDue
			if err := volRows.Scan(&v.id, &v.uuid, &v.freq, &v.teamID, &v.backupableID); err == nil {
				if cronDueAt(v.freq, now, "UTC") {
					vols = append(vols, v)
				}
			}
		}
		volRows.Close()
		for _, v := range vols {
			schedKey := fmt.Sprintf("volume_backup:%d", v.id)
			execUUID := fmt.Sprintf("vbe-%d-%s", v.id, now.UTC().Format("20060102T1504"))
			payload := map[string]any{
				"schedule_id":    v.id,
				"schedule_uuid":  v.uuid,
				"execution_uuid": execUUID,
				"volume_name":    v.uuid,
				"mount_path":     "/data",
			}
			won, err := recordScheduledJobDelivery(ctx, r.cfg.Pool, schedKey, now, "volume_backup", v.id, r.cfg.WorkerID, payload)
			if err != nil || !won {
				continue
			}
			_, _ = r.cfg.Pool.Exec(ctx, `
				INSERT INTO scheduled_volume_backup_executions (uuid, scheduled_volume_backup_id, status, worker_host)
				VALUES ($1, $2, 'running', $3)
				ON CONFLICT (uuid) DO NOTHING
			`, execUUID, v.id, r.cfg.WorkerID)
			opID := "sched-vol-" + execUUID
			if err := r.cfg.JobCoord.AdmitOperation(ctx, jobs.DurableJobSpec{
				JobOperation: jobs.JobOperation{
					JobID: "job-" + opID, OperationID: opID, AttemptID: "att-0",
					ResourceUUID: v.uuid, ServerUUID: "localhost", TeamID: v.teamID, Status: jobs.StatusQueued, MaxAttempts: 1,
				},
				JobType: "volume_backup",
				Payload: payload,
			}); err == nil {
				enqueued++
			}
		}
	}
	return enqueued, nil
}

// replicateDatabaseBackupS3Replicas uploads a completed database backup file to all
// attached S3 targets in `scheduled_database_backup_s3_storage` and persists rows in
// `database_backup_s3_replicas`.
func (r *Runner) replicateDatabaseBackupS3Replicas(
	ctx context.Context,
	schedID, teamID, execID int64,
	filename string,
	primaryS3ID int64,
) {
	if r.cfg.Pool == nil || schedID <= 0 || execID <= 0 || filename == "" {
		return
	}
	rows, err := r.cfg.Pool.Query(ctx, `
		SELECT s.id, s.endpoint, s.bucket, s.region, s.key, s.secret
		FROM scheduled_database_backup_s3_storage j
		JOIN s3_storages s ON s.id = j.s3_storage_id
		WHERE j.scheduled_database_backup_id = $1 AND s.team_id = $2
		ORDER BY s.id ASC
	`, schedID, teamID)
	if err != nil {
		return
	}
	defer rows.Close()

	type s3Row struct {
		id                               int64
		ep, bucket, region, kEnc, secEnc string
	}
	var targets []s3Row
	for rows.Next() {
		var t s3Row
		if err := rows.Scan(&t.id, &t.ep, &t.bucket, &t.region, &t.kEnc, &t.secEnc); err == nil {
			targets = append(targets, t)
		}
	}
	if len(targets) == 0 {
		return
	}
	data, readErr := os.ReadFile(filename)
	s3Client := &backups.S3HTTPClient{}
	objKey := "backups/" + filepath.Base(filename)

	for _, t := range targets {
		if t.id == primaryS3ID {
			// Primary S3 target was already uploaded by RunRealBackup; record its replica row cleanly.
			_, _ = r.cfg.Pool.Exec(ctx, `
				INSERT INTO database_backup_s3_replicas (execution_id, s3_storage_id, s3_uploaded, s3_storage_deleted, message)
				VALUES ($1, $2, TRUE, FALSE, 'uploaded via primary S3 target')
			`, execID, t.id)
			continue
		}
		if readErr != nil {
			_, _ = r.cfg.Pool.Exec(ctx, `
				INSERT INTO database_backup_s3_replicas (execution_id, s3_storage_id, s3_uploaded, s3_storage_deleted, message)
				VALUES ($1, $2, FALSE, FALSE, $3)
			`, execID, t.id, readErr.Error())
			continue
		}
		putErr := s3Client.PutObject(ctx, backups.S3Target{
			Endpoint:  t.ep,
			Bucket:    t.bucket,
			Region:    t.region,
			AccessKey: decryptPayloadOrPlain(r.cfg.Cipher, t.kEnc),
			SecretKey: decryptPayloadOrPlain(r.cfg.Cipher, t.secEnc),
		}, objKey, data)
		if putErr != nil {
			_, _ = r.cfg.Pool.Exec(ctx, `
				INSERT INTO database_backup_s3_replicas (execution_id, s3_storage_id, s3_uploaded, s3_storage_deleted, message)
				VALUES ($1, $2, FALSE, FALSE, $3)
			`, execID, t.id, putErr.Error())
		} else {
			_, _ = r.cfg.Pool.Exec(ctx, `
				INSERT INTO database_backup_s3_replicas (execution_id, s3_storage_id, s3_uploaded, s3_storage_deleted, message)
				VALUES ($1, $2, TRUE, FALSE, $3)
			`, execID, t.id, "replicated "+objKey)
		}
	}
}
