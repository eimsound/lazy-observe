package meta

import (
	"context"
	"time"

	dbent "github.com/eimsound/lazy-observe/internal/db/ent"
	"github.com/eimsound/lazy-observe/internal/db/ent/project"
	"github.com/eimsound/lazy-observe/internal/db/ent/scrapetarget"
)

type ScrapeTargetView struct {
	ID        int
	Project   ProjectInfo
	Name      string
	URL       string
	Interval  string
	Labels    map[string]string
	Enabled   bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

type ScrapeTargetCreate struct {
	ProjectSlug string
	Name        string
	URL         string
	Interval    string
	Labels      map[string]string
	Enabled     *bool
}

type ScrapeTargetUpdate struct {
	Name     *string
	URL      *string
	Interval *string
	Labels   map[string]string
	Enabled  *bool
}

func (s *Store) ListScrapeTargets(ctx context.Context, projectSlug string) ([]ScrapeTargetView, error) {
	rows, err := s.client.ScrapeTarget.Query().
		Where(scrapetarget.HasProjectWith(project.SlugEQ(projectSlug))).
		WithProject().
		Order(orderAsc(scrapetarget.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return scrapeTargetViews(rows)
}

func (s *Store) ScrapeTarget(ctx context.Context, projectSlug string, id int) (ScrapeTargetView, error) {
	row, err := s.client.ScrapeTarget.Query().
		Where(scrapetarget.IDEQ(id), scrapetarget.HasProjectWith(project.SlugEQ(projectSlug))).
		WithProject().
		Only(ctx)
	if err != nil {
		return ScrapeTargetView{}, err
	}
	return scrapeTargetView(row)
}

func (s *Store) CreateScrapeTarget(ctx context.Context, input ScrapeTargetCreate) (ScrapeTargetView, error) {
	projectRow, err := s.client.Project.Query().Where(project.SlugEQ(input.ProjectSlug)).Only(ctx)
	if err != nil {
		return ScrapeTargetView{}, err
	}
	create := s.client.ScrapeTarget.Create().
		SetProjectID(projectRow.ID).
		SetName(input.Name).
		SetURL(input.URL).
		SetLabels(input.Labels)
	if input.Interval != "" {
		create.SetInterval(input.Interval)
	}
	if input.Enabled != nil {
		create.SetEnabled(*input.Enabled)
	}
	row, err := create.Save(ctx)
	if err != nil {
		return ScrapeTargetView{}, err
	}
	row.Edges.Project = projectRow
	return scrapeTargetView(row)
}

func (s *Store) UpdateScrapeTarget(
	ctx context.Context,
	projectSlug string,
	id int,
	input ScrapeTargetUpdate,
) (ScrapeTargetView, error) {
	if _, err := s.ScrapeTarget(ctx, projectSlug, id); err != nil {
		return ScrapeTargetView{}, err
	}
	update := s.client.ScrapeTarget.UpdateOneID(id)
	if input.Name != nil {
		update.SetName(*input.Name)
	}
	if input.URL != nil {
		update.SetURL(*input.URL)
	}
	if input.Interval != nil {
		update.SetInterval(*input.Interval)
	}
	if input.Labels != nil {
		update.SetLabels(input.Labels)
	}
	if input.Enabled != nil {
		update.SetEnabled(*input.Enabled)
	}
	if _, err := update.Save(ctx); err != nil {
		return ScrapeTargetView{}, err
	}
	return s.ScrapeTarget(ctx, projectSlug, id)
}

func (s *Store) DeleteScrapeTarget(ctx context.Context, projectSlug string, id int) error {
	if _, err := s.ScrapeTarget(ctx, projectSlug, id); err != nil {
		return err
	}
	return s.client.ScrapeTarget.DeleteOneID(id).Exec(ctx)
}

func (s *Store) ScrapeConfigSnapshot(ctx context.Context) ([]ScrapeTargetView, error) {
	rows, err := s.client.ScrapeTarget.Query().
		Where(scrapetarget.EnabledEQ(true)).
		WithProject().
		Order(orderAsc(scrapetarget.FieldID)).
		All(ctx)
	if err != nil {
		return nil, err
	}
	return scrapeTargetViews(rows)
}

func scrapeTargetViews(rows []*dbent.ScrapeTarget) ([]ScrapeTargetView, error) {
	views := make([]ScrapeTargetView, 0, len(rows))
	for _, row := range rows {
		view, err := scrapeTargetView(row)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

func scrapeTargetView(row *dbent.ScrapeTarget) (ScrapeTargetView, error) {
	projectRow, err := row.Edges.ProjectOrErr()
	if err != nil {
		return ScrapeTargetView{}, err
	}
	return ScrapeTargetView{
		ID:        row.ID,
		Project:   ProjectInfo{ID: projectRow.ID, Name: projectRow.Name, Slug: projectRow.Slug},
		Name:      row.Name,
		URL:       row.URL,
		Interval:  row.Interval,
		Labels:    row.Labels,
		Enabled:   row.Enabled,
		CreatedAt: row.CreatedAt,
		UpdatedAt: row.UpdatedAt,
	}, nil
}
