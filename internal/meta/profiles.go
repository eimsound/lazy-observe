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
		SetStart(start.UTC().Round(0)).
		SetEnd(end.UTC().Round(0)).
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
		q = q.Where(profilemeta.EndGTE(opts.Start.UTC().Round(0)))
	}
	if opts.End != nil {
		q = q.Where(profilemeta.StartLTE(opts.End.UTC().Round(0)))
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
		Where(profilemeta.EndLT(cutoff.UTC().Round(0))).
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

// ReferencedProfileBlobKeys returns the set of blob keys referenced by active
// profile_metas rows for a project so retention can sweep orphaned profile blobs.
func (s *Store) ReferencedProfileBlobKeys(ctx context.Context, projectID int) (map[string]bool, error) {
	rows, err := s.db.QueryContext(
		ctx,
		bind(s.dialect, `SELECT blob_key FROM profile_metas WHERE project_id = ?`),
		projectID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make(map[string]bool)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys[key] = true
	}
	return keys, rows.Err()
}
