package profiling

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/eimsound/lazy-observe/internal/meta"
	pprofprofile "github.com/google/pprof/profile"
)

const (
	// DefaultMaxProfileBytes caps the compressed pprof payload size.
	DefaultMaxProfileBytes = 10 << 20
	// DefaultMaxDecodedBytes caps the decompressed protobuf payload size.
	DefaultMaxDecodedBytes = 10 << 20
	// DefaultMaxFlameDepth caps the maximum frame depth in an aggregated flamegraph.
	DefaultMaxFlameDepth = 256
	// DefaultMaxFlameNodes caps the maximum number of nodes in an aggregated flamegraph.
	DefaultMaxFlameNodes = 10_000
)

var (
	ErrNotGzip            = errors.New("profile is not gzip")
	ErrCompressedTooLarge = errors.New("compressed profile exceeds limit")
	ErrDecodedTooLarge    = errors.New("decoded profile exceeds limit")
	ErrNestedGzip         = errors.New("nested gzip pprof payload is not allowed")
	ErrInvalidProfile     = errors.New("invalid pprof profile")
	ErrInvalidSampleType  = errors.New("profile has invalid or incompatible sample types")
)

// GunzipBounded decompresses a gzip stream while enforcing a strict output byte
// budget so crafted payloads cannot exhaust memory.
func GunzipBounded(data []byte, limit int) ([]byte, error) {
	if limit <= 0 {
		limit = DefaultMaxDecodedBytes
	}
	if !isGzipMagic(data) {
		return nil, ErrNotGzip
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidProfile, err)
	}
	decoded, readErr := io.ReadAll(io.LimitReader(zr, int64(limit)+1))
	if len(decoded) > limit {
		_ = zr.Close()
		return nil, ErrDecodedTooLarge
	}
	closeErr := zr.Close()
	if readErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidProfile, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidProfile, closeErr)
	}
	return decoded, nil
}

// ParseGzipped decompresses and validates a single-layer gzipped pprof payload.
func ParseGzipped(data []byte, decodedLimit int) (*pprofprofile.Profile, error) {
	return ParseGzippedForType(data, decodedLimit, "")
}

// ParseGzippedForType decompresses, validates, and checks SampleType
// compatibility of a single-layer gzipped pprof payload against profileType.
// When profileType is empty, only structural SampleType validity is enforced.
func ParseGzippedForType(
	data []byte,
	decodedLimit int,
	profileType meta.ProfileType,
) (*pprofprofile.Profile, error) {
	decoded, err := GunzipBounded(data, decodedLimit)
	if err != nil {
		return nil, err
	}
	// Never allow nested gzip: checking explicitly guarantees a clear rejection
	// before invoking the protobuf parser.
	if isGzipMagic(decoded) {
		return nil, ErrNestedGzip
	}
	parsed, err := pprofprofile.ParseUncompressed(decoded)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidProfile, err)
	}
	if err := parsed.CheckValid(); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidProfile, err)
	}
	if err := ValidateSampleTypes(parsed, profileType); err != nil {
		return nil, err
	}
	return parsed, nil
}

// ValidateSampleTypes checks that a parsed pprof profile declares non-empty,
// well-formed SampleType entries and, when profileType is specified, contains
// at least one SampleType compatible with that profile kind across Go, Node,
// and Python producers.
func ValidateSampleTypes(p *pprofprofile.Profile, profileType meta.ProfileType) error {
	if p == nil || len(p.SampleType) == 0 {
		return ErrInvalidSampleType
	}
	for _, st := range p.SampleType {
		if st == nil || strings.TrimSpace(st.Type) == "" {
			return ErrInvalidSampleType
		}
	}
	if profileType == "" {
		return nil
	}
	for _, st := range p.SampleType {
		if sampleTypeCompatible(profileType, strings.TrimSpace(st.Type)) {
			return nil
		}
	}
	return fmt.Errorf("%w: no compatible sample type for %s", ErrInvalidSampleType, profileType)
}

func sampleTypeCompatible(profileType meta.ProfileType, sampleType string) bool {
	switch profileType {
	case meta.ProfileTypeCPU:
		switch sampleType {
		case "cpu", "samples", "wall":
			return true
		}
	case meta.ProfileTypeHeap:
		switch sampleType {
		case "inuse_space", "alloc_space", "inuse_objects", "alloc_objects", "space", "objects":
			return true
		}
	case meta.ProfileTypeGoroutine:
		switch sampleType {
		case "goroutine", "samples", "count":
			return true
		}
	default:
		return true
	}
	return false
}

func isGzipMagic(data []byte) bool {
	return len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b
}
