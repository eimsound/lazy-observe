// Package storage centralizes signal endpoints and project-isolation behavior.
package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql/parser"
)

type MetricsKind string

const (
	MetricsVM         MetricsKind = "vm"
	MetricsPrometheus MetricsKind = "prometheus"
)

type MetricsOptions struct {
	Cluster         bool
	TenantAccountID string
}

// SignalEndpoint configures an external VictoriaLogs or VictoriaTraces service.
// When QueryURL or IngestURL is unset, that side falls back to URL.
type SignalEndpoint struct {
	URL       string
	QueryURL  string
	IngestURL string
}

// BackendOptions groups the endpoints and tenant settings for all three signals.
type BackendOptions struct {
	MetricsKind      MetricsKind
	MetricsQueryURL  string
	MetricsIngestURL string
	Metrics          MetricsOptions
	Logs             SignalEndpoint
	Traces           SignalEndpoint
}

// ErrInvalidTenant reports a missing or invalid project tenant boundary.
var ErrInvalidTenant = errors.New("storage: invalid tenant")

// SignalBackend is the storage boundary consumed by the gateway. Both the
// embedded and standalone process topologies use the same HTTP contract; only
// the configured endpoints differ. Keeping URL and tenancy decisions behind
// this interface prevents handlers from accidentally sending writes to a
// query-only endpoint when standalone uses separate services.
type SignalBackend interface {
	Health(context.Context) map[string]error
	MetricsMode() MetricsKind
	MetricsQueryBase() string
	MetricsIngestBase() string
	MetricsIngestEnabled() bool
	MetricsClustered() bool
	LogsBase() string
	LogsQueryBase() string
	LogsIngestBase() string
	TracesBase() string
	TracesQueryBase() string
	TracesIngestBase() string
	PrometheusRemoteWriteBase() string
	TenantAccountID() string
	ApplyTenantHeaders(http.Header, int) error
	SetTenantHeaders(http.Header, int)
	ApplyMetricsQueryIsolation(url.Values, string, bool) error
	MetricsIngestExtraLabel(string) (string, error)
}

type Backend struct {
	metricsKind      MetricsKind
	metricsQueryURL  string
	metricsIngestURL string
	metricsCluster   bool
	tenantAccountID  string
	logsQueryURL     string
	logsIngestURL    string
	tracesQueryURL   string
	tracesIngestURL  string
	client           *http.Client
}

var _ SignalBackend = (*Backend)(nil)

func New(kind MetricsKind, queryURL, ingestURL, logsURL, tracesURL string, client *http.Client) (*Backend, error) {
	return NewWithMetricsOptions(kind, queryURL, ingestURL, logsURL, tracesURL, MetricsOptions{}, client)
}

func NewWithMetricsOptions(
	kind MetricsKind,
	queryURL, ingestURL, logsURL, tracesURL string,
	opts MetricsOptions,
	client *http.Client,
) (*Backend, error) {
	return NewWithOptions(
		BackendOptions{
			MetricsKind:      kind,
			MetricsQueryURL:  queryURL,
			MetricsIngestURL: ingestURL,
			Metrics:          opts,
			Logs:             SignalEndpoint{URL: logsURL},
			Traces:           SignalEndpoint{URL: tracesURL},
		},
		client,
	)
}

func NewWithOptions(opts BackendOptions, client *http.Client) (*Backend, error) {
	if opts.MetricsKind != MetricsVM && opts.MetricsKind != MetricsPrometheus {
		return nil, fmt.Errorf("unsupported metrics kind %q", opts.MetricsKind)
	}
	if opts.Metrics.TenantAccountID != "" {
		if _, err := strconv.ParseUint(opts.Metrics.TenantAccountID, 10, 32); err != nil {
			return nil, fmt.Errorf("metrics tenant account ID must be an unsigned 32-bit integer: %w", err)
		}
	}
	if opts.Metrics.Cluster {
		if opts.MetricsKind != MetricsVM {
			return nil, errors.New("metrics cluster mode requires kind vm")
		}
		if opts.Metrics.TenantAccountID == "" {
			return nil, errors.New("metrics tenant account ID is required in cluster mode")
		}
		if opts.MetricsIngestURL == "" {
			return nil, errors.New("metrics ingest URL is required in cluster mode")
		}
	}
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	if err := validateURL(opts.MetricsQueryURL); err != nil {
		return nil, fmt.Errorf("metrics query URL: %w", err)
	}
	if opts.MetricsIngestURL != "" {
		if err := validateURL(opts.MetricsIngestURL); err != nil {
			return nil, fmt.Errorf("metrics ingest URL: %w", err)
		}
	}
	logsQueryURL, logsIngestURL, err := resolveSignalEndpoint("logs", opts.Logs)
	if err != nil {
		return nil, err
	}
	tracesQueryURL, tracesIngestURL, err := resolveSignalEndpoint("traces", opts.Traces)
	if err != nil {
		return nil, err
	}
	return &Backend{
		metricsKind:      opts.MetricsKind,
		metricsQueryURL:  strings.TrimRight(opts.MetricsQueryURL, "/"),
		metricsIngestURL: strings.TrimRight(opts.MetricsIngestURL, "/"),
		metricsCluster:   opts.Metrics.Cluster,
		tenantAccountID:  opts.Metrics.TenantAccountID,
		logsQueryURL:     logsQueryURL,
		logsIngestURL:    logsIngestURL,
		tracesQueryURL:   tracesQueryURL,
		tracesIngestURL:  tracesIngestURL,
		client:           client,
	}, nil
}

