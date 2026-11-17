package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

type NotificationChannel struct{ ent.Schema }

func (NotificationChannel) Fields() []ent.Field {
	return []ent.Field{
		field.Enum("type").Values("webhook", "feishu", "dingtalk", "wecom", "smtp", "slack"),
		field.String("name").NotEmpty().Unique(),
		field.JSON("config", map[string]any{}).Default(map[string]any{}),
		field.Bool("enabled").Default(true),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
	}
}

func (NotificationChannel) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("rules", AlertRule.Type).Ref("channels"),
		edge.From("project_alert_configs", ProjectAlertConfig.Type).Ref("channels"),
	}
}
