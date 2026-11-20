package access_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
)

func int64Ptr(v int64) *int64 { return &v }

func TestAuthorizer_ResourceTeamOverridesSwitchedSession(t *testing.T) {
	// Review Focus #1:
	// User is a read-only MEMBER of team 10 (where the server/app lives)
	// and OWNER of team 20, and switched their browser session to team 20 after mount.
	authz := access.NewAuthorizer()
	ctx := context.Background()

	p := auth.Principal{
		UserID:        42,
		AuthMethod:    "session",
		SessionTeamID: 20, // switched to another team where user is OWNER
		TeamRoles: map[int64]auth.Role{
			10: auth.RoleMember,
			20: auth.RoleOwner,
		},
		TeamWideAccess: map[int64]bool{10: false, 20: true},
		ResourceGrants: []auth.ResourceGrant{
			{ResourceType: "application", ResourceID: 500, TeamID: 10, Level: auth.AccessRead},
		},
	}

	// Allow read on granted application in team 10 even though session is team 20
	if err := authz.Check(ctx, p, access.ActionRead, access.ResourceRef{
		Type: "application", ID: 500, TeamID: int64Ptr(10),
	}); err != nil {
		t.Fatalf("expected read allow via resource team 10 grant: %v", err)
	}

	// Deny update/deploy/sensitive on team 10 resource despite being owner of session team 20
	for _, act := range []access.Action{access.ActionUpdate, access.ActionDeploy, access.ActionReadSensitive, access.ActionDelete} {
		if err := authz.Check(ctx, p, act, access.ResourceRef{
			Type: "application", ID: 500, TeamID: int64Ptr(10),
		}); err == nil {
			t.Fatalf("expected deny for action %s on team 10 resource when session is team 20", act)
		}
	}

	// Missing ResourceRef.TeamID must default deny
	if err := authz.Check(ctx, p, access.ActionRead, access.ResourceRef{
		Type: "application", ID: 500, TeamID: nil,
	}); err == nil {
		t.Fatal("expected deny when ResourceRef.TeamID is nil")
	}
}

func TestAuthorizer_FixturesAndLocalhostSentinels(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "auth", "v1", "fixtures", "policies.json"))
	if err != nil {
		t.Fatalf("read policy fixtures: %v", err)
	}
	var doc struct {
		Cases []struct {
			Name          string            `json:"name"`
			UserID        int64             `json:"user_id"`
			SessionTeamID int64             `json:"session_team_id"`
			TeamRoles     map[int64]string  `json:"team_roles"`
			Grants        []auth.ResourceGrant `json:"resource_grants"`
			Resource      struct {
				Type   string `json:"type"`
				ID     int64  `json:"id"`
				TeamID int64  `json:"team_id"`
			} `json:"resource"`
			Action        string `json:"action"`
			ExpectedAllow bool   `json:"expected_allow"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal policy fixtures: %v", err)
	}

	authz := access.NewAuthorizer()
	ctx := context.Background()
	for _, tc := range doc.Cases {
		roles := make(map[int64]auth.Role)
		for tid, r := range tc.TeamRoles {
			roles[tid] = auth.Role(r)
		}
		p := auth.Principal{
			UserID:          tc.UserID,
			SessionTeamID:   tc.SessionTeamID,
			AuthMethod:      "session",
			IsInstanceAdmin: tc.UserID == 0,
			TeamRoles:       roles,
			ResourceGrants:  tc.Grants,
		}
		err := authz.Check(ctx, p, access.Action(tc.Action), access.ResourceRef{
			Type:   tc.Resource.Type,
			ID:     tc.Resource.ID,
			TeamID: int64Ptr(tc.Resource.TeamID),
		})
		if tc.ExpectedAllow && err != nil {
			t.Errorf("case %s: expected allow, got %v", tc.Name, err)
		}
		if !tc.ExpectedAllow && err == nil {
			t.Errorf("case %s: expected deny, got allow", tc.Name)
		}
	}

	// Localhost server placement requires team admin even if member has teamWideWrite
	nonAdmin := auth.Principal{
		UserID:         9,
		AuthMethod:     "session",
		TeamRoles:      map[int64]auth.Role{0: auth.RoleMember},
		TeamWideAccess: map[int64]bool{0: true},
	}
	if err := authz.Check(ctx, nonAdmin, access.ActionPlaceResource, access.ResourceRef{
		Type: "server", ID: 0, TeamID: int64Ptr(0), IsLocalhost: true,
	}); err == nil {
		t.Fatal("expected localhost placement to deny non-admin member")
	}
}
