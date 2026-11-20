package teams

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/realtime"
)

type TeamService struct {
	mu     sync.RWMutex
	authz  *access.Authorizer
	outbox *realtime.OutboxStore
	disp   *realtime.Dispatcher
	grants map[int64]map[int64][]auth.ResourceGrant // teamID -> userID -> grants
}

func NewTeamService(authz *access.Authorizer, outbox *realtime.OutboxStore, disp *realtime.Dispatcher) *TeamService {
	return &TeamService{
		authz:  authz,
		outbox: outbox,
		disp:   disp,
		grants: make(map[int64]map[int64][]auth.ResourceGrant),
	}
}

func (s *TeamService) GrantAccess(ctx context.Context, principal auth.Principal, teamID int64, memberID int64, grant auth.ResourceGrant) error {
	if err := s.authz.Check(ctx, principal, access.ActionUpdate, access.ResourceRef{
		Type: "team", ID: teamID, TeamID: &teamID, APISegment: "teams",
	}); err != nil {
		return err
	}
	grant.TeamID = teamID
	s.mu.Lock()
	if s.grants[teamID] == nil {
		s.grants[teamID] = make(map[int64][]auth.ResourceGrant)
	}
	s.grants[teamID][memberID] = append(s.grants[teamID][memberID], grant)
	s.mu.Unlock()

	if s.outbox != nil {
		_ = s.outbox.AppendInTx(ctx, true, realtime.DomainEvent{
			EventID: fmt.Sprintf("grant-%d-%d-%d", teamID, memberID, grant.ResourceID),
			SchemaVersion: "v1", Producer: "go-controlplane", TeamID: teamID,
			ResourceType: "access", ResourceUUID: fmt.Sprintf("user-%d", memberID),
			AggregateRevision: 1, OwnershipEpoch: 1, MutationType: "team.grant_revoked",
			CommittedAt: time.Now().UTC(),
		})
		if s.disp != nil {
			_ = s.disp.FlushPending(ctx)
		}
	}
	return nil
}
