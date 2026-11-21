package gateway

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	storagepkg "github.com/eimsound/lazy-observe/internal/storage"
	"github.com/gofiber/fiber/v3"
	"golang.org/x/sync/errgroup"
)

const (
	vitalsMaxBreakdownRows = 200
	vitalsTopkSeriesLimit  = 2_000
)

var vitalMetricKeys = []string{"lcp", "inp", "cls", "ttfb", "fcp"}

type vitalQueryFilters struct {
	Page          string `json:"page,omitempty"`
	DeviceType    string `json:"device_type,omitempty"`
	BrowserFamily string `json:"browser_family,omitempty"`
	OSFamily      string `json:"os_family,omitempty"`
	Country       string `json:"country,omitempty"`
}

type vitalValuesDTO struct {
	LCP  *float64 `json:"lcp,omitempty"`
	INP  *float64 `json:"inp,omitempty"`
	CLS  *float64 `json:"cls,omitempty"`
	TTFB *float64 `json:"ttfb,omitempty"`
	FCP  *float64 `json:"fcp,omitempty"`
}

type vitalBreakdownRowDTO struct {
	Value   string         `json:"value"`
	Samples int64          `json:"samples"`
	Values  vitalValuesDTO `json:"values"`
}

type performanceVitalsResponse struct {
	Start        int64                  `json:"start"`
	End          int64                  `json:"end"`
	StepSeconds  int64                  `json:"step_seconds"`
	Filters      vitalQueryFilters      `json:"filters"`
	Summary      vitalValuesDTO         `json:"summary"`
	TotalSamples int64                  `json:"total_samples"`
	ByPage       []vitalBreakdownRowDTO `json:"by_page"`
	ByDevice     []vitalBreakdownRowDTO `json:"by_device"`
	ByBrowser    []vitalBreakdownRowDTO `json:"by_browser"`
	ByOS         []vitalBreakdownRowDTO `json:"by_os"`
	ByCountry    []vitalBreakdownRowDTO `json:"by_country"`
	Truncated    bool                   `json:"truncated"`
}

func (g *Gateway) performanceVitals(c fiber.Ctx) error {
	start, end, step, ok := g.performanceRange(c)
	if !ok {
		return nil
	}
	filters, err := parseVitalQueryFilters(c)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": err.Error()})
	}
	response, err := g.queryPerformanceVitals(c.Context(), currentProject(c), start, end, step, filters)
	if err != nil {
		g.logger.Warn(
			"performance vitals query failed",
			"project", currentProject(c).Slug,
			"error", err,
		)
		return c.Status(http.StatusBadGateway).JSON(fiber.Map{"error": "web vitals query failed"})
	}
	return c.JSON(response)
}

func parseVitalQueryFilters(c fiber.Ctx) (vitalQueryFilters, error) {
	var filters vitalQueryFilters
	if raw := strings.TrimSpace(c.Query("page")); raw != "" {
		page, err := validateVitalPageFilter(raw)
		if err != nil {
			return vitalQueryFilters{}, err
		}
		filters.Page = page
	}
	if raw := strings.TrimSpace(c.Query("device_type")); raw != "" {
		device, ok := normalizeVitalDeviceType(raw)
		if !ok {
			return vitalQueryFilters{}, errors.New("invalid device_type filter")
		}
		filters.DeviceType = device
	}
	if raw := strings.TrimSpace(c.Query("browser_family")); raw != "" {
		browser, ok := normalizeVitalBrowserFamily(raw)
		if !ok {
			return vitalQueryFilters{}, errors.New("invalid browser_family filter")
		}
		filters.BrowserFamily = browser
	}
	if raw := strings.TrimSpace(c.Query("os_family")); raw != "" {
		osFamily, ok := normalizeVitalOSFamily(raw)
		if !ok {
			return vitalQueryFilters{}, errors.New("invalid os_family filter")
		}
		filters.OSFamily = osFamily
	}
	if raw := strings.TrimSpace(c.Query("country")); raw != "" {
		if strings.EqualFold(raw, vitalDimensionUnknown) {
			filters.Country = vitalDimensionUnknown
		} else if code, ok := normalizeVitalCountry(raw); ok {
			filters.Country = code
		} else {
			return vitalQueryFilters{}, errors.New("invalid country filter")
		}
	}
	return filters, nil
}

type vitalSeriesQueryResult struct {
	vital     string
	avg       []storagepkg.MetricsVectorSample
	count     []storagepkg.MetricsVectorSample
	truncated bool
}

