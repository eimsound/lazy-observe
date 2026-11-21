package janitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/symbolicate"
)

type SourceMapStore interface {
	ListExpiredSourceMapReleases(context.Context, time.Time, int) ([]meta.ExpiredSourceMapRelease, error)
	DeleteSourceMapArtifacts(context.Context, int, string) error
	ProjectBySlug(context.Context, string) (meta.ProjectInfo, error)
	ReferencedSourceMapDigests(context.Context, int) (map[string]bool, error)
}

type conditionalSourceMapReleaseDeleter interface {
	DeleteExpiredSourceMapRelease(context.Context, int, string, time.Time) (bool, error)
}

type sourceMapSweepStore interface {
	ReferencedSourceMapDigestsAt(context.Context, int, time.Time) (map[string]bool, error)
	WithSourceMapSweepLock(context.Context, int, time.Time, func(map[string]bool) error) error
}

type sourceMapPinPruner interface {
	PruneExpiredSourceMapUploadPins(context.Context, time.Time) (int, error)
}

const (
	DefaultSourceMapGracePeriod   = 15 * time.Minute
	sourceMapSweepDeleteBatchSize = 16
)

type SourceMaps struct {
	Metadata    SourceMapStore
	Blobs       blob.Store
	GracePeriod time.Duration
	Now         func() time.Time
}

// Clean deletes expired releases' metadata, then reclaims revision blobs that
// nothing references any more. Revisions are content-addressed and shared by
// releases, so blobs are never deleted per release; a reconciling sweep also
// collects revisions left behind by manual deletes or by releases that were
// still pinned by pending Inbox work on an earlier run.
func (c *SourceMaps) Clean(ctx context.Context, cutoff time.Time) (int, error) {
	if c.Metadata == nil || c.Blobs == nil {
		return 0, nil
	}
	cutoff = cutoff.UTC().Round(0)
	deleted := 0
	skipped := make(map[meta.ExpiredSourceMapRelease]struct{})
	conditionalDeleter, hasConditionalDelete := c.Metadata.(conditionalSourceMapReleaseDeleter)
	for {
		releases, err := c.Metadata.ListExpiredSourceMapReleases(ctx, cutoff, 100)
		if err != nil {
			return deleted, fmt.Errorf("list expired source map releases: %w", err)
		}
		if len(releases) == 0 {
			break
		}
		progressed := false
		for _, release := range releases {
			if _, skip := skipped[release]; skip {
				continue
			}
			if hasConditionalDelete {
				removed, err := conditionalDeleter.DeleteExpiredSourceMapRelease(
					ctx,
					release.ProjectID,
					release.Release,
					cutoff,
				)
				if err != nil {
					return deleted, fmt.Errorf("delete source map metadata for %s: %w", release.Release, err)
				}
				if !removed {
					skipped[release] = struct{}{}
					continue
				}
			} else if err := c.Metadata.DeleteSourceMapArtifacts(ctx, release.ProjectID, release.Release); err != nil {
				return deleted, fmt.Errorf("delete source map metadata for %s: %w", release.Release, err)
			}
			deleted++
			progressed = true
		}
		if !progressed || len(releases) < 100 {
			break
		}
	}
	return deleted, c.sweepRevisions(ctx)
}

