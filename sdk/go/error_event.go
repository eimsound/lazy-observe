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
	errorBatchSize              = 20
	errorFlushInterval          = 100 * time.Millisecond
	errorQueueSize              = 100
	errorSendTimeout            = 5 * time.Second
	maxErrorEventBytes          = 200 << 10
	maxErrorMessageBytes        = 32 << 10
	maxErrorExceptionTypeBytes  = 256
	maxErrorExceptionValueBytes = 32 << 10
	maxErrorReleaseBytes        = 256
	maxErrorEnvironmentBytes    = 128
	maxErrorURLBytes            = 4 << 10
	maxErrorTagCount            = 64
	maxErrorTagKeyBytes         = 128
	maxErrorTagValBytes         = 1024
	maxErrorFrameCount          = 64
	maxErrorFrameFunctionBytes  = 512
	maxErrorFrameFilenameBytes  = 256
	maxErrorFrameAbsPathBytes   = 1024
	maxErrorFrameModuleBytes    = 256
)

// errorEventBudget centralizes all per-field and whole-event byte/count limits
// aligned with the server gateway's 200 KiB single error-event ingest contract.
type errorEventBudget struct {
	MaxEventBytes          int
	MaxMessageBytes        int
	MaxExceptionTypeBytes  int
	MaxExceptionValueBytes int
	MaxReleaseBytes        int
	MaxEnvironmentBytes    int
	MaxURLBytes            int
	MaxTagCount            int
	MaxTagKeyBytes         int
	MaxTagValueBytes       int
	MaxFrames              int
	MaxFrameFunctionBytes  int
	MaxFrameFilenameBytes  int
	MaxFrameAbsPathBytes   int
	MaxFrameModuleBytes    int
}

var defaultErrorEventBudget = errorEventBudget{
	MaxEventBytes:          maxErrorEventBytes,
	MaxMessageBytes:        maxErrorMessageBytes,
	MaxExceptionTypeBytes:  maxErrorExceptionTypeBytes,
	MaxExceptionValueBytes: maxErrorExceptionValueBytes,
	MaxReleaseBytes:        maxErrorReleaseBytes,
	MaxEnvironmentBytes:    maxErrorEnvironmentBytes,
	MaxURLBytes:            maxErrorURLBytes,
	MaxTagCount:            maxErrorTagCount,
	MaxTagKeyBytes:         maxErrorTagKeyBytes,
	MaxTagValueBytes:       maxErrorTagValBytes,
	MaxFrames:              maxErrorFrameCount,
	MaxFrameFunctionBytes:  maxErrorFrameFunctionBytes,
	MaxFrameFilenameBytes:  maxErrorFrameFilenameBytes,
	MaxFrameAbsPathBytes:   maxErrorFrameAbsPathBytes,
	MaxFrameModuleBytes:    maxErrorFrameModuleBytes,
}

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
	ingest       *ingestClient
	logger       *slog.Logger
	warnThrottle *queueFullLogThrottle
	delivery     *deliveryQueue[errorEvent]
}

