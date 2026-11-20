package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/profiling"
	"github.com/gofiber/fiber/v3"
)

const (
	maxProfileSize         = profiling.DefaultMaxProfileBytes
	maxFlameDepth          = profiling.DefaultMaxFlameDepth
	maxFlameNodes          = profiling.DefaultMaxFlameNodes
	maxFlamegraphProfiles  = 64
	maxFlamegraphWindow    = 7 * 24 * time.Hour
	defaultProfileListSize = 200
	maxProfileListSize     = 500
	maxProfileServiceLen   = 128
	maxProfileLabelsRawLen = 8192
	maxProfileLabelsCount  = 32
	maxProfileLabelKeyLen  = 64
	maxProfileLabelValLen  = 256
	blobRollbackTimeout    = 5 * time.Second
)

type ProfileStore interface {
	CreateProfileMeta(
		context.Context,
		int,
		string,
		meta.ProfileType,
		time.Time,
		time.Time,
		int64,
		string,
		string,
		string,
		map[string]string,
	) (*meta.ProfileMeta, error)
	GetProfileByUploadID(context.Context, int, string) (*meta.ProfileMeta, error)
	ListProfileServices(context.Context, int) ([]string, error)
	ListProfiles(context.Context, int, meta.ProfileListOptions) ([]*meta.ProfileMeta, error)
	GetProfile(context.Context, int, int) (*meta.ProfileMeta, error)
}

// profileRecordResponse is the public profile metadata contract. Keep the
// storage-only blob key, project ID and arbitrary labels out of API responses;
// trace_id is the one correlation label the UI needs for profile -> trace
// navigation.
type profileRecordResponse struct {
	ID      int              `json:"id"`
	Service string           `json:"service"`
	Type    meta.ProfileType `json:"type"`
	Start   string           `json:"start"`
	End     string           `json:"end"`
	Size    int64            `json:"size"`
	TraceID string           `json:"trace_id,omitempty"`
}

type profileServicesResponse struct {
	Services []string `json:"services"`
}

type profileListResponse struct {
	Profiles []profileRecordResponse `json:"profiles"`
}

func newProfileRecordResponse(row *meta.ProfileMeta) profileRecordResponse {
	return profileRecordResponse{
		ID:      row.ID,
		Service: row.Service,
		Type:    row.Type,
		Start:   row.Start.UTC().Format(time.RFC3339Nano),
		End:     row.End.UTC().Format(time.RFC3339Nano),
		Size:    row.Size,
		TraceID: strings.TrimSpace(row.Labels["trace_id"]),
	}
}

func newProfileListResponse(rows []*meta.ProfileMeta) profileListResponse {
	profiles := make([]profileRecordResponse, 0, len(rows))
	for _, row := range rows {
		profiles = append(profiles, newProfileRecordResponse(row))
	}
	return profileListResponse{Profiles: profiles}
}

func parseProfileLabels(raw string) (map[string]string, bool) {
	labels := map[string]string{}
	if raw == "" {
		return labels, true
	}
	if !validIngestText(raw, maxProfileLabelsRawLen) {
		return nil, false
	}
	if err := json.Unmarshal([]byte(raw), &labels); err != nil || labels == nil {
		return nil, false
	}
	if len(labels) > maxProfileLabelsCount {
		return nil, false
	}
	for k, v := range labels {
		if k == "" || !validIngestText(k, maxProfileLabelKeyLen) || !validIngestText(v, maxProfileLabelValLen) {
			return nil, false
		}
	}
	return labels, true
}

