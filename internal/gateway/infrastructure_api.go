package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/scrape"
	"github.com/eimsound/lazy-observe/internal/storage"
	"github.com/gofiber/fiber/v2"
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

func (g *Gateway) scrapeUnavailable(c *fiber.Ctx) bool {
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

func (g *Gateway) listScrapeTargets(c *fiber.Ctx) error {
	if g.scrapeUnavailable(c) {
		return nil
	}
	v, err := g.scrapes.List(c.UserContext(), projectSlug(c))
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(scrapeTargetsJSON(v))
}
func (g *Gateway) getScrapeTarget(c *fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.scrapeUnavailable(c) {
		return nil
	}
	v, err := g.scrapes.Get(c.UserContext(), projectSlug(c), id)
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(scrapeTargetJSON(v))
}
func (g *Gateway) createScrapeTarget(c *fiber.Ctx) error {
	if g.scrapeUnavailable(c) {
		return nil
	}
	var in scrape.Input
	if c.BodyParser(&in) != nil {
		return badRequest(c)
	}
	v, err := g.scrapes.Create(c.UserContext(), projectSlug(c), in)
	if errors.Is(err, scrape.ErrInvalidTarget) {
		return badRequestMessage(c, err.Error())
	}
	if err != nil {
		return apiError(c, err)
	}
	return c.Status(fiber.StatusCreated).JSON(scrapeTargetJSON(v))
}
func (g *Gateway) updateScrapeTarget(c *fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.scrapeUnavailable(c) {
		return nil
	}
	var in scrape.Input
	if c.BodyParser(&in) != nil {
		return badRequest(c)
	}
	v, err := g.scrapes.Update(c.UserContext(), projectSlug(c), id, in)
	if errors.Is(err, scrape.ErrInvalidTarget) {
		return badRequestMessage(c, err.Error())
	}
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(scrapeTargetJSON(v))
}
func (g *Gateway) deleteScrapeTarget(c *fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.scrapeUnavailable(c) {
		return nil
	}
	if err := g.scrapes.Delete(c.UserContext(), projectSlug(c), id); err != nil {
		return apiError(c, err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

type metricSample struct {
	Metric map[string]string `json:"metric"`
	Value  []json.RawMessage `json:"value"`
}
type queryResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string         `json:"resultType"`
		Result     []metricSample `json:"result"`
	} `json:"data"`
}

func (g *Gateway) instantQuery(ctx *fiber.Ctx, slug, expr string) ([]metricSample, error) {
	q := url.Values{"query": {expr}}
	if g.storage.MetricsMode() == storage.MetricsPrometheus {
		if err := injectPromValues(q, slug); err != nil {
			return nil, err
		}
	} else {
		q.Set("extra_filters[]", fmt.Sprintf(`{project_id=%q}`, slug))
	}
	req, err := http.NewRequestWithContext(
		ctx.UserContext(),
		http.MethodGet,
		strings.TrimRight(g.storage.MetricsQueryBase(), "/")+"/api/v1/query?"+q.Encode(),
		nil,
	)
	if err != nil {
		return nil, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("metrics query status %d: %s", resp.StatusCode, body)
	}
	var decoded queryResponse
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	if decoded.Status != "success" || decoded.Data.ResultType != "vector" {
		return nil, errors.New("unexpected metrics query response")
	}
	return decoded.Data.Result, nil
}

func sampleFloat(s metricSample) (float64, bool) {
	if len(s.Value) != 2 {
		return 0, false
	}
	var raw string
	if json.Unmarshal(s.Value[1], &raw) != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(raw, 64)
	return v, err == nil
}
func sampleTime(s metricSample) *time.Time {
	if len(s.Value) == 0 {
		return nil
	}
	var seconds float64
	if json.Unmarshal(s.Value[0], &seconds) != nil {
		return nil
	}
	t := time.Unix(int64(seconds), int64((seconds-float64(int64(seconds)))*1e9)).UTC()
	return &t
}

func (g *Gateway) scrapeTargetHealth(c *fiber.Ctx) error {
	id, ok := pathID(c, "id")
	if !ok {
		return nil
	}
	if g.scrapeUnavailable(c) {
		return nil
	}
	target, err := g.scrapes.Get(c.UserContext(), projectSlug(c), id)
	if err != nil {
		return apiError(c, err)
	}
	job := scrape.JobName(target)
	result, err := g.instantQuery(c, projectSlug(c), fmt.Sprintf(`up{scrape_job=%q}`, job))
	if err != nil {
		return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": err.Error()})
	}
	status := "unknown"
	var last *time.Time
	if len(result) > 0 {
		last = sampleTime(result[0])
		if value, valid := sampleFloat(result[0]); valid {
			if value == 1 {
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

func (g *Gateway) hosts(c *fiber.Ctx) error {
	slug := projectSlug(c)
	queries := []struct{ key, expr string }{
		{"uname", `max by(instance,nodename) (node_uname_info)`},
		{"up", `max by(instance) (up{scrape_job=~"scrape-.*"})`},
		{"cores", `count(count by(instance,cpu) (node_cpu_seconds_total)) by(instance)`},
		{"load", `max by(instance) (node_load1)`},
		{
			"memory",
			`100 * (1 - max by(instance) (node_memory_MemAvailable_bytes) / max by(instance) (node_memory_MemTotal_bytes))`,
		},
		{
			"disk",
			`max by(instance) (100 * (1 - node_filesystem_avail_bytes{fstype!~"tmpfs|overlay|squashfs"} / node_filesystem_size_bytes{fstype!~"tmpfs|overlay|squashfs"}))`,
		},
	}
	all := make(map[string][]metricSample, len(queries))
	for _, query := range queries {
		result, err := g.instantQuery(c, slug, query.expr)
		if err != nil {
			return c.Status(fiber.StatusBadGateway).JSON(fiber.Map{"error": err.Error()})
		}
		all[query.key] = result
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
	for _, s := range all["up"] {
		instance := s.Metric["instance"]
		if instance == "" {
			continue
		}
		h := ensure(instance)
		if v, ok := sampleFloat(s); ok {
			h.Up = boolPtr(v == 1)
		}
	}
	for _, s := range all["uname"] {
		instance := s.Metric["instance"]
		if instance == "" {
			continue
		}
		h := ensure(instance)
		if name := s.Metric["nodename"]; name != "" {
			h.Hostname = name
		}
	}
	for _, item := range []struct {
		key string
		set func(*hostDTO, *float64)
	}{{"cores", func(h *hostDTO, v *float64) { h.CPUCores = v }}, {"load", func(h *hostDTO, v *float64) { h.Load1 = v }}, {"memory", func(h *hostDTO, v *float64) { h.MemoryUsedPercent = v }}, {"disk", func(h *hostDTO, v *float64) { h.DiskUsedPercentMax = v }}} {
		for _, s := range all[item.key] {
			if v, ok := sampleFloat(s); ok && s.Metric["instance"] != "" {
				item.set(ensure(s.Metric["instance"]), ptr(v))
			}
		}
	}
	result := make([]hostDTO, 0, len(hosts))
	for _, h := range hosts {
		result = append(result, *h)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Instance < result[j].Instance })
	return c.JSON(result)
}

func badRequestMessage(c *fiber.Ctx, message string) error {
	return c.Status(fiber.StatusBadRequest).JSON(fiber.Map{"error": message})
}
