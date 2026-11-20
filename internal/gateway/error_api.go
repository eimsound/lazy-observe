package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/eimsound/lazy-observe/internal/errortrack"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

const (
	maxErrorEventSize = 200 << 10
	// A 200 KiB error event stored in VictoriaLogs contains both raw_event and
	// symbolicated_event as escaped JSON strings plus metadata, which can exceed
	// 1 MiB after double-escaping and symbolication expansion.
	maxVLogsErrorLineBytes = 4 << 20

	maxVitalsBatchSize             = 100
	maxVitalPageBytes              = 256
	maxVitalsDistinctPagesPerBatch = 32
)

var (
	minMetricTimestamp = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	maxMetricTimestamp = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
)

type errorEnvelope struct {
	Events []json.RawMessage `json:"events"`
}

func (g *Gateway) errorsIngest(c fiber.Ctx) error {
	if g.errorTracker == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "issue storage unavailable"})
	}
	var envelope errorEnvelope
	if err := json.Unmarshal(requestBody(c), &envelope); err != nil || envelope.Events == nil {
		return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "invalid error event batch"})
	}
	if len(envelope.Events) > 100 {
		return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "batch exceeds 100 events"})
	}
	events := make([]errortrack.Event, len(envelope.Events))
	for i, raw := range envelope.Events {
		if len(raw) > maxErrorEventSize {
			return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "event exceeds 200KB"})
		}
		if err := json.Unmarshal(raw, &events[i]); err != nil || !validErrorEvent(events[i]) {
			return c.Status(http.StatusUnprocessableEntity).
				JSON(fiber.Map{"error": fmt.Sprintf("invalid event at index %d", i)})
		}
	}
	project := currentProject(c)
	accepted := make([]errortrack.AcceptedEvent, len(events))
	for i, event := range events {
		accepted[i] = errortrack.AcceptedEvent{Raw: envelope.Events[i], Event: event}
	}
	if err := g.errorTracker.Accept(
		c.Context(),
		errortrack.Project{ID: project.ID, Slug: project.Slug},
		accepted,
	); err != nil {
		switch {
		case errors.Is(err, errortrack.ErrInvalid):
			return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "invalid error event batch"})
		case errors.Is(err, errortrack.ErrConflict):
			return c.Status(http.StatusConflict).JSON(fiber.Map{"error": "event ID conflicts with accepted content"})
		case errors.Is(err, errortrack.ErrCapacity):
			c.Set("Retry-After", "1")
			return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "error Inbox capacity exhausted"})
		case errors.Is(err, errortrack.ErrUnavailable):
			return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "error tracking unavailable"})
		default:
			return fiber.NewError(http.StatusInternalServerError, "accept error events: "+err.Error())
		}
	}
	return c.JSON(fiber.Map{"accepted": len(events)})
}

func validErrorEvent(e errortrack.Event) bool {
	if strings.TrimSpace(e.EventID) == "" || e.Timestamp.IsZero() || strings.TrimSpace(e.Platform) == "" ||
		strings.TrimSpace(e.Level) == "" {
		return false
	}
	if strings.TrimSpace(e.Exception.Type) == "" && strings.TrimSpace(e.Message) == "" {
		return false
	}
	for _, f := range e.Exception.Stacktrace.Frames {
		if strings.TrimSpace(f.Function) == "" && strings.TrimSpace(f.Filename) == "" {
			return false
		}
	}
	return true
}

type vitalInput struct {
	Name      string          `json:"name"`
	Value     float64         `json:"value"`
	Page      string          `json:"page"`
	Timestamp json.RawMessage `json:"timestamp"`
}

