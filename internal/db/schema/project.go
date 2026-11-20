package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
)

// Project is the metadata isolation unit.
type Project struct {
	ent.Schema
}

func (Project) Fields() []ent.Field {
	return []ent.Field{
		field.String("name").NotEmpty(),
		field.String("slug").NotEmpty().Unique(),
		field.Time("created_at").Default(UTCNow).Immutable(),
	}
}

func (Project) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("api_tokens", ProjectAPIToken.Type),
		edge.To("source_map_upload_tokens", SourceMapUploadToken.Type),
		edge.To("alert_rules", AlertRule.Type),
		edge.To("silences", Silence.Type),
		edge.To("inhibition_rules", InhibitionRule.Type),
		edge.To("issues", Issue.Type),
		edge.To("replay_sessions", ReplaySession.Type),
		edge.To("source_map_artifacts", SourceMapArtifact.Type),
		edge.To("alert_config", ProjectAlertConfig.Type).Unique(),
		edge.To("scrape_targets", ScrapeTarget.Type),
		edge.To("profile_metas", ProfileMeta.Type),
		edge.To("telemetry_inbox", TelemetryInbox.Type),
		edge.To("issue_lifecycle_events", IssueLifecycleEvent.Type),
	}
}
