package replay

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/meta"
)

type MetadataStore interface {
	ListExpiredReplaySessions(context.Context, time.Time, int) ([]meta.ExpiredReplaySession, error)
	DeleteReplaySession(context.Context, int) error
}

type Cleaner struct {
	Metadata MetadataStore
	Blobs    blob.Store
}

func (c *Cleaner) Clean(ctx context.Context, cutoff time.Time) (int, error) {
	if c.Metadata == nil || c.Blobs == nil {
		return 0, nil
	}
	cutoff = cutoff.UTC().Round(0)
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
			return deleted, errors.Join(errs...)
		}
		progressed := false
		for _, row := range rows {
			if _, skip := failed[row.ID]; skip {
				continue
			}
			prefix := "replay/" + row.ProjectSlug + "/" + row.SessionID + "/"
			if err := c.Blobs.DeletePrefix(ctx, prefix); err != nil && !errors.Is(err, blob.ErrNotFound) {
				failed[row.ID] = struct{}{}
				errs = append(errs, fmt.Errorf("delete replay blobs for %s: %w", row.SessionID, err))
				continue
			}
			if err := c.Metadata.DeleteReplaySession(ctx, row.ID); err != nil && !meta.IsNotFound(err) {
				failed[row.ID] = struct{}{}
				errs = append(errs, fmt.Errorf("delete replay metadata for %s: %w", row.SessionID, err))
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
