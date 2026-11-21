package gateway

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	storagepkg "github.com/eimsound/lazy-observe/internal/storage"
	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
	"go.opentelemetry.io/otel/metric"
)

const (
	defaultUnaryProxyTimeout  = 60 * time.Second
	statusClientClosedRequest = 499
	maxStreamErrorBodyBytes   = 1 << 20
)

type proxyKind uint8

const (
	proxyKindUnary proxyKind = iota
	proxyKindStream
)

type proxyConcurrencyGroup string

const (
	proxyConcurrencyNone     proxyConcurrencyGroup = ""
	proxyConcurrencyLiveTail proxyConcurrencyGroup = "live_tail"
)

type storageProxyConfig struct {
	storage             storagepkg.SignalBackend
	client              *http.Client
	tailClient          *http.Client
	proxyTimeout        time.Duration
	liveTail            *liveTailLimiter
	liveTailMaxDuration time.Duration
	liveTailHeartbeat   time.Duration
	shutdownCtx         context.Context
	logger              *slog.Logger
	meterProvider       metric.MeterProvider
}

// storageProxy isolates all unary and streaming reverse-proxy state and logic
// for metrics, logs, and traces from the Gateway composition root.
type storageProxy struct {
	storage             storagepkg.SignalBackend
	client              *http.Client
	tailClient          *http.Client
	proxyTimeout        time.Duration
	liveTail            *liveTailLimiter
	liveTailMaxDuration time.Duration
	liveTailHeartbeat   time.Duration
	shutdownCtx         context.Context
	logger              *slog.Logger
	streamErrors        atomic.Int64
	streamErrCounter    metric.Int64Counter
}

func newStorageProxy(cfg storageProxyConfig) *storageProxy {
	p := &storageProxy{
		storage:             cfg.storage,
		client:              cfg.client,
		tailClient:          cfg.tailClient,
		proxyTimeout:        cfg.proxyTimeout,
		liveTail:            cfg.liveTail,
		liveTailMaxDuration: cfg.liveTailMaxDuration,
		liveTailHeartbeat:   cfg.liveTailHeartbeat,
		shutdownCtx:         cfg.shutdownCtx,
		logger:              cfg.logger,
	}
	if cfg.meterProvider != nil {
		meter := cfg.meterProvider.Meter("github.com/eimsound/lazy-observe/internal/gateway")
		p.streamErrCounter, _ = meter.Int64Counter("gateway.proxy.stream_errors")
	}
	return p
}

// proxySpec declaratively describes one proxied upstream request so bounded
// unary requests and long-lived streaming connections follow explicit contracts
// without path-based special-casing.
type proxySpec struct {
	kind             proxyKind
	base             string
	upstreamPath     string
	timeout          time.Duration
	maxDuration      time.Duration
	heartbeat        time.Duration
	concurrencyGroup proxyConcurrencyGroup
	requireTenant    bool
	projectID        int
	metricsProject   string
	reservedParams   bool
	allowedParams    map[string]bool
	addQuery         url.Values
	requestHeaders   map[string]bool
	responseHeaders  map[string]bool
	body             []byte
}

type logsProxyRoute struct {
	apiPath          string
	upstreamPath     string
	kind             proxyKind
	concurrencyGroup proxyConcurrencyGroup
}

