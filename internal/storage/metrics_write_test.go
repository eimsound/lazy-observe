package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

func TestWriteMetricSamplesVictoriaMetricsSingleAndCluster(t *testing.T) {
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)
	samples := []MetricSample{{
		Name: "web_vitals_lcp",
		Labels: []MetricLabel{
			{Name: "project_id", Value: "alpha"},
			{Name: "page", Value: "/checkout"},
		},
		Value:     123.4,
		Timestamp: at,
	}}

	for _, tc := range []struct {
		name     string
		opts     MetricsOptions
		wantPath string
	}{
		{
			name:     "vm single node",
			opts:     MetricsOptions{},
			wantPath: "/api/v1/import/prometheus",
		},
		{
			name:     "vm cluster",
			opts:     MetricsOptions{Cluster: true, TenantAccountID: "7"},
			wantPath: "/insert/7/prometheus/api/v1/import/prometheus",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotContentType, gotBody string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				gotPath = r.URL.Path
				gotContentType = r.Header.Get("Content-Type")
				gotBody = string(raw)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer srv.Close()

			backend, err := NewWithMetricsOptions(MetricsVM, srv.URL, srv.URL, srv.URL, srv.URL, tc.opts, srv.Client())
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteMetricSamples(context.Background(), backend, srv.Client(), samples); err != nil {
				t.Fatalf("WriteMetricSamples error: %v", err)
			}
			if gotPath != tc.wantPath {
				t.Fatalf("path=%q want=%q", gotPath, tc.wantPath)
			}
			if gotContentType != "text/plain" {
				t.Fatalf("content-type=%q want=text/plain", gotContentType)
			}
			wantLine := `web_vitals_lcp{project_id="alpha",page="/checkout"} 123.4 1787488496789` + "\n"
			if gotBody != wantLine {
				t.Fatalf("body=%q want=%q", gotBody, wantLine)
			}
		})
	}
}

func TestWriteMetricSamplesPrometheusRemoteWrite(t *testing.T) {
	at := time.Date(2026, 8, 23, 12, 34, 56, 789_000_000, time.UTC)
	var gotPath, gotContentType, gotEncoding, gotVersion string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotEncoding = r.Header.Get("Content-Encoding")
		gotVersion = r.Header.Get("X-Prometheus-Remote-Write-Version")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	backend, err := New(MetricsPrometheus, srv.URL, srv.URL, srv.URL, srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = WriteMetricSamples(context.Background(), backend, srv.Client(), []MetricSample{{
		Name: "lazy_observe_sdk_dropped_telemetry_delta",
		Labels: []MetricLabel{
			{Name: "signal", Value: "replay"},
			{Name: "project_id", Value: "alpha"},
			{Name: "service_name", Value: "web\r\napp"},
			{Name: "reason", Value: "queue_full"},
		},
		Value:     5,
		Timestamp: at,
	}})
	if err != nil {
		t.Fatalf("WriteMetricSamples error: %v", err)
	}
	if gotPath != "/api/v1/write" || gotContentType != "application/x-protobuf" ||
		gotEncoding != "snappy" || gotVersion != "0.1.0" {
		t.Fatalf(
			"path=%q contentType=%q encoding=%q version=%q",
			gotPath,
			gotContentType,
			gotEncoding,
			gotVersion,
		)
	}
	decoded, err := snappy.Decode(nil, gotBody)
	if err != nil {
		t.Fatalf("snappy decode: %v", err)
	}
	var req prompb.WriteRequest
	if err := req.Unmarshal(decoded); err != nil {
		t.Fatalf("unmarshal prompb: %v", err)
	}
	if len(req.Timeseries) != 1 || len(req.Timeseries[0].Samples) != 1 {
		t.Fatalf("timeseries=%#v", req.Timeseries)
	}
	ts := req.Timeseries[0]
	// Labels must be strictly sorted by name per Prometheus Remote Write 1.0.
	for i := 1; i < len(ts.Labels); i++ {
		if ts.Labels[i-1].Name >= ts.Labels[i].Name {
			t.Fatalf("labels not strictly sorted: %#v", ts.Labels)
		}
	}
	gotLabels := make(map[string]string, len(ts.Labels))
	for _, lbl := range ts.Labels {
		gotLabels[lbl.Name] = lbl.Value
	}
	if gotLabels["__name__"] != "lazy_observe_sdk_dropped_telemetry_delta" ||
		gotLabels["project_id"] != "alpha" ||
		gotLabels["service_name"] != "web\napp" ||
		gotLabels["signal"] != "replay" ||
		gotLabels["reason"] != "queue_full" {
		t.Fatalf("labels=%v", gotLabels)
	}
	if ts.Samples[0].Value != 5 || ts.Samples[0].Timestamp != at.UnixMilli() {
		t.Fatalf("sample=%#v", ts.Samples[0])
	}
}

func TestWriteMetricSamplesValidationAndErrorHandling(t *testing.T) {
	at := time.Date(2026, 8, 23, 12, 34, 56, 0, time.UTC)
	for _, bad := range []MetricSample{
		{Name: "bad metric{x=1}", Value: 1, Timestamp: at},
		{Name: "1starts_with_digit", Value: 1, Timestamp: at},
		{Name: "ok_metric", Labels: []MetricLabel{{Name: "bad-label", Value: "v"}}, Value: 1, Timestamp: at},
		{Name: "ok_metric", Value: 1},
	} {
		if _, err := EncodePrometheusText([]MetricSample{bad}); err == nil {
			t.Fatalf("EncodePrometheusText unexpectedly accepted %#v", bad)
		}
		if _, err := EncodeRemoteWrite([]MetricSample{bad}); err == nil {
			t.Fatalf("EncodeRemoteWrite unexpectedly accepted %#v", bad)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "upstream overloaded", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	backend, err := New(MetricsVM, srv.URL, srv.URL, srv.URL, srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = WriteMetricSamples(context.Background(), backend, srv.Client(), []MetricSample{{
		Name:      "web_vitals_lcp",
		Labels:    []MetricLabel{{Name: "project_id", Value: "alpha"}},
		Value:     10,
		Timestamp: at,
	}})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("expected 503 error, got %v", err)
	}
}
