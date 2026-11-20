package compat

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

var (
	ErrInvalidAppKey      = errors.New("invalid APP_KEY")
	ErrMalformedEnvelope  = errors.New("malformed laravel encryption envelope")
	ErrInvalidMAC         = errors.New("laravel payload MAC verification failed")
	ErrInvalidPadding     = errors.New("invalid PKCS7 padding")
	ErrInvalidPHPSerialize = errors.New("invalid serialized PHP string payload")
)

type laravelEnvelope struct {
	IV    string `json:"iv"`
	Value string `json:"value"`
	MAC   string `json:"mac"`
	Tag   string `json:"tag,omitempty"`
}

// Cipher implements Laravel's Illuminate\Encryption\Encrypter (AES-256-CBC + HMAC-SHA256).
type Cipher struct {
	key []byte
}

// NewCipher parses a Laravel APP_KEY ("base64:..." or 32-byte raw key).
func NewCipher(appKey string) (*Cipher, error) {
	var raw []byte
	if strings.HasPrefix(appKey, "base64:") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(appKey, "base64:"))
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrInvalidAppKey, err)
		}
		raw = decoded
	} else {
		raw = []byte(appKey)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("%w: expected 32-byte AES-256 key, got %d bytes", ErrInvalidAppKey, len(raw))
	}
	cp := make([]byte, 32)
	copy(cp, raw)
	return &Cipher{key: cp}, nil
}

// EncryptLaravelPayload encrypts plain into a base64-encoded Laravel JSON envelope.
// When serializePHP is true, plain is wrapped as a PHP serialized string s:<len>:"<plain>";.
func (c *Cipher) EncryptLaravelPayload(plain []byte, serializePHP bool) (string, error) {
	payload := plain
	if serializePHP {
		payload = []byte(fmt.Sprintf(`s:%d:"%s";`, len(plain), string(plain)))
	}

	block, err := aes.NewCipher(c.key)
	if err != nil {
		return "", err
	}
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return "", err
	}

	padded := pkcs7Pad(payload, aes.BlockSize)
	ciphertext := make([]byte, len(padded))
	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(ciphertext, padded)

	ivB64 := base64.StdEncoding.EncodeToString(iv)
	valB64 := base64.StdEncoding.EncodeToString(ciphertext)
	mac := computeLaravelMAC(c.key, ivB64, valB64)

	env := laravelEnvelope{
		IV:    ivB64,
		Value: valB64,
		MAC:   mac,
	}
	rawJSON, err := json.Marshal(env)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(rawJSON), nil
}

// DecryptLaravelPayload validates HMAC-SHA256 and decrypts a Laravel encrypted value.
// Exact whitespace, empty string, and multiline bytes are preserved.
func (c *Cipher) DecryptLaravelPayload(raw string, unserializePHP bool) ([]byte, error) {
	envBytes, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: base64 decode: %v", ErrMalformedEnvelope, err)
	}
	var env laravelEnvelope
	if err := json.Unmarshal(envBytes, &env); err != nil {
		return nil, fmt.Errorf("%w: json decode: %v", ErrMalformedEnvelope, err)
	}
	if env.IV == "" || env.Value == "" || env.MAC == "" {
		return nil, ErrMalformedEnvelope
	}
	iv, err := base64.StdEncoding.DecodeString(env.IV)
	if err != nil || len(iv) != aes.BlockSize {
		return nil, ErrMalformedEnvelope
	}
	ciphertext, err := base64.StdEncoding.DecodeString(env.Value)
	if err != nil || len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, ErrMalformedEnvelope
	}

	expectedMACHex := computeLaravelMAC(c.key, env.IV, env.Value)
	gotMAC, err := hex.DecodeString(env.MAC)
	expectedMAC, err2 := hex.DecodeString(expectedMACHex)
	if err != nil || err2 != nil || !hmac.Equal(gotMAC, expectedMAC) {
		return nil, ErrInvalidMAC
	}

	block, err := aes.NewCipher(c.key)
	if err != nil {
		return nil, err
	}
	plainPadded := make([]byte, len(ciphertext))
	mode := cipher.NewCBCDecrypter(block, iv)
	mode.CryptBlocks(plainPadded, ciphertext)

	plain, err := pkcs7Unpad(plainPadded, aes.BlockSize)
	if err != nil {
		return nil, err
	}
	if unserializePHP {
		return unserializePHPString(plain)
	}
	return plain, nil
}

func computeLaravelMAC(key []byte, ivB64 string, valB64 string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(ivB64))
	mac.Write([]byte(valB64))
	return hex.EncodeToString(mac.Sum(nil))
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	padLen := blockSize - (len(data) % blockSize)
	out := make([]byte, len(data)+padLen)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(padLen)
	}
	return out
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, ErrInvalidPadding
	}
	padLen := int(data[len(data)-1])
	if padLen == 0 || padLen > blockSize || padLen > len(data) {
		return nil, ErrInvalidPadding
	}
	for i := len(data) - padLen; i < len(data); i++ {
		if int(data[i]) != padLen {
			return nil, ErrInvalidPadding
		}
	}
	return data[:len(data)-padLen], nil
}

func unserializePHPString(raw []byte) ([]byte, error) {
	s := string(raw)
	if !strings.HasPrefix(s, "s:") {
		return nil, ErrInvalidPHPSerialize
	}
	rest := strings.TrimPrefix(s, "s:")
	colonIdx := strings.IndexByte(rest, ':')
	if colonIdx <= 0 {
		return nil, ErrInvalidPHPSerialize
	}
	n, err := strconv.Atoi(rest[:colonIdx])
	if err != nil || n < 0 {
		return nil, ErrInvalidPHPSerialize
	}
	body := rest[colonIdx+1:]
	if len(body) != n+3 || body[0] != '"' || body[n+1] != '"' || body[n+2] != ';' {
		return nil, ErrInvalidPHPSerialize
	}
	return []byte(body[1 : n+1]), nil
}
