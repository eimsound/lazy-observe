package storage

import (
	"errors"
	"fmt"
	"sort"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

// InjectRemoteWriteProject decodes a Prometheus remote-write 1.0 request,
// replaces every client-supplied project_id label, and returns a canonical
// snappy-compressed request. Prometheus requires labels to be unique and sorted
// by name, so the trusted label is inserted before re-encoding.
// ErrRemoteWriteTooLarge reports a request whose declared decoded size exceeds
// the caller's limit.
var ErrRemoteWriteTooLarge = errors.New("remote-write body exceeds decoded size limit")

func InjectRemoteWriteProject(body []byte, project string, maxDecoded int) ([]byte, error) {
	declared, err := snappy.DecodedLen(body)
	if err != nil {
		return nil, fmt.Errorf("decode snappy remote-write body: %w", err)
	}
	if declared > maxDecoded {
		return nil, ErrRemoteWriteTooLarge
	}
	decoded, err := snappy.Decode(nil, body)
	if err != nil {
		return nil, fmt.Errorf("decode snappy remote-write body: %w", err)
	}
	var request prompb.WriteRequest
	if err := request.Unmarshal(decoded); err != nil {
		return nil, fmt.Errorf("decode remote-write protobuf: %w", err)
	}
	for i := range request.Timeseries {
		labels := request.Timeseries[i].Labels[:0]
		for _, label := range request.Timeseries[i].Labels {
			if label.Name != "project_id" {
				labels = append(labels, label)
			}
		}
		labels = append(labels, prompb.Label{Name: "project_id", Value: project})
		sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })
		request.Timeseries[i].Labels = labels
	}
	encoded, err := request.Marshal()
	if err != nil {
		return nil, fmt.Errorf("encode remote-write protobuf: %w", err)
	}
	return snappy.Encode(nil, encoded), nil
}
