package worker

import (
	"context"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/domains/ops"
)

// CollectServerMetrics samples CPU, memory, and disk utilization for localhost (id=0)
// and reachable remote servers that have not received a recent Sentinel/metric sample,
// persisting real measurements to go_server_metrics.
func (r *Runner) CollectServerMetrics(ctx context.Context) (int, error) {
	if r.cfg.Pool == nil {
		return 0, nil
	}

	rows, err := r.cfg.Pool.Query(ctx, `
		SELECT s.id, s.uuid, s.team_id, COALESCE(s.ip, ''), COALESCE(s."user", 'root'),
		       COALESCE(s.port, 22), s.private_key_id, COALESCE(ss.is_reachable, FALSE),
		       (SELECT MAX(m.recorded_at) FROM go_server_metrics m WHERE m.server_id = s.id)
		FROM servers s
		LEFT JOIN server_settings ss ON ss.server_id = s.id
		WHERE s.deleted_at IS NULL
		  AND s.transferred_at IS NULL
		  AND COALESCE(ss.force_disabled, FALSE) = FALSE
		ORDER BY s.id ASC
	`)
	if err != nil {
		return 0, err
	}

	type target struct {
		id           int64
		uuid         string
		teamID       int64
		ip           string
		user         string
		port         int
		privateKeyID *int64
		isReachable  bool
		lastRecorded *time.Time
	}
	var targets []target
	for rows.Next() {
		var t target
		if rows.Scan(&t.id, &t.uuid, &t.teamID, &t.ip, &t.user, &t.port, &t.privateKeyID, &t.isReachable, &t.lastRecorded) == nil {
			targets = append(targets, t)
		}
	}
	rows.Close()

	collected := 0
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		// Skip servers that already received a fresh sample (e.g. from Sentinel push) in the last 25s.
		if t.lastRecorded != nil && time.Since(*t.lastRecorded) < 25*time.Second {
			continue
		}

		if t.id == 0 {
			sample, sErr := ops.CollectLocalHostMetrics(ctx)
			if sErr != nil {
				continue
			}
			if ops.RecordServerMetrics(ctx, r.cfg.Pool, t.id, sample) == nil {
				collected++
			}
			continue
		}

		if !t.isReachable || t.privateKeyID == nil || r.cfg.Executor == nil || strings.TrimSpace(t.ip) == "" {
			continue
		}

		var rawKey string
		if err := r.cfg.Pool.QueryRow(ctx, `SELECT COALESCE(private_key, '') FROM private_keys WHERE id = $1`, *t.privateKeyID).Scan(&rawKey); err != nil || strings.TrimSpace(rawKey) == "" {
			continue
		}
		sshKey := rawKey
		if r.cfg.Cipher != nil {
			if plain, decErr := r.cfg.Cipher.DecryptLaravelPayload(rawKey, false); decErr == nil {
				sshKey = string(plain)
			}
		}

		sample, sErr := ops.CollectRemoteServerMetrics(ctx, r.cfg.Executor, t.uuid, t.teamID, t.ip, t.user, t.port, sshKey)
		if sErr != nil {
			continue
		}
		if ops.RecordServerMetrics(ctx, r.cfg.Pool, t.id, sample) == nil {
			collected++
		}
	}

	return collected, nil
}
