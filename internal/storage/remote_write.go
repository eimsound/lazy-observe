package storage

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

const (
	// DefaultRemoteWriteMaxDecodedBytes bounds one Prometheus remote-write
	// payload after Snappy decompression so public project tokens cannot
	// amplify small compressed bodies into multi-GiB heap allocations.
	DefaultRemoteWriteMaxDecodedBytes = 8 << 20
	DefaultRemoteWriteMaxTimeseries   = 10_000
	DefaultRemoteWriteMaxLabels       = 200_000
	DefaultRemoteWriteMaxSamples      = 100_000
)

// RemoteWriteLimits bounds decoded bytes and protobuf cardinality for a single
// Prometheus remote-write request.
type RemoteWriteLimits struct {
	MaxDecodedBytes int
	MaxTimeseries   int
	MaxLabels       int
	MaxSamples      int
}

// DefaultRemoteWriteLimits returns the standard remote-write admission limits.
func DefaultRemoteWriteLimits() RemoteWriteLimits {
	return RemoteWriteLimits{
		MaxDecodedBytes: DefaultRemoteWriteMaxDecodedBytes,
		MaxTimeseries:   DefaultRemoteWriteMaxTimeseries,
		MaxLabels:       DefaultRemoteWriteMaxLabels,
		MaxSamples:      DefaultRemoteWriteMaxSamples,
	}
}

var (
	// ErrRemoteWriteTooLarge reports a request whose decoded size or cardinality
	// exceeds the caller's limit.
	ErrRemoteWriteTooLarge = errors.New("remote-write body exceeds limit")
	// ErrRemoteWriteInvalid reports a malformed or empty-series remote-write payload.
	ErrRemoteWriteInvalid = errors.New("invalid remote-write payload")
)

// InjectRemoteWriteProject decodes a Prometheus remote-write 1.0 request,
// replaces every client-supplied project_id label, and returns a canonical
// snappy-compressed request. Prometheus requires labels to be unique and sorted
// by name, so the trusted label is inserted before re-encoding.
func InjectRemoteWriteProject(body []byte, project string, maxDecoded int) ([]byte, error) {
	limits := DefaultRemoteWriteLimits()
	if maxDecoded > 0 {
		limits.MaxDecodedBytes = maxDecoded
	}
	return InjectRemoteWriteProjectWithLimits(body, project, limits)
}

// InjectRemoteWriteProjectWithLimits enforces decoded-size and wire-level
// cardinality bounds before protobuf unmarshaling, then replaces project_id.
func InjectRemoteWriteProjectWithLimits(body []byte, project string, limits RemoteWriteLimits) ([]byte, error) {
	defaults := DefaultRemoteWriteLimits()
	if limits.MaxDecodedBytes <= 0 {
		limits.MaxDecodedBytes = defaults.MaxDecodedBytes
	}
	if limits.MaxTimeseries <= 0 {
		limits.MaxTimeseries = defaults.MaxTimeseries
	}
	if limits.MaxLabels <= 0 {
		limits.MaxLabels = defaults.MaxLabels
	}
	if limits.MaxSamples <= 0 {
		limits.MaxSamples = defaults.MaxSamples
	}
	if len(body) > limits.MaxDecodedBytes {
		return nil, ErrRemoteWriteTooLarge
	}
	declared, err := snappy.DecodedLen(body)
	if err != nil {
		return nil, fmt.Errorf("decode snappy remote-write body: %w", err)
	}
	if declared > limits.MaxDecodedBytes {
		return nil, ErrRemoteWriteTooLarge
	}
	decoded, err := snappy.Decode(nil, body)
	if err != nil {
		return nil, fmt.Errorf("decode snappy remote-write body: %w", err)
	}
	if len(decoded) > limits.MaxDecodedBytes {
		return nil, ErrRemoteWriteTooLarge
	}
	if err := validateRemoteWriteWire(decoded, limits); err != nil {
		return nil, err
	}
	var request prompb.WriteRequest
	if err := request.Unmarshal(decoded); err != nil {
		return nil, fmt.Errorf("decode remote-write protobuf: %w", err)
	}
	for i := range request.Timeseries {
		ts := &request.Timeseries[i]
		if len(ts.Labels) == 0 || (len(ts.Samples) == 0 && len(ts.Exemplars) == 0 && len(ts.Histograms) == 0) {
			return nil, fmt.Errorf("%w: empty time series", ErrRemoteWriteInvalid)
		}
		labels := ts.Labels[:0]
		for _, label := range ts.Labels {
			if label.Name != "project_id" {
				labels = append(labels, label)
			}
		}
		labels = append(labels, prompb.Label{Name: "project_id", Value: project})
		sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })
		ts.Labels = labels
	}
	encoded, err := request.Marshal()
	if err != nil {
		return nil, fmt.Errorf("encode remote-write protobuf: %w", err)
	}
	return snappy.Encode(nil, encoded), nil
}

