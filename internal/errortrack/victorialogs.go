package errortrack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

type LogsBackend interface {
	LogsBase() string
	SetTenantHeaders(http.Header, int)
}

type VictoriaLogsWriter struct {
	backend LogsBackend
	client  *http.Client
}

func NewVictoriaLogsWriter(backend LogsBackend, client *http.Client) *VictoriaLogsWriter {
	return &VictoriaLogsWriter{backend: backend, client: client}
}

func (w *VictoriaLogsWriter) WriteErrorEvent(
	ctx context.Context,
	project Project,
	raw json.RawMessage,
	event Event,
) error {
	if w == nil || w.backend == nil || w.client == nil {
		return ErrUnavailable
	}
	symbolicated, err := json.Marshal(event)
	if err != nil {
		return err
	}
	var original Event
	_ = json.Unmarshal(raw, &original)
	rawStack, _ := json.Marshal(original.Exception.Stacktrace)
	symbolicatedStack, _ := json.Marshal(event.Exception.Stacktrace)
	record := map[string]any{
		"event_type": "error", "event_id": event.EventID,
		"timestamp": event.Timestamp.UTC().Format(timeFormatRFC3339Nano),
		"level":     event.Level, "platform": event.Platform, "fingerprint": Fingerprint(event),
		"raw_event": string(raw), "symbolicated_event": string(symbolicated),
		"raw_stacktrace": string(rawStack), "symbolicated_stacktrace": string(symbolicatedStack),
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(record); err != nil {
		return err
	}
	target := strings.TrimRight(w.backend.LogsBase(), "/") + "/insert/jsonline?" + url.Values{
		"_stream_fields": []string{"event_type"}, "_time_field": []string{"timestamp"},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/stream+json")
	w.backend.SetTenantHeaders(req.Header, project.ID)
	resp, err := w.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		response, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("VLogs status %d: %s", resp.StatusCode, response)
	}
	return nil
}

const timeFormatRFC3339Nano = "2006-01-02T15:04:05.999999999Z07:00"
