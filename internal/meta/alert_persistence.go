package meta

import (
	"context"
	"fmt"
	"time"

	dbent "github.com/eimsound/lazy-observe/internal/db/ent"
	"github.com/eimsound/lazy-observe/internal/db/ent/alertevent"
	"github.com/eimsound/lazy-observe/internal/db/ent/alertrule"
	"github.com/eimsound/lazy-observe/internal/db/ent/inhibitionrule"
	"github.com/eimsound/lazy-observe/internal/db/ent/project"
	"github.com/eimsound/lazy-observe/internal/db/ent/silence"
)

// AlertRuleView is the metadata-owned rule representation used by callers.
// Ent edges and generated enums stay inside this package.
type AlertRuleView struct {
	ID                int
	Project           ProjectInfo
	Name              string
	Signal            string
	EvaluationEngine  string
	Expression        string
	For               string
	Severity          string
	Labels            map[string]string
	Annotations       map[string]string
	Enabled           bool
	ChannelIDs        []int
	EnabledChannelIDs []int
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type AlertRuleCreate struct {
	ProjectSlug string
	Name        string
	Signal      string
	Expression  string
	For         string
	Severity    string
	Labels      map[string]string
	Annotations map[string]string
	Enabled     *bool
	ChannelIDs  []int
}

type AlertRuleUpdate struct {
	Name               *string
	Signal             *string
	EvaluationEngine   *string
	Expression         *string
	For                *string
	Severity           *string
	Labels             map[string]string
	Annotations        map[string]string
	Enabled            *bool
	ChannelIDs         *[]int
	ActivateEvaluation bool
	ResolveFiring      bool
}

type AlertInhibitionView struct {
	SourceMatchers []map[string]string
	TargetMatchers []map[string]string
	EqualLabels    []string
}

type AlertEventSourceView struct {
	ID          int
	Fingerprint string
	Labels      map[string]string
	Annotations map[string]string
	StartsAt    time.Time
	Inhibited   bool
}

type AlertPolicySnapshot struct {
	Silences        [][]map[string]string
	InhibitionRules []AlertInhibitionView
	FiringEvents    []AlertEventSourceView
}

func (s *Store) ListAlertRules(ctx context.Context, projectSlug string) ([]AlertRuleView, error) {
	rows, err := s.client.AlertRule.Query().
		Where(alertrule.HasProjectWith(project.SlugEQ(projectSlug))).
		WithProject().
		WithChannels().
		Order(orderAsc(alertrule.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]AlertRuleView, 0, len(rows))
	for _, row := range rows {
		view, err := alertRuleView(row)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *Store) AlertEvaluationRules(ctx context.Context) ([]AlertRuleView, error) {
	rows, err := s.client.AlertRule.Query().
		Where(alertrule.EnabledEQ(true)).
		WithProject().
		WithChannels().
		Order(orderAsc(alertrule.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]AlertRuleView, 0, len(rows))
	for _, row := range rows {
		view, err := alertRuleView(row)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *Store) AlertPolicySnapshot(
	ctx context.Context,
	projectID int,
	now time.Time,
) (AlertPolicySnapshot, error) {
	now = now.UTC()
	silenceRows, err := s.client.Silence.Query().
		Where(silence.ProjectIDEQ(projectID), silence.StartsAtLTE(now), silence.EndsAtGT(now)).
		All(ctx)
	if err != nil {
		return AlertPolicySnapshot{}, err
	}
	inhibitionRows, err := s.client.InhibitionRule.Query().
		Where(inhibitionrule.ProjectIDEQ(projectID), inhibitionrule.EnabledEQ(true)).
		All(ctx)
	if err != nil {
		return AlertPolicySnapshot{}, err
	}
	eventRows, err := s.client.AlertEvent.Query().
		Where(
			alertevent.StatusEQ(alertevent.StatusFiring),
			alertevent.HasRuleWith(alertrule.HasProjectWith(project.IDEQ(projectID))),
		).
		All(ctx)
	if err != nil {
		return AlertPolicySnapshot{}, err
	}
	snapshot := AlertPolicySnapshot{
		Silences:        make([][]map[string]string, 0, len(silenceRows)),
		InhibitionRules: make([]AlertInhibitionView, 0, len(inhibitionRows)),
		FiringEvents:    make([]AlertEventSourceView, 0, len(eventRows)),
	}
	for _, row := range silenceRows {
		snapshot.Silences = append(snapshot.Silences, row.Matchers)
	}
	for _, row := range inhibitionRows {
		snapshot.InhibitionRules = append(snapshot.InhibitionRules, AlertInhibitionView{
			SourceMatchers: row.SourceMatchers,
			TargetMatchers: row.TargetMatchers,
			EqualLabels:    row.EqualLabels,
		})
	}
	for _, row := range eventRows {
		snapshot.FiringEvents = append(snapshot.FiringEvents, AlertEventSourceView{
			ID:          row.ID,
			Fingerprint: row.Fingerprint,
			Labels:      row.Labels,
			Annotations: row.Annotations,
			StartsAt:    row.StartsAt,
			Inhibited:   row.Inhibited,
		})
	}
	return snapshot, nil
}

func (s *Store) CreateAlertRule(ctx context.Context, input AlertRuleCreate) (AlertRuleView, error) {
	projectRow, err := s.client.Project.Query().Where(project.SlugEQ(input.ProjectSlug)).Only(ctx)
	if err != nil {
		return AlertRuleView{}, err
	}
	create := s.client.AlertRule.Create().
		SetProjectID(projectRow.ID).
		SetName(input.Name).
		SetSignal(alertrule.Signal(input.Signal)).
		SetEvaluationEngine(alertrule.EvaluationEngineNative).
		SetExpr(input.Expression).
		SetFor(input.For).
		SetSeverity(input.Severity).
		SetLabels(nonNilStringMap(input.Labels)).
		SetAnnotations(nonNilStringMap(input.Annotations)).
		AddChannelIDs(input.ChannelIDs...)
	if input.Enabled != nil {
		create.SetEnabled(*input.Enabled)
	}
	row, err := create.Save(ctx)
	if err != nil {
		return AlertRuleView{}, err
	}
	return s.alertRuleByID(ctx, row.ID)
}

func (s *Store) AlertRule(ctx context.Context, projectSlug string, id int) (AlertRuleView, error) {
	row, err := s.client.AlertRule.Query().
		Where(alertrule.IDEQ(id), alertrule.HasProjectWith(project.SlugEQ(projectSlug))).
		WithProject().
		WithChannels().
		Only(ctx)
	if err != nil {
		return AlertRuleView{}, err
	}
	return alertRuleView(row)
}

// UpdateAlertRule applies the caller's domain decision as one persistence
// result. Rule fields, evaluation authority, channel edges, and firing-event
// resolution either commit together or all roll back.
func (s *Store) UpdateAlertRule(
	ctx context.Context,
	projectSlug string,
	id int,
	input AlertRuleUpdate,
) (AlertRuleView, error) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return AlertRuleView{}, fmt.Errorf("begin alert rule update: %w", err)
	}
	defer tx.Rollback()

	row, err := tx.AlertRule.Query().
		Where(alertrule.IDEQ(id), alertrule.HasProjectWith(project.SlugEQ(projectSlug))).
		Only(ctx)
	if err != nil {
		return AlertRuleView{}, err
	}
	update := row.Update()
	if input.Name != nil {
		update.SetName(*input.Name)
	}
	if input.Signal != nil {
		update.SetSignal(alertrule.Signal(*input.Signal))
	}
	if input.Expression != nil {
		update.SetExpr(*input.Expression)
	}
	if input.For != nil {
		update.SetFor(*input.For)
	}
	if input.Severity != nil {
		update.SetSeverity(*input.Severity)
	}
	if input.Labels != nil {
		update.SetLabels(input.Labels)
	}
	if input.Annotations != nil {
		update.SetAnnotations(input.Annotations)
	}
	if input.Enabled != nil {
		update.SetEnabled(*input.Enabled)
	}
	if input.ChannelIDs != nil {
		update.ClearChannels().AddChannelIDs((*input.ChannelIDs)...)
	}
	if input.EvaluationEngine != nil {
		update.SetEvaluationEngine(alertrule.EvaluationEngine(*input.EvaluationEngine))
	}
	if input.ActivateEvaluation {
		update.SetEvaluationEngine(alertrule.EvaluationEngineNative)
	}
	if input.ResolveFiring {
		if _, err := tx.AlertEvent.Update().
			Where(alertevent.RuleIDEQ(id), alertevent.StatusEQ(alertevent.StatusFiring)).
			SetStatus(alertevent.StatusResolved).
			SetInhibited(false).
			SetEndsAt(time.Now().UTC()).
			Save(ctx); err != nil {
			return AlertRuleView{}, fmt.Errorf("resolve alert rule events: %w", err)
		}
	}
	if _, err := update.Save(ctx); err != nil {
		return AlertRuleView{}, fmt.Errorf("update alert rule: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return AlertRuleView{}, fmt.Errorf("commit alert rule update: %w", err)
	}
	return s.AlertRule(ctx, projectSlug, id)
}

func (s *Store) DeleteAlertRule(ctx context.Context, projectSlug string, id int) error {
	row, err := s.client.AlertRule.Query().
		Where(alertrule.IDEQ(id), alertrule.HasProjectWith(project.SlugEQ(projectSlug))).
		Only(ctx)
	if err != nil {
		return err
	}
	return s.client.AlertRule.DeleteOne(row).Exec(ctx)
}

func (s *Store) alertRuleByID(ctx context.Context, id int) (AlertRuleView, error) {
	row, err := s.client.AlertRule.Query().
		Where(alertrule.IDEQ(id)).
		WithProject().
		WithChannels().
		Only(ctx)
	if err != nil {
		return AlertRuleView{}, err
	}
	return alertRuleView(row)
}

func alertRuleView(row *dbent.AlertRule) (AlertRuleView, error) {
	projectRow, err := row.Edges.ProjectOrErr()
	if err != nil {
		return AlertRuleView{}, err
	}
	channelIDs := make([]int, 0, len(row.Edges.Channels))
	enabledChannelIDs := make([]int, 0, len(row.Edges.Channels))
	for _, channel := range row.Edges.Channels {
		channelIDs = append(channelIDs, channel.ID)
		if channel.Enabled {
			enabledChannelIDs = append(enabledChannelIDs, channel.ID)
		}
	}
	return AlertRuleView{
		ID:                row.ID,
		Project:           ProjectInfo{ID: projectRow.ID, Name: projectRow.Name, Slug: projectRow.Slug},
		Name:              row.Name,
		Signal:            string(row.Signal),
		EvaluationEngine:  string(row.EvaluationEngine),
		Expression:        row.Expr,
		For:               row.For,
		Severity:          row.Severity,
		Labels:            row.Labels,
		Annotations:       row.Annotations,
		Enabled:           row.Enabled,
		ChannelIDs:        channelIDs,
		EnabledChannelIDs: enabledChannelIDs,
		CreatedAt:         row.CreatedAt,
		UpdatedAt:         row.UpdatedAt,
	}, nil
}
