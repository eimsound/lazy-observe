package meta

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	dbent "github.com/eimsound/lazy-observe/internal/db/ent"
	"github.com/eimsound/lazy-observe/internal/db/ent/alertevent"
	"github.com/eimsound/lazy-observe/internal/db/ent/alertrule"
	"github.com/eimsound/lazy-observe/internal/db/ent/inhibitionrule"
	"github.com/eimsound/lazy-observe/internal/db/ent/notificationchannel"
	"github.com/eimsound/lazy-observe/internal/db/ent/project"
	"github.com/eimsound/lazy-observe/internal/db/ent/projectalertconfig"
	"github.com/eimsound/lazy-observe/internal/db/ent/silence"
	"github.com/eimsound/lazy-observe/internal/outbound"
	"github.com/eimsound/lazy-observe/internal/secret"
)

type NotificationChannelView struct {
	ID        int
	Type      string
	Name      string
	Config    map[string]any
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

type NotificationChannelCreate struct {
	Type    string
	Name    string
	Config  map[string]any
	Enabled *bool
}

type NotificationChannelUpdate struct {
	Type    *string
	Name    *string
	Config  map[string]any
	Enabled *bool
}

type ProjectAlertConfigView struct {
	ProjectID                   int   `json:"project_id"`
	NewIssue                    bool  `json:"new_issue"`
	Regression                  bool  `json:"regression"`
	ErrorRateThresholdPerMinute int   `json:"error_rate_threshold_per_min"`
	ChannelIDs                  []int `json:"channel_ids"`
	Channels                    []NotificationChannelView
}

type ProjectAlertConfigUpdate struct {
	NewIssue                    bool
	Regression                  bool
	ErrorRateThresholdPerMinute int
	ChannelIDs                  []int
}

type SilenceView struct {
	ID          int
	ProjectID   int
	ProjectSlug string
	Matchers    []map[string]string
	StartsAt    time.Time
	EndsAt      time.Time
	Comment     string
	Creator     string
	CreatedAt   time.Time
}

type SilenceCreate struct {
	ProjectSlug string
	Matchers    []map[string]string
	StartsAt    time.Time
	EndsAt      time.Time
	Comment     string
	Creator     string
}

type SilenceUpdate struct {
	Matchers []map[string]string
	StartsAt *time.Time
	EndsAt   *time.Time
	Comment  *string
}

type InhibitionRuleView struct {
	ID             int                 `json:"id,omitempty"`
	ProjectID      int                 `json:"project_id,omitempty"`
	ProjectSlug    string              `json:"-"`
	SourceMatchers []map[string]string `json:"source_matchers,omitempty"`
	TargetMatchers []map[string]string `json:"target_matchers,omitempty"`
	EqualLabels    []string            `json:"equal_labels,omitempty"`
	Enabled        bool                `json:"enabled,omitempty"`
	Comment        string              `json:"comment,omitempty"`
	CreatedAt      time.Time           `json:"created_at,omitempty"`
	UpdatedAt      time.Time           `json:"updated_at,omitempty"`
}

type InhibitionRuleCreate struct {
	ProjectSlug    string
	SourceMatchers []map[string]string
	TargetMatchers []map[string]string
	EqualLabels    []string
	Enabled        *bool
	Comment        string
}

type InhibitionRuleUpdate struct {
	SourceMatchers []map[string]string
	TargetMatchers []map[string]string
	EqualLabels    []string
	Enabled        *bool
	Comment        *string
}

type AlertEventView struct {
	ID             int
	RuleID         int
	RuleName       string
	Fingerprint    string
	Status         string
	Labels         map[string]string
	Annotations    map[string]string
	StartsAt       time.Time
	EndsAt         *time.Time
	LastNotifiedAt *time.Time
	Inhibited      bool
	UpdatedAt      time.Time
}

type AlertEventCreate struct {
	RuleID      int
	Fingerprint string
	Status      string
	Labels      map[string]string
	Annotations map[string]string
	StartsAt    time.Time
	EndsAt      *time.Time
	Inhibited   bool
	UpdatedAt   *time.Time
}

type AlertEventInhibition struct {
	EventID   int
	Inhibited bool
}

const channelConfigCipherPrefix = "enc:v1:"

func (s *Store) ListNotificationChannels(ctx context.Context) ([]NotificationChannelView, error) {
	rows, err := s.client.NotificationChannel.Query().Order(orderAsc(notificationchannel.FieldID)).All(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]NotificationChannelView, 0, len(rows))
	for _, row := range rows {
		view, err := s.notificationChannelView(row, false)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *Store) NotificationChannel(ctx context.Context, id int) (NotificationChannelView, error) {
	row, err := s.client.NotificationChannel.Get(ctx, id)
	if err != nil {
		return NotificationChannelView{}, err
	}
	return s.notificationChannelView(row, true)
}

func (s *Store) CreateNotificationChannel(
	ctx context.Context,
	input NotificationChannelCreate,
) (NotificationChannelView, error) {
	sealed, err := sealChannelConfig(s.secretBox.Load(), input.Config)
	if err != nil {
		return NotificationChannelView{}, err
	}
	create := s.client.NotificationChannel.Create().
		SetType(notificationchannel.Type(input.Type)).
		SetName(input.Name).
		SetConfig(sealed)
	if input.Enabled != nil {
		create.SetEnabled(*input.Enabled)
	}
	row, err := create.Save(ctx)
	if err != nil {
		return NotificationChannelView{}, err
	}
	return s.notificationChannelView(row, false)
}

func (s *Store) UpdateNotificationChannel(
	ctx context.Context,
	id int,
	input NotificationChannelUpdate,
) (NotificationChannelView, error) {
	update := s.client.NotificationChannel.UpdateOneID(id)
	if input.Type != nil {
		update.SetType(notificationchannel.Type(*input.Type))
	}
	if input.Name != nil {
		update.SetName(*input.Name)
	}
	if input.Config != nil {
		sealed, err := sealChannelConfig(s.secretBox.Load(), input.Config)
		if err != nil {
			return NotificationChannelView{}, err
		}
		update.SetConfig(sealed)
	}
	if input.Enabled != nil {
		update.SetEnabled(*input.Enabled)
	}
	row, err := update.Save(ctx)
	if err != nil {
		return NotificationChannelView{}, err
	}
	return s.notificationChannelView(row, false)
}

func (s *Store) DeleteNotificationChannel(ctx context.Context, id int) error {
	return s.client.NotificationChannel.DeleteOneID(id).Exec(ctx)
}

func (s *Store) notificationChannelView(row *dbent.NotificationChannel, strict bool) (NotificationChannelView, error) {
	cfg, err := openChannelConfig(s.secretBox.Load(), row.Config, strict, row.ID)
	if err != nil {
		return NotificationChannelView{}, err
	}
	return NotificationChannelView{
		ID:        row.ID,
		Type:      string(row.Type),
		Name:      row.Name,
		Config:    cfg,
		Enabled:   row.Enabled,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}, nil
}

func isSensitiveChannelKey(key string) bool {
	lower := strings.ToLower(strings.TrimSpace(key))
	if lower == "url" {
		return true
	}
	for _, marker := range []string{"secret", "password", "passwd", "token", "key", "auth", "credential", "cookie"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func sealChannelConfig(box *secret.Box, cfg map[string]any) (map[string]any, error) {
	if cfg == nil {
		return nil, nil
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if k == "headers" {
			if headers, ok := v.(map[string]any); ok {
				sealedHeaders := make(map[string]any, len(headers))
				for hk, hv := range headers {
					sealedVal, err := sealChannelStringValue(box, hv)
					if err != nil {
						return nil, fmt.Errorf("encrypt notification channel header %q: %w", hk, err)
					}
					sealedHeaders[hk] = sealedVal
				}
				out[k] = sealedHeaders
				continue
			}
		}
		if isSensitiveChannelKey(k) {
			sealedVal, err := sealChannelStringValue(box, v)
			if err != nil {
				return nil, fmt.Errorf("encrypt notification channel field %q: %w", k, err)
			}
			out[k] = sealedVal
			continue
		}
		out[k] = v
	}
	return out, nil
}

func sealChannelStringValue(box *secret.Box, value any) (any, error) {
	if box == nil {
		return value, nil
	}
	str, ok := value.(string)
	if !ok || str == "" || strings.HasPrefix(str, channelConfigCipherPrefix) {
		return value, nil
	}
	sealed, err := box.Encrypt(str)
	if err != nil {
		return nil, err
	}
	return channelConfigCipherPrefix + sealed, nil
}

func openChannelConfig(box *secret.Box, cfg map[string]any, strict bool, channelID int) (map[string]any, error) {
	if cfg == nil {
		return nil, nil
	}
	out := make(map[string]any, len(cfg))
	for k, v := range cfg {
		if k == "headers" {
			if headers, ok := v.(map[string]any); ok {
				openedHeaders := make(map[string]any, len(headers))
				for hk, hv := range headers {
					openedVal, err := openChannelStringValue(box, hv, strict, channelID, "headers."+hk)
					if err != nil {
						return nil, err
					}
					openedHeaders[hk] = openedVal
				}
				out[k] = openedHeaders
				continue
			}
		}
		openedVal, err := openChannelStringValue(box, v, strict, channelID, k)
		if err != nil {
			return nil, err
		}
		out[k] = openedVal
	}
	return out, nil
}

func openChannelStringValue(box *secret.Box, value any, strict bool, channelID int, field string) (any, error) {
	str, ok := value.(string)
	if !ok || !strings.HasPrefix(str, channelConfigCipherPrefix) {
		return value, nil
	}
	if box == nil {
		if strict {
			return nil, errors.New("notification channel encryption key is unavailable")
		}
		return outbound.RedactedValue, nil
	}
	plaintext, err := box.Decrypt(strings.TrimPrefix(str, channelConfigCipherPrefix))
	if err != nil {
		if strict {
			return nil, fmt.Errorf("decrypt notification channel %d field %q: %w", channelID, field, err)
		}
		slog.Warn("decrypt notification channel field failed", "channel_id", channelID, "field", field, "error", err)
		return outbound.RedactedValue, nil
	}
	return plaintext, nil
}

func (s *Store) ProjectAlertConfig(ctx context.Context, projectID int) (ProjectAlertConfigView, error) {
	if _, err := s.client.Project.Get(ctx, projectID); err != nil {
		return ProjectAlertConfigView{}, err
	}
	row, err := s.client.ProjectAlertConfig.Query().
		Where(projectalertconfig.ProjectIDEQ(projectID)).
		WithChannels().
		Only(ctx)
	if IsNotFound(err) {
		return ProjectAlertConfigView{
			ProjectID:  projectID,
			NewIssue:   true,
			Regression: true,
			ChannelIDs: []int{},
			Channels:   []NotificationChannelView{},
		}, nil
	}
	if err != nil {
		return ProjectAlertConfigView{}, err
	}
	return s.projectAlertConfigView(row)
}

func (s *Store) PutProjectAlertConfig(
	ctx context.Context,
	projectID int,
	input ProjectAlertConfigUpdate,
) (ProjectAlertConfigView, error) {
	if _, err := s.client.Project.Get(ctx, projectID); err != nil {
		return ProjectAlertConfigView{}, err
	}
	row, err := s.client.ProjectAlertConfig.Query().Where(projectalertconfig.ProjectIDEQ(projectID)).Only(ctx)
	if IsNotFound(err) {
		row, err = s.client.ProjectAlertConfig.Create().
			SetProjectID(projectID).
			SetNewIssue(input.NewIssue).
			SetRegression(input.Regression).
			SetErrorRateThresholdPerMin(input.ErrorRateThresholdPerMinute).
			AddChannelIDs(input.ChannelIDs...).
			Save(ctx)
	} else if err == nil {
		row, err = row.Update().
			SetNewIssue(input.NewIssue).
			SetRegression(input.Regression).
			SetErrorRateThresholdPerMin(input.ErrorRateThresholdPerMinute).
			ClearChannels().
			AddChannelIDs(input.ChannelIDs...).
			Save(ctx)
	}
	if err != nil {
		return ProjectAlertConfigView{}, err
	}
	row, err = s.client.ProjectAlertConfig.Query().
		Where(projectalertconfig.IDEQ(row.ID)).
		WithChannels().
		Only(ctx)
	if err != nil {
		return ProjectAlertConfigView{}, err
	}
	return s.projectAlertConfigView(row)
}

func (s *Store) projectAlertConfigView(row *dbent.ProjectAlertConfig) (ProjectAlertConfigView, error) {
	view := ProjectAlertConfigView{
		ProjectID:                   row.ProjectID,
		NewIssue:                    row.NewIssue,
		Regression:                  row.Regression,
		ErrorRateThresholdPerMinute: row.ErrorRateThresholdPerMin,
		ChannelIDs:                  make([]int, 0, len(row.Edges.Channels)),
		Channels:                    make([]NotificationChannelView, 0, len(row.Edges.Channels)),
	}
	for _, channel := range row.Edges.Channels {
		channelView, err := s.notificationChannelView(channel, false)
		if err != nil {
			return ProjectAlertConfigView{}, err
		}
		view.ChannelIDs = append(view.ChannelIDs, channel.ID)
		view.Channels = append(view.Channels, channelView)
	}
	return view, nil
}

func (s *Store) ProjectAlertPolicy(
	ctx context.Context,
	projectID int,
) (ProjectAlertConfigView, ProjectInfo, error) {
	row, err := s.client.Project.Get(ctx, projectID)
	if err != nil {
		return ProjectAlertConfigView{}, ProjectInfo{}, err
	}
	return s.projectAlertPolicy(ctx, row)
}

func (s *Store) ProjectAlertPolicyBySlug(
	ctx context.Context,
	projectSlug string,
) (ProjectAlertConfigView, ProjectInfo, error) {
	row, err := s.client.Project.Query().Where(project.SlugEQ(projectSlug)).Only(ctx)
	if err != nil {
		return ProjectAlertConfigView{}, ProjectInfo{}, err
	}
	return s.projectAlertPolicy(ctx, row)
}

func (s *Store) projectAlertPolicy(
	ctx context.Context,
	row *dbent.Project,
) (ProjectAlertConfigView, ProjectInfo, error) {
	config, err := s.ProjectAlertConfig(ctx, row.ID)
	if err != nil {
		return ProjectAlertConfigView{}, ProjectInfo{}, err
	}
	return config, ProjectInfo{ID: row.ID, Name: row.Name, Slug: row.Slug}, nil
}

func (s *Store) StormNotificationChannels(
	ctx context.Context,
	projectSlug string,
) ([]NotificationChannelView, ProjectInfo, error) {
	projectRow, err := s.client.Project.Query().Where(project.SlugEQ(projectSlug)).Only(ctx)
	if err != nil {
		return nil, ProjectInfo{}, err
	}
	rules, err := s.client.AlertRule.Query().
		Where(alertrule.EnabledEQ(true), alertrule.ProjectIDEQ(projectRow.ID)).
		WithChannels().
		All(ctx)
	if err != nil {
		return nil, ProjectInfo{}, err
	}
	channels := make(map[int]NotificationChannelView)
	for _, rule := range rules {
		for _, channel := range rule.Edges.Channels {
			if channel.Enabled {
				channelView, viewErr := s.notificationChannelView(channel, false)
				if viewErr != nil {
					return nil, ProjectInfo{}, viewErr
				}
				channels[channel.ID] = channelView
			}
		}
	}
	config, err := s.ProjectAlertConfig(ctx, projectRow.ID)
	if err != nil {
		return nil, ProjectInfo{}, err
	}
	for _, channel := range config.Channels {
		if channel.Enabled {
			channels[channel.ID] = channel
		}
	}
	views := make([]NotificationChannelView, 0, len(channels))
	for _, channel := range channels {
		views = append(views, channel)
	}
	return views, ProjectInfo{ID: projectRow.ID, Name: projectRow.Name, Slug: projectRow.Slug}, nil
}

func (s *Store) ListSilences(ctx context.Context, projectSlug string) ([]SilenceView, error) {
	rows, err := s.client.Silence.Query().
		Where(silence.HasProjectWith(project.SlugEQ(projectSlug))).
		WithProject().
		Order(orderAsc(silence.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]SilenceView, 0, len(rows))
	for _, row := range rows {
		view, err := silenceView(row)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *Store) CreateSilence(ctx context.Context, input SilenceCreate) (SilenceView, error) {
	projectRow, err := s.client.Project.Query().Where(project.SlugEQ(input.ProjectSlug)).Only(ctx)
	if err != nil {
		return SilenceView{}, err
	}
	row, err := s.client.Silence.Create().
		SetProjectID(projectRow.ID).
		SetMatchers(input.Matchers).
		SetStartsAt(input.StartsAt.UTC()).
		SetEndsAt(input.EndsAt.UTC()).
		SetComment(input.Comment).
		SetCreator(input.Creator).
		Save(ctx)
	if err != nil {
		return SilenceView{}, err
	}
	row.Edges.Project = projectRow
	return silenceView(row)
}

func (s *Store) UpdateSilence(
	ctx context.Context,
	projectSlug string,
	id int,
	input SilenceUpdate,
) (SilenceView, error) {
	row, err := s.client.Silence.Query().
		Where(silence.IDEQ(id), silence.HasProjectWith(project.SlugEQ(projectSlug))).
		WithProject().
		Only(ctx)
	if err != nil {
		return SilenceView{}, err
	}
	update := row.Update()
	if input.Matchers != nil {
		update.SetMatchers(input.Matchers)
	}
	if input.StartsAt != nil {
		update.SetStartsAt(input.StartsAt.UTC())
	}
	if input.EndsAt != nil {
		update.SetEndsAt(input.EndsAt.UTC())
	}
	if input.Comment != nil {
		update.SetComment(*input.Comment)
	}
	if _, err := update.Save(ctx); err != nil {
		return SilenceView{}, err
	}
	row, err = s.client.Silence.Query().
		Where(silence.IDEQ(id), silence.HasProjectWith(project.SlugEQ(projectSlug))).
		WithProject().
		Only(ctx)
	if err != nil {
		return SilenceView{}, err
	}
	return silenceView(row)
}

func (s *Store) DeleteSilence(ctx context.Context, projectSlug string, id int) error {
	row, err := s.client.Silence.Query().
		Where(silence.IDEQ(id), silence.HasProjectWith(project.SlugEQ(projectSlug))).
		Only(ctx)
	if err != nil {
		return err
	}
	return s.client.Silence.DeleteOne(row).Exec(ctx)
}

func silenceView(row *dbent.Silence) (SilenceView, error) {
	projectRow, err := row.Edges.ProjectOrErr()
	if err != nil {
		return SilenceView{}, err
	}
	return SilenceView{
		ID:          row.ID,
		ProjectID:   projectRow.ID,
		ProjectSlug: projectRow.Slug,
		Matchers:    row.Matchers,
		StartsAt:    row.StartsAt,
		EndsAt:      row.EndsAt,
		Comment:     row.Comment,
		Creator:     row.Creator,
		CreatedAt:   row.CreatedAt,
	}, nil
}

func (s *Store) ListInhibitionRules(ctx context.Context, projectSlug string) ([]InhibitionRuleView, error) {
	rows, err := s.client.InhibitionRule.Query().
		Where(inhibitionrule.HasProjectWith(project.SlugEQ(projectSlug))).
		WithProject().
		Order(orderAsc(inhibitionrule.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]InhibitionRuleView, 0, len(rows))
	for _, row := range rows {
		view, err := inhibitionRuleView(row)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *Store) CreateInhibitionRule(
	ctx context.Context,
	input InhibitionRuleCreate,
) (InhibitionRuleView, error) {
	projectRow, err := s.client.Project.Query().Where(project.SlugEQ(input.ProjectSlug)).Only(ctx)
	if err != nil {
		return InhibitionRuleView{}, err
	}
	create := s.client.InhibitionRule.Create().
		SetProjectID(projectRow.ID).
		SetSourceMatchers(input.SourceMatchers).
		SetTargetMatchers(input.TargetMatchers).
		SetEqualLabels(input.EqualLabels).
		SetComment(input.Comment)
	if input.Enabled != nil {
		create.SetEnabled(*input.Enabled)
	}
	row, err := create.Save(ctx)
	if err != nil {
		return InhibitionRuleView{}, err
	}
	row.Edges.Project = projectRow
	return inhibitionRuleView(row)
}

func (s *Store) UpdateInhibitionRule(
	ctx context.Context,
	projectSlug string,
	id int,
	input InhibitionRuleUpdate,
) (InhibitionRuleView, error) {
	row, err := s.client.InhibitionRule.Query().
		Where(inhibitionrule.IDEQ(id), inhibitionrule.HasProjectWith(project.SlugEQ(projectSlug))).
		WithProject().
		Only(ctx)
	if err != nil {
		return InhibitionRuleView{}, err
	}
	update := row.Update()
	if input.SourceMatchers != nil {
		update.SetSourceMatchers(input.SourceMatchers)
	}
	if input.TargetMatchers != nil {
		update.SetTargetMatchers(input.TargetMatchers)
	}
	if input.EqualLabels != nil {
		update.SetEqualLabels(input.EqualLabels)
	}
	if input.Enabled != nil {
		update.SetEnabled(*input.Enabled)
	}
	if input.Comment != nil {
		update.SetComment(*input.Comment)
	}
	if _, err := update.Save(ctx); err != nil {
		return InhibitionRuleView{}, err
	}
	row, err = s.client.InhibitionRule.Query().
		Where(inhibitionrule.IDEQ(id), inhibitionrule.HasProjectWith(project.SlugEQ(projectSlug))).
		WithProject().
		Only(ctx)
	if err != nil {
		return InhibitionRuleView{}, err
	}
	return inhibitionRuleView(row)
}

func (s *Store) DeleteInhibitionRule(ctx context.Context, projectSlug string, id int) (ProjectInfo, error) {
	row, err := s.client.InhibitionRule.Query().
		Where(inhibitionrule.IDEQ(id), inhibitionrule.HasProjectWith(project.SlugEQ(projectSlug))).
		WithProject().
		Only(ctx)
	if err != nil {
		return ProjectInfo{}, err
	}
	projectRow, err := row.Edges.ProjectOrErr()
	if err != nil {
		return ProjectInfo{}, err
	}
	if err := s.client.InhibitionRule.DeleteOne(row).Exec(ctx); err != nil {
		return ProjectInfo{}, err
	}
	return ProjectInfo{ID: projectRow.ID, Name: projectRow.Name, Slug: projectRow.Slug}, nil
}

func inhibitionRuleView(row *dbent.InhibitionRule) (InhibitionRuleView, error) {
	projectRow, err := row.Edges.ProjectOrErr()
	if err != nil {
		return InhibitionRuleView{}, err
	}
	return InhibitionRuleView{
		ID:             row.ID,
		ProjectID:      row.ProjectID,
		ProjectSlug:    projectRow.Slug,
		SourceMatchers: row.SourceMatchers,
		TargetMatchers: row.TargetMatchers,
		EqualLabels:    row.EqualLabels,
		Enabled:        row.Enabled,
		Comment:        row.Comment,
		CreatedAt:      row.CreatedAt,
		UpdatedAt:      row.UpdatedAt,
	}, nil
}

func (s *Store) ListAlertEvents(
	ctx context.Context,
	projectSlug, status string,
	inhibited *bool,
) ([]AlertEventView, error) {
	query := s.client.AlertEvent.Query().
		Where(alertevent.HasRuleWith(alertrule.HasProjectWith(project.SlugEQ(projectSlug)))).
		WithRule().
		Order(orderDesc(alertevent.FieldUpdatedAt))
	if status != "" {
		query.Where(alertevent.StatusEQ(alertevent.Status(status)))
	}
	if inhibited != nil {
		query.Where(alertevent.InhibitedEQ(*inhibited))
	}
	rows, err := query.All(ctx)
	if err != nil {
		return nil, err
	}
	views := make([]AlertEventView, 0, len(rows))
	for _, row := range rows {
		view, err := alertEventView(row)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func (s *Store) CreateAlertEvent(ctx context.Context, input AlertEventCreate) (AlertEventView, error) {
	create := s.client.AlertEvent.Create().
		SetRuleID(input.RuleID).
		SetFingerprint(input.Fingerprint).
		SetStatus(alertevent.Status(input.Status)).
		SetLabels(input.Labels).
		SetAnnotations(input.Annotations).
		SetStartsAt(input.StartsAt).
		SetInhibited(input.Inhibited)
	if input.EndsAt != nil {
		create.SetEndsAt(*input.EndsAt)
	}
	if input.UpdatedAt != nil {
		create.SetUpdatedAt(*input.UpdatedAt)
	}
	row, err := create.Save(ctx)
	if err != nil {
		return AlertEventView{}, err
	}
	row, err = s.client.AlertEvent.Query().Where(alertevent.IDEQ(row.ID)).WithRule().Only(ctx)
	if err != nil {
		return AlertEventView{}, err
	}
	return alertEventView(row)
}

func (s *Store) SetAlertEventInhibition(ctx context.Context, updates []AlertEventInhibition) error {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return fmt.Errorf("begin alert event inhibition update: %w", err)
	}
	defer tx.Rollback()
	for _, update := range updates {
		if _, err := tx.AlertEvent.UpdateOneID(update.EventID).SetInhibited(update.Inhibited).Save(ctx); err != nil {
			return fmt.Errorf("update alert event inhibition: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit alert event inhibition update: %w", err)
	}
	return nil
}

func alertEventView(row *dbent.AlertEvent) (AlertEventView, error) {
	rule, err := row.Edges.RuleOrErr()
	if err != nil {
		return AlertEventView{}, err
	}
	return AlertEventView{
		ID:             row.ID,
		RuleID:         row.RuleID,
		RuleName:       rule.Name,
		Fingerprint:    row.Fingerprint,
		Status:         string(row.Status),
		Labels:         row.Labels,
		Annotations:    row.Annotations,
		StartsAt:       row.StartsAt,
		EndsAt:         row.EndsAt,
		LastNotifiedAt: row.LastNotifiedAt,
		Inhibited:      row.Inhibited,
		UpdatedAt:      row.UpdatedAt,
	}, nil
}
