package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

type Silence struct{ ent.Schema }

func (Silence) Fields() []ent.Field {
	return []ent.Field{
		field.JSON("matchers", []map[string]string{}),
		field.Time("starts_at"),
		field.Time("ends_at"),
		field.String("comment").Optional(),
		field.String("creator").NotEmpty(),
		field.Time("created_at").Default(UTCNow).Immutable(),
		field.Int("project_id"),
	}
}

func (Silence) Edges() []ent.Edge {
	return []ent.Edge{edge.From("project", Project.Type).Ref("silences").Field("project_id").Unique().Required()}
}
