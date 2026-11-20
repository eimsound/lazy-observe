package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	entschema "entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ReplaySegment stores per-segment summary metadata and extracted markers for a
// browser replay session so ingest and detail reads stay decoupled from blob
// listing and full-segment decompression.
type ReplaySegment struct {
	ent.Schema
}

func (ReplaySegment) Annotations() []entschema.Annotation {
	return []entschema.Annotation{entsql.Annotation{Table: "replay_segments"}}
}

func (ReplaySegment) Fields() []ent.Field {
	return []ent.Field{
		field.String("session_id").NotEmpty(),
		field.Int("seq").NonNegative(),
		field.Time("started_at").Default(UTCNow),
		field.Time("last_seen_at").Default(UTCNow),
		field.Int("event_count").Default(0).NonNegative(),
		field.Int64("size_bytes").Default(0).NonNegative(),
		field.String("payload_sha256").Default(""),
		field.Text("markers_json").Default("[]"),
		field.Time("created_at").Default(UTCNow).Immutable(),
		field.Int("project_id"),
	}
}

func (ReplaySegment) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).Ref("replay_segments").Field("project_id").Unique().Required(),
	}
}

func (ReplaySegment) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "session_id", "seq").Unique(),
	}
}
