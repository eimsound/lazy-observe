package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	entschema "entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// ProfileMeta indexes a pprof payload stored in blob storage.
type ProfileMeta struct {
	ent.Schema
}

func (ProfileMeta) Annotations() []entschema.Annotation {
	return []entschema.Annotation{entsql.Annotation{Table: "profile_metas"}}
}

func (ProfileMeta) Fields() []ent.Field {
	return []ent.Field{
		field.String("upload_id").NotEmpty().Immutable(),
		field.String("payload_sha256").NotEmpty().Immutable(),
		field.String("service").NotEmpty(),
		field.Enum("type").Values("cpu", "heap", "goroutine"),
		field.Time("start").Immutable(),
		field.Time("end").Immutable(),
		field.Int64("size").NonNegative(),
		field.String("blob_key").NotEmpty().Unique(),
		field.JSON("labels", map[string]string{}).Default(func() map[string]string { return map[string]string{} }),
		field.Int("project_id"),
	}
}

func (ProfileMeta) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).Ref("profile_metas").Field("project_id").Unique().Required(),
	}
}

func (ProfileMeta) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "upload_id").Unique(),
		index.Fields("project_id", "service", "type", "start"),
		index.Fields("project_id", "end"),
	}
}
