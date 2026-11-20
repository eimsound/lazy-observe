package secret

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestLoadOrCreateGeneratesAndReusesKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "secret.key")

	first, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("first LoadOrCreate: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("key file missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode = %o, want 600", info.Mode().Perm())
	}

	second, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("second LoadOrCreate: %v", err)
	}
	if first.key != second.key {
		t.Fatal("LoadOrCreate regenerated the key instead of reusing the file")
	}

	// One-shot temp files are not left behind.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("stale tmp key file left behind: %s", e.Name())
		}
	}
}

func TestBoxEncryptDecryptRoundTrip(t *testing.T) {
	box, err := LoadOrCreate(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	values := []string{"", "sk-abc123", "deepseek-key-🀄", strings.Repeat("x", 4096)}
	for _, value := range values {
		sealed, err := box.Encrypt(value)
		if err != nil {
			t.Fatalf("Encrypt(%q): %v", value, err)
		}
		if sealed == value {
			t.Fatalf("Encrypt returned plaintext for %q", value)
		}
		opened, err := box.Decrypt(sealed)
		if err != nil {
			t.Fatalf("Decrypt(%q): %v", sealed, err)
		}
		if opened != value {
			t.Fatalf("round trip mismatch: got %q want %q", opened, value)
		}
	}
}

func TestEncryptIsRandomized(t *testing.T) {
	box, err := LoadOrCreate(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := box.Encrypt("same-value")
	if err != nil {
		t.Fatal(err)
	}
	b, err := box.Encrypt("same-value")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("Encrypt produced identical ciphertext for the same plaintext")
	}
}

func TestDecryptRejectsTampering(t *testing.T) {
	box, err := LoadOrCreate(filepath.Join(t.TempDir(), "secret.key"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := box.Encrypt("secret-value")
	if err != nil {
		t.Fatal(err)
	}
	// Flip a middle character: the final base64 char can be padding-only, but
	// a middle char always changes decoded bytes and breaks the GCM tag.
	tampered := sealed[:2] + flippedChar(sealed[2]) + sealed[3:]
	if _, err := box.Decrypt(tampered); err == nil {
		t.Fatal("Decrypt accepted tampered ciphertext")
	}
	if _, err := box.Decrypt("not-base64!"); err == nil {
		t.Fatal("Decrypt accepted garbage input")
	}
	if _, err := box.Decrypt(""); err == nil {
		t.Fatal("Decrypt accepted empty input")
	}
}

func TestKeysDoNotCross(t *testing.T) {
	a, err := LoadOrCreate(filepath.Join(t.TempDir(), "a.key"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreate(filepath.Join(t.TempDir(), "b.key"))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := a.Encrypt("cross-box")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Decrypt(sealed); err == nil {
		t.Fatal("box B decrypted a value sealed under box A's key")
	}
}

func TestLoadRejectsMalformedKeyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(path, []byte("too-short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(path); err == nil {
		t.Fatal("LoadOrCreate accepted a malformed key file")
	}
}

// TestLoadAcceptsProvisionedKeyEncodings covers key files that operators
// provision themselves (Kubernetes Secret, `openssl rand -base64 32`, echo):
// padded or unpadded standard base64 with surrounding whitespace must load the
// same key, and a read-only file must never be rewritten.
func TestLoadAcceptsProvisionedKeyEncodings(t *testing.T) {
	want := [keyBytes]byte{}
	for i := range want {
		want[i] = byte(i*7 + 1)
	}
	unpadded := base64.RawStdEncoding.EncodeToString(want[:])
	padded := base64.StdEncoding.EncodeToString(want[:])
	cases := map[string]string{
		"unpadded":                unpadded,
		"unpadded with newline":   unpadded + "\n",
		"padded":                  padded,
		"padded with newline":     padded + "\n",
		"padded with CRLF":        padded + "\r\n",
		"surrounding whitespace":  "  " + padded + " \n",
		"wrapped across two rows": padded[:20] + "\n" + padded[20:] + "\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secret.key")
			if err := os.WriteFile(path, []byte(content), 0o400); err != nil {
				t.Fatal(err)
			}
			box, err := LoadOrCreate(path)
			if err != nil {
				t.Fatalf("LoadOrCreate(%q): %v", content, err)
			}
			if box.key != want {
				t.Fatalf("LoadOrCreate(%q) loaded a different key", content)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != content {
				t.Fatalf("LoadOrCreate rewrote a provisioned key file: %q", got)
			}
		})
	}
}

func TestLoadRejectsWrongLengthPaddedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.key")
	short := base64.StdEncoding.EncodeToString(make([]byte, 16)) + "\n"
	if err := os.WriteFile(path, []byte(short), 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(path); err == nil {
		t.Fatal("LoadOrCreate accepted a 16-byte key")
	}
}

func flippedChar(c byte) string {
	if c == 'a' {
		return "b"
	}
	return "a"
}

// TestFingerprintIsStableAndOpaque pins the properties the metadata fingerprint
// relies on: the value identifies one key across reloads and never contains the
// key material itself.
func TestFingerprintIsStableAndOpaque(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrCreate(filepath.Join(dir, "a.key"))
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadOrCreate(filepath.Join(dir, "a.key"))
	if err != nil {
		t.Fatal(err)
	}
	other, err := LoadOrCreate(filepath.Join(dir, "b.key"))
	if err != nil {
		t.Fatal(err)
	}

	fingerprint := first.Fingerprint()
	if got := reloaded.Fingerprint(); got != fingerprint {
		t.Fatalf("fingerprint changed across reload: %q != %q", got, fingerprint)
	}
	if other.Fingerprint() == fingerprint {
		t.Fatal("two different keys produced the same fingerprint")
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		t.Fatalf("fingerprint %q is not hexadecimal: %v", fingerprint, err)
	}
	if len(fingerprint) != 32 {
		t.Fatalf("fingerprint length = %d, want 32 hex characters (128 bits)", len(fingerprint))
	}
	for _, encoding := range []string{
		base64.StdEncoding.EncodeToString(first.key[:]),
		base64.RawStdEncoding.EncodeToString(first.key[:]),
		hex.EncodeToString(first.key[:]),
	} {
		if strings.Contains(fingerprint, encoding) {
			t.Fatalf("fingerprint %q contains the key encoding %q", fingerprint, encoding)
		}
	}
}

// TestLoadNeverCreatesAKey pins the read-only contract the operator commands
// rely on: a missing key file is an error, not a fresh key, so a typo cannot
// silently replace the platform key.
func TestLoadNeverCreatesAKey(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.key")
	box, err := Load(missing)
	if err == nil {
		t.Fatalf("Load returned a box for a missing file: %#v", box)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Fatalf("Load error does not name the path %s: %v", missing, err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Load created the missing key file: stat error %v", err)
	}

	path := filepath.Join(t.TempDir(), "secret.key")
	created, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load on an existing file: %v", err)
	}
	if loaded.key != created.key {
		t.Fatal("Load read a different key than LoadOrCreate wrote")
	}
}

func TestLoadOrCreateConcurrentConvergesOnOneKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shared", "secret.key")
	const workers = 16
	start := make(chan struct{})
	boxes := make([]*Box, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			boxes[idx], errs[idx] = LoadOrCreate(path)
		}(i)
	}
	close(start)
	wg.Wait()

	var expected [keyBytes]byte
	for i := range workers {
		if errs[i] != nil {
			t.Fatalf("worker %d LoadOrCreate failed: %v", i, errs[i])
		}
		if i == 0 {
			expected = boxes[i].key
		} else if boxes[i].key != expected {
			t.Fatalf("worker %d loaded a different key from worker 0", i)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "secret.key" {
		t.Fatalf("unexpected files left in key dir: %v", entries)
	}
}