func (g *Gateway) profileIngest(c fiber.Ctx) error {
	if g.profiles == nil || g.blobs == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "profile storage unavailable"})
	}
	project := currentProject(c)
	form, err := c.MultipartForm()
	if err != nil {
		return badRequest(c)
	}
	files := form.File["profile"]
	if len(files) != 1 || files[0].Size > maxProfileSize {
		if len(files) == 1 && files[0].Size > maxProfileSize {
			return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "profile exceeds 10MB"})
		}
		return badRequest(c)
	}

	service := strings.TrimSpace(c.FormValue("service"))
	uploadID := strings.TrimSpace(c.FormValue("upload_id"))
	profileType, ok := parseProfileType(c.FormValue("type"))
	start, startErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(c.FormValue("start")))
	end, endErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(c.FormValue("end")))
	if service == "" || !validIngestText(service, maxProfileServiceLen) ||
		!validProfileUploadID(uploadID) || !ok || startErr != nil || endErr != nil ||
		start.Before(minIngestTimestamp) || end.Before(minIngestTimestamp) || end.Before(start) {
		return badRequest(c)
	}
	start, end = clampIngestWindow(start, end, g.now())
	labels, labelsOK := parseProfileLabels(strings.TrimSpace(c.FormValue("labels")))
	if !labelsOK {
		return badRequest(c)
	}

	file, err := files[0].Open()
	if err != nil {
		return badRequest(c)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxProfileSize+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return badRequest(c)
	}
	if len(data) > maxProfileSize {
		return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "profile exceeds 10MB"})
	}
	if _, err := profiling.ParseGzippedForType(data, maxDecodedRequestBody, profileType); err != nil {
		if errors.Is(err, profiling.ErrNotGzip) {
			return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "profile must be gzip pprof"})
		}
		if errors.Is(err, profiling.ErrDecodedTooLarge) {
			return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "decoded profile exceeds limit"})
		}
		if errors.Is(err, profiling.ErrInvalidSampleType) {
			return c.Status(http.StatusUnprocessableEntity).
				JSON(fiber.Map{"error": "profile has invalid or incompatible sample types"})
		}
		return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "invalid pprof profile"})
	}

	payloadDigest := sha256.Sum256(data)
	payloadSHA256 := hex.EncodeToString(payloadDigest[:])
	existing, err := g.profiles.GetProfileByUploadID(c.Context(), project.ID, uploadID)
	if err == nil {
		return existingProfileUploadResponse(
			c,
			existing,
			service,
			profileType,
			start,
			end,
			int64(len(data)),
			payloadSHA256,
			labels,
		)
	}
	if !meta.IsNotFound(err) {
		return apiError(c, err)
	}
	uuid, err := randomProfileID()
	if err != nil {
		return fiber.NewError(http.StatusInternalServerError, "generate profile id")
	}
	key := fmt.Sprintf(
		"profiles/%s/%s/%s/%d_%s.pb.gz",
		safeProfileSegment(project.Slug),
		safeProfileSegment(service),
		profileType,
		start.Unix(),
		uuid,
	)
	if err := g.blobs.Put(c.Context(), key, bytes.NewReader(data)); err != nil {
		g.logger.Error("store profile blob failed", "project", project.Slug, "key", key, "error", err)
		return fiber.NewError(http.StatusInternalServerError, "store profile blob")
	}
	row, err := g.profiles.CreateProfileMeta(
		c.Context(),
		project.ID,
		service,
		profileType,
		start,
		end,
		int64(len(data)),
		uploadID,
		payloadSHA256,
		key,
		labels,
	)
	if err != nil {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Context()), blobRollbackTimeout)
		defer cancel()
		if delErr := g.blobs.Delete(rollbackCtx, key); delErr != nil && !errors.Is(delErr, blob.ErrNotFound) {
			g.logger.Warn("rollback profile blob failed", "project", project.Slug, "key", key, "error", delErr)
		}
		existing, lookupErr := g.profiles.GetProfileByUploadID(c.Context(), project.ID, uploadID)
		if lookupErr == nil {
			return existingProfileUploadResponse(
				c,
				existing,
				service,
				profileType,
				start,
				end,
				int64(len(data)),
				payloadSHA256,
				labels,
			)
		}
		return apiError(c, err)
	}
	return c.Status(http.StatusCreated).JSON(newProfileRecordResponse(row))
}

