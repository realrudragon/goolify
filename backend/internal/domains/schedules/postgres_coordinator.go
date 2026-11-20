package schedules

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresScheduleCoordinator enforces distributed single-execution cron leases across multiple Go workers
// using PostgreSQL PRIMARY KEY (schedule_id, minute_bucket) atomicity (not Redis TTLs).
type PostgresScheduleCoordinator struct {
	pool *pgxpool.Pool
}

func NewPostgresScheduleCoordinator(pool *pgxpool.Pool) *PostgresScheduleCoordinator {
	return &PostgresScheduleCoordinator{pool: pool}
}

func (c *PostgresScheduleCoordinator) AcquireDueLease(ctx context.Context, scheduleID int64, workerID string, now time.Time, tzName string) (bool, error) {
	loc := time.UTC
	if tzName != "" {
		if loaded, err := time.LoadLocation(tzName); err == nil {
			loc = loaded
		}
	}
	// Format includes numeric offset (-0700) so DST fall-back repeated wall-clock minutes stay distinct.
	bucket := now.In(loc).Format("2006-01-02T15:04-0700")
	tag, err := c.pool.Exec(ctx, `
		INSERT INTO go_schedule_leases (schedule_id, minute_bucket, worker_id)
		VALUES ($1, $2, $3)
		ON CONFLICT (schedule_id, minute_bucket) DO NOTHING
	`, scheduleID, bucket, workerID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
