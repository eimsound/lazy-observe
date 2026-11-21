package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/janitor"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/symbolicate"
)

type hookedBlobStore struct {
	blob.Store

	mu           sync.Mutex
	staleModTime map[string]time.Time
	onPut        func(ctx context.Context, key string)
	onAfterPut   func(ctx context.Context, key string)
	onList       func(ctx context.Context, prefix string)
	onDelete     func(ctx context.Context, key string)
	onAfterDel   func(ctx context.Context, key string)
}

func (h *hookedBlobStore) Put(ctx context.Context, key string, r io.Reader) error {
	h.mu.Lock()
	onPut := h.onPut
	onAfterPut := h.onAfterPut
	h.mu.Unlock()
	if onPut != nil {
		onPut(ctx, key)
	}
	err := h.Store.Put(ctx, key, r)
	if err == nil && onAfterPut != nil {
		onAfterPut(ctx, key)
	}
	return err
}

func (h *hookedBlobStore) List(ctx context.Context, prefix string) ([]blob.Object, error) {
	objects, err := h.Store.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	h.mu.Lock()
	stale := h.staleModTime
	onList := h.onList
	h.mu.Unlock()
	for i := range objects {
		if ts, ok := stale[objects[i].Key]; ok {
			objects[i].ModTime = ts
		}
	}
	if onList != nil {
		onList(ctx, prefix)
	}
	return objects, nil
}

func (h *hookedBlobStore) Delete(ctx context.Context, key string) error {
	h.mu.Lock()
	onDelete := h.onDelete
	onAfterDel := h.onAfterDel
	h.mu.Unlock()
	if onDelete != nil {
		onDelete(ctx, key)
	}
	err := h.Store.Delete(ctx, key)
	if err == nil && onAfterDel != nil {
		onAfterDel(ctx, key)
	}
	return err
}

type hookedSourceMapStore struct {
	*meta.Store

	onBeforePin                  func()
	onAfterPin                   func()
	onAfterReferencedDigests     func()
	onAfterListExpiredReleases   func()
	onBeforeCommitPinnedArtifact func()
}

func (h *hookedSourceMapStore) PinSourceMapUploads(
	ctx context.Context,
	projectID int,
	digests []string,
	ttl time.Duration,
) (string, error) {
	if h.onBeforePin != nil {
		h.onBeforePin()
	}
	token, err := h.Store.PinSourceMapUploads(ctx, projectID, digests, ttl)
	if err == nil && h.onAfterPin != nil {
		h.onAfterPin()
	}
	return token, err
}

func (h *hookedSourceMapStore) CommitPinnedSourceMapArtifacts(
	ctx context.Context,
	projectID int,
	release, pinToken string,
	items []meta.SourceMapArtifactInput,
	maxFiles int,
	maxBytes int64,
	createdAt time.Time,
) error {
	if h.onBeforeCommitPinnedArtifact != nil {
		h.onBeforeCommitPinnedArtifact()
	}
	return h.Store.CommitPinnedSourceMapArtifacts(
		ctx,
		projectID,
		release,
		pinToken,
		items,
		maxFiles,
		maxBytes,
		createdAt,
	)
}

func (h *hookedSourceMapStore) ReferencedSourceMapDigests(
	ctx context.Context,
	projectID int,
) (map[string]bool, error) {
	refs, err := h.Store.ReferencedSourceMapDigests(ctx, projectID)
	if err == nil && h.onAfterReferencedDigests != nil {
		h.onAfterReferencedDigests()
	}
	return refs, err
}

func (h *hookedSourceMapStore) ListExpiredSourceMapReleases(
	ctx context.Context,
	cutoff time.Time,
	limit int,
) ([]meta.ExpiredSourceMapRelease, error) {
	releases, err := h.Store.ListExpiredSourceMapReleases(ctx, cutoff, limit)
	if err == nil && h.onAfterListExpiredReleases != nil {
		h.onAfterListExpiredReleases()
	}
	return releases, err
}

