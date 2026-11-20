package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// SourceMapUploadToken is a confidential, project-scoped credential that can only
// upload source maps for its project (ADR 0018). It lives in its own table so the
// ingest authenticator, which reads project_api_tokens, can never resolve it.
type SourceMapUploadToken struct {
	ent.Schema
}

func (SourceMapUploadToken) Fields() []ent.Field {
	return []ent.Field{
		field.String("token").NotEmpty().Unique().Sensitive(),
		field.String("name").NotEmpty(),
		field.String("prefix").NotEmpty(),
		field.Bool("revoked").Default(false),
		field.Time("created_at").Default(UTCNow).Immutable(),
		field.Time("last_used_at").Optional().Nillable(),
		field.Int("project_id"),
	}
}

func (SourceMapUploadToken) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).
			Ref("source_map_upload_tokens").
			Field("project_id").
			Unique().
			Required(),
	}
}
