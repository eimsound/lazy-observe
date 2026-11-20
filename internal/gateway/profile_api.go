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
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
	pprofprofile "github.com/google/pprof/profile"
)

const (
	maxProfileSize         = 10 << 20
	maxFlameDepth          = 256
	maxFlameNodes          = 10_000
	maxFlamegraphProfiles  = 64
	maxFlamegraphWindow    = 7 * 24 * time.Hour
	defaultProfileListSize = 200
	maxProfileListSize     = 500
	profileIngestRateLimit = 120
	maxProfileServiceLen   = 128
	maxProfileLabelsRawLen = 8192
	maxProfileLabelsCount  = 32
	maxProfileLabelKeyLen  = 64
	maxProfileLabelValLen  = 256
	maxIngestClockSkew     = 5 * time.Minute
)

var (
	minIngestTimestamp = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	errProfileNotGzip  = errors.New("profile is not gzip")
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

func parseGzippedProfile(data []byte, decodedLimit int) (*pprofprofile.Profile, error) {
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return nil, errProfileNotGzip
	}
	decoded, err := gunzipBounded(data, decodedLimit)
	if err != nil {
		return nil, err
	}
	// Never allow nested gzip: ParseUncompressed refuses compressed streams, and
	// checking explicitly guarantees a clear rejection without extra work.
	if len(decoded) >= 2 && decoded[0] == 0x1f && decoded[1] == 0x8b {
		return nil, errors.New("nested gzip pprof payload is not allowed")
	}
	parsed, err := pprofprofile.ParseUncompressed(decoded)
	if err != nil {
		return nil, err
	}
	if err := parsed.CheckValid(); err != nil {
		return nil, err
	}
	return parsed, nil
}

func validProfileText(value string, maxLen int) bool {
	if len(value) > maxLen || !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
		return false
	}
	return true
}

func parseProfileLabels(raw string) (map[string]string, bool) {
	labels := map[string]string{}
	if raw == "" {
		return labels, true
	}
	if len(raw) > maxProfileLabelsRawLen || !utf8.ValidString(raw) || strings.ContainsRune(raw, 0) {
		return nil, false
	}
	if err := json.Unmarshal([]byte(raw), &labels); err != nil || labels == nil {
		return nil, false
	}
	if len(labels) > maxProfileLabelsCount {
		return nil, false
	}
	for k, v := range labels {
		if k == "" || !validProfileText(k, maxProfileLabelKeyLen) || !validProfileText(v, maxProfileLabelValLen) {
			return nil, false
		}
	}
	return labels, true
}

func clampProfileWindow(start, end, now time.Time) (time.Time, time.Time) {
	now = now.UTC().Round(0)
	start = start.UTC().Round(0)
	end = end.UTC().Round(0)
	maxAllowed := now.Add(maxIngestClockSkew)
	if end.After(maxAllowed) {
		end = now
	}
	if start.After(end) {
		start = end
	}
	return start, end
}

func (g *Gateway) allowIngestRate(c fiber.Ctx, key string, limit int, message string) (bool, error) {
	if g.limits == nil {
		return true, nil
	}
	allowed, err := g.limits.Allow(c.Context(), key, limit, time.Minute)
	if err != nil {
		g.logger.Warn("ingest rate limiter error", "key", key, "error", err)
		return false, c.Status(http.StatusServiceUnavailable).
			JSON(fiber.Map{"error": "rate limiter temporarily unavailable"})
	}
	if !allowed {
		c.Set(fiber.HeaderRetryAfter, "60")
		return false, c.Status(http.StatusTooManyRequests).JSON(fiber.Map{"error": message})
	}
	return true, nil
}

