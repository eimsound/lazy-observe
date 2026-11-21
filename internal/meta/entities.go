package meta

import (
	"errors"
	"fmt"
	"maps"
	"time"

	"entgo.io/ent/dialect/sql/sqlgraph"
	dbent "github.com/eimsound/lazy-observe/internal/db/ent"
)

// Project is the persistence-layer DTO for a project. Callers never receive
// generated Ent entities or query builders (ADR 0004).
type Project struct {
	ID        int       `json:"id,omitempty"`
	Name      string    `json:"name,omitempty"`
	Slug      string    `json:"slug,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// ProfileType is the kind of a stored profile. Callers use these constants
// rather than importing the generated Ent schema package (ADR 0004).
type ProfileType string

const (
	ProfileTypeCPU       ProfileType = "cpu"
	ProfileTypeHeap      ProfileType = "heap"
	ProfileTypeGoroutine ProfileType = "goroutine"
)

// ProfileMeta is the persistence-layer DTO for a stored continuous profile.
type ProfileMeta struct {
	ID            int               `json:"id,omitempty"`
	UploadID      string            `json:"upload_id,omitempty"`
	PayloadSha256 string            `json:"payload_sha256,omitempty"`
	Service       string            `json:"service,omitempty"`
	Type          ProfileType       `json:"type,omitempty"`
	Start         time.Time         `json:"start,omitempty"`
	End           time.Time         `json:"end,omitempty"`
	Size          int64             `json:"size,omitempty"`
	BlobKey       string            `json:"blob_key,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	ProjectID     int               `json:"project_id,omitempty"`
}

// ReplaySegment is the persistence-layer DTO for one committed replay segment.
type ReplaySegment struct {
	ID            int       `json:"id,omitempty"`
	SessionID     string    `json:"session_id,omitempty"`
	Seq           int       `json:"seq,omitempty"`
	StartedAt     time.Time `json:"started_at,omitempty"`
	LastSeenAt    time.Time `json:"last_seen_at,omitempty"`
	EventCount    int       `json:"event_count,omitempty"`
	SizeBytes     int64     `json:"size_bytes,omitempty"`
	PayloadSha256 string    `json:"payload_sha256,omitempty"`
	MarkersJSON   string    `json:"markers_json,omitempty"`
	CreatedAt     time.Time `json:"created_at,omitempty"`
	ProjectID     int       `json:"project_id,omitempty"`
}

// ReplaySession is the persistence-layer DTO for one aggregated replay session.
type ReplaySession struct {
	ID           int       `json:"id,omitempty"`
	SessionID    string    `json:"session_id,omitempty"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	LastSeenAt   time.Time `json:"last_seen_at,omitempty"`
	SegmentCount int       `json:"segment_count,omitempty"`
	TotalBytes   int64     `json:"total_bytes,omitempty"`
	HasError     bool      `json:"has_error,omitempty"`
	Release      *string   `json:"release,omitempty"`
	PageURL      *string   `json:"page_url,omitempty"`
	ProjectID    int       `json:"project_id,omitempty"`
}

// SourceMapArtifact is the persistence-layer DTO for one source map artifact.
type SourceMapArtifact struct {
	ID        int       `json:"id,omitempty"`
	Release   string    `json:"release,omitempty"`
	Filename  string    `json:"filename,omitempty"`
	Size      int64     `json:"size,omitempty"`
	Sha256    string    `json:"sha256,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	ProjectID int       `json:"project_id,omitempty"`
}

func projectFromEnt(row *dbent.Project) *Project {
	if row == nil {
		return nil
	}
	return &Project{
		ID:        row.ID,
		Name:      row.Name,
		Slug:      row.Slug,
		CreatedAt: row.CreatedAt,
	}
}

func profileMetaFromEnt(row *dbent.ProfileMeta) *ProfileMeta {
	if row == nil {
		return nil
	}
	return &ProfileMeta{
		ID:            row.ID,
		UploadID:      row.UploadID,
		PayloadSha256: row.PayloadSha256,
		Service:       row.Service,
		Type:          ProfileType(row.Type),
		Start:         row.Start,
		End:           row.End,
		Size:          row.Size,
		BlobKey:       row.BlobKey,
		Labels:        maps.Clone(row.Labels),
		ProjectID:     row.ProjectID,
	}
}

func replaySegmentFromEnt(row *dbent.ReplaySegment) *ReplaySegment {
	if row == nil {
		return nil
	}
	return &ReplaySegment{
		ID:            row.ID,
		SessionID:     row.SessionID,
		Seq:           row.Seq,
		StartedAt:     row.StartedAt,
		LastSeenAt:    row.LastSeenAt,
		EventCount:    row.EventCount,
		SizeBytes:     row.SizeBytes,
		PayloadSha256: row.PayloadSha256,
		MarkersJSON:   row.MarkersJSON,
		CreatedAt:     row.CreatedAt,
		ProjectID:     row.ProjectID,
	}
}

func cloneStringPtr(src *string) *string {
	if src == nil {
		return nil
	}
	v := *src
	return &v
}

func replaySessionFromEnt(row *dbent.ReplaySession) *ReplaySession {
	if row == nil {
		return nil
	}
	return &ReplaySession{
		ID:           row.ID,
		SessionID:    row.SessionID,
		StartedAt:    row.StartedAt,
		LastSeenAt:   row.LastSeenAt,
		SegmentCount: row.SegmentCount,
		TotalBytes:   row.TotalBytes,
		HasError:     row.HasError,
		Release:      cloneStringPtr(row.Release),
		PageURL:      cloneStringPtr(row.PageURL),
		ProjectID:    row.ProjectID,
	}
}

func sourceMapArtifactFromEnt(row *dbent.SourceMapArtifact) *SourceMapArtifact {
	if row == nil {
		return nil
	}
	return &SourceMapArtifact{
		ID:        row.ID,
		Release:   row.Release,
		Filename:  row.Filename,
		Size:      row.Size,
		Sha256:    row.Sha256,
		CreatedAt: row.CreatedAt,
		ProjectID: row.ProjectID,
	}
}

// Ordering helpers stay private: query builders are not part of the metadata
// API (ADR 0004).
var (
	orderAsc  = dbent.Asc
	orderDesc = dbent.Desc
)

var errConstraint = errors.New("constraint violation")

func IsConstraintError(err error) bool {
	return dbent.IsConstraintError(err) || sqlgraph.IsConstraintError(err) || errors.Is(err, errConstraint)
}

// NotFoundError covers metadata operations implemented with reviewed SQL
// rather than an Ent builder. IsNotFound recognizes both error families.
type NotFoundError struct{ Label string }

func (e *NotFoundError) Error() string { return fmt.Sprintf("meta: %s not found", e.Label) }

func IsNotFound(err error) bool {
	var metadataError *NotFoundError
	return dbent.IsNotFound(err) || errors.As(err, &metadataError)
}
