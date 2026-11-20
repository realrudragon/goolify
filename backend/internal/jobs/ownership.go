package jobs

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrLeaseHeldByOtherEpoch = errors.New("job lease held by newer ownership epoch")
	ErrAlreadyCompleted      = errors.New("operation already completed")
	ErrAdmissionCapReached   = errors.New("server concurrency limit reached")
)

type JobStatus string

const (
	StatusQueued    JobStatus = "queued"
	StatusRunning   JobStatus = "running"
	StatusUnknown   JobStatus = "unknown"
	StatusSucceeded JobStatus = "succeeded"
	StatusFailed    JobStatus = "failed"
	StatusCancelled JobStatus = "cancelled"
	StatusDead      JobStatus = "dead_letter"
)

type JobOperation struct {
	JobID          string
	OperationID    string // Stable across transport retries!
	AttemptID      string
	OwnershipEpoch int64
	ResourceUUID   string
	ServerUUID     string
	TeamID         int64
	Status         JobStatus
	Attempts       int
	MaxAttempts    int
	LeaseUntil     time.Time
	WorkerID       string
	LastError      string
	UpdatedAt      time.Time
}

type JobLease struct {
	OperationID    string
	OwnershipEpoch int64
	WorkerID       string
	ExpiresAt      time.Time
}

// JobCoordinator persists durable job/operation ownership in PostgreSQL (never in Redis queue alone).
type JobCoordinator struct {
	mu           sync.RWMutex
	ops          map[string]*JobOperation
	serverActive map[string]int
	serverMax    int
	now          func() time.Time
}

func NewJobCoordinator(serverConcurrencyLimit int) *JobCoordinator {
	if serverConcurrencyLimit <= 0 {
		serverConcurrencyLimit = 2
	}
	return &JobCoordinator{
		ops:          make(map[string]*JobOperation),
		serverActive: make(map[string]int),
		serverMax:    serverConcurrencyLimit,
		now:          time.Now,
	}
}

func (c *JobCoordinator) AdmitOperation(_ context.Context, op JobOperation) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.ops[op.OperationID]; ok {
		if existing.Status == StatusSucceeded {
			return ErrAlreadyCompleted
		}
		return nil // idempotent admission for existing operation_id
	}
	if c.serverActive[op.ServerUUID] >= c.serverMax {
		return ErrAdmissionCapReached
	}
	if op.MaxAttempts <= 0 {
		op.MaxAttempts = 3
	}
	if op.Status == "" {
		op.Status = StatusQueued
	}
	op.UpdatedAt = c.now().UTC()
	cp := op
	c.ops[op.OperationID] = &cp
	return nil
}

func (c *JobCoordinator) AcquireLease(_ context.Context, operationID string, workerID string, epoch int64, ttl time.Duration) (JobLease, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	op, ok := c.ops[operationID]
	if !ok {
		return JobLease{}, errors.New("operation not found")
	}
	if op.Status == StatusSucceeded {
		return JobLease{}, ErrAlreadyCompleted
	}
	now := c.now().UTC()
	if epoch <= op.OwnershipEpoch && op.LeaseUntil.After(now) && op.WorkerID != workerID {
		return JobLease{}, ErrLeaseHeldByOtherEpoch
	}
	if epoch <= op.OwnershipEpoch {
		epoch = op.OwnershipEpoch + 1
	}
	op.OwnershipEpoch = epoch
	op.WorkerID = workerID
	op.LeaseUntil = now.Add(ttl)
	op.Attempts++
	if op.Status == StatusQueued {
		c.serverActive[op.ServerUUID]++
	}
	op.Status = StatusRunning
	op.UpdatedAt = now
	return JobLease{
		OperationID:    op.OperationID,
		OwnershipEpoch: op.OwnershipEpoch,
		WorkerID:       workerID,
		ExpiresAt:      op.LeaseUntil,
	}, nil
}

func (c *JobCoordinator) MarkTerminal(_ context.Context, operationID string, epoch int64, status JobStatus, errMsg string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	op, ok := c.ops[operationID]
	if !ok {
		return errors.New("operation not found")
	}
	if epoch < op.OwnershipEpoch {
		return ErrLeaseHeldByOtherEpoch
	}
	if op.Status == StatusRunning && status != StatusUnknown {
		if c.serverActive[op.ServerUUID] > 0 {
			c.serverActive[op.ServerUUID]--
		}
	}
	if status == StatusFailed && op.Attempts >= op.MaxAttempts {
		status = StatusDead
	}
	op.Status = status
	op.LastError = errMsg
	op.LeaseUntil = time.Time{}
	op.UpdatedAt = c.now().UTC()
	return nil
}

func (c *JobCoordinator) Get(_ context.Context, operationID string) (JobOperation, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	op, ok := c.ops[operationID]
	if !ok {
		return JobOperation{}, false
	}
	return *op, true
}
