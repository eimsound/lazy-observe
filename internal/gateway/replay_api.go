package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/gofiber/fiber/v3"
)

const (
	maxReplaySegment            = 5 << 20
	maxReplaySegmentMetaBytes   = 1 << 20
	maxReplaySessionIDLen       = 128
	maxReplaySegmentsPerSession = 200
	maxReplaySessionBytes       = 50 << 20
	maxReplayMarkersPerSegment  = 200
	maxReplayMarkersPerSession  = 1000
	maxReplayPage               = 10_000
	replayPageSize              = 50
	replayProjectRateLimit      = 600
	replaySessionRateLimit      = 120
	maxReplayTimestampMs        = 253402300799999 // 9999-12-31T23:59:59.999Z
)

var (
	replayIDPattern     = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	replayMethodPattern = regexp.MustCompile("^[A-Z0-9!#$%&'*+.^_`|~-]{1,32}$")
)

type ReplayStore interface {
	UpsertReplaySession(context.Context, int, string, time.Time, time.Time, int, int64, bool, string, string) error
	ListReplaySessions(context.Context, int, meta.ReplayListOptions) ([]*meta.ReplaySession, error)
	CountReplaySessions(context.Context, int, meta.ReplayListOptions) (int, error)
	GetReplaySession(context.Context, int, string) (*meta.ReplaySession, error)
}

type replayIssueBatchLookup interface {
	IssueIDsForErrorEvents(context.Context, int, []string) (map[string]int, error)
}

type replaySessionDTO struct {
	SessionID    string `json:"session_id"`
	StartedAt    int64  `json:"started_at"`
	LastSeenAt   int64  `json:"last_seen_at"`
	SegmentCount int    `json:"segment_count"`
	TotalBytes   int64  `json:"total_bytes"`
	HasError     bool   `json:"has_error"`
	Release      string `json:"release,omitempty"`
	PageURL      string `json:"page_url"`
}

type replaySegmentDTO struct {
	Seq        int   `json:"seq"`
	StartedAt  int64 `json:"started_at"`
	EventCount int   `json:"event_count"`
	SizeBytes  int64 `json:"size_bytes"`
}

type replayMarkerDTO struct {
	Type       string   `json:"type"`
	Time       int64    `json:"time"`
	IssueID    string   `json:"issue_id,omitempty"`
	EventID    string   `json:"event_id,omitempty"`
	Title      string   `json:"title,omitempty"`
	Level      string   `json:"level,omitempty"`
	Method     string   `json:"method,omitempty"`
	Path       string   `json:"path,omitempty"`
	Status     *int     `json:"status,omitempty"`
	DurationMS *float64 `json:"duration_ms,omitempty"`
	Name       string   `json:"name,omitempty"`
	Value      *float64 `json:"value,omitempty"`
	Rating     string   `json:"rating,omitempty"`
}

type replaySegmentIndex struct {
	Seq        int               `json:"seq"`
	StartedAt  int64             `json:"started_at"`
	EventCount int               `json:"event_count"`
	SizeBytes  int64             `json:"size_bytes"`
	Markers    []replayMarkerDTO `json:"markers,omitempty"`
}

type replayEventEnvelope struct {
	Type      int
	Timestamp int64
	HasTime   bool
	RawData   json.RawMessage
	Tag       string
	Payload   json.RawMessage
}

type parsedReplaySegment struct {
	startedAt  time.Time
	lastSeenAt time.Time
	segStartMs int64
	eventCount int
	pageURL    string
	release    string
	markers    []replayMarkerDTO
}

