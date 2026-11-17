package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// ProjectAPIToken authenticates telemetry ingest and queries for one project.
type ProjectAPIToken struct {
	ent.Schema
}

func (ProjectAPIToken) Fields() []ent.Field {
	return []ent.Field{
		field.String("token").NotEmpty().Unique().Sensitive(),
		field.String("name").NotEmpty().Default("default"),
		field.String("prefix").NotEmpty(),
		field.Bool("revoked").Default(false),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("last_used_at").Optional().Nillable(),
		field.Int("project_id"),
	}
}

func (ProjectAPIToken) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).
			Ref("api_tokens").
			Field("project_id").
			Unique().
			Required(),
	}
}
