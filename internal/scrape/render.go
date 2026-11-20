package scrape

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
	"gopkg.in/yaml.v3"
)

const emptyScrapeConfigYAML = "scrape_configs: []\n"

type vmagentConfig struct {
	ScrapeConfigs []scrapeConfig `yaml:"scrape_configs"`
}

type scrapeConfig struct {
	JobName         string              `yaml:"job_name"`
	ScrapeInterval  string              `yaml:"scrape_interval"`
	ScrapeTimeout   string              `yaml:"scrape_timeout"`
	FollowRedirects bool                `yaml:"follow_redirects"`
	SampleLimit     int                 `yaml:"sample_limit"`
	LabelLimit      int                 `yaml:"label_limit"`
	Scheme          string              `yaml:"scheme"`
	MetricsPath     string              `yaml:"metrics_path"`
	Params          map[string][]string `yaml:"params,omitempty"`
	StaticConfigs   []staticConfig      `yaml:"static_configs"`
}

type staticConfig struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels"`
}

// JobName returns the deterministic vmagent scrape job name for a target.
func JobName(t meta.ScrapeTargetView) string { return fmt.Sprintf("scrape-%d", t.ID) }

// Render is a pure function that deterministically renders enabled scrape
// targets into vmagent YAML configuration without accessing storage or disk.
// Corrupt target rows are skipped and disallowed labels are dropped so a single
// malformed record cannot poison global scraping.
func Render(targets []meta.ScrapeTargetView) ([]byte, error) {
	return renderTargets(targets, nil)
}

func renderTargets(targets []meta.ScrapeTargetView, logger *slog.Logger) ([]byte, error) {
	ordered := append([]meta.ScrapeTargetView(nil), targets...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].ID != ordered[j].ID {
			return ordered[i].ID < ordered[j].ID
		}
		if ordered[i].Project.Slug != ordered[j].Project.Slug {
			return ordered[i].Project.Slug < ordered[j].Project.Slug
		}
		return ordered[i].Name < ordered[j].Name
	})

	cfg := vmagentConfig{ScrapeConfigs: make([]scrapeConfig, 0, len(ordered))}
	for _, target := range ordered {
		if !target.Enabled {
			continue
		}
		if strings.TrimSpace(target.Project.Slug) == "" {
			if logger != nil {
				logger.Warn(
					"skipping scrape target without project slug while rendering config",
					"target_id",
					target.ID,
				)
			}
			continue
		}
		u, _, err := parseTargetURL(target.URL)
		if err != nil {
			if logger != nil {
				logger.Warn(
					"skipping invalid scrape target URL while rendering config",
					"target_id",
					target.ID,
					"project",
					target.Project.Slug,
					"error",
					err,
				)
			}
			continue
		}
		intervalLiteral, intervalDur, err := normalizeInterval(target.Interval)
		if err != nil {
			if logger != nil {
				logger.Warn(
					"skipping scrape target with invalid interval while rendering config",
					"target_id",
					target.ID,
					"project",
					target.Project.Slug,
					"error",
					err,
				)
			}
			continue
		}
		labels := make(map[string]string, len(target.Labels)+2)
		keys := make([]string, 0, len(target.Labels))
		for k := range target.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !isAllowedCustomLabelName(k) || len(target.Labels[k]) > maxLabelValueLen ||
				strings.ContainsAny(target.Labels[k], "\r\n\x00") {
				if logger != nil {
					logger.Warn(
						"dropping disallowed scrape target label while rendering config",
						"target_id",
						target.ID,
						"project",
						target.Project.Slug,
						"label",
						k,
					)
				}
				continue
			}
			labels[k] = target.Labels[k]
		}
		job := JobName(target)
		labels["project_id"], labels["scrape_job"] = target.Project.Slug, job
		params := make(map[string][]string, len(u.Query()))
		for k, values := range u.Query() {
			params[k] = values
		}
		cfg.ScrapeConfigs = append(
			cfg.ScrapeConfigs,
			scrapeConfig{
				JobName:         job,
				ScrapeInterval:  intervalLiteral,
				ScrapeTimeout:   scrapeTimeoutForInterval(intervalDur),
				FollowRedirects: false,
				SampleLimit:     defaultSampleLimit,
				LabelLimit:      defaultLabelLimit,
				Scheme:          u.Scheme,
				MetricsPath:     u.Path,
				Params:          params,
				StaticConfigs:   []staticConfig{{Targets: []string{u.Host}, Labels: labels}},
			},
		)
	}
	return yaml.Marshal(cfg)
}

func WriteConfig(ctx context.Context, store *meta.Store, path string) error {
	return WriteConfigWithLogger(ctx, store, path, slog.Default())
}

// WriteConfigWithLogger renders enabled scrape targets to path atomically,
// skipping corrupt rows with a warning so one bad record cannot poison global
// vmagent scraping or prevent startup.
func WriteConfigWithLogger(ctx context.Context, store *meta.Store, path string, logger *slog.Logger) error {
	data, err := renderConfig(ctx, store, logger)
	if err != nil {
		return err
	}
	return writeAtomicFile(path, data)
}

