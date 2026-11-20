package schedules_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/domains/schedules"
)

func TestScheduleCoordinator_NoDuplicateBackupRuns(t *testing.T) {
	// Review Focus #5: multiple concurrent Go workers must never run the same scheduled backup twice
	coord := schedules.NewScheduleCoordinator()
	now := time.Date(2026, 11, 19, 3, 15, 0, 0, time.UTC)

	var wg sync.WaitGroup
	var mu sync.Mutex
	acquired := 0

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ok, err := coord.AcquireDueLease(context.Background(), 0, "worker-"+string(rune('A'+idx)), now, "Asia/Bangkok")
			if err == nil && ok {
				mu.Lock()
				acquired++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	if acquired != 1 {
		t.Fatalf("expected exactly 1 worker to acquire due lease (even for legacy schedule id=0), got %d", acquired)
	}
}