func normalizedTestSourceMap(t *testing.T, raw string) ([]byte, string) {
	t.Helper()
	normalized, _, err := symbolicate.ValidateAndNormalize([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(normalized)
	return normalized, hex.EncodeToString(sum[:])
}

func uploadSourceMapBody(t *testing.T, gw *Gateway, token, release, filename, payload string) (int, string) {
	t.Helper()
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	part, err := w.CreateFormFile(filename, filename+".map")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/releases/"+release+"/sourcemaps", &body)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := gw.App().Test(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(readBody(resp))
}

func waitSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", label)
	}
}

func seedOldBlob(t *testing.T, root string, store blob.Store, key string, data []byte, modTime time.Time) {
	t.Helper()
	if err := store.Put(context.Background(), key, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	diskPath := filepath.Join(root, filepath.FromSlash(key))
	if err := os.Chtimes(diskPath, modTime, modTime); err != nil {
		t.Fatal(err)
	}
}

func createSourceMapRaceProject(t *testing.T, store *meta.Store) *meta.Project {
	t.Helper()
	project, err := store.CreateProject(context.Background(), "SourceMapRace", "sourcemap-race")
	if err != nil {
		t.Fatal(err)
	}
	return project
}

func testStoreWithPath(t *testing.T) (*meta.Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "meta.db")
	store, err := meta.OpenStore(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, dbPath
}

func expireSourceMapUploadPinsSQLite(t *testing.T, dbPath string, projectID int) {
	t.Helper()
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rawDB.Close() }()
	if _, err := rawDB.ExecContext(
		context.Background(),
		`UPDATE source_map_upload_pins
		 SET expires_at = strftime('%Y-%m-%d %H:%M:%f', 'now', '-1 minute')
		 WHERE project_id = ?`,
		projectID,
	); err != nil {
		t.Fatal(err)
	}
}

// Scenario 1: An old-ModTime revision blob (previously orphaned when an older
// release was deleted) is re-referenced by a concurrent upload that has written
// the blob but has not yet committed metadata when janitor.SourceMaps runs.
// Even if blob List still reports the stale ModTime past GracePeriod, the
// active upload pin must prevent deletion while truly orphaned blobs are swept.
func TestSourceMapGCRaceOldModTimeReReferenceBetweenPutAndCommit(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	project := createSourceMapRaceProject(t, store)
	uploadToken := createUploadTokenTest(t, store, project.ID)
	blobRoot := t.TempDir()
	localBlobs, err := blob.NewLocal(blobRoot)
	if err != nil {
		t.Fatal(err)
	}

	baseNow := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	staleTime := baseNow.Add(-2 * time.Hour)
	mapData, digest := normalizedTestSourceMap(t, testSourceMap)
	sharedKey := symbolicate.RevisionKey(project.Slug, digest)
	orphanDigest := strings.Repeat("d", 64)
	orphanKey := symbolicate.RevisionKey(project.Slug, orphanDigest)

	seedOldBlob(t, blobRoot, localBlobs, sharedKey, mapData, staleTime)
	seedOldBlob(
		t,
		blobRoot,
		localBlobs,
		orphanKey,
		[]byte(`{"version":3,"sources":[],"names":[],"mappings":""}`),
		staleTime,
	)

	putDone := make(chan struct{})
	allowCommit := make(chan struct{})
	var putOnce sync.Once

	hookedBlobs := &hookedBlobStore{
		Store: localBlobs,
		// Force List to report the pre-upload stale ModTime to prove safety does
		// not depend on storage ModTime refresh visibility.
		staleModTime: map[string]time.Time{sharedKey: staleTime, orphanKey: staleTime},
		onAfterPut: func(_ context.Context, key string) {
			if key == sharedKey {
				putOnce.Do(func() {
					close(putDone)
					<-allowCommit
				})
			}
		},
	}

	gw := newMetadataTestGateway(store, Config{
		Tokens:     store,
		Store:      store,
		Blobs:      hookedBlobs,
		SourceMaps: store,
		Now:        func() time.Time { return baseNow },
	})
	t.Cleanup(func() { _ = gw.Shutdown() })

	type uploadResult struct {
		status int
		body   string
	}
	uploadDone := make(chan uploadResult, 1)
	go func() {
		status, body := uploadSourceMapBody(t, gw, uploadToken, "v2", "app.min.js", testSourceMap)
		uploadDone <- uploadResult{status: status, body: body}
	}()

	waitSignal(t, putDone, "upload blob Put completion")

	// Run janitor while upload is paused between Blobs.Put and DB commit.
	cleaner := &janitor.SourceMaps{
		Metadata:    store,
		Blobs:       hookedBlobs,
		GracePeriod: janitor.DefaultSourceMapGracePeriod,
		Now:         func() time.Time { return baseNow },
	}
	if _, err := cleaner.Clean(ctx, baseNow.Add(-90*24*time.Hour)); err != nil {
		t.Fatalf("janitor Clean failed: %v", err)
	}

	// Truly orphaned old blob must be reclaimed; pinned blob must survive.
	if _, err := localBlobs.Get(ctx, orphanKey); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("orphan blob still exists after sweep: err=%v", err)
	}
	if r, err := localBlobs.Get(ctx, sharedKey); err != nil {
		t.Fatalf("pinned blob deleted during in-flight upload: %v", err)
	} else {
		_ = r.Close()
	}

	close(allowCommit)
	res := <-uploadDone
	if res.status != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s want 201", res.status, res.body)
	}

	r, err := localBlobs.Get(ctx, sharedKey)
	if err != nil {
		t.Fatalf("blob missing after upload commit: %v", err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, mapData) {
		t.Fatalf("blob bytes mismatch after commit")
	}
	artifacts, err := store.SourceMapArtifacts(ctx, project.ID, "v2")
	if err != nil || len(artifacts) != 1 || artifacts[0].Sha256 != digest {
		t.Fatalf("artifacts=%+v err=%v", artifacts, err)
	}
}

