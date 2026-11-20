package realtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	goredis "github.com/coollabsio/coolify/backend/internal/store/redis"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresOutboxStore persists transactional domain events in `go_outbox_events` inside the caller's pgx.Tx.
// If the surrounding transaction rolls back, zero outbox rows exist and zero invalidations/broadcasts occur.
type PostgresOutboxStore struct {
	pool *pgxpool.Pool
}

func NewPostgresOutboxStore(pool *pgxpool.Pool) *PostgresOutboxStore {
	return &PostgresOutboxStore{pool: pool}
}

func (s *PostgresOutboxStore) AppendInTx(ctx context.Context, tx pgx.Tx, evt DomainEvent) error {
	if tx == nil {
		return errors.New("AppendInTx requires an active pgx.Tx")
	}
	if evt.SchemaVersion == "" {
		evt.SchemaVersion = "v1"
	}
	if evt.CommittedAt.IsZero() {
		evt.CommittedAt = time.Now().UTC()
	}
	payloadBytes, err := json.Marshal(evt.Payload)
	if err != nil {
		return fmt.Errorf("marshal outbox payload: %w", err)
	}
	if len(payloadBytes) == 0 || string(payloadBytes) == "null" {
		payloadBytes = []byte("{}")
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO go_outbox_events (
			event_id, schema_version, producer, team_id, resource_type, resource_uuid,
			aggregate_revision, ownership_epoch, mutation_type, payload_json, committed_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10::jsonb,$11)
		ON CONFLICT (event_id) DO NOTHING
	`, evt.EventID, evt.SchemaVersion, evt.Producer, evt.TeamID, evt.ResourceType, evt.ResourceUUID,
		evt.AggregateRevision, evt.OwnershipEpoch, evt.MutationType, string(payloadBytes), evt.CommittedAt)
	return err
}

func (s *PostgresOutboxStore) Pending(ctx context.Context, limit int) ([]DomainEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT event_id, schema_version, producer, team_id, resource_type, resource_uuid,
		       aggregate_revision, ownership_epoch, mutation_type, payload_json, committed_at
		FROM go_outbox_events
		WHERE dispatched_at IS NULL AND dead_letter = FALSE
		ORDER BY id ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DomainEvent
	for rows.Next() {
		var evt DomainEvent
		var payloadRaw []byte
		if err := rows.Scan(
			&evt.EventID, &evt.SchemaVersion, &evt.Producer, &evt.TeamID,
			&evt.ResourceType, &evt.ResourceUUID, &evt.AggregateRevision,
			&evt.OwnershipEpoch, &evt.MutationType, &payloadRaw, &evt.CommittedAt,
		); err != nil {
			return nil, err
		}
		if len(payloadRaw) > 0 {
			_ = json.Unmarshal(payloadRaw, &evt.Payload)
		}
		out = append(out, evt)
	}
	return out, rows.Err()
}

func (s *PostgresOutboxStore) MarkDispatched(ctx context.Context, eventID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE go_outbox_events SET dispatched_at = NOW() WHERE event_id = $1`, eventID)
	return err
}

func (s *PostgresOutboxStore) MarkFailed(ctx context.Context, eventID string, errMsg string, maxAttempts int) error {
	if maxAttempts <= 0 {
		maxAttempts = 5
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE go_outbox_events
		SET attempts = attempts + 1,
		    last_error = $2,
		    dead_letter = (attempts + 1 >= $3)
		WHERE event_id = $1
	`, eventID, errMsg, maxAttempts)
	return err
}

func (s *PostgresOutboxStore) DeadLetterCount(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM go_outbox_events WHERE dead_letter = TRUE`).Scan(&n)
	return n, err
}

func (s *PostgresOutboxStore) ReplayDeadLetters(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `
		UPDATE go_outbox_events
		SET dead_letter = FALSE, attempts = 0, last_error = ''
		WHERE dead_letter = TRUE
	`)
	return tag.RowsAffected(), err
}

// RedisCacheCoherence uses atomic Redis INCR + Lua CAS scripts under coolify:go:v1:* to prevent stale-refill races
// across multiple Go controlplane/worker instances. When Redis is unavailable it fails closed so callers bypass cache.
type RedisCacheCoherence struct {
	client *goredis.Client
}

func NewRedisCacheCoherence(client *goredis.Client) *RedisCacheCoherence {
	return &RedisCacheCoherence{client: client}
}

func (c *RedisCacheCoherence) CurrentVersion(ctx context.Context, versionKey string) (int64, error) {
	return c.client.GetInt64(ctx, versionKey)
}

func (c *RedisCacheCoherence) InvalidateAndBump(ctx context.Context, evt DomainEvent) (int64, error) {
	vkey := versionKeyFor(evt)
	newVer, err := c.client.Incr(ctx, vkey)
	if err != nil {
		return 0, err
	}
	_ = c.client.Del(ctx, vkey+":val")
	return newVer, nil
}