var logsProxyRoutes = []logsProxyRoute{
	{apiPath: "/api/v1/logs/query", upstreamPath: "/select/logsql/query", kind: proxyKindUnary},
	{apiPath: "/api/v1/logs/hits", upstreamPath: "/select/logsql/hits", kind: proxyKindUnary},
	{apiPath: "/api/v1/logs/stats_query_range", upstreamPath: "/select/logsql/stats_query_range", kind: proxyKindUnary},
	{apiPath: "/api/v1/logs/query_time_range", upstreamPath: "/select/logsql/query_time_range", kind: proxyKindUnary},
	{apiPath: "/api/v1/logs/field_names", upstreamPath: "/select/logsql/field_names", kind: proxyKindUnary},
	{apiPath: "/api/v1/logs/field_values", upstreamPath: "/select/logsql/field_values", kind: proxyKindUnary},
	{
		apiPath:      "/api/v1/logs/stream_field_names",
		upstreamPath: "/select/logsql/stream_field_names",
		kind:         proxyKindUnary,
	},
	{
		apiPath:      "/api/v1/logs/stream_field_values",
		upstreamPath: "/select/logsql/stream_field_values",
		kind:         proxyKindUnary,
	},
	{
		apiPath:          "/api/v1/logs/tail",
		upstreamPath:     "/select/logsql/tail",
		kind:             proxyKindStream,
		concurrencyGroup: proxyConcurrencyLiveTail,
	},
}

var defaultUnaryRequestHeaders = map[string]bool{
	"content-type":                      true,
	"content-encoding":                  true,
	"accept":                            true,
	"accept-encoding":                   true,
	"user-agent":                        true,
	"x-prometheus-remote-write-version": true,
	"x-lazy-observe-perf-request":       true,
}

// defaultStreamRequestHeaders intentionally omits Accept-Encoding so upstream
// storage does not buffer small streaming chunks inside a gzip/zstd compressor.
var defaultStreamRequestHeaders = map[string]bool{
	"content-type":                      true,
	"content-encoding":                  true,
	"accept":                            true,
	"user-agent":                        true,
	"x-prometheus-remote-write-version": true,
	"x-lazy-observe-perf-request":       true,
}

var defaultProxyResponseHeaders = map[string]bool{
	"content-type":        true,
	"content-encoding":    true,
	"content-disposition": true,
	"cache-control":       true,
	"vary":                true,
	"retry-after":         true,
	"x-prometheus-remote-write-written-samples":    true,
	"x-prometheus-remote-write-written-histograms": true,
	"x-prometheus-remote-write-written-exemplars":  true,
	"x-lazy-observe-perf-sink":                     true,
	"x-lazy-observe-perf-receipt":                  true,
	"x-lazy-observe-perf-request":                  true,
}

func (p *storageProxy) logsHandler(route logsProxyRoute) fiber.Handler {
	return func(c fiber.Ctx) error {
		project := currentProject(c)
		spec := proxySpec{
			kind:             route.kind,
			base:             p.storage.LogsQueryBase(),
			upstreamPath:     route.upstreamPath,
			concurrencyGroup: route.concurrencyGroup,
			requireTenant:    true,
			projectID:        project.ID,
		}
		if route.kind == proxyKindStream {
			spec.maxDuration = p.liveTailMaxDuration
			spec.heartbeat = p.liveTailHeartbeat
		}
		return p.execute(c, spec)
	}
}

func (p *storageProxy) tenantHandler(base, upstreamPath string) fiber.Handler {
	return func(c fiber.Ctx) error {
		project := currentProject(c)
		return p.execute(c, proxySpec{
			kind:          proxyKindUnary,
			base:          base,
			upstreamPath:  upstreamPath,
			requireTenant: true,
			projectID:     project.ID,
		})
	}
}

func (p *storageProxy) metricsWrite(c fiber.Ctx, upstreamPath string, addProjectQuery bool) error {
	return p.metricsWriteWithBody(c, upstreamPath, addProjectQuery, nil)
}

func (p *storageProxy) metricsWriteWithBody(
	c fiber.Ctx,
	upstreamPath string,
	addProjectQuery bool,
	body []byte,
) error {
	if !p.storage.MetricsIngestEnabled() {
		return c.Status(fiber.StatusNotImplemented).
			JSON(fiber.Map{"error": "metrics ingest is disabled: storage.metrics.ingest_url is not configured"})
	}
	project := currentProject(c)
	base := p.storage.MetricsIngestBase()
	if !p.storage.MetricsClustered() {
		upstreamPath = strings.TrimPrefix(upstreamPath, "/prometheus")
	}
	spec := proxySpec{
		kind:           proxyKindUnary,
		base:           base,
		upstreamPath:   upstreamPath,
		reservedParams: true,
		body:           body,
	}
	if addProjectQuery {
		extraLabel, err := p.storage.MetricsIngestExtraLabel(project.Slug)
		if err != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "project context is missing")
		}
		spec.addQuery = url.Values{"extra_label": []string{extraLabel}}
	}
	return p.execute(c, spec)
}

