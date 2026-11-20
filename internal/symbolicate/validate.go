package symbolicate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
)

const (
	// MaxSourceMapBytes bounds the size of a single source map payload parsed
	// into memory.
	MaxSourceMapBytes = 50 << 20
	// MaxMappingSegments bounds total VLQ mapping separators (',' and ';')
	// across a source map so go-sourcemap's 24-byte-per-mapping preallocation
	// cannot be abused as an OOM bomb.
	MaxMappingSegments = 1_500_000
	// MaxSourceMapSections bounds the number of sections in an indexed map.
	MaxSourceMapSections = 1024
)

var ErrInvalidSourceMap = errors.New("invalid source map v3")

type rawSourceMap struct {
	Version    int          `json:"version"`
	File       string       `json:"file,omitempty"`
	SourceRoot string       `json:"sourceRoot,omitempty"`
	Sources    []string     `json:"sources,omitempty"`
	Names      []string     `json:"names,omitempty"`
	Mappings   string       `json:"mappings,omitempty"`
	Sections   []rawSection `json:"sections,omitempty"`
}

type rawSection struct {
	Offset rawOffset     `json:"offset"`
	Map    *rawSourceMap `json:"map"`
}

type rawOffset struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

// ValidateAndNormalize verifies that data is a valid, bounded Source Map v3
// JSON object safe for go-sourcemap (preventing nil section maps, out-of-bounds
// source/name indices, and VLQ preallocation bombs), and strips unused fields
// such as sourcesContent. It returns the normalized JSON bytes and the total
// VLQ segment count for memory-budget accounting.
func ValidateAndNormalize(data []byte) ([]byte, int, error) {
	if len(data) == 0 {
		return nil, 0, fmt.Errorf("%w: empty payload", ErrInvalidSourceMap)
	}
	if len(data) > MaxSourceMapBytes {
		return nil, 0, fmt.Errorf("%w: exceeds %d bytes", ErrInvalidSourceMap, MaxSourceMapBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var parsed rawSourceMap
	if err := decoder.Decode(&parsed); err != nil {
		return nil, 0, fmt.Errorf("%w: %v", ErrInvalidSourceMap, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, 0, fmt.Errorf("%w: trailing JSON data", ErrInvalidSourceMap)
	}
	if parsed.Version != 3 {
		return nil, 0, fmt.Errorf("%w: unsupported version %d", ErrInvalidSourceMap, parsed.Version)
	}
	totalSegments := 0
	if len(parsed.Sections) > 0 {
		if len(parsed.Sections) > MaxSourceMapSections {
			return nil, 0, fmt.Errorf("%w: too many sections (%d)", ErrInvalidSourceMap, len(parsed.Sections))
		}
		for i := range parsed.Sections {
			sec := &parsed.Sections[i]
			if sec.Offset.Line < 0 || sec.Offset.Column < 0 {
				return nil, 0, fmt.Errorf("%w: section %d has negative offset", ErrInvalidSourceMap, i)
			}
			if sec.Map == nil {
				return nil, 0, fmt.Errorf("%w: section %d is missing map object", ErrInvalidSourceMap, i)
			}
			if sec.Map.Sections != nil {
				return nil, 0, fmt.Errorf("%w: nested sections are not supported", ErrInvalidSourceMap)
			}
			if sec.Map.Version != 3 {
				return nil, 0, fmt.Errorf(
					"%w: section %d has unsupported version %d",
					ErrInvalidSourceMap,
					i,
					sec.Map.Version,
				)
			}
			segs, err := validateMappings(sec.Map.Mappings, len(sec.Map.Sources), len(sec.Map.Names))
			if err != nil {
				return nil, 0, fmt.Errorf("%w: section %d: %v", ErrInvalidSourceMap, i, err)
			}
			totalSegments += segs
			if totalSegments > MaxMappingSegments {
				return nil, 0, fmt.Errorf("%w: mappings exceed %d segments", ErrInvalidSourceMap, MaxMappingSegments)
			}
		}
		// Keep top-level fields clean for indexed maps.
		parsed.Sources = nil
		parsed.Names = nil
		parsed.Mappings = ""
	} else {
		segs, err := validateMappings(parsed.Mappings, len(parsed.Sources), len(parsed.Names))
		if err != nil {
			return nil, 0, fmt.Errorf("%w: %v", ErrInvalidSourceMap, err)
		}
		totalSegments = segs
	}
	normalized, err := json.Marshal(parsed)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: marshal normalized map: %v", ErrInvalidSourceMap, err)
	}
	return normalized, totalSegments, nil
}

var base64VLQTable = func() [256]int8 {
	var t [256]int8
	for i := range t {
		t[i] = -1
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	for i := 0; i < len(alphabet); i++ {
		t[alphabet[i]] = int8(i)
	}
	return t
}()

// validateMappings decodes every Base64-VLQ segment in mappings and verifies
// separator count limits, syntax, int32 range, and that every referenced
// source index lies within [0, sourcesLen) so go-sourcemap cannot panic.
func validateMappings(mappings string, sourcesLen, namesLen int) (int, error) {
	if mappings == "" {
		return 0, nil
	}
	separators := 0
	for i := 0; i < len(mappings); i++ {
		if mappings[i] == ',' || mappings[i] == ';' {
			separators++
			if separators > MaxMappingSegments {
				return 0, fmt.Errorf("mappings exceed %d segments", MaxMappingSegments)
			}
		}
	}
	var genCol, srcIdx, srcLine, srcCol, nameIdx int64
	i := 0
	n := len(mappings)
	for i < n {
		c := mappings[i]
		if c == ';' {
			genCol = 0
			i++
			continue
		}
		if c == ',' {
			i++
			continue
		}
		var fields [5]int64
		count := 0
		for i < n && mappings[i] != ',' && mappings[i] != ';' {
			if count >= 5 {
				return 0, errors.New("mapping segment has more than 5 VLQ fields")
			}
			val, next, err := decodeVLQ(mappings, i)
			if err != nil {
				return 0, err
			}
			fields[count] = val
			count++
			i = next
		}
		if count != 1 && count != 4 && count != 5 {
			return 0, fmt.Errorf("mapping segment has invalid field count %d", count)
		}
		genCol += fields[0]
		if genCol < 0 || genCol > math.MaxInt32 {
			return 0, errors.New("generated column out of range")
		}
		if count >= 4 {
			srcIdx += fields[1]
			if srcIdx < 0 || srcIdx >= int64(sourcesLen) {
				return 0, fmt.Errorf("source index %d out of bounds (sources=%d)", srcIdx, sourcesLen)
			}
			srcLine += fields[2]
			srcCol += fields[3]
			if srcLine < math.MinInt32 || srcLine > math.MaxInt32 ||
				srcCol < math.MinInt32 || srcCol > math.MaxInt32 {
				return 0, errors.New("source position out of int32 range")
			}
		}
		if count == 5 {
			nameIdx += fields[4]
			if nameIdx < 0 || nameIdx >= int64(namesLen) {
				return 0, fmt.Errorf("name index %d out of bounds (names=%d)", nameIdx, namesLen)
			}
		}
	}
	return separators + 1, nil
}

func decodeVLQ(s string, pos int) (int64, int, error) {
	var result uint64
	var shift uint
	for {
		if pos >= len(s) {
			return 0, pos, errors.New("truncated VLQ sequence")
		}
		digit := base64VLQTable[s[pos]]
		if digit < 0 {
			return 0, pos, fmt.Errorf("invalid base64 VLQ byte %q", s[pos])
		}
		pos++
		continuation := (digit & 32) != 0
		value := uint64(digit & 31)
		if shift > 35 {
			return 0, pos, errors.New("VLQ integer overflow")
		}
		result |= value << shift
		shift += 5
		if !continuation {
			break
		}
	}
	neg := (result & 1) == 1
	mag := result >> 1
	if mag > math.MaxInt32 {
		return 0, pos, errors.New("VLQ value exceeds int32")
	}
	if neg {
		if mag == 0 {
			return 0, pos, nil
		}
		return -int64(mag), pos, nil
	}
	return int64(mag), pos, nil
}