// TryPopulate writes val ONLY IF the current version in Redis still equals expectedVersion (atomic Lua CAS).
func (c *RedisCacheCoherence) TryPopulate(ctx context.Context, versionKey string, expectedVersion int64, val any) (bool, error) {
	raw, err := json.Marshal(val)
	if err != nil {
		return false, err
	}
	const luaCAS = `
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
local exp = tonumber(ARGV[1])
if cur == exp then
  redis.call('SET', KEYS[2], ARGV[2])
  redis.call('SET', KEYS[3], ARGV[1])
  return 1
end
return 0`
	res, err := c.client.Eval(
		ctx,
		luaCAS,
		[]string{versionKey, versionKey + ":val", versionKey + ":val_ver"},
		strconv.FormatInt(expectedVersion, 10),
		string(raw),
	)
	if err != nil {
		return false, err
	}
	okInt, _ := res.(int64)
	return okInt == 1, nil
}

func (c *RedisCacheCoherence) Read(ctx context.Context, versionKey string, dst any) (int64, bool, error) {
	const luaRead = `
local cur = tonumber(redis.call('GET', KEYS[1]) or '0')
local vver = tonumber(redis.call('GET', KEYS[3]) or '-1')
if cur == vver then
  return {cur, redis.call('GET', KEYS[2])}
end
return {cur, false}`
	res, err := c.client.Eval(ctx, luaRead, []string{versionKey, versionKey + ":val", versionKey + ":val_ver"})
	if err != nil {
		return 0, false, err
	}
	arr, ok := res.([]any)
	if !ok || len(arr) != 2 {
		return 0, false, nil
	}
	ver, _ := arr[0].(int64)
	rawStr, ok := arr[1].(string)
	if !ok || rawStr == "" {
		return ver, false, nil
	}
	if dst != nil {
		if err := json.Unmarshal([]byte(rawStr), dst); err != nil {
			return ver, false, nil
		}
	}
	return ver, true, nil
}

// PostgresDispatcher drains durable `go_outbox_events` in PostgreSQL, bumps Redis cache versions,
// and fans out committed events to Redis pubsub + local RealtimeHub subscribers.
type PostgresDispatcher struct {
	outbox *PostgresOutboxStore
	cache  *RedisCacheCoherence
	redis  *goredis.Client
	hub    *RealtimeHub
}

func NewPostgresDispatcher(outbox *PostgresOutboxStore, cache *RedisCacheCoherence, redisClient *goredis.Client, hub *RealtimeHub) *PostgresDispatcher {
	return &PostgresDispatcher{outbox: outbox, cache: cache, redis: redisClient, hub: hub}
}

func (d *PostgresDispatcher) FlushPending(ctx context.Context) (int, error) {
	pending, err := d.outbox.Pending(ctx, 100)
	if err != nil {
		return 0, err
	}
	dispatched := 0
	for _, evt := range pending {
		if d.cache != nil {
			if _, err := d.cache.InvalidateAndBump(ctx, evt); err != nil {
				_ = d.outbox.MarkFailed(ctx, evt.EventID, err.Error(), 5)
				return dispatched, err
			}
		}
		if d.redis != nil {
			if raw, err := json.Marshal(evt); err == nil {
				_ = d.redis.Publish(ctx, fmt.Sprintf("team.%d", evt.TeamID), string(raw))
			}
		}
		if d.hub != nil {
			d.hub.Publish(ctx, evt)
		}
		if err := d.outbox.MarkDispatched(ctx, evt.EventID); err != nil {
			return dispatched, err
		}
		dispatched++
	}
	return dispatched, nil
}

// PostgresLogStore persists deployment/resource log chunks in `go_operation_logs` for durable SSE replay and gap detection.
type PostgresLogStore struct {
	pool    *pgxpool.Pool
	secrets []string
}

func NewPostgresLogStore(pool *pgxpool.Pool, secrets ...string) *PostgresLogStore {
	return &PostgresLogStore{pool: pool, secrets: secrets}
}

func (s *PostgresLogStore) Append(ctx context.Context, resourceKey string, operationID string, stream string, rawText string) (LogEntry, error) {
	redacted := rawText
	for _, sec := range s.secrets {
		if sec != "" {
			redacted = strings.ReplaceAll(redacted, sec, "[REDACTED]")
		}
	}
	var id int64
	var createdAt time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO go_operation_logs (resource_key, operation_id, stream, text)
		VALUES ($1, $2, $3, $4)
		RETURNING id, created_at
	`, resourceKey, operationID, stream, redacted).Scan(&id, &createdAt)
	if err != nil {
		return LogEntry{}, err
	}
	return LogEntry{
		Cursor:    id,
		Stream:    stream,
		Text:      redacted,
		Timestamp: createdAt.UTC().Format(time.RFC3339Nano),
	}, nil
}

func (s *PostgresLogStore) ReplayFrom(ctx context.Context, resourceKey string, sinceCursor int64, maxBatch int) ([]LogEntry, bool, error) {
	if maxBatch <= 0 {
		maxBatch = 500
	}
	var minID *int64
	if err := s.pool.QueryRow(ctx, `SELECT MIN(id) FROM go_operation_logs WHERE resource_key = $1`, resourceKey).Scan(&minID); err != nil {
		return nil, false, err
	}
	gap := false
	if minID != nil && sinceCursor > 0 && sinceCursor < *minID-1 {
		gap = true
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, stream, text, created_at
		FROM go_operation_logs
		WHERE resource_key = $1 AND id > $2
		ORDER BY id ASC
		LIMIT $3
	`, resourceKey, sinceCursor, maxBatch)
	if err != nil {
		return nil, gap, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		var e LogEntry
		var ts time.Time
		if err := rows.Scan(&e.Cursor, &e.Stream, &e.Text, &ts); err != nil {
			return nil, gap, err
		}
		e.Timestamp = ts.UTC().Format(time.RFC3339Nano)
		out = append(out, e)
	}
	return out, gap, rows.Err()
}

