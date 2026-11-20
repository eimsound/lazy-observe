// Package scrape manages exporter targets and renders vmagent configuration.
package scrape

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
	"gopkg.in/yaml.v3"
)

const (
	DefaultMaxTargetsPerProject = 100
	defaultScrapeInterval       = 30 * time.Second
	minScrapeInterval           = time.Second
	maxScrapeInterval           = time.Hour
	defaultMaxScrapeTimeout     = 10 * time.Second
	defaultSampleLimit          = 10_000
	defaultLabelLimit           = 64
	maxCustomLabelsPerTarget    = 32
	maxLabelNameLen             = 128
	maxLabelValueLen            = 1024
	dnsLookupTimeout            = 2 * time.Second
)

var (
	ErrInvalidTarget = errors.New("invalid scrape target")
	ErrQuotaExceeded = errors.New("scrape target quota exceeded")

	validLabelNamePattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)
	promDurationPattern   = regexp.MustCompile(`^(?:([0-9]+)h)?(?:([0-9]+)m)?(?:([0-9]+)s)?$`)
)

const DefaultLeaderSyncInterval = 5 * time.Second

type ReloadFunc func() error

type Config struct {
	Store                *meta.Store
	ConfigPath           string
	Reload               ReloadFunc
	PlatformPorts        []int
	BlockedHostPorts     []string
	OutboundPolicy       outbound.Policy
	MaxTargetsPerProject int
	Resolver             outbound.Resolver
	Logger               *slog.Logger
}

type Service struct {
	mu                   sync.Mutex
	store                *meta.Store
	configPath           string
	reload               ReloadFunc
	platformPorts        map[int]struct{}
	blockedHostPorts     map[string]struct{}
	outboundPolicy       outbound.Policy
	maxTargetsPerProject int
	resolver             outbound.Resolver
	logger               *slog.Logger

	leaderMu sync.RWMutex
	isLeader bool
	hasHash  bool
	lastHash [sha256.Size]byte
}

type Input struct {
	Name     string            `json:"name"`
	URL      string            `json:"url"`
	Interval string            `json:"interval"`
	Labels   map[string]string `json:"labels"`
	Enabled  *bool             `json:"enabled"`
}

func NewService(cfg Config) *Service {
	ports := map[int]struct{}{3100: {}, 18428: {}, 19428: {}, 20428: {}, 21428: {}, 22428: {}}
	for _, port := range cfg.PlatformPorts {
		if port > 0 && port <= 65535 {
			ports[port] = struct{}{}
		}
	}
	for port := range cfg.OutboundPolicy.BlockedPorts {
		if port > 0 && port <= 65535 {
			ports[port] = struct{}{}
		}
	}
	policy := cfg.OutboundPolicy
	blockedPortsCopy := make(map[int]struct{}, len(ports))
	for p := range ports {
		blockedPortsCopy[p] = struct{}{}
	}
	policy.BlockedPorts = blockedPortsCopy

	blockedHostPorts := make(map[string]struct{}, len(cfg.BlockedHostPorts))
	for _, entry := range cfg.BlockedHostPorts {
		if normalized := normalizeHostPortKey(entry); normalized != "" {
			blockedHostPorts[normalized] = struct{}{}
		}
	}
	maxTargets := cfg.MaxTargetsPerProject
	if maxTargets <= 0 {
		maxTargets = DefaultMaxTargetsPerProject
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	clustered := cfg.Store != nil && cfg.Store.IsClustered()
	return &Service{
		store:                cfg.Store,
		configPath:           cfg.ConfigPath,
		reload:               cfg.Reload,
		platformPorts:        ports,
		blockedHostPorts:     blockedHostPorts,
		outboundPolicy:       policy,
		maxTargetsPerProject: maxTargets,
		resolver:             cfg.Resolver,
		logger:               logger,
		isLeader:             !clustered,
	}
}

func JobName(t meta.ScrapeTargetView) string { return fmt.Sprintf("scrape-%d", t.ID) }

func (s *Service) IsLeader() bool {
	s.leaderMu.RLock()
	defer s.leaderMu.RUnlock()
	return s.isLeader
}

func (s *Service) setLeader(leader bool) {
	s.leaderMu.Lock()
	s.isLeader = leader
	s.leaderMu.Unlock()
}

func (s *Service) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncLocked(ctx)
}

