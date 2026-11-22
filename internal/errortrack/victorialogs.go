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
	"time"
)

const (
	timeFormatRFC3339Nano = "2006-01-02T15:04:05.999999999Z07:00"

	maxVLogsBatchLines           = 16
	maxVLogsBatchBytes           = 4 << 20
	vlogsDrainBodyLimitBytes     = 64 << 10
	vlogsErrorBodyReadLimit      = 4096
	defaultVLogsHTTPTimeout      = 15 * time.Second
	defaultVLogsMaxIdleConns     = 32
	defaultVLogsMaxIdleConnsHost = 16
	defaultVLogsIdleConnTimeout  = 90 * time.Second
)

type LogsBackend interface {
	LogsBase() string
	SetTenantHeaders(http.Header, int)
}

// PreparedEvent carries a decoded, optionally symbolicated error event with its
// precomputed fingerprint so batch writers never re-unmarshal raw JSON or
// recompute fingerprints.
type PreparedEvent struct {
	Project       Project
	Raw           json.RawMessage
	Event         Event
	RawStacktrace *Stacktrace
	Fingerprint   string
}

type vlogsErrorRecord struct {
	EventType              string `json:"event_type"`
	EventID                string `json:"event_id"`
	Timestamp              string `json:"timestamp"`
	Level                  string `json:"level"`
	Platform               string `json:"platform"`
	Fingerprint            string `json:"fingerprint"`
	RawEvent               string `json:"raw_event"`
	SymbolicatedEvent      string `json:"symbolicated_event"`
	RawStacktrace          string `json:"raw_stacktrace"`
	SymbolicatedStacktrace string `json:"symbolicated_stacktrace"`
}

type VictoriaLogsWriter struct {
	backend LogsBackend
	client  *http.Client
}

