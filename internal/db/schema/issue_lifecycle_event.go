package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	entschema "entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// IssueLifecycleEvent durably records committed Issue lifecycle transitions
// (new_issue, regression) inside the same metadata transaction as the Inbox
// completion receipt so downstream alert/AI delivery survives crashes.
type IssueLifecycleEvent struct{ ent.Schema }

func (IssueLifecycleEvent) Annotations() []entschema.Annotation {
	return []entschema.Annotation{entsql.Annotation{Table: "issue_lifecycle_events"}}
}

func (IssueLifecycleEvent) Fields() []ent.Field {
	return []ent.Field{
		field.Int("project_id"),
		field.Int("issue_id"),
		field.Int64("inbox_item_id").NonNegative(),
		field.Enum("kind").Values("new_issue", "regression"),
		field.String("title"),
		field.String("level"),
		field.Time("seen_at"),
		field.Enum("status").Values("pending", "processing", "dispatched", "failed").Default("pending"),
		field.Time("available_at"),
		field.String("claim_owner").Default(""),
		field.Time("claim_until").Optional().Nillable(),
		field.Int64("claim_epoch").Default(0).NonNegative(),
		field.Int("attempts").Default(0).NonNegative(),
		field.String("last_error").Default(""),
		field.Time("dispatched_at").Optional().Nillable(),
		field.Time("created_at").Default(UTCNow).Immutable(),
		field.Time("updated_at").Default(UTCNow).UpdateDefault(UTCNow),
	}
}

func (IssueLifecycleEvent) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).Ref("issue_lifecycle_events").Field("project_id").Unique().Required(),
	}
}

func (IssueLifecycleEvent) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("status", "available_at", "id"),
		index.Fields("claim_until"),
		index.Fields("project_id", "created_at"),
		index.Fields("dispatched_at"),
	}
}