func (p *storageProxy) metricsQuery(c fiber.Ctx) error {
	project := currentProject(c)
	// proxy only isolates when metricsProject is non-empty; fail closed instead of forwarding an unfiltered query.
	if project.Slug == "" {
		return fiber.NewError(fiber.StatusInternalServerError, "project context is missing")
	}
	upstreamPath := c.Path()
	if c.Params("name") != "" {
		labelName, err := unescapedPathParam(c, "name", nil)
		if err != nil {
			return err
		}
		upstreamPath = "/api/v1/label/" + url.PathEscape(labelName) + "/values"
	}
	return p.execute(c, proxySpec{
		kind:           proxyKindUnary,
		base:           p.storage.MetricsQueryBase(),
		upstreamPath:   upstreamPath,
		metricsProject: project.Slug,
		reservedParams: true,
	})
}

// metricsPrettify exposes VictoriaMetrics' parser-backed formatter without
// opening a data-read path. It is still project-authenticated, but only the
// query parameter is forwarded; storage isolation parameters and arbitrary
// client-supplied options are deliberately discarded.
func (p *storageProxy) metricsPrettify(c fiber.Ctx) error {
	return p.execute(c, proxySpec{
		kind:          proxyKindUnary,
		base:          p.storage.MetricsQueryBase(),
		upstreamPath:  "/prettify-query",
		allowedParams: map[string]bool{"query": true},
	})
}

func (p *storageProxy) execute(c fiber.Ctx, spec proxySpec) error {
	if (spec.requireTenant && spec.projectID <= 0) || spec.projectID < 0 {
		return fiber.NewError(fiber.StatusInternalServerError, "project context is missing")
	}
	switch spec.kind {
	case proxyKindUnary:
		return p.unary(c, spec)
	case proxyKindStream:
		return p.stream(c, spec)
	default:
		return fiber.NewError(fiber.StatusInternalServerError, "unsupported proxy kind")
	}
}

