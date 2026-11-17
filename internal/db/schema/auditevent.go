package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// AuditEvent is one persisted management audit record. Values are
// allow-listed by the gateway before they reach the store; no request body,
// credential, or provider configuration is ever stored here.
type AuditEvent struct {
	ent.Schema
}

func (AuditEvent) Fields() []ent.Field {
	return []ent.Field{
		field.String("action").NotEmpty(),
		field.Int("actor_id").Optional(),
		field.String("actor_username").Default("anonymous"),
		field.String("actor_role").Optional(),
		field.String("target_type").Default(""),
		field.Int("target_id").Optional(),
		field.String("target_name").Default(""),
		field.String("target_role").Default(""),
		field.Int("project_id").Optional(),
		field.String("project_slug").Default(""),
		field.String("changed_fields").Default(""),
		field.Bool("success").Default(true),
		field.String("failure_reason").Default(""),
		field.String("http_method").Default(""),
		field.String("http_path").Default(""),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}

func (AuditEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("created_at"),
		index.Fields("actor_username", "created_at"),
		index.Fields("action", "created_at"),
		index.Fields("project_id", "created_at"),
	}
}
