package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ScrapeTarget is an exporter endpoint managed by vmagent.
type ScrapeTarget struct{ ent.Schema }

func (ScrapeTarget) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty(),
		field.String("url").NotEmpty(),
		field.String("interval").Default("30s"),
		field.JSON("labels", map[string]string{}).Default(map[string]string{}),
		field.Bool("enabled").Default(true),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Int("project_id"),
	}
}

func (ScrapeTarget) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).Ref("scrape_targets").Field("project_id").Unique().Required(),
	}
}

func (ScrapeTarget) Indexes() []ent.Index {
	return []ent.Index{index.Fields("project_id", "name").Unique()}
}