func (p *storageProxy) unary(c fiber.Ctx, spec proxySpec) error {
	query, err := url.ParseQuery(string(c.RequestCtx().QueryArgs().QueryString()))
	if err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid query parameters")
	}
	if spec.reservedParams || spec.metricsProject != "" {
		storagepkg.StripReservedMetricsParams(query)
	}
	body := ownedProxyRequestBody(c, spec.body)
	contentType := string(c.Request().Header.ContentType())
	mediaType := normalizedMediaType(contentType)
	hasFormMatch := false
	if (spec.reservedParams || spec.metricsProject != "") && mediaType == fiber.MIMEApplicationForm {
		form, formErr := url.ParseQuery(string(body))
		if formErr != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid form body")
		}
		storagepkg.StripReservedMetricsParams(form)
		if spec.metricsProject != "" && p.storage.MetricsMode() == storagepkg.MetricsPrometheus {
			if injectErr := storagepkg.InjectPromValues(form, spec.metricsProject); injectErr != nil {
				return fiber.NewError(fiber.StatusBadRequest, "invalid PromQL: "+injectErr.Error())
			}
			hasFormMatch = len(form["match[]"]) > 0
		}
		body = []byte(form.Encode())
	} else if spec.metricsProject != "" &&
		(mediaType == "multipart/form-data" ||
			((c.Method() == http.MethodPost || c.Method() == http.MethodPut || c.Method() == http.MethodPatch) &&
				len(body) > 0 && mediaType != fiber.MIMEApplicationForm)) {
		return fiber.NewError(
			fiber.StatusUnsupportedMediaType,
			"metrics requests with a body must use application/x-www-form-urlencoded",
		)
	}
	if spec.metricsProject != "" {
		defaultMatcher := spec.upstreamPath != "/api/v1/query" &&
			spec.upstreamPath != "/api/v1/query_range" &&
			!hasFormMatch
		if err := p.applyMetricsIsolation(query, spec.metricsProject, defaultMatcher); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid PromQL: "+err.Error())
		}
	}
	applyProxyQueryRules(query, spec.allowedParams, spec.addQuery)
	target := buildProxyTarget(spec.base, spec.upstreamPath, query)

	timeout := spec.timeout
	if timeout <= 0 {
		timeout = p.proxyTimeout
	}
	if timeout <= 0 {
		timeout = defaultUnaryProxyTimeout
	}
	parentCtx := c.Context()
	reqCtx, cancel := context.WithTimeout(parentCtx, timeout)
	req, err := http.NewRequestWithContext(reqCtx, c.Method(), target, requestBodyReader(body))
	if err != nil {
		cancel()
		p.log().Error("build upstream proxy request failed", "path", spec.upstreamPath, "error", err)
		return fiber.NewError(fiber.StatusInternalServerError, "failed to construct upstream request")
	}
	reqAllowlist := spec.requestHeaders
	if reqAllowlist == nil {
		reqAllowlist = defaultUnaryRequestHeaders
	}
	copyAllowlistedRequestHeaders(req.Header, &c.Request().Header, reqAllowlist)
	if spec.projectID > 0 {
		if err := p.storage.ApplyTenantHeaders(req.Header, spec.projectID); err != nil {
			cancel()
			return fiber.NewError(fiber.StatusInternalServerError, "project context is missing")
		}
	}

	resp, err := p.client.Do(req)
	if err != nil {
		cancel()
		return p.writeUpstreamError(c, parentCtx, reqCtx, spec.upstreamPath, err)
	}
	respAllowlist := spec.responseHeaders
	if respAllowlist == nil {
		respAllowlist = defaultProxyResponseHeaders
	}
	copyAllowlistedResponseHeaders(c, resp.Header, respAllowlist)
	c.Status(resp.StatusCode)
	if resp.StatusCode == http.StatusNoContent ||
		resp.StatusCode == http.StatusNotModified ||
		resp.ContentLength == 0 {
		drainAndCloseBody(resp.Body)
		cancel()
		return nil
	}
	statusCode := resp.StatusCode
	upstreamPath := spec.upstreamPath
	c.RequestCtx().SetBodyStreamWriter(func(w *bufio.Writer) {
		defer cancel()
		defer resp.Body.Close()
		if _, copyErr := io.Copy(w, resp.Body); copyErr != nil {
			p.recordStreamError(upstreamPath, statusCode, copyErr)
		}
	})
	return nil
}

