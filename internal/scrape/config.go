// Package scrape manages exporter targets and renders vmagent configuration.
package scrape

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"gopkg.in/yaml.v3"
)

var ErrInvalidTarget = errors.New("invalid scrape target")

type ReloadFunc func() error

type Config struct {
	Store         *meta.Store
	ConfigPath    string
	Reload        ReloadFunc
	PlatformPorts []int
}

type Service struct {
	store         *meta.Store
	configPath    string
	reload        ReloadFunc
	platformPorts map[string]struct{}
}

type Input struct {
	Name     string            `json:"name"`
	URL      string            `json:"url"`
	Interval string            `json:"interval"`
	Labels   map[string]string `json:"labels"`
	Enabled  *bool             `json:"enabled"`
}

func NewService(cfg Config) *Service {
	ports := map[string]struct{}{"3100": {}, "18428": {}, "19428": {}, "20428": {}, "21428": {}, "22428": {}}
	for _, port := range cfg.PlatformPorts {
		if port > 0 {
			ports[strconv.Itoa(port)] = struct{}{}
		}
	}
	return &Service{store: cfg.Store, configPath: cfg.ConfigPath, reload: cfg.Reload, platformPorts: ports}
}

func JobName(t meta.ScrapeTargetView) string { return fmt.Sprintf("scrape-%d", t.ID) }

func (s *Service) Sync(ctx context.Context) error {
	if err := WriteConfig(ctx, s.store, s.configPath); err != nil {
		return err
	}
	if s.reload != nil {
		return s.reload()
	}
	return nil
}

func (s *Service) List(ctx context.Context, slug string) ([]meta.ScrapeTargetView, error) {
	return s.store.ListScrapeTargets(ctx, slug)
}

func (s *Service) Get(ctx context.Context, slug string, id int) (meta.ScrapeTargetView, error) {
	return s.store.ScrapeTarget(ctx, slug, id)
}

func (s *Service) Create(ctx context.Context, slug string, in Input) (meta.ScrapeTargetView, error) {
	if err := s.validate(in, false); err != nil {
		return meta.ScrapeTargetView{}, err
	}
	target, err := s.store.CreateScrapeTarget(ctx, meta.ScrapeTargetCreate{
		ProjectSlug: slug,
		Name:        strings.TrimSpace(in.Name),
		URL:         strings.TrimSpace(in.URL),
		Interval:    in.Interval,
		Labels:      nonNilLabels(in.Labels),
		Enabled:     in.Enabled,
	})
	if err == nil {
		err = s.Sync(ctx)
	}
	return target, err
}

func (s *Service) Update(ctx context.Context, slug string, id int, in Input) (meta.ScrapeTargetView, error) {
	old, err := s.Get(ctx, slug, id)
	if err != nil {
		return meta.ScrapeTargetView{}, err
	}
	merged := Input{Name: old.Name, URL: old.URL, Interval: old.Interval, Labels: old.Labels, Enabled: &old.Enabled}
	if in.Name != "" {
		merged.Name = in.Name
	}
	if in.URL != "" {
		merged.URL = in.URL
	}
	if in.Interval != "" {
		merged.Interval = in.Interval
	}
	if in.Labels != nil {
		merged.Labels = in.Labels
	}
	if in.Enabled != nil {
		merged.Enabled = in.Enabled
	}
	if err := s.validate(merged, false); err != nil {
		return meta.ScrapeTargetView{}, err
	}
	update := meta.ScrapeTargetUpdate{Labels: in.Labels, Enabled: in.Enabled}
	if in.Name != "" {
		name := strings.TrimSpace(in.Name)
		update.Name = &name
	}
	if in.URL != "" {
		url := strings.TrimSpace(in.URL)
		update.URL = &url
	}
	if in.Interval != "" {
		update.Interval = &in.Interval
	}
	target, err := s.store.UpdateScrapeTarget(ctx, slug, id, update)
	if err == nil {
		err = s.Sync(ctx)
	}
	return target, err
}

func (s *Service) Delete(ctx context.Context, slug string, id int) error {
	if err := s.store.DeleteScrapeTarget(ctx, slug, id); err != nil {
		return err
	}
	return s.Sync(ctx)
}

func (s *Service) validate(in Input, partial bool) error {
	if !partial && strings.TrimSpace(in.Name) == "" {
		return fmt.Errorf("%w: name is required", ErrInvalidTarget)
	}
	if !partial && strings.TrimSpace(in.URL) == "" {
		return fmt.Errorf("%w: url is required", ErrInvalidTarget)
	}
	if in.URL != "" {
		u, err := parseTargetURL(in.URL)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrInvalidTarget, err)
		}
		if _, forbidden := s.platformPorts[u.Port()]; forbidden {
			return fmt.Errorf("%w: platform port %s is not allowed", ErrInvalidTarget, u.Port())
		}
	}
	if in.Interval != "" {
		d, err := time.ParseDuration(in.Interval)
		if err != nil || d < time.Second {
			return fmt.Errorf("%w: interval must be at least 1s", ErrInvalidTarget)
		}
	}
	for key := range in.Labels {
		if key == "project_id" || key == "scrape_job" || key == "job" || key == "instance" ||
			strings.TrimSpace(key) == "" {
			return fmt.Errorf("%w: reserved label %q", ErrInvalidTarget, key)
		}
	}
	return nil
}

func nonNilLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return map[string]string{}
	}
	return labels
}

type vmagentConfig struct {
	ScrapeConfigs []scrapeConfig `yaml:"scrape_configs"`
}
type scrapeConfig struct {
	JobName        string              `yaml:"job_name"`
	ScrapeInterval string              `yaml:"scrape_interval"`
	Scheme         string              `yaml:"scheme"`
	MetricsPath    string              `yaml:"metrics_path"`
	Params         map[string][]string `yaml:"params,omitempty"`
	StaticConfigs  []staticConfig      `yaml:"static_configs"`
}
type staticConfig struct {
	Targets []string          `yaml:"targets"`
	Labels  map[string]string `yaml:"labels"`
}

func WriteConfig(ctx context.Context, store *meta.Store, path string) error {
	targets, err := store.ScrapeConfigSnapshot(ctx)
	if err != nil {
		return err
	}
	cfg := vmagentConfig{ScrapeConfigs: make([]scrapeConfig, 0, len(targets))}
	for _, target := range targets {
		u, err := parseTargetURL(target.URL)
		if err != nil {
			return fmt.Errorf("target %d: %w", target.ID, err)
		}
		labels := make(map[string]string, len(target.Labels)+2)
		keys := make([]string, 0, len(target.Labels))
		for k := range target.Labels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
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
				JobName:        job,
				ScrapeInterval: target.Interval,
				Scheme:         u.Scheme,
				MetricsPath:    u.Path,
				Params:         params,
				StaticConfigs:  []staticConfig{{Targets: []string{u.Host}, Labels: labels}},
			},
		)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func parseTargetURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, errors.New("invalid url")
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("url must be http(s)://host:port/metrics")
	}
	if u.Port() == "" {
		return nil, errors.New("explicit port is required")
	}
	if _, err := net.LookupPort("tcp", u.Port()); err != nil {
		return nil, errors.New("invalid port")
	}
	if u.Path == "" {
		u.Path = "/metrics"
	}
	return u, nil
}