func (c *SourceMaps) sweepRevisions(ctx context.Context) error {
	objects, err := c.Blobs.List(ctx, "sourcemaps/")
	if err != nil {
		return fmt.Errorf("list source map revisions: %w", err)
	}
	nowFn := c.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC().Round(0)
	grace := c.GracePeriod
	if grace == 0 {
		grace = DefaultSourceMapGracePeriod
	}
	byProject := make(map[string][]string)
	for _, object := range objects {
		parts := strings.Split(object.Key, "/")
		if len(parts) != 4 || parts[2] != "revisions" || !strings.HasSuffix(parts[3], ".map") {
			continue
		}
		// Skip recently written blobs so an unpinned upload between blob Put
		// and metadata Upsert commit is never reclaimed as an orphan.
		if grace > 0 && !object.ModTime.IsZero() && now.Sub(object.ModTime.UTC()) < grace {
			continue
		}
		byProject[parts[1]] = append(byProject[parts[1]], strings.TrimSuffix(parts[3], ".map"))
	}
	sweepStore, hasSweepLock := c.Metadata.(sourceMapSweepStore)
	for slug, digests := range byProject {
		project, err := c.Metadata.ProjectBySlug(ctx, slug)
		if err != nil {
			if !meta.IsNotFound(err) {
				return fmt.Errorf("resolve source map project %s: %w", slug, err)
			}
			// Project no longer exists; every past-grace revision under this
			// slug is orphaned and must be reclaimed.
			for _, digest := range digests {
				key := symbolicate.RevisionKey(slug, digest)
				if err := c.Blobs.Delete(ctx, key); err != nil && !errors.Is(err, blob.ErrNotFound) {
					return fmt.Errorf("delete source map revision %s: %w", key, err)
				}
			}
			continue
		}
		if !hasSweepLock {
			referenced, err := c.Metadata.ReferencedSourceMapDigests(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("list referenced source map revisions for %s: %w", slug, err)
			}
			for _, digest := range digests {
				if referenced[digest] {
					continue
				}
				key := symbolicate.RevisionKey(slug, digest)
				if err := c.Blobs.Delete(ctx, key); err != nil && !errors.Is(err, blob.ErrNotFound) {
					return fmt.Errorf("delete source map revision %s: %w", key, err)
				}
			}
			continue
		}
		referenced, err := sweepStore.ReferencedSourceMapDigestsAt(ctx, project.ID, now)
		if err != nil {
			return fmt.Errorf("list referenced source map revisions for %s: %w", slug, err)
		}
		candidates := make([]string, 0)
		for _, digest := range digests {
			if !referenced[digest] {
				candidates = append(candidates, digest)
			}
		}
		for start := 0; start < len(candidates); start += sourceMapSweepDeleteBatchSize {
			batch := candidates[start:min(start+sourceMapSweepDeleteBatchSize, len(candidates))]
			if err := sweepStore.WithSourceMapSweepLock(
				ctx,
				project.ID,
				now,
				func(lockedReferenced map[string]bool) error {
					for _, digest := range batch {
						if lockedReferenced[digest] {
							continue
						}
						key := symbolicate.RevisionKey(slug, digest)
						if err := c.Blobs.Delete(ctx, key); err != nil && !errors.Is(err, blob.ErrNotFound) {
							return fmt.Errorf("delete source map revision %s: %w", key, err)
						}
					}
					return nil
				},
			); err != nil {
				return fmt.Errorf("sweep source map revisions for %s: %w", slug, err)
			}
		}
	}
	if pruner, ok := c.Metadata.(sourceMapPinPruner); ok {
		if _, err := pruner.PruneExpiredSourceMapUploadPins(ctx, now); err != nil {
			return fmt.Errorf("prune expired source map upload pins: %w", err)
		}
	}
	return nil
}

type AlertEventStore interface {
	DeleteExpiredAlertEvents(context.Context, time.Time) (int, error)
}

type ErrorReceiptStore interface {
	DeleteCompletedErrorReceipts(context.Context, time.Time) (int, error)
}

type ErrorReceipts struct{ Metadata ErrorReceiptStore }

func (c ErrorReceipts) Clean(ctx context.Context, cutoff time.Time) (int, error) {
	if c.Metadata == nil {
		return 0, nil
	}
	return c.Metadata.DeleteCompletedErrorReceipts(ctx, cutoff)
}

type AlertEvents struct{ Metadata AlertEventStore }

func (c AlertEvents) Clean(ctx context.Context, cutoff time.Time) (int, error) {
	if c.Metadata == nil {
		return 0, nil
	}
	return c.Metadata.DeleteExpiredAlertEvents(ctx, cutoff)
}

type ClosedIssueStore interface {
	DeleteExpiredClosedIssues(context.Context, time.Time) (int, error)
}

type ClosedIssues struct{ Metadata ClosedIssueStore }

func (c ClosedIssues) Clean(ctx context.Context, cutoff time.Time) (int, error) {
	if c.Metadata == nil {
		return 0, nil
	}
	return c.Metadata.DeleteExpiredClosedIssues(ctx, cutoff)
}

