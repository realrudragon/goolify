package worker

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/coollabsio/coolify/backend/internal/domains/notifications"
	"github.com/coollabsio/coolify/backend/internal/opscli"
)

// CancelSupersededDeployments cancels older queued deployments for the same application
// when a newer deployment is admitted (parity with CancelSupersededDeploymentJob.php).
func (r *Runner) CancelSupersededDeployments(ctx context.Context, applicationID int64, keepDeploymentUUID string) (int64, error) {
	if r.cfg.Pool == nil {
		return 0, nil
	}
	tag, err := r.cfg.Pool.Exec(ctx, `
		UPDATE application_deployment_queues
		SET status = 'cancelled-by-user', finished_at = NOW(), updated_at = NOW()
		WHERE application_id = $1 AND status = 'queued' AND deployment_uuid <> $2
	`, applicationID, keepDeploymentUUID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SweepApiTokenExpirations inspects personal_access_tokens expiring within `withinDays`
// and dispatches throttled warnings to each owning team's configured notification channels
// (parity with ApiTokenExpirationWarningJob.php).
func (r *Runner) SweepApiTokenExpirations(ctx context.Context, withinDays int) (int, error) {
	if r.cfg.Pool == nil {
		return 0, nil
	}
	if withinDays <= 0 {
		withinDays = 7
	}
	cutoff := time.Now().UTC().Add(time.Duration(withinDays) * 24 * time.Hour)
	rows, err := r.cfg.Pool.Query(ctx, `
		SELECT id, team_id::text, name, expires_at
		FROM personal_access_tokens
		WHERE expires_at IS NOT NULL AND expires_at > NOW() AND expires_at <= $1
		ORDER BY id ASC
	`, cutoff)
	if err != nil {
		return 0, err
	}
	type expTok struct {
		id, teamID int64
		name       string
		exp        time.Time
	}
	var due []expTok
	for rows.Next() {
		var item expTok
		var teamStr string
		if rows.Scan(&item.id, &teamStr, &item.name, &item.exp) == nil {
			item.teamID, _ = strconv.ParseInt(teamStr, 10, 64)
			due = append(due, item)
		}
	}
	rows.Close()

	notified := 0
	for _, tok := range due {
		if r.cfg.NotifySvc == nil {
			continue
		}
		throttled, err := r.cfg.NotifySvc.CheckAndRecordThrottle(ctx, `App\Models\PersonalAccessToken`, tok.id, "api_token_expiring", 24*time.Hour)
		if err != nil || throttled {
			continue
		}
		msg := fmt.Sprintf("API token %q expires at %s", tok.name, tok.exp.UTC().Format(time.RFC3339))
		r.dispatchTeamAlert(ctx, tok.teamID, "api_token.expiring", msg)
		notified++
	}
	return notified, nil
}

// SweepMissingBackups detects enabled database and volume backup schedules that have had no
// successful execution within their configured missing_backup_notification_days window
// (parity with CheckMissingDatabaseBackupsJob.php & CheckMissingVolumeBackupsJob.php).
func (r *Runner) SweepMissingBackups(ctx context.Context) (int, error) {
	if r.cfg.Pool == nil {
		return 0, nil
	}
	alerts := 0

	// 1. Missing Volume Backups
	vRows, err := r.cfg.Pool.Query(ctx, `
		SELECT b.id, b.uuid, b.team_id, b.missing_backup_notification_days
		FROM scheduled_volume_backups b
		WHERE b.enabled = TRUE AND b.missing_backup_notification_days > 0
		  AND NOT EXISTS (
			SELECT 1 FROM scheduled_volume_backup_executions e
			WHERE e.scheduled_volume_backup_id = b.id
			  AND e.status = 'success'
			  AND e.created_at >= NOW() - (b.missing_backup_notification_days || ' days')::interval
		  )
	`)
	if err == nil {
		type miss struct {
			id, teamID int64
			uuid       string
			days       int
		}
		var list []miss
		for vRows.Next() {
			var m miss
			if vRows.Scan(&m.id, &m.uuid, &m.teamID, &m.days) == nil {
				list = append(list, m)
			}
		}
		vRows.Close()
		for _, m := range list {
			if r.cfg.NotifySvc == nil {
				continue
			}
			throttled, err := r.cfg.NotifySvc.CheckAndRecordThrottle(ctx, `App\Models\ScheduledVolumeBackup`, m.id, "missing_volume_backup", 24*time.Hour)
			if err != nil || throttled {
				continue
			}
			r.dispatchTeamAlert(ctx, m.teamID, "backup.missing", fmt.Sprintf("Volume backup schedule %s has no successful run in the last %d days", m.uuid, m.days))
			alerts++
		}
	}

	// 2. Missing Database Backups (when enabled schedule has no successful execution in last 24h)
	dRows, err := r.cfg.Pool.Query(ctx, `
		SELECT b.id, b.uuid, b.team_id
		FROM scheduled_database_backups b
		WHERE b.enabled = TRUE
		  AND b.created_at < NOW() - INTERVAL '24 hours'
		  AND NOT EXISTS (
			SELECT 1 FROM scheduled_database_backup_executions e
			WHERE e.scheduled_database_backup_id = b.id
			  AND e.status = 'success'
			  AND e.created_at >= NOW() - INTERVAL '48 hours'
		  )
	`)
	if err == nil {
		type missDB struct {
			id, teamID int64
			uuid       string
		}
		var list []missDB
		for dRows.Next() {
			var m missDB
			if dRows.Scan(&m.id, &m.uuid, &m.teamID) == nil {
				list = append(list, m)
			}
		}
		dRows.Close()
		for _, m := range list {
			if r.cfg.NotifySvc == nil {
				continue
			}
			throttled, err := r.cfg.NotifySvc.CheckAndRecordThrottle(ctx, `App\Models\ScheduledDatabaseBackup`, m.id, "missing_database_backup", 24*time.Hour)
			if err != nil || throttled {
				continue
			}
			r.dispatchTeamAlert(ctx, m.teamID, "backup.missing", fmt.Sprintf("Database backup schedule %s has no successful run in the last 48 hours", m.uuid))
			alerts++
		}
	}
	return alerts, nil
}

func (r *Runner) dispatchTeamAlert(ctx context.Context, teamID int64, event, summary string) {
	if r.cfg.NotifySvc == nil {
		return
	}
	for _, ch := range []notifications.ChannelKind{
		notifications.ChannelWebhook,
		notifications.ChannelDiscord,
		notifications.ChannelSlack,
		notifications.ChannelTelegram,
		notifications.ChannelPushover,
		notifications.ChannelEmail,
	} {
		if cfg, err := r.cfg.NotifySvc.LoadConfig(ctx, teamID, ch); err == nil && cfg.Enabled {
			_, _ = r.cfg.NotifySvc.SendReal(ctx, teamID, ch, event, summary)
		}
	}
}

// RunMaintenanceSweep runs periodic cleanup & operational watchdog checks.
func (r *Runner) RunMaintenanceSweep(ctx context.Context) error {
	if r.cfg.Pool == nil {
		return nil
	}
	ops := opscli.NewRunner(r.cfg.Pool, nil, r.cfg.Cipher)
	_, _ = ops.CleanupDeploymentQueue(ctx, 2*time.Hour)
	_, _ = ops.CleanupStuckResources(ctx, 30*time.Minute)
	_, _ = ops.CleanupImportUploads(ctx, 24*time.Hour)
	_, _ = r.SweepApiTokenExpirations(ctx, 7)
	_, _ = r.SweepMissingBackups(ctx)
	return nil
}
