package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ---- DockerImageParser (parity with app/Services/DockerImageParser.php) ----

type ParsedDockerImage struct {
	FullImage  string `json:"full_image"`
	Registry   string `json:"registry"`
	Repository string `json:"repository"`
	Tag        string `json:"tag"`
	Digest     string `json:"digest,omitempty"`
}

func ParseDockerImage(raw string) ParsedDockerImage {
	s := strings.TrimSpace(raw)
	res := ParsedDockerImage{FullImage: s, Registry: "docker.io", Tag: "latest"}
	if s == "" {
		return res
	}
	if idx := strings.Index(s, "@"); idx >= 0 {
		res.Digest = s[idx+1:]
		s = s[:idx]
	}
	// Separate tag carefully (avoid confusing registry:port with :tag)
	lastSlash := strings.LastIndex(s, "/")
	lastColon := strings.LastIndex(s, ":")
	if lastColon > lastSlash {
		res.Tag = s[lastColon+1:]
		s = s[:lastColon]
	}
	parts := strings.Split(s, "/")
	if len(parts) > 1 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") || parts[0] == "localhost") {
		res.Registry = parts[0]
		res.Repository = strings.Join(parts[1:], "/")
	} else {
		res.Repository = s
	}
	return res
}

// ---- ContainerStatusAggregator (parity with app/Services/ContainerStatusAggregator.php) ----

func AggregateContainerStatuses(statuses []string) string {
	if len(statuses) == 0 {
		return "exited"
	}
	runningHealthy := 0
	runningUnhealthy := 0
	runningUnknown := 0
	restarting := 0
	exited := 0
	for _, raw := range statuses {
		s := strings.ToLower(strings.TrimSpace(raw))
		switch {
		case strings.HasPrefix(s, "restarting"):
			restarting++
		case s == "running:healthy" || s == "running (healthy)":
			runningHealthy++
		case s == "running:unhealthy" || s == "running (unhealthy)":
			runningUnhealthy++
		case strings.HasPrefix(s, "running"):
			runningUnknown++
		default:
			exited++
		}
	}
	total := len(statuses)
	if restarting > 0 {
		return "restarting"
	}
	if runningHealthy == total {
		return "running:healthy"
	}
	if runningHealthy+runningUnknown == total {
		return "running:unknown"
	}
	if exited == total {
		return "exited"
	}
	if runningUnhealthy > 0 {
		return "degraded:unhealthy"
	}
	return "degraded"
}

// ---- Deployment Configuration Differ (parity with app/Services/DeploymentConfiguration/*) ----

type ConfigDiffEntry struct {
	Field  string `json:"field"`
	Before any    `json:"before,omitempty"`
	After  any    `json:"after,omitempty"`
	Kind   string `json:"kind"` // added | removed | changed
}

func DiffConfigSnapshots(before, after map[string]any) []ConfigDiffEntry {
	keys := make(map[string]struct{})
	for k := range before {
		keys[k] = struct{}{}
	}
	for k := range after {
		keys[k] = struct{}{}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	var out []ConfigDiffEntry
	for _, k := range sorted {
		bVal, hasB := before[k]
		aVal, hasA := after[k]
		if !hasB && hasA {
			out = append(out, ConfigDiffEntry{Field: k, After: aVal, Kind: "added"})
			continue
		}
		if hasB && !hasA {
			out = append(out, ConfigDiffEntry{Field: k, Before: bVal, Kind: "removed"})
			continue
		}
		bj, _ := json.Marshal(bVal)
		aj, _ := json.Marshal(aVal)
		if string(bj) != string(aj) {
			out = append(out, ConfigDiffEntry{Field: k, Before: bVal, After: aVal, Kind: "changed"})
		}
	}
	return out
}

// ---- ImageScanEvaluator (parity with app/Services/ImageScan/*) ----

type ScanFinding struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Package  string `json:"package"`
}

