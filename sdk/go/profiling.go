package lazyobserve

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"runtime/pprof"
	"strings"
	"time"
)

const (
	profilingCPUWindow       = 10 * time.Second
	profilingQueueSize       = 6
	profilingSendTimeout     = 15 * time.Second
	profilingMaxPayloadBytes = 10 << 20
	// profilingMinCancelledWindow is the shortest CPU window that is still
	// uploaded when shutdown cancels it early (the owner's decision 14, shared
	// with the Python SDK; see .agents/docs/plans/python-profiling.md). A window
	// that ran for less is discarded: it is not a delivery failure, so it is not
	// counted as dropped telemetry.
	profilingMinCancelledWindow = 5 * time.Second
)

// cpuProfiler is the clock and CPU profiler a collection cycle runs against.
// Production uses systemCPUProfiler; tests inject fakes so the 5 s boundary
// does not need a real 5 s window.
type cpuProfiler struct {
	now       func() time.Time
	start     func(io.Writer) error
	stop      func()
	minWindow time.Duration
}

var systemCPUProfiler = &cpuProfiler{
	now:       time.Now,
	start:     pprof.StartCPUProfile,
	stop:      pprof.StopCPUProfile,
	minWindow: profilingMinCancelledWindow,
}

type collectedProfile struct {
	uploadID string
	typeName string
	start    time.Time
	end      time.Time
	data     []byte
}

func (c *Client) profilingLoop(ctx context.Context) {
	defer close(c.profileDone)
	for {
		cycleStart := time.Now()
		profiles := collectProfiles(ctx, profilingCPUWindow, c.cpuProfilerOrDefault())
		for _, collected := range profiles {
			c.profileDelivery.enqueue(collected)
		}
		if ctx.Err() != nil {
			return
		}
		delay := jitterProfilingDelay(c.cfg.ProfilingInterval - time.Since(cycleStart))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		case <-timer.C:
		}
	}
}

func jitterProfilingDelay(delay time.Duration) time.Duration {
	if delay <= 0 {
		return 0
	}
	spread := min(delay/10, 5*time.Second)
	if spread <= 0 {
		return delay
	}
	return delay + time.Duration(rand.Int64N(int64(spread)+1))
}

func newProfileDelivery(c *Client) *deliveryQueue[collectedProfile] {
	return newDeliveryQueue(deliveryOptions[collectedProfile]{
		Capacity:      c.cfg.Delivery.ProfileQueueCapacity,
		BatchSize:     1,
		FlushInterval: deliveryDefaultFlushInterval,
		MaxAttempts:   c.cfg.Delivery.MaxAttempts,
		RetryInitial:  c.cfg.Delivery.RetryInitial,
		RetryMax:      c.cfg.Delivery.RetryMax,
		Send: func(ctx context.Context, profiles []collectedProfile) error {
			uploadCtx, cancel := context.WithTimeout(ctx, profilingSendTimeout)
			defer cancel()
			return c.uploadProfile(uploadCtx, profiles[0])
		},
		Warn: func(reason string, err error, dropped int) {
			c.recordTelemetryDrop("profile", reason, dropped)
			if c.logger != nil {
				c.logger.Warn(
					"profile telemetry dropped",
					"reason",
					reason,
					"profiles",
					dropped,
					"error",
					err,
				)
			}
		},
	})
}

func (c *Client) cpuProfilerOrDefault() *cpuProfiler {
	if c.cpuProfiler != nil {
		return c.cpuProfiler
	}
	return systemCPUProfiler
}

// collectProfiles records one CPU window and then heap and goroutine
// snapshots. When ctx is cancelled before the window completes, the CPU
// window is kept only if it ran for at least p.minWindow; the snapshots are
// instantaneous and are always collected.
func collectProfiles(ctx context.Context, cpuWindow time.Duration, p *cpuProfiler) []collectedProfile {
	result := make([]collectedProfile, 0, 3)
	if cpu, ok := collectCPUWindow(ctx, cpuWindow, p); ok {
		result = append(result, cpu)
	}
	if heap, ok := collectSnapshotProfile("heap", "heap"); ok {
		result = append(result, heap)
	}
	if goroutine, ok := collectSnapshotProfile("goroutine", "goroutine"); ok {
		result = append(result, goroutine)
	}
	return result
}