// validateRemoteWriteWire scans the WriteRequest protobuf wire format without
// allocating slices or strings. Rejecting empty TimeSeries and excessive series,
// label, or sample counts here prevents multi-GiB heap amplification inside
// prompb.WriteRequest.Unmarshal.
func validateRemoteWriteWire(data []byte, limits RemoteWriteLimits) error {
	var seriesCount, labelCount, sampleCount int
	offset := 0
	for offset < len(data) {
		fieldNum, wireType, n, err := readProtoTag(data[offset:])
		if err != nil {
			return err
		}
		offset += n
		if fieldNum == 1 {
			if wireType != 2 {
				return fmt.Errorf("%w: invalid timeseries wire type", ErrRemoteWriteInvalid)
			}
			tsBytes, consumed, err := readProtoBytes(data[offset:])
			if err != nil {
				return err
			}
			offset += consumed
			seriesCount++
			if seriesCount > limits.MaxTimeseries {
				return fmt.Errorf("%w: timeseries count exceeds %d", ErrRemoteWriteTooLarge, limits.MaxTimeseries)
			}
			tsLabels, tsPoints, err := scanTimeSeriesWire(tsBytes, limits, labelCount, sampleCount)
			if err != nil {
				return err
			}
			if tsLabels == 0 || tsPoints == 0 {
				return fmt.Errorf("%w: empty time series", ErrRemoteWriteInvalid)
			}
			labelCount += tsLabels
			sampleCount += tsPoints
			continue
		}
		skipped, err := skipProtoField(data[offset:], wireType)
		if err != nil {
			return err
		}
		offset += skipped
	}
	return nil
}

func scanTimeSeriesWire(
	tsBytes []byte,
	limits RemoteWriteLimits,
	existingLabels, existingSamples int,
) (int, int, error) {
	if len(tsBytes) == 0 {
		return 0, 0, fmt.Errorf("%w: empty time series", ErrRemoteWriteInvalid)
	}
	var labels, points int
	offset := 0
	for offset < len(tsBytes) {
		fieldNum, wireType, n, err := readProtoTag(tsBytes[offset:])
		if err != nil {
			return 0, 0, err
		}
		offset += n
		switch fieldNum {
		case 1: // labels
			if wireType != 2 {
				return 0, 0, fmt.Errorf("%w: invalid label wire type", ErrRemoteWriteInvalid)
			}
			_, consumed, err := readProtoBytes(tsBytes[offset:])
			if err != nil {
				return 0, 0, err
			}
			offset += consumed
			labels++
			if existingLabels+labels > limits.MaxLabels {
				return 0, 0, fmt.Errorf("%w: label count exceeds %d", ErrRemoteWriteTooLarge, limits.MaxLabels)
			}
		case 2, 3, 4: // samples, exemplars, histograms
			if wireType != 2 {
				return 0, 0, fmt.Errorf("%w: invalid sample wire type", ErrRemoteWriteInvalid)
			}
			_, consumed, err := readProtoBytes(tsBytes[offset:])
			if err != nil {
				return 0, 0, err
			}
			offset += consumed
			points++
			if existingSamples+points > limits.MaxSamples {
				return 0, 0, fmt.Errorf("%w: sample count exceeds %d", ErrRemoteWriteTooLarge, limits.MaxSamples)
			}
		default:
			skipped, err := skipProtoField(tsBytes[offset:], wireType)
			if err != nil {
				return 0, 0, err
			}
			offset += skipped
		}
	}
	return labels, points, nil
}

func readProtoTag(data []byte) (fieldNum uint64, wireType uint64, consumed int, err error) {
	tag, n := binary.Uvarint(data)
	if n <= 0 {
		return 0, 0, 0, fmt.Errorf("%w: malformed protobuf tag", ErrRemoteWriteInvalid)
	}
	fieldNum = tag >> 3
	wireType = tag & 0x7
	if fieldNum == 0 {
		return 0, 0, 0, fmt.Errorf("%w: zero protobuf field number", ErrRemoteWriteInvalid)
	}
	return fieldNum, wireType, n, nil
}

func readProtoBytes(data []byte) ([]byte, int, error) {
	length, n := binary.Uvarint(data)
	if n <= 0 {
		return nil, 0, fmt.Errorf("%w: malformed protobuf length", ErrRemoteWriteInvalid)
	}
	if length > uint64(len(data)-n) {
		return nil, 0, fmt.Errorf("%w: truncated protobuf field", ErrRemoteWriteInvalid)
	}
	end := n + int(length)
	return data[n:end], end, nil
}

func skipProtoField(data []byte, wireType uint64) (int, error) {
	switch wireType {
	case 0: // varint
		_, n := binary.Uvarint(data)
		if n <= 0 {
			return 0, fmt.Errorf("%w: malformed varint", ErrRemoteWriteInvalid)
		}
		return n, nil
	case 1: // 64-bit
		if len(data) < 8 {
			return 0, fmt.Errorf("%w: truncated fixed64", ErrRemoteWriteInvalid)
		}
		return 8, nil
	case 2: // length-delimited
		_, consumed, err := readProtoBytes(data)
		return consumed, err
	case 5: // 32-bit
		if len(data) < 4 {
			return 0, fmt.Errorf("%w: truncated fixed32", ErrRemoteWriteInvalid)
		}
		return 4, nil
	default:
		return 0, fmt.Errorf("%w: unsupported protobuf wire type %d", ErrRemoteWriteInvalid, wireType)
	}
}