func existingProfileUploadResponse(
	c fiber.Ctx,
	existing *meta.ProfileMeta,
	service string,
	profileType meta.ProfileType,
	start, end time.Time,
	size int64,
	payloadSHA256 string,
	labels map[string]string,
) error {
	if existing.Service != service || existing.Type != profileType ||
		!existing.Start.Equal(start.UTC()) || !existing.End.Equal(end.UTC()) ||
		existing.Size != size || existing.PayloadSha256 != payloadSHA256 || !maps.Equal(existing.Labels, labels) {
		return c.Status(http.StatusConflict).
			JSON(fiber.Map{"error": "profile upload_id conflicts with accepted content"})
	}
	return c.Status(http.StatusOK).JSON(newProfileRecordResponse(existing))
}

func validProfileUploadID(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func (g *Gateway) listProfileServices(c fiber.Ctx) error {
	if g.profiles == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "profile storage unavailable"})
	}
	services, err := g.profiles.ListProfileServices(c.Context(), currentProject(c).ID)
	if err != nil {
		return apiError(c, err)
	}
	if services == nil {
		services = []string{}
	}
	return c.JSON(profileServicesResponse{Services: services})
}

func (g *Gateway) listProfiles(c fiber.Ctx) error {
	if g.profiles == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "profile storage unavailable"})
	}
	opts, ok := profileQuery(c, false)
	if !ok {
		return nil
	}
	rows, err := g.profiles.ListProfiles(c.Context(), currentProject(c).ID, opts)
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(newProfileListResponse(rows))
}

func (g *Gateway) profileFlamegraph(c fiber.Ctx) error {
	if g.profiles == nil || g.blobs == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "profile storage unavailable"})
	}
	opts, ok := profileQuery(c, true)
	if !ok {
		return nil
	}
	rows, err := g.profiles.ListProfiles(c.Context(), currentProject(c).ID, opts)
	if err != nil {
		return apiError(c, err)
	}
	if len(rows) == 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "no profiles in window"})
	}
	builder := profiling.NewFlamegraphBuilder(opts.Type, maxFlameDepth, maxFlameNodes)
	for _, row := range rows {
		r, err := g.blobs.Get(c.Context(), row.BlobKey)
		if errors.Is(err, blob.ErrNotFound) {
			g.logger.Warn("profile blob missing during flamegraph", "profile_id", row.ID, "blob_key", row.BlobKey)
			continue
		}
		if err != nil {
			g.logger.Error("read profile blob failed", "profile_id", row.ID, "blob_key", row.BlobKey, "error", err)
			return fiber.NewError(http.StatusInternalServerError, "read profile blob")
		}
		addErr := builder.AddGzippedReader(r, maxProfileSize, maxDecodedRequestBody)
		closeErr := r.Close()
		if addErr != nil {
			switch {
			case errors.Is(addErr, profiling.ErrCompressedTooLarge):
				g.logger.Warn("skipping oversized profile blob", "profile_id", row.ID, "blob_key", row.BlobKey)
				continue
			case errors.Is(addErr, profiling.ErrNotGzip),
				errors.Is(addErr, profiling.ErrDecodedTooLarge),
				errors.Is(addErr, profiling.ErrNestedGzip),
				errors.Is(addErr, profiling.ErrInvalidProfile),
				errors.Is(addErr, profiling.ErrInvalidSampleType):
				g.logger.Warn(
					"skipping corrupt profile blob",
					"profile_id", row.ID, "blob_key", row.BlobKey, "error", addErr,
				)
				continue
			default:
				g.logger.Error(
					"stream profile blob failed",
					"profile_id", row.ID, "blob_key", row.BlobKey,
					"read_err", addErr, "close_err", closeErr,
				)
				return fiber.NewError(http.StatusInternalServerError, "read profile blob")
			}
		}
		if closeErr != nil {
			g.logger.Error(
				"close profile blob failed",
				"profile_id", row.ID, "blob_key", row.BlobKey, "close_err", closeErr,
			)
			return fiber.NewError(http.StatusInternalServerError, "read profile blob")
		}
	}
	if builder.Count() == 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "no profiles in window"})
	}
	return c.JSON(builder.Finish())
}