// NewVictoriaLogsHTTPClient builds a dedicated pooled and timed HTTP client
// for VictoriaLogs writes without sharing or mutating http.DefaultClient.
func NewVictoriaLogsHTTPClient() *http.Client {
	var transport *http.Transport
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	} else {
		transport = &http.Transport{}
	}
	transport.MaxIdleConns = defaultVLogsMaxIdleConns
	transport.MaxIdleConnsPerHost = defaultVLogsMaxIdleConnsHost
	transport.IdleConnTimeout = defaultVLogsIdleConnTimeout
	return &http.Client{
		Timeout:   defaultVLogsHTTPTimeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func NewVictoriaLogsWriter(backend LogsBackend, client *http.Client) *VictoriaLogsWriter {
	if client == nil {
		client = NewVictoriaLogsHTTPClient()
	}
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
	var original Event
	_ = json.Unmarshal(raw, &original)
	line, err := encodePreparedEvent(PreparedEvent{
		Project:       project,
		Raw:           raw,
		Event:         event,
		RawStacktrace: &original.Exception.Stacktrace,
		Fingerprint:   Fingerprint(event),
	})
	if err != nil {
		return err
	}
	_, err = w.postNDJSON(ctx, w.logsInsertTarget(), project.ID, line)
	return err
}

// WriteErrorEvents writes events grouped strictly by ProjectID in bounded NDJSON
// chunks (<= 16 lines, <= 4 MiB). A batch 4xx automatically falls back to
// per-event writes so one malformed event cannot poison healthy events; 5xx and
// transport errors are returned for every event in the failing chunk.
func (w *VictoriaLogsWriter) WriteErrorEvents(ctx context.Context, events []PreparedEvent) []error {
	if len(events) == 0 {
		return nil
	}
	errs := make([]error, len(events))
	if w == nil || w.backend == nil || w.client == nil {
		for idx := range errs {
			errs[idx] = ErrUnavailable
		}
		return errs
	}

	type encodedItem struct {
		origIdx int
		line    []byte
	}
	projectOrder := make([]int, 0, len(events))
	byProject := make(map[int][]encodedItem)
	for idx, item := range events {
		line, err := encodePreparedEvent(item)
		if err != nil {
			errs[idx] = err
			continue
		}
		if _, exists := byProject[item.Project.ID]; !exists {
			projectOrder = append(projectOrder, item.Project.ID)
		}
		byProject[item.Project.ID] = append(byProject[item.Project.ID], encodedItem{
			origIdx: idx,
			line:    line,
		})
	}

	target := w.logsInsertTarget()
	for _, projectID := range projectOrder {
		items := byProject[projectID]
		if ctxErr := ctx.Err(); ctxErr != nil {
			for _, it := range items {
				errs[it.origIdx] = ctxErr
			}
			continue
		}
		for start := 0; start < len(items); {
			if ctxErr := ctx.Err(); ctxErr != nil {
				for _, it := range items[start:] {
					errs[it.origIdx] = ctxErr
				}
				break
			}
			end := start
			chunkBytes := 0
			for end < len(items) && end-start < maxVLogsBatchLines {
				lineLen := len(items[end].line)
				if end > start && chunkBytes+lineLen > maxVLogsBatchBytes {
					break
				}
				chunkBytes += lineLen
				end++
			}
			chunk := items[start:end]
			var body bytes.Buffer
			body.Grow(chunkBytes)
			for _, it := range chunk {
				body.Write(it.line)
			}
			status, sendErr := w.postNDJSON(ctx, target, projectID, body.Bytes())
			if sendErr != nil {
				if len(chunk) > 1 && isBatchFallbackStatus(status) {
					for _, it := range chunk {
						if ctxErr := ctx.Err(); ctxErr != nil {
							errs[it.origIdx] = ctxErr
							continue
						}
						_, singleErr := w.postNDJSON(ctx, target, projectID, it.line)
						errs[it.origIdx] = singleErr
					}
				} else {
					for _, it := range chunk {
						errs[it.origIdx] = sendErr
					}
				}
			}
			start = end
		}
	}
	return errs
}

func encodePreparedEvent(p PreparedEvent) ([]byte, error) {
	if p.Project.ID <= 0 {
		return nil, fmt.Errorf("%w: project ID is required", ErrInvalid)
	}
	symbolicated, err := json.Marshal(p.Event)
	if err != nil {
		return nil, err
	}
	symbolicatedStack, err := json.Marshal(p.Event.Exception.Stacktrace)
	if err != nil {
		return nil, err
	}
	rawStack := symbolicatedStack
	if p.RawStacktrace != nil {
		rawStack, err = json.Marshal(*p.RawStacktrace)
		if err != nil {
			return nil, err
		}
	}
	fp := p.Fingerprint
	if fp == "" {
		fp = Fingerprint(p.Event)
	}
	record := vlogsErrorRecord{
		EventType:              "error",
		EventID:                p.Event.EventID,
		Timestamp:              p.Event.Timestamp.UTC().Format(timeFormatRFC3339Nano),
		Level:                  p.Event.Level,
		Platform:               p.Event.Platform,
		Fingerprint:            fp,
		RawEvent:               string(p.Raw),
		SymbolicatedEvent:      string(symbolicated),
		RawStacktrace:          string(rawStack),
		SymbolicatedStacktrace: string(symbolicatedStack),
	}
	var body bytes.Buffer
	if err := json.NewEncoder(&body).Encode(record); err != nil {
		return nil, err
	}
	return body.Bytes(), nil
}

func (w *VictoriaLogsWriter) logsInsertTarget() string {
	base := w.backend.LogsBase()
	if ingest, ok := w.backend.(interface{ LogsIngestBase() string }); ok && ingest.LogsIngestBase() != "" {
		base = ingest.LogsIngestBase()
	}
	return strings.TrimRight(base, "/") + "/insert/jsonline?" + url.Values{
		"_stream_fields": []string{"event_type"}, "_time_field": []string{"timestamp"},
	}.Encode()
}

func (w *VictoriaLogsWriter) postNDJSON(
	ctx context.Context,
	target string,
	projectID int,
	payload []byte,
) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/stream+json")
	w.backend.SetTenantHeaders(req.Header, projectID)
	resp, err := w.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		response, _ := io.ReadAll(io.LimitReader(resp.Body, vlogsErrorBodyReadLimit))
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, vlogsDrainBodyLimitBytes))
		return resp.StatusCode, fmt.Errorf("VLogs status %d: %s", resp.StatusCode, response)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, vlogsDrainBodyLimitBytes))
	return resp.StatusCode, nil
}

func isBatchFallbackStatus(status int) bool {
	return status >= 400 && status < 500 &&
		status != http.StatusRequestTimeout &&
		status != http.StatusTooManyRequests
}
