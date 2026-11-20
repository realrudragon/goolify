package realtime

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

var (
	ErrOutboxRolledBack = errors.New("transaction rolled back before outbox commit")
)

type DomainEvent struct {
	EventID           string         `json:"event_id"`
	SchemaVersion     string         `json:"schema_version"`
	Producer          string         `json:"producer"`
	TeamID            int64          `json:"team_id"`
	ResourceType      string         `json:"resource_type"`
	ResourceUUID      string         `json:"resource_uuid"`
	AggregateRevision int64          `json:"aggregate_revision"`
	OwnershipEpoch    int64          `json:"ownership_epoch"`
	MutationType      string         `json:"mutation_type"`
	CommittedAt       time.Time      `json:"committed_at"`
	Payload           map[string]any `json:"payload,omitempty"`
}

// TxHandle abstracts a DB transaction or unit-of-work so tests can verify rollback isolation.
type TxHandle interface {
	IsCommitted() bool
}

type OutboxRecord struct {
	Event       DomainEvent
	Dispatched  bool
	Attempts    int
	DeadLetter  bool
	LastError   string
}

type OutboxStore struct {
	mu      sync.Mutex
	records []OutboxRecord
	seenIDs map[string]bool
	lastRev map[string]int64
}

func NewOutboxStore() *OutboxStore {
	return &OutboxStore{
		seenIDs: make(map[string]bool),
		lastRev: make(map[string]int64),
	}
}

// AppendInTx enqueues evt only when tx commits.
func (s *OutboxStore) AppendInTx(_ context.Context, committed bool, evt DomainEvent) error {
	if !committed {
		return ErrOutboxRolledBack
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seenIDs[evt.EventID] {
		return nil // idempotent deduplication
	}
	s.seenIDs[evt.EventID] = true
	s.records = append(s.records, OutboxRecord{Event: evt})
	return nil
}

func (s *OutboxStore) Pending() []DomainEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []DomainEvent
	for _, r := range s.records {
		if !r.Dispatched && !r.DeadLetter {
			out = append(out, r.Event)
		}
	}
	return out
}

func (s *OutboxStore) MarkDispatched(eventID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.records {
		if s.records[i].Event.EventID == eventID {
			s.records[i].Dispatched = true
		}
	}
}

func (s *OutboxStore) MarkFailed(eventID string, errMsg string, maxAttempts int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.records {
		if s.records[i].Event.EventID == eventID {
			s.records[i].Attempts++
			s.records[i].LastError = errMsg
			if s.records[i].Attempts >= maxAttempts {
				s.records[i].DeadLetter = true
			}
		}
	}
}

// CacheCoherence maintains atomic monotonic version numbers per team & cache domain
// so concurrent Go workers cannot write stale data back into Redis after invalidation (stale-refill race).
type CacheCoherence struct {
	mu       sync.RWMutex
	versions map[string]int64
	entries  map[string]versionedValue
	down     atomic.Bool
}

type versionedValue struct {
	version int64
	value   any
}

func NewCacheCoherence() *CacheCoherence {
	return &CacheCoherence{
		versions: make(map[string]int64),
		entries:  make(map[string]versionedValue),
	}
}

func (c *CacheCoherence) SetUnavailable(v bool) {
	c.down.Store(v)
}

func (c *CacheCoherence) CurrentVersion(key string) int64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.versions[key]
}