func (g *Gateway) profileIngest(c fiber.Ctx) error {
	if g.profiles == nil || g.blobs == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "profile storage unavailable"})
	}
	project := currentProject(c)
	if allowed, err := g.allowIngestRate(
		c,
		"profile:ingest:project:"+strconv.Itoa(project.ID),
		profileIngestRateLimit,
		"profile ingest rate limit exceeded",
	); !allowed {
		return err
	}
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
	if _, err := parseGzippedProfile(data, maxDecodedRequestBody); err != nil {
		if errors.Is(err, errProfileNotGzip) {
			return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "profile must be gzip pprof"})
		}
		if errors.Is(err, errDecodedBodyTooLarge) {
			return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "decoded profile exceeds limit"})
		}
		return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "invalid pprof profile"})
	}

	service := strings.TrimSpace(c.FormValue("service"))
	uploadID := strings.TrimSpace(c.FormValue("upload_id"))
	profileType, ok := parseProfileType(c.FormValue("type"))
	start, startErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(c.FormValue("start")))
	end, endErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(c.FormValue("end")))
	if service == "" || !validProfileText(service, maxProfileServiceLen) ||
		!validProfileUploadID(uploadID) || !ok || startErr != nil || endErr != nil ||
		start.Before(minIngestTimestamp) || end.Before(minIngestTimestamp) || end.Before(start) {
		return badRequest(c)
	}
	start, end = clampProfileWindow(start, end, g.now())
	labels, labelsOK := parseProfileLabels(strings.TrimSpace(c.FormValue("labels")))
	if !labelsOK {
		return badRequest(c)
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
		if delErr := g.blobs.Delete(c.Context(), key); delErr != nil && !errors.Is(delErr, blob.ErrNotFound) {
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
	builder := newFlamegraphBuilder(opts.Type, maxFlameDepth, maxFlameNodes)
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
		raw, readErr := io.ReadAll(io.LimitReader(r, maxProfileSize+1))
		closeErr := r.Close()
		if readErr != nil || closeErr != nil {
			g.logger.Error(
				"stream profile blob failed",
				"profile_id", row.ID, "blob_key", row.BlobKey,
				"read_err", readErr, "close_err", closeErr,
			)
			return fiber.NewError(http.StatusInternalServerError, "read profile blob")
		}
		if len(raw) > maxProfileSize {
			g.logger.Warn("skipping oversized profile blob", "profile_id", row.ID, "blob_key", row.BlobKey)
			continue
		}
		parsed, parseErr := parseGzippedProfile(raw, maxDecodedRequestBody)
		if parseErr != nil {
			g.logger.Warn(
				"skipping corrupt profile blob",
				"profile_id", row.ID, "blob_key", row.BlobKey, "error", parseErr,
			)
			continue
		}
		builder.addProfile(parsed)
	}
	if builder.profileCount == 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "no profiles in window"})
	}
	return c.JSON(builder.finish())
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

