package gateway

import (
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"

	"github.com/eimsound/lazy-observe/internal/ingestlimit"
)

// Endpoint anti-abuse guardrails (ADR 0020). Every quota below is enforced in
// process by one application replica; it never touches Redis, so the effective
// HA ceiling scales with the replica count.
const (
	maxIngestClockSkew = 5 * time.Minute

	profileProjectRateLimit     = 120
	profileIPRateLimit          = 240
	maxProfileMultipartOverhead = 1 << 20

	replayProjectRateLimit = 600
	replayIPRateLimit      = 300
	replaySessionRateLimit = 120

	defaultRemoteWriteConcurrency = 16
	defaultRemoteWriteRateLimit   = 600

	localIngestRateWindow = time.Minute
)

var (
	minIngestTimestamp = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)

	profileIngestPolicy = publicIngestPolicy{
		MaxBodyBytes:        maxProfileSize + maxProfileMultipartOverhead,
		ProjectPerMinute:    profileProjectRateLimit,
		IPPerMinute:         profileIPRateLimit,
		RateLimitMessage:    "profile ingest rate limit exceeded",
		BodyTooLargeMessage: "profile exceeds 10MB",
	}

	replayIngestPolicy = publicIngestPolicy{
		MaxBodyBytes:        maxReplaySegment,
		ProjectPerMinute:    replayProjectRateLimit,
		IPPerMinute:         replayIPRateLimit,
		RateLimitMessage:    "replay ingest rate limit exceeded",
		BodyTooLargeMessage: "segment exceeds 5MB",
	}
)

// publicIngestPolicy defines the unified admission bounds for public
// project-token ingest endpoints (profiling and browser replay).
type publicIngestPolicy struct {
	MaxBodyBytes        int
	ProjectPerMinute    int
	IPPerMinute         int
	RateLimitMessage    string
	BodyTooLargeMessage string
}

// publicIngestGuard binds a policy to this gateway's process-local limiters.
// Each dimension owns a separate bounded limiter so churn in one key space
// (for example many client IPs) cannot evict counters of another.
type publicIngestGuard struct {
	policy  publicIngestPolicy
	project *ingestlimit.Window
	ip      *ingestlimit.Window
}

func newPublicIngestGuard(policy publicIngestPolicy, now func() time.Time) *publicIngestGuard {
	return &publicIngestGuard{
		policy:  policy,
		project: newProjectIngestWindow(policy.ProjectPerMinute, now),
		ip:      newLocalIngestWindow(policy.IPPerMinute, now),
	}
}

// newLocalIngestWindow builds a per-minute limiter for high-cardinality keys
// (client IPs, replay sessions), bounded by ingestlimit.DefaultMaxWindowKeys.
func newLocalIngestWindow(perMinute int, now func() time.Time) *ingestlimit.Window {
	return ingestlimit.NewWindow(ingestlimit.WindowConfig{Limit: perMinute, Window: localIngestRateWindow, Now: now})
}

// newProjectIngestWindow builds a per-minute limiter keyed by project ID,
// bounded like ingest_rate_limit's default max_tracked_projects.
func newProjectIngestWindow(perMinute int, now func() time.Time) *ingestlimit.Window {
	return ingestlimit.NewWindow(ingestlimit.WindowConfig{
		Limit:   perMinute,
		Window:  localIngestRateWindow,
		MaxKeys: ingestlimit.DefaultMaxTrackedProjects,
		Now:     now,
	})
}

// publicIngestAdmission enforces the single-request byte quota plus per-project
// and per-IP local rate limits before a handler parses payloads.
func publicIngestAdmission(guard *publicIngestGuard) fiber.Handler {
	policy := guard.policy
	return func(c fiber.Ctx) error {
		if policy.MaxBodyBytes > 0 && ingestPayloadBytes(c) > int64(policy.MaxBodyBytes) {
			msg := policy.BodyTooLargeMessage
			if msg == "" {
				msg = "request body exceeds limit"
			}
			return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": msg})
		}
		if project := currentProject(c); project.ID > 0 {
			if ok, err := admitLocalIngest(c, guard.project, strconv.Itoa(project.ID), policy.RateLimitMessage); !ok {
				return err
			}
		}
		if ip := strings.TrimSpace(c.IP()); ip != "" {
			if ok, err := admitLocalIngest(c, guard.ip, ip, policy.RateLimitMessage); !ok {
				return err
			}
		}
		return c.Next()
	}
}

// admitLocalIngest is the single admission point for endpoint anti-abuse
// quotas. It reports true when admitted; otherwise it writes 429 with a
// Retry-After header set to the seconds until the key's window resets, and
// the caller must stop the handler chain by returning the accompanying error.
func admitLocalIngest(c fiber.Ctx, limiter *ingestlimit.Window, key, message string) (bool, error) {
	decision := limiter.Allow(key)
	if decision.Allowed {
		return true, nil
	}
	c.Set(fiber.HeaderRetryAfter, strconv.FormatInt(ingestlimit.RetryAfterSeconds(decision.RetryAfter), 10))
	return false, c.Status(http.StatusTooManyRequests).JSON(fiber.Map{"error": message})
}

// clampIngestTimestamp clamps client-supplied timestamps to [1970-01-01Z, now+5m]
// so future or malformed client clocks cannot pin retention windows.
func clampIngestTimestamp(at, fallback, maxAllowed time.Time) time.Time {
	at = at.UTC().Round(0)
	if at.Before(minIngestTimestamp) || at.After(maxAllowed) {
		return fallback.UTC().Round(0)
	}
	return at
}

// clampIngestWindow normalizes a [start, end] pair to UTC and clamps future
// endpoints back to server time while preserving start <= end.
func clampIngestWindow(start, end, now time.Time) (time.Time, time.Time) {
	now = now.UTC().Round(0)
	start = start.UTC().Round(0)
	end = end.UTC().Round(0)
	maxAllowed := now.Add(maxIngestClockSkew)
	if end.After(maxAllowed) {
		end = now
	}
	if start.After(end) {
		start = end
	}
	return start, end
}

// validIngestText validates UTF-8 metadata strings with a strict byte length
// bound and rejects NUL bytes.
func validIngestText(value string, maxLen int) bool {
	if len(value) > maxLen || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return false
	}
	return true
}

// sanitizeIngestText strips invalid UTF-8 sequences, NUL bytes, and surrounding
// whitespace from client-supplied metadata text.
func sanitizeIngestText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "")
	}
	if strings.ContainsRune(value, 0) {
		value = strings.ReplaceAll(value, "\x00", "")
	}
	return strings.TrimSpace(value)
}

// truncateIngestText sanitizes and truncates value to at most limit UTF-8 bytes
// without splitting a multibyte rune.
func truncateIngestText(value string, limit int) string {
	value = sanitizeIngestText(value)
	if len(value) > limit {
		value = value[:limit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}
