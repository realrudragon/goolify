package controlplane

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/executor"
)

// Registry commands always target the managed host, including the instance host.
// Credentials travel over executor stdin and never appear in the command or response.
func (a *App) runRegistryCommand(ctx context.Context, m slice12ServerMeta, command, input string) error {
	if m.TransferredAt != nil {
		return fmt.Errorf("remote commands are disabled for transferred servers")
	}
	var host, user, key string
	var port int
	if err := a.Pool.QueryRow(ctx, `SELECT s.ip,s."user",s.port,COALESCE(pk.private_key,'') FROM servers s LEFT JOIN private_keys pk ON pk.id=s.private_key_id WHERE s.id=$1 AND s.deleted_at IS NULL AND s.transferred_at IS NULL`, m.ID).Scan(&host, &user, &port, &key); err != nil {
		return err
	}
	if a.Cipher != nil && key != "" {
		plain, err := a.Cipher.DecryptLaravelPayload(key, false)
		if err != nil {
			return fmt.Errorf("cannot decrypt server private key")
		}
		key = string(plain)
	}
	if strings.TrimSpace(host) == "" || strings.TrimSpace(user) == "" || port < 1 || port > 65535 {
		return fmt.Errorf("server SSH connection is incomplete")
	}
	op := "registry-" + a.Hooks.NewCUID2()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := a.Executor.Run(ctx, executor.ExecutionRequest{
		ProtocolVersion: executor.ProtocolVersionV1, FrameType: executor.FrameStart, JobID: op, OperationID: op, AttemptID: "att-1", OwnershipEpoch: 1, ResourceUUID: m.UUID, TeamID: m.TeamID, DeadlineMS: 30000, MaxFrameBytes: executor.DefaultMaxFrameBytes, ExpectedCapability: "ssh_exec", StdinData: input,
		Operation: executor.OperationSpec{Kind: "exec", Host: host, Port: port, User: user, SSHKeyContent: key, UseSudo: user != "root", Command: command},
	}, nil)
	if err != nil || result.Ambiguous || result.ExitCode != 0 {
		return fmt.Errorf("Docker registry command failed on the server; check the connection and registry credentials")
	}
	return nil
}

func registryLoginCommand(registry, username string) string {
	// Rust keeps SSH stdin open for control frames. Read one encoded line, then
	// close the pipeline so Docker's password-stdin reader receives EOF.
	return `IFS= read -r registry_password && printf '%s' "$registry_password" | base64 -d | docker login --username ` + shellSingleQuote(username) + " --password-stdin " + shellSingleQuote(registry)
}

func registryPasswordInput(password string) string {
	return base64.StdEncoding.EncodeToString([]byte(password)) + "\n"
}
