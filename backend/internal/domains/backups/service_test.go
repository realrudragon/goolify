package backups_test

import (
	"context"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/domains/backups"
	"github.com/coollabsio/coolify/backend/internal/domains/databases"
)

func TestBackupService_AllTypesInstanceDBAndRetention(t *testing.T) {
	authz := access.NewAuthorizer()
	dbSvc := databases.NewDatabaseService(authz)
	bkSvc := backups.NewBackupService(authz)
	admin := auth.Principal{UserID: 0, AuthMethod: "session", IsInstanceAdmin: true, TeamRoles: map[int64]auth.Role{0: auth.RoleOwner, 10: auth.RoleAdmin}}
	ctx := context.Background()

	// Verify all 9 supported DB types can provision & run backup -> restore
	for _, dbType := range databases.SupportedDatabaseTypes {
		dbRec := databases.DatabaseRecord{ID: 50, UUID: "db-" + dbType, Name: dbType, Type: dbType, TeamID: 10}
		if _, err := dbSvc.Provision(ctx, admin, dbRec); err != nil {
			t.Fatalf("Provision %s failed: %v", dbType, err)
		}
		sched := backups.BackupSchedule{ID: 1, UUID: "sched-" + dbType, DatabaseID: 50, DatabaseUUID: dbRec.UUID, DatabaseType: dbType, TeamID: 10, RetentionCount: 2}
		rec, err := bkSvc.RunBackup(ctx, admin, sched, dbRec, "op-"+dbType+"-1")
		if err != nil {
			t.Fatalf("RunBackup %s: %v", dbType, err)
		}
		if err := bkSvc.RestoreBackup(ctx, admin, rec.ExecutionUUID, "sha256:verified-"+dbType+"-op-"+dbType+"-1"); err != nil {
			t.Fatalf("RestoreBackup %s: %v", dbType, err)
		}
	}

	// Instance DB coolify-db (id=0) resolved via relation even when schedule ID != 0
	instDB := databases.DatabaseRecord{ID: 0, UUID: "coolify-db-uuid", Name: "coolify-db", Type: "postgresql", TeamID: 0}
	sched := backups.BackupSchedule{ID: 42, UUID: "sched-inst", DatabaseID: 0, DatabaseUUID: "coolify-db-uuid", DatabaseType: "postgresql", TeamID: 0, RetentionCount: 1}
	rec, err := bkSvc.RunBackup(ctx, admin, sched, instDB, "op-inst-1")
	if err != nil || !rec.IsInstanceDB {
		t.Fatalf("expected IsInstanceDB=true via DB relation, got rec=%+v err=%v", rec, err)
	}
	rec2, err := bkSvc.RunBackup(ctx, admin, sched, instDB, "op-inst-2")
	if err != nil || rec2.PrunedCount != 1 {
		t.Fatalf("expected retention pruning to prune 1 old backup, got rec=%+v err=%v", rec2, err)
	}
}
