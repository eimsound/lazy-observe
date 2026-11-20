package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
)

// PlatformKeyFingerprint is the single-row (id = 1) record of the platform
// encryption key fingerprint. Startup compares the configured key against this
// row so a swapped key (for example a regenerated GitOps Secret) or replicas
// that do not share one key file fail loudly instead of silently losing every
// sealed value. It stores a truncated HMAC of the key, never key material.
type PlatformKeyFingerprint struct {
	ent.Schema
}

func (PlatformKeyFingerprint) Fields() []ent.Field {
	return []ent.Field{
		field.Int("id").Immutable(), // fixed row id 1
		field.String("fingerprint").NotEmpty(),
		field.Time("created_at").Immutable().Default(UTCNow),
		field.Time("updated_at").Default(UTCNow).UpdateDefault(UTCNow),
	}
}