func (g *Gateway) replayIngest(c fiber.Ctx) error {
	if g.replays == nil || g.blobs == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "replay storage unavailable"})
	}
	sessionID := strings.TrimSpace(c.Query("session_id"))
	seq, err := strconv.Atoi(c.Query("seq"))
	if len(sessionID) == 0 || len(sessionID) > maxReplaySessionIDLen ||
		!replayIDPattern.MatchString(sessionID) || err != nil || seq < 0 {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid session_id or seq"})
	}
	if seq >= maxReplaySegmentsPerSession {
		return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "replay session quota exceeded"})
	}
	p := currentProject(c)
	if allowed, err := g.allowIngestRate(
		c,
		"replay:ingest:project:"+strconv.Itoa(p.ID),
		replayProjectRateLimit,
		"replay ingest rate limit exceeded",
	); !allowed {
		return err
	}
	if allowed, err := g.allowIngestRate(
		c,
		"replay:ingest:session:"+strconv.Itoa(p.ID)+":"+sessionID,
		replaySessionRateLimit,
		"replay session rate limit exceeded",
	); !allowed {
		return err
	}

	decoded, status, errMsg := decodeReplayBody(c)
	if status != 0 {
		return c.Status(status).JSON(fiber.Map{"error": errMsg})
	}
	now := g.now().UTC().Round(0)
	parsed, ok := parseReplaySegmentEvents(decoded, now, true)
	if !ok {
		return c.Status(http.StatusUnprocessableEntity).
			JSON(fiber.Map{"error": "segment must contain a JSON event array"})
	}

	var stored bytes.Buffer
	zw := gzip.NewWriter(&stored)
	_, writeErr := zw.Write(decoded)
	closeErr := zw.Close()
	if writeErr != nil || closeErr != nil {
		return fiber.NewError(http.StatusInternalServerError, "compress replay segment")
	}

	lock := g.replayLock(p.ID, sessionID)
	lock.Lock()
	defer lock.Unlock()
	prefix := replayPrefix(p.Slug, sessionID)
	key := fmt.Sprintf("%s%d.json.gz", prefix, seq)
	metaKey := replaySegmentMetaKey(prefix, seq)

	sameContent, conflict, checkErr := g.checkExistingReplaySegment(c.Context(), key, decoded)
	if checkErr != nil {
		g.logger.Error(
			"check existing replay segment failed",
			"project",
			p.Slug,
			"session_id",
			sessionID,
			"seq",
			seq,
			"error",
			checkErr,
		)
		return fiber.NewError(http.StatusInternalServerError, "store replay segment")
	}
	if sameContent {
		return c.Status(http.StatusAccepted).JSON(fiber.Map{"accepted": true, "seq": seq})
	}
	if conflict {
		return c.Status(http.StatusConflict).JSON(fiber.Map{"error": "replay segment already exists"})
	}

	existingObjects, err := g.blobs.List(c.Context(), prefix)
	if err != nil {
		g.logger.Error(
			"list replay segments before put failed",
			"project",
			p.Slug,
			"session_id",
			sessionID,
			"error",
			err,
		)
		return fiber.NewError(http.StatusInternalServerError, "store replay segment")
	}
	existingCount, existingBytes := countReplaySegmentObjects(existingObjects)
	if existingCount >= maxReplaySegmentsPerSession || existingBytes+int64(stored.Len()) > maxReplaySessionBytes {
		return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "replay session quota exceeded"})
	}

	if err := g.blobs.Put(c.Context(), key, bytes.NewReader(stored.Bytes())); err != nil {
		if errors.Is(err, blob.ErrInvalidKey) {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid replay key"})
		}
		g.logger.Error("put replay segment blob failed", "project", p.Slug, "key", key, "error", err)
		return fiber.NewError(http.StatusInternalServerError, "store replay segment")
	}

	wroteMeta := false
	segIndex := replaySegmentIndex{
		Seq:        seq,
		StartedAt:  parsed.segStartMs,
		EventCount: parsed.eventCount,
		SizeBytes:  int64(stored.Len()),
		Markers:    parsed.markers,
	}
	if metaBytes, marshalErr := json.Marshal(segIndex); marshalErr == nil {
		if putErr := g.blobs.Put(c.Context(), metaKey, bytes.NewReader(metaBytes)); putErr != nil {
			g.logger.Warn("put replay segment summary failed", "project", p.Slug, "key", metaKey, "error", putErr)
		} else {
			wroteMeta = true
		}
	}

	objects, err := g.blobs.List(c.Context(), prefix)
	if err != nil {
		g.rollbackReplaySegment(c.Context(), key, metaKey, wroteMeta)
		g.logger.Error(
			"list replay segments after put failed",
			"project",
			p.Slug,
			"session_id",
			sessionID,
			"error",
			err,
		)
		return fiber.NewError(http.StatusInternalServerError, "store replay segment")
	}
	segCount, totalBytes := countReplaySegmentObjects(objects)
	if segCount == 0 {
		segCount = existingCount + 1
		totalBytes = existingBytes + int64(stored.Len())
	}
	if err := g.replays.UpsertReplaySession(
		c.Context(),
		p.ID,
		sessionID,
		parsed.startedAt,
		parsed.lastSeenAt,
		segCount,
		totalBytes,
		c.Get("X-LO-Replay-Error") == "1",
		parsed.release,
		parsed.pageURL,
	); err != nil {
		g.rollbackReplaySegment(c.Context(), key, metaKey, wroteMeta)
		g.logger.Error("upsert replay session failed", "project", p.Slug, "session_id", sessionID, "error", err)
		return fiber.NewError(http.StatusInternalServerError, "store replay session")
	}
	return c.Status(http.StatusAccepted).JSON(fiber.Map{"accepted": true, "seq": seq})
}