func collectCPUWindow(ctx context.Context, cpuWindow time.Duration, p *cpuProfiler) (collectedProfile, bool) {
	var cpu bytes.Buffer
	start := p.now().UTC()
	if err := p.start(&cpu); err != nil {
		return collectedProfile{}, false
	}
	cancelled := false
	timer := time.NewTimer(cpuWindow)
	select {
	case <-ctx.Done():
		cancelled = true
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	case <-timer.C:
	}
	p.stop()
	end := p.now().UTC()
	if cancelled && end.Sub(start) < p.minWindow {
		// Shutdown cut the window short. Discarding it is not a delivery
		// failure, so it is neither queued nor counted as dropped telemetry.
		return collectedProfile{}, false
	}
	data, err := ensureGzip(cpu.Bytes())
	if err != nil || len(data) == 0 {
		return collectedProfile{}, false
	}
	return collectedProfile{uploadID: newEventID(), typeName: "cpu", start: start, end: end, data: data}, true
}

func collectSnapshotProfile(profileName, typeName string) (collectedProfile, bool) {
	profile := pprof.Lookup(profileName)
	if profile == nil {
		return collectedProfile{}, false
	}
	var raw bytes.Buffer
	start := time.Now().UTC()
	if err := profile.WriteTo(&raw, 0); err != nil {
		return collectedProfile{}, false
	}
	data, err := ensureGzip(raw.Bytes())
	if err != nil || len(data) == 0 {
		return collectedProfile{}, false
	}
	return collectedProfile{
		uploadID: newEventID(),
		typeName: typeName,
		start:    start,
		end:      time.Now().UTC(),
		data:     data,
	}, true
}

func ensureGzip(data []byte) ([]byte, error) {
	if len(data) >= 2 && data[0] == 0x1f && data[1] == 0x8b {
		return data, nil
	}
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(data); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return compressed.Bytes(), nil
}

func (c *Client) httpProfileClient() *http.Client {
	if c.profileClient != nil {
		return c.profileClient
	}
	return &http.Client{Timeout: profilingSendTimeout}
}

func (c *Client) uploadProfile(ctx context.Context, collected collectedProfile) error {
	if len(collected.data) > profilingMaxPayloadBytes {
		return &deliveryError{
			statusCode: http.StatusRequestEntityTooLarge,
			permanent:  true,
			err: fmt.Errorf(
				"profile payload %d bytes exceeds %d-byte limit",
				len(collected.data),
				profilingMaxPayloadBytes,
			),
		}
	}
	var prefix bytes.Buffer
	w := multipart.NewWriter(&prefix)
	fields := [][2]string{
		{"upload_id", collected.uploadID},
		{"type", collected.typeName},
		{"service", c.cfg.ServiceName},
		{"start", collected.start.Format(time.RFC3339Nano)},
		{"end", collected.end.Format(time.RFC3339Nano)},
	}
	for _, field := range fields {
		if err := w.WriteField(field[0], field[1]); err != nil {
			return err
		}
	}
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="profile"; filename="profile.pb.gz"`)
	header.Set("Content-Type", "application/octet-stream")
	if _, err := w.CreatePart(header); err != nil {
		return err
	}
	prefixBytes := prefix.Bytes()
	suffix := "\r\n--" + w.Boundary() + "--\r\n"
	contentType := w.FormDataContentType()
	newBody := func() io.ReadCloser {
		return io.NopCloser(io.MultiReader(
			bytes.NewReader(prefixBytes),
			bytes.NewReader(collected.data),
			strings.NewReader(suffix),
		))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.profileURL, newBody())
	if err != nil {
		return err
	}
	req.ContentLength = int64(len(prefixBytes) + len(collected.data) + len(suffix))
	req.GetBody = func() (io.ReadCloser, error) { return newBody(), nil }
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+c.profileToken)
	resp, err := c.httpProfileClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		response, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		msg := strings.TrimSpace(string(response))
		errMsg := fmt.Errorf("profile upload returned %s", resp.Status)
		if msg != "" {
			errMsg = fmt.Errorf("profile upload returned %s: %s", resp.Status, msg)
		}
		return &deliveryError{
			statusCode: resp.StatusCode,
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			permanent:  isPermanentHTTPStatus(resp.StatusCode),
			err:        errMsg,
		}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}
