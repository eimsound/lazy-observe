package storage

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

func TestInjectRemoteWriteProjectReplacesHostileLabelAndSorts(t *testing.T) {
	request := prompb.WriteRequest{Timeseries: []prompb.TimeSeries{
		{
			Labels: []prompb.Label{
				{Name: "z", Value: "last"},
				{Name: "project_id", Value: "evil"},
				{Name: "__name__", Value: "requests_total"},
			},
			Samples: []prompb.Sample{{Value: 1, Timestamp: 123}},
		},
	}}
	protobuf, err := request.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	body, err := InjectRemoteWriteProject(snappy.Encode(nil, protobuf), "alpha", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := snappy.Decode(nil, body)
	if err != nil {
		t.Fatal(err)
	}
	var got prompb.WriteRequest
	if err := got.Unmarshal(decoded); err != nil {
		t.Fatal(err)
	}
	labels := got.Timeseries[0].Labels
	if len(labels) != 3 || labels[0].Name != "__name__" || labels[1].Name != "project_id" ||
		labels[1].Value != "alpha" ||
		labels[2].Name != "z" {
		t.Fatalf("labels=%v", labels)
	}
}

func TestInjectRemoteWriteProjectRejectsInvalidBody(t *testing.T) {
	if _, err := InjectRemoteWriteProject([]byte("not-snappy"), "alpha", 1<<20); err == nil {
		t.Fatal("invalid remote-write body accepted")
	}
}

func TestInjectRemoteWriteProjectRejectsOversizedDeclaredLength(t *testing.T) {
	const limit = 1 << 20
	// A snappy block starts with the uvarint decoded length. Five bytes claim
	// limit+1 decoded bytes; the decoder must refuse before allocating them.
	header := binary.AppendUvarint(nil, limit+1)
	_, err := InjectRemoteWriteProject(append(header, 0x00), "alpha", limit)
	if !errors.Is(err, ErrRemoteWriteTooLarge) {
		t.Fatalf("err=%v want ErrRemoteWriteTooLarge", err)
	}
}
