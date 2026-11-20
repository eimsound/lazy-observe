package janitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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

type SourceMaps struct {
	Metadata SourceMapStore
	Blobs    blob.Store
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
	byProject := make(map[string][]string)
	for _, object := range objects {
		parts := strings.Split(object.Key, "/")
		if len(parts) != 4 || parts[2] != "revisions" || !strings.HasSuffix(parts[3], ".map") {
			continue
		}
		byProject[parts[1]] = append(byProject[parts[1]], strings.TrimSuffix(parts[3], ".map"))
	}
	for slug, digests := range byProject {
		project, err := c.Metadata.ProjectBySlug(ctx, slug)
		if err != nil {
			if meta.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("resolve source map project %s: %w", slug, err)
		}
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
