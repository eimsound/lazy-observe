package replay

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/meta"
)

type MetadataStore interface {
	ListExpiredReplaySessions(context.Context, time.Time, int) ([]meta.ExpiredReplaySession, error)
	DeleteReplaySession(context.Context, int) error
}

type conditionalReplayDeleter interface {
	DeleteExpiredReplaySession(context.Context, int, time.Time) (bool, error)
}

type replayOrphanStore interface {
	ProjectBySlug(context.Context, string) (meta.ProjectInfo, error)
	ListReplaySessionIDs(context.Context, int) (map[string]bool, error)
	ReferencedReplaySegmentSeqs(context.Context, int, string) (map[int]bool, error)
}

const DefaultGracePeriod = 15 * time.Minute

type Cleaner struct {
	Metadata    MetadataStore
	Blobs       blob.Store
	GracePeriod time.Duration
	Now         func() time.Time
}

func (c *Cleaner) Clean(ctx context.Context, cutoff time.Time) (int, error) {
	if c.Metadata == nil || c.Blobs == nil {
		return 0, nil
	}
	cutoff = cutoff.UTC().Round(0)
	nowFn := c.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC()
	deleted := 0
	failed := make(map[int]struct{})
	var errs []error
	for {
		rows, err := c.Metadata.ListExpiredReplaySessions(ctx, cutoff, 100)
		if err != nil {
			errs = append(errs, fmt.Errorf("list expired replay sessions: %w", err))
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
			prefix := "replay/" + row.ProjectSlug + "/" + row.SessionID + "/"
			if c.GracePeriod > 0 {
				objects, listErr := c.Blobs.List(ctx, prefix)
				if listErr != nil {
					failed[row.ID] = struct{}{}
					errs = append(errs, fmt.Errorf("list replay blobs for %s: %w", row.SessionID, listErr))
					continue
				}
				if hasRecentReplayObject(objects, now, c.GracePeriod) {
					failed[row.ID] = struct{}{}
					continue
				}
			}
			if err := c.Blobs.DeletePrefix(ctx, prefix); err != nil && !errors.Is(err, blob.ErrNotFound) {
				failed[row.ID] = struct{}{}
				errs = append(errs, fmt.Errorf("delete replay blobs for %s: %w", row.SessionID, err))
				continue
			}
			if conditional, ok := c.Metadata.(conditionalReplayDeleter); ok {
				removed, delErr := conditional.DeleteExpiredReplaySession(ctx, row.ID, cutoff)
				if delErr != nil {
					failed[row.ID] = struct{}{}
					errs = append(errs, fmt.Errorf("delete replay metadata for %s: %w", row.SessionID, delErr))
					continue
				}
				if !removed {
					// A concurrent segment refreshed last_seen_at past cutoff.
					failed[row.ID] = struct{}{}
					continue
				}
			} else if err := c.Metadata.DeleteReplaySession(ctx, row.ID); err != nil && !meta.IsNotFound(err) {
				failed[row.ID] = struct{}{}
				errs = append(errs, fmt.Errorf("delete replay metadata for %s: %w", row.SessionID, err))
				continue
			}
			deleted++
			progressed = true
		}
		if !progressed || len(rows) < 100 {
			break
		}
	}
	if orphanStore, ok := c.Metadata.(replayOrphanStore); ok {
		swept, sweepErr := c.sweepOrphans(ctx, orphanStore, now)
		deleted += swept
		if sweepErr != nil {
			errs = append(errs, sweepErr)
		}
	}
	return deleted, errors.Join(errs...)
}

func (c *Cleaner) sweepOrphans(ctx context.Context, store replayOrphanStore, now time.Time) (int, error) {
	objects, err := c.Blobs.List(ctx, "replay/")
	if err != nil {
		return 0, fmt.Errorf("list replay blobs for orphan sweep: %w", err)
	}
	grace := c.GracePeriod
	if grace == 0 {
		grace = DefaultGracePeriod
	}
	bySlugSession := make(map[string]map[string][]blob.Object)
	for _, object := range objects {
		if grace > 0 && !object.ModTime.IsZero() && now.Sub(object.ModTime.UTC()) < grace {
			continue
		}
		parts := strings.Split(object.Key, "/")
		if len(parts) != 4 {
			continue
		}
		slug, sessionID := parts[1], parts[2]
		if bySlugSession[slug] == nil {
			bySlugSession[slug] = make(map[string][]blob.Object)
		}
		bySlugSession[slug][sessionID] = append(bySlugSession[slug][sessionID], object)
	}
	swept := 0
	var errs []error
	for slug, sessionsMap := range bySlugSession {
		var activeSessions map[string]bool
		var projectID int
		project, err := store.ProjectBySlug(ctx, slug)
		if err != nil {
			if !meta.IsNotFound(err) {
				errs = append(errs, fmt.Errorf("resolve replay project %s: %w", slug, err))
				continue
			}
		} else {
			projectID = project.ID
			activeSessions, err = store.ListReplaySessionIDs(ctx, project.ID)
			if err != nil {
				errs = append(errs, fmt.Errorf("list replay sessions for %s: %w", slug, err))
				continue
			}
		}
		for sessionID, objs := range sessionsMap {
			if !activeSessions[sessionID] {
				for _, obj := range objs {
					if err := c.Blobs.Delete(ctx, obj.Key); err != nil && !errors.Is(err, blob.ErrNotFound) {
						errs = append(errs, fmt.Errorf("delete orphan replay blob %s: %w", obj.Key, err))
						continue
					}
					swept++
				}
				continue
			}
			committedSeqs, err := store.ReferencedReplaySegmentSeqs(ctx, projectID, sessionID)
			if err != nil {
				errs = append(errs, fmt.Errorf("list replay segments for %s/%s: %w", slug, sessionID, err))
				continue
			}
			// Preserve legacy sessions that predate replay_segments rows.
			if len(committedSeqs) == 0 {
				continue
			}
			for _, obj := range objs {
				seq, ok := replayBlobSeq(obj.Key)
				if !ok || committedSeqs[seq] {
					continue
				}
				if err := c.Blobs.Delete(ctx, obj.Key); err != nil && !errors.Is(err, blob.ErrNotFound) {
					errs = append(errs, fmt.Errorf("delete orphan replay segment blob %s: %w", obj.Key, err))
					continue
				}
				swept++
			}
		}
	}
	return swept, errors.Join(errs...)
}

func replayBlobSeq(key string) (int, bool) {
	name := path.Base(key)
	for _, suffix := range []string{".json.gz", ".meta.json"} {
		if trimmed := strings.TrimSuffix(name, suffix); trimmed != name {
			seq, err := strconv.Atoi(trimmed)
			return seq, err == nil && seq >= 0
		}
	}
	return 0, false
}

func hasRecentReplayObject(objects []blob.Object, now time.Time, grace time.Duration) bool {
	for _, object := range objects {
		if !object.ModTime.IsZero() && now.Sub(object.ModTime.UTC()) < grace {
			return true
		}
	}
	return false
}
