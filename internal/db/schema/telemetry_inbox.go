package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	entschema "entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

// TelemetryInbox persists platform-owned asynchronous error work before the
// Gateway acknowledges it. Completed rows retain identity and outcome facts
// while clearing the payload bytes.
type TelemetryInbox struct{ ent.Schema }

func (TelemetryInbox) Annotations() []entschema.Annotation {
	return []entschema.Annotation{entsql.Annotation{Table: "telemetry_inbox"}}
}

func (TelemetryInbox) Fields() []ent.Field {
	return []ent.Field{
		field.String("event_id").NotEmpty(),
		field.String("canonical_digest").NotEmpty(),
		field.Bytes("payload").Optional().Nillable(),
		field.Int64("payload_bytes").NonNegative(),
		field.Time("occurred_at"),
		field.Time("accepted_at"),
		field.JSON("source_map_snapshot", map[string]string{}),
		field.Enum("status").Values("pending", "processing", "completed", "terminal").Default("pending"),
		field.Time("available_at"),
		field.String("claim_owner").Default(""),
		field.Time("claim_until").Optional().Nillable(),
		field.Int("attempts").Default(0).NonNegative(),
		field.String("last_error").Default(""),
		field.String("outcome").Default(""),
		field.Time("processed_at").Optional().Nillable(),
		field.Time("completed_at").Optional().Nillable(),
		field.Time("created_at").Default(time.Now).Immutable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Int("project_id"),
	}
}

func (TelemetryInbox) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).Ref("telemetry_inbox").Field("project_id").Unique().Required(),
	}
}

func (TelemetryInbox) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("project_id", "event_id").Unique(),
		index.Fields("status", "available_at", "id"),
		index.Fields("claim_until"),
		index.Fields("completed_at"),
	}
}