// Scenario 2: Janitor lists an old-ModTime orphan blob and sees it unreferenced
// during the initial metadata scan, then a concurrent upload commits a new
// release referencing that exact digest before janitor enters the delete phase.
func TestSourceMapGCRaceNewUploadCommitsAfterListAndScanBeforeDelete(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	project := createSourceMapRaceProject(t, store)
	uploadToken := createUploadTokenTest(t, store, project.ID)
	blobRoot := t.TempDir()
	localBlobs, err := blob.NewLocal(blobRoot)
	if err != nil {
		t.Fatal(err)
	}

	baseNow := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	staleTime := baseNow.Add(-2 * time.Hour)
	mapData, digest := normalizedTestSourceMap(t, testSourceMap)
	sharedKey := symbolicate.RevisionKey(project.Slug, digest)
	orphanDigest := strings.Repeat("e", 64)
	orphanKey := symbolicate.RevisionKey(project.Slug, orphanDigest)

	seedOldBlob(t, blobRoot, localBlobs, sharedKey, mapData, staleTime)
	seedOldBlob(
		t,
		blobRoot,
		localBlobs,
		orphanKey,
		[]byte(`{"version":3,"sources":[],"names":[],"mappings":""}`),
		staleTime,
	)

	scanned := make(chan struct{})
	allowSweepDelete := make(chan struct{})
	var scanOnce sync.Once
	janitorStore := &hookedSourceMapStore{
		Store: store,
		onAfterReferencedDigests: func() {
			scanOnce.Do(func() {
				close(scanned)
				<-allowSweepDelete
			})
		},
	}

	gw := newMetadataTestGateway(store, Config{
		Tokens:     store,
		Store:      store,
		Blobs:      localBlobs,
		SourceMaps: store,
		Now:        func() time.Time { return baseNow },
	})
	t.Cleanup(func() { _ = gw.Shutdown() })

	cleanErrCh := make(chan error, 1)
	go func() {
		cleaner := &janitor.SourceMaps{
			Metadata:    janitorStore,
			Blobs:       localBlobs,
			GracePeriod: janitor.DefaultSourceMapGracePeriod,
			Now:         func() time.Time { return baseNow },
		}
		_, err := cleaner.Clean(ctx, baseNow.Add(-90*24*time.Hour))
		cleanErrCh <- err
	}()

	waitSignal(t, scanned, "janitor initial scan completion")

	// While janitor is paused between initial scan and delete, complete a full upload.
	status, body := uploadSourceMapBody(t, gw, uploadToken, "v2", "app.min.js", testSourceMap)
	if status != http.StatusCreated {
		t.Fatalf("concurrent upload status=%d body=%s want 201", status, body)
	}

	close(allowSweepDelete)
	if err := <-cleanErrCh; err != nil {
		t.Fatalf("janitor Clean failed: %v", err)
	}

	if _, err := localBlobs.Get(ctx, orphanKey); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("orphan blob was not reclaimed: err=%v", err)
	}
	r, err := localBlobs.Get(ctx, sharedKey)
	if err != nil {
		t.Fatalf("newly committed revision blob was deleted by janitor: %v", err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, mapData) {
		t.Fatalf("blob content corrupted after concurrent sweep")
	}
}