func decodeReplayBody(c fiber.Ctx) ([]byte, int, string) {
	body := requestBody(c)
	compression := strings.ToLower(strings.TrimSpace(c.Get("X-LO-Replay-Compression")))
	encoding, _ := c.Locals(decodedContentEncodingLocal).(string)

	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		decoded, err := gunzipBounded(body, maxReplaySegment)
		if errors.Is(err, errDecodedBodyTooLarge) {
			return nil, http.StatusRequestEntityTooLarge, "segment exceeds 5MB"
		}
		if err != nil {
			return nil, http.StatusUnprocessableEntity, "invalid gzip segment"
		}
		return decoded, 0, ""
	}
	if encoding == "gzip" || encoding == "x-gzip" {
		if len(body) > maxReplaySegment {
			return nil, http.StatusRequestEntityTooLarge, "segment exceeds 5MB"
		}
		return append([]byte(nil), body...), 0, ""
	}
	if compression == "identity" {
		if len(body) > maxReplaySegment {
			return nil, http.StatusRequestEntityTooLarge, "segment exceeds 5MB"
		}
		return append([]byte(nil), body...), 0, ""
	}
	if compression != "" && compression != "gzip" {
		return nil, http.StatusUnprocessableEntity, "unsupported replay compression"
	}
	return nil, http.StatusUnprocessableEntity, "segment body must be gzip"
}

func (g *Gateway) checkExistingReplaySegment(
	ctx context.Context,
	key string,
	decoded []byte,
) (same, conflict bool, err error) {
	r, err := g.blobs.Get(ctx, key)
	if errors.Is(err, blob.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	existingRaw, readErr := io.ReadAll(io.LimitReader(r, maxReplaySegment+1))
	closeErr := r.Close()
	if readErr != nil || closeErr != nil {
		return false, false, errors.Join(readErr, closeErr)
	}
	existingDecoded, gunzipErr := gunzipBounded(existingRaw, maxReplaySegment)
	if gunzipErr == nil && bytes.Equal(existingDecoded, decoded) {
		return true, false, nil
	}
	return false, true, nil
}

func (g *Gateway) rollbackReplaySegment(ctx context.Context, key, metaKey string, wroteMeta bool) {
	if err := g.blobs.Delete(ctx, key); err != nil && !errors.Is(err, blob.ErrNotFound) {
		g.logger.Warn("rollback replay segment blob failed", "key", key, "error", err)
	}
	if wroteMeta {
		if err := g.blobs.Delete(ctx, metaKey); err != nil && !errors.Is(err, blob.ErrNotFound) {
			g.logger.Warn("rollback replay summary blob failed", "key", metaKey, "error", err)
		}
	}
}

func countReplaySegmentObjects(objects []blob.Object) (int, int64) {
	count := 0
	var total int64
	for _, object := range objects {
		if _, ok := replaySeq(object.Key); !ok {
			continue
		}
		count++
		total += object.Size
	}
	return count, total
}

func (g *Gateway) listReplaySessions(c fiber.Ctx) error {
	if g.replays == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "replay storage unavailable"})
	}
	opts := meta.ReplayListOptions{Limit: replayPageSize}
	if value := strings.TrimSpace(c.Query("has_error")); value != "" {
		parsed, err := strconv.ParseBool(value)
		if err != nil {
			return badRequest(c)
		}
		opts.HasError = &parsed
	}
	if value := strings.TrimSpace(c.Query("start")); value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return badRequest(c)
		}
		normalized := parsed.UTC().Round(0)
		opts.Start = &normalized
	}
	if value := strings.TrimSpace(c.Query("end")); value != "" {
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return badRequest(c)
		}
		normalized := parsed.UTC().Round(0)
		opts.End = &normalized
	}
	if opts.Start != nil && opts.End != nil && opts.End.Before(*opts.Start) {
		return badRequest(c)
	}
	if value := c.Query("page"); value != "" {
		page, err := strconv.Atoi(value)
		if err != nil || page < 1 || page > maxReplayPage {
			return badRequest(c)
		}
		opts.Page = page
	}
	rows, err := g.replays.ListReplaySessions(c.Context(), currentProject(c).ID, opts)
	if err != nil {
		return apiError(c, err)
	}
	total, err := g.replays.CountReplaySessions(c.Context(), currentProject(c).ID, opts)
	if err != nil {
		return apiError(c, err)
	}
	sessions := make([]replaySessionDTO, 0, len(rows))
	for _, row := range rows {
		sessions = append(sessions, replaySessionFromMeta(row))
	}
	return c.JSON(
		fiber.Map{"sessions": sessions, "total": total, "page": max(opts.Page, 1), "page_size": replayPageSize},
	)
}

