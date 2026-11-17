package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// InhibitionRule suppresses matching target alerts while a matching source is firing.
type InhibitionRule struct{ ent.Schema }

func (InhibitionRule) Fields() []ent.Field {
	return []ent.Field{
		field.JSON("source_matchers", []map[string]string{}),
		field.JSON("target_matchers", []map[string]string{}),
		field.JSON("equal_labels", []string{}),
		field.Bool("enabled").Default(true),
		field.String("comment").Optional(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Int("project_id"),
	}
}

func (InhibitionRule) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).Ref("inhibition_rules").Field("project_id").Unique().Required(),
	}
}

func (InhibitionRule) Indexes() []ent.Index {
	return []ent.Index{index.Fields("project_id", "enabled")}
}
