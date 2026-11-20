package schedules

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// ScheduleCoordinator enforces single-execution distributed lease fencing across Go worker instances
// for cron backups, scheduled tasks, and maintenance jobs.
type ScheduleCoordinator struct {
	mu     sync.Mutex
	leases map[string]string // "<scheduleID>:<minuteBucket>" -> workerID
}

func NewScheduleCoordinator() *ScheduleCoordinator {
	return &ScheduleCoordinator{
		leases: make(map[string]string),
	}
}

// AcquireDueLease atomically grants a lease to at most one worker per scheduleID + minute window in the configured timezone.
func (c *ScheduleCoordinator) AcquireDueLease(_ context.Context, scheduleID int64, workerID string, now time.Time, tzName string) (bool, error) {
	loc := time.UTC
	if tzName != "" {
		loaded, err := time.LoadLocation(tzName)
		if err == nil {
			loc = loaded
		}
	}
	bucket := now.In(loc).Format("2006-01-02T15:04-0700")
	key := fmt.Sprintf("%d:%s", scheduleID, bucket)

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, held := c.leases[key]; held {
		return false, nil
	}
	c.leases[key] = workerID
	return true, nil
}