func (g *Gateway) getReplaySession(c fiber.Ctx) error {
	row, ok := g.replaySession(c)
	if !ok {
		return nil
	}
	project := currentProject(c)
	objects, err := g.blobs.List(c.Context(), replayPrefix(project.Slug, row.SessionID))
	if err != nil {
		g.logger.Error(
			"list replay session blobs failed",
			"project",
			project.Slug,
			"session_id",
			row.SessionID,
			"error",
			err,
		)
		return fiber.NewError(http.StatusInternalServerError, "list replay segments")
	}
	segObjects := make(map[int]blob.Object)
	metaObjects := make(map[int]blob.Object)
	seqs := make([]int, 0, len(objects))
	seenSeq := make(map[int]struct{})
	for _, object := range objects {
		if seq, isSeg := replaySeq(object.Key); isSeg {
			segObjects[seq] = object
			if _, ok := seenSeq[seq]; !ok {
				seenSeq[seq] = struct{}{}
				seqs = append(seqs, seq)
			}
			continue
		}
		if seq, isMeta := replayMetaSeq(object.Key); isMeta {
			metaObjects[seq] = object
		}
	}
	sort.Ints(seqs)
	if len(seqs) > maxReplaySegmentsPerSession {
		seqs = seqs[:maxReplaySegmentsPerSession]
	}

	segments := make([]replaySegmentDTO, 0, len(seqs))
	markers := make([]replayMarkerDTO, 0)
	for _, seq := range seqs {
		object := segObjects[seq]
		var (
			segment        replaySegmentDTO
			segmentMarkers []replayMarkerDTO
			loaded         bool
		)
		if metaObj, hasMeta := metaObjects[seq]; hasMeta {
			idx, metaErr := g.readReplaySegmentIndex(c.Context(), metaObj.Key)
			if metaErr == nil {
				segment = replaySegmentDTO{
					Seq:        seq,
					StartedAt:  idx.StartedAt,
					EventCount: idx.EventCount,
					SizeBytes:  object.Size,
				}
				segmentMarkers = idx.Markers
				loaded = true
			} else if !errors.Is(metaErr, blob.ErrNotFound) {
				g.logger.Warn(
					"read replay summary failed, falling back to segment",
					"key",
					metaObj.Key,
					"error",
					metaErr,
				)
			}
		}
		if !loaded {
			var segErr error
			segment, segmentMarkers, segErr = g.replaySegmentMetadata(c.Context(), object, seq, row.StartedAt)
			if errors.Is(segErr, blob.ErrNotFound) {
				g.logger.Warn("replay segment missing during read", "session_id", row.SessionID, "seq", seq)
				continue
			}
			if segErr != nil {
				g.logger.Warn(
					"skipping unreadable replay segment",
					"session_id",
					row.SessionID,
					"seq",
					seq,
					"error",
					segErr,
				)
				continue
			}
		}
		segments = append(segments, segment)
		if remaining := maxReplayMarkersPerSession - len(markers); remaining > 0 && len(segmentMarkers) > 0 {
			if len(segmentMarkers) > remaining {
				segmentMarkers = segmentMarkers[:remaining]
			}
			markers = append(markers, segmentMarkers...)
		}
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].Seq < segments[j].Seq })
	sort.Slice(markers, func(i, j int) bool { return markers[i].Time < markers[j].Time })
	if err := g.resolveReplayIssueMarkers(c.Context(), project.ID, markers); err != nil {
		g.logger.Error(
			"resolve replay issue markers failed",
			"project",
			project.Slug,
			"session_id",
			row.SessionID,
			"error",
			err,
		)
		return fiber.NewError(http.StatusInternalServerError, "resolve replay Issue markers")
	}
	return c.JSON(struct {
		replaySessionDTO
		Segments []replaySegmentDTO `json:"segments"`
		Markers  []replayMarkerDTO  `json:"markers"`
	}{replaySessionDTO: replaySessionFromMeta(row), Segments: segments, Markers: markers})
}

