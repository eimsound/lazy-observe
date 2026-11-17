package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// User is a local lazy-observe account.
type User struct {
	ent.Schema
}

func (User) Fields() []ent.Field {
	return []ent.Field{
		field.String("username").NotEmpty().Unique(),
		field.String("password_hash").NotEmpty().Sensitive(),
		field.Enum("role").Values("admin", "member", "viewer"),
		field.String("display_name").Optional(),
		field.String("email").Optional().Unique(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("last_login_at").Optional(),
	}
}

func (User) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("sessions", Session.Type),
		edge.To("api_tokens", UserAPIToken.Type),
	}
}