func (p *storageProxy) stream(c fiber.Ctx, spec proxySpec) error {
	shutdownCtx := p.shutdownCtx
	if shutdownCtx == nil {
		shutdownCtx = context.Background()
	}
	if shutdownCtx.Err() != nil {
		return writeGatewayDraining(c)
	}

	u, _ := c.Locals(userLocal).(meta.UserInfo)
	releaseSlot, ok := p.acquireConcurrency(spec.concurrencyGroup, u.ID, spec.projectID)
	if !ok {
		p.log().Warn(
			"stream proxy concurrency limit exceeded",
			"group", string(spec.concurrencyGroup),
			"path", spec.upstreamPath,
			"user_id", u.ID,
			"project_id", spec.projectID,
		)
		return c.Status(fiber.StatusTooManyRequests).JSON(fiber.Map{
			"code":  "live_tail_limit_exceeded",
			"error": "too many concurrent live tail streams",
		})
	}

	query, err := url.ParseQuery(string(c.RequestCtx().QueryArgs().QueryString()))
	if err != nil {
		releaseSlot()
		return fiber.NewError(fiber.StatusBadRequest, "invalid query parameters")
	}
	applyProxyQueryRules(query, spec.allowedParams, spec.addQuery)
	target := buildProxyTarget(spec.base, spec.upstreamPath, query)

	maxDuration := spec.maxDuration
	if maxDuration <= 0 {
		maxDuration = p.liveTailMaxDuration
	}
	if maxDuration <= 0 {
		maxDuration = defaultLiveTailMaxDuration
	}
	heartbeatInterval := spec.heartbeat
	if heartbeatInterval <= 0 {
		heartbeatInterval = p.liveTailHeartbeat
	}

	parentCtx := c.Context()
	streamCtx, streamCancel := context.WithTimeout(shutdownCtx, maxDuration)
	body := ownedProxyRequestBody(c, spec.body)
	req, err := http.NewRequestWithContext(streamCtx, c.Method(), target, requestBodyReader(body))
	if err != nil {
		streamCancel()
		releaseSlot()
		return fiber.NewError(fiber.StatusInternalServerError, "failed to construct upstream request")
	}
	reqAllowlist := spec.requestHeaders
	if reqAllowlist == nil {
		reqAllowlist = defaultStreamRequestHeaders
	}
	copyAllowlistedRequestHeaders(req.Header, &c.Request().Header, reqAllowlist)
	req.Header.Del("Accept-Encoding")
	if spec.projectID > 0 {
		if err := p.storage.ApplyTenantHeaders(req.Header, spec.projectID); err != nil {
			streamCancel()
			releaseSlot()
			return fiber.NewError(fiber.StatusInternalServerError, "project context is missing")
		}
	}

	client := p.tailClient
	if client == nil {
		client = p.client
	}
	resp, err := client.Do(req)
	if err != nil {
		streamCancel()
		releaseSlot()
		if shutdownCtx.Err() != nil {
			return writeGatewayDraining(c)
		}
		return p.writeUpstreamError(c, parentCtx, streamCtx, spec.upstreamPath, err)
	}

	respAllowlist := spec.responseHeaders
	if respAllowlist == nil {
		respAllowlist = defaultProxyResponseHeaders
	}
	copyAllowlistedResponseHeaders(c, resp.Header, respAllowlist)
	c.Set("X-Accel-Buffering", "no")
	c.Set("Cache-Control", "no-cache")
	c.Status(resp.StatusCode)

	if resp.StatusCode == http.StatusNoContent ||
		resp.StatusCode == http.StatusNotModified ||
		resp.ContentLength == 0 {
		drainAndCloseBody(resp.Body)
		streamCancel()
		releaseSlot()
		return nil
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxStreamErrorBodyBytes))
		drainAndCloseBody(resp.Body)
		streamCancel()
		releaseSlot()
		return c.Send(errBody)
	}

	c.Response().ImmediateHeaderFlush = true
	c.Response().Header.SetConnectionClose()

	// Capture only values that are safe outside the Fiber handler lifecycle;
	// SetBodyStreamWriter callbacks must never touch Ctx or RequestCtx.
	clientConn := c.RequestCtx().Conn()
	statusCode := resp.StatusCode
	projectID := spec.projectID
	userID := u.ID
	upstreamPath := spec.upstreamPath

	c.RequestCtx().SetBodyStreamWriter(func(w *bufio.Writer) {
		var closeBodyOnce sync.Once
		closeBody := func() {
			closeBodyOnce.Do(func() {
				_ = resp.Body.Close()
			})
		}
		defer releaseSlot()
		defer closeBody()
		defer streamCancel()

		stopWatcher := startClientDisconnectWatcher(streamCtx, clientConn, streamCancel)
		defer stopWatcher()

		type streamChunk struct {
			data []byte
			err  error
		}
		chunks := make(chan streamChunk, 1)
		readDone := make(chan struct{})
		go func() {
			defer close(readDone)
			defer close(chunks)
			buf := make([]byte, 32*1024)
			for {
				n, readErr := resp.Body.Read(buf)
				if n > 0 {
					chunk := make([]byte, n)
					copy(chunk, buf[:n])
					select {
					case chunks <- streamChunk{data: chunk}:
					case <-streamCtx.Done():
						return
					}
				}
				if readErr != nil {
					select {
					case chunks <- streamChunk{err: readErr}:
					case <-streamCtx.Done():
					}
					return
				}
			}
		}()
		defer func() {
			streamCancel()
			closeBody()
			<-readDone
		}()

		var ticker *time.Ticker
		var heartbeatCh <-chan time.Time
		if heartbeatInterval > 0 {
			ticker = time.NewTicker(heartbeatInterval)
			heartbeatCh = ticker.C
			defer ticker.Stop()
		}

		fw := flushingWriter{writer: w}
		for {
			select {
			case <-streamCtx.Done():
				return
			case <-heartbeatCh:
				if _, writeErr := fw.Write([]byte("\n")); writeErr != nil {
					p.recordStreamError(upstreamPath, statusCode, writeErr)
					return
				}
			case chunk, ok := <-chunks:
				if !ok {
					return
				}
				if len(chunk.data) > 0 {
					if _, writeErr := fw.Write(chunk.data); writeErr != nil {
						p.recordStreamError(upstreamPath, statusCode, writeErr)
						return
					}
					if ticker != nil {
						ticker.Reset(heartbeatInterval)
					}
				}
				if chunk.err != nil {
					if !errors.Is(chunk.err, io.EOF) && streamCtx.Err() == nil {
						p.log().Warn(
							"stream proxy upstream interrupted",
							"path", upstreamPath,
							"project_id", projectID,
							"user_id", userID,
							"status", statusCode,
							"error", chunk.err,
						)
						p.recordStreamError(upstreamPath, statusCode, chunk.err)
					}
					return
				}
			}
		}
	})
	return nil
}

