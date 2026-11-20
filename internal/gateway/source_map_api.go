package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/eimsound/lazy-observe/internal/symbolicate"
	"github.com/gofiber/fiber/v3"
)

const (
	maxSourceMapFiles = 200
	maxSourceMapBytes = 50 << 20
)

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
	release := strings.TrimSpace(c.Params("release"))
	if !sourceMapComponent.MatchString(release) {
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
		filename := path.Base(strings.ReplaceAll(strings.TrimSpace(field), "\\", "/"))
		if len(headers) != 1 || !strings.HasSuffix(strings.ToLower(headers[0].Filename), ".map") ||
			!sourceMapComponent.MatchString(filename) ||
			filename == "." ||
			seen[filename] {
			return c.Status(http.StatusBadRequest).
				JSON(fiber.Map{"error": "each field must be a unique minified filename with one .map file"})
		}
		seen[filename] = true
		f, openErr := headers[0].Open()
		if openErr != nil {
			return fiber.NewError(http.StatusInternalServerError, openErr.Error())
		}
		data, readErr := io.ReadAll(io.LimitReader(f, maxSourceMapBytes+1))
		closeErr := f.Close()
		if readErr != nil || closeErr != nil {
			return c.Status(http.StatusBadRequest).JSON(fiber.Map{"error": "read source map"})
		}
		incomingBytes += int64(len(data))
		if incomingBytes > maxSourceMapBytes {
			return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "release exceeds 50MB"})
		}
		sum := sha256.Sum256(data)
		uploads = append(uploads, sourceMapUpload{filename: filename, data: data, digest: hex.EncodeToString(sum[:])})
	}
	if len(uploads) > maxSourceMapFiles {
		return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "release exceeds 200 files"})
	}
	p := currentProject(c)
	existing, err := g.sourceMaps.SourceMapArtifacts(c.Context(), p.ID, release)
	if err != nil {
		return fiber.NewError(http.StatusInternalServerError, err.Error())
	}
	count, total := len(existing), int64(0)
	for _, artifact := range existing {
		total += artifact.Size
		if seen[artifact.Filename] {
			count--
			total -= artifact.Size
		}
	}
	if count+len(uploads) > maxSourceMapFiles || total+incomingBytes > maxSourceMapBytes {
		return c.Status(http.StatusRequestEntityTooLarge).JSON(fiber.Map{"error": "release archive limit exceeded"})
	}
	for _, upload := range uploads {
		key := sourceMapRevisionKey(p.Slug, upload.digest)
		if err := g.blobs.Put(c.Context(), key, bytes.NewReader(upload.data)); err != nil {
			return fiber.NewError(http.StatusInternalServerError, err.Error())
		}
		if err := g.sourceMaps.UpsertSourceMapArtifact(
			c.Context(),
			p.ID,
			release,
			upload.filename,
			int64(len(upload.data)),
			upload.digest,
			g.now().UTC(),
		); err != nil {
			return fiber.NewError(http.StatusInternalServerError, err.Error())
		}
	}
	return c.Status(http.StatusCreated).
		JSON(fiber.Map{"release": release, "uploaded": len(uploads), "size": incomingBytes})
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
	release := strings.TrimSpace(c.Params("release"))
	if !sourceMapComponent.MatchString(release) {
		return badRequest(c)
	}
	p := currentProject(c)
	if err := g.sourceMaps.DeleteSourceMapArtifacts(c.Context(), p.ID, release); err != nil {
		return apiError(c, err)
	}
	return c.SendStatus(http.StatusNoContent)
}

func sourceMapRevisionKey(project, digest string) string {
	return symbolicate.RevisionKey(project, digest)
}
