package transfer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/transfer"
)

func TestTransfer_RoundTripRootZeroAndWhitespace(t *testing.T) {
	// Review Focus #2: owner-only transfer, id=0 sentinel awareness, encrypted secrets, null/empty/whitespace preservation
	c, err := compat.NewCipher("base64:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	authz := access.NewAuthorizer()
	hooks := compat.NewEloquentHooks()
	exp := transfer.NewExporter(authz, c)
	imp := transfer.NewImporter(authz, c, hooks)

	adminOnly := auth.Principal{UserID: 2, AuthMethod: "session", TeamRoles: map[int64]auth.Role{0: auth.RoleAdmin}}
	owner := auth.Principal{UserID: 0, AuthMethod: "session", IsInstanceAdmin: true, TeamRoles: map[int64]auth.Role{0: auth.RoleOwner, 10: auth.RoleOwner}}

	empty := ""
	ws := "  keep-ws \nline2  "
	src := transfer.ServerDataSource{
		ID: 0, UUID: "srv-root-0", TeamID: 0, Name: "localhost", IP: "127.0.0.1",
		PrivateKeys: []struct {
			UUID      string
			Name      string
			Plaintext string
		}{{UUID: "pk-0", Name: "root-key", Plaintext: "-----BEGIN OPENSSH PRIVATE KEY-----\nTEST\n-----END OPENSSH PRIVATE KEY-----\n"}},
		Envs: []struct {
			UUID      string
			Key       string
			Plaintext *string
		}{
			{UUID: "ev-null", Key: "NULL_KEY", Plaintext: nil},
			{UUID: "ev-empty", Key: "EMPTY_KEY", Plaintext: &empty},
			{UUID: "ev-ws", Key: "WS_KEY", Plaintext: &ws},
		},
	}

	// Non-owner admin must be denied export
	if _, err := exp.Export(context.Background(), adminOnly, src); !errors.Is(err, access.ErrForbidden) {
		t.Fatalf("expected non-owner admin denied transfer export, got %v", err)
	}

	bundle, err := exp.Export(context.Background(), owner, src)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if bundle.SourceServerID != 0 || len(bundle.PostTransferChecklist) != 1 {
		t.Fatalf("unexpected bundle metadata: %+v", bundle)
	}

	receipt, err := imp.Import(context.Background(), owner, 10, bundle)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if receipt.DecryptedRoundTrips != 3 || receipt.RemappedKeyUUIDs["pk-0"] == "" || receipt.RemappedKeyUUIDs["pk-0"] == "pk-0" {
		t.Fatalf("expected UUID remapping & 3 decrypted non-null roundtrips, got %+v", receipt)
	}
}

func TestTransfer_TransferredAwayArchiveNoSideEffects(t *testing.T) {
	now := time.Now().UTC()
	archived := transfer.ServerDataSource{ID: 55, UUID: "srv-55", TeamID: 10, TransferredAt: &now}

	sshCalled := false
	err := transfer.DeleteArchive(archived, func() error {
		sshCalled = true
		return errors.New("must not run SSH on transferred-away archive")
	})
	if err != nil || sshCalled {
		t.Fatalf("transferred-away archive delete invoked remote side effects: err=%v sshCalled=%v", err, sshCalled)
	}
}
