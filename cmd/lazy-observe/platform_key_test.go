package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appconfig "github.com/eimsound/lazy-observe/internal/config"
	"github.com/eimsound/lazy-observe/internal/meta"
)

// platformKeyTestConfig builds the smallest configuration openServerMetadata
// accepts: a temporary data directory with a local blob store and the platform
// key file inside it.
func platformKeyTestConfig(t *testing.T) (appconfig.Config, string) {
	t.Helper()
	dataDir := t.TempDir()
	keyFile := filepath.Join(dataDir, "secret.key")
	cfg := appconfig.Config{DataDir: dataDir}
	cfg.Blob.Backend = "local"
	cfg.Security.SecretKeyFile = keyFile
	return cfg, keyFile
}

func platformKeyTestLogger() (*slog.Logger, func() string) {
	var buffer bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buffer, nil))
	return logger, func() string { return buffer.String() }
}

// TestServerStartupRecordsAndVerifiesPlatformKeyFingerprint keeps the silent
// failure mode closed: the startup dependency function records the fingerprint
// on the first start, accepts the same key on the next one, and reports both in
// its log without ever writing key material.
func TestServerStartupRecordsAndVerifiesPlatformKeyFingerprint(t *testing.T) {
	cfg, keyFile := platformKeyTestConfig(t)
	logger, logs := platformKeyTestLogger()
	box, err := loadSecretBox(cfg)
	if err != nil {
		t.Fatalf("load platform key: %v", err)
	}

	first, err := openServerMetadata(context.Background(), cfg, logger, box)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	firstToken := first.selfToken
	first.Close()
	assertRecordedPlatformKeyFingerprint(t, cfg, box.Fingerprint())

	second, err := openServerMetadata(context.Background(), cfg, logger, box)
	if err != nil {
		t.Fatalf("second start with the same key: %v", err)
	}
	if second.selfToken != firstToken {
		t.Fatalf("self-monitoring token changed across identical starts: %q != %q", second.selfToken, firstToken)
	}
	second.Close()

	output := logs()
	if !strings.Contains(output, box.Fingerprint()) {
		t.Fatalf("startup log does not report the platform key fingerprint: %s", output)
	}
	if !strings.Contains(output, keyFile) {
		t.Fatalf("startup log does not name the key file %s: %s", keyFile, output)
	}
	assertNoPlatformKeyMaterial(t, output, readPlatformKeyFile(t, keyFile))
}

// TestServerStartupRefusesChangedPlatformKey is the behaviour this slice
// exists for: a swapped key (regenerated Secret, replica with another key)
// stops the start instead of silently disabling AI and re-deriving the
// self-monitoring token.
func TestServerStartupRefusesChangedPlatformKey(t *testing.T) {
	cfg, keyFile := platformKeyTestConfig(t)
	logger, logs := platformKeyTestLogger()
	original, err := loadSecretBox(cfg)
	if err != nil {
		t.Fatalf("load platform key: %v", err)
	}
	originalKey := readPlatformKeyFile(t, keyFile)
	metadata, err := openServerMetadata(context.Background(), cfg, logger, original)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	selfToken := metadata.selfToken
	metadata.Close()

	replacePlatformKeyFile(t, keyFile, 0x42)
	changed, err := loadSecretBox(cfg)
	if err != nil {
		t.Fatalf("load replaced platform key: %v", err)
	}
	changedKey := readPlatformKeyFile(t, keyFile)

	_, err = openServerMetadata(context.Background(), cfg, logger, changed)
	if err == nil {
		t.Fatal("startup accepted a platform key that differs from the recorded fingerprint")
	}
	message := err.Error()
	for _, want := range []string{
		keyFile,
		original.Fingerprint(),
		changed.Fingerprint(),
		"does not match",
		"regenerated",
		"reset-key-fingerprint",
		"can no longer be decrypted",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("refusal error does not contain %q: %s", want, message)
		}
	}
	assertNoPlatformKeyMaterial(t, message, originalKey, changedKey)
	assertNoPlatformKeyMaterial(t, logs(), originalKey, changedKey)

	// The refused start must not have re-derived the self-monitoring token:
	// the credential recorded by the first start still authenticates.
	store, err := openMetadataStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open metadata store: %v", err)
	}
	defer store.Close()
	if _, err := store.AuthenticateProjectAPIToken(context.Background(), selfToken); err != nil {
		t.Fatalf("self-monitoring token was replaced after the refused start: %v", err)
	}
}

func assertRecordedPlatformKeyFingerprint(t *testing.T, cfg appconfig.Config, want string) {
	t.Helper()
	store, err := openMetadataStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open metadata store: %v", err)
	}
	defer store.Close()
	// Asking for the same fingerprint must read the row back (created=false);
	// asking for another one must still report the recorded value.
	same, created, err := store.EnsurePlatformKeyFingerprint(context.Background(), want)
	if err != nil {
		t.Fatalf("read platform key fingerprint: %v", err)
	}
	if created {
		t.Fatal("startup did not record the platform key fingerprint")
	}
	if same.Fingerprint != want {
		t.Fatalf("recorded fingerprint = %q, want %q", same.Fingerprint, want)
	}
	other, created, err := store.EnsurePlatformKeyFingerprint(context.Background(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("read platform key fingerprint: %v", err)
	}
	if created {
		t.Fatal("a different fingerprint replaced the recorded one")
	}
	if other.Fingerprint != want {
		t.Fatalf("recorded fingerprint = %q after probing another value, want %q", other.Fingerprint, want)
	}
}

