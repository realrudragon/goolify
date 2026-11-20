package servers

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/realtime"
)

type ServerService struct {
	repo   Repository
	authz  *access.Authorizer
	hooks  *compat.EloquentHooks
	outbox *realtime.OutboxStore
	disp   *realtime.Dispatcher
}

func NewServerService(
	repo Repository,
	authz *access.Authorizer,
	hooks *compat.EloquentHooks,
	outbox *realtime.OutboxStore,
	disp *realtime.Dispatcher,
) *ServerService {
	return &ServerService{
		repo:   repo,
		authz:  authz,
		hooks:  hooks,
		outbox: outbox,
		disp:   disp,
	}
}

func (s *ServerService) Create(ctx context.Context, principal auth.Principal, input CreateServerInput) (Server, error) {
	teamID := input.TeamID
	if err := s.authz.Check(ctx, principal, access.ActionCreate, access.ResourceRef{
		Type: "server", TeamID: &teamID, APISegment: "servers",
	}); err != nil {
		return Server{}, err
	}

	pk, err := s.repo.FindPrivateKeyByUUID(ctx, input.PrivateKeyUUID)
	if err != nil {
		return Server{}, err
	}
	if pk.TeamID != teamID {
		return Server{}, ErrPrivateKeyTeam
	}

	port := input.Port
	if port == 0 {
		port = 22
	}
	user := strings.TrimSpace(input.User)
	if user == "" {
		user = "root"
	}

	row := compat.ModelRow{Name: compat.SanitizeString(input.Name)}
	if err := s.hooks.ApplyModelDefaults("server", &row); err != nil {
		return Server{}, err
	}

	srv := Server{
		ID:             1, // assigned by repo unless sentinel
		UUID:           row.UUID,
		Name:           row.Name,
		Description:    compat.SanitizeString(input.Description),
		IP:             strings.TrimSpace(input.IP),
		Port:           port,
		User:           user,
		TeamID:         teamID,
		PrivateKeyID:   pk.ID,
		PrivateKeyUUID: pk.UUID,
		IsUsable:       true,
	}
	saved, err := s.repo.Save(ctx, srv)
	if err != nil {
		return Server{}, err
	}
	s.emitEvent(ctx, saved, "server.created")
	return saved, nil
}

func (s *ServerService) Get(ctx context.Context, principal auth.Principal, serverUUID string) (Server, error) {
	srv, err := s.repo.GetByUUID(ctx, serverUUID)
	if err != nil {
		return Server{}, err
	}
	teamID := srv.TeamID
	if err := s.authz.Check(ctx, principal, access.ActionRead, access.ResourceRef{
		Type: "server", ID: srv.ID, TeamID: &teamID, IsLocalhost: srv.IsLocalhost(), APISegment: "servers",
	}); err != nil {
		return Server{}, err
	}
	return redactServer(srv), nil
}

func (s *ServerService) Update(ctx context.Context, principal auth.Principal, serverUUID string, input UpdateServerInput) (Server, error) {
	srv, err := s.repo.GetByUUID(ctx, serverUUID)
	if err != nil {
		return Server{}, err
	}
	teamID := srv.TeamID
	if err := s.authz.Check(ctx, principal, access.ActionUpdate, access.ResourceRef{
		Type: "server", ID: srv.ID, TeamID: &teamID, IsLocalhost: srv.IsLocalhost(), APISegment: "servers",
	}); err != nil {
		return Server{}, err
	}
	if srv.IsTransferredAway() {
		return Server{}, ErrTransferredServer
	}

	if input.PrivateKeyUUID != nil {
		pk, err := s.repo.FindPrivateKeyByUUID(ctx, *input.PrivateKeyUUID)
		if err != nil {
			return Server{}, err
		}
		if pk.TeamID != srv.TeamID {
			return Server{}, ErrPrivateKeyTeam
		}
		srv.PrivateKeyID = pk.ID
		srv.PrivateKeyUUID = pk.UUID
	}
	if input.Name != nil {
		srv.Name = compat.SanitizeString(*input.Name)
	}
	if input.Description != nil {
		srv.Description = compat.SanitizeString(*input.Description)
	}
	if input.IP != nil {
		srv.IP = strings.TrimSpace(*input.IP)
	}
	if input.Port != nil && *input.Port > 0 {
		srv.Port = *input.Port
	}
	if input.User != nil && strings.TrimSpace(*input.User) != "" {
		srv.User = strings.TrimSpace(*input.User)
	}

	saved, err := s.repo.Save(ctx, srv)
	if err != nil {
		return Server{}, err
	}
	s.emitEvent(ctx, saved, "server.updated")
	return redactServer(saved), nil
}

func (s *ServerService) Delete(ctx context.Context, principal auth.Principal, serverUUID string) error {
	srv, err := s.repo.GetByUUID(ctx, serverUUID)
	if err != nil {
		return err
	}
	teamID := srv.TeamID
	if err := s.authz.Check(ctx, principal, access.ActionDelete, access.ResourceRef{
		Type: "server", ID: srv.ID, TeamID: &teamID, IsLocalhost: srv.IsLocalhost(), APISegment: "servers",
	}); err != nil {
		return err
	}
	if err := s.repo.DeleteByUUID(ctx, serverUUID); err != nil {
		return err
	}
	// Note: If srv.IsTransferredAway(), archive delete never executes SSH/Cloud/DNS side effects.
	s.emitEvent(ctx, srv, "server.deleted")
	return nil
}

func (s *ServerService) ValidatePlacement(ctx context.Context, principal auth.Principal, serverUUID string, resourceTeamID int64) error {
	srv, err := s.repo.GetByUUID(ctx, serverUUID)
	if err != nil {
		return err
	}
	if srv.TeamID != resourceTeamID {
		return fmt.Errorf("%w: cross-team server placement denied", access.ErrForbidden)
	}
	if srv.IsTransferredAway() {
		return ErrTransferredServer
	}
	if srv.ForceDisabled {
		return ErrDisabledServer
	}
	teamID := srv.TeamID
	return s.authz.Check(ctx, principal, access.ActionPlaceResource, access.ResourceRef{
		Type: "server", ID: srv.ID, TeamID: &teamID, IsLocalhost: srv.IsLocalhost(), APISegment: "servers",
	})
}

func redactServer(s Server) Server {
	// Never return internal private key bytes; only UUID reference.
	return s
}

func (s *ServerService) emitEvent(ctx context.Context, srv Server, mutType string) {
	if s.outbox == nil {
		return
	}
	evt := realtime.DomainEvent{
		EventID:           fmt.Sprintf("%s-%s-%d", mutType, srv.UUID, srv.Revision),
		SchemaVersion:     "v1",
		Producer:          "go-controlplane",
		TeamID:            srv.TeamID,
		ResourceType:      "server",
		ResourceUUID:      srv.UUID,
		AggregateRevision: maxInt64(srv.Revision, 1),
		OwnershipEpoch:    1,
		MutationType:      mutType,
		CommittedAt:       time.Now().UTC(),
		Payload:           map[string]any{"name": srv.Name, "uuid": srv.UUID},
	}
	_ = s.outbox.AppendInTx(ctx, true, evt)
	if s.disp != nil {
		_ = s.disp.FlushPending(ctx)
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
