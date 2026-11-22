//go:build linux && (amd64 || arm64)

package bins

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
)

func TestParseProcMapsLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want procMapEntry
		ok   bool
	}{
		{
			name: "standard read-only mapping",
			line: "02247000-07480000 r--p 01e47000 08:01 123456   /usr/local/bin/lazy-observe",
			want: procMapEntry{
				start:    0x02247000,
				end:      0x07480000,
				perms:    "r--p",
				offset:   0x01e47000,
				devMajor: 0x08,
				devMinor: 0x01,
				inode:    123456,
				path:     "/usr/local/bin/lazy-observe",
			},
			ok: true,
		},
		{
			name: "path with spaces and deleted suffix",
			line: "00400000-00500000 r--p 00010000 fe:10 99 /tmp/my dir/lazy observe (deleted)",
			want: procMapEntry{
				start:    0x00400000,
				end:      0x00500000,
				perms:    "r--p",
				offset:   0x00010000,
				devMajor: 0xfe,
				devMinor: 0x10,
				inode:    99,
				path:     "/tmp/my dir/lazy observe (deleted)",
			},
			ok: true,
		},
		{
			name: "anonymous mapping",
			line: "7fc67f4ce000-7fc67f900000 rw-p 00000000 00:00 0 ",
			want: procMapEntry{
				start: 0x7fc67f4ce000,
				end:   0x7fc67f900000,
				perms: "rw-p",
			},
			ok: true,
		},
		{
			name: "malformed line",
			line: "00400000-00400000 r--p 00000000 08:01 12",
			ok:   false,
		},
		{
			name: "too few fields",
			line: "00400000-00401000 r--p 00000000",
			ok:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseProcMapsLine(tc.line)
			if ok != tc.ok {
				t.Fatalf("parseProcMapsLine(%q) ok = %v, want %v", tc.line, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("parseProcMapsLine(%q) = %+v, want %+v", tc.line, got, tc.want)
			}
		})
	}
}

func TestSelectReadOnlyExeMappings(t *testing.T) {
	entries := []procMapEntry{
		{start: 0x1000, end: 0x2000, perms: "r-xp", devMajor: 8, devMinor: 1, inode: 42, path: "/app/bin"},
		{start: 0x2000, end: 0x5000, perms: "r--p", devMajor: 8, devMinor: 1, inode: 42, path: "/app/bin"},
		{start: 0x5000, end: 0x6000, perms: "rw-p", devMajor: 8, devMinor: 1, inode: 42, path: "/app/bin"},
		{start: 0x6000, end: 0x7000, perms: "r--s", devMajor: 8, devMinor: 1, inode: 42, path: "/app/bin"},
		{start: 0x7000, end: 0x8000, perms: "r--p", devMajor: 0, devMinor: 0, inode: 0, path: "/app/bin"},
		{start: 0x9000, end: 0xa000, perms: "r--p", devMajor: 0, devMinor: 99, inode: 777, path: "/app/bin"},
	}

	exact := selectReadOnlyExeMappings(entries, 8, 1, 42, "/app/bin")
	if len(exact) != 1 || exact[0].start != 0x2000 {
		t.Fatalf("exact match = %+v, want single r--p mapping at 0x2000", exact)
	}

	// Overlayfs fallback: stat(/proc/self/exe) dev/ino differs from lowerdir mapping dev/ino.
	fallback := selectReadOnlyExeMappings(entries, 0, 55, 9999, "/app/bin")
	if len(fallback) != 2 || fallback[0].perms != "r--p" || fallback[1].perms != "r--p" {
		t.Fatalf("fallback match = %+v, want both file-backed r--p mappings", fallback)
	}

	none := selectReadOnlyExeMappings(entries, 0, 55, 9999, "/other/bin")
	if len(none) != 0 {
		t.Fatalf("unexpected match for unrelated executable: %+v", none)
	}
}