// readPlatformKeyFile decodes the key file so the assertions cover every
// encoding an operator could recognise the key by.
func readPlatformKeyFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read key file %s: %v", path, err)
	}
	encoded := strings.TrimSpace(string(raw))
	key, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(encoded, "="))
	if err != nil {
		t.Fatalf("decode key file %s: %v", path, err)
	}
	return key
}

// replacePlatformKeyFile writes a provisioned key file with the same encoding a
// deployer would (unpadded base64, mode 0600), imitating a regenerated Secret.
func replacePlatformKeyFile(t *testing.T, path string, fill byte) {
	t.Helper()
	key := bytes.Repeat([]byte{fill}, 32)
	encoded := base64.RawStdEncoding.EncodeToString(key)
	if err := os.WriteFile(path, []byte(encoded+"\n"), 0o600); err != nil {
		t.Fatalf("write key file %s: %v", path, err)
	}
}

// assertNoPlatformKeyMaterial fails when text carries the key in any encoding a
// log reader or an error report could recover it from. The fingerprint itself
// is allowed: it is a truncated one-way digest, not key material.
func assertNoPlatformKeyMaterial(t *testing.T, text string, keys ...[]byte) {
	t.Helper()
	for _, key := range keys {
		for _, material := range []string{
			base64.StdEncoding.EncodeToString(key),
			base64.RawStdEncoding.EncodeToString(key),
			hex.EncodeToString(key),
		} {
			if strings.Contains(text, material) {
				t.Fatalf("output leaks platform key material %q: %s", material, text)
			}
		}
	}
}

// TestAdminResetKeyFingerprintAcceptsRotation is the documented rotation exit:
// the refusal stays until an operator explicitly records the new key, and the
// command tells them what stops working.
func TestAdminResetKeyFingerprintAcceptsRotation(t *testing.T) {
	cfg, keyFile := platformKeyTestConfig(t)
	t.Setenv("LO_SECURITY_SECRET_KEY_FILE", keyFile)
	logger, _ := platformKeyTestLogger()
	original, err := loadSecretBox(cfg)
	if err != nil {
		t.Fatalf("load platform key: %v", err)
	}
	metadata, err := openServerMetadata(context.Background(), cfg, logger, original)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	metadata.Close()

	replacePlatformKeyFile(t, keyFile, 0x24)
	rotated, err := loadSecretBox(cfg)
	if err != nil {
		t.Fatalf("load rotated platform key: %v", err)
	}
	keyBefore, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openServerMetadata(context.Background(), cfg, logger, rotated); err == nil {
		t.Fatal("startup accepted the rotated key before the explicit reset")
	}

	output, err := runAdminResetKeyFingerprintCommand(t, "--data-dir", cfg.DataDir)
	if err != nil {
		t.Fatalf("admin reset-key-fingerprint: %v", err)
	}
	for _, want := range []string{keyFile, original.Fingerprint(), rotated.Fingerprint(), "can no longer be decrypted"} {
		if !strings.Contains(output, want) {
			t.Fatalf("reset output does not contain %q: %s", want, output)
		}
	}
	after, err := os.ReadFile(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, keyBefore) {
		t.Fatal("admin reset-key-fingerprint rewrote the key file")
	}

	restarted, err := openServerMetadata(context.Background(), cfg, logger, rotated)
	if err != nil {
		t.Fatalf("startup after the explicit reset: %v", err)
	}
	restarted.Close()

	store, err := openMetadataStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open metadata store: %v", err)
	}
	defer store.Close()
	events, err := store.ListAudit(context.Background(), meta.AuditListOptions{
		Action: "security.key_fingerprint_reset", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	event := events[0]
	if !event.Success || event.ActorUsername != "cli" || event.TargetName != rotated.Fingerprint() {
		t.Fatalf("audit event = %#v", event)
	}
	// changed_fields stays a comma-joined list of field names; the new
	// fingerprint is the target, and the replaced one has no column.
	if event.ChangedFields != "fingerprint" {
		t.Fatalf("audit changed_fields = %q, want %q", event.ChangedFields, "fingerprint")
	}
}

func runAdminResetKeyFingerprintCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	command := newRootCommand()
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs(append([]string{"admin", "reset-key-fingerprint"}, args...))
	err := command.Execute()
	return output.String(), err
}