func (g *Gateway) vitalsIngest(c fiber.Ctx) error {
	if !g.storage.MetricsIngestEnabled() {
		return c.Status(fiber.StatusNotImplemented).
			JSON(fiber.Map{"error": "Web Vitals ingest is disabled: storage.metrics.ingest_url is not configured"})
	}
	var body struct {
		Vitals []vitalInput `json:"vitals"`
	}
	if err := json.Unmarshal(requestBody(c), &body); err != nil || body.Vitals == nil {
		return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "invalid vitals batch"})
	}
	if len(body.Vitals) > maxVitalsBatchSize {
		return c.Status(http.StatusBadRequest).
			JSON(fiber.Map{"error": fmt.Sprintf("vitals batch exceeds %d items", maxVitalsBatchSize)})
	}
	project := currentProject(c)
	var samples strings.Builder
	// Unknown metric names are a forward-compatibility signal (a newer SDK knows a
	// vital this server does not), not a malformed batch: skip and count them so the
	// known metrics in the same batch still land. Structural problems stay fatal for
	// the whole batch, so nothing is written when any entry is malformed.
	skippedNames := newUnknownEnumValues()
	skipped := 0
	distinctPages := make(map[string]struct{}, min(len(body.Vitals), maxVitalsDistinctPagesPerBatch))
	for i, vital := range body.Vitals {
		name := strings.ToLower(strings.TrimSpace(vital.Name))
		page, pageOK := normalizeVitalPage(vital.Page)
		if name == "" || !pageOK || math.IsNaN(vital.Value) || math.IsInf(vital.Value, 0) {
			return c.Status(http.StatusUnprocessableEntity).
				JSON(fiber.Map{"error": fmt.Sprintf("invalid vital at index %d", i)})
		}
		timestamp, err := parseMetricTimestamp(vital.Timestamp)
		if err != nil {
			return c.Status(http.StatusUnprocessableEntity).
				JSON(fiber.Map{"error": fmt.Sprintf("invalid vital timestamp at index %d", i)})
		}
		if _, seen := distinctPages[page]; !seen {
			if len(distinctPages) >= maxVitalsDistinctPagesPerBatch {
				return c.Status(http.StatusBadRequest).
					JSON(fiber.Map{
						"error": fmt.Sprintf("vitals batch exceeds %d distinct pages", maxVitalsDistinctPagesPerBatch),
					})
			}
			distinctPages[page] = struct{}{}
		}
		if !validVitalName(name) {
			skippedNames.add(name)
			skipped++
			continue
		}
		fmt.Fprintf(
			&samples,
			"web_vitals_%s{project_id=%s,page=%s} %s %d\n",
			name,
			formatPromLabelValue(project.Slug),
			formatPromLabelValue(page),
			strconv.FormatFloat(vital.Value, 'g', -1, 64),
			timestamp.UnixMilli(),
		)
	}
	if skipped > 0 {
		g.logger.Warn(
			"vitals ingest skipped unknown metric names",
			"project", project.Slug,
			"accepted", len(body.Vitals)-skipped,
			"rejected", skipped,
			"names", skippedNames.String(),
		)
	}
	if samples.Len() > 0 {
		if err := g.importPrometheusSamples(c.Context(), samples.String()); err != nil {
			return err
		}
	}
	return c.JSON(fiber.Map{"accepted": len(body.Vitals) - skipped, "rejected": skipped})
}

const (
	maxLoggedSkippedEnumValues = 5
	maxSkippedEnumValueBytes   = 64
)

// unknownEnumValues keeps a bounded, de-duplicated sample of rejected enum values
// (vital names, SDK diagnostic signals and reasons). Both private ingest endpoints are
// effectively public because the ingest token ships inside browser DSNs: without a
// length cap a single request inside the body limit could turn one warning into a
// multi-megabyte log line. Values are therefore truncated before de-duplication (so the
// cap cannot be bypassed by appending bytes) and only the first few distinct values are
// retained. Log volume still grows with request volume; rate limiting the warning is
// deliberately out of scope here.
type unknownEnumValues struct {
	values map[string]struct{}
	// overflow counts skipped entries whose truncated value never made it into values; it
	// is an entry count, not a distinct-value count, because values past the cap are no
	// longer retained and cannot be de-duplicated.
	overflow int
}

func newUnknownEnumValues() *unknownEnumValues {
	return &unknownEnumValues{values: make(map[string]struct{})}
}

func (u *unknownEnumValues) add(value string) {
	value = truncateEnumValue(value)
	if _, seen := u.values[value]; seen {
		return
	}
	if len(u.values) >= maxLoggedSkippedEnumValues {
		u.overflow++
		return
	}
	u.values[value] = struct{}{}
}

func (u *unknownEnumValues) String() string {
	ordered := make([]string, 0, len(u.values)+1)
	for value := range u.values {
		ordered = append(ordered, value)
	}
	sort.Strings(ordered)
	if u.overflow > 0 {
		ordered = append(ordered, fmt.Sprintf("+%d more", u.overflow))
	}
	return strings.Join(ordered, ",")
}