func resolveSignalEndpoint(signal string, endpoint SignalEndpoint) (string, string, error) {
	queryURL := strings.TrimSpace(endpoint.QueryURL)
	if queryURL == "" {
		queryURL = strings.TrimSpace(endpoint.URL)
	}
	ingestURL := strings.TrimSpace(endpoint.IngestURL)
	if ingestURL == "" {
		ingestURL = strings.TrimSpace(endpoint.URL)
	}
	if err := validateURL(queryURL); err != nil {
		return "", "", fmt.Errorf("%s query URL: %w", signal, err)
	}
	if err := validateURL(ingestURL); err != nil {
		return "", "", fmt.Errorf("%s ingest URL: %w", signal, err)
	}
	return strings.TrimRight(queryURL, "/"), strings.TrimRight(ingestURL, "/"), nil
}

func (b *Backend) MetricsMode() MetricsKind { return b.metricsKind }

func (b *Backend) MetricsIngestEnabled() bool { return b.metricsIngestURL != "" }

func (b *Backend) MetricsClustered() bool { return b.metricsCluster }

func (b *Backend) LogsBase() string { return b.logsQueryURL }

func (b *Backend) LogsQueryBase() string { return b.logsQueryURL }

func (b *Backend) LogsIngestBase() string { return b.logsIngestURL }

func (b *Backend) TracesBase() string { return b.tracesQueryURL }

func (b *Backend) TracesQueryBase() string { return b.tracesQueryURL }

func (b *Backend) TracesIngestBase() string { return b.tracesIngestURL }

// TenantAccountID returns the effective Victoria account identifier across
// metrics cluster paths and VictoriaLogs/VictoriaTraces headers.
func (b *Backend) TenantAccountID() string {
	if b.tenantAccountID == "" {
		return "0"
	}
	return b.tenantAccountID
}

// ApplyTenantHeaders validates projectID fail-closed and writes the native
// VictoriaLogs/VictoriaTraces AccountID and ProjectID headers.
func (b *Backend) ApplyTenantHeaders(header http.Header, projectID int) error {
	if projectID <= 0 {
		return fmt.Errorf("%w: invalid project ID %d for tenant headers", ErrInvalidTenant, projectID)
	}
	header.Set("AccountID", b.TenantAccountID())
	header.Set("ProjectID", strconv.Itoa(projectID))
	return nil
}

// SetTenantHeaders applies the native VictoriaLogs/VictoriaTraces tenant
// boundary. The gateway strips incoming values before calling this method.
// Non-positive project IDs fail closed instead of silently landing in tenant 0.
func (b *Backend) SetTenantHeaders(header http.Header, projectID int) {
	if err := b.ApplyTenantHeaders(header, projectID); err != nil {
		panic(fmt.Sprintf("storage: invalid project ID %d for tenant headers", projectID))
	}
}

// MetricsIngestExtraLabel validates the project slug fail-closed and returns
// the trusted extra_label parameter value for OTLP metrics ingestion.
func (b *Backend) MetricsIngestExtraLabel(projectSlug string) (string, error) {
	if projectSlug == "" {
		return "", fmt.Errorf("%w: project slug is empty", ErrInvalidTenant)
	}
	return "project_id=" + projectSlug, nil
}

