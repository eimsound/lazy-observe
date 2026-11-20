package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// SourceMapArtifact indexes source maps whose bodies live in blob storage.
type SourceMapArtifact struct{ ent.Schema }

func (SourceMapArtifact) Fields() []ent.Field {
	return []ent.Field{
		field.String("release").NotEmpty(),
		field.String("filename").NotEmpty(),
		field.Int64("size").NonNegative(),
		field.String("sha256").NotEmpty(),
		field.Time("created_at").Default(UTCNow).Immutable(),
		field.Int("project_id"),
	}
}

func (SourceMapArtifact) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).Ref("source_map_artifacts").Field("project_id").Unique().Required(),
	}
}

func (SourceMapArtifact) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "release", "filename").Unique(),
		index.Fields("project_id", "created_at"),
	}
}
