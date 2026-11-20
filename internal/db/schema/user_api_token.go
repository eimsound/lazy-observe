package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// UserAPIToken authenticates API requests as one local user.
type UserAPIToken struct {
	ent.Schema
}

func (UserAPIToken) Fields() []ent.Field {
	return []ent.Field{
		field.String("token").NotEmpty().Unique().Sensitive(),
		field.String("name").NotEmpty(),
		field.String("prefix").NotEmpty(),
		field.Bool("revoked").Default(false),
		field.Time("created_at").Default(UTCNow).Immutable(),
		field.Time("last_used_at").Optional().Nillable(),
		field.Int("user_id"),
	}
}

func (UserAPIToken) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("user", User.Type).
			Ref("api_tokens").
			Field("user_id").
			Unique().
			Required(),
	}
}
