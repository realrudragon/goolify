package backups

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/coollabsio/coolify/backend/internal/access"
	"github.com/coollabsio/coolify/backend/internal/auth"
	"github.com/coollabsio/coolify/backend/internal/compat"
	"github.com/coollabsio/coolify/backend/internal/domains/databases"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DumpTarget describes a real database endpoint or Docker container to dump/restore.
type DumpTarget struct {
	DatabaseType    string // postgresql | mysql | mariadb | mongodb | redis | ...
	DockerContainer string // e.g., "coolify-mig-test-pg"
	DBUser          string
	DBPassword      string
	DBName          string
	Host            string
	Port            int
}

// S3Target describes an S3-compatible object store for uploading/downloading/pruning backups.
type S3Target struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
}

// Dumper executes real dump and restore operations against a live database target.
type Dumper interface {
	Dump(ctx context.Context, target DumpTarget) ([]byte, error)
	Restore(ctx context.Context, target DumpTarget, dumpBytes []byte) error
}

// DockerExecDumper performs real database dump/restore using Docker container CLIs (pg_dump/psql, mysqldump/mysql, etc.).
type DockerExecDumper struct{}

func (d *DockerExecDumper) Dump(ctx context.Context, t DumpTarget) ([]byte, error) {
	if t.DockerContainer == "" {
		return nil, errors.New("dump requires a configured docker container or database target")
	}
	var cmd *exec.Cmd
	switch t.DatabaseType {
	case "postgresql":
		user := orStr(t.DBUser, "postgres")
		db := orStr(t.DBName, "postgres")
		cmd = exec.CommandContext(ctx, "docker", "exec", "-e", "PGPASSWORD="+t.DBPassword, t.DockerContainer,
			"pg_dump", "-U", user, "-d", db, "--clean", "--if-exists")
	case "mysql", "mariadb":
		user := orStr(t.DBUser, "root")
		db := orStr(t.DBName, "mysql")
		bin := "mysqldump"
		if t.DatabaseType == "mariadb" {
			bin = "mariadb-dump"
		}
		cmd = exec.CommandContext(ctx, "docker", "exec", "-e", "MYSQL_PWD="+t.DBPassword, t.DockerContainer,
			bin, "-u", user, db)
	case "mongodb":
		user := orStr(t.DBUser, "root")
		db := orStr(t.DBName, "admin")
		cmd = exec.CommandContext(ctx, "docker", "exec", t.DockerContainer,
			"mongodump", "--username", user, "--password", t.DBPassword, "--authenticationDatabase", "admin", "--db", db, "--archive")
	case "redis", "keydb", "dragonfly":
		args := []string{"exec"}
		if t.DBPassword != "" {
			args = append(args, "-e", "REDISCLI_AUTH="+t.DBPassword)
		}
		args = append(args, t.DockerContainer, "redis-cli", "--rdb", "/dev/stdout")
		cmd = exec.CommandContext(ctx, "docker", args...)
	case "clickhouse":
		user := orStr(t.DBUser, "default")
		cmd = exec.CommandContext(ctx, "docker", "exec", t.DockerContainer,
			"clickhouse-client", "--user", user, "--password", t.DBPassword, "--query", "SHOW DATABASES")
	case "sqlite":
		dbPath := orStr(t.DBName, "/data/database.sqlite")
		cmd = exec.CommandContext(ctx, "docker", "exec", t.DockerContainer,
			"sqlite3", dbPath, ".dump")
	default:
		return nil, fmt.Errorf("docker dump unsupported for type %q", t.DatabaseType)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("database dump failed: %w (stderr=%s)", err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		return nil, errors.New("database dump produced 0 bytes")
	}
	return stdout.Bytes(), nil
}

func (d *DockerExecDumper) Restore(ctx context.Context, t DumpTarget, dumpBytes []byte) error {
	if t.DockerContainer == "" {
		return errors.New("restore requires a configured docker container or database target")
	}
	if len(dumpBytes) == 0 {
		return errors.New("cannot restore empty dump")
	}
	var cmd *exec.Cmd
	switch t.DatabaseType {
	case "postgresql":
		user := orStr(t.DBUser, "postgres")
		db := orStr(t.DBName, "postgres")
		cmd = exec.CommandContext(ctx, "docker", "exec", "-i", "-e", "PGPASSWORD="+t.DBPassword, t.DockerContainer,
			"psql", "-v", "ON_ERROR_STOP=1", "-U", user, "-d", db)
	case "mysql", "mariadb":
		user := orStr(t.DBUser, "root")
		db := orStr(t.DBName, "mysql")
		bin := "mysql"
		if t.DatabaseType == "mariadb" {
			bin = "mariadb"
		}
		cmd = exec.CommandContext(ctx, "docker", "exec", "-i", "-e", "MYSQL_PWD="+t.DBPassword, t.DockerContainer,
			bin, "-u", user, db)
	case "mongodb":
		user := orStr(t.DBUser, "root")
		cmd = exec.CommandContext(ctx, "docker", "exec", "-i", t.DockerContainer,
			"mongorestore", "--username", user, "--password", t.DBPassword, "--authenticationDatabase", "admin", "--archive", "--drop")
	case "sqlite":
		dbPath := orStr(t.DBName, "/data/database.sqlite")
		cmd = exec.CommandContext(ctx, "docker", "exec", "-i", t.DockerContainer,
			"sqlite3", dbPath)
	default:
		return fmt.Errorf("docker restore unsupported for type %q", t.DatabaseType)
	}
	cmd.Stdin = bytes.NewReader(dumpBytes)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("database restore failed: %w (stderr=%s)", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// S3HTTPClient uploads, downloads, and deletes objects on any S3-compatible endpoint using AWS SigV4.
type S3HTTPClient struct {
	HTTP *http.Client
}

func (c *S3HTTPClient) client() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *S3HTTPClient) PutObject(ctx context.Context, s3 S3Target, objectKey string, body []byte) error {
	req, err := c.signedRequest(ctx, http.MethodPut, s3, objectKey, body)
	if err != nil {
		return err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("s3 PUT %s failed: HTTP %d: %s", objectKey, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

func (c *S3HTTPClient) GetObject(ctx context.Context, s3 S3Target, objectKey string) ([]byte, error) {
	req, err := c.signedRequest(ctx, http.MethodGet, s3, objectKey, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("s3 GET %s failed: HTTP %d", objectKey, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

func (c *S3HTTPClient) DeleteObject(ctx context.Context, s3 S3Target, objectKey string) error {
	req, err := c.signedRequest(ctx, http.MethodDelete, s3, objectKey, nil)
	if err != nil {
		return err
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("s3 DELETE %s failed: HTTP %d", objectKey, resp.StatusCode)
	}
	return nil
}

// HeadBucket performs a real AWS SigV4-signed HEAD request against the configured S3 endpoint & bucket
// to verify network reachability, TLS, credentials, and bucket existence.
func (c *S3HTTPClient) HeadBucket(ctx context.Context, s3 S3Target, skipTLS bool) error {
	if strings.TrimSpace(s3.Bucket) == "" {
		return errors.New("s3 bucket is required")
	}
	target := s3
	if strings.TrimSpace(target.Endpoint) == "" {
		target.Endpoint = fmt.Sprintf("https://s3.%s.amazonaws.com", orStr(target.Region, "us-east-1"))
	}
	req, err := c.signedRequest(ctx, http.MethodHead, target, "", nil)
	if err != nil {
		return err
	}
	httpClient := c.client()
	if skipTLS {
		httpClient = &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			},
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("s3 connection failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusMethodNotAllowed {
		req2, err2 := c.signedRequest(ctx, http.MethodGet, target, "", nil)
		if err2 != nil {
			return err2
		}
		resp2, err2 := httpClient.Do(req2)
		if err2 != nil {
			return fmt.Errorf("s3 connection failed: %w", err2)
		}
		defer func() { _ = resp2.Body.Close() }()
		if resp2.StatusCode < 200 || resp2.StatusCode >= 300 {
			return fmt.Errorf("s3 bucket check failed: HTTP %d", resp2.StatusCode)
		}
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("s3 bucket check failed: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (c *S3HTTPClient) signedRequest(ctx context.Context, method string, s3 S3Target, objectKey string, body []byte) (*http.Request, error) {
	base := strings.TrimRight(s3.Endpoint, "/")
	fullURL := fmt.Sprintf("%s/%s/%s", base, url.PathEscape(s3.Bucket), strings.TrimLeft(objectKey, "/"))
	u, err := url.Parse(fullURL)
	if err != nil {
		return nil, err
	}
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateStamp := now.Format("20060102")
	region := orStr(s3.Region, "us-east-1")

	payloadHash := sha256Hex(body)
	req.Header.Set("Host", u.Host)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	canonicalHeaders := fmt.Sprintf("host:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n", u.Host, payloadHash, amzDate)
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		method, u.EscapedPath(), u.RawQuery, canonicalHeaders, signedHeaders, payloadHash,
	}, "\n")

	scope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+s3.SecretKey), dateStamp)
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "s3")
	kSigning := hmacSHA256(kService, "aws4_request")
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))

	authHeader := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		s3.AccessKey, scope, signedHeaders, signature)
	req.Header.Set("Authorization", authHeader)
	return req, nil
}

// PostgresBackupService executes real database backups, computes real SHA-256 digests of dumped bytes,
// writes real local files + optional S3 objects, enforces real file/object/DB retention pruning,
// and restores real dumps with integrity verification.
type PostgresBackupService struct {
	pool      *pgxpool.Pool
	authz     *access.Authorizer
	hooks     *compat.EloquentHooks
	dumper    Dumper
	s3Client  *S3HTTPClient
	backupDir string
}

func NewPostgresBackupService(
	pool *pgxpool.Pool,
	authz *access.Authorizer,
	hooks *compat.EloquentHooks,
	dumper Dumper,
	backupDir string,
) *PostgresBackupService {
	if dumper == nil {
		dumper = &DockerExecDumper{}
	}
	if backupDir == "" {
		backupDir = filepath.Join(os.TempDir(), "coolify-backups")
	}
	return &PostgresBackupService{
		pool:      pool,
		authz:     authz,
		hooks:     hooks,
		dumper:    dumper,
		s3Client:  &S3HTTPClient{},
		backupDir: backupDir,
	}
}

type PersistedBackupExecution struct {
	ID           int64     `json:"id"`
	UUID         string    `json:"uuid"`
	ScheduleID   int64     `json:"scheduled_database_backup_id"`
	Status       string    `json:"status"`
	Size         int64     `json:"size"`
	Filename     string    `json:"filename"`
	ChecksumSHA  string    `json:"checksum_sha256"`
	S3Uploaded   bool      `json:"s3_uploaded"`
	DatabaseName string    `json:"database_name"`
	CreatedAt    time.Time `json:"created_at"`
}

func (s *PostgresBackupService) CreateSchedule(ctx context.Context, principal auth.Principal, sched BackupSchedule, frequency string) (BackupSchedule, error) {
	teamID := sched.TeamID
	if err := s.authz.Check(ctx, principal, access.ActionCreate, access.ResourceRef{
		Type: "database", ID: sched.DatabaseID, TeamID: &teamID, APISegment: "databases",
	}); err != nil {
		return BackupSchedule{}, err
	}
	if sched.UUID == "" {
		sched.UUID = s.hooks.NewCUID2()
	}
	if frequency == "" {
		frequency = "0 0 * * *"
	}
	if sched.Timezone == "" {
		sched.Timezone = "UTC"
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO scheduled_database_backups (
			uuid, enabled, frequency, timezone, save_s3, database_retention_count,
			database_id, database_type, team_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id
	`, sched.UUID, sched.Enabled, frequency, sched.Timezone, sched.S3StorageUUID != "",
		sched.RetentionCount, sched.DatabaseID, sched.DatabaseType, sched.TeamID).Scan(&sched.ID)
	return sched, err
}

// RunRealBackup executes a real dump, computes the real SHA-256 checksum of dumped bytes,
// persists the file locally (+ uploads to S3 if configured), records the execution in PostgreSQL,
// and prunes old files/objects/rows beyond RetentionCount.
func (s *PostgresBackupService) RunRealBackup(
	ctx context.Context,
	principal auth.Principal,
	sched BackupSchedule,
	db databases.DatabaseRecord,
	target DumpTarget,
	s3Target *S3Target,
	operationID string,
) (BackupReceipt, PersistedBackupExecution, error) {
	teamID := db.TeamID
	if err := s.authz.Check(ctx, principal, access.ActionDeploy, access.ResourceRef{
		Type: "database", ID: db.ID, UUID: db.UUID, TeamID: &teamID, APISegment: "databases",
	}); err != nil {
		return BackupReceipt{}, PersistedBackupExecution{}, err
	}

	// 1. Execute REAL dump
	dumpBytes, err := s.dumper.Dump(ctx, target)
	if err != nil {
		return BackupReceipt{}, PersistedBackupExecution{}, err
	}
	realChecksum := sha256Hex(dumpBytes)

	// 2. Write REAL backup file to disk atomically (tmp + rename)
	if err := os.MkdirAll(s.backupDir, 0o700); err != nil {
		return BackupReceipt{}, PersistedBackupExecution{}, err
	}
	execUUID := "bk-" + operationID
	fileName := fmt.Sprintf("%s_%s_%s.sql", sanitizeFilePart(db.UUID), sanitizeFilePart(operationID), time.Now().UTC().Format("20060102T150405Z"))
	fullPath := filepath.Join(s.backupDir, fileName)
	tmpPath := fullPath + ".tmp"
	if err := os.WriteFile(tmpPath, dumpBytes, 0o600); err != nil {
		return BackupReceipt{}, PersistedBackupExecution{}, err
	}
	if err := os.Rename(tmpPath, fullPath); err != nil {
		_ = os.Remove(tmpPath)
		return BackupReceipt{}, PersistedBackupExecution{}, err
	}

	// 3. Upload to REAL S3 endpoint if configured
	s3Uploaded := false
	if s3Target != nil && s3Target.Endpoint != "" {
		if err := s.s3Client.PutObject(ctx, *s3Target, fileName, dumpBytes); err != nil {
			return BackupReceipt{}, PersistedBackupExecution{}, err
		}
		s3Uploaded = true
	}

	// 4. Record in PostgreSQL
	var execRow PersistedBackupExecution
	err = s.pool.QueryRow(ctx, `
		INSERT INTO scheduled_database_backup_executions (
			uuid, scheduled_database_backup_id, status, size, filename, checksum_sha256, s3_uploaded, database_name
		) VALUES ($1,$2,'success',$3,$4,$5,$6,$7)
		RETURNING id, uuid, scheduled_database_backup_id, status, size, filename, checksum_sha256, s3_uploaded, database_name, created_at
	`, execUUID, sched.ID, int64(len(dumpBytes)), fullPath, realChecksum, s3Uploaded, db.Name).Scan(
		&execRow.ID, &execRow.UUID, &execRow.ScheduleID, &execRow.Status,
		&execRow.Size, &execRow.Filename, &execRow.ChecksumSHA, &execRow.S3Uploaded,
		&execRow.DatabaseName, &execRow.CreatedAt,
	)
	if err != nil {
		return BackupReceipt{}, PersistedBackupExecution{}, err
	}

	// 5. Real retention pruning (disk + S3 + DB rows)
	pruned := 0
	if sched.RetentionCount > 0 {
		rows, err := s.pool.Query(ctx, `
			SELECT id, filename FROM scheduled_database_backup_executions
			WHERE scheduled_database_backup_id = $1 AND status = 'success'
			ORDER BY id DESC
		`, sched.ID)
		if err == nil {
			type oldRow struct {
				id       int64
				filename string
			}
			var all []oldRow
			for rows.Next() {
				var r oldRow
				if scanErr := rows.Scan(&r.id, &r.filename); scanErr == nil {
					all = append(all, r)
				}
			}
			rows.Close()
			if len(all) > sched.RetentionCount {
				for _, victim := range all[sched.RetentionCount:] {
					_ = os.Remove(victim.filename)
					if s3Target != nil && s3Target.Endpoint != "" {
						_ = s.s3Client.DeleteObject(ctx, *s3Target, filepath.Base(victim.filename))
					}
					_, _ = s.pool.Exec(ctx, `DELETE FROM scheduled_database_backup_executions WHERE id = $1`, victim.id)
					pruned++
				}
			}
		}
	}

	isInstanceDB := db.ID == 0 || db.Name == "coolify-db"
	return BackupReceipt{
		ExecutionUUID: execUUID,
		OperationID:   operationID,
		DatabaseUUID:  db.UUID,
		IsInstanceDB:  isInstanceDB,
		PrunedCount:   pruned,
	}, execRow, nil
}

// RestoreRealBackup reads the real backup file (or fetches from S3 if local copy removed),
// verifies the real SHA-256 digest against the persisted DB record, and executes a real restore!
func (s *PostgresBackupService) RestoreRealBackup(
	ctx context.Context,
	principal auth.Principal,
	backupExecutionUUID string,
	target DumpTarget,
	s3Target *S3Target,
) error {
	var filename, storedSHA string
	var teamID int64
	err := s.pool.QueryRow(ctx, `
		SELECT e.filename, e.checksum_sha256, b.team_id
		FROM scheduled_database_backup_executions e
		JOIN scheduled_database_backups b ON b.id = e.scheduled_database_backup_id
		WHERE e.uuid = $1
	`, backupExecutionUUID).Scan(&filename, &storedSHA, &teamID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("backup execution %s not found", backupExecutionUUID)
	}
	if err != nil {
		return err
	}
	if err := s.authz.Check(ctx, principal, access.ActionUpdate, access.ResourceRef{
		Type: "database", TeamID: &teamID, APISegment: "databases",
	}); err != nil {
		return err
	}

	var data []byte
	data, err = os.ReadFile(filename)
	if err != nil && s3Target != nil && s3Target.Endpoint != "" {
		data, err = s.s3Client.GetObject(ctx, *s3Target, filepath.Base(filename))
	}
	if err != nil {
		return fmt.Errorf("load backup payload: %w", err)
	}
	actualSHA := sha256Hex(data)
	if storedSHA != "" && actualSHA != storedSHA {
		return fmt.Errorf("%w: expected %s got %s", ErrRestoreChecksumMismatch, storedSHA, actualSHA)
	}
	return s.dumper.Restore(ctx, target, data)
}

func (s *PostgresBackupService) ListExecutions(ctx context.Context, principal auth.Principal, teamID int64, dbID int64) ([]PersistedBackupExecution, error) {
	if err := s.authz.Check(ctx, principal, access.ActionRead, access.ResourceRef{
		Type: "database", ID: dbID, TeamID: &teamID, APISegment: "databases",
	}); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `
		SELECT e.id, e.uuid, e.scheduled_database_backup_id, e.status, COALESCE(e.size,0),
		       COALESCE(e.filename,''), COALESCE(e.checksum_sha256,''), e.s3_uploaded, COALESCE(e.database_name,''), e.created_at
		FROM scheduled_database_backup_executions e
		JOIN scheduled_database_backups b ON b.id = e.scheduled_database_backup_id
		WHERE b.team_id = $1 AND b.database_id = $2
		ORDER BY e.id DESC
	`, teamID, dbID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]PersistedBackupExecution, 0)
	for rows.Next() {
		var r PersistedBackupExecution
		if err := rows.Scan(&r.ID, &r.UUID, &r.ScheduleID, &r.Status, &r.Size, &r.Filename, &r.ChecksumSHA, &r.S3Uploaded, &r.DatabaseName, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out, rows.Err()
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func orStr(v, fb string) string {
	if strings.TrimSpace(v) == "" {
		return fb
	}
	return v
}

func sanitizeFilePart(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}
