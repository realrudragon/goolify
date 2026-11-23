package worker

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/applications"
	"github.com/coollabsio/coolify/backend/internal/domains/backups"
	"github.com/coollabsio/coolify/backend/internal/domains/databases"
	"github.com/coollabsio/coolify/backend/internal/domains/deployments"
	"github.com/coollabsio/coolify/backend/internal/domains/notifications"
	"github.com/coollabsio/coolify/backend/internal/domains/ops"
	"github.com/coollabsio/coolify/backend/internal/domains/proxy"
	"github.com/coollabsio/coolify/backend/internal/domains/schedules"
	"github.com/coollabsio/coolify/backend/internal/executor"
	"github.com/coollabsio/coolify/backend/internal/jobs"
	"github.com/coollabsio/coolify/backend/internal/realtime"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	WorkerID        string
	Pool            *pgxpool.Pool
	Cipher          *compat.Cipher
	JobCoord        *jobs.PostgresJobCoordinator
	ScheduleCoord   *schedules.PostgresScheduleCoordinator
	Outbox          *realtime.PostgresOutboxStore
	Dispatcher      *realtime.PostgresDispatcher
	LogStore        *realtime.PostgresLogStore
	Executor        *executor.Client
	Tracker         *executor.RemoteTracker
	Probe           executor.RemoteHostProbe
	BackupSvc       *backups.PostgresBackupService
	NotifySvc       *notifications.PostgresNotificationService
	BackupDir       string
	BackupDumper    backups.Dumper
	LeaseTTL        time.Duration
	CommandOverride func(spec jobs.DurableJobSpec) (capability string, command string)
}

// Runner is the real production Go Worker loop consuming durable PostgreSQL jobs,
// dispatching transactional outbox events, and coordinating distributed cron schedules.
type Runner struct {
	cfg Config
}

func NewRunner(cfg Config) *Runner {
	if cfg.WorkerID == "" {
		cfg.WorkerID = fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 2 * time.Minute
	}
	if cfg.Executor == nil {
		cfg.Executor = executor.NewClient(executor.NewLocalCommandTransport())
	}
	if cfg.Probe == nil && cfg.Pool != nil {
		var decFn func(string) string
		if cfg.Cipher != nil {
			decFn = func(raw string) string {
				if plain, decErr := cfg.Cipher.DecryptLaravelPayload(raw, false); decErr == nil {
					return string(plain)
				}
				return raw
			}
		}
		cfg.Probe = executor.NewSSHAwareRemoteProbe(cfg.Pool, cfg.Executor, decFn, true)
	}
	if cfg.Tracker == nil && cfg.Probe != nil {
		cfg.Tracker = executor.NewRemoteTracker(cfg.Probe)
	}
	if cfg.BackupSvc == nil && cfg.Pool != nil {
		cfg.BackupSvc = backups.NewPostgresBackupService(cfg.Pool, access.NewAuthorizer(), compat.NewEloquentHooks(), cfg.BackupDumper, cfg.BackupDir)
	}
	return &Runner{cfg: cfg}
}