func (g *Gateway) rawProfile(c fiber.Ctx) error {
	if g.profiles == nil || g.blobs == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "profile storage unavailable"})
	}
	id, err := strconv.Atoi(c.Params("id"))
	if err != nil || id <= 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	row, err := g.profiles.GetProfile(c.Context(), currentProject(c).ID, id)
	if meta.IsNotFound(err) {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	if err != nil {
		return apiError(c, err)
	}
	r, err := g.blobs.Get(c.Context(), row.BlobKey)
	if errors.Is(err, blob.ErrNotFound) {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	if err != nil {
		g.logger.Error("read raw profile blob failed", "profile_id", row.ID, "blob_key", row.BlobKey, "error", err)
		return fiber.NewError(http.StatusInternalServerError, "read profile blob")
	}
	c.Set(fiber.HeaderContentType, "application/gzip")
	c.Set(fiber.HeaderContentDisposition, fmt.Sprintf(`attachment; filename="profile-%d.pb.gz"`, row.ID))
	return c.SendStream(r)
}

func profileQuery(c fiber.Ctx, flame bool) (meta.ProfileListOptions, bool) {
	opts := meta.ProfileListOptions{Service: strings.TrimSpace(c.Query("service"))}
	if flame && opts.Service == "" {
		_ = badRequest(c)
		return opts, false
	}
	rawType := strings.TrimSpace(c.Query("type"))
	if flame && rawType == "" {
		rawType = "cpu"
	}
	if rawType != "" {
		var valid bool
		opts.Type, valid = parseProfileType(rawType)
		if !valid {
			_ = badRequest(c)
			return opts, false
		}
	}
	for name, target := range map[string]**time.Time{"start": &opts.Start, "end": &opts.End} {
		if raw := strings.TrimSpace(c.Query(name)); raw != "" {
			parsed, err := time.Parse(time.RFC3339Nano, raw)
			if err != nil {
				_ = badRequest(c)
				return opts, false
			}
			normalized := parsed.UTC().Round(0)
			*target = &normalized
		}
	}
	if opts.Start != nil && opts.End != nil && opts.End.Before(*opts.Start) {
		_ = badRequest(c)
		return opts, false
	}
	if flame {
		if (opts.Start == nil) != (opts.End == nil) {
			_ = badRequest(c)
			return opts, false
		}
		if opts.Start != nil && opts.End != nil && opts.End.Sub(*opts.Start) > maxFlamegraphWindow {
			_ = badRequest(c)
			return opts, false
		}
		opts.Limit = maxFlamegraphProfiles
		return opts, true
	}
	opts.Limit = defaultProfileListSize
	if rawLimit := strings.TrimSpace(c.Query("limit")); rawLimit != "" {
		limit, err := strconv.Atoi(rawLimit)
		if err != nil || limit < 1 || limit > maxProfileListSize {
			_ = badRequest(c)
			return opts, false
		}
		opts.Limit = limit
	}
	return opts, true
}

func parseProfileType(raw string) (meta.ProfileType, bool) {
	v := meta.ProfileType(strings.TrimSpace(raw))
	return v, v == meta.ProfileTypeCPU || v == meta.ProfileTypeHeap || v == meta.ProfileTypeGoroutine
}

func randomProfileID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	encoded := hex.EncodeToString(b)
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:], nil
}

func safeProfileSegment(value string) string {
	value = strings.TrimSpace(value)
	var b strings.Builder
	for _, r := range value {
		if r <= unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)) || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 || b.String() == "." || b.String() == ".." {
		return "unknown"
	}
	return b.String()
}