type ScanGateDecision struct {
	Allowed         bool          `json:"allowed"`
	Action          string        `json:"action"` // allow | warn | block
	BlockingCount   int           `json:"blocking_count"`
	IgnoredCount    int           `json:"ignored_count"`
	EffectiveCounts map[string]int `json:"effective_counts"`
}

func EvaluateImageScanGate(ctx context.Context, pool *pgxpool.Pool, teamID int64, findings []ScanFinding) (ScanGateDecision, error) {
	gateMode := "warn"
	countMedium := false
	_ = pool.QueryRow(ctx, `
		SELECT COALESCE(gate_mode,'warn'), COALESCE(count_medium,FALSE)
		FROM image_scan_policies
		WHERE scope_type = 'team' AND scope_id = $1
	`, teamID).Scan(&gateMode, &countMedium)

	// Load active (non-expired) CVE / package ignore rules for this team
	ignoredCVEs := make(map[string]bool)
	ignoredPkgs := make(map[string]bool)
	rows, err := pool.Query(ctx, `
		SELECT COALESCE(vulnerability_id,''), COALESCE(package,'')
		FROM image_scan_ignores
		WHERE team_id = $1 AND (expires_at IS NULL OR expires_at > NOW())
	`, teamID)
	if err != nil {
		return ScanGateDecision{}, err
	}
	for rows.Next() {
		var vid, pkg string
		if rows.Scan(&vid, &pkg) == nil {
			if vid != "" {
				ignoredCVEs[strings.ToUpper(vid)] = true
			}
			if pkg != "" {
				ignoredPkgs[strings.ToLower(pkg)] = true
			}
		}
	}
	rows.Close()

	counts := map[string]int{"critical": 0, "high": 0, "medium": 0, "low": 0}
	ignored := 0
	for _, f := range findings {
		if ignoredCVEs[strings.ToUpper(f.ID)] || ignoredPkgs[strings.ToLower(f.Package)] {
			ignored++
			continue
		}
		sev := strings.ToLower(strings.TrimSpace(f.Severity))
		if _, ok := counts[sev]; ok {
			counts[sev]++
		}
	}

	blocking := counts["critical"] + counts["high"]
	if countMedium {
		blocking += counts["medium"]
	}
	decision := ScanGateDecision{
		Allowed:         true,
		Action:          "allow",
		BlockingCount:   blocking,
		IgnoredCount:    ignored,
		EffectiveCounts: counts,
	}
	if blocking > 0 {
		if gateMode == "block" {
			decision.Allowed = false
			decision.Action = "block"
		} else {
			decision.Action = "warn"
		}
	}
	return decision, nil
}

// ---- FleetMetricsAggregator (real PostgreSQL aggregation over go_server_metrics & servers) ----

type FleetMetricsSummary struct {
	TeamID            int64     `json:"team_id"`
	TotalServers      int       `json:"total_servers"`
	ReachableServers  int       `json:"reachable_servers"`
	AvgCPUPercent     float64   `json:"avg_cpu_percent"`
	AvgMemoryPercent  float64   `json:"avg_memory_percent"`
	AvgDiskPercent    float64   `json:"avg_disk_percent"`
	SamplesConsidered int       `json:"samples_considered"`
	ComputedAt        time.Time `json:"computed_at"`
}

func AggregateFleetMetrics(ctx context.Context, pool *pgxpool.Pool, teamID int64) (FleetMetricsSummary, error) {
	out := FleetMetricsSummary{TeamID: teamID, ComputedAt: time.Now().UTC()}
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE ss.is_reachable = TRUE)
		FROM servers s
		LEFT JOIN server_settings ss ON ss.server_id = s.id
		WHERE s.team_id = $1 AND s.deleted_at IS NULL AND s.transferred_at IS NULL
	`, teamID).Scan(&out.TotalServers, &out.ReachableServers); err != nil {
		return out, err
	}
	_ = pool.QueryRow(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (m.server_id) m.cpu_percent, m.memory_percent, m.disk_percent
			FROM go_server_metrics m
			JOIN servers s ON s.id = m.server_id
			WHERE s.team_id = $1 AND s.deleted_at IS NULL AND s.transferred_at IS NULL
			ORDER BY m.server_id, m.recorded_at DESC
		)
		SELECT COALESCE(AVG(cpu_percent), 0), COALESCE(AVG(memory_percent), 0), COALESCE(AVG(disk_percent), 0), COUNT(*)
		FROM latest
	`, teamID).Scan(&out.AvgCPUPercent, &out.AvgMemoryPercent, &out.AvgDiskPercent, &out.SamplesConsidered)
	return out, nil
}

