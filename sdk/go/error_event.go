package lazyobserve

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"reflect"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"go.opentelemetry.io/otel/trace"
)

const (
	errorBatchSize       = 20
	errorFlushInterval   = 100 * time.Millisecond
	errorQueueSize       = 100
	errorSendTimeout     = 5 * time.Second
	maxErrorEventBytes   = 200 << 10
	maxErrorMessageBytes = 32 << 10
	maxErrorURLBytes     = 4 << 10
	maxErrorTagCount     = 64
	maxErrorTagKeyBytes  = 128
	maxErrorTagValBytes  = 1024
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
	bounded, ok := boundErrorEvent(event, maxErrorEventBytes)
	if !ok {
		c.errorTransport.delivery.recordDrop(
			"server_rejected",
			fmt.Errorf("error event exceeds %d-byte limit", maxErrorEventBytes),
			1,
		)
		return eventID
	}
	c.errorTransport.enqueue(bounded)
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
	if len(events) == 0 {
		return nil
	}
	err := t.postBatch(parent, events)
	if err == nil {
		return nil
	}
	var dErr *deliveryError
	if len(events) > 1 && errors.As(err, &dErr) && dErr.statusCode == http.StatusRequestEntityTooLarge {
		accepted, lastRejectErr, retryableErr := t.sendSplitOnTooLarge(parent, events)
		if retryableErr != nil {
			return retryableErr
		}
		if accepted == 0 && lastRejectErr != nil {
			return &deliveryHandledDropError{err: lastRejectErr}
		}
		return nil
	}
	return err
}

func (t *errorTransport) sendSplitOnTooLarge(parent context.Context, events []errorEvent) (int, error, error) {
	mid := len(events) / 2
	halves := [][]errorEvent{events[:mid], events[mid:]}
	accepted := 0
	var lastRejectErr error
	for _, half := range halves {
		err := t.postBatch(parent, half)
		if err == nil {
			accepted += len(half)
			continue
		}
		var dErr *deliveryError
		if errors.As(err, &dErr) && dErr.statusCode == http.StatusRequestEntityTooLarge {
			if len(half) == 1 {
				t.delivery.recordDrop("server_rejected", err, 1)
				lastRejectErr = err
				continue
			}
			subAccepted, subReject, retryableErr := t.sendSplitOnTooLarge(parent, half)
			accepted += subAccepted
			if subReject != nil {
				lastRejectErr = subReject
			}
			if retryableErr != nil {
				return accepted, lastRejectErr, retryableErr
			}
			continue
		}
		if errors.As(err, &dErr) && dErr.permanent {
			t.delivery.recordDrop("server_rejected", err, len(half))
			lastRejectErr = err
			continue
		}
		return accepted, lastRejectErr, err
	}
	return accepted, lastRejectErr, nil
}

func (t *errorTransport) postBatch(parent context.Context, events []errorEvent) error {
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
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return &deliveryError{
			statusCode: resp.StatusCode,
			retryAfter: parseRetryAfter(resp.Header.Get("Retry-After")),
			permanent:  isPermanentHTTPStatus(resp.StatusCode),
			err: fmt.Errorf(
				"send error events: HTTP %d: %s",
				resp.StatusCode,
				strings.TrimSpace(string(response)),
			),
		}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func boundErrorEvent(event errorEvent, limit int) (errorEvent, bool) {
	event.Message = truncateUTF8Bytes(event.Message, maxErrorMessageBytes)
	event.Exception.Value = truncateUTF8Bytes(event.Exception.Value, maxErrorMessageBytes)
	event.URL = truncateUTF8Bytes(event.URL, maxErrorURLBytes)
	event.Tags = boundErrorTags(event.Tags, maxErrorTagCount, maxErrorTagKeyBytes, maxErrorTagValBytes)
	if eventFitsJSON(event, limit) {
		return event, true
	}

	for _, textLimit := range []int{4 << 10, 1024, 256} {
		event.Message = truncateUTF8Bytes(event.Message, textLimit)
		event.Exception.Value = truncateUTF8Bytes(event.Exception.Value, textLimit)
		if eventFitsJSON(event, limit) {
			return event, true
		}
	}

	event.URL = truncateUTF8Bytes(event.URL, 256)
	event.Tags = essentialErrorTags(event.Tags, 256)
	if eventFitsJSON(event, limit) {
		return event, true
	}

	if len(event.Exception.Stacktrace.Frames) > 0 {
		frames := slices.Clone(event.Exception.Stacktrace.Frames)
		for i := range frames {
			frames[i].AbsPath = ""
		}
		event.Exception.Stacktrace.Frames = frames
		if eventFitsJSON(event, limit) {
			return event, true
		}
		for _, maxFrames := range []int{32, 16, 8, 0} {
			if len(frames) > maxFrames {
				event.Exception.Stacktrace.Frames = frames[len(frames)-maxFrames:]
			}
			if eventFitsJSON(event, limit) {
				return event, true
			}
		}
	}
	return errorEvent{}, false
}

func eventFitsJSON(event errorEvent, limit int) bool {
	encoded, err := json.Marshal(event)
	return err == nil && len(encoded) <= limit
}

func truncateUTF8Bytes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	const ellipsis = "..."
	if limit <= len(ellipsis) {
		trimmed := value[:limit]
		for !utf8.ValidString(trimmed) {
			trimmed = trimmed[:len(trimmed)-1]
		}
		return trimmed
	}
	trimmed := value[:limit-len(ellipsis)]
	for !utf8.ValidString(trimmed) {
		trimmed = trimmed[:len(trimmed)-1]
	}
	return trimmed + ellipsis
}

