package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type AlertRule struct{ ent.Schema }

func (AlertRule) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty(),
		field.Enum("signal").Values("metrics", "logs").Default("metrics"),
		field.Enum("evaluation_engine").Values("paused", "native").Default("native"),
		field.String("expr").NotEmpty(),
		field.String("for").Default("0s"),
		field.String("severity").Default("warning"),
		field.JSON("labels", map[string]string{}).Default(map[string]string{}),
		field.JSON("annotations", map[string]string{}).Default(map[string]string{}),
		field.Bool("enabled").Default(true),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Int("project_id"),
	}
}

func (AlertRule) Indexes() []ent.Index {
	return []ent.Index{index.Fields("project_id", "name").Unique()}
}

func (AlertRule) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).Ref("alert_rules").Field("project_id").Unique().Required(),
		edge.To("channels", NotificationChannel.Type),
		edge.To("events", AlertEvent.Type),
	}
}
