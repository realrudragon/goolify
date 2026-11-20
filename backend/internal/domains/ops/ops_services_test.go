package ops

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/store/postgres"
)

func TestOpsDomainServices_RealLogicAndPostgresEvaluation(t *testing.T) {
	// 1. DockerImageParser
	p1 := ParseDockerImage("ghcr.io/coollabsio/coolify:4.0.0-beta@sha256:abc123")
	if p1.Registry != "ghcr.io" || p1.Repository != "coollabsio/coolify" || p1.Tag != "4.0.0-beta" || p1.Digest != "sha256:abc123" {
		t.Fatalf("unexpected parse p1: %+v", p1)
	}
	p2 := ParseDockerImage("localhost:5000/myorg/api:v2")
	if p2.Registry != "localhost:5000" || p2.Repository != "myorg/api" || p2.Tag != "v2" {
		t.Fatalf("unexpected parse p2: %+v", p2)
	}
	p3 := ParseDockerImage("nginx")
	if p3.Registry != "docker.io" || p3.Repository != "nginx" || p3.Tag != "latest" {
		t.Fatalf("unexpected parse p3: %+v", p3)
	}

	// 2. ContainerStatusAggregator
	if got := AggregateContainerStatuses([]string{"running:healthy", "running:healthy"}); got != "running:healthy" {
		t.Fatalf("expected running:healthy, got %s", got)
	}
	if got := AggregateContainerStatuses([]string{"running:healthy", "running:unhealthy"}); got != "degraded:unhealthy" {
		t.Fatalf("expected degraded:unhealthy, got %s", got)
	}
	if got := AggregateContainerStatuses([]string{"running:healthy", "restarting"}); got != "restarting" {
		t.Fatalf("expected restarting, got %s", got)
	}
	if got := AggregateContainerStatuses([]string{"exited", "exited"}); got != "exited" {
		t.Fatalf("expected exited, got %s", got)
	}

	// 3. ConfigurationDiffer
	diffs := DiffConfigSnapshots(
		map[string]any{"ports": "3000", "branch": "main", "legacy": true},
		map[string]any{"ports": "8080", "branch": "main", "new_flag": "on"},
	)
	if len(diffs) != 3 {
		t.Fatalf("expected 3 diff entries, got %+v", diffs)
	}

	// 4. BuildDatabaseStartCommand for all 9 engines
	for _, eng := range []string{"postgresql", "mysql", "mariadb", "mongodb", "redis", "keydb", "dragonfly", "clickhouse", "sqlite"} {
		cmd := BuildDatabaseStartCommand(eng, "db-u1", "", "op-1")
		if eng == "sqlite" {
			if !strings.Contains(cmd, "mkdir -p") || strings.Contains(cmd, "docker run") {
				t.Fatalf("sqlite must use local dir without docker TCP container, got: %s", cmd)
			}
		} else if !strings.Contains(cmd, "coolify.db_type="+eng) {
			t.Fatalf("engine %s command missing db_type label: %s", eng, cmd)
		}
	}

	// 5. OpenAPI Spec Generator + ProxyPortParser + RepositoryDetector + SensitiveDataRedactor
	ports := ParseProxyPortMappings("80:8080,443:8443,3000")
	if len(ports) != 3 || ports[0].HostPort != 80 || ports[0].ContainerPort != 8080 || ports[2].HostPort != 3000 {
		t.Fatalf("unexpected port mappings: %+v", ports)
	}
	repo := DetectRepositoryInfo("git@github.com:coollabsio/coolify.git")
	if repo.Provider != "github" || repo.Owner != "coollabsio" || repo.Repo != "coolify" {
		t.Fatalf("unexpected repo detection: %+v", repo)
	}
	redacted := RedactSensitiveData("Authorization: Bearer abc123secret token=xyz999 custom=MY_SUPER_SECRET", "MY_SUPER_SECRET")
	if strings.Contains(redacted, "abc123secret") || strings.Contains(redacted, "xyz999") || strings.Contains(redacted, "MY_SUPER_SECRET") {
		t.Fatalf("sensitive data leaked after redaction: %s", redacted)
	}

	specBytes, err := GenerateOpenAPISpec("4.0.0-test")
	if err != nil {
		t.Fatal(err)
	}
	var spec map[string]any
	if err := json.Unmarshal(specBytes, &spec); err != nil || spec["openapi"] != "3.0.3" {
		t.Fatalf("invalid OpenAPI spec: %v", err)
	}

	// 6. Real PostgreSQL ImageScanEvaluator + FleetMetricsAggregator
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://coolify:coolify_test_pw@127.0.0.1:15432/coolify_test?sslmode=disable"
	}
	ctx := context.Background()
	pool, err := postgres.Connect(ctx, dsn)
	if err != nil {
		t.Skipf("isolated test postgres unavailable: %v", err)
	}
	defer pool.Close()
	_, _ = pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public;`)
	if err := postgres.EnsureBaselineSchema(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if err := postgres.SeedInstanceSentinels(ctx, pool, "root@coolify.test", "$2y$10$h", ""); err != nil {
		t.Fatal(err)
	}
	_, _ = pool.Exec(ctx, `INSERT INTO image_scan_policies (uuid, scope_type, scope_id, gate_mode, count_medium) VALUES ('pol-1', 'team', 0, 'block', FALSE)`)
	_, _ = pool.Exec(ctx, `INSERT INTO image_scan_ignores (uuid, team_id, vulnerability_id, reason) VALUES ('ign-1', 0, 'CVE-2026-0001', 'accepted risk')`)

	dec1, err := EvaluateImageScanGate(ctx, pool, 0, []ScanFinding{
		{ID: "CVE-2026-0001", Severity: "CRITICAL", Package: "openssl"},
		{ID: "CVE-2026-0002", Severity: "MEDIUM", Package: "zlib"},
	})
	if err != nil || !dec1.Allowed || dec1.IgnoredCount != 1 {
		t.Fatalf("ignored critical + uncounted medium should be allowed: %+v err=%v", dec1, err)
	}
	dec2, err := EvaluateImageScanGate(ctx, pool, 0, []ScanFinding{
		{ID: "CVE-2026-9999", Severity: "HIGH", Package: "curl"},
	})
	if err != nil || dec2.Allowed || dec2.Action != "block" {
		t.Fatalf("unignored HIGH under block policy must block: %+v err=%v", dec2, err)
	}

	_, _ = pool.Exec(ctx, `INSERT INTO go_server_metrics (server_id, cpu_percent, memory_percent, disk_percent) VALUES (0, 24.5, 51.0, 42.0)`)
	fm, err := AggregateFleetMetrics(ctx, pool, 0)
	if err != nil || fm.TotalServers != 1 || fm.SamplesConsidered != 1 || fm.AvgCPUPercent != 24.5 {
		t.Fatalf("unexpected fleet metrics: %+v err=%v", fm, err)
	}
}