func (p *storageProxy) acquireConcurrency(group proxyConcurrencyGroup, userID, projectID int) (func(), bool) {
	switch group {
	case proxyConcurrencyNone:
		return func() {}, true
	case proxyConcurrencyLiveTail:
		if p.liveTail == nil {
			return func() {}, true
		}
		return p.liveTail.acquire(userID, projectID)
	default:
		return func() {}, true
	}
}

func writeGatewayDraining(c fiber.Ctx) error {
	return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
		"code":  "gateway_draining",
		"error": "gateway is shutting down",
	})
}

func (p *storageProxy) writeUpstreamError(
	c fiber.Ctx,
	parentCtx context.Context,
	reqCtx context.Context,
	upstreamPath string,
	err error,
) error {
	status, code, msg := classifyProxyError(parentCtx, reqCtx, err)
	p.log().Warn(
		"gateway upstream proxy failed",
		"path", upstreamPath,
		"status", status,
		"code", code,
		"error", err,
	)
	return c.Status(status).JSON(fiber.Map{
		"code":  code,
		"error": msg,
	})
}

func classifyProxyError(parentCtx, reqCtx context.Context, err error) (int, string, string) {
	if isClientCanceled(parentCtx, reqCtx, err) {
		return statusClientClosedRequest, "client_closed_request", "client closed request"
	}
	if isProxyTimeout(reqCtx, err) {
		return fiber.StatusGatewayTimeout, "upstream_timeout", "upstream storage timed out"
	}
	return fiber.StatusBadGateway, "upstream_unavailable", "upstream storage unavailable"
}

func isClientCanceled(parentCtx, reqCtx context.Context, err error) bool {
	if parentCtx != nil && errors.Is(parentCtx.Err(), context.Canceled) {
		return true
	}
	if reqCtx != nil && errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
		return false
	}
	return errors.Is(err, context.Canceled)
}

