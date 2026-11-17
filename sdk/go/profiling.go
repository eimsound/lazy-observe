package lazyobserve

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"runtime/pprof"
	"strconv"
	"time"
)

const (
	profilingCPUWindow   = 10 * time.Second
	profilingQueueSize   = 6
	profilingSendTimeout = 15 * time.Second
)

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
		profiles := collectProfiles(ctx, profilingCPUWindow)
		for _, collected := range profiles {
			c.profileDelivery.enqueue(collected)
		}
		if ctx.Err() != nil {
			return
		}
		delay := c.cfg.ProfilingInterval - time.Since(cycleStart)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return
		case <-timer.C:
		}
	}
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

func collectProfiles(ctx context.Context, cpuWindow time.Duration) []collectedProfile {
	result := make([]collectedProfile, 0, 3)
	var cpu bytes.Buffer
	start := time.Now().UTC()
	if err := pprof.StartCPUProfile(&cpu); err == nil {
		timer := time.NewTimer(cpuWindow)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
		pprof.StopCPUProfile()
		end := time.Now().UTC()
		if data, err := ensureGzip(cpu.Bytes()); err == nil && len(data) > 0 {
			result = append(result, collectedProfile{
				uploadID: newEventID(),
				typeName: "cpu",
				start:    start,
				end:      end,
				data:     data,
			})
		}
	}

	if heap, ok := collectSnapshotProfile("heap", "heap"); ok {
		result = append(result, heap)
	}
	if goroutine, ok := collectSnapshotProfile("goroutine", "goroutine"); ok {
		result = append(result, goroutine)
	}
	return result
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
		return append([]byte(nil), data...), nil
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

func (c *Client) uploadProfile(ctx context.Context, collected collectedProfile) error {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="profile"; filename="profile.pb.gz"`)
	header.Set("Content-Type", "application/octet-stream")
	part, err := w.CreatePart(header)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, bytes.NewReader(collected.data)); err != nil {
		return err
	}
	fields := map[string]string{
		"upload_id": collected.uploadID,
		"type":      collected.typeName,
		"service":   c.cfg.ServiceName,
		"start":     collected.start.Format(time.RFC3339Nano),
		"end":       collected.end.Format(time.RFC3339Nano),
	}
	for name, value := range fields {
		if err := w.WriteField(name, value); err != nil {
			return err
		}
	}
	if err := w.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.profileURL, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.profileToken)
	req.Header.Set("Content-Length", strconv.Itoa(body.Len()))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("profile upload returned %s", resp.Status)
	}
	return nil
}