func TestAlignSectionRangeInMapping(t *testing.T) {
	entry := procMapEntry{start: 0x200000, end: 0x280000, offset: 0x10000, perms: "r--p"}

	// Partial overlap: section starts mid-page at 0x11200, ends mid-page at 0x71800.
	addr, length, ok := alignSectionRangeInMapping(entry, 0x11200, 0x60600, 4096)
	if !ok {
		t.Fatal("expected valid alignment for 4KiB pages")
	}
	if addr != 0x202000 || length != 0x5f000 {
		t.Fatalf("4KiB aligned = (0x%x, 0x%x), want (0x202000, 0x5f000)", addr, length)
	}

	// 64KiB pages (arm64): inward rounding must stay strictly within section & VMA.
	addr64, len64, ok := alignSectionRangeInMapping(entry, 0x11200, 0x60600, 65536)
	if !ok {
		t.Fatal("expected valid alignment for 64KiB pages")
	}
	if addr64 != 0x210000 || len64 != 0x50000 {
		t.Fatalf("64KiB aligned = (0x%x, 0x%x), want (0x210000, 0x50000)", addr64, len64)
	}

	// Sub-page section must return false rather than crossing page boundary.
	if _, _, ok := alignSectionRangeInMapping(entry, 0x11200, 0x100, 4096); ok {
		t.Fatal("expected sub-page section to return ok=false")
	}

	// Disjoint section must return false.
	if _, _, ok := alignSectionRangeInMapping(entry, 0x90000, 0x10000, 4096); ok {
		t.Fatal("expected disjoint section to return ok=false")
	}

	// Overflow guard near MaxUint64 where vaStart + (pageSize-1) would wrap.
	high := procMapEntry{start: math.MaxUint64 - 0x800, end: math.MaxUint64, offset: 0, perms: "r--p"}
	if _, _, ok := alignSectionRangeInMapping(high, 0, 0x800, 4096); ok {
		t.Fatal("expected near-MaxUint64 range without full page to return ok=false")
	}
}

func findMappedFileVMA(t *testing.T, targetPath string) procMapEntry {
	t.Helper()
	data, err := os.ReadFile("/proc/self/maps")
	if err != nil {
		t.Fatalf("read /proc/self/maps: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		entry, ok := parseProcMapsLine(line)
		if ok && entry.path == targetPath && entry.perms == "r--p" {
			return entry
		}
	}
	t.Fatalf("mapped VMA for %s not found in /proc/self/maps", targetPath)
	return procMapEntry{}
}

func readVMARSSKB(t *testing.T, startAddr uint64) int {
	t.Helper()
	data, err := os.ReadFile("/proc/self/smaps")
	if err != nil {
		t.Fatalf("read /proc/self/smaps: %v", err)
	}
	targetPrefix := fmt.Sprintf("%x-", startAddr)
	inTarget := false
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, targetPrefix) {
			inTarget = true
			continue
		}
		if inTarget {
			if strings.HasPrefix(line, "Rss:") {
				fields := strings.Fields(line)
				if len(fields) < 2 {
					t.Fatalf("unexpected Rss line in smaps: %q", line)
				}
				kb, convErr := strconv.Atoi(fields[1])
				if convErr != nil {
					t.Fatalf("parse Rss kB from %q: %v", line, convErr)
				}
				return kb
			}
			if len(line) > 0 && !strings.Contains(line, ":") {
				break
			}
		}
	}
	t.Fatalf("VMA starting at %s not found in /proc/self/smaps", targetPrefix)
	return 0
}

