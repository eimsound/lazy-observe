package meta

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

const (
	argonTime    = uint32(1)
	argonMemory  = uint32(64 * 1024)
	argonThreads = uint8(4)
	argonKeyLen  = uint32(32)
)

// HashPassword returns a self-describing Argon2id password hash.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate password salt: %w", err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	b64 := base64.RawStdEncoding
	return fmt.Sprintf(
		"$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s",
		argonMemory,
		argonTime,
		argonThreads,
		b64.EncodeToString(salt),
		b64.EncodeToString(key),
	), nil
}

// VerifyPassword checks a password against a HashPassword encoding.
func VerifyPassword(encoded, password string) bool {
	var memory, iterations uint32
	var threads uint8
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" {
		return false
	}
	if _, err := fmt.Sscanf(
		parts[3],
		"m=%d,t=%d,p=%d",
		&memory,
		&iterations,
		&threads,
	); err != nil || memory == 0 || iterations == 0 ||
		threads == 0 {
		return false
	}
	b64 := base64.RawStdEncoding
	salt, err := b64.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := b64.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	got := argon2.IDKey([]byte(password), salt, iterations, memory, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

var errInvalidCredentials = errors.New("invalid credentials")

// IsInvalidCredentials reports whether authentication failed because the
// supplied username/password pair was invalid. Callers must still return one
// generic response for missing users and incorrect passwords.
func IsInvalidCredentials(err error) bool {
	return errors.Is(err, errInvalidCredentials)
}

func argon2Dummy(password string) []byte {
	return argon2.IDKey(
		[]byte(password),
		[]byte("lazy-observe-dummy"),
		argonTime,
		argonMemory,
		argonThreads,
		argonKeyLen,
	)
}
