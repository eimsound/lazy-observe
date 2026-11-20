package gateway

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/scrape"
	storagepkg "github.com/eimsound/lazy-observe/internal/storage"
	"github.com/gofiber/fiber/v3"
	"golang.org/x/sync/errgroup"
)

const (
	metricsInstantQueryTimeout  = storagepkg.DefaultMetricsQueryTimeout
	metricsInstantQueryMaxBytes = storagepkg.DefaultMetricsQueryMaxBytes
	metricsBodyDrainLimit       = storagepkg.DefaultMetricsQueryBodyDrainSize
)

type scrapeTargetResponse struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	URL      string            `json:"url"`
	Interval string            `json:"interval"`
	Labels   map[string]string `json:"labels"`
	Enabled  bool              `json:"enabled"`
}

func scrapeTargetJSON(target meta.ScrapeTargetView) scrapeTargetResponse {
	labels := target.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	return scrapeTargetResponse{
		ID: strconv.Itoa(target.ID), Name: target.Name, URL: target.URL,
		Interval: target.Interval, Labels: labels, Enabled: target.Enabled,
	}
}

func scrapeTargetsJSON(targets []meta.ScrapeTargetView) []scrapeTargetResponse {
	response := make([]scrapeTargetResponse, 0, len(targets))
	for _, target := range targets {
		response = append(response, scrapeTargetJSON(target))
	}
	return response
}

func (g *Gateway) scrapeUnavailable(c fiber.Ctx) bool {
	if g.scrapes == nil {
		reason := g.scrapeUnavailableReason
		if reason == "" {
			reason = "scrape management unavailable"
		}
		_ = c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{"error": reason})
		return true
	}
	return false
}

func sessionProjectSlug(c fiber.Ctx) (string, bool) {
	p := currentProject(c)
	if p.ID <= 0 || p.Slug == "" {
		_ = c.Status(fiber.StatusInternalServerError).JSON(fiber.Map{"error": "project context is missing"})
		return "", false
	}
	return p.Slug, true
}

func (g *Gateway) listScrapeTargets(c fiber.Ctx) error {
	if g.scrapeUnavailable(c) {
		return nil
	}
	slug, ok := sessionProjectSlug(c)
	if !ok {
		return nil
	}
	v, err := g.scrapes.List(c.Context(), slug)
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(scrapeTargetsJSON(v))
}

func (g *Gateway) getScrapeTarget(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.scrapeUnavailable(c) {
		return nil
	}
	slug, ok := sessionProjectSlug(c)
	if !ok {
		return nil
	}
	v, err := g.scrapes.Get(c.Context(), slug, id)
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(scrapeTargetJSON(v))
}

