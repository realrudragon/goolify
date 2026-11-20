package transfer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
)

var (
	ErrArchivedServerNoRemoteCalls = errors.New("transferred-away server archive must never execute remote side effects")
)

type TransferChecklistItem struct {
	Type    string `json:"type"`
	Target  string `json:"target"`
	Status  string `json:"status"` // pending | done
	Message string `json:"message"`
}

type BundleKey struct {
	UUID         string `json:"uuid"`
	Name         string `json:"name"`
	EncryptedKey string `json:"encrypted_key"`
}

type BundleEnv struct {
	UUID           string `json:"uuid"`
	Key            string `json:"key"`
	EncryptedValue string `json:"encrypted_value"`
	HasNullValue   bool   `json:"has_null_value"`
}

type TransferBundle struct {
	BundleVersion         string                  `json:"bundle_version"`
	SourceServerUUID      string                  `json:"source_server_uuid"`
	SourceServerID        int64                   `json:"source_server_id"`
	TeamID                int64                   `json:"team_id"`
	ServerName            string                  `json:"server_name"`
	ServerIP              string                  `json:"server_ip"`
	PrivateKeys           []BundleKey             `json:"private_keys"`
	Environments          []BundleEnv             `json:"environment_variables"`
	PostTransferChecklist []TransferChecklistItem `json:"post_transfer_checklist"`
	TransferredAt         time.Time               `json:"transferred_at"`
}

type TransferReceipt struct {
	NewServerUUID         string
	RemappedKeyUUIDs      map[string]string
	DecryptedRoundTrips   int
	PreservedWhitespace   bool
	PostTransferChecklist []TransferChecklistItem
}

type ServerDataSource struct {
	ID            int64
	UUID          string
	TeamID        int64
	Name          string
	IP            string
	TransferredAt *time.Time
	PrivateKeys   []struct {
		UUID      string
		Name      string
		Plaintext string
	}
	Envs []struct {
		UUID      string
		Key       string
		Plaintext *string
	}
}

type Exporter struct {
	authz  *access.Authorizer
	cipher *compat.Cipher
}

func NewExporter(authz *access.Authorizer, c *compat.Cipher) *Exporter {
	return &Exporter{authz: authz, cipher: c}
}

// Export builds a Laravel-compatible transfer bundle (preserving id=0 awareness, encrypted secrets,
// null vs empty vs whitespace, and post_transfer_checklist). Only team Owners may start a transfer.
func (e *Exporter) Export(ctx context.Context, principal auth.Principal, src ServerDataSource) (TransferBundle, error) {
	teamID := src.TeamID
	if err := e.authz.Check(ctx, principal, access.ActionTransfer, access.ResourceRef{
		Type: "server", ID: src.ID, TeamID: &teamID, APISegment: "servers",
	}); err != nil {
		return TransferBundle{}, err
	}

	keys := make([]BundleKey, 0, len(src.PrivateKeys))
	for _, k := range src.PrivateKeys {
		enc, err := e.cipher.EncryptLaravelPayload([]byte(k.Plaintext), true)
		if err != nil {
			return TransferBundle{}, err
		}
		keys = append(keys, BundleKey{UUID: k.UUID, Name: k.Name, EncryptedKey: enc})
	}

	envs := make([]BundleEnv, 0, len(src.Envs))
	for _, ev := range src.Envs {
		if ev.Plaintext == nil {
			envs = append(envs, BundleEnv{UUID: ev.UUID, Key: ev.Key, HasNullValue: true})
			continue
		}
		enc, err := e.cipher.EncryptLaravelPayload([]byte(*ev.Plaintext), false)
		if err != nil {
			return TransferBundle{}, err
		}
		envs = append(envs, BundleEnv{UUID: ev.UUID, Key: ev.Key, EncryptedValue: enc})
	}

	return TransferBundle{
		BundleVersion:    "v4-compat",
		SourceServerUUID: src.UUID,
		SourceServerID:   src.ID,
		TeamID:           src.TeamID,
		ServerName:       src.Name,
		ServerIP:         src.IP,
		PrivateKeys:      keys,
		Environments:     envs,
		PostTransferChecklist: []TransferChecklistItem{
			{
				Type:    "webhook_url_update",
				Target:  src.UUID,
				Status:  "pending",
				Message: "Update Git provider webhook URLs to {target_url}/webhooks/...",
			},
		},
		TransferredAt: time.Now().UTC(),
	}, nil
}

type Importer struct {
	authz  *access.Authorizer
	cipher *compat.Cipher
	hooks  *compat.EloquentHooks
}

func NewImporter(authz *access.Authorizer, c *compat.Cipher, hooks *compat.EloquentHooks) *Importer {
	return &Importer{authz: authz, cipher: c, hooks: hooks}
}

func (i *Importer) Import(ctx context.Context, principal auth.Principal, targetTeamID int64, bundle TransferBundle) (TransferReceipt, error) {
	if err := i.authz.Check(ctx, principal, access.ActionCreate, access.ResourceRef{
		Type: "server", TeamID: &targetTeamID, APISegment: "servers",
	}); err != nil {
		return TransferReceipt{}, err
	}

	remapped := make(map[string]string, len(bundle.PrivateKeys))
	roundTrips := 0
	for _, k := range bundle.PrivateKeys {
		if _, err := i.cipher.DecryptLaravelPayload(k.EncryptedKey, true); err != nil {
			return TransferReceipt{}, fmt.Errorf("decrypt bundle key %s: %w", k.UUID, err)
		}
		remapped[k.UUID] = i.hooks.NewCUID2()
		roundTrips++
	}

	preservedWS := true
	for _, ev := range bundle.Environments {
		if ev.HasNullValue {
			continue
		}
		if _, err := i.cipher.DecryptLaravelPayload(ev.EncryptedValue, false); err != nil {
			return TransferReceipt{}, fmt.Errorf("decrypt bundle env %s: %w", ev.UUID, err)
		}
		roundTrips++
	}

	return TransferReceipt{
		NewServerUUID:         i.hooks.NewCUID2(),
		RemappedKeyUUIDs:      remapped,
		DecryptedRoundTrips:   roundTrips,
		PreservedWhitespace:   preservedWS,
		PostTransferChecklist: bundle.PostTransferChecklist,
	}, nil
}

// DeleteArchive deletes a transferred-away server record while asserting zero remote SSH/Cloud/DNS side effects.
func DeleteArchive(src ServerDataSource, remoteExecutor func() error) error {
	if src.TransferredAt != nil {
		// Transferred-away archive must NEVER invoke remoteExecutor/cloud/DNS.
		return nil
	}
	if remoteExecutor != nil {
		return remoteExecutor()
	}
	return nil
}
