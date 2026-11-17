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
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v2"
	pprofprofile "github.com/google/pprof/profile"
)

const (
	maxProfileSize = 10 << 20
	maxFlameDepth  = 64
	maxFlameNodes  = 10_000
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

func (g *Gateway) profileIngest(c *fiber.Ctx) error {
	if g.profiles == nil || g.blobs == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "profile storage unavailable"})
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
	if len(data) < 2 || data[0] != 0x1f || data[1] != 0x8b {
		return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "profile must be gzip pprof"})
	}
	// pprof.ParseData gunzips without a limit; decode with the request bound
	// first and parse the already-decoded protobuf.
	decodedProfile, err := gunzipBounded(data, maxDecodedRequestBody)
	if errors.Is(err, errDecodedBodyTooLarge) {
		return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "decoded profile exceeds limit"})
	}
	if err != nil {
		return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "invalid pprof profile"})
	}
	if _, err := pprofprofile.ParseData(decodedProfile); err != nil {
		return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "invalid pprof profile"})
	}

	service := strings.TrimSpace(c.FormValue("service"))
	uploadID := strings.TrimSpace(c.FormValue("upload_id"))
	profileType, ok := parseProfileType(c.FormValue("type"))
	start, startErr := time.Parse(time.RFC3339, strings.TrimSpace(c.FormValue("start")))
	end, endErr := time.Parse(time.RFC3339, strings.TrimSpace(c.FormValue("end")))
	if service == "" || !validProfileUploadID(uploadID) || !ok || startErr != nil || endErr != nil ||
		end.Before(start) {
		return badRequest(c)
	}
	labels := map[string]string{}
	if raw := strings.TrimSpace(c.FormValue("labels")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &labels); err != nil || labels == nil {
			return badRequest(c)
		}
	}
	project := currentProject(c)
	payloadDigest := sha256.Sum256(data)
	payloadSHA256 := hex.EncodeToString(payloadDigest[:])
	existing, err := g.profiles.GetProfileByUploadID(c.UserContext(), project.ID, uploadID)
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
	if err := g.blobs.Put(c.UserContext(), key, bytes.NewReader(data)); err != nil {
		return fiber.NewError(http.StatusInternalServerError, "store profile blob")
	}
	row, err := g.profiles.CreateProfileMeta(
		c.UserContext(),
		project.ID,
		service,
		profileType,
		start.UTC(),
		end.UTC(),
		int64(len(data)),
		uploadID,
		payloadSHA256,
		key,
		labels,
	)
	if err != nil {
		_ = g.blobs.Delete(c.UserContext(), key)
		existing, lookupErr := g.profiles.GetProfileByUploadID(c.UserContext(), project.ID, uploadID)
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
	c *fiber.Ctx,
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

func (g *Gateway) listProfileServices(c *fiber.Ctx) error {
	if g.profiles == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "profile storage unavailable"})
	}
	services, err := g.profiles.ListProfileServices(c.UserContext(), currentProject(c).ID)
	if err != nil {
		return apiError(c, err)
	}
	if services == nil {
		services = []string{}
	}
	return c.JSON(profileServicesResponse{Services: services})
}

func (g *Gateway) listProfiles(c *fiber.Ctx) error {
	if g.profiles == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "profile storage unavailable"})
	}
	opts, ok := profileQuery(c, false)
	if !ok {
		return nil
	}
	rows, err := g.profiles.ListProfiles(c.UserContext(), currentProject(c).ID, opts)
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(newProfileListResponse(rows))
}

func (g *Gateway) profileFlamegraph(c *fiber.Ctx) error {
	if g.profiles == nil || g.blobs == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "profile storage unavailable"})
	}
	opts, ok := profileQuery(c, true)
	if !ok {
		return nil
	}
	rows, err := g.profiles.ListProfiles(c.UserContext(), currentProject(c).ID, opts)
	if err != nil {
		return apiError(c, err)
	}
	if len(rows) == 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "no profiles in window"})
	}
	profiles := make([]*pprofprofile.Profile, 0, len(rows))
	for _, row := range rows {
		r, err := g.blobs.Get(c.UserContext(), row.BlobKey)
		if err != nil {
			return fiber.NewError(http.StatusInternalServerError, "read profile blob")
		}
		parsed, parseErr := pprofprofile.Parse(r)
		closeErr := r.Close()
		if parseErr != nil || closeErr != nil {
			return fiber.NewError(http.StatusInternalServerError, "parse stored profile")
		}
		profiles = append(profiles, parsed)
	}
	merged, err := pprofprofile.Merge(profiles)
	if err != nil {
		return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "profiles cannot be merged"})
	}
	return c.JSON(buildFlamegraph(merged, opts.Type, maxFlameDepth, maxFlameNodes))
}

func (g *Gateway) rawProfile(c *fiber.Ctx) error {
	if g.profiles == nil || g.blobs == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "profile storage unavailable"})
	}
	id, err := strconv.Atoi(c.Params("id"))
	if err != nil || id <= 0 {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	row, err := g.profiles.GetProfile(c.UserContext(), currentProject(c).ID, id)
	if meta.IsNotFound(err) {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	if err != nil {
		return apiError(c, err)
	}
	r, err := g.blobs.Get(c.UserContext(), row.BlobKey)
	if errors.Is(err, blob.ErrNotFound) {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	if err != nil {
		return fiber.NewError(http.StatusInternalServerError, "read profile blob")
	}
	c.Set(fiber.HeaderContentType, "application/octet-stream")
	c.Set(fiber.HeaderContentEncoding, "gzip")
	c.Set(fiber.HeaderContentDisposition, fmt.Sprintf(`attachment; filename="profile-%d.pb.gz"`, row.ID))
	return c.SendStream(r)
}

func profileQuery(c *fiber.Ctx, flame bool) (meta.ProfileListOptions, bool) {
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
			parsed, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				_ = badRequest(c)
				return opts, false
			}
			*target = &parsed
		}
	}
	if opts.Start != nil && opts.End != nil && opts.End.Before(*opts.Start) {
		_ = badRequest(c)
		return opts, false
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
	Children  []*flameNode `json:"children,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
	byName    map[string]*flameNode
}

func buildFlamegraph(p *pprofprofile.Profile, profileType meta.ProfileType, maxDepth, maxNodes int) *flameNode {
	root := &flameNode{Name: "root", byName: make(map[string]*flameNode)}
	if p == nil || maxNodes <= 0 {
		root.Truncated = p != nil && len(p.Sample) > 0
		return root
	}
	valueIndex := profileValueIndex(p, profileType)
	nodes := 1
	for _, sample := range p.Sample {
		if valueIndex >= len(sample.Value) {
			continue
		}
		value := sample.Value[valueIndex]
		root.Value += value
		frames := sampleFrames(sample)
		current := root
		for depth, name := range frames {
			if depth >= maxDepth {
				current.Truncated = true
				break
			}
			child := current.byName[name]
			if child == nil {
				if nodes >= maxNodes {
					current.Truncated = true
					break
				}
				child = &flameNode{Name: name, byName: make(map[string]*flameNode)}
				current.byName[name] = child
				current.Children = append(current.Children, child)
				nodes++
			}
			child.Value += value
			current = child
		}
	}
	sortFlameTree(root)
	return root
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
			if sampleType.Type == name {
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
