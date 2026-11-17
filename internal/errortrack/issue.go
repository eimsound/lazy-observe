package errortrack

import (
	"context"
	"hash/fnv"
	"time"
)

type IssueDelta struct {
	ProjectID   int
	Fingerprint string
	Title       string
	Level       string
	Count       int64
	SessionBits uint64
	FirstSeen   time.Time
	LastSeen    time.Time
	LastEventID string
	AcceptedAt  time.Time
	AppliedAt   time.Time
}

type IssueChange struct {
	Kind      string    `json:"kind"`
	ProjectID int       `json:"project_id"`
	IssueID   int       `json:"issue_id"`
	Title     string    `json:"title"`
	Level     string    `json:"level"`
	SeenAt    time.Time `json:"seen_at"`
}

type IssueChangeFunc func(context.Context, IssueChange) error

type ErrorOccurrence struct {
	ProjectID  int
	OccurredAt time.Time
	AcceptedAt time.Time
}

type ErrorOccurrenceFunc func(context.Context, ErrorOccurrence) error

func sessionBit(sessionID string) uint64 {
	if sessionID == "" {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(sessionID))
	return uint64(1) << (h.Sum64() % 63)
}