func (g *Gateway) readReplaySegmentIndex(ctx context.Context, key string) (replaySegmentIndex, error) {
	r, err := g.blobs.Get(ctx, key)
	if err != nil {
		return replaySegmentIndex{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(r, maxReplaySegmentMetaBytes+1))
	closeErr := r.Close()
	if readErr != nil || closeErr != nil {
		return replaySegmentIndex{}, errors.Join(readErr, closeErr)
	}
	if len(data) > maxReplaySegmentMetaBytes {
		return replaySegmentIndex{}, errors.New("replay segment summary exceeds limit")
	}
	var idx replaySegmentIndex
	if err := json.Unmarshal(data, &idx); err != nil {
		return replaySegmentIndex{}, err
	}
	return idx, nil
}

func (g *Gateway) resolveReplayIssueMarkers(ctx context.Context, projectID int, markers []replayMarkerDTO) error {
	if g.issues == nil {
		return nil
	}
	eventIDs := make([]string, 0)
	seen := make(map[string]struct{})
	for _, marker := range markers {
		if marker.Type != "error" || marker.EventID == "" || marker.IssueID != "" {
			continue
		}
		if _, ok := seen[marker.EventID]; ok {
			continue
		}
		seen[marker.EventID] = struct{}{}
		eventIDs = append(eventIDs, marker.EventID)
	}
	if len(eventIDs) == 0 {
		return nil
	}
	resolved := make(map[string]string, len(eventIDs))
	if batch, ok := g.issues.(replayIssueBatchLookup); ok {
		mapping, err := batch.IssueIDsForErrorEvents(ctx, projectID, eventIDs)
		if err != nil {
			return err
		}
		for eventID, issueID := range mapping {
			resolved[eventID] = strconv.Itoa(issueID)
		}
	} else {
		for _, eventID := range eventIDs {
			issueID, found, err := g.issues.IssueIDForErrorEvent(ctx, projectID, eventID)
			if err != nil {
				return err
			}
			if found {
				resolved[eventID] = strconv.Itoa(issueID)
			}
		}
	}
	for index := range markers {
		marker := &markers[index]
		if marker.Type != "error" || marker.EventID == "" || marker.IssueID != "" {
			continue
		}
		if issueID, ok := resolved[marker.EventID]; ok {
			marker.IssueID = issueID
		}
	}
	return nil
}

func (g *Gateway) getReplaySegment(c fiber.Ctx) error {
	row, ok := g.replaySession(c)
	if !ok {
		return nil
	}
	seq, err := strconv.Atoi(c.Params("seq"))
	if err != nil || seq < 0 || seq >= maxReplaySegmentsPerSession {
		return badRequest(c)
	}
	project := currentProject(c)
	key := fmt.Sprintf("%s%d.json.gz", replayPrefix(project.Slug, row.SessionID), seq)
	r, err := g.blobs.Get(c.Context(), key)
	if errors.Is(err, blob.ErrNotFound) {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	if err != nil {
		g.logger.Error("read replay segment blob failed", "project", project.Slug, "key", key, "error", err)
		return fiber.NewError(http.StatusInternalServerError, "read replay segment")
	}
	body, readErr := io.ReadAll(io.LimitReader(r, maxReplaySegment+1))
	closeErr := r.Close()
	if readErr != nil || closeErr != nil {
		g.logger.Error(
			"stream replay segment blob failed",
			"project",
			project.Slug,
			"key",
			key,
			"read_err",
			readErr,
			"close_err",
			closeErr,
		)
		return fiber.NewError(http.StatusInternalServerError, "read replay segment")
	}
	if len(body) > maxReplaySegment {
		return fiber.NewError(http.StatusInternalServerError, "read replay segment")
	}
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	c.Set(fiber.HeaderContentEncoding, "gzip")
	return c.Send(body)
}

func (g *Gateway) replaySession(c fiber.Ctx) (*meta.ReplaySession, bool) {
	if g.replays == nil || g.blobs == nil {
		_ = c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "replay storage unavailable"})
		return nil, false
	}
	sessionID := c.Params("session_id")
	if len(sessionID) == 0 || len(sessionID) > maxReplaySessionIDLen || !replayIDPattern.MatchString(sessionID) {
		_ = c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "not found"})
		return nil, false
	}
	row, err := g.replays.GetReplaySession(c.Context(), currentProject(c).ID, sessionID)
	if meta.IsNotFound(err) {
		_ = c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "not found"})
		return nil, false
	}
	if err != nil {
		_ = c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": "internal error"})
		return nil, false
	}
	return row, true
}

func replaySessionFromMeta(row *meta.ReplaySession) replaySessionDTO {
	var release, pageURL string
	if row.Release != nil {
		release = *row.Release
	}
	if row.PageURL != nil {
		pageURL = *row.PageURL
	}
	return replaySessionDTO{
		SessionID:    row.SessionID,
		StartedAt:    row.StartedAt.UnixMilli(),
		LastSeenAt:   row.LastSeenAt.UnixMilli(),
		SegmentCount: row.SegmentCount,
		TotalBytes:   row.TotalBytes,
		HasError:     row.HasError,
		Release:      release,
		PageURL:      pageURL,
	}
}

