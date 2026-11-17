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
	"github.com/gofiber/fiber/v2"
)

const (
	maxReplaySegment = 5 << 20
	replayPageSize   = 50
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

type replayEventEnvelope struct {
	Type      int   `json:"type"`
	Timestamp int64 `json:"timestamp"`
	Data      struct {
		Tag     string          `json:"tag"`
		Payload json.RawMessage `json:"payload"`
	} `json:"data"`
}

func (g *Gateway) replayIngest(c *fiber.Ctx) error {
	if g.replays == nil || g.blobs == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "replay storage unavailable"})
	}
	sessionID := strings.TrimSpace(c.Query("session_id"))
	seq, err := strconv.Atoi(c.Query("seq"))
	if !replayIDPattern.MatchString(sessionID) || err != nil || seq < 0 {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid session_id or seq"})
	}
	body := c.Body()
	var decoded []byte
	if len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "invalid gzip segment"})
		}
		var readErr error
		decoded, readErr = io.ReadAll(io.LimitReader(zr, maxReplaySegment+1))
		closeErr := zr.Close()
		if readErr != nil || closeErr != nil {
			return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "invalid gzip segment"})
		}
	} else if encoding, _ := c.Locals(decodedContentEncodingLocal).(string); encoding == "gzip" ||
		encoding == "x-gzip" {
		// boundedRequestDecoding already removed Content-Encoding: gzip.
		decoded = append([]byte(nil), body...)
	} else {
		return c.Status(http.StatusUnprocessableEntity).JSON(fiber.Map{"error": "segment body must be gzip"})
	}
	if len(decoded) > maxReplaySegment {
		return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "segment exceeds 5MB"})
	}
	var events []any
	if err := json.Unmarshal(decoded, &events); err != nil || events == nil {
		return c.Status(http.StatusUnprocessableEntity).
			JSON(fiber.Map{"error": "segment must contain a JSON event array"})
	}
	startedAt, lastSeenAt := replayEventRange(events, g.now().UTC())
	p := currentProject(c)
	lock := g.replayLock(p.ID, sessionID)
	lock.Lock()
	defer lock.Unlock()
	prefix := replayPrefix(p.Slug, sessionID)
	key := fmt.Sprintf("%s%d.json.gz", prefix, seq)
	var stored bytes.Buffer
	zw := gzip.NewWriter(&stored)
	_, writeErr := zw.Write(decoded)
	closeErr := zw.Close()
	if writeErr != nil || closeErr != nil {
		return fiber.NewError(http.StatusInternalServerError, "compress replay segment")
	}
	if err := g.blobs.Put(c.UserContext(), key, bytes.NewReader(stored.Bytes())); err != nil {
		if errors.Is(err, blob.ErrInvalidKey) {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid replay key"})
		}
		return fiber.NewError(http.StatusInternalServerError, err.Error())
	}
	objects, err := g.blobs.List(c.UserContext(), prefix)
	if err != nil {
		return fiber.NewError(http.StatusInternalServerError, err.Error())
	}
	var total int64
	for _, object := range objects {
		total += object.Size
	}
	var pageURL, release string
	if seq == 0 {
		pageURL, release = replayFields(events)
	}
	if err := g.replays.UpsertReplaySession(
		c.UserContext(),
		p.ID,
		sessionID,
		startedAt,
		lastSeenAt,
		len(objects),
		total,
		c.Get("X-LO-Replay-Error") == "1",
		release,
		pageURL,
	); err != nil {
		return fiber.NewError(http.StatusInternalServerError, err.Error())
	}
	return c.Status(http.StatusAccepted).JSON(fiber.Map{"accepted": true, "seq": seq})
}

func (g *Gateway) listReplaySessions(c *fiber.Ctx) error {
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
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return badRequest(c)
		}
		opts.Start = &parsed
	}
	if value := strings.TrimSpace(c.Query("end")); value != "" {
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return badRequest(c)
		}
		opts.End = &parsed
	}
	if value := c.Query("page"); value != "" {
		page, err := strconv.Atoi(value)
		if err != nil || page < 1 {
			return badRequest(c)
		}
		opts.Page = page
	}
	rows, err := g.replays.ListReplaySessions(c.UserContext(), currentProject(c).ID, opts)
	if err != nil {
		return apiError(c, err)
	}
	total, err := g.replays.CountReplaySessions(c.UserContext(), currentProject(c).ID, opts)
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