var essentialTagKeys = []string{
	"service.name",
	"http.request.method",
	"http.route",
	"http.response.status_code",
}

func boundErrorTags(tags map[string]string, maxCount, maxKey, maxVal int) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, min(len(tags), maxCount))
	for _, key := range essentialTagKeys {
		if value, ok := tags[key]; ok && len(out) < maxCount {
			out[key] = truncateUTF8Bytes(value, maxVal)
		}
	}
	if len(out) >= maxCount {
		return out
	}
	keys := make([]string, 0, len(tags))
	for key := range tags {
		if _, exists := out[key]; !exists {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		if len(out) >= maxCount {
			break
		}
		truncKey := truncateUTF8Bytes(key, maxKey)
		if truncKey == "" {
			continue
		}
		if _, exists := out[truncKey]; exists {
			continue
		}
		out[truncKey] = truncateUTF8Bytes(tags[key], maxVal)
	}
	return out
}

func essentialErrorTags(tags map[string]string, maxVal int) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(essentialTagKeys))
	for _, key := range essentialTagKeys {
		if value, ok := tags[key]; ok {
			out[key] = truncateUTF8Bytes(value, maxVal)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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

// frameModule extracts the Go package import path from runtime.Frame.Function:
// everything before the first '.' after the last '/' (ignoring generic type parameters).
func frameModule(function string) string {
	if idx := strings.IndexByte(function, '['); idx >= 0 {
		function = function[:idx]
	}
	searchStart := 0
	if slash := strings.LastIndexByte(function, '/'); slash >= 0 {
		searchStart = slash + 1
	}
	dot := strings.IndexByte(function[searchStart:], '.')
	if dot <= 0 {
		return ""
	}
	return function[:searchStart+dot]
}

var (
	buildModulesOnce sync.Once
	cachedMainModule string
	cachedDepModules []string
)

func loadBuildModules() (string, []string) {
	buildModulesOnce.Do(func() {
		info, ok := debug.ReadBuildInfo()
		if !ok || info == nil {
			return
		}
		cachedMainModule = info.Main.Path
		deps := make([]string, 0, len(info.Deps))
		for _, dep := range info.Deps {
			if dep != nil && dep.Path != "" {
				deps = append(deps, dep.Path)
			}
		}
		cachedDepModules = deps
	})
	return cachedMainModule, cachedDepModules
}

func isInAppFrame(function, filename string) bool {
	mainModule, depModules := loadBuildModules()
	return isInAppFrameWithBuildInfo(function, filename, mainModule, depModules)
}

func isInAppFrameWithBuildInfo(function, filename, mainModule string, depModules []string) bool {
	pkg := frameModule(function)
	if pkg == "" {
		return false
	}
	if pkg == instrumentationName && !strings.HasSuffix(filepath.Base(filename), "_test.go") {
		return false
	}
	if matchesModulePath(pkg, "github.com/valyala/fasthttp") ||
		matchesModulePath(pkg, "github.com/gofiber/fiber") ||
		matchesModulePath(pkg, "github.com/gofiber/utils") ||
		strings.HasPrefix(pkg, "go.opentelemetry.io/") {
		return false
	}
	if pkg == "main" || strings.HasPrefix(pkg, "main/") {
		return true
	}
	if mainModule != "" && matchesModulePath(pkg, mainModule) {
		return true
	}
	for _, dep := range depModules {
		if matchesModulePath(pkg, dep) {
			return false
		}
	}
	slashFile := filepath.ToSlash(filename)
	if strings.Contains(slashFile, "/pkg/mod/") || strings.Contains(slashFile, "/vendor/") {
		return false
	}
	firstSegment, _, _ := strings.Cut(pkg, "/")
	return strings.Contains(firstSegment, ".")
}

func matchesModulePath(pkg, modulePath string) bool {
	return pkg == modulePath || strings.HasPrefix(pkg, modulePath+"/")
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