// A key file that is not there (mistyped path, Secret not mounted) must never be
// replaced by a freshly generated one: that would record a fingerprint no real
// replica can match and take the whole deployment down.
func TestAdminResetKeyFingerprintRefusesMissingKeyFile(t *testing.T) {
	cfg, keyFile := platformKeyTestConfig(t)
	t.Setenv("LO_SECURITY_SECRET_KEY_FILE", keyFile)
	logger, _ := platformKeyTestLogger()
	box, err := loadSecretBox(cfg)
	if err != nil {
		t.Fatalf("load platform key: %v", err)
	}
	metadata, err := openServerMetadata(context.Background(), cfg, logger, box)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	metadata.Close()
	if err := os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}
	before, ok, err := recordedPlatformKeyFingerprint(t, cfg)
	if err != nil || !ok {
		t.Fatalf("read recorded fingerprint: %#v ok=%v err=%v", before, ok, err)
	}

	output, err := runAdminResetKeyFingerprintCommand(t, "--data-dir", cfg.DataDir)
	if err == nil {
		t.Fatalf("admin reset-key-fingerprint succeeded without a key file: %s", output)
	}
	if !strings.Contains(err.Error(), keyFile) || !strings.Contains(err.Error(), "security.secret_key_file") {
		t.Fatalf("missing key file error does not name the path and the setting: %v", err)
	}
	if _, statErr := os.Stat(keyFile); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("admin reset-key-fingerprint created the missing key file: %v", statErr)
	}
	after, ok, err := recordedPlatformKeyFingerprint(t, cfg)
	if err != nil || !ok {
		t.Fatalf("read recorded fingerprint: %#v ok=%v err=%v", after, ok, err)
	}
	if after.Fingerprint != before.Fingerprint || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("recorded fingerprint changed without a key file: %#v != %#v", after, before)
	}
}

// Rotating twice with the same key is a no-op: nothing is rewritten and no
// second audit event claims a rotation happened.
func TestAdminResetKeyFingerprintIsIdempotent(t *testing.T) {
	cfg, keyFile := platformKeyTestConfig(t)
	t.Setenv("LO_SECURITY_SECRET_KEY_FILE", keyFile)
	logger, _ := platformKeyTestLogger()
	box, err := loadSecretBox(cfg)
	if err != nil {
		t.Fatalf("load platform key: %v", err)
	}
	metadata, err := openServerMetadata(context.Background(), cfg, logger, box)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	metadata.Close()
	before, ok, err := recordedPlatformKeyFingerprint(t, cfg)
	if err != nil || !ok {
		t.Fatalf("read recorded fingerprint: %#v ok=%v err=%v", before, ok, err)
	}

	output, err := runAdminResetKeyFingerprintCommand(t, "--data-dir", cfg.DataDir)
	if err != nil {
		t.Fatalf("admin reset-key-fingerprint: %v", err)
	}
	if !strings.Contains(output, "already recorded") || !strings.Contains(output, box.Fingerprint()) {
		t.Fatalf("reset output does not report an already recorded fingerprint: %s", output)
	}
	after, ok, err := recordedPlatformKeyFingerprint(t, cfg)
	if err != nil || !ok {
		t.Fatalf("read recorded fingerprint: %#v ok=%v err=%v", after, ok, err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("already-recorded reset rewrote the row: %#v != %#v", after, before)
	}
	store, err := openMetadataStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open metadata store: %v", err)
	}
	defer store.Close()
	events, err := store.ListAudit(context.Background(), meta.AuditListOptions{
		Action: "security.key_fingerprint_reset", Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("already-recorded reset wrote %d audit events, want 0: %#v", len(events), events)
	}
}

func recordedPlatformKeyFingerprint(
	t *testing.T,
	cfg appconfig.Config,
) (meta.PlatformKeyFingerprint, bool, error) {
	t.Helper()
	store, err := openMetadataStore(context.Background(), cfg)
	if err != nil {
		t.Fatalf("open metadata store: %v", err)
	}
	defer store.Close()
	return store.RecordedPlatformKeyFingerprint(context.Background())
}

// Recording the first fingerprint is not a rotation: nothing was sealed under an
// earlier key, so the "values can no longer be decrypted" warning must not show.
func TestAdminResetKeyFingerprintFirstRecordSkipsRotationWarning(t *testing.T) {
	cfg, keyFile := platformKeyTestConfig(t)
	t.Setenv("LO_SECURITY_SECRET_KEY_FILE", keyFile)
	box, err := loadSecretBox(cfg)
	if err != nil {
		t.Fatalf("load platform key: %v", err)
	}

	output, err := runAdminResetKeyFingerprintCommand(t, "--data-dir", cfg.DataDir)
	if err != nil {
		t.Fatalf("admin reset-key-fingerprint: %v", err)
	}
	if !strings.Contains(output, "(first record)") || !strings.Contains(output, box.Fingerprint()) {
		t.Fatalf("first-record output = %q, want the new fingerprint and a first-record marker", output)
	}
	if strings.Contains(output, "can no longer be decrypted") {
		t.Fatalf("first-record output warns about a rotation that did not happen: %s", output)
	}
	recorded, ok, err := recordedPlatformKeyFingerprint(t, cfg)
	if err != nil || !ok {
		t.Fatalf("read recorded fingerprint: %#v ok=%v err=%v", recorded, ok, err)
	}
	if recorded.Fingerprint != box.Fingerprint() {
		t.Fatalf("recorded fingerprint = %q, want %q", recorded.Fingerprint, box.Fingerprint())
	}
}
