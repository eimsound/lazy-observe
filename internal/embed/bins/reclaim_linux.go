//go:build linux && (amd64 || arm64)

package bins

import (
	"bufio"
	"debug/elf"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"syscall"
)

type procMapEntry struct {
	start    uint64
	end      uint64
	perms    string
	offset   uint64
	devMajor uint32
	devMinor uint32
	inode    uint64
	path     string
}

func linuxDevMajorMinor(dev uint64) (uint32, uint32) {
	major := uint32((dev>>8)&0xfff) | uint32((dev>>32)&^uint64(0xfff))
	minor := uint32(dev&0xff) | uint32((dev>>12)&^uint64(0xff))
	return major, minor
}

func nextMapsField(s string) (field, rest string, ok bool) {
	s = strings.TrimLeft(s, " \t")
	if s == "" {
		return "", "", false
	}
	idx := strings.IndexAny(s, " \t")
	if idx < 0 {
		return s, "", true
	}
	return s[:idx], s[idx:], true
}

func parseProcMapsLine(line string) (procMapEntry, bool) {
	addrRange, rest, ok := nextMapsField(line)
	if !ok {
		return procMapEntry{}, false
	}
	perms, rest, ok := nextMapsField(rest)
	if !ok {
		return procMapEntry{}, false
	}
	offsetRaw, rest, ok := nextMapsField(rest)
	if !ok {
		return procMapEntry{}, false
	}
	devRaw, rest, ok := nextMapsField(rest)
	if !ok {
		return procMapEntry{}, false
	}
	inodeRaw, rest, ok := nextMapsField(rest)
	if !ok {
		return procMapEntry{}, false
	}

	startRaw, endRaw, ok := strings.Cut(addrRange, "-")
	if !ok || startRaw == "" || endRaw == "" {
		return procMapEntry{}, false
	}
	start, err := strconv.ParseUint(startRaw, 16, 64)
	if err != nil {
		return procMapEntry{}, false
	}
	end, err := strconv.ParseUint(endRaw, 16, 64)
	if err != nil || end <= start {
		return procMapEntry{}, false
	}
	offset, err := strconv.ParseUint(offsetRaw, 16, 64)
	if err != nil {
		return procMapEntry{}, false
	}
	majRaw, minRaw, ok := strings.Cut(devRaw, ":")
	if !ok || majRaw == "" || minRaw == "" {
		return procMapEntry{}, false
	}
	maj, err := strconv.ParseUint(majRaw, 16, 32)
	if err != nil {
		return procMapEntry{}, false
	}
	minorNum, err := strconv.ParseUint(minRaw, 16, 32)
	if err != nil {
		return procMapEntry{}, false
	}
	inode, err := strconv.ParseUint(inodeRaw, 10, 64)
	if err != nil {
		return procMapEntry{}, false
	}

	return procMapEntry{
		start:    start,
		end:      end,
		perms:    perms,
		offset:   offset,
		devMajor: uint32(maj),
		devMinor: uint32(minorNum),
		inode:    inode,
		path:     strings.TrimSpace(rest),
	}, true
}

func selectReadOnlyExeMappings(
	entries []procMapEntry,
	devMajor, devMinor uint32,
	inode uint64,
	exePath string,
) []procMapEntry {
	var exact []procMapEntry
	var pathFallback []procMapEntry
	for _, e := range entries {
		if e.perms != "r--p" || e.inode == 0 {
			continue
		}
		if e.devMajor == devMajor && e.devMinor == devMinor && e.inode == inode {
			exact = append(exact, e)
			continue
		}
		if exePath != "" && e.path == exePath {
			pathFallback = append(pathFallback, e)
		}
	}
	if len(exact) > 0 {
		return exact
	}
	return pathFallback
}