type ProfileStore interface {
	ListExpiredProfiles(context.Context, time.Time, int) ([]meta.ExpiredProfile, error)
	DeleteProfile(context.Context, int) error
}

type profileOrphanStore interface {
	ProjectBySlug(context.Context, string) (meta.ProjectInfo, error)
	ReferencedProfileBlobKeys(context.Context, int) (map[string]bool, error)
}

const DefaultProfileGracePeriod = 15 * time.Minute

type AuditStore interface {
	DeleteAuditBefore(context.Context, time.Time) (int, error)
}

type AuditEvents struct{ Metadata AuditStore }

func (c AuditEvents) Clean(ctx context.Context, cutoff time.Time) (int, error) {
	if c.Metadata == nil {
		return 0, nil
	}
	return c.Metadata.DeleteAuditBefore(ctx, cutoff)
}

type Profiles struct {
	Metadata    ProfileStore
	Blobs       blob.Store
	GracePeriod time.Duration
	Now         func() time.Time
}

func (c *Profiles) Clean(ctx context.Context, cutoff time.Time) (int, error) {
	if c.Metadata == nil || c.Blobs == nil {
		return 0, nil
	}
	cutoff = cutoff.UTC().Round(0)
	deleted := 0
	failed := make(map[int]struct{})
	var errs []error
	for {
		rows, err := c.Metadata.ListExpiredProfiles(ctx, cutoff, 100)
		if err != nil {
			errs = append(errs, fmt.Errorf("list expired profiles: %w", err))
			return deleted, errors.Join(errs...)
		}
		if len(rows) == 0 {
			break
		}
		progressed := false
		for _, row := range rows {
			if _, skip := failed[row.ID]; skip {
				continue
			}
			if err := c.Blobs.Delete(ctx, row.BlobKey); err != nil && !errors.Is(err, blob.ErrNotFound) {
				failed[row.ID] = struct{}{}
				errs = append(errs, fmt.Errorf("delete profile blob %d: %w", row.ID, err))
				continue
			}
			if err := c.Metadata.DeleteProfile(ctx, row.ID); err != nil && !meta.IsNotFound(err) {
				failed[row.ID] = struct{}{}
				errs = append(errs, fmt.Errorf("delete profile metadata %d: %w", row.ID, err))
				continue
			}
			deleted++
			progressed = true
		}
		if !progressed || len(rows) < 100 {
			break
		}
	}
	if orphanStore, ok := c.Metadata.(profileOrphanStore); ok {
		swept, sweepErr := c.sweepOrphans(ctx, orphanStore)
		deleted += swept
		if sweepErr != nil {
			errs = append(errs, sweepErr)
		}
	}
	return deleted, errors.Join(errs...)
}

func (c *Profiles) sweepOrphans(ctx context.Context, store profileOrphanStore) (int, error) {
	objects, err := c.Blobs.List(ctx, "profiles/")
	if err != nil {
		return 0, fmt.Errorf("list profile blobs for orphan sweep: %w", err)
	}
	nowFn := c.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC()
	grace := c.GracePeriod
	if grace == 0 {
		grace = DefaultProfileGracePeriod
	}
	bySlug := make(map[string][]string)
	for _, object := range objects {
		parts := strings.Split(object.Key, "/")
		if len(parts) < 3 {
			continue
		}
		if grace > 0 && !object.ModTime.IsZero() && now.Sub(object.ModTime.UTC()) < grace {
			continue
		}
		bySlug[parts[1]] = append(bySlug[parts[1]], object.Key)
	}
	swept := 0
	var errs []error
	for slug, keys := range bySlug {
		var referenced map[string]bool
		project, err := store.ProjectBySlug(ctx, slug)
		if err != nil {
			if !meta.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("resolve profile project %s: %w", slug, err))
				continue
			}
		} else {
			referenced, err = store.ReferencedProfileBlobKeys(ctx, project.ID)
			if err != nil {
				errs = append(errs, fmt.Errorf("list referenced profile blobs for %s: %w", slug, err))
				continue
			}
		}
		for _, key := range keys {
			if referenced[key] {
				continue
			}
			if err := c.Blobs.Delete(ctx, key); err != nil && !errors.Is(err, blob.ErrNotFound) {
				errs = append(errs, fmt.Errorf("delete orphan profile blob %s: %w", key, err))
				continue
			}
			swept++
		}
	}
	return swept, errors.Join(errs...)
}