func (s *Service) syncLocked(ctx context.Context) error {
	if s.store != nil && s.store.IsClustered() && !s.IsLeader() {
		// In HA mode, only the replica holding the scrape leader lock renders
		// targets to vmagent. The leader's sync loop picks up DB changes written
		// by non-leader replicas automatically.
		return nil
	}
	return s.syncActiveLocked(ctx, true)
}

func (s *Service) syncActiveLocked(ctx context.Context, forceReload bool) error {
	data, err := renderConfig(ctx, s.store, s.logger)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	if !forceReload && s.hasHash && s.lastHash == sum {
		return nil
	}
	if err := writeAtomicFile(s.configPath, data); err != nil {
		return err
	}
	if s.reload != nil {
		if err := s.reload(); err != nil {
			return err
		}
	}
	s.lastHash = sum
	s.hasHash = true
	return nil
}

func (s *Service) syncStandbyLocked() error {
	data := []byte("scrape_configs: []\n")
	sum := sha256.Sum256(data)
	if s.hasHash && s.lastHash == sum {
		return nil
	}
	if err := writeAtomicFile(s.configPath, data); err != nil {
		return err
	}
	if s.reload != nil {
		if err := s.reload(); err != nil {
			return err
		}
	}
	s.lastHash = sum
	s.hasHash = true
	return nil
}

// RunLeaderLoop coordinates single-owner scraping across HA PostgreSQL replicas
// and periodically reconciles target changes written through other replicas.
func (s *Service) RunLeaderLoop(ctx context.Context, logger *slog.Logger, interval time.Duration) {
	if s.store == nil || !s.store.IsClustered() {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultLeaderSyncInterval
	}
	var release func() error
	releaseLeader := func() {
		s.setLeader(false)
		if release != nil {
			if err := release(); err != nil {
				logger.Warn("release scrape leader lock failed", "error", err)
			}
			release = nil
		}
	}
	defer releaseLeader()

	step := func() {
		if !s.IsLeader() {
			rel, acquired, err := s.store.TryAcquireScrapeLeader(ctx)
			if err != nil {
				if ctx.Err() == nil {
					logger.Warn("acquire scrape leader lock failed", "error", err)
				}
				return
			}
			if !acquired {
				s.mu.Lock()
				err = s.syncStandbyLocked()
				s.mu.Unlock()
				if err != nil {
					logger.Warn("clear standby scrape config failed", "error", err)
				}
				return
			}
			release = rel
			s.setLeader(true)
			logger.Info("acquired HA scrape leader lock")
			s.mu.Lock()
			err = s.syncActiveLocked(ctx, true)
			s.mu.Unlock()
			if err != nil {
				logger.Warn("initial leader scrape sync failed", "error", err)
			}
			return
		}
		s.mu.Lock()
		err := s.syncActiveLocked(ctx, false)
		s.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			logger.Warn("reconcile leader scrape config failed", "error", err)
		}
	}

	step()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			step()
		}
	}
}

func (s *Service) List(ctx context.Context, slug string) ([]meta.ScrapeTargetView, error) {
	return s.store.ListScrapeTargets(ctx, slug)
}

func (s *Service) Get(ctx context.Context, slug string, id int) (meta.ScrapeTargetView, error) {
	return s.store.ScrapeTarget(ctx, slug, id)
}

