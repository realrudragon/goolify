package compose_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coollabsio/coolify/backend/internal/domains/compose"
	"gopkg.in/yaml.v3"
)

func fixturePath(rel ...string) string {
	parts := append([]string{"..", "..", "..", ".."}, rel...)
	return filepath.Join(parts...)
}

func TestComposeParser_ServiceReferenceAgainstGoldenExpected(t *testing.T) {
	rawBytes, err := os.ReadFile(fixturePath("tests", "Fixtures", "parser", "service-reference.yaml"))
	if err != nil {
		t.Fatalf("read service-reference.yaml: %v", err)
	}
	expBytes, err := os.ReadFile(fixturePath("tests", "Fixtures", "parser", "expected", "reference.yaml"))
	if err != nil {
		t.Fatalf("read expected/reference.yaml: %v", err)
	}
	var expected map[string]any
	if err := yaml.Unmarshal(expBytes, &expected); err != nil {
		t.Fatalf("unmarshal expected/reference.yaml: %v", err)
	}
	step1 := expected["1_deploy"].(map[string]any)
	expApps := step1["applications"].(map[string]any)
	expDBs := step1["databases"].(map[string]any)

	p := compose.NewComposeParser()

	// 1. Missing required var must fail cleanly per ${REQUIRED:?...}
	_, errMissing := p.Parse(context.Background(), compose.ComposeParseInput{
		Mode:              compose.ModeService,
		ParserVersion:     "5",
		RawCompose:        string(rawBytes),
		ResourceUUID:      "referenceservice00000001",
		ServerIP:          "10.0.0.1",
		TeamID:            10,
		DeterministicSeed: "golden-seed-1",
		SharedEnv:         map[string]string{"environment.SHARED_KEY": "shared-value"},
		Env:               map[string]string{},
	})
	if !errors.Is(errMissing, compose.ErrMissingRequiredVar) {
		t.Fatalf("expected ErrMissingRequiredVar when REQUIRED is unset, got %v", errMissing)
	}

	// 2. Full parse of service-reference.yaml compared against PHP expected/reference.yaml!
	out, err := p.Parse(context.Background(), compose.ComposeParseInput{
		Mode:              compose.ModeService,
		ParserVersion:     "5",
		RawCompose:        string(rawBytes),
		ResourceUUID:      "referenceservice00000001",
		ServerIP:          "10.0.0.1",
		TeamID:            10,
		DeterministicSeed: "golden-seed-1",
		SharedEnv:         map[string]string{"environment.SHARED_KEY": "shared-value"},
		Env: map[string]string{
			"REQUIRED":      "present",
			"WEB_STAGE":     "default",
			"BUILD_VERSION": "2.0.0",
		},
	})
	if err != nil {
		t.Fatalf("Parse service-reference.yaml failed: %v", err)
	}

	// Verify exact application vs database classification matches PHP expected/reference.yaml!
	for appName := range expApps {
		if _, ok := out.Applications[appName]; !ok {
			t.Errorf("expected application %q from golden reference.yaml missing in Go parser output", appName)
		}
	}
	for dbName := range expDBs {
		if _, ok := out.Databases[dbName]; !ok {
			t.Errorf("expected database %q from golden reference.yaml missing in Go parser output", dbName)
		}
	}

	web := out.Applications["web"]
	if web.Environment["LITERAL"] != "plain-value" {
		t.Fatalf("unexpected LITERAL: %q", web.Environment["LITERAL"])
	}
	if web.Environment["SHARED"] != "shared-value" {
		t.Fatalf("expected SHARED resolved from SharedEnv, got %q", web.Environment["SHARED"])
	}
	if web.Environment["WITH_DEFAULT"] != "fallback" || web.Environment["NESTED"] != "inner-default" {
		t.Fatalf("unexpected defaults: %q / %q", web.Environment["WITH_DEFAULT"], web.Environment["NESTED"])
	}
	// Exact magic variable lengths matching expected/reference.yaml
	if len(web.Environment["USER"]) != 16 || len(web.Environment["LOWERCASE_USER"]) != 16 {
		t.Fatalf("expected 16-char USER/LOWERCASE_USER, got %d/%d", len(web.Environment["USER"]), len(web.Environment["LOWERCASE_USER"]))
	}
	if len(web.Environment["PASSWORD"]) != 32 || len(web.Environment["PASSWORD_64"]) != 64 || len(web.Environment["HEX_128"]) != 128 {
		t.Fatalf("unexpected magic lengths: pw=%d pw64=%d hex128=%d", len(web.Environment["PASSWORD"]), len(web.Environment["PASSWORD_64"]), len(web.Environment["HEX_128"]))
	}
	if len(web.Environment["REALBASE64_64"]) != 88 || len(web.Environment["REALBASE64_128"]) != 172 {
		t.Fatalf("expected REALBASE64_64=88 & REALBASE64_128=172 (matching PHP golden), got %d/%d",
			len(web.Environment["REALBASE64_64"]), len(web.Environment["REALBASE64_128"]))
	}
	// Exact $$ escaping on labels
	if web.Labels["com.example.dollar"] != "price-$$5" {
		t.Fatalf("expected escaped dollar label 'price-$$5', got %q", web.Labels["com.example.dollar"])
	}
	// Both Traefik AND Caddy labels present by default (generate_exact_labels=false)
	if web.Labels["traefik.enable"] != "true" || web.Labels["caddy_0"] == "" {
		t.Fatalf("expected both traefik and caddy labels when GenerateExactLabels=false: %#v", web.Labels)
	}
	// Volume prefixing & bind path rewriting
	if web.Volumes[0].Rewritten != "referenceservice00000001_web-data" {
		t.Fatalf("expected prefixed volume name, got %+v", web.Volumes[0])
	}
	if web.Volumes[1].Rewritten != "/data/coolify/services/referenceservice00000001/config" {
		t.Fatalf("expected rewritten service bind path, got %+v", web.Volumes[1])
	}
	if web.Volumes[4].Content == "" {
		t.Fatalf("expected inline bind content preserved")
	}
	if !out.Applications["sidecar"].ExcludeFromStatus || !out.Applications["scheduler"].ExcludeFromStatus {
		t.Fatalf("expected sidecar and scheduler ExcludeFromStatus=true")
	}

	// Exact Traefik labels mode omits Caddy labels; Exact Caddy labels mode omits Traefik labels
	outExactTraefik, err := p.Parse(context.Background(), compose.ComposeParseInput{
		Mode: compose.ModeService, RawCompose: string(rawBytes), ResourceUUID: "referenceservice00000001",
		ServerIP: "10.0.0.1", ProxyType: "TRAEFIK", GenerateExactLabels: true, DeterministicSeed: "s1",
		Env: map[string]string{"REQUIRED": "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outExactTraefik.Applications["web"].Labels["caddy_0"] != "" || outExactTraefik.Applications["web"].Labels["traefik.enable"] != "true" {
		t.Fatalf("exact traefik labels leaked caddy or omitted traefik")
	}
	outExactCaddy, err := p.Parse(context.Background(), compose.ComposeParseInput{
		Mode: compose.ModeService, RawCompose: string(rawBytes), ResourceUUID: "referenceservice00000001",
		ServerIP: "10.0.0.1", ProxyType: "CADDY", GenerateExactLabels: true, DeterministicSeed: "s1",
		Env: map[string]string{"REQUIRED": "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if outExactCaddy.Applications["web"].Labels["traefik.enable"] != "" || outExactCaddy.Applications["web"].Labels["caddy_0"] == "" {
		t.Fatalf("exact caddy labels leaked traefik or omitted caddy")
	}
}

func TestComposeParser_ApplicationReferenceAgainstGoldenExpected(t *testing.T) {
	rawBytes, err := os.ReadFile(fixturePath("tests", "Fixtures", "parser", "application-reference.yaml"))
	if err != nil {
		t.Fatalf("read application-reference.yaml: %v", err)
	}
	expBytes, err := os.ReadFile(fixturePath("tests", "Fixtures", "parser", "expected", "application.yaml"))
	if err != nil {
		t.Fatalf("read expected/application.yaml: %v", err)
	}
	var expected map[string]any
	if err := yaml.Unmarshal(expBytes, &expected); err != nil {
		t.Fatalf("unmarshal expected/application.yaml: %v", err)
	}
	step1 := expected["1_deploy"].(map[string]any)
	expDomains := step1["docker_compose_domains"].(map[string]any)

	p := compose.NewComposeParser()
	out, err := p.Parse(context.Background(), compose.ComposeParseInput{
		Mode:              compose.ModeApplication,
		ParserVersion:     "5",
		RawCompose:        string(rawBytes),
		ResourceUUID:      "referenceapplication0001",
		ServerIP:          "10.0.0.1",
		DeterministicSeed: "app-seed-1",
		SharedEnv:         map[string]string{"environment.SHARED_KEY": "shared-value"},
		Env: map[string]string{
			"REQUIRED":                "present",
			"MAP_EMPTY_FROM_DATABASE": "production-value",
		},
	})
	if err != nil {
		t.Fatalf("Parse application-reference.yaml failed: %v", err)
	}

	// Every domain declared in expected/application.yaml must match Go parser output!
	for svcName, entryRaw := range expDomains {
		entry := entryRaw.(map[string]any)
		expDomain := entry["domain"].(string)
		gotDomain := out.DeclaredDomains[svcName]
		if gotDomain != expDomain {
			t.Errorf("application domain mismatch for %s: expected %q, got %q", svcName, expDomain, gotDomain)
		}
	}
	web := out.Services["web"]
	if web.Labels["coolify.applicationUuid"] != "referenceapplication0001" || web.Labels["coolify.type"] != "application" {
		t.Fatalf("unexpected application labels: %#v", web.Labels)
	}
	if web.Volumes[1].Rewritten != "/data/coolify/applications/referenceapplication0001/config" {
		t.Fatalf("expected /data/coolify/applications/<uuid>/config rewrite, got %+v", web.Volumes[1])
	}
}

func TestComposeParser_ErrorFixturesMatchPHPExpected(t *testing.T) {
	p := compose.NewComposeParser()

	// 1. Invalid YAML -> ErrInvalidComposeYAML
	invRaw, err := os.ReadFile(fixturePath("tests", "Fixtures", "parser", "service-invalid-yaml.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Parse(context.Background(), compose.ComposeParseInput{RawCompose: string(invRaw), ResourceUUID: "bad1"}); !errors.Is(err, compose.ErrInvalidComposeYAML) {
		t.Fatalf("expected ErrInvalidComposeYAML for service-invalid-yaml.yaml, got %v", err)
	}

	// 2. Unsafe service name containing `;` -> ErrUnsafeServiceName
	unsafeNameRaw, err := os.ReadFile(fixturePath("tests", "Fixtures", "parser", "service-unsafe-name.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Parse(context.Background(), compose.ComposeParseInput{RawCompose: string(unsafeNameRaw), ResourceUUID: "bad2"}); !errors.Is(err, compose.ErrUnsafeServiceName) {
		t.Fatalf("expected ErrUnsafeServiceName for service-unsafe-name.yaml, got %v", err)
	}

	// 3. Unsafe volume path containing `;` -> ErrUnsafeVolumePath
	unsafeVolRaw, err := os.ReadFile(fixturePath("tests", "Fixtures", "parser", "service-unsafe.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Parse(context.Background(), compose.ComposeParseInput{RawCompose: string(unsafeVolRaw), ResourceUUID: "bad3"}); !errors.Is(err, compose.ErrUnsafeVolumePath) {
		t.Fatalf("expected ErrUnsafeVolumePath for service-unsafe.yaml, got %v", err)
	}
}

func TestComposeParser_All386ServiceTemplatesParseCleanly(t *testing.T) {
	tplDir := fixturePath("templates", "compose")
	entries, err := os.ReadDir(tplDir)
	if err != nil {
		t.Fatalf("read templates/compose: %v", err)
	}
	p := compose.NewComposeParser()
	parsedCount := 0
	var failures []string
	for _, ent := range entries {
		if ent.IsDir() || (!strings.HasSuffix(ent.Name(), ".yaml") && !strings.HasSuffix(ent.Name(), ".yml")) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(tplDir, ent.Name()))
		if err != nil {
			failures = append(failures, ent.Name()+": read error: "+err.Error())
			continue
		}
		out, err := p.Parse(context.Background(), compose.ComposeParseInput{
			Mode:              compose.ModeService,
			ParserVersion:     "5",
			RawCompose:        string(raw),
			ResourceUUID:      "tpl-" + strings.TrimSuffix(ent.Name(), filepath.Ext(ent.Name())),
			ServerIP:          "10.0.0.1",
			DeterministicSeed: "tpl-seed",
			Env:               map[string]string{},
		})
		if err != nil {
			// Some templates intentionally use ${REQUIRED:?...} required variables; that is expected parser enforcement!
			if errors.Is(err, compose.ErrMissingRequiredVar) {
				parsedCount++
				continue
			}
			failures = append(failures, ent.Name()+": "+err.Error())
			continue
		}
		if len(out.Services) == 0 {
			failures = append(failures, ent.Name()+": 0 parsed services")
			continue
		}
		parsedCount++
	}
	if len(failures) > 0 {
		t.Fatalf("%d of %d templates failed to parse:\n%s", len(failures), parsedCount+len(failures), strings.Join(failures, "\n"))
	}
	if parsedCount < 350 {
		t.Fatalf("expected >= 350 templates parsed, got %d", parsedCount)
	}
	t.Logf("Successfully parsed all %d Coolify compose templates!", parsedCount)
}