func alignSectionRangeInMapping(
	entry procMapEntry,
	secOffset, secSize, pageSize uint64,
) (addr, length uintptr, ok bool) {
	if secSize == 0 || pageSize == 0 || pageSize&(pageSize-1) != 0 || entry.end <= entry.start {
		return 0, 0, false
	}
	vmaSize := entry.end - entry.start
	if entry.offset > math.MaxUint64-vmaSize || secOffset > math.MaxUint64-secSize {
		return 0, 0, false
	}
	vFileStart := entry.offset
	vFileEnd := entry.offset + vmaSize
	secEnd := secOffset + secSize

	ovStart := vFileStart
	if secOffset > ovStart {
		ovStart = secOffset
	}
	ovEnd := vFileEnd
	if secEnd < ovEnd {
		ovEnd = secEnd
	}
	if ovEnd <= ovStart {
		return 0, 0, false
	}

	vaStart := entry.start + (ovStart - vFileStart)
	vaEnd := entry.start + (ovEnd - vFileStart)
	if vaStart > math.MaxUint64-(pageSize-1) {
		return 0, 0, false
	}
	alignedStart := (vaStart + pageSize - 1) &^ (pageSize - 1)
	alignedEnd := vaEnd &^ (pageSize - 1)
	if alignedEnd <= alignedStart || alignedStart < entry.start || alignedEnd > entry.end {
		return 0, 0, false
	}
	return uintptr(alignedStart), uintptr(alignedEnd - alignedStart), true
}

func madviseDontNeed(addr, length uintptr) error {
	if length == 0 {
		return nil
	}
	_, _, errno := syscall.Syscall(syscall.SYS_MADVISE, addr, length, syscall.MADV_DONTNEED)
	if errno != 0 {
		return errno
	}
	return nil
}

// releaseEmbeddedReadOnlyPages advises the Linux kernel to drop clean
// file-backed pages covering the executable's .rodata section after embedded
// storage archives have been extracted. Subsequent reads re-fault seamlessly
// from the executable file without keeping ~51 MiB pinned in RSS while idle.
func releaseEmbeddedReadOnlyPages() error {
	exeFile, err := os.Open("/proc/self/exe")
	if err != nil {
		return fmt.Errorf("open /proc/self/exe: %w", err)
	}
	defer exeFile.Close()

	info, err := exeFile.Stat()
	if err != nil {
		return fmt.Errorf("stat /proc/self/exe: %w", err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == 0 {
		return errors.New("unsupported stat payload for /proc/self/exe")
	}
	devMajor, devMinor := linuxDevMajorMinor(st.Dev)
	exePath, _ := os.Readlink("/proc/self/exe")

	ef, err := elf.NewFile(exeFile)
	if err != nil {
		return fmt.Errorf("parse executable ELF: %w", err)
	}
	defer ef.Close()

	sec := ef.Section(".rodata")
	if sec == nil || sec.Size == 0 || sec.Type != elf.SHT_PROGBITS ||
		sec.Flags&elf.SHF_ALLOC == 0 || sec.Flags&elf.SHF_WRITE != 0 || sec.Flags&elf.SHF_EXECINSTR != 0 {
		return nil
	}

	mapsFile, err := os.Open("/proc/self/maps")
	if err != nil {
		return fmt.Errorf("open /proc/self/maps: %w", err)
	}
	defer mapsFile.Close()

	var entries []procMapEntry
	scanner := bufio.NewScanner(mapsFile)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		if entry, valid := parseProcMapsLine(scanner.Text()); valid {
			entries = append(entries, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan /proc/self/maps: %w", err)
	}

	pageSize := os.Getpagesize()
	if pageSize <= 0 {
		return errors.New("invalid OS page size")
	}
	var errs []error
	for _, entry := range selectReadOnlyExeMappings(entries, devMajor, devMinor, st.Ino, exePath) {
		addr, length, valid := alignSectionRangeInMapping(entry, sec.Offset, sec.Size, uint64(pageSize))
		if !valid {
			continue
		}
		if err := madviseDontNeed(addr, length); err != nil {
			errs = append(errs, fmt.Errorf("madvise(0x%x, 0x%x): %w", addr, length, err))
		}
	}
	return errors.Join(errs...)
}