func (g *Gateway) getReplaySession(c *fiber.Ctx) error {
	row, ok := g.replaySession(c)
	if !ok {
		return nil
	}
	objects, err := g.blobs.List(c.UserContext(), replayPrefix(currentProject(c).Slug, row.SessionID))
	if err != nil {
		return fiber.NewError(http.StatusInternalServerError, err.Error())
	}
	segments := make([]replaySegmentDTO, 0, len(objects))
	markers := make([]replayMarkerDTO, 0)
	for _, object := range objects {
		seq, ok := replaySeq(object.Key)
		if !ok {
			continue
		}
		segment, segmentMarkers, err := g.replaySegmentMetadata(c.UserContext(), object, seq, row.StartedAt)
		if err != nil {
			return fiber.NewError(http.StatusInternalServerError, err.Error())
		}
		segments = append(segments, segment)
		markers = append(markers, segmentMarkers...)
	}
	sort.Slice(segments, func(i, j int) bool { return segments[i].Seq < segments[j].Seq })
	sort.Slice(markers, func(i, j int) bool { return markers[i].Time < markers[j].Time })
	if err := g.resolveReplayIssueMarkers(c.UserContext(), currentProject(c).ID, markers); err != nil {
		return fiber.NewError(http.StatusInternalServerError, "resolve replay Issue markers")
	}
	return c.JSON(struct {
		replaySessionDTO
		Segments []replaySegmentDTO `json:"segments"`
		Markers  []replayMarkerDTO  `json:"markers"`
	}{replaySessionDTO: replaySessionFromMeta(row), Segments: segments, Markers: markers})
}

func (g *Gateway) resolveReplayIssueMarkers(ctx context.Context, projectID int, markers []replayMarkerDTO) error {
	if g.issues == nil {
		return nil
	}
	resolved := make(map[string]string)
	for index := range markers {
		marker := &markers[index]
		if marker.Type != "error" || marker.EventID == "" || marker.IssueID != "" {
			continue
		}
		if issueID, ok := resolved[marker.EventID]; ok {
			marker.IssueID = issueID
			continue
		}
		issueID, ok, err := g.issues.IssueIDForErrorEvent(ctx, projectID, marker.EventID)
		if err != nil {
			return err
		}
		if !ok {
			resolved[marker.EventID] = ""
			continue
		}
		marker.IssueID = strconv.Itoa(issueID)
		resolved[marker.EventID] = marker.IssueID
	}
	return nil
}

func (g *Gateway) getReplaySegment(c *fiber.Ctx) error {
	row, ok := g.replaySession(c)
	if !ok {
		return nil
	}
	seq, err := strconv.Atoi(c.Params("seq"))
	if err != nil || seq < 0 {
		return badRequest(c)
	}
	r, err := g.blobs.Get(
		c.UserContext(),
		fmt.Sprintf("%s%d.json.gz", replayPrefix(currentProject(c).Slug, row.SessionID), seq),
	)
	if errors.Is(err, blob.ErrNotFound) {
		return c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "not found"})
	}
	if err != nil {
		return fiber.NewError(http.StatusInternalServerError, err.Error())
	}
	defer r.Close()
	body, err := io.ReadAll(r)
	if err != nil {
		return fiber.NewError(http.StatusInternalServerError, err.Error())
	}
	c.Set(fiber.HeaderContentType, fiber.MIMEApplicationJSON)
	c.Set(fiber.HeaderContentEncoding, "gzip")
	return c.Send(body)
}

func (g *Gateway) replaySession(c *fiber.Ctx) (*meta.ReplaySession, bool) {
	if g.replays == nil || g.blobs == nil {
		_ = c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "replay storage unavailable"})
		return nil, false
	}
	sessionID := c.Params("session_id")
	if !replayIDPattern.MatchString(sessionID) {
		_ = c.Status(http.StatusNotFound).JSON(fiber.Map{"error": "not found"})
		return nil, false
	}
	row, err := g.replays.GetReplaySession(c.UserContext(), currentProject(c).ID, sessionID)
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

func replayEventRange(events []any, fallback time.Time) (time.Time, time.Time) {
	var startedAt, lastSeenAt time.Time
	for _, value := range events {
		event, ok := value.(map[string]any)
		if !ok {
			continue
		}
		raw, ok := event["timestamp"].(float64)
		if !ok || raw <= 0 {
			continue
		}
		at := time.UnixMilli(int64(raw)).UTC()
		if startedAt.IsZero() || at.Before(startedAt) {
			startedAt = at
		}
		if lastSeenAt.IsZero() || at.After(lastSeenAt) {
			lastSeenAt = at
		}
	}
	if startedAt.IsZero() {
		return fallback, fallback
	}
	return startedAt, lastSeenAt
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
	defer r.Close()
	zr, err := gzip.NewReader(r)
	if err != nil {
		return replaySegmentDTO{}, nil, fmt.Errorf("decompress replay segment %d: %w", seq, err)
	}
	decoded, readErr := io.ReadAll(io.LimitReader(zr, maxReplaySegment+1))
	closeErr := zr.Close()
	if readErr != nil || closeErr != nil || len(decoded) > maxReplaySegment {
		return replaySegmentDTO{}, nil, fmt.Errorf("decompress replay segment %d", seq)
	}
	var events []replayEventEnvelope
	if err := json.Unmarshal(decoded, &events); err != nil || events == nil {
		return replaySegmentDTO{}, nil, fmt.Errorf("decode replay segment %d", seq)
	}
	var startedAt int64
	markers := make([]replayMarkerDTO, 0)
	for _, event := range events {
		if event.Timestamp > 0 && (startedAt <= 0 || event.Timestamp < startedAt) {
			startedAt = event.Timestamp
		}
		if event.Type != 5 {
			continue
		}
		if marker, ok := replayMarker(event, fallback); ok {
			markers = append(markers, marker)
		}
	}
	if startedAt <= 0 {
		startedAt = fallback.UnixMilli()
	}
	return replaySegmentDTO{
		Seq:        seq,
		StartedAt:  startedAt,
		EventCount: len(events),
		SizeBytes:  object.Size,
	}, markers, nil
}