// GenerateOpenAPISpec builds a valid OpenAPI 3.0.3 JSON specification describing the Go standalone REST API v1.
func GenerateOpenAPISpec(version string) ([]byte, error) {
	if version == "" {
		version = "4.0.0-standalone"
	}
	paths := map[string]any{
		"/api/v1/health":                            map[string]any{"get": map[string]any{"summary": "Health check"}},
		"/api/v1/version":                           map[string]any{"get": map[string]any{"summary": "Coolify version"}},
		"/api/v1/servers":                           map[string]any{"get": map[string]any{"summary": "List servers"}, "post": map[string]any{"summary": "Create server"}},
		"/api/v1/servers/{uuid}":                    map[string]any{"get": map[string]any{"summary": "Get server"}, "patch": map[string]any{"summary": "Update server"}, "delete": map[string]any{"summary": "Delete server"}},
		"/api/v1/servers/{uuid}/docker-cleanup":     map[string]any{"get": map[string]any{"summary": "Get cleanup settings"}, "patch": map[string]any{"summary": "Update cleanup settings"}, "post": map[string]any{"summary": "Run cleanup"}},
		"/api/v1/servers/{uuid}/certificates":       map[string]any{"get": map[string]any{"summary": "List SSL certificates"}, "post": map[string]any{"summary": "Create SSL certificate"}},
		"/api/v1/servers/{uuid}/github-runners":     map[string]any{"get": map[string]any{"summary": "Get GitHub runner config"}, "post": map[string]any{"summary": "Configure GitHub runner"}},
		"/api/v1/servers/{uuid}/host-scan":          map[string]any{"get": map[string]any{"summary": "Get host security scan"}, "post": map[string]any{"summary": "Record host security scan"}},
		"/api/v1/projects":                          map[string]any{"get": map[string]any{"summary": "List projects"}, "post": map[string]any{"summary": "Create project"}},
		"/api/v1/applications":                      map[string]any{"get": map[string]any{"summary": "List applications"}},
		"/api/v1/deploy":                            map[string]any{"get": map[string]any{"summary": "Deploy by UUID or tag"}, "post": map[string]any{"summary": "Deploy by UUID or tag"}},
		"/api/v1/deployments":                       map[string]any{"get": map[string]any{"summary": "List deployments"}},
		"/api/v1/services":                          map[string]any{"get": map[string]any{"summary": "List services"}, "post": map[string]any{"summary": "Create service"}},
		"/api/v1/databases":                         map[string]any{"get": map[string]any{"summary": "List standalone databases"}},
		"/api/v1/databases/{uuid}/backups":          map[string]any{"get": map[string]any{"summary": "List backup schedules"}, "post": map[string]any{"summary": "Create backup schedule or run backup"}},
		"/api/v1/s3-storages":                       map[string]any{"get": map[string]any{"summary": "List S3 storages"}, "post": map[string]any{"summary": "Create S3 storage"}},
		"/api/v1/security/keys":                     map[string]any{"get": map[string]any{"summary": "List private keys"}, "post": map[string]any{"summary": "Create private key"}},
		"/api/v1/security/integration-tokens":       map[string]any{"get": map[string]any{"summary": "List integration tokens"}, "post": map[string]any{"summary": "Create integration token"}},
		"/api/v1/security/image-scan-policy":        map[string]any{"get": map[string]any{"summary": "Get image scan policy"}, "patch": map[string]any{"summary": "Update image scan policy"}},
		"/api/v1/security/image-scan-ignores":       map[string]any{"get": map[string]any{"summary": "List CVE ignore rules"}, "post": map[string]any{"summary": "Create CVE ignore rule"}},
		"/api/v1/vulnerabilities":                   map[string]any{"get": map[string]any{"summary": "Fleet vulnerabilities"}},
		"/api/v1/fleet/metrics":                     map[string]any{"get": map[string]any{"summary": "Aggregated fleet metrics from PostgreSQL"}},
		"/api/v1/notifications":                     map[string]any{"get": map[string]any{"summary": "List team notification channels"}},
		"/api/v1/notifications/{channel}":           map[string]any{"get": map[string]any{"summary": "Get channel config"}, "patch": map[string]any{"summary": "Update channel config"}},
		"/api/v1/teams":                             map[string]any{"get": map[string]any{"summary": "List teams"}},
		"/api/v1/audit-events":                      map[string]any{"get": map[string]any{"summary": "List team audit events"}},
		"/api/v1/subscription":                      map[string]any{"get": map[string]any{"summary": "Get team subscription status"}},
		"/api/v1/stars":                             map[string]any{"get": map[string]any{"summary": "List starred resources"}, "post": map[string]any{"summary": "Star resource"}},
		"/api/v1/changelog/reads":                   map[string]any{"get": map[string]any{"summary": "List changelog reads"}, "post": map[string]any{"summary": "Mark changelog tag read"}},
	}
	doc := map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":   "Coolify Standalone REST API",
			"version": version,
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{"type": "http", "scheme": "bearer"},
			},
		},
		"paths": paths,
	}
	return json.MarshalIndent(doc, "", "  ")
}

