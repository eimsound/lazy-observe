package lazyobserve

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"go.opentelemetry.io/otel/trace"
)

const (
	errorBatchSize     = 20
	errorFlushInterval = 100 * time.Millisecond
	errorQueueSize     = 100
	errorSendTimeout   = 5 * time.Second
)

type errorFrame struct {
	Function string `json:"function"`
	Filename string `json:"filename"`
	Lineno   *int   `json:"lineno,omitempty"`
	AbsPath  string `json:"abs_path,omitempty"`
	Module   string `json:"module,omitempty"`
	InApp    bool   `json:"in_app"`
}

type errorEvent struct {
	EventID     string            `json:"event_id"`
	Timestamp   time.Time         `json:"timestamp"`
	Platform    string            `json:"platform"`
	Release     string            `json:"release,omitempty"`
	Environment string            `json:"environment,omitempty"`
	Level       string            `json:"level"`
	Message     string            `json:"message"`
	Exception   errorException    `json:"exception"`
	Tags        map[string]string `json:"tags,omitempty"`
	Contexts    *errorContexts    `json:"contexts,omitempty"`
	URL         string            `json:"url,omitempty"`
}

type errorException struct {
	Type       string          `json:"type"`
	Value      string          `json:"value"`
	Stacktrace errorStacktrace `json:"stacktrace"`
}

type errorStacktrace struct {
	Frames []errorFrame `json:"frames"`
}

type errorContexts struct {
	Trace *errorTraceContext `json:"trace,omitempty"`
}

type errorTraceContext struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
}

type errorCaptureOptions struct {
	exceptionType string
	frames        []errorFrame
	omitStack     bool
	tags          map[string]string
	url           string
}

type errorTransport struct {
	endpoint string
	token    string
	client   *http.Client
	logger   *slog.Logger
	delivery *deliveryQueue[errorEvent]
}

func newErrorTransport(
	endpoint, token string,
	logger *slog.Logger,
	config DeliveryConfig,
	onDrop func(signal, reason string, count int),
) *errorTransport {
	transport := &errorTransport{
		endpoint: strings.TrimRight(endpoint, "/") + "/api/events/errors",
		token:    token,
		client:   &http.Client{Timeout: errorSendTimeout},
		logger:   logger,
	}
	transport.delivery = newDeliveryQueue(deliveryOptions[errorEvent]{
		Capacity:      config.ErrorQueueCapacity,
		BatchSize:     errorBatchSize,
		FlushInterval: errorFlushInterval,
		MaxAttempts:   config.MaxAttempts,
		RetryInitial:  config.RetryInitial,
		RetryMax:      config.RetryMax,
		Send:          transport.send,
		Warn: func(reason string, err error, dropped int) {
			if onDrop != nil {
				onDrop("error", reason, dropped)
			}
			if logger != nil {
				logger.Warn(
					"error event telemetry dropped",
					"reason",
					reason,
					"events",
					dropped,
					"error",
					err,
				)
			}
		},
	})
	return transport
}

// CaptureException queues err for Issue aggregation and returns its event ID.
// It is non-blocking; an empty ID means err was nil or the SDK is degraded.
func (c *Client) CaptureException(ctx context.Context, err error) string {
	if err == nil {
		return ""
	}
	return c.captureException(ctx, err, errorCaptureOptions{frames: callerFrames(3)})
}

func (c *Client) captureException(ctx context.Context, err error, options errorCaptureOptions) string {
	if err == nil || c.errorTransport == nil {
		return ""
	}
	eventID := newEventID()
	frames := options.frames
	if options.omitStack {
		frames = []errorFrame{}
	}
	event := errorEvent{
		EventID:     eventID,
		Timestamp:   time.Now().UTC(),
		Platform:    "go",
		Release:     c.cfg.Release,
		Environment: c.cfg.Environment,
		Level:       "error",
		Message:     err.Error(),
		Exception: errorException{
			Type:       exceptionType(err, options.exceptionType),
			Value:      err.Error(),
			Stacktrace: errorStacktrace{Frames: frames},
		},
		Tags: mergedErrorTags(c.cfg.ServiceName, c.cfg.Tags, options.tags),
		URL:  options.url,
	}
	if spanContext := trace.SpanContextFromContext(ctx); spanContext.IsValid() {
		event.Contexts = &errorContexts{Trace: &errorTraceContext{
			TraceID: spanContext.TraceID().String(),
			SpanID:  spanContext.SpanID().String(),
		}}
	}
	c.errorTransport.enqueue(event)
	return eventID
}

func (t *errorTransport) enqueue(event errorEvent) bool {
	return t.delivery.enqueue(event)
}

func (t *errorTransport) close(ctx context.Context) error {
	return t.delivery.close(ctx)
}

func (t *errorTransport) dropped() uint64 {
	return t.delivery.dropped()
}

func (t *errorTransport) send(parent context.Context, events []errorEvent) error {
	body, err := json.Marshal(struct {
		Events []errorEvent `json:"events"`
	}{Events: events})
	if err != nil {
		return fmt.Errorf("marshal error events: %w", err)
	}
	ctx, cancel := context.WithTimeout(parent, errorSendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create error event request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+t.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("send error events: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		response, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("send error events: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(response)))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func callerFrames(skip int) []errorFrame {
	pcs := make([]uintptr, 64)
	count := runtime.Callers(skip, pcs)
	iterator := runtime.CallersFrames(pcs[:count])
	frames := make([]errorFrame, 0, count)
	for {
		frame, more := iterator.Next()
		line := frame.Line
		frames = append(frames, errorFrame{
			Function: frame.Function,
			Filename: filepath.Base(frame.File),
			Lineno:   &line,
			AbsPath:  frame.File,
			Module:   frameModule(frame.Function),
			InApp:    isInAppFrame(frame.Function, frame.File),
		})
		if !more {
			break
		}
	}
	for left, right := 0, len(frames)-1; left < right; left, right = left+1, right-1 {
		frames[left], frames[right] = frames[right], frames[left]
	}
	return frames
}

func frameModule(function string) string {
	if index := strings.LastIndex(function, "."); index > 0 {
		return function[:index]
	}
	return ""
}

func isInAppFrame(function, filename string) bool {
	if strings.HasPrefix(function, "runtime.") ||
		strings.HasPrefix(function, "testing.") ||
		strings.Contains(function, "github.com/gofiber/fiber/") {
		return false
	}
	base := filepath.Base(filename)
	if (base == "fiber.go" || base == "error_event.go") &&
		strings.Contains(function, "github.com/eimsound/lazy-observe/sdk/go") {
		return false
	}
	return true
}

func exceptionType(err error, override string) string {
	if override != "" {
		return override
	}
	valueType := reflect.TypeOf(err)
	for valueType.Kind() == reflect.Pointer {
		valueType = valueType.Elem()
	}
	name := valueType.Name()
	first, _ := utf8.DecodeRuneInString(name)
	if name == "" || !unicode.IsUpper(first) {
		return "Error"
	}
	return name
}

func mergedErrorTags(service string, configured, event map[string]string) map[string]string {
	tags := make(map[string]string, len(configured)+len(event)+1)
	for key, value := range configured {
		tags[key] = value
	}
	tags["service.name"] = service
	for key, value := range event {
		tags[key] = value
	}
	return tags
}

func newEventID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	// crypto/rand failure is exceptionally rare; preserve a valid non-empty ID
	// without making error capture fail the host application.
	return hex.EncodeToString([]byte(fmt.Sprintf("%d", time.Now().UnixNano())))
}