// Scenario 3: Janitor already holds the project sweep lock and is deleting an
// unreferenced old blob when a concurrent upload for the same digest arrives.
// The upload must serialize behind the sweep lock so Blobs.Delete finishes
// strictly before Blobs.Put rewrites the blob and commits metadata.
func TestSourceMapGCRaceConcurrentUploadWaitsForInProgressDelete(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	project := createSourceMapRaceProject(t, store)
	uploadToken := createUploadTokenTest(t, store, project.ID)
	blobRoot := t.TempDir()
	localBlobs, err := blob.NewLocal(blobRoot)
	if err != nil {
		t.Fatal(err)
	}

	baseNow := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	staleTime := baseNow.Add(-2 * time.Hour)
	mapData, digest := normalizedTestSourceMap(t, testSourceMap)
	sharedKey := symbolicate.RevisionKey(project.Slug, digest)
	seedOldBlob(t, blobRoot, localBlobs, sharedKey, mapData, staleTime)

	deleteEntered := make(chan struct{})
	allowDeleteFinish := make(chan struct{})
	pinEntered := make(chan struct{})
	pinReturned := make(chan struct{})

	var eventMu sync.Mutex
	var events []string
	record := func(e string) {
		eventMu.Lock()
		events = append(events, e)
		eventMu.Unlock()
	}

	hookedBlobs := &hookedBlobStore{
		Store: localBlobs,
		onDelete: func(_ context.Context, key string) {
			if key == sharedKey {
				record("delete_start")
				close(deleteEntered)
				<-allowDeleteFinish
			}
		},
		onAfterDel: func(_ context.Context, key string) {
			if key == sharedKey {
				record("delete_done")
			}
		},
		onPut: func(_ context.Context, key string) {
			if key == sharedKey {
				record("put_start")
			}
		},
	}

	uploadStore := &hookedSourceMapStore{
		Store: store,
		onBeforePin: func() {
			close(pinEntered)
		},
		onAfterPin: func() {
			record("pin_acquired")
			close(pinReturned)
		},
	}

	gw := newMetadataTestGateway(store, Config{
		Tokens:     store,
		Store:      store,
		Blobs:      hookedBlobs,
		SourceMaps: uploadStore,
		Now:        func() time.Time { return baseNow },
	})
	t.Cleanup(func() { _ = gw.Shutdown() })

	cleanErrCh := make(chan error, 1)
	go func() {
		cleaner := &janitor.SourceMaps{
			Metadata:    store,
			Blobs:       hookedBlobs,
			GracePeriod: janitor.DefaultSourceMapGracePeriod,
			Now:         func() time.Time { return baseNow },
		}
		_, err := cleaner.Clean(ctx, baseNow.Add(-90*24*time.Hour))
		cleanErrCh <- err
	}()

	waitSignal(t, deleteEntered, "janitor delete start")

	type uploadResult struct {
		status int
		body   string
	}
	uploadDone := make(chan uploadResult, 1)
	go func() {
		status, body := uploadSourceMapBody(t, gw, uploadToken, "v2", "app.min.js", testSourceMap)
		uploadDone <- uploadResult{status: status, body: body}
	}()

	waitSignal(t, pinEntered, "concurrent upload pin entry")
	select {
	case <-pinReturned:
		t.Fatal("upload acquired pin while janitor still held the project sweep lock")
	default:
	}

	close(allowDeleteFinish)
	if err := <-cleanErrCh; err != nil {
		t.Fatalf("janitor Clean failed: %v", err)
	}
	res := <-uploadDone
	if res.status != http.StatusCreated {
		t.Fatalf("upload status=%d body=%s want 201", res.status, res.body)
	}

	eventMu.Lock()
	gotEvents := slices.Clone(events)
	eventMu.Unlock()
	wantEvents := []string{"delete_start", "delete_done", "pin_acquired", "put_start"}
	if !slices.Equal(gotEvents, wantEvents) {
		t.Fatalf("event order=%v want %v", gotEvents, wantEvents)
	}

	r, err := localBlobs.Get(ctx, sharedKey)
	if err != nil {
		t.Fatalf("blob missing after serialized upload: %v", err)
	}
	got, _ := io.ReadAll(r)
	_ = r.Close()
	if !bytes.Equal(got, mapData) {
		t.Fatalf("blob bytes mismatch after rewrite")
	}
}