// DatabaseStartSpec holds the full runtime configuration for starting a standalone database container.
type DatabaseStartSpec struct {
	DBType       string
	ResourceUUID string
	Image        string
	OperationID  string
	Network      string
	Password     string
	IsPublic     bool
	PublicPort   int
}

// BuildDatabaseStartCommand generates engine-specific real Docker commands for all 9 standalone database types
// (parity with app/Actions/Database/Start{Postgresql,Mysql,Mariadb,Mongodb,Redis,Keydb,Dragonfly,Clickhouse,Sqlite}.php).
func BuildDatabaseStartCommand(dbType, resourceUUID, image, opID string) string {
	return BuildDatabaseStartCommandFull(DatabaseStartSpec{
		DBType:       dbType,
		ResourceUUID: resourceUUID,
		Image:        image,
		OperationID:  opID,
		Network:      "coolify",
	})
}

func BuildDatabaseStartCommandFull(spec DatabaseStartSpec) string {
	safeUUID := strings.ReplaceAll(spec.ResourceUUID, "'", "")
	safeOp := strings.ReplaceAll(spec.OperationID, "'", "")
	safeType := strings.ReplaceAll(spec.DBType, "'", "")
	netName := strings.ReplaceAll(strings.TrimSpace(spec.Network), "'", "")
	if netName == "" {
		netName = "coolify"
	}
	image := strings.TrimSpace(spec.Image)
	if image == "" {
		switch spec.DBType {
		case "postgresql":
			image = "postgres:16-alpine"
		case "mysql":
			image = "mysql:8"
		case "mariadb":
			image = "mariadb:11"
		case "mongodb":
			image = "mongo:7"
		case "redis":
			image = "redis:7-alpine"
		case "keydb":
			image = "eqalpha/keydb:latest"
		case "dragonfly":
			image = "docker.dragonflydb.io/dragonflydb/dragonfly:latest"
		case "clickhouse":
			image = "clickhouse/clickhouse-server:latest"
		case "sqlite":
			return fmt.Sprintf("mkdir -p '/data/coolify/databases/%s' && chmod 750 '/data/coolify/databases/%s'", safeUUID, safeUUID)
		default:
			image = "postgres:16-alpine"
		}
	}
	if spec.DBType == "sqlite" {
		return fmt.Sprintf("mkdir -p '/data/coolify/databases/%s' && chmod 750 '/data/coolify/databases/%s'", safeUUID, safeUUID)
	}
	safeImg := strings.ReplaceAll(image, "'", "")
	dataDir := "/data/coolify/databases/" + safeUUID
	pw := spec.Password
	if pw == "" {
		pw = "coolify-default-secret"
	}
	qPW := "'" + strings.ReplaceAll(pw, "'", `'\''`) + "'"

	var envFlags, mountTarget, extraArgs string
	var internalPort int
	switch spec.DBType {
	case "postgresql":
		internalPort = 5432
		mountTarget = "/var/lib/postgresql/data"
		envFlags = fmt.Sprintf("-e POSTGRES_USER=postgres -e POSTGRES_DB=postgres -e POSTGRES_PASSWORD=%s", qPW)
	case "mysql":
		internalPort = 3306
		mountTarget = "/var/lib/mysql"
		envFlags = fmt.Sprintf("-e MYSQL_ROOT_PASSWORD=%s -e MYSQL_USER=mysql -e MYSQL_PASSWORD=%s -e MYSQL_DATABASE=default", qPW, qPW)
	case "mariadb":
		internalPort = 3306
		mountTarget = "/var/lib/mysql"
		envFlags = fmt.Sprintf("-e MARIADB_ROOT_PASSWORD=%s -e MARIADB_USER=mariadb -e MARIADB_PASSWORD=%s -e MARIADB_DATABASE=default", qPW, qPW)
	case "mongodb":
		internalPort = 27017
		mountTarget = "/data/db"
		envFlags = fmt.Sprintf("-e MONGO_INITDB_ROOT_USERNAME=root -e MONGO_INITDB_ROOT_PASSWORD=%s -e MONGO_INITDB_DATABASE=default", qPW)
	case "redis":
		internalPort = 6379
		mountTarget = "/data"
		extraArgs = fmt.Sprintf(" redis-server --appendonly yes --requirepass %s", qPW)
	case "keydb":
		internalPort = 6379
		mountTarget = "/data"
		extraArgs = fmt.Sprintf(" keydb-server --appendonly yes --requirepass %s", qPW)
	case "dragonfly":
		internalPort = 6379
		mountTarget = "/data"
		extraArgs = fmt.Sprintf(" dragonfly --requirepass=%s", qPW)
	case "clickhouse":
		internalPort = 8123
		mountTarget = "/var/lib/clickhouse"
		envFlags = fmt.Sprintf("-e CLICKHOUSE_USER=default -e CLICKHOUSE_PASSWORD=%s", qPW)
	default:
		internalPort = 5432
		mountTarget = "/var/lib/postgresql/data"
		envFlags = fmt.Sprintf("-e POSTGRES_PASSWORD=%s", qPW)
	}

	portFlag := ""
	if spec.IsPublic && spec.PublicPort > 0 && internalPort > 0 {
		portFlag = fmt.Sprintf(" -p %d:%d", spec.PublicPort, internalPort)
	}
	volFlag := ""
	if mountTarget != "" {
		volFlag = fmt.Sprintf(" -v '%s:%s'", dataDir, mountTarget)
	}
	if envFlags != "" {
		envFlags = " " + envFlags
	}

	return fmt.Sprintf(
		"mkdir -p '%s' && (docker network inspect '%s' >/dev/null 2>&1 || docker network create --attachable '%s') && docker pull '%s' && (docker rm -f 'coolify-db-%s' >/dev/null 2>&1 || true) && docker run -d --restart unless-stopped --name 'coolify-db-%s' --network '%s'%s%s%s --label 'coolify.managed=true' --label 'coolify.db_type=%s' --label 'coolify.operation_id=%s' '%s'%s",
		dataDir, netName, netName, safeImg, safeUUID, safeUUID, netName, portFlag, volFlag, envFlags, safeType, safeOp, safeImg, extraArgs,
	)
}
