package meta

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
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
	Name                      *string
	Signal                    *string
	EvaluationEngine          *string
	Expression                *string
	For                       *string
	Severity                  *string
	Labels                    map[string]string
	Annotations               map[string]string
	Enabled                   *bool
	ChannelIDs                *[]int
	ActivateEvaluation        bool
	ResolveFiring             bool
	BuildResolveNotifications func(
		resolved []AlertRuleState,
		enabledChannelIDs []int,
		now time.Time,
	) ([]NotificationOutboxEnqueue, error)
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
	return s.AlertRuleByID(ctx, row.ID)
}

// AlertRuleByID loads the latest persisted alert rule by ID without project
// slug filtering. Internal evaluators use it after acquiring a lease so
// evaluation always runs against the current rule definition.
func (s *Store) AlertRuleByID(ctx context.Context, id int) (AlertRuleView, error) {
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
// result. Rule fields, evaluation authority, channel edges, rule state reset,
// lease epoch bump, and firing-event resolution either commit together or all
// roll back.
func (s *Store) UpdateAlertRule(
	ctx context.Context,
	projectSlug string,
	id int,
	input AlertRuleUpdate,
) (AlertRuleView, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AlertRuleView{}, fmt.Errorf("begin alert rule update: %w", err)
	}
	defer tx.Rollback()

	selectQuery := `SELECT r.name, r.signal, r.evaluation_engine, r.expr, r."for", r.severity,
		r.labels, r.annotations, r.enabled
		FROM alert_rules AS r
		JOIN projects AS p ON p.id = r.project_id
		WHERE r.id = ? AND p.slug = ?`
	if s.dialect == dialect.Postgres {
		selectQuery += ` FOR UPDATE OF r`
	}
	var (
		name, signal, evalEngine, expr, forDur, severity string
		labelsRaw, annotationsRaw                        []byte
		enabled                                          bool
	)
	if err := tx.QueryRowContext(ctx, bind(s.dialect, selectQuery), id, projectSlug).Scan(
		&name, &signal, &evalEngine, &expr, &forDur, &severity, &labelsRaw, &annotationsRaw, &enabled,
	); errors.Is(err, sql.ErrNoRows) {
		return AlertRuleView{}, &NotFoundError{Label: alertrule.Label}
	} else if err != nil {
		return AlertRuleView{}, fmt.Errorf("load alert rule for update: %w", err)
	}

	oldExpr, oldSignal, oldEnabled := expr, signal, enabled
	if input.Name != nil {
		name = *input.Name
	}
	if input.Signal != nil {
		signal = *input.Signal
	}
	if input.Expression != nil {
		expr = *input.Expression
	}
	if input.For != nil {
		forDur = *input.For
	}
	if input.Severity != nil {
		severity = *input.Severity
	}
	if input.Labels != nil {
		encoded, marshalErr := json.Marshal(nonNilStringMap(input.Labels))
		if marshalErr != nil {
			return AlertRuleView{}, fmt.Errorf("encode alert rule labels: %w", marshalErr)
		}
		labelsRaw = encoded
	}
	if input.Annotations != nil {
		encoded, marshalErr := json.Marshal(nonNilStringMap(input.Annotations))
		if marshalErr != nil {
			return AlertRuleView{}, fmt.Errorf("encode alert rule annotations: %w", marshalErr)
		}
		annotationsRaw = encoded
	}
	if input.Enabled != nil {
		enabled = *input.Enabled
	}
	if input.EvaluationEngine != nil {
		evalEngine = *input.EvaluationEngine
	}
	if input.ActivateEvaluation {
		evalEngine = string(alertrule.EvaluationEngineNative)
	}

	resetState := input.ResolveFiring || expr != oldExpr || signal != oldSignal || enabled != oldEnabled
	if _, err := tx.ExecContext(
		ctx,
		bind(
			s.dialect,
			`UPDATE alert_rules SET name = ?, signal = ?, evaluation_engine = ?, expr = ?, "for" = ?,
				severity = ?, labels = ?, annotations = ?, enabled = ?, updated_at = CURRENT_TIMESTAMP
			 WHERE id = ?`,
		),
		name, signal, evalEngine, expr, forDur, severity, string(labelsRaw), string(annotationsRaw), enabled, id,
	); err != nil {
		return AlertRuleView{}, fmt.Errorf("update alert rule: %w", err)
	}
	if input.ChannelIDs != nil {
		if _, err := tx.ExecContext(
			ctx,
			bind(s.dialect, `DELETE FROM alert_rule_channels WHERE alert_rule_id = ?`),
			id,
		); err != nil {
			return AlertRuleView{}, fmt.Errorf("clear alert rule channels: %w", err)
		}
		seen := make(map[int]struct{}, len(*input.ChannelIDs))
		for _, channelID := range *input.ChannelIDs {
			if _, dup := seen[channelID]; dup {
				continue
			}
			seen[channelID] = struct{}{}
			if _, err := tx.ExecContext(
				ctx,
				bind(
					s.dialect,
					`INSERT INTO alert_rule_channels(alert_rule_id, notification_channel_id) VALUES (?, ?)`,
				),
				id,
				channelID,
			); err != nil {
				return AlertRuleView{}, fmt.Errorf("link alert rule channel %d: %w", channelID, err)
			}
		}
	}
	if resetState {
		resolvedAt := time.Now().UTC()
		// Advance the fencing epoch and soft-expire any active lease so an
		// in-flight evaluation of the old expression cannot commit back stale
		// state after this update commits (locked after alert_rules to match
		// CommitNativeAlertEvaluation lock order).
		if _, err := tx.ExecContext(
			ctx,
			s.alertClockSQL(
				`UPDATE alert_rule_leases SET owner_id = '', epoch = epoch + 1, expires_at = CURRENT_TIMESTAMP, updated_at = CURRENT_TIMESTAMP WHERE rule_id = ?`,
			),
			id,
		); err != nil {
			return AlertRuleView{}, fmt.Errorf("fence alert rule lease on update: %w", err)
		}
		if input.BuildResolveNotifications != nil {
			firingStates, err := loadFiringAlertStatesTx(ctx, tx, s.dialect, id)
			if err != nil {
				return AlertRuleView{}, err
			}
			if len(firingStates) != 0 {
				enabledChannels, err := loadEnabledAlertRuleChannelsTx(ctx, tx, s.dialect, id)
				if err != nil {
					return AlertRuleView{}, err
				}
				if len(enabledChannels) != 0 {
					notifications, buildErr := input.BuildResolveNotifications(
						firingStates,
						enabledChannels,
						resolvedAt,
					)
					if buildErr != nil {
						return AlertRuleView{}, fmt.Errorf("build rule update resolve notifications: %w", buildErr)
					}
					for _, notification := range notifications {
						if notification.RuleID == nil || *notification.RuleID != id ||
							notification.IdempotencyKey == "" ||
							notification.ProjectID <= 0 ||
							notification.ChannelID <= 0 ||
							!json.Valid(notification.Payload) {
							return AlertRuleView{}, errors.New("rule update resolve notification is invalid")
						}
						if _, err := tx.ExecContext(
							ctx,
							bind(
								s.dialect,
								`INSERT INTO notification_outbox(idempotency_key, project_id, rule_id, channel_id, payload, priority,
									status, available_at, created_at, updated_at)
								 VALUES (?, ?, ?, ?, ?, ?, 'pending', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)
								 ON CONFLICT(idempotency_key) DO NOTHING`,
							),
							notification.IdempotencyKey,
							notification.ProjectID,
							notification.RuleID,
							notification.ChannelID,
							string(notification.Payload),
							notification.Priority,
						); err != nil {
							return AlertRuleView{}, fmt.Errorf("enqueue rule update resolve notification: %w", err)
						}
					}
				}
			}
		}
		if _, err := tx.ExecContext(
			ctx,
			bind(s.dialect, `DELETE FROM alert_rule_states WHERE rule_id = ?`),
			id,
		); err != nil {
			return AlertRuleView{}, fmt.Errorf("reset alert rule states: %w", err)
		}
		if _, err := tx.ExecContext(
			ctx,
			bind(
				s.dialect,
				`UPDATE alert_events SET status = 'resolved', inhibited = FALSE, ends_at = ?, updated_at = CURRENT_TIMESTAMP
				 WHERE rule_id = ? AND status = 'firing'`,
			),
			resolvedAt,
			id,
		); err != nil {
			return AlertRuleView{}, fmt.Errorf("resolve alert rule events: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return AlertRuleView{}, fmt.Errorf("commit alert rule update: %w", err)
	}
	return s.AlertRule(ctx, projectSlug, id)
}

func (s *Store) DeleteAlertRule(ctx context.Context, projectSlug string, id int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin alert rule delete: %w", err)
	}
	defer tx.Rollback()
	var found int
	if err := tx.QueryRowContext(
		ctx,
		bind(
			s.dialect,
			`SELECT r.id FROM alert_rules AS r JOIN projects AS p ON p.id = r.project_id WHERE r.id = ? AND p.slug = ?`,
		),
		id,
		projectSlug,
	).Scan(&found); errors.Is(err, sql.ErrNoRows) {
		return &NotFoundError{Label: alertrule.Label}
	} else if err != nil {
		return fmt.Errorf("lookup alert rule for delete: %w", err)
	}
	if _, err := tx.ExecContext(ctx, bind(s.dialect, `DELETE FROM alert_events WHERE rule_id = ?`), id); err != nil {
		return fmt.Errorf("delete alert rule events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, bind(s.dialect, `DELETE FROM alert_rules WHERE id = ?`), id); err != nil {
		return fmt.Errorf("delete alert rule: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit alert rule delete: %w", err)
	}
	return nil
}

func loadFiringAlertStatesTx(
	ctx context.Context,
	tx *sql.Tx,
	dbDialect string,
	ruleID int,
) ([]AlertRuleState, error) {
	rows, err := tx.QueryContext(
		ctx,
		bind(
			dbDialect,
			`SELECT rule_id, fingerprint, status, pending_since, firing_since, last_evaluated_at,
				labels, annotations, value, last_notified_at, lease_epoch
			 FROM alert_rule_states
			 WHERE rule_id = ? AND status = 'firing'
			 ORDER BY fingerprint`,
		),
		ruleID,
	)
	if err != nil {
		return nil, fmt.Errorf("load firing alert states for resolve notification: %w", err)
	}
	defer rows.Close()
	states, err := scanAlertRuleStates(rows)
	if err != nil {
		return nil, fmt.Errorf("scan firing alert states for resolve notification: %w", err)
	}
	return states, nil
}

func loadEnabledAlertRuleChannelsTx(
	ctx context.Context,
	tx *sql.Tx,
	dbDialect string,
	ruleID int,
) ([]int, error) {
	rows, err := tx.QueryContext(
		ctx,
		bind(
			dbDialect,
			`SELECT c.id FROM alert_rule_channels AS rc
			 JOIN notification_channels AS c ON c.id = rc.notification_channel_id
			 WHERE rc.alert_rule_id = ? AND c.enabled = TRUE
			 ORDER BY c.id`,
		),
		ruleID,
	)
	if err != nil {
		return nil, fmt.Errorf("load rule notification channels for resolve: %w", err)
	}
	defer rows.Close()
	var enabledChannels []int
	for rows.Next() {
		var channelID int
		if err := rows.Scan(&channelID); err != nil {
			return nil, fmt.Errorf("scan rule notification channel for resolve: %w", err)
		}
		enabledChannels = append(enabledChannels, channelID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rule notification channels for resolve: %w", err)
	}
	return enabledChannels, nil
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
