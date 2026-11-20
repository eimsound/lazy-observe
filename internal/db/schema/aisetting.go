package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

// AISetting is the single-row (id = 1) runtime AI configuration managed by
// administrators from the web UI. The BYO API key is stored encrypted
// (api_key_ciphertext); plaintext keys never touch the database.
type AISetting struct {
	ent.Schema
}

func (AISetting) Fields() []ent.Field {
	return []ent.Field{
		field.Int("id").Immutable(), // fixed row id 1
		field.Bool("enabled").Default(false),
		field.String("provider").Default(""),
		field.String("base_url").Default(""),
		field.String("model").Default(""),
		field.String("api_key_ciphertext").Default(""),
		field.Int("max_tokens").Default(0),
		field.Time("updated_at").Default(UTCNow).UpdateDefault(UTCNow),
		field.String("updated_by").Default(""),
	}
}