func (s *Service) Create(ctx context.Context, slug string, in Input) (meta.ScrapeTargetView, error) {
	normalized, err := s.validateAndNormalize(ctx, in, false)
	if err != nil {
		return meta.ScrapeTargetView{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	target, err := s.store.CreateScrapeTargetWithLimit(ctx, meta.ScrapeTargetCreate{
		ProjectSlug: slug,
		Name:        normalized.Name,
		URL:         normalized.URL,
		Interval:    normalized.Interval,
		Labels:      nonNilLabels(normalized.Labels),
		Enabled:     normalized.Enabled,
	}, s.maxTargetsPerProject)
	if errors.Is(err, meta.ErrScrapeTargetQuotaExceeded) {
		return meta.ScrapeTargetView{}, fmt.Errorf(
			"%w: project may configure at most %d scrape targets",
			ErrQuotaExceeded,
			s.maxTargetsPerProject,
		)
	}
	if err != nil {
		return meta.ScrapeTargetView{}, err
	}
	if err := s.syncLocked(ctx); err != nil {
		if rollbackErr := s.store.DeleteScrapeTarget(ctx, slug, target.ID); rollbackErr != nil {
			s.logger.Warn(
				"failed to roll back scrape target after sync failure",
				"target_id",
				target.ID,
				"project",
				slug,
				"error",
				rollbackErr,
			)
		} else {
			_ = WriteConfigWithLogger(ctx, s.store, s.configPath, s.logger)
		}
		return meta.ScrapeTargetView{}, err
	}
	return target, nil
}

func (s *Service) Update(ctx context.Context, slug string, id int, in Input) (meta.ScrapeTargetView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	old, target, err := s.store.UpdateScrapeTargetAtomic(
		ctx,
		slug,
		id,
		func(current meta.ScrapeTargetView) (meta.ScrapeTargetUpdate, error) {
			merged := Input{
				Name:     current.Name,
				URL:      current.URL,
				Interval: current.Interval,
				Labels:   cloneLabels(current.Labels),
				Enabled:  &current.Enabled,
			}
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
			normalized, validateErr := s.validateAndNormalize(ctx, merged, false)
			if validateErr != nil {
				return meta.ScrapeTargetUpdate{}, validateErr
			}
			update := meta.ScrapeTargetUpdate{Enabled: in.Enabled}
			if in.Name != "" {
				name := normalized.Name
				update.Name = &name
			}
			if in.URL != "" {
				u := normalized.URL
				update.URL = &u
			}
			if in.Interval != "" {
				interval := normalized.Interval
				update.Interval = &interval
			}
			if in.Labels != nil {
				update.Labels = nonNilLabels(normalized.Labels)
			}
			return update, nil
		},
	)
	if err != nil {
		return meta.ScrapeTargetView{}, err
	}
	if err := s.syncLocked(ctx); err != nil {
		// Restore previous state so DB and rendered vmagent config stay aligned.
		oldName, oldURL, oldInterval, oldEnabled := old.Name, old.URL, old.Interval, old.Enabled
		if _, rollbackErr := s.store.UpdateScrapeTarget(ctx, slug, id, meta.ScrapeTargetUpdate{
			Name:     &oldName,
			URL:      &oldURL,
			Interval: &oldInterval,
			Labels:   nonNilLabels(old.Labels),
			Enabled:  &oldEnabled,
		}); rollbackErr != nil {
			s.logger.Warn(
				"failed to restore scrape target after sync failure",
				"target_id",
				id,
				"project",
				slug,
				"error",
				rollbackErr,
			)
		} else {
			_ = WriteConfigWithLogger(ctx, s.store, s.configPath, s.logger)
		}
		return meta.ScrapeTargetView{}, err
	}
	return target, nil
}

func (s *Service) Delete(ctx context.Context, slug string, id int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.store.DeleteScrapeTarget(ctx, slug, id); err != nil {
		return err
	}
	return s.syncLocked(ctx)
}

func (s *Service) validate(in Input, partial bool) error {
	_, err := s.validateAndNormalize(context.Background(), in, partial)
	return err
}

func (s *Service) validateAndNormalize(ctx context.Context, in Input, partial bool) (Input, error) {
	out := Input{
		Name:    strings.TrimSpace(in.Name),
		URL:     strings.TrimSpace(in.URL),
		Enabled: in.Enabled,
	}
	if !partial && out.Name == "" {
		return Input{}, fmt.Errorf("%w: name is required", ErrInvalidTarget)
	}
	if !partial && out.URL == "" {
		return Input{}, fmt.Errorf("%w: url is required", ErrInvalidTarget)
	}
	if out.URL != "" {
		u, port, err := parseTargetURL(out.URL)
		if err != nil {
			return Input{}, fmt.Errorf("%w: %v", ErrInvalidTarget, err)
		}
		if _, forbidden := s.platformPorts[port]; forbidden {
			return Input{}, fmt.Errorf("%w: platform port %d is not allowed", ErrInvalidTarget, port)
		}
		if _, forbidden := s.blockedHostPorts[hostPortKey(u.Hostname(), port)]; forbidden {
			return Input{}, fmt.Errorf("%w: target endpoint is reserved by platform", ErrInvalidTarget)
		}
		if err := s.validateTargetDestination(ctx, u, port); err != nil {
			return Input{}, err
		}
		out.URL = u.String()
	}
	if strings.TrimSpace(in.Interval) != "" {
		canonical, _, err := normalizeInterval(in.Interval)
		if err != nil {
			return Input{}, fmt.Errorf("%w: %v", ErrInvalidTarget, err)
		}
		out.Interval = canonical
	}
	if in.Labels != nil {
		if err := validateLabels(in.Labels); err != nil {
			return Input{}, err
		}
		out.Labels = cloneLabels(in.Labels)
	}
	return out, nil
}

func (s *Service) validateTargetDestination(ctx context.Context, u *url.URL, port int) error {
	host := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(u.Hostname()), "."))
	if isCloudMetadataHostname(host) {
		return fmt.Errorf("%w: target host %q is blocked by outbound policy", ErrInvalidTarget, host)
	}
	if _, err := outbound.ValidateURL(u.String(), s.outboundPolicy); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidTarget, err)
	}
	// If host is already an IP literal, outbound.ValidateURL checked it via Policy.CheckIP.
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	if s.outboundPolicy.AllowPrivate {
		return nil
	}
	resolver := s.resolver
	explicitResolver := resolver != nil
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	lookupCtx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
	defer cancel()
	addrs, err := resolver.LookupNetIP(lookupCtx, "ip", host)
	if err != nil {
		if explicitResolver {
			return fmt.Errorf("%w: failed to resolve target host: %v", ErrInvalidTarget, err)
		}
		// Allow unresolved hostnames (e.g. offline tests or not-yet-created cluster
		// services) while still blocking any resolvable internal/metadata target.
		return nil
	}
	for _, addr := range addrs {
		if checkErr := s.outboundPolicy.CheckIP(addr, host, port); checkErr != nil {
			return fmt.Errorf("%w: target host resolves to blocked address", ErrInvalidTarget)
		}
	}
	return nil
}

