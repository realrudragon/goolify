package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DurableJobSpec represents a queued or active job persisted in PostgreSQL `go_job_operations`.
type DurableJobSpec struct {
	JobOperation
	JobType string         `json:"job_type"` // deploy | service_deploy | db_provision | backup | restore | notify
	Payload map[string]any `json:"payload,omitempty"`
}

// PostgresJobCoordinator persists durable job admission, fencing epochs, and lease ownership in PostgreSQL.
type PostgresJobCoordinator struct {
	pool      *pgxpool.Pool
	serverMax int
}

func NewPostgresJobCoordinator(pool *pgxpool.Pool, serverConcurrencyLimit int) *PostgresJobCoordinator {
	if serverConcurrencyLimit <= 0 {
		serverConcurrencyLimit = 4
	}
	return &PostgresJobCoordinator{
		pool:      pool,
		serverMax: serverConcurrencyLimit,
	}
}

// AdmitOperation idempotently persists a job operation in PostgreSQL.
func (c *PostgresJobCoordinator) AdmitOperation(ctx context.Context, spec DurableJobSpec) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := c.AdmitOperationInTx(ctx, tx, spec); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (c *PostgresJobCoordinator) AdmitOperationInTx(ctx context.Context, tx pgx.Tx, spec DurableJobSpec) error {
	var existingStatus string
	err := tx.QueryRow(ctx, `
		SELECT status FROM go_job_operations WHERE operation_id = $1 FOR UPDATE
	`, spec.OperationID).Scan(&existingStatus)
	if err == nil {
		if JobStatus(existingStatus) == StatusSucceeded {
			return ErrAlreadyCompleted
		}
		return nil // idempotent admission for same operation_id
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	var activeCount int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM go_job_operations
		WHERE server_uuid = $1 AND status = 'running' AND lease_until > NOW()
	`, spec.ServerUUID).Scan(&activeCount); err != nil {
		return err
	}
	if activeCount >= c.serverMax {
		return ErrAdmissionCapReached
	}

	if spec.MaxAttempts <= 0 {
		spec.MaxAttempts = 3
	}
	if spec.Status == "" {
		spec.Status = StatusQueued
	}
	if spec.JobType == "" {
		spec.JobType = "deploy"
	}
	payloadRaw, err := json.Marshal(spec.Payload)
	if err != nil {
		return err
	}
	if len(payloadRaw) == 0 || string(payloadRaw) == "null" {
		payloadRaw = []byte("{}")
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO go_job_operations (
			operation_id, job_id, attempt_id, job_type, ownership_epoch,
			resource_uuid, server_uuid, team_id, status, payload_json,
			attempts, max_attempts, updated_at
		) VALUES ($1,$2,$3,$4,0,$5,$6,$7,$8,$9::jsonb,0,$10,NOW())
	`, spec.OperationID, spec.JobID, spec.AttemptID, spec.JobType,
		spec.ResourceUUID, spec.ServerUUID, spec.TeamID, string(spec.Status),
		string(payloadRaw), spec.MaxAttempts)
	return err
}

