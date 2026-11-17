package secret

import (
	"os"
	"path/filepath"
	"strings"
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

func flippedChar(c byte) string {
	if c == 'a' {
		return "b"
	}
	return "a"
}