func isCloudMetadataHostname(host string) bool {
	switch host {
	case "metadata", "metadata.google.internal", "metadata.goog", "instance-data", "instance-data.ec2.internal":
		return true
	default:
		return false
	}
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

func nonNilLabels(labels map[string]string) map[string]string {
	if labels == nil {
		return map[string]string{}
	}
	return labels
}

func cloneLabels(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

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
	return writeAtomicFile(path, []byte("scrape_configs: []\n"))
}

func renderConfig(ctx context.Context, store *meta.Store, logger *slog.Logger) ([]byte, error) {
	if logger == nil {
		logger = slog.Default()
	}
	targets, err := store.ScrapeConfigSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	cfg := vmagentConfig{ScrapeConfigs: make([]scrapeConfig, 0, len(targets))}
	for _, target := range targets {
		u, _, err := parseTargetURL(target.URL)
		if err != nil {
			logger.Warn(
				"skipping invalid scrape target URL while rendering config",
				"target_id",
				target.ID,
				"project",
				target.Project.Slug,
				"error",
				err,
			)
			continue
		}
		intervalLiteral, intervalDur, err := normalizeInterval(target.Interval)
		if err != nil {
			logger.Warn(
				"skipping scrape target with invalid interval while rendering config",
				"target_id",
				target.ID,
				"project",
				target.Project.Slug,
				"error",
				err,
			)
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
				logger.Warn(
					"dropping disallowed scrape target label while rendering config",
					"target_id",
					target.ID,
					"project",
					target.Project.Slug,
					"label",
					k,
				)
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