func (g *Gateway) createScrapeTarget(c fiber.Ctx) error {
	if g.scrapeUnavailable(c) {
		return nil
	}
	slug, ok := sessionProjectSlug(c)
	if !ok {
		return nil
	}
	var in scrape.Input
	if c.Bind().Body(&in) != nil {
		return badRequest(c)
	}
	v, err := g.scrapes.Create(c.Context(), slug, in)
	if errors.Is(err, scrape.ErrInvalidTarget) {
		return badRequestMessage(c, err.Error())
	}
	if errors.Is(err, scrape.ErrQuotaExceeded) {
		return c.Status(fiber.StatusUnprocessableEntity).JSON(fiber.Map{"error": err.Error()})
	}
	if err != nil {
		return apiError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(scrapeTargetJSON(v))
}

func (g *Gateway) updateScrapeTarget(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.scrapeUnavailable(c) {
		return nil
	}
	slug, ok := sessionProjectSlug(c)
	if !ok {
		return nil
	}
	var in scrape.Input
	if c.Bind().Body(&in) != nil {
		return badRequest(c)
	}
	v, err := g.scrapes.Update(c.Context(), slug, id, in)
	if errors.Is(err, scrape.ErrInvalidTarget) {
		return badRequestMessage(c, err.Error())
	}
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(scrapeTargetJSON(v))
}

func (g *Gateway) deleteScrapeTarget(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.scrapeUnavailable(c) {
		return nil
	}
	slug, ok := sessionProjectSlug(c)
	if !ok {
		return nil
	}
	if err := g.scrapes.Delete(c.Context(), slug, id); err != nil {
		return apiError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (g *Gateway) metricsQueryClient() *storagepkg.MetricsClient {
	if g.metrics != nil {
		return g.metrics
	}
	return storagepkg.NewMetricsClient(storagepkg.MetricsClientConfig{
		Backend:        g.storage,
		HTTPClient:     g.client,
		DefaultTimeout: metricsInstantQueryTimeout,
		MaxBytes:       metricsInstantQueryMaxBytes,
		BodyDrainLimit: metricsBodyDrainLimit,
	})
}

func metricSeriesFingerprint(labels map[string]string) string {
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
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('\x00')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	return b.String()
}

func (g *Gateway) scrapeTargetHealth(c fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.scrapeUnavailable(c) {
		return nil
	}
	slug, ok := sessionProjectSlug(c)
	if !ok {
		return nil
	}
	target, err := g.scrapes.Get(c.Context(), slug, id)
	if err != nil {
		return apiError(c, err)
	}
	job := scrape.JobName(target)
	upExpr := fmt.Sprintf(`up{scrape_job=%q}`, job)
	tsExpr := fmt.Sprintf(`timestamp(up{scrape_job=%q})`, job)

	mc := g.metricsQueryClient()
	opts := storagepkg.MetricsQueryOptions{
		Timeout:  metricsInstantQueryTimeout,
		MaxBytes: metricsInstantQueryMaxBytes,
	}
	var upSamples, tsSamples []storagepkg.MetricsVectorSample
	eg, queryCtx := errgroup.WithContext(c.Context())
	eg.Go(func() error {
		var qErr error
		upSamples, qErr = mc.QueryVector(queryCtx, slug, upExpr, opts)
		return qErr
	})
	eg.Go(func() error {
		var qErr error
		tsSamples, qErr = mc.QueryVector(queryCtx, slug, tsExpr, opts)
		return qErr
	})
	if err := eg.Wait(); err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": err.Error()})
	}

	timestamps := make(map[string]float64, len(tsSamples))
	for _, s := range tsSamples {
		fp := metricSeriesFingerprint(s.Metric)
		if existing, exists := timestamps[fp]; !exists || s.Point.Value > existing {
			timestamps[fp] = s.Point.Value
		}
	}

	status := "unknown"
	var last *time.Time
	var chosen bool
	var chosenTS float64
	var chosenKey string
	for _, s := range upSamples {
		fp := metricSeriesFingerprint(s.Metric)
		ts, hasTS := timestamps[fp]
		if !hasTS {
			ts = s.Point.Timestamp
		}
		if !chosen || ts > chosenTS || (ts == chosenTS && fp > chosenKey) {
			chosen = true
			chosenTS = ts
			chosenKey = fp
			last = (storagepkg.MetricsPoint{Timestamp: ts}).Time()
			if s.Point.Value == 1 {
				status = "up"
			} else {
				status = "down"
			}
		}
	}
	return c.JSON(fiber.Map{"status": status, "last_scrape_at": last, "scrape_job": job})
}

type hostDTO struct {
	Instance           string   `json:"instance"`
	Hostname           string   `json:"hostname"`
	Up                 *bool    `json:"up"`
	CPUCores           *float64 `json:"cpu_cores"`
	Load1              *float64 `json:"load1"`
	MemoryUsedPercent  *float64 `json:"mem_used_percent"`
	DiskUsedPercentMax *float64 `json:"disk_used_percent_max"`
}

func ptr(v float64) *float64 { return &v }
func boolPtr(v bool) *bool   { return &v }

type hostMetricQuerySpec struct {
	key   string
	expr  string
	apply func(hosts map[string]*hostDTO, ensure func(string) *hostDTO, samples []storagepkg.MetricsVectorSample)
}

var hostMetricQueries = []hostMetricQuerySpec{
	{
		key:  "uname",
		expr: `max by(instance,nodename) (node_uname_info)`,
		apply: func(_ map[string]*hostDTO, ensure func(string) *hostDTO, samples []storagepkg.MetricsVectorSample) {
			for _, s := range samples {
				instance := s.Metric["instance"]
				if instance == "" {
					continue
				}
				h := ensure(instance)
				if name := s.Metric["nodename"]; name != "" {
					h.Hostname = name
				}
			}
		},
	},
	{
		key:  "cores",
		expr: `count(count by(instance,cpu) (node_cpu_seconds_total)) by(instance)`,
		apply: applyHostFloatMetric(func(h *hostDTO, v *float64) {
			h.CPUCores = v
		}),
	},
	{
		key:  "load",
		expr: `max by(instance) (node_load1)`,
		apply: applyHostFloatMetric(func(h *hostDTO, v *float64) {
			h.Load1 = v
		}),
	},
	{
		key:  "memory",
		expr: `100 * (1 - max by(instance) (node_memory_MemAvailable_bytes) / (max by(instance) (node_memory_MemTotal_bytes) > 0))`,
		apply: applyHostFloatMetric(func(h *hostDTO, v *float64) {
			h.MemoryUsedPercent = v
		}),
	},
	{
		key:  "disk",
		expr: `max by(instance) (100 * (1 - node_filesystem_avail_bytes{fstype!~"tmpfs|overlay|squashfs"} / (node_filesystem_size_bytes{fstype!~"tmpfs|overlay|squashfs"} > 0)))`,
		apply: applyHostFloatMetric(func(h *hostDTO, v *float64) {
			h.DiskUsedPercentMax = v
		}),
	},
	{
		// Only instances with node_* metrics represent hosts; non-host exporters
		// that emit `up{scrape_job=~"scrape-.*"}` must not appear as host cards.
		key:  "up",
		expr: `max by(instance) (up{scrape_job=~"scrape-.*"})`,
		apply: func(hosts map[string]*hostDTO, _ func(string) *hostDTO, samples []storagepkg.MetricsVectorSample) {
			for _, s := range samples {
				instance := s.Metric["instance"]
				if instance == "" {
					continue
				}
				h := hosts[instance]
				if h == nil {
					continue
				}
				h.Up = boolPtr(s.Point.Value == 1)
			}
		},
	},
}

func applyHostFloatMetric(
	setter func(*hostDTO, *float64),
) func(map[string]*hostDTO, func(string) *hostDTO, []storagepkg.MetricsVectorSample) {
	return func(_ map[string]*hostDTO, ensure func(string) *hostDTO, samples []storagepkg.MetricsVectorSample) {
		for _, s := range samples {
			if instance := s.Metric["instance"]; instance != "" {
				setter(ensure(instance), ptr(s.Point.Value))
			}
		}
	}
}

func (g *Gateway) hosts(c fiber.Ctx) error {
	slug, ok := sessionProjectSlug(c)
	if !ok {
		return nil
	}
	mc := g.metricsQueryClient()
	opts := storagepkg.MetricsQueryOptions{
		Timeout:  metricsInstantQueryTimeout,
		MaxBytes: metricsInstantQueryMaxBytes,
	}
	results := make([][]storagepkg.MetricsVectorSample, len(hostMetricQueries))
	eg, queryCtx := errgroup.WithContext(c.Context())
	for i, spec := range hostMetricQueries {
		idx, expr := i, spec.expr
		eg.Go(func() error {
			samples, err := mc.QueryVector(queryCtx, slug, expr, opts)
			if err != nil {
				return err
			}
			results[idx] = samples
			return nil
		})
	}
	if err := eg.Wait(); err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": err.Error()})
	}

	hosts := map[string]*hostDTO{}
	ensure := func(instance string) *hostDTO {
		h := hosts[instance]
		if h == nil {
			h = &hostDTO{Instance: instance, Hostname: instance}
			hosts[instance] = h
		}
		return h
	}
	for i, spec := range hostMetricQueries {
		spec.apply(hosts, ensure, results[i])
	}
	result := make([]hostDTO, 0, len(hosts))
	for _, h := range hosts {
		result = append(result, *h)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Instance < result[j].Instance })
	return c.JSON(result)
}

func badRequestMessage(c fiber.Ctx, message string) error {
	return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": message})
}