// ProcessOneJob atomically claims one queued or lease-expired job from PostgreSQL,
// reconciles reclaimed jobs before taking side effects, executes via Executor, streams logs to Postgres,
// updates durable statuses, and emits transactional outbox events.
func (r *Runner) ProcessOneJob(ctx context.Context) (bool, error) {
	spec, lease, claimed, err := r.cfg.JobCoord.ClaimNext(ctx, r.cfg.WorkerID, r.cfg.LeaseTTL)
	if err != nil || !claimed {
		return false, err
	}

	// Reclaimed after lease expiry (previous worker crashed mid-job): MUST reconcile remote journal first!
	if spec.Payload != nil && spec.Payload["_reclaimed_after_lease_expiry"] == true && r.cfg.Tracker != nil {
		j, recErr := r.cfg.Tracker.ReconcileRemote(ctx, spec.ServerUUID, spec.OperationID)
		if recErr == nil {
			switch j.State {
			case executor.RemoteSucceeded:
				_ = r.finishJob(ctx, *spec, lease, jobs.StatusSucceeded, "reconciled remote succeeded", "")
				return true, nil
			case executor.RemoteRunning, executor.RemoteStarted, executor.RemoteUnknown:
				// Review Focus #3: NEVER blind-retry running or unknown remote state!
				_ = r.finishJob(ctx, *spec, lease, jobs.StatusUnknown, "reclaimed operation remote state is "+string(j.State)+"; manual/postcondition reconcile required", "")
				return true, nil
			}
		} else {
			_ = r.finishJob(ctx, *spec, lease, jobs.StatusUnknown, "reconcile probe error: "+recErr.Error(), "")
			return true, nil
		}
	}

	// Persist remote operation journal intent before side effects
	ensuredBootID := "host-local"
	if r.cfg.Tracker != nil {
		j, jErr := r.cfg.Tracker.EnsureOperationJournal(ctx, executor.OperationIntent{
			JobID:            spec.JobID,
			OperationID:      spec.OperationID,
			AttemptID:        spec.AttemptID,
			OwnershipEpoch:   lease.OwnershipEpoch,
			ResourceUUID:     spec.ResourceUUID,
			ServerUUID:       spec.ServerUUID,
			InputFingerprint: executor.ComputeInputFingerprint(spec.JobType, spec.ResourceUUID, fmt.Sprint(spec.Payload["input_revision"])),
		})
		if jErr != nil {
			_ = r.finishJob(ctx, *spec, lease, jobs.StatusUnknown, "journal ensure failed: "+jErr.Error(), "")
			return true, jErr
		}
		if j.BootID != "" {
			ensuredBootID = j.BootID
		}
	}

	// Handle real scheduled database backup jobs inside the worker (dump + SHA-256 + S3 + retention pruning)
	if spec.JobType == "backup" && r.cfg.CommandOverride == nil && r.cfg.BackupSvc != nil {
		return r.executeScheduledBackupJob(ctx, *spec, lease, ensuredBootID)
	}

	// Heartbeat goroutine renews fenced lease while executing
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go func() {
		interval := r.cfg.LeaseTTL / 3
		if interval < 100*time.Millisecond {
			interval = 100 * time.Millisecond
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				if err := r.cfg.JobCoord.RenewLease(hbCtx, spec.OperationID, lease.OwnershipEpoch, r.cfg.WorkerID, r.cfg.LeaseTTL); err != nil {
					return
				}
			}
		}
	}()

	capability, command, secrets, planErr := r.prepareExecutionCommand(ctx, *spec)
	if planErr != nil {
		if r.cfg.LogStore != nil {
			_, _ = r.cfg.LogStore.Append(ctx, spec.ResourceUUID, spec.OperationID, "stderr", "[planner] "+planErr.Error())
		}
		_ = r.finishJob(ctx, *spec, lease, jobs.StatusFailed, planErr.Error(), planErr.Error())
		return true, nil
	}

	if r.cfg.LogStore != nil {
		_, _ = r.cfg.LogStore.Append(ctx, spec.ResourceUUID, spec.OperationID, "stdout",
			fmt.Sprintf("[worker %s] starting %s operation=%s epoch=%d", r.cfg.WorkerID, spec.JobType, spec.OperationID, lease.OwnershipEpoch))
	}
	if spec.JobType == "deploy" && r.cfg.Pool != nil {
		if depUUID, ok := spec.Payload["deployment_uuid"].(string); ok && depUUID != "" {
			_, _ = r.cfg.Pool.Exec(ctx, `UPDATE application_deployment_queues SET status = 'in_progress', updated_at = NOW() WHERE deployment_uuid = $1 AND status = 'queued'`, depUUID)
		}
	}

	// Look up target server host/port/user & encrypted SSH key from PostgreSQL
	srvID, host, port, user, decryptedKey := r.loadServerExecutionTarget(ctx, spec.ServerUUID)
	if decryptedKey != "" {
		secrets = append(secrets, decryptedKey)
	}
	if r.cfg.CommandOverride == nil {
		if (srvID == 0 || spec.ServerUUID == "localhost") && spec.TeamID == 0 {
			capability = "local_exec"
			decryptedKey = ""
		} else {
			capability = "ssh_exec"
		}
	}

	var logLines []string
	res, runErr := r.cfg.Executor.Run(ctx, executor.ExecutionRequest{
		ProtocolVersion:    executor.ProtocolVersionV1,
		FrameType:          executor.FrameStart,
		JobID:              spec.JobID,
		OperationID:        spec.OperationID,
		AttemptID:          spec.AttemptID,
		OwnershipEpoch:     lease.OwnershipEpoch,
		ResourceUUID:       spec.ResourceUUID,
		TeamID:             spec.TeamID,
		DeadlineMS:         120000,
		MaxFrameBytes:      executor.DefaultMaxFrameBytes,
		ExpectedCapability: capability,
		Operation: executor.OperationSpec{
			Kind:          "exec",
			Host:          host,
			Port:          port,
			User:          user,
			SSHKeyContent: decryptedKey,
			UseSudo:       user != "root",
			Command:       command,
		},
	}, func(ev executor.ExecutionEvent) error {
		if ev.EventType == "output" {
			line := redactJobSecrets(ev.Data, secrets)
			logLines = append(logLines, line)
			if r.cfg.LogStore != nil {
				stream := ev.Stream
				if stream == "" {
					stream = "stdout"
				}
				_, _ = r.cfg.LogStore.Append(ctx, spec.ResourceUUID, spec.OperationID, stream, line)
			}
		}
		return nil
	})
	stopHB()

	redactedTail := redactJobSecrets(res.OutputTail, secrets)
	if len(logLines) > 0 && redactedTail == "" {
		redactedTail = strings.Join(logLines, "\n")
	}

	// Determine true terminal state from execution evidence (NEVER fabricate success on ambiguous/EOF!)
	var finalStatus jobs.JobStatus
	var remoteState executor.RemoteState
	var errMsg string

	switch {
	case res.Ambiguous:
		finalStatus = jobs.StatusUnknown
		remoteState = executor.RemoteUnknown
		if runErr != nil {
			errMsg = "ambiguous remote state: " + runErr.Error()
		} else {
			errMsg = "ambiguous remote state: missing exited frame"
		}
	case runErr != nil || res.ExitCode != 0:
		finalStatus = jobs.StatusFailed
		remoteState = executor.RemoteFailed
		if runErr != nil {
			errMsg = runErr.Error()
		} else {
			errMsg = fmt.Sprintf("command exited with code %d", res.ExitCode)
		}
	default:
		finalStatus = jobs.StatusSucceeded
		remoteState = executor.RemoteSucceeded
	}

	if r.cfg.Probe != nil {
		code := res.ExitCode
		_ = r.cfg.Probe.WriteJournalAtomic(ctx, spec.ServerUUID, executor.RemoteJournal{
			ProtocolVersion: executor.ProtocolVersionV1,
			JobID:           spec.JobID,
			OperationID:     spec.OperationID,
			AttemptID:       spec.AttemptID,
			OwnershipEpoch:  lease.OwnershipEpoch,
			ResourceUUID:    spec.ResourceUUID,
			State:           remoteState,
			BootID:          ensuredBootID,
			ExitCode:        &code,
			UpdatedAt:       time.Now().UTC(),
		})
	}

	if err := r.finishJob(ctx, *spec, lease, finalStatus, errMsg, redactedTail); err != nil {
		return true, err
	}
	return true, nil
}