// AcquireLease atomically checks/bumps the ownership_epoch in PostgreSQL so stale workers are fenced out.
func (c *PostgresJobCoordinator) AcquireLease(ctx context.Context, operationID string, workerID string, requestedEpoch int64, ttl time.Duration) (JobLease, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return JobLease{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var curEpoch int64
	var curStatus, curWorker string
	var curLease *time.Time
	err = tx.QueryRow(ctx, `
		SELECT ownership_epoch, status, worker_id, lease_until
		FROM go_job_operations
		WHERE operation_id = $1
		FOR UPDATE
	`, operationID).Scan(&curEpoch, &curStatus, &curWorker, &curLease)
	if errors.Is(err, pgx.ErrNoRows) {
		return JobLease{}, errors.New("operation not found")
	}
	if err != nil {
		return JobLease{}, err
	}
	if JobStatus(curStatus) == StatusSucceeded {
		return JobLease{}, ErrAlreadyCompleted
	}
	ttlMs := ttl.Milliseconds()
	if ttlMs <= 0 {
		ttlMs = 1000
	}
	var dbNow time.Time
	if err := tx.QueryRow(ctx, `SELECT NOW()`).Scan(&dbNow); err != nil {
		return JobLease{}, err
	}
	if requestedEpoch <= curEpoch && curLease != nil && curLease.After(dbNow) && curWorker != workerID {
		return JobLease{}, ErrLeaseHeldByOtherEpoch
	}
	newEpoch := requestedEpoch
	if newEpoch <= curEpoch {
		newEpoch = curEpoch + 1
	}
	attemptID := fmt.Sprintf("att-%d", newEpoch)
	var expiresAt time.Time
	err = tx.QueryRow(ctx, `
		UPDATE go_job_operations
		SET ownership_epoch = $2,
		    attempt_id = $3,
		    worker_id = $4,
		    lease_until = NOW() + ($5 * INTERVAL '1 millisecond'),
		    attempts = attempts + 1,
		    status = 'running',
		    updated_at = NOW()
		WHERE operation_id = $1
		RETURNING lease_until
	`, operationID, newEpoch, attemptID, workerID, ttlMs).Scan(&expiresAt)
	if err != nil {
		return JobLease{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return JobLease{}, err
	}
	return JobLease{
		OperationID:    operationID,
		OwnershipEpoch: newEpoch,
		WorkerID:       workerID,
		ExpiresAt:      expiresAt,
	}, nil
}

// ClaimNext atomically claims one queued job (or expired-lease running job for reconciliation) using SKIP LOCKED.
func (c *PostgresJobCoordinator) ClaimNext(ctx context.Context, workerID string, ttl time.Duration) (*DurableJobSpec, JobLease, bool, error) {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return nil, JobLease{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var spec DurableJobSpec
	var statusStr string
	var payloadRaw []byte
	var leaseUntil *time.Time
	err = tx.QueryRow(ctx, `
		SELECT operation_id, job_id, attempt_id, job_type, ownership_epoch,
		       resource_uuid, server_uuid, team_id, status, payload_json,
		       attempts, max_attempts, lease_until, worker_id, last_error, updated_at
		FROM go_job_operations
		WHERE status = 'queued'
		   OR (status = 'running' AND lease_until IS NOT NULL AND lease_until < NOW())
		ORDER BY created_at ASC
		FOR UPDATE SKIP LOCKED
		LIMIT 1
	`).Scan(
		&spec.OperationID, &spec.JobID, &spec.AttemptID, &spec.JobType, &spec.OwnershipEpoch,
		&spec.ResourceUUID, &spec.ServerUUID, &spec.TeamID, &statusStr, &payloadRaw,
		&spec.Attempts, &spec.MaxAttempts, &leaseUntil, &spec.WorkerID, &spec.LastError, &spec.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, JobLease{}, false, nil
	}
	if err != nil {
		return nil, JobLease{}, false, err
	}
	spec.Status = JobStatus(statusStr)
	if len(payloadRaw) > 0 {
		_ = json.Unmarshal(payloadRaw, &spec.Payload)
	}

	// Enforce per-server concurrency cap excluding this operation
	var serverActive int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM go_job_operations
		WHERE server_uuid = $1 AND operation_id <> $2 AND status = 'running' AND lease_until > NOW()
	`, spec.ServerUUID, spec.OperationID).Scan(&serverActive); err != nil {
		return nil, JobLease{}, false, err
	}
	if serverActive >= c.serverMax {
		return nil, JobLease{}, false, nil
	}

	wasExpired := spec.Status == StatusRunning
	newEpoch := spec.OwnershipEpoch + 1
	ttlMs := ttl.Milliseconds()
	if ttlMs <= 0 {
		ttlMs = 1000
	}
	newAttempt := fmt.Sprintf("att-%d", newEpoch)

	var expiresAt time.Time
	err = tx.QueryRow(ctx, `
		UPDATE go_job_operations
		SET ownership_epoch = $2,
		    attempt_id = $3,
		    worker_id = $4,
		    lease_until = NOW() + ($5 * INTERVAL '1 millisecond'),
		    attempts = attempts + 1,
		    status = 'running',
		    updated_at = NOW()
		WHERE operation_id = $1
		RETURNING lease_until
	`, spec.OperationID, newEpoch, newAttempt, workerID, ttlMs).Scan(&expiresAt)
	if err != nil {
		return nil, JobLease{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, JobLease{}, false, err
	}

	spec.OwnershipEpoch = newEpoch
	spec.AttemptID = newAttempt
	spec.WorkerID = workerID
	spec.LeaseUntil = expiresAt
	spec.Attempts++
	if wasExpired {
		// Preserve prior running status signal in Payload so worker reconciles before re-executing!
		if spec.Payload == nil {
			spec.Payload = make(map[string]any)
		}
		spec.Payload["_reclaimed_after_lease_expiry"] = true
	}
	spec.Status = StatusRunning

	return &spec, JobLease{
		OperationID:    spec.OperationID,
		OwnershipEpoch: newEpoch,
		WorkerID:       workerID,
		ExpiresAt:      expiresAt,
	}, true, nil
}

// RenewLease extends an active lease ONLY IF ownership_epoch matches (fencing).
func (c *PostgresJobCoordinator) RenewLease(ctx context.Context, operationID string, epoch int64, workerID string, ttl time.Duration) error {
	ttlMs := ttl.Milliseconds()
	if ttlMs <= 0 {
		ttlMs = 1000
	}
	tag, err := c.pool.Exec(ctx, `
		UPDATE go_job_operations
		SET lease_until = NOW() + ($4 * INTERVAL '1 millisecond'), updated_at = NOW()
		WHERE operation_id = $1 AND ownership_epoch = $2 AND worker_id = $3 AND status = 'running'
	`, operationID, epoch, workerID, ttlMs)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseHeldByOtherEpoch
	}
	return nil
}

// MarkTerminal updates operation status ONLY IF the caller still holds the latest ownership_epoch.
func (c *PostgresJobCoordinator) MarkTerminal(ctx context.Context, operationID string, epoch int64, status JobStatus, errMsg string) error {
	tx, err := c.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var curEpoch int64
	var attempts, maxAttempts int
	err = tx.QueryRow(ctx, `
		SELECT ownership_epoch, attempts, max_attempts
		FROM go_job_operations
		WHERE operation_id = $1
		FOR UPDATE
	`, operationID).Scan(&curEpoch, &attempts, &maxAttempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("operation not found")
	}
	if err != nil {
		return err
	}
	if epoch < curEpoch {
		return ErrLeaseHeldByOtherEpoch
	}
	finalStatus := status
	if finalStatus == StatusFailed && attempts >= maxAttempts {
		finalStatus = StatusDead
	}
	_, err = tx.Exec(ctx, `
		UPDATE go_job_operations
		SET status = $2, last_error = $3, lease_until = NULL, updated_at = NOW()
		WHERE operation_id = $1
	`, operationID, string(finalStatus), errMsg)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (c *PostgresJobCoordinator) Get(ctx context.Context, operationID string) (DurableJobSpec, bool, error) {
	var spec DurableJobSpec
	var statusStr string
	var payloadRaw []byte
	var leaseUntil *time.Time
	err := c.pool.QueryRow(ctx, `
		SELECT operation_id, job_id, attempt_id, job_type, ownership_epoch,
		       resource_uuid, server_uuid, team_id, status, payload_json,
		       attempts, max_attempts, lease_until, worker_id, last_error, updated_at
		FROM go_job_operations
		WHERE operation_id = $1
	`, operationID).Scan(
		&spec.OperationID, &spec.JobID, &spec.AttemptID, &spec.JobType, &spec.OwnershipEpoch,
		&spec.ResourceUUID, &spec.ServerUUID, &spec.TeamID, &statusStr, &payloadRaw,
		&spec.Attempts, &spec.MaxAttempts, &leaseUntil, &spec.WorkerID, &spec.LastError, &spec.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return DurableJobSpec{}, false, nil
	}
	if err != nil {
		return DurableJobSpec{}, false, err
	}
	spec.Status = JobStatus(statusStr)
	if leaseUntil != nil {
		spec.LeaseUntil = *leaseUntil
	}
	if len(payloadRaw) > 0 {
		_ = json.Unmarshal(payloadRaw, &spec.Payload)
	}
	return spec, true, nil
}

func (c *PostgresJobCoordinator) ListByResource(ctx context.Context, resourceUUID string, limit int) ([]DurableJobSpec, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := c.pool.Query(ctx, `
		SELECT operation_id, job_id, attempt_id, job_type, ownership_epoch,
		       resource_uuid, server_uuid, team_id, status, payload_json,
		       attempts, max_attempts, lease_until, worker_id, last_error, updated_at
		FROM go_job_operations
		WHERE resource_uuid = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, resourceUUID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DurableJobSpec
	for rows.Next() {
		var spec DurableJobSpec
		var statusStr string
		var payloadRaw []byte
		var leaseUntil *time.Time
		if err := rows.Scan(
			&spec.OperationID, &spec.JobID, &spec.AttemptID, &spec.JobType, &spec.OwnershipEpoch,
			&spec.ResourceUUID, &spec.ServerUUID, &spec.TeamID, &statusStr, &payloadRaw,
			&spec.Attempts, &spec.MaxAttempts, &leaseUntil, &spec.WorkerID, &spec.LastError, &spec.UpdatedAt,
		); err != nil {
			return nil, err
		}
		spec.Status = JobStatus(statusStr)
		if leaseUntil != nil {
			spec.LeaseUntil = *leaseUntil
		}
		if len(payloadRaw) > 0 {
			_ = json.Unmarshal(payloadRaw, &spec.Payload)
		}
		out = append(out, spec)
	}
	return out, rows.Err()
}
