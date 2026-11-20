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

func TestInjectRemoteWriteProjectRejectsEmptyTimeSeries(t *testing.T) {
	// A 2-byte empty TimeSeries (field 1, length 0) repeats trivially in Snappy
	// and can amplify memory usage by orders of magnitude during protobuf unmarshal.
	var payload []byte
	for range 16 {
		payload = append(payload, 0x0a, 0x00)
	}
	_, err := InjectRemoteWriteProject(snappy.Encode(nil, payload), "alpha", 8<<20)
	if err == nil {
		t.Fatal("empty TimeSeries remote-write payload must be rejected")
	}
}

func TestInjectRemoteWriteProjectRejectsExcessiveTimeseries(t *testing.T) {
	ts := prompb.TimeSeries{
		Labels:  []prompb.Label{{Name: "__name__", Value: "m"}},
		Samples: []prompb.Sample{{Value: 1, Timestamp: 1}},
	}
	single, err := (&prompb.WriteRequest{Timeseries: []prompb.TimeSeries{ts}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// Repeat 10_001 valid series so decoded size stays well below 8 MiB while
	// exceeding the per-request series budget.
	payload := make([]byte, 0, len(single)*10_001)
	for range 10_001 {
		payload = append(payload, single...)
	}
	_, err = InjectRemoteWriteProject(snappy.Encode(nil, payload), "alpha", 8<<20)
	if !errors.Is(err, ErrRemoteWriteTooLarge) {
		t.Fatalf("excessive timeseries err=%v want ErrRemoteWriteTooLarge", err)
	}
}

func TestInjectRemoteWriteProjectWithLimitsEnforcesLabelAndSampleBudgets(t *testing.T) {
	req := prompb.WriteRequest{Timeseries: []prompb.TimeSeries{{
		Labels: []prompb.Label{
			{Name: "__name__", Value: "m"},
			{Name: "a", Value: "1"},
			{Name: "b", Value: "2"},
		},
		Samples: []prompb.Sample{{Value: 1, Timestamp: 1}, {Value: 2, Timestamp: 2}},
	}}}
	raw, err := req.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	compressed := snappy.Encode(nil, raw)
	_, err = InjectRemoteWriteProjectWithLimits(compressed, "alpha", RemoteWriteLimits{
		MaxDecodedBytes: 1 << 20, MaxTimeseries: 10, MaxLabels: 2, MaxSamples: 10,
	})
	if !errors.Is(err, ErrRemoteWriteTooLarge) {
		t.Fatalf("label limit err=%v want ErrRemoteWriteTooLarge", err)
	}
	_, err = InjectRemoteWriteProjectWithLimits(compressed, "alpha", RemoteWriteLimits{
		MaxDecodedBytes: 1 << 20, MaxTimeseries: 10, MaxLabels: 10, MaxSamples: 1,
	})
	if !errors.Is(err, ErrRemoteWriteTooLarge) {
		t.Fatalf("sample limit err=%v want ErrRemoteWriteTooLarge", err)
	}
}
