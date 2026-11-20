package auth

import (
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrUnsupportedHashFormat = errors.New("unsupported password hash format")
)

// PasswordVerifier verifies passwords hashed by Laravel (bcrypt $2y$/$2a$/$2b$ and Argon2id).
type PasswordVerifier struct{}

// NewPasswordVerifier creates a PasswordVerifier.
func NewPasswordVerifier() *PasswordVerifier {
	return &PasswordVerifier{}
}

// Verify checks plain against a Laravel-stored bcrypt or Argon2id password hash.
func (v *PasswordVerifier) Verify(hashed string, plain string) (bool, error) {
	if hashed == "" || plain == "" {
		return false, nil
	}
	if strings.HasPrefix(hashed, "$2y$") || strings.HasPrefix(hashed, "$2a$") || strings.HasPrefix(hashed, "$2b$") {
		normalized := hashed
		if strings.HasPrefix(hashed, "$2y$") {
			normalized = "$2a$" + hashed[4:]
		}
		err := bcrypt.CompareHashAndPassword([]byte(normalized), []byte(plain))
		if err == nil {
			return true, nil
		}
		if errors.Is(err, bcrypt.ErrMismatchedHashAndPassword) {
			return false, nil
		}
		return false, err
	}
	if strings.HasPrefix(hashed, "$argon2id$") {
		return verifyArgon2id(hashed, plain)
	}
	return false, fmt.Errorf("%w", ErrUnsupportedHashFormat)
}

func verifyArgon2id(encoded string, plain string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, ErrUnsupportedHashFormat
	}
	var memory uint32
	var iterations uint32
	var parallelism uint8
	for _, param := range strings.Split(parts[3], ",") {
		kv := strings.SplitN(param, "=", 2)
		if len(kv) != 2 {
			return false, ErrUnsupportedHashFormat
		}
		val, err := strconv.ParseUint(kv[1], 10, 32)
		if err != nil {
			return false, ErrUnsupportedHashFormat
		}
		switch kv[0] {
		case "m":
			memory = uint32(val)
		case "t":
			iterations = uint32(val)
		case "p":
			parallelism = uint8(val)
		}
	}
	if memory == 0 || iterations == 0 || parallelism == 0 {
		return false, ErrUnsupportedHashFormat
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, fmt.Errorf("decode argon2 salt: %w", err)
	}
	expected, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, fmt.Errorf("decode argon2 hash: %w", err)
	}
	actual := argon2.IDKey([]byte(plain), salt, iterations, memory, parallelism, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}
