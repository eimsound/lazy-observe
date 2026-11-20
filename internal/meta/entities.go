package meta

import (
	"errors"
	"fmt"

	"entgo.io/ent/dialect/sql/sqlgraph"
	dbent "github.com/eimsound/lazy-observe/internal/db/ent"
	"github.com/eimsound/lazy-observe/internal/db/ent/profilemeta"
)

// Ent entities are exposed by the metadata service because they are part of
// its existing query API. Their implementation is generated into
// internal/db/ent; no generated source lives in this package.
type (
	AuditEvent        = dbent.AuditEvent
	Issue             = dbent.Issue
	ProfileMeta       = dbent.ProfileMeta
	Project           = dbent.Project
	ProjectAPIToken   = dbent.ProjectAPIToken
	ReplaySegment     = dbent.ReplaySegment
	ReplaySession     = dbent.ReplaySession
	SourceMapArtifact = dbent.SourceMapArtifact
	User              = dbent.User
	UserAPIToken      = dbent.UserAPIToken
)

// ProfileType is the kind of a stored profile. Callers use these names rather
// than importing the generated Ent schema package (ADR 0004).
type ProfileType = profilemeta.Type

const (
	ProfileTypeCPU       = profilemeta.TypeCPU
	ProfileTypeHeap      = profilemeta.TypeHeap
	ProfileTypeGoroutine = profilemeta.TypeGoroutine
)

// Ordering helpers stay private: query builders are not part of the metadata
// API (ADR 0004).
var (
	orderAsc  = dbent.Asc
	orderDesc = dbent.Desc
)

func IsConstraintError(err error) bool {
	return dbent.IsConstraintError(err) || sqlgraph.IsConstraintError(err)
}

// NotFoundError covers metadata operations implemented with reviewed SQL
// rather than an Ent builder. IsNotFound recognizes both error families.
type NotFoundError struct{ Label string }

func (e *NotFoundError) Error() string { return fmt.Sprintf("meta: %s not found", e.Label) }

func IsNotFound(err error) bool {
	var metadataError *NotFoundError
	return dbent.IsNotFound(err) || errors.As(err, &metadataError)
}
