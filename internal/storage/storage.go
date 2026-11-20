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
	TracesBase() string
	PrometheusRemoteWriteBase() string
	SetTenantHeaders(http.Header, int)
}

type Backend struct {
	metricsKind      MetricsKind
	metricsQueryURL  string
	metricsIngestURL string
	metricsCluster   bool
	tenantAccountID  string
	logsURL          string
	tracesURL        string
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
	if kind != MetricsVM && kind != MetricsPrometheus {
		return nil, fmt.Errorf("unsupported metrics kind %q", kind)
	}
	if opts.Cluster {
		if kind != MetricsVM {
			return nil, errors.New("metrics cluster mode requires kind vm")
		}
		if _, err := strconv.ParseUint(opts.TenantAccountID, 10, 32); err != nil {
			return nil, fmt.Errorf("metrics tenant account ID must be an unsigned 32-bit integer: %w", err)
		}
		if ingestURL == "" {
			return nil, errors.New("metrics ingest URL is required in cluster mode")
		}
	}
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	for name, raw := range map[string]string{"metrics query": queryURL, "logs": logsURL, "traces": tracesURL} {
		if err := validateURL(raw); err != nil {
			return nil, fmt.Errorf("%s URL: %w", name, err)
		}
	}
	if ingestURL != "" {
		if err := validateURL(ingestURL); err != nil {
			return nil, fmt.Errorf("metrics ingest URL: %w", err)
		}
	}
	return &Backend{
		metricsKind:      kind,
		metricsQueryURL:  strings.TrimRight(queryURL, "/"),
		metricsIngestURL: strings.TrimRight(ingestURL, "/"),
		metricsCluster:   opts.Cluster,
		tenantAccountID:  opts.TenantAccountID,
		logsURL:          strings.TrimRight(logsURL, "/"),
		tracesURL:        strings.TrimRight(tracesURL, "/"),
		client:           client,
	}, nil
}

func (b *Backend) MetricsMode() MetricsKind { return b.metricsKind }

func (b *Backend) MetricsIngestEnabled() bool { return b.metricsIngestURL != "" }

func (b *Backend) MetricsClustered() bool { return b.metricsCluster }

func (b *Backend) LogsBase() string { return b.logsURL }

func (b *Backend) TracesBase() string { return b.tracesURL }

// SetTenantHeaders applies the native VictoriaLogs/VictoriaTraces tenant
// boundary. The gateway strips incoming values before calling this method.
// Non-positive project IDs fail closed instead of silently landing in tenant 0.
func (b *Backend) SetTenantHeaders(header http.Header, projectID int) {
	if projectID <= 0 {
		panic(fmt.Sprintf("storage: invalid project ID %d for tenant headers", projectID))
	}
	header.Set("AccountID", "0")
	header.Set("ProjectID", strconv.Itoa(projectID))
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
		"logs":    b.probe(ctx, b.logsURL, "/health"),
		"traces":  b.probe(ctx, b.tracesURL, "/health"),
	}
	if b.metricsIngestURL != "" && (b.metricsCluster || b.metricsIngestURL != b.metricsQueryURL) {
		// A standalone deployment may split Prometheus-compatible reads from a
		// VictoriaMetrics write endpoint. Probe both common health routes so a
		// dead write path cannot hide behind a healthy query service.
		result["metrics-ingest"] = b.probe(ctx, b.metricsIngestURL, "/-/healthy", "/health")
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
