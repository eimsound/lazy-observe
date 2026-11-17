package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

type AlertEvent struct{ ent.Schema }

func (AlertEvent) Fields() []ent.Field {
	return []ent.Field{
		field.String("fingerprint").NotEmpty(),
		field.Enum("status").Values("firing", "resolved"),
		field.JSON("labels", map[string]string{}),
		field.JSON("annotations", map[string]string{}),
		field.Time("starts_at"),
		field.Time("ends_at").Optional().Nillable(),
		field.Time("last_notified_at").Optional().Nillable(),
		field.Bool("inhibited").Default(false),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Int("rule_id"),
	}
}

func (AlertEvent) Edges() []ent.Edge {
	return []ent.Edge{edge.From("rule", AlertRule.Type).Ref("events").Field("rule_id").Unique().Required()}
}

func (AlertEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("fingerprint", "rule_id").Unique(),
		index.Fields("status"),
		index.Fields("inhibited"),
	}
}