// truncateEnumValue cuts on a rune boundary; slicing mid-sequence would log a
// replacement character instead of the bytes the client sent.
func truncateEnumValue(value string) string {
	if len(value) <= maxSkippedEnumValueBytes {
		return value
	}
	cut := maxSkippedEnumValueBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "…"
}

type sdkDiagnosticInput struct {
	MeasurementID string          `json:"measurement_id"`
	Timestamp     json.RawMessage `json:"timestamp"`
	Service       string          `json:"service"`
	Signal        string          `json:"signal"`
	Reason        string          `json:"reason"`
	Dropped       int64           `json:"dropped"`
}

func (g *Gateway) sdkDiagnosticsIngest(c fiber.Ctx) error {
	if !g.storage.MetricsIngestEnabled() {
		return c.Status(fiber.StatusNotImplemented).
			JSON(fiber.Map{"error": "SDK diagnostics ingest is disabled: storage.metrics.ingest_url is not configured"})
	}
	var body struct {
		Measurements []sdkDiagnosticInput `json:"measurements"`
	}
	if err := json.Unmarshal(
		requestBody(c),
		&body,
	); err != nil || body.Measurements == nil ||
		len(body.Measurements) > 100 {
		return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "invalid SDK diagnostics batch"})
	}
	project := currentProject(c)
	var samples strings.Builder
	// Unknown signal/reason values are a forward-compatibility signal (a newer SDK knows
	// a diagnostic this server does not), not a malformed batch: skip and count them so
	// the known measurements in the same batch still land. Structural problems (missing
	// or malformed ids, timestamps, service names, counts, and empty signal/reason) stay
	// fatal for the whole batch, so nothing is written when any entry is malformed.
	skippedSignals := newUnknownEnumValues()
	skippedReasons := newUnknownEnumValues()
	skipped := 0
	for i, measurement := range body.Measurements {
		measurement.Service = strings.TrimSpace(measurement.Service)
		measurement.Signal = strings.TrimSpace(measurement.Signal)
		measurement.Reason = strings.TrimSpace(measurement.Reason)
		timestamp, err := parseMetricTimestamp(measurement.Timestamp)
		if err != nil || !validSDKMeasurementID(measurement.MeasurementID) ||
			len(measurement.Service) == 0 || len(measurement.Service) > 128 ||
			measurement.Signal == "" || measurement.Reason == "" ||
			measurement.Dropped <= 0 || measurement.Dropped > 1_000_000_000 {
			return c.Status(http.StatusUnprocessableEntity).
				JSON(fiber.Map{"error": fmt.Sprintf("invalid SDK diagnostic at index %d", i)})
		}
		unknown := false
		if !validSDKDiagnosticSignal(measurement.Signal) {
			skippedSignals.add(measurement.Signal)
			unknown = true
		}
		if !validSDKDiagnosticReason(measurement.Reason) {
			skippedReasons.add(measurement.Reason)
			unknown = true
		}
		if unknown {
			skipped++
			continue
		}
		// Browser diagnostics report per-batch drop deltas rather than a
		// monotonic cumulative OTel counter, so they use a dedicated _delta
		// metric name to avoid corrupting cumulative counter queries.
		fmt.Fprintf(
			&samples,
			"lazy_observe_sdk_dropped_telemetry_delta{project_id=%s,service_name=%s,"+
				"sdk_language=\"browser\",signal=%s,reason=%s} %d %d\n",
			formatPromLabelValue(project.Slug),
			formatPromLabelValue(measurement.Service),
			formatPromLabelValue(measurement.Signal),
			formatPromLabelValue(measurement.Reason),
			measurement.Dropped,
			timestamp.UnixMilli(),
		)
	}
	if skipped > 0 {
		g.logger.Warn(
			"SDK diagnostics ingest skipped unknown enum values",
			"project", project.Slug,
			"accepted", len(body.Measurements)-skipped,
			"rejected", skipped,
			"signals", skippedSignals.String(),
			"reasons", skippedReasons.String(),
		)
	}
	if samples.Len() > 0 {
		if err := g.importPrometheusSamples(c.Context(), samples.String()); err != nil {
			return err
		}
	}
	return c.JSON(fiber.Map{
		"accepted": len(body.Measurements) - skipped,
		"rejected": skipped,
	})
}

