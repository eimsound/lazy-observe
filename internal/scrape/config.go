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
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
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
	RetryBaseDelay       time.Duration
	RetryMaxDelay        time.Duration
	Now                  func() time.Time
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
	retryBaseDelay       time.Duration
	retryMaxDelay        time.Duration
	now                  func() time.Time

	statusMu        sync.RWMutex
	isLeader        bool
	desiredRevision uint64
	appliedRevision uint64
	lastSyncAt      *time.Time
	lastSyncError   string

	wakeCh   chan struct{}
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
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
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
		retryBaseDelay:       cfg.RetryBaseDelay,
		retryMaxDelay:        cfg.RetryMaxDelay,
		now:                  nowFn,
		isLeader:             !clustered,
		wakeCh:               make(chan struct{}, 1),
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
	s.bumpDesiredRevision()
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
		s.signalWake()
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
	s.bumpDesiredRevision()
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
		s.signalWake()
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
	s.bumpDesiredRevision()
	if err := s.syncLocked(ctx); err != nil {
		s.signalWake()
		return err
	}
	return nil
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
