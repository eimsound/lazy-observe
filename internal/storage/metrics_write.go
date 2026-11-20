package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang/snappy"
	"github.com/prometheus/prometheus/prompb"
)

// MetricLabel is one structured label pair attached to a written metric sample.
type MetricLabel struct {
	Name  string
	Value string
}

// MetricSample is one structured metric data point written through the storage
// boundary without coupling callers to backend-specific wire formats or paths.
type MetricSample struct {
	Name      string
	Labels    []MetricLabel
	Value     float64
	Timestamp time.Time
}

// WriteMetricSamples writes structured samples to the configured metrics ingest
// backend. VictoriaMetrics receives Prometheus text exposition on
// /api/v1/import/prometheus (with cluster prefix when clustered); Prometheus
// backends receive Snappy-compressed Prometheus Remote Write 1.0 on /api/v1/write.
func WriteMetricSamples(
	ctx context.Context,
	backend SignalBackend,
	client *http.Client,
	samples []MetricSample,
) error {
	if len(samples) == 0 {
		return nil
	}
	if backend == nil || !backend.MetricsIngestEnabled() {
		return errors.New("metrics ingest is not configured")
	}
	if client == nil {
		client = http.DefaultClient
	}
	var req *http.Request
	var err error
	if backend.MetricsMode() == MetricsPrometheus {
		payload, encodeErr := EncodeRemoteWrite(samples)
		if encodeErr != nil {
			return encodeErr
		}
		target := strings.TrimRight(backend.PrometheusRemoteWriteBase(), "/") + "/api/v1/write"
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/x-protobuf")
		req.Header.Set("Content-Encoding", "snappy")
		req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	} else {
		text, encodeErr := EncodePrometheusText(samples)
		if encodeErr != nil {
			return encodeErr
		}
		importPath := "/api/v1/import/prometheus"
		if backend.MetricsClustered() {
			importPath = "/prometheus" + importPath
		}
		target := strings.TrimRight(backend.MetricsIngestBase(), "/") + importPath
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(text))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "text/plain")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("metrics ingest status %d", resp.StatusCode)
	}
	return nil
}

// EncodePrometheusText encodes structured samples into Prometheus text
// exposition format with strict metric/label name validation and spec-compliant
// label value escaping.
func EncodePrometheusText(samples []MetricSample) (string, error) {
	var b strings.Builder
	for i, sample := range samples {
		if err := validateMetricSample(sample); err != nil {
			return "", fmt.Errorf("sample[%d]: %w", i, err)
		}
		b.WriteString(sample.Name)
		if len(sample.Labels) > 0 {
			b.WriteByte('{')
			for idx, label := range sample.Labels {
				if idx > 0 {
					b.WriteByte(',')
				}
				b.WriteString(label.Name)
				b.WriteByte('=')
				b.WriteString(FormatPromLabelValue(label.Value))
			}
			b.WriteByte('}')
		}
		b.WriteByte(' ')
		b.WriteString(strconv.FormatFloat(sample.Value, 'g', -1, 64))
		b.WriteByte(' ')
		b.WriteString(strconv.FormatInt(sample.Timestamp.UTC().UnixMilli(), 10))
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// EncodeRemoteWrite encodes structured samples into a Snappy-compressed
// Prometheus Remote Write 1.0 WriteRequest protobuf payload with sorted unique
// labels per time series.
func EncodeRemoteWrite(samples []MetricSample) ([]byte, error) {
	timeseries := make([]prompb.TimeSeries, 0, len(samples))
	for i, sample := range samples {
		if err := validateMetricSample(sample); err != nil {
			return nil, fmt.Errorf("sample[%d]: %w", i, err)
		}
		lbls := make([]prompb.Label, 0, len(sample.Labels)+1)
		lbls = append(lbls, prompb.Label{Name: "__name__", Value: sample.Name})
		for _, label := range sample.Labels {
			if label.Name == "__name__" {
				continue
			}
			lbls = append(lbls, prompb.Label{
				Name:  label.Name,
				Value: sanitizeMetricLabelValue(label.Value),
			})
		}
		sort.Slice(lbls, func(a, b int) bool { return lbls[a].Name < lbls[b].Name })
		deduped := lbls[:0]
		for _, lbl := range lbls {
			if len(deduped) > 0 && deduped[len(deduped)-1].Name == lbl.Name {
				deduped[len(deduped)-1].Value = lbl.Value
				continue
			}
			deduped = append(deduped, lbl)
		}
		timeseries = append(timeseries, prompb.TimeSeries{
			Labels: deduped,
			Samples: []prompb.Sample{{
				Value:     sample.Value,
				Timestamp: sample.Timestamp.UTC().UnixMilli(),
			}},
		})
	}
	request := prompb.WriteRequest{Timeseries: timeseries}
	encoded, err := request.Marshal()
	if err != nil {
		return nil, fmt.Errorf("encode remote-write metrics: %w", err)
	}
	return snappy.Encode(nil, encoded), nil
}

// FormatPromLabelValue formats a Prometheus text exposition label value using
// only the three escapes allowed by the Prometheus exposition spec (\, ", \n),
// preventing Go-only strconv.Quote escapes (\t, \r, \x.., \u....) from causing
// VictoriaMetrics 400 rejections.
func FormatPromLabelValue(value string) string {
	valid := sanitizeMetricLabelValue(value)
	var b strings.Builder
	b.Grow(len(valid) + 2)
	b.WriteByte('"')
	for i := 0; i < len(valid); i++ {
		switch valid[i] {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteByte(valid[i])
		}
	}
	b.WriteByte('"')
	return b.String()
}

func sanitizeMetricLabelValue(value string) string {
	valid := strings.ToValidUTF8(value, "\ufffd")
	if !strings.ContainsRune(valid, '\r') {
		return valid
	}
	// Strip CR so it cannot split line-oriented exposition records or leak
	// inconsistent values between text and remote-write encodings.
	return strings.ReplaceAll(valid, "\r", "")
}

func validateMetricSample(sample MetricSample) error {
	if !validMetricName(sample.Name) {
		return fmt.Errorf("invalid metric name %q", sample.Name)
	}
	if math.IsNaN(sample.Value) || math.IsInf(sample.Value, 0) {
		return errors.New("metric value must be finite")
	}
	if sample.Timestamp.IsZero() {
		return errors.New("metric timestamp is required")
	}
	for _, label := range sample.Labels {
		if !validMetricLabelName(label.Name) {
			return fmt.Errorf("invalid label name %q", label.Name)
		}
	}
	return nil
}

func validMetricName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		ch := name[i]
		isLetter := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
		isDigit := ch >= '0' && ch <= '9'
		if i == 0 {
			if !isLetter && ch != '_' && ch != ':' {
				return false
			}
			continue
		}
		if !isLetter && !isDigit && ch != '_' && ch != ':' {
			return false
		}
	}
	return true
}

func validMetricLabelName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		ch := name[i]
		isLetter := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z')
		isDigit := ch >= '0' && ch <= '9'
		if i == 0 {
			if !isLetter && ch != '_' {
				return false
			}
			continue
		}
		if !isLetter && !isDigit && ch != '_' {
			return false
		}
	}
	return true
}