// Scenario 4: If an upload stalls past its pin TTL (with or without janitor
// having already swept the expired pin and orphan blob), the stalled upload's
// late commit must be rejected instead of persisting dangling metadata.
func TestSourceMapGCRaceExpiredUploadPinRejectsLateCommit(t *testing.T) {
	for _, runJanitorBeforeCommit := range []bool{true, false} {
		name := "after_janitor_sweep"
		if !runJanitorBeforeCommit {
			name = "without_janitor_prune"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			store, dbPath := testStoreWithPath(t)
			project := createSourceMapRaceProject(t, store)
			uploadToken := createUploadTokenTest(t, store, project.ID)
			blobRoot := t.TempDir()
			localBlobs, err := blob.NewLocal(blobRoot)
			if err != nil {
				t.Fatal(err)
			}

			baseNow := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
			_, digest := normalizedTestSourceMap(t, testSourceMap)
			key := symbolicate.RevisionKey(project.Slug, digest)

			putDone := make(chan struct{})
			allowCommit := make(chan struct{})
			var once sync.Once
			hookedBlobs := &hookedBlobStore{
				Store: localBlobs,
				onAfterPut: func(_ context.Context, gotKey string) {
					if gotKey == key {
						once.Do(func() {
							close(putDone)
							<-allowCommit
						})
					}
				},
			}

			gw := newMetadataTestGateway(store, Config{
				Tokens:     store,
				Store:      store,
				Blobs:      hookedBlobs,
				SourceMaps: store,
				Now:        func() time.Time { return baseNow },
			})
			gw.sourceMapUploadPinTTL = 5 * time.Minute
			t.Cleanup(func() { _ = gw.Shutdown() })

			type uploadResult struct {
				status int
				body   string
			}
			uploadDone := make(chan uploadResult, 1)
			go func() {
				status, body := uploadSourceMapBody(t, gw, uploadToken, "stalled", "app.min.js", testSourceMap)
				uploadDone <- uploadResult{status: status, body: body}
			}()

			waitSignal(t, putDone, "stalled upload Put completion")

			// Deterministically expire the DB-clock upload pin without sleeping.
			expireSourceMapUploadPinsSQLite(t, dbPath, project.ID)

			if runJanitorBeforeCommit {
				later := baseNow.Add(janitor.DefaultSourceMapGracePeriod + time.Minute)
				diskPath := filepath.Join(blobRoot, filepath.FromSlash(key))
				if err := os.Chtimes(diskPath, baseNow, baseNow); err != nil {
					t.Fatal(err)
				}
				cleaner := &janitor.SourceMaps{
					Metadata:    store,
					Blobs:       localBlobs,
					GracePeriod: janitor.DefaultSourceMapGracePeriod,
					Now:         func() time.Time { return later },
				}
				if _, err := cleaner.Clean(ctx, later.Add(-90*24*time.Hour)); err != nil {
					t.Fatalf("janitor Clean failed: %v", err)
				}
				if _, err := localBlobs.Get(ctx, key); !errors.Is(err, blob.ErrNotFound) {
					t.Fatalf("expired stalled upload blob was not reclaimed: err=%v", err)
				}
			}

			close(allowCommit)
			res := <-uploadDone
			if res.status != http.StatusServiceUnavailable {
				t.Fatalf("late commit after pin expiry status=%d body=%s want 503", res.status, res.body)
			}
			artifacts, err := store.SourceMapArtifacts(ctx, project.ID, "stalled")
			if err != nil || len(artifacts) != 0 {
				t.Fatalf("dangling metadata persisted after pin expiry: artifacts=%+v err=%v", artifacts, err)
			}
		})
	}
}