func parseReplaySegmentEvents(decoded []byte, now time.Time, requireObjects bool) (parsedReplaySegment, bool) {
	fallback := now.UTC().Round(0)
	maxAllowed := fallback.Add(maxIngestClockSkew)

	var rawEvents []json.RawMessage
	if err := json.Unmarshal(decoded, &rawEvents); err != nil || rawEvents == nil {
		return parsedReplaySegment{}, false
	}
	var (
		startedAt  time.Time
		lastSeenAt time.Time
		segStartMs int64
		pageURL    string
		release    string
	)
	markers := make([]replayMarkerDTO, 0)
	for _, rawEvent := range rawEvents {
		trimmed := bytes.TrimSpace(rawEvent)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			if requireObjects {
				return parsedReplaySegment{}, false
			}
			continue
		}
		ev, ok := decodeReplayEventEnvelope(trimmed)
		if !ok {
			continue
		}
		if ev.HasTime {
			clamped := clampReplayTimestamp(ev.Timestamp, fallback, maxAllowed)
			ms := clamped.UnixMilli()
			if segStartMs <= 0 || ms < segStartMs {
				segStartMs = ms
			}
			if startedAt.IsZero() || clamped.Before(startedAt) {
				startedAt = clamped
			}
			if lastSeenAt.IsZero() || clamped.After(lastSeenAt) {
				lastSeenAt = clamped
			}
			ev.Timestamp = ms
		}
		if pageURL == "" || release == "" {
			evPage, evRelease := extractReplayEventFields(ev)
			if pageURL == "" && evPage != "" {
				pageURL = evPage
			}
			if release == "" && evRelease != "" {
				release = evRelease
			}
		}
		if ev.Type == 5 && len(markers) < maxReplayMarkersPerSegment {
			if marker, ok := replayMarker(ev, fallback, maxAllowed); ok {
				markers = append(markers, marker)
			}
		}
	}
	if startedAt.IsZero() {
		startedAt = fallback
		lastSeenAt = fallback
	}
	if segStartMs <= 0 {
		segStartMs = fallback.UnixMilli()
	}
	return parsedReplaySegment{
		startedAt:  startedAt,
		lastSeenAt: lastSeenAt,
		segStartMs: segStartMs,
		eventCount: len(rawEvents),
		pageURL:    pageURL,
		release:    release,
		markers:    markers,
	}, true
}

func decodeReplayEventEnvelope(raw []byte) (replayEventEnvelope, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return replayEventEnvelope{}, false
	}
	var env replayEventEnvelope
	if rawType, ok := fields["type"]; ok {
		var t int
		if json.Unmarshal(rawType, &t) == nil {
			env.Type = t
		}
	}
	if rawTS, ok := fields["timestamp"]; ok {
		if ts, ok := parseSafeReplayMs(rawTS); ok {
			env.Timestamp = ts
			env.HasTime = true
		}
	}
	if rawData, ok := fields["data"]; ok {
		env.RawData = rawData
		var data struct {
			Tag     string          `json:"tag"`
			Payload json.RawMessage `json:"payload"`
		}
		if json.Unmarshal(rawData, &data) == nil {
			env.Tag = data.Tag
			env.Payload = data.Payload
		}
	}
	return env, true
}

func parseSafeReplayMs(raw json.RawMessage) (int64, bool) {
	var f float64
	if json.Unmarshal(raw, &f) != nil {
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 || f > maxReplayTimestampMs {
		return 0, false
	}
	return int64(f), true
}

func clampReplayTimestamp(ms int64, fallback, maxAllowed time.Time) time.Time {
	at := time.UnixMilli(ms).UTC().Round(0)
	if at.Before(minIngestTimestamp) {
		return fallback
	}
	if at.After(maxAllowed) {
		return fallback
	}
	return at
}

func extractReplayEventFields(ev replayEventEnvelope) (pageURL, release string) {
	switch ev.Type {
	case 4:
		var metaData struct {
			Href    string `json:"href"`
			Release string `json:"release"`
		}
		if json.Unmarshal(ev.RawData, &metaData) == nil {
			if metaData.Href != "" {
				pageURL = sanitizeReplayPageURL(metaData.Href)
			}
			if metaData.Release != "" {
				release = truncateReplayField(metaData.Release, 255)
			}
		}
	case 5:
		switch ev.Tag {
		case "lazy-observe:page":
			var payload struct {
				PageURL string `json:"page_url"`
				Page    string `json:"page"`
				Href    string `json:"href"`
				Release string `json:"release"`
			}
			if json.Unmarshal(ev.Payload, &payload) == nil {
				rawURL := payload.PageURL
				if rawURL == "" {
					rawURL = payload.Page
				}
				if rawURL == "" {
					rawURL = payload.Href
				}
				if rawURL != "" {
					pageURL = sanitizeReplayPageURL(rawURL)
				}
				if payload.Release != "" {
					release = truncateReplayField(payload.Release, 255)
				}
			}
		default:
			if strings.HasPrefix(ev.Tag, "lazy-observe:") {
				var payload struct {
					Release string `json:"release"`
				}
				if json.Unmarshal(ev.Payload, &payload) == nil && payload.Release != "" {
					release = truncateReplayField(payload.Release, 255)
				}
			}
		}
	}
	return pageURL, release
}

func sanitizeReplayPageURL(raw string) string {
	value := sanitizeReplayText(raw)
	if value == "" {
		return ""
	}
	if parsed, err := url.Parse(value); err == nil {
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		parsed.RawFragment = ""
		value = parsed.String()
	} else if idx := strings.IndexAny(value, "?#"); idx >= 0 {
		value = value[:idx]
	}
	return truncateReplayField(value, 2048)
}

func sanitizeReplayText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if !utf8.ValidString(value) {
		value = strings.ToValidUTF8(value, "")
	}
	if strings.ContainsRune(value, 0) {
		value = strings.ReplaceAll(value, "\x00", "")
	}
	return strings.TrimSpace(value)
}