type SessionStore interface {
	DeleteExpiredSessions(context.Context, time.Time) (int, error)
}

type Sessions struct{ Metadata SessionStore }

func (c Sessions) Clean(ctx context.Context, cutoff time.Time) (int, error) {
	if c.Metadata == nil {
		return 0, nil
	}
	return c.Metadata.DeleteExpiredSessions(ctx, cutoff)
}

type NotificationDeliveryStore interface {
	DeleteExpiredNotificationDeliveries(context.Context, time.Time) (int, error)
}

type NotificationDeliveries struct{ Metadata NotificationDeliveryStore }

func (c NotificationDeliveries) Clean(ctx context.Context, cutoff time.Time) (int, error) {
	if c.Metadata == nil {
		return 0, nil
	}
	return c.Metadata.DeleteExpiredNotificationDeliveries(ctx, cutoff)
}

type NotificationOutboxStore interface {
	DeleteExpiredNotificationOutbox(context.Context, time.Time) (int, error)
}

type NotificationOutbox struct{ Metadata NotificationOutboxStore }

func (c NotificationOutbox) Clean(ctx context.Context, cutoff time.Time) (int, error) {
	if c.Metadata == nil {
		return 0, nil
	}
	return c.Metadata.DeleteExpiredNotificationOutbox(ctx, cutoff)
}

type DeletedProjectBlobStore interface {
	ListPendingDeletedProjectBlobs(context.Context, int) ([]string, error)
	MarkDeletedProjectBlobsCleaned(context.Context, string, time.Time) error
}

// DeletedProjectBlobs retries blob prefix removal for deleted projects so
// transient S3 or process failures never leave orphaned project blobs behind.
type DeletedProjectBlobs struct {
	Metadata DeletedProjectBlobStore
	Blobs    blob.Store
	Now      func() time.Time
}

func (c *DeletedProjectBlobs) Clean(ctx context.Context, _ time.Time) (int, error) {
	if c.Metadata == nil || c.Blobs == nil {
		return 0, nil
	}
	nowFn := c.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	cleaned := 0
	failed := make(map[string]struct{})
	var errs []error
	for {
		slugs, err := c.Metadata.ListPendingDeletedProjectBlobs(ctx, 100)
		if err != nil {
			errs = append(errs, fmt.Errorf("list deleted project blob cleanups: %w", err))
			return cleaned, errors.Join(errs...)
		}
		if len(slugs) == 0 {
			return cleaned, errors.Join(errs...)
		}
		progressed := false
		for _, slug := range slugs {
			if _, skip := failed[slug]; skip {
				continue
			}
			var slugErr error
			for _, prefix := range []string{
				"profiles/" + safeBlobSegment(slug) + "/",
				"replay/" + slug + "/",
				"sourcemaps/" + slug + "/",
			} {
				if err := c.Blobs.DeletePrefix(ctx, prefix); err != nil && !errors.Is(err, blob.ErrNotFound) {
					slugErr = errors.Join(slugErr, fmt.Errorf("delete %s: %w", prefix, err))
				}
			}
			if slugErr != nil {
				failed[slug] = struct{}{}
				errs = append(errs, slugErr)
				continue
			}
			if err := c.Metadata.MarkDeletedProjectBlobsCleaned(ctx, slug, nowFn().UTC()); err != nil {
				failed[slug] = struct{}{}
				errs = append(errs, fmt.Errorf("mark deleted project blobs cleaned for %s: %w", slug, err))
				continue
			}
			cleaned++
			progressed = true
		}
		if !progressed || len(slugs) < 100 {
			return cleaned, errors.Join(errs...)
		}
	}
}

func safeBlobSegment(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 || b.String() == "." || b.String() == ".." {
		return "unknown"
	}
	return b.String()
}
