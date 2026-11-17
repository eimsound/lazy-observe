package replay

import (
	"context"
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
	deleted := 0
	for {
		rows, err := c.Metadata.ListExpiredReplaySessions(ctx, cutoff, 100)
		if err != nil {
			return deleted, fmt.Errorf("list expired replay sessions: %w", err)
		}
		if len(rows) == 0 {
			return deleted, nil
		}
		for _, row := range rows {
			prefix := "replay/" + row.ProjectSlug + "/" + row.SessionID + "/"
			if err := c.Blobs.DeletePrefix(ctx, prefix); err != nil {
				return deleted, fmt.Errorf("delete replay blobs for %s: %w", row.SessionID, err)
			}
			if err := c.Metadata.DeleteReplaySession(ctx, row.ID); err != nil {
				return deleted, fmt.Errorf("delete replay metadata for %s: %w", row.SessionID, err)
			}
			deleted++
		}
		if len(rows) < 100 {
			return deleted, nil
		}
	}
}