// Scenario 5: While janitor is deleting an expired release listed before cutoff,
// a concurrent upload refreshes that release (either by adding a new file or by
// overwriting the existing filename); the conditional delete must not wipe the
// refreshed release and ListSourceMapReleases must reflect the refreshed time.
func TestSourceMapGCRaceConcurrentUploadToExpiringRelease(t *testing.T) {
	for _, tc := range []struct {
		name          string
		uploadFile    string
		uploadPayload string
		wantFiles     int
	}{
		{
			name:          "add_new_file",
			uploadFile:    "new.js",
			uploadPayload: testSourceMap,
			wantFiles:     2,
		},
		{
			name:          "overwrite_existing_filename",
			uploadFile:    "old.js",
			uploadPayload: `{"version":3,"sources":["b.ts"],"names":[],"mappings":"AAAA"}`,
			wantFiles:     1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := testStore(t)
			project := createSourceMapRaceProject(t, store)
			uploadToken := createUploadTokenTest(t, store, project.ID)
			localBlobs, err := blob.NewLocal(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}

			baseNow := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
			oldTime := baseNow.Add(-91 * 24 * time.Hour)
			mapData, digest := normalizedTestSourceMap(t, testSourceMap)
			if err := localBlobs.Put(
				ctx,
				symbolicate.RevisionKey(project.Slug, digest),
				bytes.NewReader(mapData),
			); err != nil {
				t.Fatal(err)
			}
			if err := store.UpsertSourceMapArtifact(
				ctx,
				project.ID,
				"v1",
				"old.js",
				int64(len(mapData)),
				digest,
				oldTime,
			); err != nil {
				t.Fatal(err)
			}

			listed := make(chan struct{})
			allowDelete := make(chan struct{})
			var listOnce sync.Once
			janitorStore := &hookedSourceMapStore{
				Store: store,
				onAfterListExpiredReleases: func() {
					listOnce.Do(func() {
						close(listed)
						<-allowDelete
					})
				},
			}

			gw := newMetadataTestGateway(store, Config{
				Tokens:     store,
				Store:      store,
				Blobs:      localBlobs,
				SourceMaps: store,
				Now:        func() time.Time { return baseNow },
			})
			t.Cleanup(func() { _ = gw.Shutdown() })

			cleanErrCh := make(chan error, 1)
			go func() {
				cleaner := &janitor.SourceMaps{
					Metadata:    janitorStore,
					Blobs:       localBlobs,
					GracePeriod: janitor.DefaultSourceMapGracePeriod,
					Now:         func() time.Time { return baseNow },
				}
				_, err := cleaner.Clean(ctx, baseNow.Add(-90*24*time.Hour))
				cleanErrCh <- err
			}()

			waitSignal(t, listed, "janitor expired release list")
			status, body := uploadSourceMapBody(t, gw, uploadToken, "v1", tc.uploadFile, tc.uploadPayload)
			if status != http.StatusCreated {
				t.Fatalf("refresh upload status=%d body=%s want 201", status, body)
			}
			close(allowDelete)
			if err := <-cleanErrCh; err != nil {
				t.Fatalf("janitor Clean failed: %v", err)
			}

			artifacts, err := store.SourceMapArtifacts(ctx, project.ID, "v1")
			if err != nil || len(artifacts) != tc.wantFiles {
				t.Fatalf("refreshed release artifacts=%+v err=%v want %d", artifacts, err, tc.wantFiles)
			}
			releases, err := store.ListSourceMapReleases(ctx, project.ID, 10)
			if err != nil || len(releases) != 1 || !releases[0].CreatedAt.Equal(baseNow) {
				t.Fatalf("ListSourceMapReleases=%+v err=%v want CreatedAt=%v", releases, err, baseNow)
			}
		})
	}
}
