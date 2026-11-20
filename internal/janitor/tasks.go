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

const DefaultSourceMapGracePeriod = 15 * time.Minute

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
	deleted := 0
	for {
		releases, err := c.Metadata.ListExpiredSourceMapReleases(ctx, cutoff, 100)
		if err != nil {
			return deleted, fmt.Errorf("list expired source map releases: %w", err)
		}
		for _, release := range releases {
			if err := c.Metadata.DeleteSourceMapArtifacts(ctx, release.ProjectID, release.Release); err != nil {
				return deleted, fmt.Errorf("delete source map metadata for %s: %w", release.Release, err)
			}
			deleted++
		}
		if len(releases) < 100 {
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
	now := nowFn().UTC()
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
		// Skip recently written blobs so an in-flight upload between blob Put
		// and metadata Upsert commit is never reclaimed as an orphan.
		if grace > 0 && !object.ModTime.IsZero() && now.Sub(object.ModTime.UTC()) < grace {
			continue
		}
		byProject[parts[1]] = append(byProject[parts[1]], strings.TrimSuffix(parts[3], ".map"))
	}
	for slug, digests := range byProject {
		var referenced map[string]bool
		project, err := c.Metadata.ProjectBySlug(ctx, slug)
		if err != nil {
			if !meta.IsNotFound(err) {
				return fmt.Errorf("resolve source map project %s: %w", slug, err)
			}
			// Project no longer exists; every past-grace revision under this
			// slug is orphaned and must be reclaimed.
		} else {
			referenced, err = c.Metadata.ReferencedSourceMapDigests(ctx, project.ID)
			if err != nil {
				return fmt.Errorf("list referenced source map revisions for %s: %w", slug, err)
			}
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
	Metadata ProfileStore
	Blobs    blob.Store
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
			return deleted, errors.Join(errs...)
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
			return deleted, errors.Join(errs...)
		}
	}
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
