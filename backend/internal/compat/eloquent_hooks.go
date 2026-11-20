package compat

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"math/big"
	"strings"
	"time"
)

const cuidAlphabet = "0123456789abcdefghijklmnopqrstuvwxyz"

var ErrUnknownModelType = errors.New("unknown model type for Eloquent lifecycle defaults")

// EloquentHooks reproduces BaseModel boot lifecycle hooks and attribute sanitizers in Go.
type EloquentHooks struct {
	now func() time.Time
}

func NewEloquentHooks() *EloquentHooks {
	return &EloquentHooks{now: time.Now}
}

// NewCUID2 generates a 24-character lowercase alphanumeric identifier matching Coolify's CUID2 format.
func (h *EloquentHooks) NewCUID2() string {
	var b strings.Builder
	b.Grow(24)
	// First character is always a lowercase letter a-z per CUID2 spec.
	firstIdx, err := rand.Int(rand.Reader, big.NewInt(26))
	if err != nil {
		b.WriteByte('c')
	} else {
		b.WriteByte(byte('a' + firstIdx.Int64()))
	}
	var tsBytes [8]byte
	binary.BigEndian.PutUint64(tsBytes[:], uint64(h.now().UnixNano()))
	for i := 1; i < 24; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(cuidAlphabet))))
		if err != nil {
			b.WriteByte(cuidAlphabet[(int(tsBytes[i%8])+i)%len(cuidAlphabet)])
			continue
		}
		b.WriteByte(cuidAlphabet[n.Int64()])
	}
	return b.String()
}

// SanitizeString trims leading/trailing whitespace matching bootstrap/helpers sanitize_string behavior.
func SanitizeString(v string) string {
	return strings.TrimSpace(v)
}

// ModelRow captures generic column defaults that Eloquent mutators/creating hooks populate.
type ModelRow struct {
	ID        *int64
	UUID      string
	Name      string
	Email     string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ApplyModelDefaults applies BaseModel creating hooks without overwriting sentinel id = 0 or preset UUIDs.
func (h *EloquentHooks) ApplyModelDefaults(modelType string, row *ModelRow) error {
	if row == nil {
		return errors.New("nil model row")
	}
	switch modelType {
	case "server", "application", "service", "project", "environment", "private_key",
		"standalone_postgresql", "standalone_mysql", "standalone_mariadb", "standalone_mongodb",
		"standalone_redis", "standalone_clickhouse", "standalone_keydb", "standalone_dragonfly",
		"standalone_sqlite", "scheduled_database_backup", "s3_storage":
		if row.UUID == "" {
			row.UUID = h.NewCUID2()
		}
	case "user":
		if row.Email != "" {
			row.Email = strings.ToLower(strings.TrimSpace(row.Email))
		}
	case "team", "instance_settings", "standalone_docker":
		// Preserve sentinel id = 0 explicitly if set; generate UUID for docker/team where applicable.
		if modelType == "standalone_docker" && row.UUID == "" {
			row.UUID = h.NewCUID2()
		}
	default:
		return ErrUnknownModelType
	}
	now := h.now().UTC()
	if row.CreatedAt.IsZero() {
		row.CreatedAt = now
	}
	if row.UpdatedAt.IsZero() {
		row.UpdatedAt = now
	}
	return nil
}
