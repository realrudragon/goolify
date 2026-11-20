package services

import (
	"context"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/domains/compose"
	"github.com/coollabsio/coolify/backend/internal/domains/deployments"
)

type ServiceRecord struct {
	ID             int64
	UUID           string
	Name           string
	TeamID         int64
	ServerUUID     string
	DockerCompose  string
	FQDN           string
	ProxyType      string
}

type ServiceService struct {
	authz  *access.Authorizer
	parser *compose.ComposeParser
}

func NewServiceService(authz *access.Authorizer, parser *compose.ComposeParser) *ServiceService {
	return &ServiceService{authz: authz, parser: parser}
}

func (s *ServiceService) DeployService(ctx context.Context, principal auth.Principal, rec ServiceRecord, env map[string]string) (deployments.DeploymentReceipt, compose.ComposeParseOutput, error) {
	teamID := rec.TeamID
	if err := s.authz.Check(ctx, principal, access.ActionDeploy, access.ResourceRef{
		Type: "service", ID: rec.ID, TeamID: &teamID, APISegment: "services",
	}); err != nil {
		return deployments.DeploymentReceipt{}, compose.ComposeParseOutput{}, err
	}
	parsed, err := s.parser.Parse(ctx, compose.ComposeParseInput{
		ParserVersion: "5",
		RawCompose:    rec.DockerCompose,
		ResourceUUID:  rec.UUID,
		TeamID:        rec.TeamID,
		ProxyType:     rec.ProxyType,
		FQDN:          rec.FQDN,
		Env:           env,
	})
	if err != nil {
		return deployments.DeploymentReceipt{}, compose.ComposeParseOutput{}, err
	}
	return deployments.DeploymentReceipt{
		DeploymentUUID: "svc-dep-" + rec.UUID,
		OperationID:    "svc-op-" + rec.UUID,
		Status:         "finished",
		PreservedPrior: true,
	}, parsed, nil
}