func WriteEmptyConfig(path string) error {
	return writeAtomicFile(path, []byte(emptyScrapeConfigYAML))
}

func renderConfig(ctx context.Context, store *meta.Store, logger *slog.Logger) ([]byte, error) {
	if logger == nil {
		logger = slog.Default()
	}
	targets, err := store.ScrapeConfigSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return renderTargets(targets, logger)
}

func writeAtomicFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmpFile, err := os.CreateTemp(dir, ".scrape-*.yml.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return err
	}
	if err := tmpFile.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func validateLabels(labels map[string]string) error {
	if len(labels) > maxCustomLabelsPerTarget {
		return fmt.Errorf("%w: at most %d custom labels are allowed", ErrInvalidTarget, maxCustomLabelsPerTarget)
	}
	for key, val := range labels {
		if !isAllowedCustomLabelName(key) {
			return fmt.Errorf("%w: invalid or reserved label %q", ErrInvalidTarget, key)
		}
		if len(val) > maxLabelValueLen {
			return fmt.Errorf("%w: label %q value exceeds %d bytes", ErrInvalidTarget, key, maxLabelValueLen)
		}
		if strings.ContainsAny(val, "\r\n\x00") {
			return fmt.Errorf("%w: label %q value contains control characters", ErrInvalidTarget, key)
		}
	}
	return nil
}

func isAllowedCustomLabelName(key string) bool {
	if key == "" || len(key) > maxLabelNameLen {
		return false
	}
	if strings.HasPrefix(key, "__") {
		return false
	}
	switch key {
	case "project_id", "scrape_job", "job", "instance":
		return false
	}
	return validLabelNamePattern.MatchString(key)
}

func normalizeInterval(raw string) (string, time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return formatPrometheusDuration(defaultScrapeInterval), defaultScrapeInterval, nil
	}
	if trimmed != raw || strings.HasPrefix(trimmed, "+") || strings.HasPrefix(trimmed, "-") {
		return "", 0, errors.New("interval must be a positive Prometheus duration (e.g. 15s, 1m)")
	}
	matches := promDurationPattern.FindStringSubmatch(trimmed)
	if matches == nil || (matches[1] == "" && matches[2] == "" && matches[3] == "") {
		return "", 0, errors.New("interval must use whole h/m/s Prometheus duration units (e.g. 15s, 1m)")
	}
	var total time.Duration
	if matches[1] != "" {
		hours, err := strconv.ParseInt(matches[1], 10, 64)
		if err != nil || hours > 24 {
			return "", 0, errors.New("interval must be between 1s and 1h")
		}
		total += time.Duration(hours) * time.Hour
	}
	if matches[2] != "" {
		minutes, err := strconv.ParseInt(matches[2], 10, 64)
		if err != nil || minutes > 1440 {
			return "", 0, errors.New("interval must be between 1s and 1h")
		}
		total += time.Duration(minutes) * time.Minute
	}
	if matches[3] != "" {
		seconds, err := strconv.ParseInt(matches[3], 10, 64)
		if err != nil || seconds > 86400 {
			return "", 0, errors.New("interval must be between 1s and 1h")
		}
		total += time.Duration(seconds) * time.Second
	}
	if total < minScrapeInterval || total > maxScrapeInterval {
		return "", 0, errors.New("interval must be between 1s and 1h")
	}
	return formatPrometheusDuration(total), total, nil
}

func formatPrometheusDuration(d time.Duration) string {
	if d%time.Hour == 0 {
		return strconv.FormatInt(int64(d/time.Hour), 10) + "h"
	}
	if d%time.Minute == 0 {
		return strconv.FormatInt(int64(d/time.Minute), 10) + "m"
	}
	return strconv.FormatInt(int64(d/time.Second), 10) + "s"
}

func scrapeTimeoutForInterval(interval time.Duration) string {
	timeout := defaultMaxScrapeTimeout
	if interval < timeout {
		timeout = interval
	}
	if timeout < minScrapeInterval {
		timeout = minScrapeInterval
	}
	return formatPrometheusDuration(timeout)
}

func parseTargetURL(raw string) (*url.URL, int, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, 0, errors.New("invalid url")
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Opaque != "" || u.Hostname() == "" || u.User != nil ||
		u.Fragment != "" {
		return nil, 0, errors.New("url must be http(s)://host:port/metrics")
	}
	rawPort := u.Port()
	if rawPort == "" {
		return nil, 0, errors.New("explicit port is required")
	}
	port, canonicalPort, err := outbound.NormalizePort(rawPort)
	if err != nil {
		return nil, 0, err
	}
	u.Scheme = scheme
	u.Host = net.JoinHostPort(u.Hostname(), canonicalPort)
	if u.Path == "" {
		u.Path = "/metrics"
	}
	return u, port, nil
}

func normalizeHostPortKey(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	host, rawPort, err := net.SplitHostPort(raw)
	if err != nil {
		return ""
	}
	port, _, err := outbound.NormalizePort(rawPort)
	if err != nil {
		return ""
	}
	return hostPortKey(host, port)
}

func hostPortKey(host string, port int) string {
	host = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if addr, err := netip.ParseAddr(host); err == nil {
		host = addr.Unmap().String()
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}