func (g *Gateway) replaySegmentMetadata(
	ctx context.Context,
	object blob.Object,
	seq int,
	fallback time.Time,
) (replaySegmentDTO, []replayMarkerDTO, error) {
	r, err := g.blobs.Get(ctx, object.Key)
	if err != nil {
		return replaySegmentDTO{}, nil, fmt.Errorf("read replay segment %d: %w", seq, err)
	}
	raw, readErr := io.ReadAll(io.LimitReader(r, maxReplaySegment+1))
	closeErr := r.Close()
	if readErr != nil || closeErr != nil {
		return replaySegmentDTO{}, nil, fmt.Errorf("read replay segment %d: %w", seq, errors.Join(readErr, closeErr))
	}
	decoded, err := gunzipBounded(raw, maxReplaySegment)
	if err != nil {
		return replaySegmentDTO{}, nil, fmt.Errorf("decompress replay segment %d: %w", seq, err)
	}
	parsed, ok := parseReplaySegmentEvents(decoded, fallback, false)
	if !ok {
		return replaySegmentDTO{}, nil, fmt.Errorf("decode replay segment %d", seq)
	}
	return replaySegmentDTO{
		Seq:        seq,
		StartedAt:  parsed.segStartMs,
		EventCount: parsed.eventCount,
		SizeBytes:  object.Size,
	}, parsed.markers, nil
}

func replayMarker(event replayEventEnvelope, fallback, maxAllowed time.Time) (replayMarkerDTO, bool) {
	switch event.Tag {
	case "lazy-observe:error":
		return replayErrorMarker(event, fallback, maxAllowed), true
	case "lazy-observe:http":
		return replayHTTPMarker(event, fallback, maxAllowed)
	case "lazy-observe:vital":
		return replayVitalMarker(event, fallback, maxAllowed)
	default:
		return replayMarkerDTO{}, false
	}
}

func replayErrorMarker(event replayEventEnvelope, fallback, maxAllowed time.Time) replayMarkerDTO {
	var payload struct {
		Timestamp json.RawMessage `json:"timestamp"`
		IssueID   string          `json:"issue_id"`
		EventID   string          `json:"event_id"`
		Title     string          `json:"title"`
		Message   string          `json:"message"`
		Level     string          `json:"level"`
	}
	_ = json.Unmarshal(event.Payload, &payload)
	at := replayMarkerTime(payload.Timestamp, event.Timestamp, fallback, maxAllowed)
	title := strings.TrimSpace(payload.Title)
	if title == "" {
		title = strings.TrimSpace(payload.Message)
	}
	if title == "" {
		title = "Browser error"
	}
	level := strings.ToLower(strings.TrimSpace(payload.Level))
	if level != "warning" && level != "info" {
		level = "error"
	}
	return replayMarkerDTO{
		Type: "error", Time: at,
		IssueID: truncateReplayField(payload.IssueID, 255),
		EventID: truncateReplayField(payload.EventID, 255),
		Title:   truncateReplayField(title, 512), Level: level,
	}
}

