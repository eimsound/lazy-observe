// Package bins extracts the platform-specific storage executables embedded in
// a production lazy-observe binary.
package bins

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const manifestName = "manifest.tsv"

var requiredBinaries = []string{
	"victoria-metrics-prod",
	"victoria-logs-prod",
	"victoria-traces-prod",
	"vmagent-prod",
}

// Extract verifies and expands embedded gzip payloads into dir. Existing
// binaries with the expected size and digest are reused.
func Extract(dir string) (string, error) {
	if platformAssets == nil {
		return "", errors.New(
			"storage binaries are not embedded for this platform; use --storage-bin-dir or mise run dist",
		)
	}
	return extract(platformAssets, dir)
}

type manifestEntry struct {
	name   string
	size   int64
	digest [sha256.Size]byte
}

func extract(assets fs.FS, dir string) (string, error) {
	data, err := fs.ReadFile(assets, manifestName)
	if err != nil {
		return "", fmt.Errorf("read embedded storage manifest: %w (use --storage-bin-dir or mise run dist)", err)
	}
	entries, err := parseManifest(data)
	if err != nil {
		return "", err
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("resolve embedded binary directory: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		return "", fmt.Errorf("create embedded binary directory: %w", err)
	}
	for _, entry := range entries {
		dst := filepath.Join(absDir, entry.name)
		if matches(dst, entry) {
			if err := os.Chmod(dst, 0o755); err != nil {
				return "", fmt.Errorf("make %s executable: %w", entry.name, err)
			}
			continue
		}
		if err := extractOne(assets, dst, entry); err != nil {
			return "", err
		}
	}
	return absDir, nil
}

func parseManifest(data []byte) ([]manifestEntry, error) {
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	entries := make([]manifestEntry, 0, len(requiredBinaries))
	seen := make(map[string]bool)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("invalid embedded storage manifest line %q", line)
		}
		name := fields[0]
		if filepath.Base(name) != name || seen[name] {
			return nil, fmt.Errorf("invalid embedded storage binary name %q", name)
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || size <= 0 {
			return nil, fmt.Errorf("invalid embedded storage size for %s", name)
		}
		rawDigest, err := hex.DecodeString(fields[2])
		if err != nil || len(rawDigest) != sha256.Size {
			return nil, fmt.Errorf("invalid embedded storage SHA-256 for %s", name)
		}
		var digest [sha256.Size]byte
		copy(digest[:], rawDigest)
		entries = append(entries, manifestEntry{name: name, size: size, digest: digest})
		seen[name] = true
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan embedded storage manifest: %w", err)
	}
	if len(entries) != len(requiredBinaries) {
		return nil, fmt.Errorf(
			"embedded storage manifest contains %d binaries, want %d",
			len(entries),
			len(requiredBinaries),
		)
	}
	for _, name := range requiredBinaries {
		if !seen[name] {
			return nil, fmt.Errorf("embedded storage manifest is missing %s", name)
		}
	}
	return entries, nil
}

func matches(path string, entry manifestEntry) bool {
	info, err := os.Stat(path)
	if err != nil || info.Size() != entry.size || !info.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return false
	}
	return string(h.Sum(nil)) == string(entry.digest[:])
}

func extractOne(assets fs.FS, dst string, entry manifestEntry) (retErr error) {
	src, err := assets.Open(entry.name + ".gz")
	if err != nil {
		return fmt.Errorf("open embedded %s: %w", entry.name, err)
	}
	defer src.Close()
	gz, err := gzip.NewReader(src)
	if err != nil {
		return fmt.Errorf("open embedded %s gzip stream: %w", entry.name, err)
	}
	defer gz.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+entry.name+"-*")
	if err != nil {
		return fmt.Errorf("create temporary %s: %w", entry.name, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmpName)
		}
	}()
	h := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(tmp, h), gz)
	closeErr := tmp.Close()
	if copyErr != nil {
		return fmt.Errorf("extract embedded %s: %w", entry.name, copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close extracted %s: %w", entry.name, closeErr)
	}
	if n != entry.size || string(h.Sum(nil)) != string(entry.digest[:]) {
		return fmt.Errorf("embedded %s failed size or SHA-256 verification", entry.name)
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return fmt.Errorf("make extracted %s executable: %w", entry.name, err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("install extracted %s: %w", entry.name, err)
	}
	return nil
}
