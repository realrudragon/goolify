package services_test

import (
	"context"
	"errors"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/domains/compose"
	"github.com/coollabsio/coolify/backend/internal/domains/services"
)

func TestServiceService_DeployAndAuth(t *testing.T) {
	svc := services.NewServiceService(access.NewAuthorizer(), compose.NewComposeParser())
	rec := services.ServiceRecord{
		ID: 1, UUID: "svc-1", Name: "plausible", TeamID: 10, ProxyType: "traefik", FQDN: "https://stats.example.com",
		DockerCompose: "services:\n  web:\n    image: nginx:alpine\n    environment:\n      - SERVICE_URL_WEB\n",
	}

	member := auth.Principal{UserID: 2, AuthMethod: "session", TeamRoles: map[int64]auth.Role{10: auth.RoleMember}}
	if _, _, err := svc.DeployService(context.Background(), member, rec, nil); !errors.Is(err, access.ErrForbidden) {
		t.Fatalf("expected member without write grant denied, got %v", err)
	}

	admin := auth.Principal{UserID: 1, AuthMethod: "session", TeamRoles: map[int64]auth.Role{10: auth.RoleAdmin}}
	receipt, parsed, err := svc.DeployService(context.Background(), admin, rec, nil)
	if err != nil || receipt.Status != "finished" || parsed.Services["web"].Environment["SERVICE_URL_WEB"] != "https://stats.example.com" {
		t.Fatalf("unexpected service deploy result: receipt=%+v parsed=%+v err=%v", receipt, parsed, err)
	}
}