// ApplyMetricsQueryIsolation strips client-supplied isolation parameters and
// enforces project scoping using either VictoriaMetrics extra_filters[] or
// Prometheus PromQL AST injection.
func (b *Backend) ApplyMetricsQueryIsolation(values url.Values, projectSlug string, defaultMatcher bool) error {
	// VictoriaMetrics v1.153+ ignores empty extra_label/extra_filters values
	// instead of rejecting them, so an empty project would silently become a
	// cross-project query. Refuse it here.
	if projectSlug == "" {
		return fmt.Errorf("%w: project slug is empty", ErrInvalidTenant)
	}
	StripReservedMetricsParams(values)
	if b.metricsKind == MetricsPrometheus {
		if err := InjectPromValues(values, projectSlug); err != nil {
			return err
		}
		if defaultMatcher && len(values["match[]"]) == 0 {
			values.Set("match[]", fmt.Sprintf(`{project_id=%q}`, projectSlug))
		}
		return nil
	}
	values.Set("extra_filters[]", fmt.Sprintf(`{project_id=%q}`, projectSlug))
	return nil
}

// StripReservedMetricsParams removes client-supplied isolation parameters so
// callers cannot inject arbitrary VictoriaMetrics filters or labels.
func StripReservedMetricsParams(values url.Values) {
	values.Del("extra_filters")
	values.Del("extra_filters[]")
	values.Del("extra_label")
}

// InjectPromValues rewrites every query and match[] expression in values to
// enforce the trusted project_id equality matcher.
func InjectPromValues(values url.Values, projectSlug string) error {
	if projectSlug == "" {
		return fmt.Errorf("%w: project slug is empty", ErrInvalidTenant)
	}
	for _, key := range []string{"query", "match[]"} {
		original := values[key]
		if len(original) == 0 {
			continue
		}
		injected := make([]string, len(original))
		for i, value := range original {
			var err error
			injected[i], err = InjectPromQL(value, projectSlug)
			if err != nil {
				return err
			}
		}
		values[key] = injected
	}
	return nil
}

// MetricsQueryBase returns the Prometheus API base. Cluster mode uses one
// platform-level VM account; project isolation remains enforced by labels.
func (b *Backend) MetricsQueryBase() string {
	if !b.metricsCluster {
		return b.metricsQueryURL
	}
	return b.metricsQueryURL + "/select/" + b.tenantAccountID + "/prometheus"
}

// MetricsIngestBase returns the tenant-scoped vminsert base. Callers append a
// protocol suffix such as /opentelemetry/v1/metrics.
func (b *Backend) MetricsIngestBase() string {
	if !b.metricsCluster {
		return b.metricsIngestURL
	}
	return b.metricsIngestURL + "/insert/" + b.tenantAccountID
}

// PrometheusRemoteWriteBase returns the base URL consumed by vmagent. Cluster
// mode needs VictoriaMetrics' tenant-scoped Prometheus compatibility prefix.
func (b *Backend) PrometheusRemoteWriteBase() string {
	if b.metricsCluster && b.metricsIngestURL != "" {
		return b.MetricsIngestBase() + "/prometheus"
	}
	return b.metricsIngestURL
}

func validateURL(raw string) error {
	if raw == "" {
		return errors.New("is required")
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("must be an absolute http(s) URL")
	}
	return nil
}

func (b *Backend) Health(ctx context.Context) map[string]error {
	metricsPaths := []string{"/health"}
	if b.metricsKind == MetricsPrometheus {
		metricsPaths = []string{"/-/healthy", "/health"}
	}
	result := map[string]error{
		"metrics": b.probe(ctx, b.metricsQueryURL, metricsPaths...),
		"logs":    b.probe(ctx, b.logsQueryURL, "/health"),
		"traces":  b.probe(ctx, b.tracesQueryURL, "/health"),
	}
	if b.metricsIngestURL != "" && (b.metricsCluster || b.metricsIngestURL != b.metricsQueryURL) {
		// A standalone deployment may split Prometheus-compatible reads from a
		// VictoriaMetrics write endpoint. Probe both common health routes so a
		// dead write path cannot hide behind a healthy query service.
		result["metrics-ingest"] = b.probe(ctx, b.metricsIngestURL, "/-/healthy", "/health")
	}
	if b.logsIngestURL != "" && b.logsIngestURL != b.logsQueryURL {
		result["logs-ingest"] = b.probe(ctx, b.logsIngestURL, "/health")
	}
	if b.tracesIngestURL != "" && b.tracesIngestURL != b.tracesQueryURL {
		result["traces-ingest"] = b.probe(ctx, b.tracesIngestURL, "/health")
	}
	return result
}

