package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ProjectAlertConfig controls notifications emitted by platform events.
type ProjectAlertConfig struct{ ent.Schema }

func (ProjectAlertConfig) Fields() []ent.Field {
	return []ent.Field{
		field.Bool("new_issue").Default(true),
		field.Bool("regression").Default(true),
		field.Int("error_rate_threshold_per_min").Default(0).NonNegative(),
		field.Time("created_at").Default(UTCNow).Immutable(),
		field.Time("updated_at").Default(UTCNow).UpdateDefault(UTCNow),
		field.Int("project_id"),
	}
}

func (ProjectAlertConfig) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).Ref("alert_config").Field("project_id").Unique().Required(),
		edge.To("channels", NotificationChannel.Type),
	}
}

func (ProjectAlertConfig) Indexes() []ent.Index {
	return []ent.Index{index.Fields("project_id").Unique()}
}
