package meta

import (
	"context"
	"time"

	"github.com/eimsound/lazy-observe/internal/db/ent/profilemeta"
)

type ProfileListOptions struct {
	Service string
	Type    profilemeta.Type
	Start   *time.Time
	End     *time.Time
	Limit   int
}

type ExpiredProfile struct {
	ID      int
	BlobKey string
}

func (s *Store) CreateProfileMeta(
	ctx context.Context,
	projectID int,
	service string,
	profileType profilemeta.Type,
	start, end time.Time,
	size int64,
	uploadID, payloadSHA256 string,
	blobKey string,
	labels map[string]string,
) (*ProfileMeta, error) {
	return s.client.ProfileMeta.Create().
		SetProjectID(projectID).
		SetUploadID(uploadID).
		SetPayloadSha256(payloadSHA256).
		SetService(service).
		SetType(profileType).
		SetStart(start).
		SetEnd(end).
		SetSize(size).
		SetBlobKey(blobKey).
		SetLabels(labels).
		Save(ctx)
}

func (s *Store) GetProfileByUploadID(ctx context.Context, projectID int, uploadID string) (*ProfileMeta, error) {
	return s.client.ProfileMeta.Query().
		Where(profilemeta.ProjectIDEQ(projectID), profilemeta.UploadIDEQ(uploadID)).
		Only(ctx)
}

func (s *Store) ListProfileServices(ctx context.Context, projectID int) ([]string, error) {
	var services []string
	err := s.client.ProfileMeta.Query().
		Where(profilemeta.ProjectIDEQ(projectID)).
		Unique(true).
		Order(orderAsc(profilemeta.FieldService)).
		Select(profilemeta.FieldService).
		Scan(ctx, &services)
	return services, err
}

func (s *Store) ListProfiles(ctx context.Context, projectID int, opts ProfileListOptions) ([]*ProfileMeta, error) {
	q := s.client.ProfileMeta.Query().Where(profilemeta.ProjectIDEQ(projectID))
	if opts.Service != "" {
		q = q.Where(profilemeta.ServiceEQ(opts.Service))
	}
	if opts.Type != "" {
		q = q.Where(profilemeta.TypeEQ(opts.Type))
	}
	// A profile belongs to the requested window when the intervals overlap.
	if opts.Start != nil {
		q = q.Where(profilemeta.EndGTE(*opts.Start))
	}
	if opts.End != nil {
		q = q.Where(profilemeta.StartLTE(*opts.End))
	}
	q = q.Order(orderDesc(profilemeta.FieldStart), orderDesc(profilemeta.FieldID))
	if opts.Limit > 0 {
		q = q.Limit(opts.Limit)
	}
	return q.All(ctx)
}

func (s *Store) GetProfile(ctx context.Context, projectID, id int) (*ProfileMeta, error) {
	return s.client.ProfileMeta.Query().Where(profilemeta.ProjectIDEQ(projectID), profilemeta.IDEQ(id)).Only(ctx)
}

func (s *Store) ListExpiredProfiles(ctx context.Context, cutoff time.Time, limit int) ([]ExpiredProfile, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.client.ProfileMeta.Query().
		Where(profilemeta.EndLT(cutoff)).
		Order(orderAsc(profilemeta.FieldID)).
		Limit(limit).
		All(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]ExpiredProfile, 0, len(rows))
	for _, row := range rows {
		result = append(result, ExpiredProfile{ID: row.ID, BlobKey: row.BlobKey})
	}
	return result, nil
}

func (s *Store) DeleteProfile(ctx context.Context, id int) error {
	return s.client.ProfileMeta.DeleteOneID(id).Exec(ctx)
}