func isProxyTimeout(reqCtx context.Context, err error) bool {
	if reqCtx != nil && errors.Is(reqCtx.Err(), context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func ownedProxyRequestBody(c fiber.Ctx, override []byte) []byte {
	if override != nil {
		return override
	}
	raw := requestBody(c)
	if len(raw) == 0 {
		return nil
	}
	// When Immutable is enabled (production gateway) or boundedRequestDecoding
	// already produced an owned slice, requestBody returns an owned buffer;
	// avoid a redundant full-body copy.
	if c.App().Config().Immutable {
		return raw
	}
	if _, owned := c.Locals(decodedBodyLocal).([]byte); owned {
		return raw
	}
	return append([]byte(nil), raw...)
}

func requestBodyReader(body []byte) io.Reader {
	if len(body) == 0 {
		return http.NoBody
	}
	return bytes.NewReader(body)
}

func applyProxyQueryRules(query url.Values, allowedParams map[string]bool, addQuery url.Values) {
	if allowedParams != nil {
		for key := range query {
			if !allowedParams[key] {
				query.Del(key)
			}
		}
	}
	for key, values := range addQuery {
		query.Del(key)
		for _, value := range values {
			query.Add(key, value)
		}
	}
}

func buildProxyTarget(base, upstreamPath string, query url.Values) string {
	target := strings.TrimRight(base, "/") + upstreamPath
	if encoded := query.Encode(); encoded != "" {
		target += "?" + encoded
	}
	return target
}

func (p *storageProxy) log() *slog.Logger {
	if p.logger != nil {
		return p.logger
	}
	return slog.Default()
}

func (p *storageProxy) recordStreamError(targetPath string, statusCode int, err error) {
	p.streamErrors.Add(1)
	if p.streamErrCounter != nil {
		p.streamErrCounter.Add(context.Background(), 1)
	}
	p.log().Warn("gateway proxy stream interrupted", "path", targetPath, "status", statusCode, "error", err)
}

func drainAndCloseBody(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, proxyDrainBodyLimitBytes))
	_ = body.Close()
}

func noFollowRedirects(*http.Request, []*http.Request) error {
	return http.ErrUseLastResponse
}

func (p *storageProxy) applyMetricsIsolation(
	values url.Values,
	project string,
	defaultMatcher bool,
) error {
	return p.storage.ApplyMetricsQueryIsolation(values, project, defaultMatcher)
}

func (g *Gateway) requestProxyTimeout() time.Duration {
	if g.proxy != nil {
		return g.proxy.proxyTimeout
	}
	return 0
}

func (g *Gateway) metricsQuery(c fiber.Ctx) error {
	if g.proxy != nil {
		return g.proxy.metricsQuery(c)
	}
	return (&storageProxy{storage: g.storage, client: g.client, logger: g.logger}).metricsQuery(c)
}

func (g *Gateway) applyMetricsIsolation(
	values url.Values,
	project string,
	defaultMatcher bool,
) error {
	if g.proxy != nil {
		return g.proxy.applyMetricsIsolation(values, project, defaultMatcher)
	}
	return g.storage.ApplyMetricsQueryIsolation(values, project, defaultMatcher)
}

func normalizedMediaType(contentType string) string {
	trimmed := strings.TrimSpace(contentType)
	if trimmed == "" {
		return ""
	}
	mediaType, _, err := mime.ParseMediaType(trimmed)
	if err != nil {
		base, _, _ := strings.Cut(trimmed, ";")
		return strings.ToLower(strings.TrimSpace(base))
	}
	return mediaType
}

func copyAllowlistedRequestHeaders(dst http.Header, src *fasthttp.RequestHeader, allowlist map[string]bool) {
	for key, value := range src.All() {
		name := string(key)
		if allowlist[strings.ToLower(name)] {
			dst.Add(name, string(value))
		}
	}
}

func copyAllowlistedResponseHeaders(c fiber.Ctx, src http.Header, allowlist map[string]bool) {
	for key, values := range src {
		if !allowlist[strings.ToLower(key)] {
			continue
		}
		// Fiber installs a default text/plain Content-Type before a streaming
		// response is proxied. Replace each upstream header field instead of
		// comma-appending it to that default; Content-Type is a singleton field,
		// and a combined value is not a valid media type.
		c.Response().Header.Del(key)
		for _, value := range values {
			c.Response().Header.Add(key, value)
		}
	}
}