// PostgresTerminalTicketStore enforces durable, atomic single-use terminal tickets in `go_terminal_tickets`.
type PostgresTerminalTicketStore struct {
	pool  *pgxpool.Pool
	authz *access.Authorizer
}

func NewPostgresTerminalTicketStore(pool *pgxpool.Pool, authz *access.Authorizer) *PostgresTerminalTicketStore {
	return &PostgresTerminalTicketStore{pool: pool, authz: authz}
}

func (s *PostgresTerminalTicketStore) IssueTicket(ctx context.Context, p auth.Principal, target TerminalTarget) (TerminalTicket, error) {
	teamID := target.TeamID
	if err := s.authz.Check(ctx, p, access.ActionTerminal, access.ResourceRef{
		Type: "server", ID: target.ServerID, UUID: target.ServerUUID, TeamID: &teamID, APISegment: "servers",
	}); err != nil {
		return TerminalTicket{}, err
	}
	if !target.TerminalEnabled || target.ForceDisabled || target.TransferredAway || target.PrivateKeyTeamID != target.TeamID {
		return TerminalTicket{}, ErrTerminalNotAllowed
	}
	if (target.ServerID == 0 || target.ServerUUID == "localhost") && !(target.TeamID == 0 && p.IsInstanceAdmin) {
		return TerminalTicket{}, ErrTerminalNotAllowed
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return TerminalTicket{}, err
	}
	tok := hex.EncodeToString(b)
	exp := time.Now().UTC().Add(60 * time.Second)
	rawTarget, err := json.Marshal(target)
	if err != nil {
		return TerminalTicket{}, err
	}
	_, err = s.pool.Exec(ctx, `
		INSERT INTO go_terminal_tickets (token, user_id, team_id, target_json, expires_at)
		VALUES ($1, $2, $3, $4::jsonb, $5)
	`, tok, p.UserID, target.TeamID, string(rawTarget), exp)
	if err != nil {
		return TerminalTicket{}, err
	}
	return TerminalTicket{
		Token:     tok,
		UserID:    p.UserID,
		TeamID:    target.TeamID,
		Target:    target,
		ExpiresAt: exp,
	}, nil
}

// RedeemTicket atomically claims a single-use unexpired ticket matching the authenticated user and allowed origin.
func (s *PostgresTerminalTicketStore) RedeemTicket(ctx context.Context, p auth.Principal, token string, origin string) (TerminalTarget, error) {
	var userID, teamID int64
	var targetRaw []byte
	err := s.pool.QueryRow(ctx, `
		UPDATE go_terminal_tickets
		SET redeemed_at = NOW()
		WHERE token = $1
		  AND redeemed_at IS NULL
		  AND expires_at > NOW()
		  AND user_id = $2
		RETURNING user_id, team_id, target_json
	`, token, p.UserID).Scan(&userID, &teamID, &targetRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return TerminalTarget{}, ErrTicketInvalidOrReplayed
	}
	if err != nil {
		return TerminalTarget{}, err
	}
	var target TerminalTarget
	if err := json.Unmarshal(targetRaw, &target); err != nil {
		return TerminalTarget{}, ErrTicketInvalidOrReplayed
	}
	if target.AllowedOrigin != "" && origin != target.AllowedOrigin {
		return TerminalTarget{}, ErrOriginMismatch
	}
	tID := target.TeamID
	if err := s.authz.Check(ctx, p, access.ActionTerminal, access.ResourceRef{
		Type: "server", ID: target.ServerID, UUID: target.ServerUUID, TeamID: &tID, APISegment: "servers",
	}); err != nil {
		return TerminalTarget{}, err
	}
	if target.TransferredAway || target.ForceDisabled {
		return TerminalTarget{}, ErrTerminalNotAllowed
	}
	if (target.ServerID == 0 || target.ServerUUID == "localhost") && !(target.TeamID == 0 && p.IsInstanceAdmin) {
		return TerminalTarget{}, ErrTerminalNotAllowed
	}
	return target, nil
}
