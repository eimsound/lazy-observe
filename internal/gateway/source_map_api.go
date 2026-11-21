package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/symbolicate"
	"github.com/gofiber/fiber/v3"
)

const (
	maxSourceMapFiles            = 200
	maxSourceMapBytes            = 50 << 20
	maxSourceMapFilenameSize     = 512
	defaultSourceMapUploadPinTTL = 15 * time.Minute
	sourceMapPinReleaseTimeout   = 5 * time.Second
)

type sourceMapPinnedUploadStore interface {
	PinSourceMapUploads(context.Context, int, []string, time.Duration) (string, error)
	ReleaseSourceMapUploadPins(context.Context, int, string) error
	CommitPinnedSourceMapArtifacts(
		context.Context,
		int,
		string,
		string,
		[]meta.SourceMapArtifactInput,
		int,
		int64,
		time.Time,
	) error
}

var sourceMapComponent = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

type sourceMapUpload struct {
	filename string
	data     []byte
	digest   string
}

func (g *Gateway) uploadSourceMaps(c fiber.Ctx) error {
	if g.sourceMaps == nil || g.blobs == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "source map storage unavailable"})
	}
	release, err := unescapedPathParam(c, "release", validateSourceMapRelease)
	if err != nil {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid release"})
	}
	form, err := c.MultipartForm()
	if err != nil || len(form.File) == 0 {
		return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "multipart source maps required"})
	}
	uploads := make([]sourceMapUpload, 0, len(form.File))
	seen := make(map[string]bool)
	var incomingBytes int64
	for field, headers := range form.File {
		filename, validName := normalizeSourceMapFilename(field)
		if len(headers) != 1 || !strings.HasSuffix(strings.ToLower(headers[0].Filename), ".map") ||
			!validName ||
			seen[filename] {
			return c.Status(http.StatusBadRequest).
				JSON(fiber.Map{"error": "each field must be a unique minified filename with one .map file"})
		}
		seen[filename] = true
		f, openErr := headers[0].Open()
		if openErr != nil {
			g.logger.Warn("open uploaded source map failed", "filename", filename, "error", openErr)
			return fiber.NewError(http.StatusInternalServerError, "open uploaded source map failed")
		}
		raw, readErr := io.ReadAll(io.LimitReader(f, maxSourceMapBytes+1))
		closeErr := f.Close()
		if readErr != nil || closeErr != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "read source map"})
		}
		if int64(len(raw)) > maxSourceMapBytes {
			return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "release exceeds 50MB"})
		}
		normalized, _, validateErr := symbolicate.ValidateAndNormalize(raw)
		if validateErr != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "invalid source map v3"})
		}
		incomingBytes += int64(len(normalized))
		if incomingBytes > maxSourceMapBytes {
			return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "release exceeds 50MB"})
		}
		sum := sha256.Sum256(normalized)
		uploads = append(
			uploads,
			sourceMapUpload{filename: filename, data: normalized, digest: hex.EncodeToString(sum[:])},
		)
	}
	if len(uploads) > maxSourceMapFiles {
		return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "release exceeds 200 files"})
	}
	p := currentProject(c)
	items := make([]meta.SourceMapArtifactInput, 0, len(uploads))
	uniqueUploads := make([]sourceMapUpload, 0, len(uploads))
	uniqueDigests := make([]string, 0, len(uploads))
	seenDigests := make(map[string]struct{}, len(uploads))
	for _, upload := range uploads {
		items = append(items, meta.SourceMapArtifactInput{
			Filename: upload.filename,
			Size:     int64(len(upload.data)),
			SHA256:   upload.digest,
		})
		if _, dup := seenDigests[upload.digest]; dup {
			continue
		}
		seenDigests[upload.digest] = struct{}{}
		uniqueUploads = append(uniqueUploads, upload)
		uniqueDigests = append(uniqueDigests, upload.digest)
	}
	pinStore, hasPinStore := g.sourceMaps.(sourceMapPinnedUploadStore)
	pinTTL := g.sourceMapUploadPinTTL
	if pinTTL <= 0 {
		pinTTL = defaultSourceMapUploadPinTTL
	}
	var pinToken string
	committed := false
	if hasPinStore {
		var pinErr error
		pinToken, pinErr = pinStore.PinSourceMapUploads(
			c.Context(),
			p.ID,
			uniqueDigests,
			pinTTL,
		)
		if pinErr != nil {
			g.logger.Warn("pin source map upload failed", "project", p.Slug, "release", release, "error", pinErr)
			return fiber.NewError(http.StatusInternalServerError, "persist source map metadata failed")
		}
		defer func() {
			if committed || pinToken == "" {
				return
			}
			releaseCtx, cancel := context.WithTimeout(
				context.WithoutCancel(c.Context()),
				sourceMapPinReleaseTimeout,
			)
			defer cancel()
			if releaseErr := pinStore.ReleaseSourceMapUploadPins(releaseCtx, p.ID, pinToken); releaseErr != nil {
				g.logger.Warn(
					"release source map upload pins failed",
					"project", p.Slug,
					"release", release,
					"error", releaseErr,
				)
			}
		}()
	}
	for _, upload := range uniqueUploads {
		key := sourceMapRevisionKey(p.Slug, upload.digest)
		if err := g.blobs.Put(c.Context(), key, bytes.NewReader(upload.data)); err != nil {
			g.logger.Warn("store source map blob failed", "project", p.Slug, "release", release, "error", err)
			return fiber.NewError(http.StatusInternalServerError, "store source map failed")
		}
	}
	var persistErr error
	if hasPinStore {
		persistErr = pinStore.CommitPinnedSourceMapArtifacts(
			c.Context(),
			p.ID,
			release,
			pinToken,
			items,
			maxSourceMapFiles,
			maxSourceMapBytes,
			g.now().UTC(),
		)
	} else {
		persistErr = g.sourceMaps.UpsertSourceMapArtifacts(
			c.Context(),
			p.ID,
			release,
			items,
			maxSourceMapFiles,
			maxSourceMapBytes,
			g.now().UTC(),
		)
	}
	if persistErr != nil {
		if errors.Is(persistErr, meta.ErrSourceMapQuotaExceeded) {
			return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "release archive limit exceeded"})
		}
		if errors.Is(persistErr, meta.ErrSourceMapUploadPinExpired) {
			g.logger.Warn(
				"source map upload pin expired before commit",
				"project", p.Slug,
				"project_id", p.ID,
				"release", release,
				"files", len(items),
				"unique_digests", len(uniqueDigests),
				"pin_ttl", pinTTL,
			)
			return c.Status(http.StatusServiceUnavailable).
				JSON(fiber.Map{"error": "source map upload expired; retry upload"})
		}
		g.logger.Warn("persist source map metadata failed", "project", p.Slug, "release", release, "error", persistErr)
		return fiber.NewError(http.StatusInternalServerError, "persist source map metadata failed")
	}
	committed = true
	return c.Status(http.StatusCreated).
		JSON(fiber.Map{"release": release, "uploaded": len(uploads), "size": incomingBytes})
}