// InvalidateAndBump atomically increments the domain version key so any concurrent reader
// holding a snapshot from an older version fails CAS on TryPopulate.
func (c *CacheCoherence) InvalidateAndBump(_ context.Context, evt DomainEvent) (int64, error) {
	if c.down.Load() {
		return 0, errors.New("cache backend unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	vkey := versionKeyFor(evt)
	c.versions[vkey]++
	delete(c.entries, vkey)
	return c.versions[vkey], nil
}

// TryPopulate writes a computed read-model back only if expectedVersion still matches the current version.
// This eliminates stale-refill races across concurrent Go workers without relying on TTLs.
func (c *CacheCoherence) TryPopulate(versionKey string, expectedVersion int64, val any) bool {
	if c.down.Load() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.versions[versionKey] != expectedVersion {
		return false
	}
	c.entries[versionKey] = versionedValue{version: expectedVersion, value: val}
	return true
}

func (c *CacheCoherence) Read(versionKey string) (any, int64, bool) {
	if c.down.Load() {
		return nil, 0, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	v := c.versions[versionKey]
	ent, ok := c.entries[versionKey]
	if !ok || ent.version != v {
		return nil, v, false
	}
	return ent.value, v, true
}

func versionKeyFor(evt DomainEvent) string {
	switch evt.ResourceType {
	case "server", "project", "environment":
		return "coolify:go:v1:team:" + itoa64(evt.TeamID) + ":search_ver"
	case "access":
		return "coolify:go:v1:team:" + itoa64(evt.TeamID) + ":auth_ver"
	default:
		return "coolify:go:v1:team:" + itoa64(evt.TeamID) + ":" + evt.ResourceType + "_ver"
	}
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// RealtimeHub manages bounded team-scoped and user-scoped subscriber channels for SSE/WS projection.
type RealtimeHub struct {
	mu       sync.RWMutex
	subs     map[int64]map[chan DomainEvent]struct{}
	userSubs map[int64]map[chan DomainEvent]struct{}
	buf      int
}

func NewRealtimeHub(bufferSize int) *RealtimeHub {
	if bufferSize <= 0 {
		bufferSize = 64
	}
	return &RealtimeHub{
		subs:     make(map[int64]map[chan DomainEvent]struct{}),
		userSubs: make(map[int64]map[chan DomainEvent]struct{}),
		buf:      bufferSize,
	}
}

func (h *RealtimeHub) Subscribe(teamID int64) (<-chan DomainEvent, func()) {
	ch := make(chan DomainEvent, h.buf)
	h.mu.Lock()
	if h.subs[teamID] == nil {
		h.subs[teamID] = make(map[chan DomainEvent]struct{})
	}
	h.subs[teamID][ch] = struct{}{}
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		if m, ok := h.subs[teamID]; ok {
			if _, exists := m[ch]; exists {
				delete(m, ch)
				close(ch)
			}
			if len(m) == 0 {
				delete(h.subs, teamID)
			}
		}
		h.mu.Unlock()
	}
	return ch, cancel
}

func (h *RealtimeHub) SubscribeUser(userID int64) (<-chan DomainEvent, func()) {
	ch := make(chan DomainEvent, h.buf)
	h.mu.Lock()
	if h.userSubs[userID] == nil {
		h.userSubs[userID] = make(map[chan DomainEvent]struct{})
	}
	h.userSubs[userID][ch] = struct{}{}
	h.mu.Unlock()

	cancel := func() {
		h.mu.Lock()
		if m, ok := h.userSubs[userID]; ok {
			if _, exists := m[ch]; exists {
				delete(m, ch)
				close(ch)
			}
			if len(m) == 0 {
				delete(h.userSubs, userID)
			}
		}
		h.mu.Unlock()
	}
	return ch, cancel
}

// Publish fans out evt exclusively to subscribers of evt.TeamID without blocking on slow consumers.
func (h *RealtimeHub) Publish(_ context.Context, evt DomainEvent) (dropped int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs[evt.TeamID] {
		select {
		case ch <- evt:
		default:
			dropped++
		}
	}
	return dropped
}

// PublishUser fans out evt exclusively to subscribers of user.{userID} without blocking on slow consumers.
func (h *RealtimeHub) PublishUser(_ context.Context, userID int64, evt DomainEvent) (dropped int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.userSubs[userID] {
		select {
		case ch <- evt:
		default:
			dropped++
		}
	}
	return dropped
}

type Dispatcher struct {
	outbox *OutboxStore
	cache  *CacheCoherence
	hub    *RealtimeHub
}

func NewDispatcher(outbox *OutboxStore, cache *CacheCoherence, hub *RealtimeHub) *Dispatcher {
	return &Dispatcher{outbox: outbox, cache: cache, hub: hub}
}

// FlushPending applies cache version bumps strictly before broadcasting to realtime subscribers.
func (d *Dispatcher) FlushPending(ctx context.Context) error {
	for _, evt := range d.outbox.Pending() {
		if _, err := d.cache.InvalidateAndBump(ctx, evt); err != nil {
			d.outbox.MarkFailed(evt.EventID, err.Error(), 5)
			return err
		}
		d.hub.Publish(ctx, evt)
		d.outbox.MarkDispatched(evt.EventID)
	}
	return nil
}