func replayHTTPMarker(event replayEventEnvelope, fallback, maxAllowed time.Time) (replayMarkerDTO, bool) {
	var payload struct {
		Timestamp  json.RawMessage `json:"timestamp"`
		Method     string          `json:"method"`
		Path       string          `json:"path"`
		Status     int             `json:"status"`
		DurationMS float64         `json:"duration_ms"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil {
		return replayMarkerDTO{}, false
	}
	method := strings.ToUpper(strings.TrimSpace(payload.Method))
	if !replayMethodPattern.MatchString(method) {
		return replayMarkerDTO{}, false
	}
	if payload.Status < 0 || payload.Status > 599 || math.IsNaN(payload.DurationMS) ||
		math.IsInf(payload.DurationMS, 0) ||
		payload.DurationMS < 0 {
		return replayMarkerDTO{}, false
	}
	status, duration := payload.Status, math.Min(payload.DurationMS, float64((24*time.Hour)/time.Millisecond))
	return replayMarkerDTO{
		Type: "http", Time: replayMarkerTime(payload.Timestamp, event.Timestamp, fallback, maxAllowed),
		Method: method, Path: replaySafePath(payload.Path), Status: &status, DurationMS: &duration,
	}, true
}

func replayVitalMarker(event replayEventEnvelope, fallback, maxAllowed time.Time) (replayMarkerDTO, bool) {
	var payload struct {
		Timestamp json.RawMessage `json:"timestamp"`
		Name      string          `json:"name"`
		Value     float64         `json:"value"`
		Rating    string          `json:"rating"`
	}
	if json.Unmarshal(event.Payload, &payload) != nil {
		return replayMarkerDTO{}, false
	}
	name := strings.ToUpper(strings.TrimSpace(payload.Name))
	switch name {
	case "LCP", "CLS", "INP", "TTFB", "FCP":
	default:
		return replayMarkerDTO{}, false
	}
	rating := strings.ToLower(strings.TrimSpace(payload.Rating))
	if rating != "good" && rating != "needs-improvement" && rating != "poor" {
		return replayMarkerDTO{}, false
	}
	if math.IsNaN(payload.Value) || math.IsInf(payload.Value, 0) || payload.Value < 0 {
		return replayMarkerDTO{}, false
	}
	value := payload.Value
	return replayMarkerDTO{
		Type: "vital", Time: replayMarkerTime(payload.Timestamp, event.Timestamp, fallback, maxAllowed),
		Name: name, Value: &value, Rating: rating,
	}, true
}

func replayMarkerTime(raw json.RawMessage, eventTimestamp int64, fallback, maxAllowed time.Time) int64 {
	if parsed, ok := replayMarkerTimestamp(raw); ok {
		return clampReplayTimestamp(parsed, fallback, maxAllowed).UnixMilli()
	}
	if eventTimestamp > 0 {
		return clampReplayTimestamp(eventTimestamp, fallback, maxAllowed).UnixMilli()
	}
	return fallback.UnixMilli()
}

func replayMarkerTimestamp(raw json.RawMessage) (int64, bool) {
	if value, ok := parseSafeReplayMs(raw); ok {
		return value, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			ms := parsed.UTC().UnixMilli()
			if ms > 0 && ms <= maxReplayTimestampMs {
				return ms, true
			}
		}
	}
	return 0, false
}

func replaySafePath(raw string) string {
	value := sanitizeReplayText(raw)
	if parsed, err := url.Parse(value); err == nil {
		value = parsed.Path
	} else {
		if index := strings.IndexAny(value, "?#"); index >= 0 {
			value = value[:index]
		}
		authorityStart := -1
		if scheme := strings.Index(value, "://"); scheme >= 0 {
			authorityStart = scheme + 3
		} else if strings.HasPrefix(value, "//") {
			authorityStart = 2
		}
		if authorityStart >= 0 {
			if pathStart := strings.Index(value[authorityStart:], "/"); pathStart >= 0 {
				value = value[authorityStart+pathStart:]
			} else {
				value = "/"
			}
		}
	}
	if index := strings.IndexAny(value, "?#"); index >= 0 {
		value = value[:index]
	}
	if value == "" {
		value = "/"
	} else if !strings.HasPrefix(value, "/") {
		value = "/" + value
	}
	return truncateReplayField(value, 2048)
}

func truncateReplayField(value string, limit int) string {
	value = sanitizeReplayText(value)
	if len(value) > limit {
		value = value[:limit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}

func replayPrefix(slug, sessionID string) string { return "replay/" + slug + "/" + sessionID + "/" }

func replaySegmentMetaKey(prefix string, seq int) string {
	return fmt.Sprintf("%s%d.meta.json", prefix, seq)
}

func (g *Gateway) replayLock(projectID int, sessionID string) *sync.Mutex {
	h := fnv.New32a()
	_, _ = fmt.Fprintf(h, "%d/%s", projectID, sessionID)
	return &g.replayLocks[h.Sum32()%uint32(len(g.replayLocks))]
}

func replaySeq(key string) (int, bool) {
	name := path.Base(key)
	value := strings.TrimSuffix(name, ".json.gz")
	if value == name {
		return 0, false
	}
	seq, err := strconv.Atoi(value)
	return seq, err == nil && seq >= 0
}

func replayMetaSeq(key string) (int, bool) {
	name := path.Base(key)
	value := strings.TrimSuffix(name, ".meta.json")
	if value == name {
		return 0, false
	}
	seq, err := strconv.Atoi(value)
	return seq, err == nil && seq >= 0
}