// normalizeSourceMapFilename accepts either a bare minified filename (e.g.
// "app.min.js") or a clean relative bundle path (e.g. "chunks/index.js") while
// rejecting traversal segments and unsafe characters.
func normalizeSourceMapFilename(field string) (string, bool) {
	cleaned := strings.ReplaceAll(strings.TrimSpace(field), "\\", "/")
	cleaned = strings.TrimLeft(cleaned, "/")
	if cleaned == "" || len(cleaned) > maxSourceMapFilenameSize {
		return "", false
	}
	cleaned = path.Clean(cleaned)
	if cleaned == "" || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	parts := strings.Split(cleaned, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || !sourceMapComponent.MatchString(part) {
			return "", false
		}
	}
	return cleaned, true
}

func (g *Gateway) listSourceMapReleases(c fiber.Ctx) error {
	if g.sourceMaps == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "source map storage unavailable"})
	}
	limit, _ := strconv.Atoi(c.Query("limit"))
	releases, err := g.sourceMaps.ListSourceMapReleases(c.Context(), currentProject(c).ID, limit)
	if err != nil {
		return apiError(c, err)
	}
	return c.JSON(fiber.Map{"releases": releases})
}

func (g *Gateway) deleteSourceMaps(c fiber.Ctx) error {
	if g.sourceMaps == nil || g.blobs == nil {
		return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": "source map storage unavailable"})
	}
	release, releaseErr := unescapedPathParam(c, "release", validateSourceMapRelease)
	p := currentProject(c)
	auditTarget := release
	if releaseErr != nil {
		auditTarget = strings.TrimSpace(c.Params("release"))
	}
	audit := auditEvent{
		Action:      "source_map.delete",
		TargetType:  "source_map",
		TargetName:  auditTarget,
		ProjectID:   p.ID,
		ProjectSlug: p.Slug,
	}
	if releaseErr != nil {
		audit.FailureReason = "invalid_request"
		g.auditManagement(c, audit)
		return badRequest(c)
	}
	if err := g.sourceMaps.DeleteSourceMapArtifacts(c.Context(), p.ID, release); err != nil {
		audit.FailureReason = "delete_failed"
		g.auditManagement(c, audit)
		return apiError(c, err)
	}
	audit.Success = true
	g.auditManagement(c, audit)
	return c.SendStatus(http.StatusNoContent)
}

func sourceMapRevisionKey(project, digest string) string {
	return symbolicate.RevisionKey(project, digest)
}
