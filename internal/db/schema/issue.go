package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// Issue is the low-write-volume aggregate for a group of error events.
type Issue struct{ ent.Schema }

func (Issue) Fields() []ent.Field {
	return []ent.Field{
		field.String("fingerprint").NotEmpty(),
		field.String("title").NotEmpty(),
		field.String("level").NotEmpty(),
		field.Enum("status").Values("open", "resolved", "ignored").Default("open"),
		field.Int64("count").Default(0),
		field.Int64("user_count").Default(0),
		// A compact linear-counting bitmap. It avoids storing high-cardinality
		// session identifiers in SQLite; user_count is therefore an estimate.
		field.Int64("user_bitmap").Default(0),
		field.Time("first_seen"),
		field.Time("last_seen"),
		field.Time("status_changed_at"),
		field.Time("regressed_at").Optional().Nillable(),
		field.String("last_event_id").NotEmpty(),
		field.JSON("ai_analysis", map[string]any{}).Optional(),
		field.Time("ai_analysis_at").Optional().Nillable(),
		field.Int("project_id"),
	}
}

func (Issue) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "fingerprint").Unique(),
		index.Fields("project_id", "status", "last_seen"),
	}
}

func (Issue) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).Ref("issues").Field("project_id").Unique().Required(),
	}
}
