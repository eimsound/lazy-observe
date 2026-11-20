package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	entschema "entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

// TelemetryInboxState is the singleton fence and durable aggregate for Inbox
// capacity and low-cardinality diagnostics. Domain work remains in errortrack;
// this schema only records transactional counters.
type TelemetryInboxState struct{ ent.Schema }

func (TelemetryInboxState) Annotations() []entschema.Annotation {
	return []entschema.Annotation{entsql.Annotation{Table: "telemetry_inbox_state"}}
}

func (TelemetryInboxState) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("active_items").Default(0).NonNegative(),
		field.Int64("active_bytes").Default(0).NonNegative(),
		field.Int64("rejections_total").Default(0).NonNegative(),
		field.Int64("retries_total").Default(0).NonNegative(),
		field.Int64("terminal_failures_total").Default(0).NonNegative(),
		field.Int64("drops_total").Default(0).NonNegative(),
		field.Time("updated_at").Default(UTCNow).UpdateDefault(UTCNow),
	}
}
