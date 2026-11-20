package deploymentcontext_test

import (
	"context"
	"errors"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/domains/deploymentcontext"
)

type stubRepo struct {
	source deploymentcontext.DeploymentSource
}

func (s *stubRepo) LoadApplicationSource(_ context.Context, _ string, _ *int64) (deploymentcontext.DeploymentSource, error) {
	return s.source, nil
}

func TestResolver_HierarchyEnvPrecedenceAndForeignCreds(t *testing.T) {
	repo := &stubRepo{
		source: deploymentcontext.DeploymentSource{
			ApplicationID:   10,
			ApplicationUUID: "app-10",
			EnvironmentID:   2,
			EnvironmentUUID: "env-2",
			ProjectID:       1,
			ProjectUUID:     "proj-1",
			ResourceTeamID:  10,
			ServerID:        0,
			ServerUUID:      "srv-0",
			PrivateKeyUUID:  "pk-1",
			PrivateKeyTeam:  10,
			BuildPack:       "dockerfile",
			TeamVars: []deploymentcontext.EnvVarRecord{
				{Key: "SHARED_HOST", Value: "db.internal"},
				{Key: "OVERRIDE_ME", Value: "from-team"},
			},
			ProjectVars: []deploymentcontext.EnvVarRecord{
				{Key: "CONN_STR", Value: "postgres://{{team.SHARED_HOST}}/app"},
				{Key: "OVERRIDE_ME", Value: "from-project"},
			},
			EnvironmentVars: []deploymentcontext.EnvVarRecord{
				{Key: "OVERRIDE_ME", Value: "from-env"},
			},
			ResourceVars: []deploymentcontext.EnvVarRecord{
				{Key: "APP_URL", Value: "{{project.CONN_STR}}?ssl=1", IsRuntime: true},
				{Key: "WHITESPACE", Value: "  keep-spaces  \nline2", IsLiteral: true},
				{Key: "EMPTY_VAL", Value: ""},
			},
			PreviewVars: []deploymentcontext.EnvVarRecord{
				{Key: "OVERRIDE_ME", Value: "from-preview"},
			},
		},
	}

	resolver := deploymentcontext.NewResolver(repo, access.NewAuthorizer())
	p := auth.Principal{
		UserID:        1,
		AuthMethod:    "session",
		SessionTeamID: 20, // switched session team
		TeamRoles:     map[int64]auth.Role{10: auth.RoleAdmin, 20: auth.RoleOwner},
	}

	pr := int64(7)
	dctx, err := resolver.Resolve(context.Background(), p, "app-10", &pr)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	vals := map[string]string{}
	for _, e := range dctx.EnvEntries {
		vals[e.Key] = e.Value
	}
	if vals["APP_URL"] != "postgres://db.internal/app?ssl=1" {
		t.Fatalf("unexpected APP_URL: %q", vals["APP_URL"])
	}
	if vals["OVERRIDE_ME"] != "from-preview" {
		t.Fatalf("expected preview override precedence, got %q", vals["OVERRIDE_ME"])
	}
	if vals["WHITESPACE"] != "  keep-spaces  \nline2" || vals["EMPTY_VAL"] != "" {
		t.Fatalf("whitespace/empty corrupted: %#v", vals)
	}

	// Immutable input revision changes when env changes
	rev1 := dctx.InputRevision
	repo.source.ResourceVars[0].Value = "changed"
	dctx2, err := resolver.Resolve(context.Background(), p, "app-10", &pr)
	if err != nil || dctx2.InputRevision == rev1 {
		t.Fatal("expected InputRevision to change when inputs mutate")
	}

	// Foreign team private key must be rejected
	repo.source.PrivateKeyTeam = 99
	if _, err := resolver.Resolve(context.Background(), p, "app-10", nil); !errors.Is(err, deploymentcontext.ErrForeignCredential) {
		t.Fatalf("expected ErrForeignCredential, got %v", err)
	}

	// Cyclic reference must error
	repo.source.PrivateKeyTeam = 10
	repo.source.TeamVars = []deploymentcontext.EnvVarRecord{{Key: "A", Value: "{{project.B}}"}}
	repo.source.ProjectVars = []deploymentcontext.EnvVarRecord{{Key: "B", Value: "{{team.A}}"}}
	if _, err := resolver.Resolve(context.Background(), p, "app-10", nil); !errors.Is(err, deploymentcontext.ErrCyclicEnvReference) {
		t.Fatalf("expected ErrCyclicEnvReference, got %v", err)
	}
}