type flameNode struct {
	Name      string       `json:"name"`
	Value     int64        `json:"value"`
	Unit      string       `json:"unit,omitempty"`
	Children  []*flameNode `json:"children,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
	byName    map[string]*flameNode
}

type flamegraphBuilder struct {
	profileType  meta.ProfileType
	maxDepth     int
	maxNodes     int
	root         *flameNode
	nodes        int
	profileCount int
	unit         string
	snapshot     bool
}

func newFlamegraphBuilder(profileType meta.ProfileType, maxDepth, maxNodes int) *flamegraphBuilder {
	return &flamegraphBuilder{
		profileType: profileType,
		maxDepth:    maxDepth,
		maxNodes:    maxNodes,
		root:        &flameNode{Name: "root", byName: make(map[string]*flameNode)},
		nodes:       1,
	}
}

func (b *flamegraphBuilder) addProfile(p *pprofprofile.Profile) {
	if p == nil {
		return
	}
	valueIndex, unit, snapshot := profileValueMeta(p, b.profileType)
	if b.profileCount == 0 {
		b.unit = unit
		b.snapshot = snapshot
	} else if !snapshot {
		b.snapshot = false
	}
	b.profileCount++
	if b.maxNodes <= 0 {
		if len(p.Sample) > 0 {
			b.root.Truncated = true
		}
		return
	}
	for _, sample := range p.Sample {
		if valueIndex < 0 || valueIndex >= len(sample.Value) {
			continue
		}
		value := sample.Value[valueIndex]
		if value <= 0 {
			continue
		}
		b.root.Value = addInt64Clamped(b.root.Value, value)
		frames := collapseDirectRecursion(sampleFrames(sample))
		current := b.root
		for depth, name := range frames {
			if depth >= b.maxDepth {
				current.Truncated = true
				break
			}
			child := current.byName[name]
			if child == nil {
				if b.nodes >= b.maxNodes {
					current.Truncated = true
					break
				}
				child = &flameNode{Name: name, byName: make(map[string]*flameNode)}
				current.byName[name] = child
				current.Children = append(current.Children, child)
				b.nodes++
			}
			child.Value = addInt64Clamped(child.Value, value)
			current = child
		}
	}
}

func (b *flamegraphBuilder) finish() *flameNode {
	if b.snapshot && b.profileCount > 1 {
		scaleFlameTreeAverage(b.root, int64(b.profileCount))
	}
	b.root.Unit = b.unit
	sortFlameTree(b.root)
	return b.root
}

func buildFlamegraph(p *pprofprofile.Profile, profileType meta.ProfileType, maxDepth, maxNodes int) *flameNode {
	builder := newFlamegraphBuilder(profileType, maxDepth, maxNodes)
	builder.addProfile(p)
	return builder.finish()
}

func addInt64Clamped(a, b int64) int64 {
	if b <= 0 {
		return a
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func scaleFlameTreeAverage(node *flameNode, count int64) {
	if node == nil || count <= 1 {
		return
	}
	if node.Value > 0 {
		q, r := node.Value/count, node.Value%count
		if r*2 >= count && q < math.MaxInt64 {
			q++
		}
		if q == 0 {
			q = 1
		}
		node.Value = q
	}
	for _, child := range node.Children {
		scaleFlameTreeAverage(child, count)
	}
}

func collapseDirectRecursion(frames []string) []string {
	if len(frames) <= 1 {
		return frames
	}
	out := make([]string, 0, len(frames))
	for _, frame := range frames {
		if len(out) > 0 && out[len(out)-1] == frame {
			continue
		}
		out = append(out, frame)
	}
	return out
}

func profileValueMeta(p *pprofprofile.Profile, profileType meta.ProfileType) (index int, unit string, snapshot bool) {
	index = profileValueIndex(p, profileType)
	if index >= 0 && index < len(p.SampleType) && p.SampleType[index] != nil {
		st := p.SampleType[index]
		unit = st.Unit
		switch st.Type {
		case "inuse_space", "inuse_objects", "goroutine":
			return index, unit, true
		case "cpu", "alloc_space", "alloc_objects":
			return index, unit, false
		case "samples":
			return index, unit, profileType == meta.ProfileTypeGoroutine
		}
	}
	return index, unit, profileType == meta.ProfileTypeHeap || profileType == meta.ProfileTypeGoroutine
}

func profileValueIndex(p *pprofprofile.Profile, profileType meta.ProfileType) int {
	wanted := []string{}
	switch profileType {
	case meta.ProfileTypeCPU:
		wanted = []string{"cpu"}
	case meta.ProfileTypeHeap:
		wanted = []string{"inuse_space", "alloc_space", "inuse_objects"}
	case meta.ProfileTypeGoroutine:
		wanted = []string{"goroutine", "samples"}
	}
	for _, name := range wanted {
		for i, sampleType := range p.SampleType {
			if sampleType != nil && sampleType.Type == name {
				return i
			}
		}
	}
	if len(p.SampleType) == 0 {
		return 0
	}
	return len(p.SampleType) - 1
}

func sampleFrames(sample *pprofprofile.Sample) []string {
	leafFirst := make([]string, 0, len(sample.Location))
	for _, location := range sample.Location {
		if location == nil {
			continue
		}
		if len(location.Line) == 0 {
			leafFirst = append(leafFirst, fmt.Sprintf("0x%x", location.Address))
			continue
		}
		for _, line := range location.Line {
			name := "unknown"
			if line.Function != nil && line.Function.Name != "" {
				name = line.Function.Name
			}
			leafFirst = append(leafFirst, name)
		}
	}
	frames := make([]string, len(leafFirst))
	for i := range leafFirst {
		frames[len(leafFirst)-1-i] = leafFirst[i]
	}
	return frames
}

func sortFlameTree(node *flameNode) {
	sort.Slice(node.Children, func(i, j int) bool {
		if node.Children[i].Value == node.Children[j].Value {
			return node.Children[i].Name < node.Children[j].Name
		}
		return node.Children[i].Value > node.Children[j].Value
	})
	for _, child := range node.Children {
		sortFlameTree(child)
	}
	node.byName = nil
}