func newErrorTransport(
	ingest *ingestClient,
	logger *slog.Logger,
	config DeliveryConfig,
	onDrop func(signal, reason string, count int),
) *errorTransport {
	transport := &errorTransport{
		ingest:       ingest,
		logger:       logger,
		warnThrottle: newQueueFullLogThrottle(deliveryDefaultQueueFullLogInterval),
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
			if logger == nil {
				return
			}
			allowed, suppressed := transport.warnThrottle.allow(reason, dropped)
			if !allowed {
				return
			}
			if suppressed > 0 {
				logger.Warn(
					"error event telemetry dropped",
					"reason",
					reason,
					"events",
					dropped,
					"suppressed",
					suppressed,
					"error",
					err,
				)
				return
			}
			logger.Warn(
				"error event telemetry dropped",
				"reason",
				reason,
				"events",
				dropped,
				"error",
				err,
			)
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
	bounded, ok := boundErrorEventWithBudget(event, defaultErrorEventBudget)
	if !ok {
		c.errorTransport.delivery.recordDrop(
			"server_rejected",
			fmt.Errorf("error event exceeds %d-byte limit", defaultErrorEventBudget.MaxEventBytes),
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
	return t.ingest.post(parent, ingestRequest{
		Path:          "/api/events/errors",
		ContentType:   "application/json",
		ContentLength: int64(len(body)),
		Body:          func() io.ReadCloser { return io.NopCloser(bytes.NewReader(body)) },
		Timeout:       errorSendTimeout,
		Op:            "send error events",
	})
}

func boundErrorEvent(event errorEvent, limit int) (errorEvent, bool) {
	budget := defaultErrorEventBudget
	budget.MaxEventBytes = limit
	return boundErrorEventWithBudget(event, budget)
}

func boundErrorEventWithBudget(event errorEvent, budget errorEventBudget) (errorEvent, bool) {
	if budget.MaxEventBytes <= 0 {
		return errorEvent{}, false
	}
	event.Release = truncateUTF8Bytes(event.Release, budget.MaxReleaseBytes)
	event.Environment = truncateUTF8Bytes(event.Environment, budget.MaxEnvironmentBytes)
	event.Exception.Type = truncateUTF8Bytes(event.Exception.Type, budget.MaxExceptionTypeBytes)
	event.Message = truncateUTF8Bytes(event.Message, budget.MaxMessageBytes)
	event.Exception.Value = truncateUTF8Bytes(event.Exception.Value, budget.MaxExceptionValueBytes)
	event.URL = truncateUTF8Bytes(event.URL, budget.MaxURLBytes)
	event.Tags = boundErrorTags(event.Tags, budget.MaxTagCount, budget.MaxTagKeyBytes, budget.MaxTagValueBytes)
	event.Exception.Stacktrace.Frames = boundErrorFrames(event.Exception.Stacktrace.Frames, budget)
	if !validErrorEventContract(event) {
		return errorEvent{}, false
	}
	if eventFitsJSON(event, budget.MaxEventBytes) {
		return event, true
	}

	for _, textLimit := range []int{4 << 10, 1024, 256} {
		event.Message = truncateUTF8Bytes(event.Message, min(textLimit, budget.MaxMessageBytes))
		event.Exception.Value = truncateUTF8Bytes(event.Exception.Value, min(textLimit, budget.MaxExceptionValueBytes))
		if eventFitsJSON(event, budget.MaxEventBytes) {
			return event, true
		}
	}

	event.URL = truncateUTF8Bytes(event.URL, min(256, budget.MaxURLBytes))
	event.Tags = essentialErrorTags(event.Tags, min(256, budget.MaxTagValueBytes))
	if eventFitsJSON(event, budget.MaxEventBytes) {
		return event, true
	}

	if len(event.Exception.Stacktrace.Frames) > 0 {
		frames := slices.Clone(event.Exception.Stacktrace.Frames)
		for i := range frames {
			frames[i].AbsPath = ""
		}
		event.Exception.Stacktrace.Frames = frames
		if eventFitsJSON(event, budget.MaxEventBytes) {
			return event, true
		}
		for _, maxFrames := range []int{32, 16, 8, 0} {
			if len(frames) > maxFrames {
				event.Exception.Stacktrace.Frames = frames[len(frames)-maxFrames:]
			}
			if eventFitsJSON(event, budget.MaxEventBytes) {
				return event, true
			}
		}
	}
	return errorEvent{}, false
}

func boundErrorFrames(frames []errorFrame, budget errorEventBudget) []errorFrame {
	if len(frames) == 0 || budget.MaxFrames == 0 {
		return []errorFrame{}
	}
	valid := make([]errorFrame, 0, len(frames))
	for _, frame := range frames {
		bounded := errorFrame{
			Function: truncateUTF8Bytes(frame.Function, budget.MaxFrameFunctionBytes),
			Filename: truncateUTF8Bytes(frame.Filename, budget.MaxFrameFilenameBytes),
			Lineno:   frame.Lineno,
			AbsPath:  truncateUTF8Bytes(frame.AbsPath, budget.MaxFrameAbsPathBytes),
			Module:   truncateUTF8Bytes(frame.Module, budget.MaxFrameModuleBytes),
			InApp:    frame.InApp,
		}
		if strings.TrimSpace(bounded.Function) == "" && strings.TrimSpace(bounded.Filename) == "" {
			continue
		}
		valid = append(valid, bounded)
	}
	if budget.MaxFrames > 0 && len(valid) > budget.MaxFrames {
		// Frames are outermost-first; preserve the innermost crash/capture frames.
		valid = valid[len(valid)-budget.MaxFrames:]
	}
	return valid
}

// validErrorEventContract mirrors the server gateway's structural validation
// in internal/gateway/error_api.go so malformed events never poison a batch.
func validErrorEventContract(event errorEvent) bool {
	if strings.TrimSpace(event.EventID) == "" || event.Timestamp.IsZero() ||
		strings.TrimSpace(event.Platform) == "" || strings.TrimSpace(event.Level) == "" {
		return false
	}
	if strings.TrimSpace(event.Exception.Type) == "" && strings.TrimSpace(event.Message) == "" {
		return false
	}
	for _, frame := range event.Exception.Stacktrace.Frames {
		if strings.TrimSpace(frame.Function) == "" && strings.TrimSpace(frame.Filename) == "" {
			return false
		}
	}
	return true
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
	pcs := make([]uintptr, maxErrorFrameCount)
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