func validSDKMeasurementID(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validSDKDiagnosticSignal(value string) bool {
	switch value {
	case "error", "trace", "vitals", "replay", "diagnostic":
		return true
	}
	return false
}

func validSDKDiagnosticReason(value string) bool {
	switch value {
	// server_rejected is a 4xx refusal (the payload will never be accepted as-is),
	// while the other three come from the SDK's bounded delivery queues.
	case "queue_full", "retry_exhausted", "shutdown_budget", "server_rejected":
		return true
	}
	return false
}

func (g *Gateway) importPrometheusSamples(ctx context.Context, samples string) error {
	path := "/api/v1/import/prometheus"
	if g.storage.MetricsClustered() {
		path = "/prometheus" + path
	}
	target := strings.TrimRight(g.storage.MetricsIngestBase(), "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(samples))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := g.client.Do(req)
	if err != nil {
		return fiber.NewError(http.StatusBadGateway, err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fiber.NewError(http.StatusBadGateway, "metrics import failed")
	}
	return nil
}

func validVitalName(name string) bool {
	switch name {
	case "lcp", "cls", "inp", "ttfb", "fcp":
		return true
	}
	return false
}

// formatPromLabelValue formats a Prometheus text exposition label value using
// only the three escapes allowed by the Prometheus exposition spec (\, ", \n),
// preventing Go-only strconv.Quote escapes (\t, \r, \x.., \u....) from causing
// VictoriaMetrics 400 -> Gateway 502 rejections.
func formatPromLabelValue(value string) string {
	valid := strings.ToValidUTF8(value, "\ufffd")
	var b strings.Builder
	b.Grow(len(valid) + 2)
	b.WriteByte('"')
	for i := 0; i < len(valid); i++ {
		switch valid[i] {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			// Prometheus text format does not define \r escaping; strip CR so
			// it cannot split the line exposition record.
		default:
			b.WriteByte(valid[i])
		}
	}
	b.WriteByte('"')
	return b.String()
}

func normalizeVitalPage(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", false
	}
	if strings.Contains(trimmed, "://") {
		if parsed, err := url.Parse(trimmed); err == nil && parsed.Host != "" {
			trimmed = parsed.Path
			if trimmed == "" {
				trimmed = "/"
			}
		}
	}
	if cut := strings.IndexAny(trimmed, "?#"); cut >= 0 {
		trimmed = trimmed[:cut]
	}
	trimmed = strings.ToValidUTF8(trimmed, "")
	var cleaned strings.Builder
	cleaned.Grow(min(len(trimmed), maxVitalPageBytes))
	for i := 0; i < len(trimmed); i++ {
		ch := trimmed[i]
		if ch < 0x20 || ch == 0x7f {
			continue
		}
		cleaned.WriteByte(ch)
	}
	normalized := strings.TrimSpace(cleaned.String())
	if normalized == "" {
		return "", false
	}
	if !strings.HasPrefix(normalized, "/") {
		normalized = "/" + normalized
	}
	normalized = path.Clean(normalized)
	if normalized != "/" {
		segments := strings.Split(strings.TrimPrefix(normalized, "/"), "/")
		for i, seg := range segments {
			if isHighCardinalityVitalSegment(seg) {
				segments[i] = ":id"
			}
		}
		normalized = "/" + strings.Join(segments, "/")
	}
	if len(normalized) > maxVitalPageBytes {
		cut := maxVitalPageBytes
		for cut > 0 && !utf8.RuneStart(normalized[cut]) {
			cut--
		}
		normalized = normalized[:cut]
	}
	if normalized == "" {
		return "", false
	}
	return normalized, true
}

func isHighCardinalityVitalSegment(seg string) bool {
	if seg == "" {
		return false
	}
	// Purely numeric IDs (e.g. /orders/987654321).
	allDigits := true
	for i := 0; i < len(seg); i++ {
		if seg[i] < '0' || seg[i] > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		return true
	}
	// Standard UUID (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx).
	if len(seg) == 36 && seg[8] == '-' && seg[13] == '-' && seg[18] == '-' && seg[23] == '-' {
		isUUID := true
		for i := 0; i < len(seg); i++ {
			if i == 8 || i == 13 || i == 18 || i == 23 {
				continue
			}
			if !isHexByte(seg[i]) {
				isUUID = false
				break
			}
		}
		if isUUID {
			return true
		}
	}
	// Long hexadecimal tokens/hashes (>= 16 hex chars).
	if len(seg) >= 16 {
		allHex := true
		for i := 0; i < len(seg); i++ {
			if !isHexByte(seg[i]) {
				allHex = false
				break
			}
		}
		if allHex {
			return true
		}
	}
	return false
}

func isHexByte(ch byte) bool {
	return (ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f') || (ch >= 'A' && ch <= 'F')
}

func parseMetricTimestamp(raw json.RawMessage) (time.Time, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		parsed, err := time.Parse(time.RFC3339Nano, text)
		if err != nil {
			return time.Time{}, err
		}
		if parsed.Before(minMetricTimestamp) || parsed.After(maxMetricTimestamp) {
			return time.Time{}, errors.New("timestamp out of range")
		}
		return parsed.UTC(), nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return time.Time{}, err
	}
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return time.Time{}, errors.New("invalid numeric timestamp")
	}
	var parsed time.Time
	if value >= 1e12 {
		if value < float64(minMetricTimestamp.UnixMilli()) || value > float64(maxMetricTimestamp.UnixMilli()) {
			return time.Time{}, errors.New("millisecond timestamp out of range")
		}
		parsed = time.UnixMilli(int64(value)).UTC()
	} else {
		if value < float64(minMetricTimestamp.Unix()) || value > float64(maxMetricTimestamp.Unix()) {
			return time.Time{}, errors.New("second timestamp out of range")
		}
		sec, frac := math.Modf(value)
		parsed = time.Unix(int64(sec), int64(frac*float64(time.Second))).UTC()
	}
	if parsed.Before(minMetricTimestamp) || parsed.After(maxMetricTimestamp) {
		return time.Time{}, errors.New("timestamp out of range")
	}
	return parsed, nil
}

const issueTrendBuckets = 14

// The issue HTTP API deliberately uses wire DTOs instead of serializing
// metadata entities. Besides keeping project_id and ORM implementation details
// private, this fixes timestamps to epoch milliseconds for browser clients.
type issueSummaryDTO struct {
	ID          int     `json:"id"`
	Fingerprint string  `json:"fingerprint"`
	Title       string  `json:"title"`
	Level       string  `json:"level"`
	Status      string  `json:"status"`
	Count       int64   `json:"count"`
	UserCount   int64   `json:"user_count"`
	FirstSeen   int64   `json:"first_seen"`
	LastSeen    int64   `json:"last_seen"`
	Trend       []int64 `json:"trend"`
	RegressedAt *int64  `json:"regressed_at,omitempty"`
	LastEventID string  `json:"last_event_id,omitempty"`
}

type issueEventSummaryDTO struct {
	EventID   string `json:"event_id"`
	Timestamp int64  `json:"timestamp"`
	TraceID   string `json:"trace_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
}

type issueDetailDTO struct {
	issueSummaryDTO
	Culprit string                 `json:"culprit"`
	Events  []issueEventSummaryDTO `json:"events"`
}

type issueClientContextDTO struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type issueUserDTO struct {
	ID string `json:"id,omitempty"`
	IP string `json:"ip,omitempty"`
}

type issueContextsDTO struct {
	Trace   *errortrack.TraceContext `json:"trace,omitempty"`
	Browser *issueClientContextDTO   `json:"browser,omitempty"`
	OS      *issueClientContextDTO   `json:"os,omitempty"`
}

type issueExceptionDTO struct {
	Type          string                 `json:"type"`
	Value         string                 `json:"value"`
	Stacktrace    errortrack.Stacktrace  `json:"stacktrace"`
	RawStacktrace *errortrack.Stacktrace `json:"raw_stacktrace,omitempty"`
}

type issueEventDTO struct {
	EventID     string            `json:"event_id"`
	Timestamp   int64             `json:"timestamp"`
	Platform    string            `json:"platform,omitempty"`
	Release     string            `json:"release,omitempty"`
	Environment string            `json:"environment,omitempty"`
	Level       string            `json:"level"`
	Message     string            `json:"message,omitempty"`
	Exception   issueExceptionDTO `json:"exception"`
	Contexts    issueContextsDTO  `json:"contexts"`
	SessionID   string            `json:"session_id,omitempty"`
	Tags        map[string]string `json:"tags"`
	URL         string            `json:"url,omitempty"`
	User        *issueUserDTO     `json:"user,omitempty"`
}

type storedRawIssueEvent struct {
	Contexts  issueContextsDTO `json:"contexts"`
	User      *issueUserDTO    `json:"user"`
	Exception struct {
		Stacktrace errortrack.Stacktrace `json:"stacktrace"`
	} `json:"exception"`
}

func issueSummary(row meta.IssueView, trend []int64) issueSummaryDTO {
	var regressedAt *int64
	if row.RegressedAt != nil {
		value := row.RegressedAt.UnixMilli()
		regressedAt = &value
	}
	if trend == nil {
		trend = make([]int64, 0)
	}
	return issueSummaryDTO{
		ID: row.ID, Fingerprint: row.Fingerprint, Title: row.Title, Level: row.Level,
		Status: row.Status, Count: row.Count, UserCount: row.UserCount,
		FirstSeen: row.FirstSeen.UnixMilli(), LastSeen: row.LastSeen.UnixMilli(),
		Trend: trend, RegressedAt: regressedAt, LastEventID: row.LastEventID,
	}
}

func (g *Gateway) listIssues(c fiber.Ctx) error {
	if !g.ensureIssues(c) {
		return nil
	}
	page, _ := strconv.Atoi(c.Query("page", "1"))
	status, level, sortBy := c.Query("status"), c.Query("level"), c.Query("sort", "last_seen")
	if status != "" && status != "open" && status != "resolved" && status != "ignored" {
		return badRequest(c)
	}
	if sortBy != "last_seen" && sortBy != "count" {
		return badRequest(c)
	}
	project := currentProject(c)
	opts := meta.IssueListOptions{Status: status, Level: level, Query: c.Query("q"), Sort: sortBy, Page: page}
	issues, err := g.issues.ListIssues(c.Context(), project.ID, opts)
	if err != nil {
		return apiError(c, err)
	}
	total, err := g.issues.CountIssues(c.Context(), project.ID, opts)
	if err != nil {
		return apiError(c, err)
	}
	trends, err := g.queryIssueTrends(c.Context(), project.ID, issues)
	if err != nil {
		return fiber.NewError(http.StatusBadGateway, err.Error())
	}
	result := make([]issueSummaryDTO, 0, len(issues))
	for _, row := range issues {
		result = append(result, issueSummary(row, trends[row.Fingerprint]))
	}
	return c.JSON(fiber.Map{"issues": result, "total": total, "page": max(page, 1), "page_size": meta.IssuePageSize})
}

func (g *Gateway) getIssue(c fiber.Ctx) error {
	if !g.ensureIssues(c) {
		return nil
	}
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	row, err := g.issues.GetIssue(c.Context(), currentProject(c).ID, id)
	if err != nil {
		return apiError(c, err)
	}
	events, err := g.queryIssueEvents(c, row.Fingerprint, "", 20)
	if err != nil {
		return fiber.NewError(http.StatusBadGateway, err.Error())
	}
	trends, err := g.queryIssueTrends(c.Context(), currentProject(c).ID, []meta.IssueView{row})
	if err != nil {
		return fiber.NewError(http.StatusBadGateway, err.Error())
	}
	summaries := make([]issueEventSummaryDTO, 0, len(events))
	for _, event := range events {
		summary := issueEventSummaryDTO{EventID: event.EventID, Timestamp: event.Timestamp, SessionID: event.SessionID}
		if event.Contexts.Trace != nil {
			summary.TraceID = event.Contexts.Trace.TraceID
		}
		summaries = append(summaries, summary)
	}
	return c.JSON(issueDetailDTO{
		issueSummaryDTO: issueSummary(row, trends[row.Fingerprint]),
		Culprit:         issueCulprit(row, events),
		Events:          summaries,
	})
}

func (g *Gateway) patchIssue(c fiber.Ctx) error {
	if !g.ensureIssues(c) {
		return nil
	}
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	var body struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(requestBody(c), &body) != nil ||
		(body.Status != "open" && body.Status != "resolved" && body.Status != "ignored") {
		return badRequest(c)
	}
	row, err := g.issues.SetIssueStatus(c.Context(), currentProject(c).ID, id, body.Status, g.now().UTC())
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(issueSummary(row, nil))
}

func (g *Gateway) batchUpdateIssues(c fiber.Ctx) error {
	if !g.ensureIssues(c) {
		return nil
	}
	var body struct {
		IDs    []int  `json:"ids"`
		Status string `json:"status"`
	}
	if json.Unmarshal(requestBody(c), &body) != nil || len(body.IDs) == 0 || len(body.IDs) > 500 ||
		(body.Status != "open" && body.Status != "resolved" && body.Status != "ignored") {
		return badRequest(c)
	}
	for _, id := range body.IDs {
		if id <= 0 {
			return badRequest(c)
		}
	}
	updated, err := g.issues.SetIssuesStatus(
		c.Context(),
		currentProject(c).ID,
		body.IDs,
		body.Status,
		g.now().UTC(),
	)
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(fiber.Map{"updated": updated})
}

func (g *Gateway) getIssueEvent(c fiber.Ctx) error {
	if !g.ensureIssues(c) {
		return nil
	}
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	row, err := g.issues.GetIssue(c.Context(), currentProject(c).ID, id)
	if err != nil {
		return apiError(c, err)
	}
	eventID, err := unescapedPathParam(c, "event_id", validateIssueEventID)
	if err != nil {
		return err
	}
	events, err := g.queryIssueEvents(c, row.Fingerprint, eventID, 1)
	if err != nil {
		return fiber.NewError(http.StatusBadGateway, err.Error())
	}
	if len(events) == 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	return c.JSON(events[0])
}

func (g *Gateway) ensureIssues(c fiber.Ctx) bool {
	if g.issues == nil {
		_ = c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "issue storage unavailable"})
		return false
	}
	return true
}

func issueEventsLogsQL(fingerprint, eventID string, limit int) string {
	query := `_stream:{event_type="error"} AND fingerprint:=` + strconv.Quote(fingerprint)
	if eventID != "" {
		query += ` AND event_id:=` + strconv.Quote(eventID)
	}
	query += ` | uniq by (event_id, _time, raw_event, symbolicated_event) | sort by (_time) desc`
	if limit > 0 {
		query += ` | limit ` + strconv.Itoa(limit)
	}
	return query
}

func readVLogsErrorLines(r io.Reader, handle func([]byte)) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64<<10), maxVLogsErrorLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		handle(line)
	}
	return scanner.Err()
}

func (g *Gateway) queryIssueEvents(c fiber.Ctx, fingerprint, eventID string, limit int) ([]issueEventDTO, error) {
	query := issueEventsLogsQL(fingerprint, eventID, limit)
	target := strings.TrimRight(
		g.storage.LogsQueryBase(),
		"/",
	) + "/select/logsql/query?" + url.Values{
		"query": []string{query},
		"limit": []string{strconv.Itoa(limit)},
	}.Encode()
	req, err := http.NewRequestWithContext(c.Context(), http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	g.storage.SetTenantHeaders(req.Header, currentProject(c).ID)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("VLogs query status %d", resp.StatusCode)
	}
	result := make([]issueEventDTO, 0)
	seen := make(map[string]struct{})
	err = readVLogsErrorLines(resp.Body, func(line []byte) {
		var record map[string]json.RawMessage
		if json.Unmarshal(line, &record) != nil {
			return
		}
		event, ok := decodeIssueEvent(record)
		if !ok {
			return
		}
		if _, dup := seen[event.EventID]; dup {
			return
		}
		seen[event.EventID] = struct{}{}
		result = append(result, event)
	})
	return result, err
}

func decodeIssueEvent(record map[string]json.RawMessage) (issueEventDTO, bool) {
	var symbolicatedJSON string
	if json.Unmarshal(record["symbolicated_event"], &symbolicatedJSON) != nil || symbolicatedJSON == "" {
		if json.Unmarshal(record["raw_event"], &symbolicatedJSON) != nil || symbolicatedJSON == "" {
			return issueEventDTO{}, false
		}
	}
	var event errortrack.Event
	if json.Unmarshal([]byte(symbolicatedJSON), &event) != nil || event.EventID == "" || event.Timestamp.IsZero() {
		return issueEventDTO{}, false
	}

	var rawJSON string
	var raw storedRawIssueEvent
	if json.Unmarshal(record["raw_event"], &rawJSON) == nil && rawJSON != "" {
		_ = json.Unmarshal([]byte(rawJSON), &raw)
	}
	contexts := raw.Contexts
	if event.Contexts != nil && event.Contexts.Trace != nil {
		contexts.Trace = event.Contexts.Trace
	}
	tags := event.Tags
	if tags == nil {
		tags = make(map[string]string)
	}
	exception := issueExceptionDTO{
		Type:       event.Exception.Type,
		Value:      event.Exception.Value,
		Stacktrace: event.Exception.Stacktrace,
	}
	if !sameStacktrace(raw.Exception.Stacktrace, event.Exception.Stacktrace) &&
		len(raw.Exception.Stacktrace.Frames) > 0 {
		rawStack := raw.Exception.Stacktrace
		exception.RawStacktrace = &rawStack
	}
	return issueEventDTO{
		EventID:     event.EventID,
		Timestamp:   event.Timestamp.UnixMilli(),
		Platform:    event.Platform,
		Release:     event.Release,
		Environment: event.Environment,
		Level:       event.Level,
		Message:     event.Message,
		Exception:   exception,
		Contexts:    contexts,
		SessionID:   event.SessionID,
		Tags:        tags,
		URL:         event.URL,
		User:        raw.User,
	}, true
}

func sameStacktrace(a, b errortrack.Stacktrace) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

func issueCulprit(row meta.IssueView, events []issueEventDTO) string {
	for _, event := range events {
		frames := event.Exception.Stacktrace.Frames
		for i := len(frames) - 1; i >= 0; i-- {
			frame := frames[i]
			if !frame.InApp {
				continue
			}
			if frame.Function != "" && frame.Filename != "" {
				return frame.Function + "(" + frame.Filename + ")"
			}
			if frame.Function != "" {
				return frame.Function
			}
			if frame.Filename != "" {
				return frame.Filename
			}
		}
	}
	return row.Title
}

type issueTrendRangeResponse struct {
	Status string `json:"status"`
	Data   struct {
		Result []struct {
			Metric map[string]string   `json:"metric"`
			Values [][]json.RawMessage `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

func (g *Gateway) queryIssueTrends(
	ctx context.Context,
	projectID int,
	rows []meta.IssueView,
) (map[string][]int64, error) {
	result := make(map[string][]int64, len(rows))
	filters := make([]string, 0, len(rows))
	for _, row := range rows {
		result[row.Fingerprint] = make([]int64, issueTrendBuckets)
		filters = append(filters, strconv.Quote(row.Fingerprint))
	}
	if len(filters) == 0 {
		return result, nil
	}
	now := g.now().UTC()
	today := now.Truncate(24 * time.Hour)
	firstDay := today.Add(-(issueTrendBuckets - 1) * 24 * time.Hour)
	end := today.Add(24 * time.Hour)
	query := `_stream:{event_type="error"} AND fingerprint:in(` +
		strings.Join(filters, ",") + `) | stats by (fingerprint) count_uniq(event_id) as hits`
	target := strings.TrimRight(g.storage.LogsQueryBase(), "/") + "/select/logsql/stats_query_range?" + url.Values{
		"query": []string{query},
		"start": []string{firstDay.Format(time.RFC3339Nano)},
		"end":   []string{end.Format(time.RFC3339Nano)},
		"step":  []string{"24h"},
	}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	g.storage.SetTenantHeaders(req.Header, projectID)
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("VLogs stats_query_range status %d", resp.StatusCode)
	}
	var payload issueTrendRangeResponse
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 4<<20))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode VLogs trend stats: %w", err)
	}
	for _, series := range payload.Data.Result {
		fingerprint := series.Metric["fingerprint"]
		trend, exists := result[fingerprint]
		if !exists {
			continue
		}
		for _, pair := range series.Values {
			if len(pair) < 2 {
				continue
			}
			var tsNum float64
			if err := json.Unmarshal(pair[0], &tsNum); err != nil {
				return nil, fmt.Errorf("invalid VLogs trend timestamp %s", pair[0])
			}
			var countText string
			if err := json.Unmarshal(pair[1], &countText); err != nil {
				return nil, fmt.Errorf("invalid VLogs trend count %s", pair[1])
			}
			count, err := strconv.ParseInt(countText, 10, 64)
			if err != nil || count < 0 {
				return nil, fmt.Errorf("invalid VLogs trend count %q", countText)
			}
			sampleDay := time.Unix(int64(tsNum), 0).UTC().Truncate(24 * time.Hour)
			bucket := int(sampleDay.Sub(firstDay) / (24 * time.Hour))
			if bucket < 0 || bucket >= issueTrendBuckets {
				continue
			}
			trend[bucket] = count
		}
	}
	return result, nil
}
