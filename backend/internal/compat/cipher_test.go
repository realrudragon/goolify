package compat_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/coollabsio/coolify/backend/internal/compat"
)

func TestCipher_RoundTripAndMalformedPayloads(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "contracts", "data", "fixtures", "laravel_encrypted.json"))
	if err != nil {
		t.Fatalf("read cipher fixture: %v", err)
	}
	var fixture struct {
		AppKey  string `json:"synthetic_app_key"`
		Samples []struct {
			Name         string `json:"name"`
			SerializePHP bool   `json:"serialize_php"`
			Plaintext    string `json:"plaintext"`
		} `json:"samples"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}

	c, err := compat.NewCipher(fixture.AppKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	for _, s := range fixture.Samples {
		enc, err := c.EncryptLaravelPayload([]byte(s.Plaintext), s.SerializePHP)
		if err != nil {
			t.Fatalf("%s encrypt: %v", s.Name, err)
		}
		dec, err := c.DecryptLaravelPayload(enc, s.SerializePHP)
		if err != nil {
			t.Fatalf("%s decrypt: %v", s.Name, err)
		}
		if !bytes.Equal(dec, []byte(s.Plaintext)) {
			t.Fatalf("%s round-trip mismatch: got %q want %q", s.Name, string(dec), s.Plaintext)
		}
	}

	// Wrong key fails MAC verification
	other, _ := compat.NewCipher("base64:BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=")
	enc, _ := c.EncryptLaravelPayload([]byte("secret"), false)
	if _, err := other.DecryptLaravelPayload(enc, false); !errors.Is(err, compat.ErrInvalidMAC) {
		t.Fatalf("expected ErrInvalidMAC for wrong key, got %v", err)
	}

	// Malformed payload fails cleanly
	if _, err := c.DecryptLaravelPayload("not-base64!!!", false); !errors.Is(err, compat.ErrMalformedEnvelope) {
		t.Fatalf("expected ErrMalformedEnvelope, got %v", err)
	}
}

type memoryDataset struct {
	tables map[string][]compat.ValidationRecord
	uuids  map[string]bool
}

func (m *memoryDataset) ListTablesInOrder(_ context.Context) ([]string, error) {
	return []string{"instance_settings", "servers", "teams", "environment_variables"}, nil
}

func (m *memoryDataset) FetchPageStartingFromZero(_ context.Context, table string, afterID *int64, limit int) ([]compat.ValidationRecord, error) {
	var out []compat.ValidationRecord
	for _, r := range m.tables[table] {
		if afterID == nil || r.ID > *afterID {
			out = append(out, r)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *memoryDataset) ParentExists(_ context.Context, parentUUID string) (bool, error) {
	return m.uuids[parentUUID], nil
}

func TestValidateDataset_RootZeroAndWhitespace(t *testing.T) {
	empty := ""
	spaces := "  leading-and-trailing \n"
	now := time.Date(2026, 11, 19, 2, 0, 0, 123456000, time.UTC)

	ds := &memoryDataset{
		uuids: map[string]bool{"srv-0": true},
		tables: map[string][]compat.ValidationRecord{
			"teams":                 {{Table: "teams", ID: 0, UUID: "team-0", UpdatedAt: now}},
			"servers":               {{Table: "servers", ID: 0, UUID: "srv-0", UpdatedAt: now}},
			"instance_settings":     {{Table: "instance_settings", ID: 0, UUID: "inst-0", UpdatedAt: now}},
			"environment_variables": {
				{Table: "environment_variables", ID: 1, UUID: "ev-1", ParentUUID: "srv-0", SecretValue: nil, UpdatedAt: now},
				{Table: "environment_variables", ID: 2, UUID: "ev-2", ParentUUID: "srv-0", SecretValue: &empty, UpdatedAt: now},
				{Table: "environment_variables", ID: 3, UUID: "ev-3", ParentUUID: "srv-0", SecretValue: &spaces, UpdatedAt: now},
			},
		},
	}

	report, err := compat.ValidateDataset(context.Background(), ds, []byte("synthetic-digest-key"))
	if err != nil {
		t.Fatalf("ValidateDataset: %v", err)
	}
	for _, sentinelTbl := range []string{"teams", "servers", "instance_settings"} {
		if !report.SentinelZeroIDs[sentinelTbl] {
			t.Fatalf("expected sentinel id=0 to be preserved in %s", sentinelTbl)
		}
	}
	if report.NullSecretCount != 4 || report.EmptySecretCount != 1 {
		t.Fatalf("expected 4 null and 1 empty secret across fixture tables, got null=%d empty=%d", report.NullSecretCount, report.EmptySecretCount)
	}
	if len(report.BrokenFKs) != 0 {
		t.Fatalf("expected 0 broken FKs, got %v", report.BrokenFKs)
	}
}

func TestEloquentHooks_DefaultsAndCUID2(t *testing.T) {
	h := compat.NewEloquentHooks()
	id0 := int64(0)
	row := compat.ModelRow{ID: &id0, Name: " localhost "}
	if err := h.ApplyModelDefaults("server", &row); err != nil {
		t.Fatalf("ApplyModelDefaults: %v", err)
	}
	if len(row.UUID) != 24 || row.UUID[0] < 'a' || row.UUID[0] > 'z' {
		t.Fatalf("unexpected CUID2 UUID format: %q", row.UUID)
	}
	if *row.ID != 0 {
		t.Fatalf("sentinel id=0 must not be overwritten, got %d", *row.ID)
	}
}

func TestCipher_RealPHPInterop(t *testing.T) {
	phpBin, err := exec.LookPath("php")
	if err != nil {
		t.Skip("php binary not available on host")
	}
	appKey := "base64:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	c, err := compat.NewCipher(appKey)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}

	samples := []struct {
		name      string
		plaintext string
		serialize bool
	}{
		{name: "empty_string", plaintext: "", serialize: false},
		{name: "exact_whitespace", plaintext: "  leading-and-trailing \n", serialize: false},
		{name: "php_serialized_secret", plaintext: "  my-api-secret-value  ", serialize: true},
		{name: "multiline_pem_utf8", plaintext: "-----BEGIN OPENSSH PRIVATE KEY-----\nline1\nสวัสดี\n-----END OPENSSH PRIVATE KEY-----\n", serialize: false},
	}

	// 1. Encrypt in real PHP using exact Laravel Illuminate\Encryption\Encrypter algorithm, decrypt in Go.
	phpEncryptScript := `
$key = base64_decode(substr($argv[1], 7));
$plain = base64_decode($argv[2]);
$serialize = $argv[3] === '1';
$payload = $serialize ? serialize($plain) : $plain;
$ivRaw = random_bytes(16);
$value = openssl_encrypt($payload, 'aes-256-cbc', $key, 0, $ivRaw);
$iv = base64_encode($ivRaw);
$mac = hash_hmac('sha256', $iv . $value, $key);
echo base64_encode(json_encode(['iv' => $iv, 'value' => $value, 'mac' => $mac, 'tag' => '']));
`
	phpDecryptScript := `
$key = base64_decode(substr($argv[1], 7));
$env = json_decode(base64_decode($argv[2]), true);
$serialize = $argv[3] === '1';
$calc = hash_hmac('sha256', $env['iv'] . $env['value'], $key);
if (!hash_equals($calc, $env['mac'])) { fwrite(STDERR, "MAC mismatch"); exit(2); }
$raw = openssl_decrypt($env['value'], 'aes-256-cbc', $key, 0, base64_decode($env['iv']));
if ($raw === false) { fwrite(STDERR, "decrypt failed"); exit(3); }
$out = $serialize ? unserialize($raw) : $raw;
echo base64_encode($out);
`
	for _, s := range samples {
		serFlag := "0"
		if s.serialize {
			serFlag = "1"
		}
		plainB64 := base64.StdEncoding.EncodeToString([]byte(s.plaintext))
		out, err := exec.Command(phpBin, "-r", phpEncryptScript, "--", appKey, plainB64, serFlag).CombinedOutput()
		if err != nil {
			t.Fatalf("%s php encrypt failed: %v (%s)", s.name, err, string(out))
		}
		dec, err := c.DecryptLaravelPayload(string(out), s.serialize)
		if err != nil {
			t.Fatalf("%s Go decrypt of PHP ciphertext failed: %v", s.name, err)
		}
		if string(dec) != s.plaintext {
			t.Fatalf("%s PHP->Go mismatch: got %q want %q", s.name, string(dec), s.plaintext)
		}

		// 2. Encrypt in Go, verify HMAC & decrypt in real PHP.
		goEnc, err := c.EncryptLaravelPayload([]byte(s.plaintext), s.serialize)
		if err != nil {
			t.Fatalf("%s Go encrypt failed: %v", s.name, err)
		}
		phpOut, err := exec.Command(phpBin, "-r", phpDecryptScript, "--", appKey, goEnc, serFlag).CombinedOutput()
		if err != nil {
			t.Fatalf("%s PHP decrypt of Go ciphertext failed: %v (%s)", s.name, err, string(phpOut))
		}
		if string(phpOut) != plainB64 {
			t.Fatalf("%s Go->PHP mismatch: got b64 %q want %q", s.name, string(phpOut), plainB64)
		}
	}
}