func (g *Gateway) queryPerformanceVitals(
	ctx context.Context,
	project meta.ProjectInfo,
	start, end time.Time,
	step time.Duration,
	filters vitalQueryFilters,
) (performanceVitalsResponse, error) {
	windowLiteral := performanceDurationLiteral(end.Sub(start))
	queryCtx, cancel := context.WithTimeout(ctx, performanceQueryTimeout)
	defer cancel()

	results := make([]vitalSeriesQueryResult, len(vitalMetricKeys))
	eg, groupCtx := errgroup.WithContext(queryCtx)
	for i, key := range vitalMetricKeys {
		idx, vitalKey := i, key
		selector := buildVitalPromSelector("web_vitals_"+vitalKey, filters)
		avgExpr := fmt.Sprintf("avg_over_time(%s[%s])", selector, windowLiteral)
		countExpr := fmt.Sprintf("count_over_time(%s[%s])", selector, windowLiteral)

		eg.Go(func() error {
			avgSamples, avgTruncated, err := g.executeVitalVectorQuery(groupCtx, project, avgExpr, end)
			if err != nil {
				return err
			}
			countSamples, countTruncated, err := g.executeVitalVectorQuery(groupCtx, project, countExpr, end)
			if err != nil {
				return err
			}
			results[idx] = vitalSeriesQueryResult{
				vital:     vitalKey,
				avg:       avgSamples,
				count:     countSamples,
				truncated: avgTruncated || countTruncated,
			}
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return performanceVitalsResponse{}, err
	}

	return buildPerformanceVitalsResponse(results, start, end, step, filters), nil
}

func (g *Gateway) executeVitalVectorQuery(
	ctx context.Context,
	project meta.ProjectInfo,
	expression string,
	evalTime time.Time,
) ([]storagepkg.MetricsVectorSample, bool, error) {
	mc := g.metricsQueryClient()
	opts := storagepkg.MetricsQueryOptions{
		Time:     evalTime,
		Timeout:  performanceQueryTimeout,
		MaxBytes: performanceMaxResponseBody,
	}
	samples, err := mc.QueryVector(ctx, project.Slug, expression, opts)
	if errors.Is(err, storagepkg.ErrMetricsResponseTooLarge) {
		bounded := fmt.Sprintf("topk(%d, %s)", vitalsTopkSeriesLimit, expression)
		samples, err = mc.QueryVector(ctx, project.Slug, bounded, opts)
		if err != nil {
			return nil, false, err
		}
		return samples, true, nil
	}
	return samples, false, err
}

func buildVitalPromSelector(metricName string, filters vitalQueryFilters) string {
	matchers := make([]string, 0, 5)
	if filters.Page != "" {
		matchers = append(matchers, "page="+storagepkg.FormatPromLabelValue(filters.Page))
	}
	if filters.DeviceType != "" {
		matchers = append(matchers, vitalDimensionMatcher("device_type", filters.DeviceType, vitalDimensionUnknown))
	}
	if filters.BrowserFamily != "" {
		matchers = append(matchers, vitalDimensionMatcher("browser_family", filters.BrowserFamily, vitalDimensionOther))
	}
	if filters.OSFamily != "" {
		matchers = append(matchers, vitalDimensionMatcher("os_family", filters.OSFamily, vitalDimensionOther))
	}
	if filters.Country != "" {
		matchers = append(matchers, vitalDimensionMatcher("country", filters.Country, vitalDimensionUnknown))
	}
	if len(matchers) == 0 {
		return metricName
	}
	return fmt.Sprintf("%s{%s}", metricName, strings.Join(matchers, ","))
}

// vitalDimensionMatcher matches missing labels on legacy series when the caller
// explicitly filters for the default bucket ("unknown" / "other").
func vitalDimensionMatcher(label, value, defaultBucket string) string {
	if value == defaultBucket {
		return fmt.Sprintf(`%s=~"|%s"`, label, defaultBucket)
	}
	return label + "=" + storagepkg.FormatPromLabelValue(value)
}

type vitalSeriesTuple struct {
	page          string
	deviceType    string
	browserFamily string
	osFamily      string
	country       string
}

type vitalMetricAccum struct {
	weightedSum float64
	weightSum   float64
}

func (a *vitalMetricAccum) add(avg, weight float64) {
	if math.IsNaN(avg) || math.IsInf(avg, 0) || math.IsNaN(weight) || math.IsInf(weight, 0) || weight <= 0 {
		return
	}
	a.weightedSum += avg * weight
	a.weightSum += weight
}

func (a *vitalMetricAccum) mean() *float64 {
	if a == nil || a.weightSum <= 0 {
		return nil
	}
	val := a.weightedSum / a.weightSum
	if math.IsNaN(val) || math.IsInf(val, 0) {
		return nil
	}
	return &val
}

type vitalGroupAccum struct {
	samples int64
	vitals  map[string]*vitalMetricAccum
}

func newVitalGroupAccum() *vitalGroupAccum {
	return &vitalGroupAccum{vitals: make(map[string]*vitalMetricAccum, len(vitalMetricKeys))}
}

func (g *vitalGroupAccum) add(vital string, avg float64, sampleCount int64) {
	if math.IsNaN(avg) || math.IsInf(avg, 0) {
		return
	}
	weight := sampleCount
	if weight <= 0 {
		weight = 1
	}
	acc := g.vitals[vital]
	if acc == nil {
		acc = &vitalMetricAccum{}
		g.vitals[vital] = acc
	}
	acc.add(avg, float64(weight))
	g.samples += weight
}

func (g *vitalGroupAccum) valuesDTO() vitalValuesDTO {
	if g == nil {
		return vitalValuesDTO{}
	}
	return vitalValuesDTO{
		LCP:  g.vitals["lcp"].mean(),
		INP:  g.vitals["inp"].mean(),
		CLS:  g.vitals["cls"].mean(),
		TTFB: g.vitals["ttfb"].mean(),
		FCP:  g.vitals["fcp"].mean(),
	}
}

func buildPerformanceVitalsResponse(
	results []vitalSeriesQueryResult,
	start, end time.Time,
	step time.Duration,
	filters vitalQueryFilters,
) performanceVitalsResponse {
	summary := newVitalGroupAccum()
	byPage := make(map[string]*vitalGroupAccum)
	byDevice := make(map[string]*vitalGroupAccum)
	byBrowser := make(map[string]*vitalGroupAccum)
	byOS := make(map[string]*vitalGroupAccum)
	byCountry := make(map[string]*vitalGroupAccum)
	truncated := false

	addGroup := func(groups map[string]*vitalGroupAccum, key, vital string, avg float64, count int64) {
		group := groups[key]
		if group == nil {
			group = newVitalGroupAccum()
			groups[key] = group
		}
		group.add(vital, avg, count)
	}

	for _, res := range results {
		if res.truncated {
			truncated = true
		}
		rawCounts := make(map[string]int64, len(res.count))
		for _, sample := range res.count {
			if _, ok := vitalTupleFromLabels(sample.Metric); !ok {
				continue
			}
			val := sample.Point.Value
			if math.IsNaN(val) || math.IsInf(val, 0) || val <= 0 {
				continue
			}
			rawCounts[rawVitalSeriesKey(sample.Metric)] += max(int64(math.Round(val)), 1)
		}
		for _, sample := range res.avg {
			tuple, ok := vitalTupleFromLabels(sample.Metric)
			if !ok {
				continue
			}
			avg := sample.Point.Value
			if math.IsNaN(avg) || math.IsInf(avg, 0) {
				continue
			}
			count := rawCounts[rawVitalSeriesKey(sample.Metric)]
			if count <= 0 {
				count = 1
			}
			summary.add(res.vital, avg, count)
			addGroup(byPage, tuple.page, res.vital, avg, count)
			addGroup(byDevice, tuple.deviceType, res.vital, avg, count)
			addGroup(byBrowser, tuple.browserFamily, res.vital, avg, count)
			addGroup(byOS, tuple.osFamily, res.vital, avg, count)
			addGroup(byCountry, tuple.country, res.vital, avg, count)
		}
	}

	pageRows, pageTruncated := finalizeVitalBreakdownRows(byPage, vitalsMaxBreakdownRows)
	deviceRows, _ := finalizeVitalBreakdownRows(byDevice, vitalsMaxBreakdownRows)
	browserRows, _ := finalizeVitalBreakdownRows(byBrowser, vitalsMaxBreakdownRows)
	osRows, _ := finalizeVitalBreakdownRows(byOS, vitalsMaxBreakdownRows)
	countryRows, countryTruncated := finalizeVitalBreakdownRows(byCountry, vitalsMaxBreakdownRows)
	truncated = truncated || pageTruncated || countryTruncated

	return performanceVitalsResponse{
		Start:        start.UnixMilli(),
		End:          end.UnixMilli(),
		StepSeconds:  int64(step / time.Second),
		Filters:      filters,
		Summary:      summary.valuesDTO(),
		TotalSamples: summary.samples,
		ByPage:       pageRows,
		ByDevice:     deviceRows,
		ByBrowser:    browserRows,
		ByOS:         osRows,
		ByCountry:    countryRows,
		Truncated:    truncated,
	}
}

func rawVitalSeriesKey(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		if k == "__name__" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		v := labels[k]
		b.WriteString(strconv.Itoa(len(k)))
		b.WriteByte(':')
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(strconv.Itoa(len(v)))
		b.WriteByte(':')
		b.WriteString(v)
		b.WriteByte(';')
	}
	return b.String()
}

func vitalTupleFromLabels(labels map[string]string) (vitalSeriesTuple, bool) {
	page := strings.TrimSpace(labels["page"])
	if page == "" {
		return vitalSeriesTuple{}, false
	}
	return vitalSeriesTuple{
		page:          page,
		deviceType:    normalizeStoredVitalDeviceType(labels["device_type"]),
		browserFamily: normalizeStoredVitalBrowserFamily(labels["browser_family"]),
		osFamily:      normalizeStoredVitalOSFamily(labels["os_family"]),
		country:       normalizeStoredVitalCountry(labels["country"]),
	}, true
}

func finalizeVitalBreakdownRows(
	groups map[string]*vitalGroupAccum,
	limit int,
) ([]vitalBreakdownRowDTO, bool) {
	rows := make([]vitalBreakdownRowDTO, 0, len(groups))
	for value, acc := range groups {
		rows = append(rows, vitalBreakdownRowDTO{
			Value:   value,
			Samples: acc.samples,
			Values:  acc.valuesDTO(),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Samples != rows[j].Samples {
			return rows[i].Samples > rows[j].Samples
		}
		return rows[i].Value < rows[j].Value
	})
	if limit > 0 && len(rows) > limit {
		return rows[:limit], true
	}
	return rows, false
}
