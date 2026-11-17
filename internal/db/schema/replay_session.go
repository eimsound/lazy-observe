package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ReplaySession stores metadata for a browser replay. Segment bodies live in blob storage.
type ReplaySession struct {
	ent.Schema
}

func (ReplaySession) Fields() []ent.Field {
	return []ent.Field{
		field.String("session_id").NotEmpty(),
		field.Time("started_at").Default(time.Now).Immutable(),
		field.Time("last_seen_at").Default(time.Now),
		field.Int("segment_count").Default(0).NonNegative(),
		field.Int64("total_bytes").Default(0).NonNegative(),
		field.Bool("has_error").Default(false),
		field.String("release").Optional().Nillable(),
		field.String("page_url").Optional().Nillable(),
		field.Int("project_id"),
	}
}

func (ReplaySession) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).Ref("replay_sessions").Field("project_id").Unique().Required(),
	}
}

func (ReplaySession) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "session_id").Unique(),
		index.Fields("project_id", "last_seen_at"),
	}
}