func TestMadviseDontNeedReclaimsReadOnlyFileRSSAndPreservesData(t *testing.T) {
	const size = 8 * 1024 * 1024
	f, err := os.CreateTemp(t.TempDir(), "ro-file-rss-*")
	if err != nil {
		t.Fatalf("create temp file: %v", err)
	}
	defer f.Close()

	pattern := make([]byte, size)
	for i := range pattern {
		pattern[i] = byte((i*31 + 7) & 0xff)
	}
	wantDigest := sha256.Sum256(pattern)
	if _, err := f.Write(pattern); err != nil {
		t.Fatalf("write pattern: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync temp file: %v", err)
	}

	mapped, err := syscall.Mmap(int(f.Fd()), 0, size, syscall.PROT_READ, syscall.MAP_PRIVATE)
	if err != nil {
		t.Fatalf("mmap temp file: %v", err)
	}
	defer func() {
		_ = syscall.Munmap(mapped)
	}()

	// Fault every page into RSS by hashing the mapped slice directly.
	if got := sha256.Sum256(mapped); got != wantDigest {
		t.Fatalf("initial faulted digest = %x, want %x", got, wantDigest)
	}

	vma := findMappedFileVMA(t, f.Name())
	beforeKB := readVMARSSKB(t, vma.start)
	if beforeKB < 7*1024 {
		t.Fatalf("before madvise Rss = %d kB, want >= 7168 kB", beforeKB)
	}

	if err := madviseDontNeed(uintptr(vma.start), uintptr(vma.end-vma.start)); err != nil {
		t.Fatalf("madviseDontNeed failed: %v", err)
	}

	afterKB := readVMARSSKB(t, vma.start)
	if afterKB >= 1024 {
		t.Fatalf("after madvise Rss = %d kB, want < 1024 kB", afterKB)
	}

	// Verify read-only file-backed pages re-fault transparently with intact contents.
	gotDigest := sha256.Sum256(mapped)
	if gotDigest != wantDigest {
		t.Fatalf("re-faulted digest = %x, want %x", gotDigest, wantDigest)
	}
}

func TestReleaseEmbeddedReadOnlyPagesRepeatable(t *testing.T) {
	const sentinel = "lazy-observe-rodata-sentinel"
	if err := releaseEmbeddedReadOnlyPages(); err != nil {
		t.Fatalf("first releaseEmbeddedReadOnlyPages() error = %v", err)
	}
	if err := releaseEmbeddedReadOnlyPages(); err != nil {
		t.Fatalf("second releaseEmbeddedReadOnlyPages() error = %v", err)
	}
	if sentinel != "lazy-observe-rodata-sentinel" {
		t.Fatalf("unexpected sentinel value after madvise: %q", sentinel)
	}
}

func TestExtractOnlyReclaimLifecycle(t *testing.T) {
	payload := []byte("synthetic embedded storage binary")
	digest := sha256.Sum256(payload)
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	assets := fstest.MapFS{}
	var manifest bytes.Buffer
	for _, name := range requiredBinaries {
		fmt.Fprintf(&manifest, "%s\t%d\t%x\n", name, len(payload), digest)
		assets[name+".gz"] = &fstest.MapFile{Data: compressed.Bytes()}
	}
	assets[manifestName] = &fstest.MapFile{Data: manifest.Bytes()}

	origAssets := platformAssets
	origRelease := releasePlatformAssets
	t.Cleanup(func() {
		platformAssets = origAssets
		releasePlatformAssets = origRelease
	})

	calls := 0
	platformAssets = assets
	releasePlatformAssets = func() error {
		calls++
		return errors.New("simulated reclaim failure")
	}

	dir := t.TempDir()
	// First extraction must succeed despite reclaim error and invoke reclaim once.
	gotDir, err := ExtractOnly(dir, "vmagent-prod")
	if err != nil {
		t.Fatalf("ExtractOnly first run failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("reclaim calls after fresh extraction = %d, want 1", calls)
	}
	gotBytes, err := os.ReadFile(filepath.Join(gotDir, "vmagent-prod"))
	if err != nil || !bytes.Equal(gotBytes, payload) {
		t.Fatalf("extracted payload mismatch: err=%v len=%d", err, len(gotBytes))
	}

	// Second call reuses matching on-disk binary and must not invoke reclaim.
	if _, err := ExtractOnly(dir, "vmagent-prod"); err != nil {
		t.Fatalf("ExtractOnly cached run failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("reclaim calls after cache hit = %d, want 1", calls)
	}
}
