package databases_test

import (
	"context"
	"errors"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/domains/databases"
)

func TestDatabaseService_SupportedTypesAndRedaction(t *testing.T) {
	svc := databases.NewDatabaseService(access.NewAuthorizer())
	admin := auth.Principal{UserID: 1, AuthMethod: "session", TeamRoles: map[int64]auth.Role{10: auth.RoleAdmin}}
	member := auth.Principal{UserID: 2, AuthMethod: "session", TeamRoles: map[int64]auth.Role{10: auth.RoleMember}}

	for _, dbType := range databases.SupportedDatabaseTypes {
		rec := databases.DatabaseRecord{ID: 1, UUID: "db-" + dbType, Name: dbType, Type: dbType, TeamID: 10, PasswordEnc: "secret"}
		if _, err := svc.Provision(context.Background(), member, rec); !errors.Is(err, access.ErrForbidden) {
			t.Fatalf("expected member denied provisioning %s, got %v", dbType, err)
		}
		res, err := svc.Provision(context.Background(), admin, rec)
		if err != nil || res.Status != "running" {
			t.Fatalf("Provision %s failed: res=%+v err=%v", dbType, res, err)
		}
		red := databases.RedactDatabase(rec)
		if red.PasswordEnc != "" {
			t.Fatalf("RedactDatabase did not clear PasswordEnc for %s", dbType)
		}
	}

	if _, err := svc.Provision(context.Background(), admin, databases.DatabaseRecord{ID: 9, Type: "oracle", TeamID: 10}); err == nil {
		t.Fatal("expected unsupported database type to fail")
	}
}
