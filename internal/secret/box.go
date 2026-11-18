// Package secret provides the platform-wide symmetric encryption key and an
// envelope (AES-256-GCM) for sensitive values stored in the metadata database,
// such as BYO LLM API keys.
//
// The key file is created on first startup with a fresh random 32-byte key
// (mode 0600) and is read once into memory for the process lifetime; it is
// never re-read per operation. HA deployments must share the same key file
// (or mount it read-only) so every replica can decrypt the same values.
//
// The file holds 32 bytes as standard base64, padded or unpadded; surrounding
// whitespace and line breaks are ignored, so `openssl rand -base64 32` output
// works as a provisioned key. An existing file is only read, never rewritten.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// keyBytes is the AES-256 key length.
const keyBytes = 32

// fingerprintLabel is the fixed HMAC message that separates the key fingerprint
// from every other use of the platform key (see Derive).
const fingerprintLabel = "lazy-observe/key-fingerprint/v1"

// fingerprintBytes is how much of the HMAC digest the fingerprint keeps. 128
// bits makes accidental collision impossible while leaking less of the digest
// than the full 256 bits would.
const fingerprintBytes = 16

// nonceBytes is the standard GCM nonce size (12 bytes).
const nonceBytes = 12

// Box encrypts and decrypts values under one in-memory AES-256 key.
type Box struct {
	key [32]byte
}

// LoadOrCreate reads the key file at path. When the file does not exist it is
// created with a fresh random key and restricted permissions (0600). The key
// stays in memory only; no file access happens on Encrypt/Decrypt.
func LoadOrCreate(path string) (*Box, error) {
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		return boxFromRaw(raw)
	case errors.Is(err, os.ErrNotExist):
		var box Box
		if _, err := rand.Read(box.key[:]); err != nil {
			return nil, fmt.Errorf("generate encryption key: %w", err)
		}
		if err := persist(path, box.key[:]); err != nil {
			return nil, err
		}
		return &box, nil
	default:
		return nil, fmt.Errorf("read encryption key file %s: %w", path, err)
	}
}

// Load reads an existing key file without ever creating one. Operator commands
// that must not change the platform key use it: a mistyped path or a Secret
// that was not mounted is an error, never a fresh key.
func Load(path string) (*Box, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read encryption key file %s: %w", path, err)
	}
	return boxFromRaw(raw)
}

// boxFromRaw decodes a key file. Generated files hold unpadded standard
// base64; operator-provisioned files (Kubernetes Secret, `openssl rand -base64
// 32`) may also be padded and carry whitespace or line breaks, all of which
// are ignored.
func boxFromRaw(raw []byte) (*Box, error) {
	encoded := strings.Join(strings.Fields(string(raw)), "")
	encoding := base64.RawStdEncoding
	if strings.HasSuffix(encoded, "=") {
		encoding = base64.StdEncoding
	}
	decoded, err := encoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode encryption key file: %w", err)
	}
	if len(decoded) != keyBytes {
		return nil, fmt.Errorf("encryption key file must contain %d bytes, got %d", keyBytes, len(decoded))
	}
	var box Box
	copy(box.key[:], decoded)
	return &box, nil
}

func persist(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create key directory: %w", err)
	}
	encoded := make([]byte, base64.RawStdEncoding.EncodedLen(len(raw)))
	base64.RawStdEncoding.Encode(encoded, raw)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, encoded, 0o600); err != nil {
		return fmt.Errorf("write encryption key file: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("persist encryption key file: %w", err)
	}
	return nil
}

// Encrypt seals plaintext into base64(nonce || ciphertext). The same value
// never produces the same ciphertext twice.
func (b *Box) Encrypt(plaintext string) (string, error) {
	block, err := aes.NewCipher(b.key[:])
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}
	nonce := make([]byte, nonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate nonce: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.RawStdEncoding.EncodeToString(sealed), nil
}

// Decrypt reverses Encrypt.
func (b *Box) Decrypt(value string) (string, error) {
	block, err := aes.NewCipher(b.key[:])
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create GCM: %w", err)
	}
	raw, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		return "", fmt.Errorf("decode sealed value: %w", err)
	}
	if len(raw) < nonceBytes {
		return "", errors.New("sealed value is truncated")
	}
	nonce, sealed := raw[:nonceBytes], raw[nonceBytes:]
	plaintext, err := aead.Open(nil, nonce, sealed, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt value: %w", err)
	}
	return string(plaintext), nil
}

// Fingerprint returns a stable identifier for the key: the first 16 bytes of
// HMAC-SHA256(key, fixed label) as lowercase hex. The metadata database records
// it so a swapped or per-replica-different key fails loudly at startup instead
// of silently disabling every sealed value.
//
// The value reveals no key material: it is a one-way digest, and recovering a
// 32-byte random key from 128 bits of an HMAC is infeasible. Like any
// deterministic fingerprint it would let somebody who already holds the
// database test a complete key guess, so it is truncated and never logged
// together with key material. Two boxes built from the same key file produce
// the same fingerprint; any other key produces a different one.
func (b *Box) Fingerprint() string {
	mac := hmac.New(sha256.New, b.key[:])
	_, _ = mac.Write([]byte(fingerprintLabel))
	return hex.EncodeToString(mac.Sum(nil)[:fingerprintBytes])
}

// Derive returns a stable 256-bit hex value bound to the platform key and
// purpose. Replicas sharing the key file derive the same value, so a credential
// derived from it never has to be stored in recoverable form.
func (b *Box) Derive(purpose string) string {
	mac := hmac.New(sha256.New, b.key[:])
	_, _ = mac.Write([]byte("lazy-observe/derive/v1\x00" + purpose))
	return hex.EncodeToString(mac.Sum(nil))
}