func replayMarker(event replayEventEnvelope, fallback time.Time) (replayMarkerDTO, bool) {
	switch event.Data.Tag {
	case "lazy-observe:error":
		return replayErrorMarker(event, fallback), true
	case "lazy-observe:http":
		return replayHTTPMarker(event, fallback)
	case "lazy-observe:vital":
		return replayVitalMarker(event, fallback)
	default:
		return replayMarkerDTO{}, false
	}
}

func replayErrorMarker(event replayEventEnvelope, fallback time.Time) replayMarkerDTO {
	var payload struct {
		Timestamp json.RawMessage `json:"timestamp"`
		IssueID   string          `json:"issue_id"`
		EventID   string          `json:"event_id"`
		Title     string          `json:"title"`
		Message   string          `json:"message"`
		Level     string          `json:"level"`
	}
	_ = json.Unmarshal(event.Data.Payload, &payload)
	at := replayMarkerTime(payload.Timestamp, event.Timestamp, fallback)
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

func replayHTTPMarker(event replayEventEnvelope, fallback time.Time) (replayMarkerDTO, bool) {
	var payload struct {
		Timestamp  json.RawMessage `json:"timestamp"`
		Method     string          `json:"method"`
		Path       string          `json:"path"`
		Status     int             `json:"status"`
		DurationMS float64         `json:"duration_ms"`
	}
	if json.Unmarshal(event.Data.Payload, &payload) != nil {
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
		Type: "http", Time: replayMarkerTime(payload.Timestamp, event.Timestamp, fallback),
		Method: method, Path: replaySafePath(payload.Path), Status: &status, DurationMS: &duration,
	}, true
}

func replayVitalMarker(event replayEventEnvelope, fallback time.Time) (replayMarkerDTO, bool) {
	var payload struct {
		Timestamp json.RawMessage `json:"timestamp"`
		Name      string          `json:"name"`
		Value     float64         `json:"value"`
		Rating    string          `json:"rating"`
	}
	if json.Unmarshal(event.Data.Payload, &payload) != nil {
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
		Type: "vital", Time: replayMarkerTime(payload.Timestamp, event.Timestamp, fallback),
		Name: name, Value: &value, Rating: rating,
	}, true
}

func replayMarkerTime(raw json.RawMessage, eventTimestamp int64, fallback time.Time) int64 {
	if parsed, ok := replayMarkerTimestamp(raw); ok {
		return parsed
	}
	if eventTimestamp > 0 {
		return eventTimestamp
	}
	return fallback.UnixMilli()
}

func replayMarkerTimestamp(raw json.RawMessage) (int64, bool) {
	var value int64
	if json.Unmarshal(raw, &value) == nil && value > 0 {
		return value, true
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
			return parsed.UnixMilli(), true
		}
	}
	return 0, false
}

func replaySafePath(raw string) string {
	value := strings.TrimSpace(raw)
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
	value = strings.TrimSpace(value)
	if len(value) > limit {
		value = value[:limit]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}

func replayPrefix(slug, sessionID string) string { return "replay/" + slug + "/" + sessionID + "/" }
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

func replayFields(events []any) (pageURL, release string) {
	var visit func(any)
	visit = func(value any) {
		switch v := value.(type) {
		case map[string]any:
			for key, child := range v {
				if text, ok := child.(string); ok {
					switch key {
					case "href", "page", "page_url":
						if pageURL == "" {
							pageURL = text
						}
					case "release":
						if release == "" {
							release = text
						}
					}
				}
				if pageURL == "" || release == "" {
					visit(child)
				}
			}
		case []any:
			for _, child := range v {
				if pageURL != "" && release != "" {
					break
				}
				visit(child)
			}
		}
	}
	visit(events)
	if len(pageURL) > 2048 {
		pageURL = pageURL[:2048]
	}
	if len(release) > 255 {
		release = release[:255]
	}
	return pageURL, release
}