func (b *Backend) probe(ctx context.Context, base string, paths ...string) error {
	var lastErr error
	for _, path := range paths {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return err
		}
		resp, err := b.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("%s status=%d", path, resp.StatusCode)
	}
	return lastErr
}

// InjectPromQL adds an equality matcher to every vector selector. Existing
// project_id matchers remain in place, so hostile regex/equality matchers are ANDed.
func InjectPromQL(query, project string) (string, error) {
	// Prometheus rejects selectors whose matchers all accept an empty value
	// (for example {project_id=~".*"}) before returning an AST. Seed every
	// braced selector with the trusted matcher so these hostile-but-parseable
	// inputs become constrained before validation; the AST pass handles bare
	// selectors and verifies complete coverage.
	query = seedBracedSelectors(query, project)
	expr, err := parser.NewParser(parser.Options{}).ParseExpr(query)
	if err != nil {
		return "", err
	}
	var inspectErr error
	parser.Inspect(expr, func(node parser.Node, _ []parser.Node) error {
		if inspectErr != nil {
			return inspectErr
		}
		selector, ok := node.(*parser.VectorSelector)
		if !ok {
			return nil
		}
		for _, existing := range selector.LabelMatchers {
			if existing.Name == "project_id" && existing.Type == labels.MatchEqual && existing.Value == project {
				return nil
			}
		}
		matcher, matcherErr := labels.NewMatcher(labels.MatchEqual, "project_id", project)
		if matcherErr != nil {
			inspectErr = matcherErr
			return matcherErr
		}
		selector.LabelMatchers = append(selector.LabelMatchers, matcher)
		return nil
	})
	if inspectErr != nil {
		return "", inspectErr
	}
	return expr.String(), nil
}

func seedBracedSelectors(query, project string) string {
	trusted := `project_id=` + strconv.Quote(project)
	var out strings.Builder
	out.Grow(len(query) + strings.Count(query, "{")*(len(trusted)+1))
	var quote byte
	escaped, comment := false, false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if comment {
			out.WriteByte(ch)
			if ch == '\n' {
				comment = false
			}
			continue
		}
		if quote != 0 {
			out.WriteByte(ch)
			if quote != '`' && ch == '\\' && !escaped {
				escaped = true
				continue
			}
			if ch == quote && !escaped {
				quote = 0
			}
			escaped = false
			continue
		}
		if ch == '#' {
			comment = true
			out.WriteByte(ch)
			continue
		}
		if ch == '"' || ch == '\'' || ch == '`' {
			quote = ch
			out.WriteByte(ch)
			continue
		}
		out.WriteByte(ch)
		if ch != '{' {
			continue
		}
		j := skipPromQLSpace(query, i+1)
		// Preserve Prometheus v3 UTF-8 quoted metric names at the first position
		// inside `{...}` (for example `{"http.server.request.duration_count"}`),
		// inserting `project_id=...` right after the quoted metric name.
		if endMetric, isQuotedMetric := scanLeadingQuotedMetricName(query, j); isQuotedMetric {
			out.WriteString(query[i+1 : endMetric])
			next := skipPromQLSpace(query, endMetric)
			if next < len(query) && query[next] == ',' {
				out.WriteByte(',')
				out.WriteString(trusted)
				out.WriteByte(',')
				i = next
			} else {
				out.WriteByte(',')
				out.WriteString(trusted)
				i = endMetric - 1
			}
			continue
		}
		out.WriteString(trusted)
		if j >= len(query) || query[j] != '}' {
			out.WriteByte(',')
		}
	}
	return out.String()
}

func skipPromQLSpace(query string, pos int) int {
	for pos < len(query) && (query[pos] == ' ' || query[pos] == '\t' || query[pos] == '\n' || query[pos] == '\r') {
		pos++
	}
	return pos
}

// scanLeadingQuotedMetricName checks whether query[start:] begins with a
// double-quoted UTF-8 metric name (not a quoted label matcher like `"a.b"="v"`).
func scanLeadingQuotedMetricName(query string, start int) (int, bool) {
	if start >= len(query) || query[start] != '"' {
		return 0, false
	}
	escaped := false
	end := -1
	for k := start + 1; k < len(query); k++ {
		if escaped {
			escaped = false
			continue
		}
		if query[k] == '\\' {
			escaped = true
			continue
		}
		if query[k] == '"' {
			end = k + 1
			break
		}
	}
	if end == -1 {
		return 0, false
	}
	after := skipPromQLSpace(query, end)
	if after < len(query) && (query[after] == '}' || query[after] == ',') {
		return end, true
	}
	return 0, false
}
