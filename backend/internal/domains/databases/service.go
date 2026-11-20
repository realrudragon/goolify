package databases

import (
	"context"
	"fmt"
	"slices"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
)

var SupportedDatabaseTypes = []string{
	"postgresql",
	"mysql",
	"mariadb",
	"mongodb",
	"redis",
	"clickhouse",
	"keydb",
	"dragonfly",
	"sqlite",
}

type DatabaseRecord struct {
	ID          int64
	UUID        string
	Name        string
	Type        string
	TeamID      int64
	ServerUUID  string
	Image       string
	CustomPort  int
	PasswordEnc string // Redacted from API views unless authorized read:sensitive
}

type ProvisionReceipt struct {
	DatabaseUUID string
	Type         string
	Status       string
}

type DatabaseService struct {
	authz *access.Authorizer
}

func NewDatabaseService(authz *access.Authorizer) *DatabaseService {
	return &DatabaseService{authz: authz}
}

func (s *DatabaseService) Provision(ctx context.Context, principal auth.Principal, db DatabaseRecord) (ProvisionReceipt, error) {
	if !slices.Contains(SupportedDatabaseTypes, db.Type) {
		return ProvisionReceipt{}, fmt.Errorf("unsupported database type %q", db.Type)
	}
	teamID := db.TeamID
	if err := s.authz.Check(ctx, principal, access.ActionDeploy, access.ResourceRef{
		Type: "database", ID: db.ID, TeamID: &teamID, APISegment: "databases",
	}); err != nil {
		return ProvisionReceipt{}, err
	}
	return ProvisionReceipt{
		DatabaseUUID: db.UUID,
		Type:         db.Type,
		Status:       "running",
	}, nil
}

func RedactDatabase(db DatabaseRecord) DatabaseRecord {
	db.PasswordEnc = ""
	return db
}