func (r *Runner) finishJob(ctx context.Context, spec jobs.DurableJobSpec, lease jobs.JobLease, status jobs.JobStatus, errMsg string, outputTail string) error {
	if err := r.cfg.JobCoord.MarkTerminal(ctx, spec.OperationID, lease.OwnershipEpoch, status, errMsg); err != nil {
		return err
	}
	if r.cfg.Pool == nil {
		return nil
	}

	tx, err := r.cfg.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	uiStatus := mapStatusForUI(status)
	resType := "application"
	mutType := "application.status_changed"

	switch spec.JobType {
	case "deploy":
		finalLogs := strings.TrimSpace(outputTail)
		if errMsg != "" && !strings.Contains(finalLogs, errMsg) {
			if finalLogs == "" {
				finalLogs = "[error] " + errMsg
			} else {
				finalLogs = finalLogs + "\n[error] " + errMsg
			}
		}
		if depUUID, ok := spec.Payload["deployment_uuid"].(string); ok && depUUID != "" {
			_, _ = tx.Exec(ctx, `UPDATE application_deployment_queues SET status = $2, logs = NULLIF($3,''), updated_at = NOW() WHERE deployment_uuid = $1`, depUUID, uiStatus, finalLogs)
		}
		if status == jobs.StatusSucceeded {
			_, _ = tx.Exec(ctx, `UPDATE applications SET status = 'running', updated_at = NOW() WHERE uuid = $1`, spec.ResourceUUID)
		} else if status == jobs.StatusFailed {
			_, _ = tx.Exec(ctx, `UPDATE applications SET status = 'exited', updated_at = NOW() WHERE uuid = $1`, spec.ResourceUUID)
		}
	case "service_deploy":
		resType = "service"
		mutType = "service.status_changed"
		svcStatus := "exited"
		if status == jobs.StatusSucceeded {
			svcStatus = "running"
		}
		_, _ = tx.Exec(ctx, `UPDATE services SET status = $2, updated_at = NOW() WHERE uuid = $1`, spec.ResourceUUID, svcStatus)
	case "app_stop":
		if status == jobs.StatusSucceeded {
			_, _ = tx.Exec(ctx, `UPDATE applications SET status = 'exited', updated_at = NOW() WHERE uuid = $1`, spec.ResourceUUID)
		}
	case "service_stop":
		resType = "service"
		mutType = "service.status_changed"
		if status == jobs.StatusSucceeded {
			_, _ = tx.Exec(ctx, `UPDATE services SET status = 'exited', updated_at = NOW() WHERE uuid = $1`, spec.ResourceUUID)
		}
	case "db_stop":
		resType = "database"
		mutType = "database.status_changed"
		if status == jobs.StatusSucceeded {
			updateStandaloneDatabaseStatus(ctx, tx, spec.ResourceUUID, spec.Payload["db_type"], "exited")
		}
	case "server_validate":
		resType = "server"
		mutType = "server.validated"
		if status == jobs.StatusSucceeded {
			_, _ = tx.Exec(ctx, `UPDATE server_settings SET is_reachable = TRUE, is_usable = TRUE, updated_at = NOW() WHERE server_id = (SELECT id FROM servers WHERE uuid = $1 AND deleted_at IS NULL)`, spec.ResourceUUID)
		}
	case "db_provision":
		resType = "database"
		mutType = "database.status_changed"
		dbStatus := "exited"
		if status == jobs.StatusSucceeded {
			dbStatus = "running"
		}
		updateStandaloneDatabaseStatus(ctx, tx, spec.ResourceUUID, spec.Payload["db_type"], dbStatus)
	case "backup":
		resType = "database"
		mutType = "database.backup_completed"
	default:
		if rt, mt, handled := finalizeExtendedJobInTx(ctx, tx, spec, status, outputTail, errMsg); handled {
			resType = rt
			mutType = mt
		}
	}

	if r.cfg.Outbox != nil {
		_ = r.cfg.Outbox.AppendInTx(ctx, tx, realtime.DomainEvent{
			EventID:           fmt.Sprintf("job.done-%s-%d", spec.OperationID, lease.OwnershipEpoch),
			SchemaVersion:     "v1",
			Producer:          "go-worker",
			TeamID:            spec.TeamID,
			ResourceType:      resType,
			ResourceUUID:      spec.ResourceUUID,
			AggregateRevision: lease.OwnershipEpoch,
			OwnershipEpoch:    lease.OwnershipEpoch,
			MutationType:      mutType,
			CommittedAt:       time.Now().UTC(),
			Payload:           map[string]any{"operation_id": spec.OperationID, "status": uiStatus, "error": errMsg},
		})
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if r.cfg.Dispatcher != nil {
		_, _ = r.cfg.Dispatcher.FlushPending(ctx)
	}
	return nil
}

func (r *Runner) prepareExecutionCommand(ctx context.Context, spec jobs.DurableJobSpec) (string, string, []string, error) {
	if r.cfg.CommandOverride != nil {
		capStr, cmdStr := r.cfg.CommandOverride(spec)
		return capStr, cmdStr, nil, nil
	}
	if spec.JobType == "deploy" && r.cfg.Pool != nil {
		hooks := compat.NewEloquentHooks()
		appSvc := applications.NewPostgresService(r.cfg.Pool, access.NewAuthorizer(), hooks, r.cfg.Cipher, r.cfg.Outbox, r.cfg.JobCoord)
		sysPrincipal := auth.Principal{
			UserID:         -1,
			AuthMethod:     "session",
			SessionTeamID:  spec.TeamID,
			TeamRoles:      map[int64]auth.Role{spec.TeamID: auth.RoleOwner},
			TeamWideAccess: map[int64]bool{spec.TeamID: true},
		}
		dctx, err := appSvc.Resolver().Resolve(ctx, sysPrincipal, spec.ResourceUUID, nil)
		if err != nil {
			return "", "", nil, fmt.Errorf("resolve deployment context: %w", err)
		}
		plan, err := deployments.NewDeploymentPlanner().BuildPlan(dctx, spec.OperationID, "")
		if err != nil {
			return "", "", nil, err
		}
		redactAll := dctx.Settings["redact_all_env_values_in_logs"] == "true"
		var secrets []string
		if dctx.DeployPrivateKey != "" {
			secrets = append(secrets, dctx.DeployPrivateKey)
		}
		for _, entry := range dctx.EnvEntries {
			if (entry.IsLocked || redactAll) && entry.Value != "" {
				secrets = append(secrets, entry.Value)
			}
		}
		workDir := "/data/coolify/applications/" + strings.ReplaceAll(dctx.ApplicationUUID, "'", "")
		fullCmd := fmt.Sprintf("mkdir -p '%s' && cd '%s' && %s", workDir, workDir, strings.Join(plan.Commands, " && "))
		return "ssh_exec", fullCmd, secrets, nil
	}
	if spec.JobType == "service_deploy" && r.cfg.Pool != nil {
		var svcID int64
		var composeYAML, composeRaw, dockerNet string
		err := r.cfg.Pool.QueryRow(ctx, `
			SELECT s.id, COALESCE(s.docker_compose, ''), COALESCE(s.docker_compose_raw, ''), COALESCE(NULLIF(sd.network, ''), 'coolify')
			FROM services s
			LEFT JOIN standalone_dockers sd ON sd.id = s.destination_id
			WHERE s.uuid = $1
		`, spec.ResourceUUID).Scan(&svcID, &composeYAML, &composeRaw, &dockerNet)
		if err == nil {
			yamlBody := strings.TrimSpace(composeYAML)
			if yamlBody == "" {
				yamlBody = strings.TrimSpace(composeRaw)
			}
			var envBuf strings.Builder
			var secrets []string
			if eRows, eErr := r.cfg.Pool.Query(ctx, `
				SELECT key, value, COALESCE(is_shown_once, FALSE)
				FROM environment_variables
				WHERE resourceable_type = 'App\Models\Service' AND resourceable_id = $1
				ORDER BY id ASC
			`, svcID); eErr == nil {
				for eRows.Next() {
					var k string
					var rawVal *string
					var locked bool
					if eRows.Scan(&k, &rawVal, &locked) == nil && strings.TrimSpace(k) != "" {
						val := ""
						if rawVal != nil {
							val = *rawVal
							if r.cfg.Cipher != nil && val != "" {
								if plain, dErr := r.cfg.Cipher.DecryptLaravelPayload(val, false); dErr == nil {
									val = string(plain)
								}
							}
						}
						if locked && val != "" {
							secrets = append(secrets, val)
						}
						fmt.Fprintf(&envBuf, "%s=%s\n", k, val)
					}
				}
				eRows.Close()
			}
			safeUUID := strings.ReplaceAll(spec.ResourceUUID, "'", "")
			workDir := "/data/coolify/services/" + safeUUID
			envB64 := base64.StdEncoding.EncodeToString([]byte(envBuf.String()))
			cmds := []string{
				fmt.Sprintf("mkdir -p '%s' && cd '%s'", workDir, workDir),
				fmt.Sprintf("printf %%s '%s' | base64 -d > .env", envB64),
				fmt.Sprintf("docker network inspect '%s' >/dev/null 2>&1 || docker network create --attachable '%s'", strings.ReplaceAll(dockerNet, "'", ""), strings.ReplaceAll(dockerNet, "'", "")),
			}
			if yamlBody != "" {
				yamlB64 := base64.StdEncoding.EncodeToString([]byte(yamlBody + "\n"))
				cmds = append(cmds, fmt.Sprintf("printf %%s '%s' | base64 -d > docker-compose.yaml", yamlB64))
			}
			cmds = append(cmds, fmt.Sprintf("docker compose --project-name '%s' -f docker-compose.yaml up -d --remove-orphans", safeUUID))
			return "ssh_exec", strings.Join(cmds, " && "), secrets, nil
		}
	}
	if (spec.JobType == "proxy_start" || spec.JobType == "proxy_restart" || spec.JobType == "proxy_stop") && r.cfg.Pool != nil {
		if spec.JobType == "proxy_stop" {
			return "ssh_exec", proxy.BuildStopProxyCommand(), nil, nil
		}
		var srvID int64
		var proxyType, customCfg string
		_ = r.cfg.Pool.QueryRow(ctx, `
			SELECT id, COALESCE(proxy->>'type', 'traefik'), COALESCE(proxy->>'custom_configuration', '')
			FROM servers WHERE uuid = $1 AND deleted_at IS NULL
		`, spec.ServerUUID).Scan(&srvID, &proxyType, &customCfg)
		if proxyType == "" {
			proxyType = "traefik"
		}
		var networks []string
		if nRows, nErr := r.cfg.Pool.Query(ctx, `SELECT DISTINCT COALESCE(NULLIF(network,''), 'coolify') FROM standalone_dockers WHERE server_id = $1`, srvID); nErr == nil {
			for nRows.Next() {
				var netName string
				if nRows.Scan(&netName) == nil {
					networks = append(networks, netName)
				}
			}
			nRows.Close()
		}
		var proxyRes []proxy.ProxyResource
		if aRows, aErr := r.cfg.Pool.Query(ctx, `
			SELECT a.uuid, COALESCE(a.fqdn,''), COALESCE(NULLIF(a.ports_exposes,''), '80')
			FROM applications a
			JOIN standalone_dockers sd ON sd.id = a.destination_id
			WHERE sd.server_id = $1 AND COALESCE(a.fqdn,'') <> ''
		`, srvID); aErr == nil {
			for aRows.Next() {
				var u, fqdn, portStr string
				if aRows.Scan(&u, &fqdn, &portStr) == nil {
					p := strings.TrimSpace(strings.Split(portStr, ",")[0])
					if p == "" {
						p = "80"
					}
					for _, domain := range strings.Split(fqdn, ",") {
						domain = strings.TrimSpace(domain)
						if domain != "" {
							proxyRes = append(proxyRes, proxy.ProxyResource{
								UUID:     u,
								FQDN:     domain,
								Upstream: fmt.Sprintf("http://%s:%s", u, p),
								TLS:      strings.HasPrefix(strings.ToLower(domain), "https://"),
							})
						}
					}
				}
			}
			aRows.Close()
		}
		gen := proxy.NewProxyConfigGenerator()
		bundle, err := gen.RenderWithNetworks(proxyType, networks, proxyRes)
		if err != nil {
			bundle, _ = gen.RenderWithNetworks("traefik", networks, proxyRes)
		}
		composeYAML := bundle.ComposeYAML
		if strings.TrimSpace(customCfg) != "" {
			composeYAML = customCfg
		}
		return "ssh_exec", proxy.BuildStartProxyCommand(proxyType, composeYAML, bundle.Dynamic, networks), nil, nil
	}
	if (spec.JobType == "db_provision" || spec.JobType == "db_start" || spec.JobType == "db_restart") && r.cfg.Pool != nil {
		dbSvc := databases.NewPostgresDatabaseManager(r.cfg.Pool, access.NewAuthorizer(), compat.NewEloquentHooks(), r.cfg.Cipher, r.cfg.Outbox, r.cfg.JobCoord)
		sysPrincipal := auth.Principal{
			UserID: -1, AuthMethod: "session", SessionTeamID: spec.TeamID,
			TeamRoles: map[int64]auth.Role{spec.TeamID: auth.RoleOwner}, TeamWideAccess: map[int64]bool{spec.TeamID: true},
		}
		if dbRec, dbErr := dbSvc.GetByUUID(ctx, sysPrincipal, spec.ResourceUUID); dbErr == nil {
			var secrets []string
			if dbRec.Password != "" {
				secrets = append(secrets, dbRec.Password)
			}
			return "ssh_exec", ops.BuildDatabaseStartCommandFull(ops.DatabaseStartSpec{
				DBType:       dbRec.Type,
				ResourceUUID: dbRec.UUID,
				Image:        dbRec.Image,
				OperationID:  spec.OperationID,
				Network:      "coolify",
				Password:     dbRec.Password,
				IsPublic:     dbRec.IsPublic,
				PublicPort:   dbRec.PublicPort,
			}), secrets, nil
		}
	}
	capStr, cmdStr := r.buildExecutionCommand(spec)
	return capStr, cmdStr, nil, nil
}

func (r *Runner) loadServerExecutionTarget(ctx context.Context, serverUUID string) (srvID int64, host string, port int, user string, decryptedKey string) {
	host = serverUUID
	port = 22
	user = "root"
	if r.cfg.Pool == nil {
		return 0, host, port, user, ""
	}
	var encKey string
	err := r.cfg.Pool.QueryRow(ctx, `
		SELECT s.id, s.ip, s.port, s."user", COALESCE(pk.private_key, '')
		FROM servers s
		LEFT JOIN private_keys pk ON pk.id = s.private_key_id
		WHERE s.uuid = $1 AND s.deleted_at IS NULL
	`, serverUUID).Scan(&srvID, &host, &port, &user, &encKey)
	if err != nil {
		return 0, serverUUID, 22, "root", ""
	}
	if port <= 0 {
		port = 22
	}
	if strings.TrimSpace(user) == "" {
		user = "root"
	}
	if encKey != "" && r.cfg.Cipher != nil {
		if plain, decErr := r.cfg.Cipher.DecryptLaravelPayload(encKey, false); decErr == nil {
			decryptedKey = string(plain)
		} else {
			decryptedKey = encKey
		}
	} else {
		decryptedKey = encKey
	}
	return srvID, host, port, user, decryptedKey
}

func redactJobSecrets(text string, secrets []string) string {
	if text == "" || len(secrets) == 0 {
		return text
	}
	sorted := make([]string, len(secrets))
	copy(sorted, secrets)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	out := text
	for _, s := range sorted {
		if s != "" {
			out = strings.ReplaceAll(out, s, "[REDACTED]")
		}
	}
	return out
}

func (r *Runner) buildExecutionCommand(spec jobs.DurableJobSpec) (string, string) {
	if r.cfg.CommandOverride != nil {
		return r.cfg.CommandOverride(spec)
	}
	switch spec.JobType {
	case "deploy":
		img, _ := spec.Payload["image"].(string)
		if img == "" {
			img = "alpine:3.21"
		}
		tag, _ := spec.Payload["tag"].(string)
		if tag != "" {
			img = img + ":" + tag
		}
		return "ssh_exec", fmt.Sprintf("docker pull '%s' && docker compose --project-name '%s' up -d --remove-orphans",
			strings.ReplaceAll(img, "'", ""), strings.ReplaceAll(spec.ResourceUUID, "'", ""))
	case "service_deploy":
		return "ssh_exec", fmt.Sprintf("docker compose --project-name '%s' up -d --remove-orphans",
			strings.ReplaceAll(spec.ResourceUUID, "'", ""))
	case "app_stop", "service_stop":
		return "ssh_exec", fmt.Sprintf("docker compose --project-name '%s' down",
			strings.ReplaceAll(spec.ResourceUUID, "'", ""))
	case "db_stop":
		return "ssh_exec", fmt.Sprintf("docker stop 'coolify-db-%s' || true",
			strings.ReplaceAll(spec.ResourceUUID, "'", ""))
	case "server_validate":
		return "ssh_exec", "docker info --format '{{.ServerVersion}}'"
	case "db_provision", "db_start", "db_restart":
		img, _ := spec.Payload["image"].(string)
		dbType, _ := spec.Payload["db_type"].(string)
		if dbType == "" {
			dbType, _ = spec.Payload["database_type"].(string)
		}
		return "ssh_exec", ops.BuildDatabaseStartCommand(dbType, spec.ResourceUUID, img, spec.OperationID)
	default:
		if capStr, cmdStr, ok := buildExtendedExecutionCommand(spec); ok {
			return capStr, cmdStr
		}
		return "ssh_exec", "echo unsupported-job-type && exit 1"
	}
}

// TickSchedules scans enabled database backup schedules due at `now` and claims distributed leases in PostgreSQL.
func (r *Runner) TickSchedules(ctx context.Context, now time.Time) (int, error) {
	if r.cfg.Pool == nil || r.cfg.ScheduleCoord == nil {
		return 0, nil
	}
	rows, err := r.cfg.Pool.Query(ctx, `
		SELECT id, uuid, frequency, timezone, database_id, database_type, team_id,
		       COALESCE(database_retention_count, 7), COALESCE(save_s3, FALSE), s3_storage_id
		FROM scheduled_database_backups
		WHERE enabled = TRUE
		ORDER BY id ASC
	`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type dueSched struct {
		id, dbID, teamID int64
		retention        int
		saveS3           bool
		s3ID             *int64
		uuid, freq, tz, dbType string
	}
	var due []dueSched
	for rows.Next() {
		var item dueSched
		if err := rows.Scan(&item.id, &item.uuid, &item.freq, &item.tz, &item.dbID, &item.dbType, &item.teamID, &item.retention, &item.saveS3, &item.s3ID); err != nil {
			return 0, err
		}
		if cronDueAt(item.freq, now, item.tz) {
			due = append(due, item)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	enqueued := 0
	for _, item := range due {
		won, err := r.cfg.ScheduleCoord.AcquireDueLease(ctx, item.id, r.cfg.WorkerID, now, item.tz)
		if err != nil || !won {
			continue
		}
		meta := resolveBackupTargetMeta(ctx, r.cfg.Pool, r.cfg.Cipher, item.dbID, item.dbType)
		srvUUID := meta.ServerUUID
		if srvUUID == "" {
			srvUUID = "localhost"
		}
		resUUID := meta.UUID
		if resUUID == "" {
			resUUID = item.uuid
		}
		opID := fmt.Sprintf("sched-bk-%d-%s", item.id, now.UTC().Format("20060102T1504"))
		payload := map[string]any{
			"schedule_id":     item.id,
			"schedule_uuid":   item.uuid,
			"database_id":     item.dbID,
			"database_uuid":   resUUID,
			"database_name":   meta.Name,
			"database_type":   normalizeBackupDBType(item.dbType),
			"retention_count": item.retention,
			"save_s3":         item.saveS3,
		}
		if item.s3ID != nil {
			payload["s3_storage_id"] = *item.s3ID
		}
		if err := r.cfg.JobCoord.AdmitOperation(ctx, jobs.DurableJobSpec{
			JobOperation: jobs.JobOperation{
				JobID: "job-" + opID, OperationID: opID, AttemptID: "att-0",
				ResourceUUID: resUUID, ServerUUID: srvUUID, TeamID: item.teamID, Status: jobs.StatusQueued, MaxAttempts: 1,
			},
			JobType: "backup",
			Payload: payload,
		}); err == nil {
			_, _ = recordScheduledJobDelivery(ctx, r.cfg.Pool, fmt.Sprintf("database_backup:%d", item.id), now, "backup", item.id, r.cfg.WorkerID, payload)
			enqueued++
		}
	}
	extEnqueued, _ := r.tickExtendedSchedules(ctx, now)
	return enqueued + extEnqueued, nil
}

func (r *Runner) executeScheduledBackupJob(ctx context.Context, spec jobs.DurableJobSpec, lease jobs.JobLease, bootID string) (bool, error) {
	schedID := int64FromAny(spec.Payload["schedule_id"])
	dbID := int64FromAny(spec.Payload["database_id"])
	dbType, _ := spec.Payload["database_type"].(string)
	retention := int(int64FromAny(spec.Payload["retention_count"]))
	saveS3, _ := spec.Payload["save_s3"].(bool)
	s3ID := int64FromAny(spec.Payload["s3_storage_id"])

	var schedUUID string
	_ = r.cfg.Pool.QueryRow(ctx, `
		SELECT uuid, COALESCE(database_retention_count, 7), COALESCE(save_s3, FALSE), COALESCE(s3_storage_id, 0)
		FROM scheduled_database_backups WHERE id = $1
	`, schedID).Scan(&schedUUID, &retention, &saveS3, &s3ID)

	meta := resolveBackupTargetMeta(ctx, r.cfg.Pool, r.cfg.Cipher, dbID, dbType)
	if meta.UUID == "" {
		meta.UUID = spec.ResourceUUID
	}
	if meta.Name == "" {
		meta.Name = meta.UUID
	}
	containerName := "coolify-db-" + meta.UUID
	if dbID == 0 || meta.UUID == "coolify-db" {
		containerName = "coolify-db"
	}

	var s3Target *backups.S3Target
	if saveS3 && s3ID > 0 {
		var ep, bucket, region, kEnc, secEnc string
		if err := r.cfg.Pool.QueryRow(ctx, `
			SELECT endpoint, bucket, region, key, secret FROM s3_storages WHERE id = $1 AND team_id = $2
		`, s3ID, spec.TeamID).Scan(&ep, &bucket, &region, &kEnc, &secEnc); err == nil && ep != "" {
			s3Target = &backups.S3Target{
				Endpoint:  ep,
				Bucket:    bucket,
				Region:    region,
				AccessKey: decryptPayloadOrPlain(r.cfg.Cipher, kEnc),
				SecretKey: decryptPayloadOrPlain(r.cfg.Cipher, secEnc),
			}
		}
	}

	sysPrincipal := auth.Principal{
		UserID:         -1,
		AuthMethod:     "session",
		SessionTeamID:  spec.TeamID,
		TeamRoles:      map[int64]auth.Role{spec.TeamID: auth.RoleOwner},
		TeamWideAccess: map[int64]bool{spec.TeamID: true},
	}
	normType := normalizeBackupDBType(dbType)
	_, execRow, runErr := r.cfg.BackupSvc.RunRealBackup(ctx, sysPrincipal, backups.BackupSchedule{
		ID:             schedID,
		UUID:           schedUUID,
		DatabaseID:     dbID,
		DatabaseUUID:   meta.UUID,
		DatabaseType:   normType,
		TeamID:         spec.TeamID,
		RetentionCount: retention,
		Enabled:        true,
	}, databases.DatabaseRecord{
		ID:         dbID,
		UUID:       meta.UUID,
		Name:       meta.Name,
		Type:       normType,
		TeamID:     spec.TeamID,
		ServerUUID: spec.ServerUUID,
	}, backups.DumpTarget{
		DatabaseType:    normType,
		DockerContainer: containerName,
		DBUser:          meta.DBUser,
		DBPassword:      meta.Password,
		DBName:          meta.DBName,
		Host:            meta.ServerHost,
		Port:            meta.Port,
	}, s3Target, spec.OperationID)

	var finalStatus jobs.JobStatus
	var remoteState executor.RemoteState
	var errMsg, tail string
	exitCode := 0
	if runErr != nil {
		finalStatus = jobs.StatusFailed
		remoteState = executor.RemoteFailed
		errMsg = runErr.Error()
		exitCode = 1
		if r.cfg.LogStore != nil {
			_, _ = r.cfg.LogStore.Append(ctx, spec.ResourceUUID, spec.OperationID, "stderr", "[backup] "+errMsg)
		}
	} else {
		finalStatus = jobs.StatusSucceeded
		remoteState = executor.RemoteSucceeded
		tail = fmt.Sprintf("backup %s saved (%d bytes sha256=%s)", execRow.UUID, execRow.Size, execRow.ChecksumSHA)
		if r.cfg.LogStore != nil {
			_, _ = r.cfg.LogStore.Append(ctx, spec.ResourceUUID, spec.OperationID, "stdout", "[backup] "+tail)
		}
		r.replicateDatabaseBackupS3Replicas(ctx, schedID, spec.TeamID, execRow.ID, execRow.Filename, s3ID)
	}
	if r.cfg.Probe != nil {
		_ = r.cfg.Probe.WriteJournalAtomic(ctx, spec.ServerUUID, executor.RemoteJournal{
			ProtocolVersion: executor.ProtocolVersionV1,
			JobID:           spec.JobID,
			OperationID:     spec.OperationID,
			AttemptID:       spec.AttemptID,
			OwnershipEpoch:  lease.OwnershipEpoch,
			ResourceUUID:    spec.ResourceUUID,
			State:           remoteState,
			BootID:          bootID,
			ExitCode:        &exitCode,
			UpdatedAt:       time.Now().UTC(),
		})
	}
	if err := r.finishJob(ctx, spec, lease, finalStatus, errMsg, tail); err != nil {
		return true, err
	}
	return true, nil
}

type backupTargetMeta struct {
	UUID       string
	Name       string
	ServerUUID string
	ServerHost string
	Port       int
	DBUser     string
	Password   string
	DBName     string
}

func normalizeBackupDBType(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	s = strings.TrimPrefix(s, `app\models\standalone`)
	switch s {
	case "postgresql", "postgresqls":
		return "postgresql"
	case "mysql", "mysqls":
		return "mysql"
	case "mariadb", "mariadbs":
		return "mariadb"
	case "mongodb", "mongodbs":
		return "mongodb"
	case "redis":
		return "redis"
	case "keydb", "keydbs":
		return "keydb"
	case "dragonfly", "dragonflies":
		return "dragonfly"
	case "clickhouse", "clickhouses":
		return "clickhouse"
	case "sqlite", "sqlites":
		return "sqlite"
	default:
		return s
	}
}

func resolveBackupTargetMeta(ctx context.Context, pool *pgxpool.Pool, c *compat.Cipher, dbID int64, rawType string) backupTargetMeta {
	if pool == nil {
		return backupTargetMeta{ServerUUID: "localhost"}
	}
	t := normalizeBackupDBType(rawType)
	var query string
	var hasUserAndDB, hasPass, hasPort bool
	switch t {
	case "postgresql":
		query = `SELECT d.uuid, d.name, COALESCE(srv.uuid,'localhost'), COALESCE(srv.ip,'localhost'), COALESCE(d.public_port,5432), d.postgres_user, d.postgres_password, d.postgres_db FROM standalone_postgresqls d LEFT JOIN standalone_dockers sd ON sd.id = d.destination_id LEFT JOIN servers srv ON srv.id = sd.server_id WHERE d.id = $1 AND d.deleted_at IS NULL`
		hasUserAndDB, hasPass, hasPort = true, true, true
	case "mysql":
		query = `SELECT d.uuid, d.name, COALESCE(srv.uuid,'localhost'), COALESCE(srv.ip,'localhost'), COALESCE(d.public_port,3306), d.mysql_user, d.mysql_password, d.mysql_database FROM standalone_mysqls d LEFT JOIN standalone_dockers sd ON sd.id = d.destination_id LEFT JOIN servers srv ON srv.id = sd.server_id WHERE d.id = $1 AND d.deleted_at IS NULL`
		hasUserAndDB, hasPass, hasPort = true, true, true
	case "mariadb":
		query = `SELECT d.uuid, d.name, COALESCE(srv.uuid,'localhost'), COALESCE(srv.ip,'localhost'), COALESCE(d.public_port,3306), d.mariadb_user, d.mariadb_password, d.mariadb_database FROM standalone_mariadbs d LEFT JOIN standalone_dockers sd ON sd.id = d.destination_id LEFT JOIN servers srv ON srv.id = sd.server_id WHERE d.id = $1 AND d.deleted_at IS NULL`
		hasUserAndDB, hasPass, hasPort = true, true, true
	case "mongodb":
		query = `SELECT d.uuid, d.name, COALESCE(srv.uuid,'localhost'), COALESCE(srv.ip,'localhost'), COALESCE(d.public_port,27017), d.mongo_initdb_root_username, d.mongo_initdb_root_password, d.mongo_initdb_database FROM standalone_mongodbs d LEFT JOIN standalone_dockers sd ON sd.id = d.destination_id LEFT JOIN servers srv ON srv.id = sd.server_id WHERE d.id = $1 AND d.deleted_at IS NULL`
		hasUserAndDB, hasPass, hasPort = true, true, true
	case "clickhouse":
		query = `SELECT d.uuid, d.name, COALESCE(srv.uuid,'localhost'), COALESCE(srv.ip,'localhost'), COALESCE(d.public_port,9000), d.clickhouse_admin_user, d.clickhouse_admin_password, 'default' FROM standalone_clickhouses d LEFT JOIN standalone_dockers sd ON sd.id = d.destination_id LEFT JOIN servers srv ON srv.id = sd.server_id WHERE d.id = $1 AND d.deleted_at IS NULL`
		hasUserAndDB, hasPass, hasPort = true, true, true
	case "redis":
		query = `SELECT d.uuid, d.name, COALESCE(srv.uuid,'localhost'), COALESCE(srv.ip,'localhost'), COALESCE(d.public_port,6379), '', d.redis_password, '' FROM standalone_redis d LEFT JOIN standalone_dockers sd ON sd.id = d.destination_id LEFT JOIN servers srv ON srv.id = sd.server_id WHERE d.id = $1 AND d.deleted_at IS NULL`
		hasUserAndDB, hasPass, hasPort = true, true, true
	case "keydb":
		query = `SELECT d.uuid, d.name, COALESCE(srv.uuid,'localhost'), COALESCE(srv.ip,'localhost'), COALESCE(d.public_port,6379), '', d.keydb_password, '' FROM standalone_keydbs d LEFT JOIN standalone_dockers sd ON sd.id = d.destination_id LEFT JOIN servers srv ON srv.id = sd.server_id WHERE d.id = $1 AND d.deleted_at IS NULL`
		hasUserAndDB, hasPass, hasPort = true, true, true
	case "dragonfly":
		query = `SELECT d.uuid, d.name, COALESCE(srv.uuid,'localhost'), COALESCE(srv.ip,'localhost'), COALESCE(d.public_port,6379), '', d.dragonfly_password, '' FROM standalone_dragonflies d LEFT JOIN standalone_dockers sd ON sd.id = d.destination_id LEFT JOIN servers srv ON srv.id = sd.server_id WHERE d.id = $1 AND d.deleted_at IS NULL`
		hasUserAndDB, hasPass, hasPort = true, true, true
	case "sqlite":
		query = `SELECT d.uuid, d.name, COALESCE(srv.uuid,'localhost'), COALESCE(srv.ip,'localhost'), 0, '', '', d.sqlite_databases FROM standalone_sqlites d LEFT JOIN standalone_dockers sd ON sd.id = d.destination_id LEFT JOIN servers srv ON srv.id = sd.server_id WHERE d.id = $1 AND d.deleted_at IS NULL`
		hasUserAndDB, hasPass, hasPort = true, true, true
	default:
		return backupTargetMeta{ServerUUID: "localhost"}
	}
	var out backupTargetMeta
	var encPass string
	if hasUserAndDB && hasPass && hasPort {
		if err := pool.QueryRow(ctx, query, dbID).Scan(&out.UUID, &out.Name, &out.ServerUUID, &out.ServerHost, &out.Port, &out.DBUser, &encPass, &out.DBName); err != nil {
			return backupTargetMeta{ServerUUID: "localhost"}
		}
		out.Password = decryptPayloadOrPlain(c, encPass)
	}
	return out
}

func decryptPayloadOrPlain(c *compat.Cipher, raw string) string {
	if raw == "" || c == nil {
		return raw
	}
	if plain, err := c.DecryptLaravelPayload(raw, false); err == nil {
		return string(plain)
	}
	return raw
}

func int64FromAny(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	default:
		return 0
	}
}

// Run loops continuously processing jobs, outbox events, due cron schedules, and periodic server metrics until ctx is cancelled.
func (r *Runner) Run(ctx context.Context) {
	jobTicker := time.NewTicker(250 * time.Millisecond)
	outboxTicker := time.NewTicker(500 * time.Millisecond)
	schedTicker := time.NewTicker(15 * time.Second)
	metricsTicker := time.NewTicker(30 * time.Second)
	maintTicker := time.NewTicker(5 * time.Minute)
	defer jobTicker.Stop()
	defer outboxTicker.Stop()
	defer schedTicker.Stop()
	defer metricsTicker.Stop()
	defer maintTicker.Stop()

	// Collect an initial server metrics snapshot shortly after startup without blocking the main loop.
	go func() {
		if _, err := r.CollectServerMetrics(ctx); err != nil && ctx.Err() == nil {
			log.Printf("[worker %s] initial metrics collection error: %v", r.cfg.WorkerID, err)
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-jobTicker.C:
			for {
				worked, err := r.ProcessOneJob(ctx)
				if err != nil {
					log.Printf("[worker %s] job error: %v", r.cfg.WorkerID, err)
					break
				}
				if !worked {
					break
				}
			}
		case <-outboxTicker.C:
			if r.cfg.Dispatcher != nil {
				if _, err := r.cfg.Dispatcher.FlushPending(ctx); err != nil {
					log.Printf("[worker %s] outbox dispatch error: %v", r.cfg.WorkerID, err)
				}
			}
		case now := <-schedTicker.C:
			if _, err := r.TickSchedules(ctx, now); err != nil {
				log.Printf("[worker %s] schedule tick error: %v", r.cfg.WorkerID, err)
			}
		case <-metricsTicker.C:
			if _, err := r.CollectServerMetrics(ctx); err != nil {
				log.Printf("[worker %s] metrics collection error: %v", r.cfg.WorkerID, err)
			}
		case <-maintTicker.C:
			if err := r.RunMaintenanceSweep(ctx); err != nil {
				log.Printf("[worker %s] maintenance sweep error: %v", r.cfg.WorkerID, err)
			}
		}
	}
}

func mapStatusForUI(st jobs.JobStatus) string {
	switch st {
	case jobs.StatusSucceeded:
		return "finished"
	case jobs.StatusFailed, jobs.StatusDead:
		return "failed"
	case jobs.StatusUnknown:
		return "unknown"
	case jobs.StatusCancelled:
		return "cancelled"
	default:
		return string(st)
	}
}

// cronDueAt evaluates standard 5-field cron expressions (`min hour dom month dow`) in the given timezone.
func cronDueAt(expr string, now time.Time, tzName string) bool {
	loc := time.UTC
	if tzName != "" {
		if l, err := time.LoadLocation(tzName); err == nil {
			loc = l
		}
	}
	t := now.In(loc)
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 5 {
		return false
	}
	return matchCronField(fields[0], t.Minute()) &&
		matchCronField(fields[1], t.Hour()) &&
		matchCronField(fields[2], t.Day()) &&
		matchCronField(fields[3], int(t.Month())) &&
		matchCronField(fields[4], int(t.Weekday()))
}

func matchCronField(field string, val int) bool {
	if field == "*" {
		return true
	}
	for _, part := range strings.Split(field, ",") {
		if strings.HasPrefix(part, "*/") {
			step, err := strconv.Atoi(strings.TrimPrefix(part, "*/"))
			if err == nil && step > 0 && val%step == 0 {
				return true
			}
			continue
		}
		if n, err := strconv.Atoi(part); err == nil && n == val {
			return true
		}
	}
	return false
}

func updateStandaloneDatabaseStatus(ctx context.Context, tx pgx.Tx, dbUUID string, dbTypeRaw any, newStatus string) {
	tableByType := map[string]string{
		"postgresql": "standalone_postgresqls",
		"mysql":      "standalone_mysqls",
		"mariadb":    "standalone_mariadbs",
		"mongodb":    "standalone_mongodbs",
		"redis":      "standalone_redis",
		"keydb":      "standalone_keydbs",
		"dragonfly":  "standalone_dragonflies",
		"clickhouse": "standalone_clickhouses",
		"sqlite":     "standalone_sqlites",
	}
	if dt, ok := dbTypeRaw.(string); ok {
		if tbl, exists := tableByType[dt]; exists {
			_, _ = tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET status = $2, updated_at = NOW() WHERE uuid = $1`, tbl), dbUUID, newStatus)
			return
		}
	}
	for _, tbl := range tableByType {
		tag, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET status = $2, updated_at = NOW() WHERE uuid = $1`, tbl), dbUUID, newStatus)
		if err == nil && tag.RowsAffected() > 0 {
			return
		}
	}
}
