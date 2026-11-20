package meta

import (
	"context"
	"errors"
	"time"

	dbent "github.com/eimsound/lazy-observe/internal/db/ent"
	"github.com/eimsound/lazy-observe/internal/db/ent/project"
	"github.com/eimsound/lazy-observe/internal/db/ent/scrapetarget"
)

// ErrScrapeTargetQuotaExceeded reports that a project reached its configured
// scrape target limit.
var ErrScrapeTargetQuotaExceeded = errors.New("scrape target quota exceeded")

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
	return s.CreateScrapeTargetWithLimit(ctx, input, 0)
}

// CreateScrapeTargetWithLimit creates a scrape target inside a transaction,
// optionally enforcing a per-project target count limit under row lock.
func (s *Store) CreateScrapeTargetWithLimit(
	ctx context.Context,
	input ScrapeTargetCreate,
	maxTargets int,
) (ScrapeTargetView, error) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return ScrapeTargetView{}, err
	}
	defer func() { _ = tx.Rollback() }()

	projectRow, err := tx.Project.Query().Where(project.SlugEQ(input.ProjectSlug)).Only(ctx)
	if err != nil {
		return ScrapeTargetView{}, err
	}
	if maxTargets > 0 {
		count, countErr := tx.ScrapeTarget.Query().
			Where(scrapetarget.ProjectIDEQ(projectRow.ID)).
			Count(ctx)
		if countErr != nil {
			return ScrapeTargetView{}, countErr
		}
		if count >= maxTargets {
			return ScrapeTargetView{}, ErrScrapeTargetQuotaExceeded
		}
	}
	create := tx.ScrapeTarget.Create().
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
	if err := tx.Commit(); err != nil {
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
	_, updated, err := s.UpdateScrapeTargetAtomic(
		ctx,
		projectSlug,
		id,
		func(ScrapeTargetView) (ScrapeTargetUpdate, error) {
			return input, nil
		},
	)
	return updated, err
}

// UpdateScrapeTargetAtomic locks the target row inside a transaction, invokes
// mutate against the latest persisted view, and applies only the returned fields
// so concurrent PATCH requests cannot overwrite each other's changes.
func (s *Store) UpdateScrapeTargetAtomic(
	ctx context.Context,
	projectSlug string,
	id int,
	mutate func(ScrapeTargetView) (ScrapeTargetUpdate, error),
) (ScrapeTargetView, ScrapeTargetView, error) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return ScrapeTargetView{}, ScrapeTargetView{}, err
	}
	defer func() { _ = tx.Rollback() }()

	row, err := tx.ScrapeTarget.Query().
		Where(scrapetarget.IDEQ(id), scrapetarget.HasProjectWith(project.SlugEQ(projectSlug))).
		WithProject().
		Only(ctx)
	if err != nil {
		return ScrapeTargetView{}, ScrapeTargetView{}, err
	}
	projectRow, err := row.Edges.ProjectOrErr()
	if err != nil {
		return ScrapeTargetView{}, ScrapeTargetView{}, err
	}
	before, err := scrapeTargetView(row)
	if err != nil {
		return ScrapeTargetView{}, ScrapeTargetView{}, err
	}
	input, err := mutate(before)
	if err != nil {
		return ScrapeTargetView{}, ScrapeTargetView{}, err
	}
	update := tx.ScrapeTarget.UpdateOneID(row.ID)
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
	saved, err := update.Save(ctx)
	if err != nil {
		return ScrapeTargetView{}, ScrapeTargetView{}, err
	}
	if err := tx.Commit(); err != nil {
		return ScrapeTargetView{}, ScrapeTargetView{}, err
	}
	saved.Edges.Project = projectRow
	after, err := scrapeTargetView(saved)
	if err != nil {
		return ScrapeTargetView{}, ScrapeTargetView{}, err
	}
	return before, after, nil
}

func (s *Store) DeleteScrapeTarget(ctx context.Context, projectSlug string, id int) error {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	affected, err := tx.ScrapeTarget.Delete().
		Where(scrapetarget.IDEQ(id), scrapetarget.HasProjectWith(project.SlugEQ(projectSlug))).
		Exec(ctx)
	if err != nil {
		return err
	}
	if affected == 0 {
		return &NotFoundError{scrapetarget.Label}
	}
	return tx.Commit()
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
