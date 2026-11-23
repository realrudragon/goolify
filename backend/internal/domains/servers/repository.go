package servers

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	ErrNotFound          = errors.New("server not found")
	ErrPrivateKeyTeam    = errors.New("private key belongs to another team")
	ErrTransferredServer = errors.New("server is transferred away and cannot run remote operations")
	ErrDisabledServer    = errors.New("server is force disabled")
	ErrInvalidLocalhost  = errors.New("cannot delete sentinel localhost server id=0")
)

type Server struct {
	ID               int64     `json:"id"`
	UUID             string    `json:"uuid"`
	Name             string    `json:"name"`
	Description      string    `json:"description,omitempty"`
	IP               string    `json:"ip"`
	Port             int       `json:"port"`
	User             string    `json:"user"`
	TeamID           int64     `json:"team_id"`
	PrivateKeyID     int64     `json:"private_key_id"`
	PrivateKeyUUID   string     `json:"private_key_uuid,omitempty"`
	WildcardDomain   string     `json:"wildcard_domain,omitempty"`
	IsReachable      bool       `json:"is_reachable"`
	IsUsable         bool      `json:"is_usable"`
	ForceDisabled    bool      `json:"force_disabled"`
	TransferredAt    *time.Time `json:"transferred_at,omitempty"`
	Revision         int64     `json:"revision"`
	UpdatedAt        time.Time `json:"updated_at"`
}

func (s Server) IsLocalhost() bool      { return s.ID == 0 }
func (s Server) IsTransferredAway() bool { return s.TransferredAt != nil }

type PrivateKeyRef struct {
	ID     int64  `json:"id"`
	UUID   string `json:"uuid"`
	TeamID int64  `json:"team_id"`
}

type Destination struct {
	ID         int64  `json:"id"`
	UUID       string `json:"uuid"`
	ServerID   int64  `json:"server_id"`
	Network    string `json:"network"`
	Type       string `json:"type"` // standalone | swarm
}

type CreateServerInput struct {
	Name           string `json:"name"`
	Description    string `json:"description"`
	IP             string `json:"ip"`
	Port           int    `json:"port"`
	User           string `json:"user"`
	TeamID         int64  `json:"team_id"`
	PrivateKeyUUID string `json:"private_key_uuid"`
	WildcardDomain string `json:"wildcard_domain,omitempty"`
}

type UpdateServerInput struct {
	Name           *string `json:"name,omitempty"`
	Description    *string `json:"description,omitempty"`
	IP             *string `json:"ip,omitempty"`
	Port           *int    `json:"port,omitempty"`
	User           *string `json:"user,omitempty"`
	PrivateKeyUUID *string `json:"private_key_uuid,omitempty"`
	WildcardDomain *string `json:"wildcard_domain,omitempty"`
}

type Repository interface {
	GetByUUID(ctx context.Context, uuid string) (Server, error)
	ListByTeam(ctx context.Context, teamID int64) ([]Server, error)
	FindPrivateKeyByUUID(ctx context.Context, uuid string) (PrivateKeyRef, error)
	Save(ctx context.Context, srv Server) (Server, error)
	DeleteByUUID(ctx context.Context, uuid string) error
	ListDestinations(ctx context.Context, serverID int64) ([]Destination, error)
}

type MemoryRepository struct {
	mu      sync.RWMutex
	servers map[string]Server
	keys    map[string]PrivateKeyRef
	dests   map[int64][]Destination
	nextID  int64
}

func NewMemoryRepository() *MemoryRepository {
	return &MemoryRepository{
		servers: make(map[string]Server),
		keys:    make(map[string]PrivateKeyRef),
		dests:   make(map[int64][]Destination),
		nextID:  1,
	}
}

func (m *MemoryRepository) SeedServer(s Server) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.servers[s.UUID] = s
	if s.ID >= m.nextID {
		m.nextID = s.ID + 1
	}
}

func (m *MemoryRepository) SeedPrivateKey(pk PrivateKeyRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys[pk.UUID] = pk
}

func (m *MemoryRepository) GetByUUID(_ context.Context, uuid string) (Server, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.servers[uuid]
	if !ok {
		return Server{}, ErrNotFound
	}
	return s, nil
}

func (m *MemoryRepository) ListByTeam(_ context.Context, teamID int64) ([]Server, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Server
	for _, s := range m.servers {
		if s.TeamID == teamID {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *MemoryRepository) FindPrivateKeyByUUID(_ context.Context, uuid string) (PrivateKeyRef, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	pk, ok := m.keys[uuid]
	if !ok {
		return PrivateKeyRef{}, ErrNotFound
	}
	return pk, nil
}

func (m *MemoryRepository) Save(_ context.Context, srv Server) (Server, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.servers[srv.UUID]; ok {
		srv.ID = existing.ID
		srv.Revision = existing.Revision + 1
	} else {
		if srv.ID == 0 && srv.UUID != "localhost" {
			srv.ID = m.nextID
			m.nextID++
		}
		srv.Revision = 1
	}
	srv.UpdatedAt = time.Now().UTC()
	m.servers[srv.UUID] = srv
	return srv, nil
}

func (m *MemoryRepository) DeleteByUUID(_ context.Context, uuid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.servers[uuid]
	if !ok {
		return ErrNotFound
	}
	if s.IsLocalhost() {
		return ErrInvalidLocalhost
	}
	delete(m.servers, uuid)
	return nil
}

func (m *MemoryRepository) ListDestinations(_ context.Context, serverID int64) ([]Destination, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Destination(nil), m.dests[serverID]...), nil
}
