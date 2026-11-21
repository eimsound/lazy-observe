package gateway

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/eimsound/lazy-observe/internal/blob"
	"github.com/eimsound/lazy-observe/internal/management"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/state"
	"github.com/redis/go-redis/v9"
)

type blockingTokenStore struct {
	mu       sync.Mutex
	revision uint64
	project  meta.ProjectInfo
	calls    map[string]int
	started  chan string
	release  chan struct{}
}

func newBlockingTokenStore() *blockingTokenStore {
	return &blockingTokenStore{
		project: meta.ProjectInfo{ID: 7, Slug: "alpha"},
		calls:   make(map[string]int),
		started: make(chan string, 256),
		release: make(chan struct{}),
	}
}

func (s *blockingTokenStore) AuthenticateProjectAPIToken(ctx context.Context, token string) (meta.ProjectInfo, error) {
	s.mu.Lock()
	s.calls[token]++
	project := s.project
	s.mu.Unlock()
	s.started <- token
	select {
	case <-ctx.Done():
		return meta.ProjectInfo{}, ctx.Err()
	case <-s.release:
		return project, nil
	}
}

func (s *blockingTokenStore) Revision() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

func (s *blockingTokenStore) callCount(token string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[token]
}

func waitAuthenticationStart(t *testing.T, started <-chan string) string {
	t.Helper()
	select {
	case token := <-started:
		return token
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for authentication store call")
		return ""
	}
}

func TestAuthenticatorCollapsesConcurrentMissesPerToken(t *testing.T) {
	store := newBlockingTokenStore()
	authenticator := NewAuthenticator(store)
	const workers = 128
	start := make(chan struct{})
	errorsSeen := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := authenticator.authenticate(context.Background(), "shared-token")
			errorsSeen <- err
		}()
	}
	close(start)
	if token := waitAuthenticationStart(t, store.started); token != "shared-token" {
		t.Fatalf("store token = %q", token)
	}
	close(store.release)
	wg.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("authenticate error = %v", err)
		}
	}
	if calls := store.callCount("shared-token"); calls != 1 {
		t.Fatalf("store calls = %d, want one collapsed lookup", calls)
	}
}

func TestAuthenticatorDoesNotSerializeDifferentTokens(t *testing.T) {
	store := newBlockingTokenStore()
	authenticator := NewAuthenticator(store)
	var wg sync.WaitGroup
	for _, token := range []string{"token-a", "token-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = authenticator.authenticate(context.Background(), token)
		}()
	}
	first := waitAuthenticationStart(t, store.started)
	second := waitAuthenticationStart(t, store.started)
	if first == second {
		t.Fatalf("different-token calls did not proceed independently: %q/%q", first, second)
	}
	close(store.release)
	wg.Wait()
}

func TestAuthenticatorWaiterCancellationAndFailureRetry(t *testing.T) {
	store := newBlockingTokenStore()
	authenticator := NewAuthenticator(store)
	leaderDone := make(chan error, 1)
	go func() {
		_, err := authenticator.authenticate(context.Background(), "shared-token")
		leaderDone <- err
	}()
	waitAuthenticationStart(t, store.started)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := authenticator.authenticate(ctx, "shared-token"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v", err)
	}
	close(store.release)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader error = %v", err)
	}

	failing := &fakeTokens{project: meta.ProjectInfo{ID: 9, Slug: "retry"}, err: errors.New("temporary store failure")}
	retrying := NewAuthenticator(failing)
	if _, err := retrying.authenticate(context.Background(), "retry-token"); err == nil {
		t.Fatal("temporary authentication failure unexpectedly passed")
	}
	failing.mu.Lock()
	failing.err = nil
	failing.mu.Unlock()
	project, err := retrying.authenticate(context.Background(), "retry-token")
	if err != nil || project.Slug != "retry" || failing.calls != 2 {
		t.Fatalf("retry project=%#v calls=%d err=%v", project, failing.calls, err)
	}
}

func TestAuthenticatorDoesNotCacheAcrossRevisionRace(t *testing.T) {
	store := newBlockingTokenStore()
	authenticator := NewAuthenticator(store)
	result := make(chan meta.ProjectInfo, 1)
	go func() {
		project, _ := authenticator.authenticate(context.Background(), "rotated-token")
		result <- project
	}()
	waitAuthenticationStart(t, store.started)
	store.mu.Lock()
	store.revision++
	store.project = meta.ProjectInfo{ID: 8, Slug: "rotated"}
	store.mu.Unlock()
	close(store.release)
	select {
	case project := <-result:
		if project.Slug != "rotated" || store.callCount("rotated-token") != 2 {
			t.Fatalf("revision-race project=%#v calls=%d", project, store.callCount("rotated-token"))
		}
	case <-time.After(time.Second):
		t.Fatal("revision-race authentication did not finish")
	}
}

func TestAuthenticatorRevisionCannotRollBackAcrossTokens(t *testing.T) {
	store := newBlockingTokenStore()
	authenticator := NewAuthenticator(store)
	oldResult := make(chan meta.ProjectInfo, 1)
	newResult := make(chan meta.ProjectInfo, 1)

	go func() {
		project, _ := authenticator.authenticate(context.Background(), "old-token")
		oldResult <- project
	}()
	if token := waitAuthenticationStart(t, store.started); token != "old-token" {
		t.Fatalf("first store token = %q", token)
	}

	store.mu.Lock()
	store.revision = 1
	store.project = meta.ProjectInfo{ID: 8, Slug: "rotated"}
	store.mu.Unlock()

	go func() {
		project, _ := authenticator.authenticate(context.Background(), "new-token")
		newResult <- project
	}()
	if token := waitAuthenticationStart(t, store.started); token != "new-token" {
		t.Fatalf("second store token = %q", token)
	}

	close(store.release)
	for name, result := range map[string]<-chan meta.ProjectInfo{
		"old": oldResult,
		"new": newResult,
	} {
		select {
		case project := <-result:
			if project.Slug != "rotated" {
				t.Fatalf("%s-token project=%#v, want rotated revision", name, project)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s-token authentication did not finish", name)
		}
	}

	authenticator.mu.Lock()
	defer authenticator.mu.Unlock()
	if authenticator.revision != 1 {
		t.Fatalf("authenticator revision=%d, want 1", authenticator.revision)
	}
	if authenticator.cache["old-token"].project.Slug != "rotated" ||
		authenticator.cache["new-token"].project.Slug != "rotated" {
		t.Fatalf("cache contains stale projects: %#v", authenticator.cache)
	}
	if calls := store.callCount("old-token"); calls != 2 {
		t.Fatalf("old-token store calls=%d, want stale lookup plus retry", calls)
	}
	if calls := store.callCount("new-token"); calls != 1 {
		t.Fatalf("new-token store calls=%d, want one lookup", calls)
	}
}

type panicOnceTokenStore struct {
	mu       sync.Mutex
	panicked bool
	started  chan struct{}
	release  chan struct{}
	project  meta.ProjectInfo
}

func (s *panicOnceTokenStore) AuthenticateProjectAPIToken(context.Context, string) (meta.ProjectInfo, error) {
	s.mu.Lock()
	shouldPanic := !s.panicked
	s.panicked = true
	s.mu.Unlock()
	if shouldPanic {
		close(s.started)
		<-s.release
		panic("injected lookup panic")
	}
	return s.project, nil
}

func (*panicOnceTokenStore) Revision() uint64 { return 0 }

func TestAuthenticatorPanicCleansInflightAndWakesWaiters(t *testing.T) {
	store := &panicOnceTokenStore{
		started: make(chan struct{}),
		release: make(chan struct{}),
		project: meta.ProjectInfo{ID: 11, Slug: "recovered"},
	}
	authenticator := NewAuthenticator(store)

	leaderPanicked := make(chan any, 1)
	go func() {
		defer func() {
			leaderPanicked <- recover()
		}()
		_, _ = authenticator.authenticate(context.Background(), "panic-token")
	}()
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for leader lookup start")
	}

	waiterErr := make(chan error, 1)
	go func() {
		_, err := authenticator.authenticate(context.Background(), "panic-token")
		waiterErr <- err
	}()
	// Give the waiter a moment to attach to inflight before triggering the panic.
	time.Sleep(20 * time.Millisecond)
	close(store.release)

	select {
	case r := <-leaderPanicked:
		if r == nil {
			t.Fatal("leader did not observe lookup panic")
		}
	case <-time.After(time.Second):
		t.Fatal("leader hung after lookup panic")
	}

	select {
	case err := <-waiterErr:
		if err == nil {
			t.Fatal("waiter unexpectedly succeeded on panicked lookup")
		}
	case <-time.After(time.Second):
		t.Fatal("waiter deadlocked after leader lookup panic")
	}

	// Subsequent lookups for the same token must not hang and must succeed.
	project, err := authenticator.authenticate(context.Background(), "panic-token")
	if err != nil || project.Slug != "recovered" {
		t.Fatalf("post-panic authenticate project=%#v err=%v", project, err)
	}
}

func TestAuthenticatorLeaderCancelDoesNotFailWaiters(t *testing.T) {
	store := newBlockingTokenStore()
	authenticator := NewAuthenticator(store)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderErr := make(chan error, 1)
	go func() {
		_, err := authenticator.authenticate(leaderCtx, "shared-cancel-token")
		leaderErr <- err
	}()
	waitAuthenticationStart(t, store.started)

	waiterResult := make(chan struct {
		project meta.ProjectInfo
		err     error
	}, 1)
	go func() {
		p, err := authenticator.authenticate(context.Background(), "shared-cancel-token")
		waiterResult <- struct {
			project meta.ProjectInfo
			err     error
		}{project: p, err: err}
	}()
	time.Sleep(20 * time.Millisecond)

	// Cancel leader before store lookup completes.
	cancelLeader()
	select {
	case err := <-leaderErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("leader error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled leader did not return promptly")
	}

	// Release lookup: waiter with healthy context must succeed.
	close(store.release)
	select {
	case got := <-waiterResult:
		if got.err != nil || got.project.Slug != "alpha" {
			t.Fatalf("waiter result=%#v, want valid project", got)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not finish after leader canceled")
	}
}

type countingTokenStore struct {
	mu       sync.RWMutex
	tokens   map[string]meta.ProjectInfo
	revision atomic.Uint64
	calls    atomic.Int64
}

func (s *countingTokenStore) AuthenticateProjectAPIToken(
	_ context.Context,
	token string,
) (meta.ProjectInfo, error) {
	s.calls.Add(1)
	s.mu.RLock()
	p, ok := s.tokens[token]
	s.mu.RUnlock()
	if !ok {
		return meta.ProjectInfo{}, meta.ErrInvalidProjectAPIToken
	}
	return p, nil
}

func (s *countingTokenStore) Revision() uint64 { return s.revision.Load() }

func TestAuthenticatorConcurrentRLockFastPathAndInvalidation(t *testing.T) {
	store := &countingTokenStore{
		tokens: map[string]meta.ProjectInfo{
			"lo_hot_1": {ID: 1, Slug: "hot-1"},
			"lo_hot_2": {ID: 2, Slug: "hot-2"},
		},
	}
	auth := NewAuthenticator(store)

	// Warm both entries.
	for _, tok := range []string{"lo_hot_1", "lo_hot_2"} {
		if _, err := auth.authenticate(context.Background(), tok); err != nil {
			t.Fatal(err)
		}
	}
	if calls := store.calls.Load(); calls != 2 {
		t.Fatalf("warm calls=%d want=2", calls)
	}

	// Burst concurrent cache hits (RLock fast path): store must not be called again.
	const readers = 64
	const iterations = 100
	var wg sync.WaitGroup
	errs := make(chan error, readers)
	for i := range readers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			tok := "lo_hot_1"
			wantSlug := "hot-1"
			if id%2 == 1 {
				tok = "lo_hot_2"
				wantSlug = "hot-2"
			}
			for range iterations {
				p, err := auth.authenticate(context.Background(), tok)
				if err != nil || p.Slug != wantSlug {
					errs <- errors.New("unexpected cached lookup result")
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if calls := store.calls.Load(); calls != 2 {
		t.Fatalf("store calls after %d cached reads = %d, want 2", readers*iterations, calls)
	}

	// Concurrent reads mixed with explicit Invalidate() must remain race-free
	// and promptly reflect revoked credentials.
	store.mu.Lock()
	delete(store.tokens, "lo_hot_2")
	store.mu.Unlock()
	auth.Invalidate()

	if _, err := auth.authenticate(context.Background(), "lo_hot_2"); !errors.Is(err, meta.ErrInvalidProjectAPIToken) {
		t.Fatalf("revoked token after Invalidate err=%v want ErrInvalidProjectAPIToken", err)
	}

	var raceWG sync.WaitGroup
	for range 16 {
		raceWG.Add(1)
		go func() {
			defer raceWG.Done()
			for range 50 {
				_, _ = auth.authenticate(context.Background(), "lo_hot_1")
			}
		}()
	}
	for range 4 {
		raceWG.Add(1)
		go func() {
			defer raceWG.Done()
			for range 20 {
				auth.Invalidate()
				time.Sleep(time.Millisecond)
			}
		}()
	}
	raceWG.Wait()
}

func TestAuthenticatorCrossReplicaRedisInvalidationSubSecond(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	adminCookie := sessionCookie(t, store, "ha-admin", "password-123", "admin")

	project, err := store.CreateProject(ctx, "HA Project", "ha-project")
	if err != nil {
		t.Fatal(err)
	}
	tokenInfo, projectToken, err := store.CreateNamedProjectAPIToken(ctx, project.ID, "ha-ingest")
	if err != nil {
		t.Fatal(err)
	}
	uploadInfo, uploadToken, err := store.CreateSourceMapUploadToken(ctx, project.ID, "ha-ci")
	if err != nil {
		t.Fatal(err)
	}

	// Replica A and Replica B each have their own TokenStore view whose local
	// Revision() does NOT change when the other replica mutates metadata,
	// simulating two separate processes sharing PostgreSQL + Redis.
	isolatedStoreA := &isolatedRevisionMetaStore{Store: store}
	isolatedStoreB := &isolatedRevisionMetaStore{Store: store}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	blobs, err := blob.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	mr := miniredis.RunT(t)
	clientA := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	clientB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = clientA.Close(); _ = clientB.Close() })
	redisA := state.NewRedis(clientA, "ha-auth-inv")
	redisB := state.NewRedis(clientB, "ha-auth-inv")

	gwA := newMetadataTestGateway(store, Config{
		Store:           store,
		Tokens:          isolatedStoreA,
		UploadTokens:    isolatedStoreA,
		AuthInvalidator: redisA,
		Management:      management.New(store),
		Blobs:           blobs,
		Logs:            upstream.URL,
	})
	t.Cleanup(func() { _ = gwA.Shutdown() })

	gwB := newMetadataTestGateway(store, Config{
		Store:           store,
		Tokens:          isolatedStoreB,
		UploadTokens:    isolatedStoreB,
		AuthInvalidator: redisB,
		Management:      management.New(store),
		Blobs:           blobs,
		Logs:            upstream.URL,
	})
	t.Cleanup(func() { _ = gwB.Shutdown() })

	waitCtx, cancelWait := context.WithTimeout(ctx, 2*time.Second)
	defer cancelWait()
	if err := gwA.waitAuthInvalidationReady(waitCtx); err != nil {
		t.Fatalf("replica A invalidator ready: %v", err)
	}
	if err := gwB.waitAuthInvalidationReady(waitCtx); err != nil {
		t.Fatalf("replica B invalidator ready: %v", err)
	}

	postLog := func(gw *Gateway, tok string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/logs", bytes.NewReader([]byte(`{}`)))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		resp, reqErr := gw.App().Test(req)
		if reqErr != nil {
			t.Fatal(reqErr)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// 1. Warm Replica B's project-token and upload-token caches.
	if status := postLog(gwB, projectToken); status != http.StatusNoContent {
		t.Fatalf("replica B warm project token status=%d want=204", status)
	}
	if status, body := uploadStatus(t, gwB, uploadToken, "r-ha-1"); status != http.StatusCreated {
		t.Fatalf("replica B warm upload token status=%d body=%s", status, body)
	}

	// 2. Revoke project API token via Replica A's HTTP API.
	delReq := httptest.NewRequest(
		http.MethodDelete,
		"/api/projects/"+strconv.Itoa(project.ID)+"/tokens/"+strconv.Itoa(tokenInfo.ID),
		nil,
	)
	delReq.AddCookie(adminCookie)
	delResp, err := gwA.App().Test(delReq)
	if err != nil || delResp.StatusCode != http.StatusNoContent {
		t.Fatalf("replica A revoke project token status=%d err=%v", delResp.StatusCode, err)
	}
	_ = delResp.Body.Close()

	// Replica B must reject the revoked token in < 1s (not 30s TTL).
	deadline := time.Now().Add(time.Second)
	for postLog(gwB, projectToken) != http.StatusUnauthorized {
		if time.Now().After(deadline) {
			t.Fatal("replica B still accepted revoked project token after 1s")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 3. Revoke source map upload token via Replica A's HTTP API.
	delUpReq := httptest.NewRequest(
		http.MethodDelete,
		"/api/projects/"+strconv.Itoa(project.ID)+"/source-map-upload-tokens/"+strconv.Itoa(uploadInfo.ID),
		nil,
	)
	delUpReq.AddCookie(adminCookie)
	delUpResp, err := gwA.App().Test(delUpReq)
	if err != nil || delUpResp.StatusCode != http.StatusNoContent {
		t.Fatalf("replica A revoke upload token status=%d err=%v", delUpResp.StatusCode, err)
	}
	_ = delUpResp.Body.Close()

	deadline = time.Now().Add(time.Second)
	for {
		status, _ := uploadStatus(t, gwB, uploadToken, "r-ha-2")
		if status == http.StatusUnauthorized {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replica B still accepted revoked upload token after 1s (status=%d)", status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// 4. Deleting a project on Replica A also invalidates cached tokens on Replica B in < 1s.
	_, token2, err := store.CreateNamedProjectAPIToken(ctx, project.ID, "ha-ingest-2")
	if err != nil {
		t.Fatal(err)
	}
	if status := postLog(gwB, token2); status != http.StatusNoContent {
		t.Fatalf("replica B warm second project token status=%d want=204", status)
	}
	delProjReq := httptest.NewRequest(http.MethodDelete, "/api/projects/"+strconv.Itoa(project.ID), nil)
	delProjReq.AddCookie(adminCookie)
	delProjResp, err := gwA.App().Test(delProjReq)
	if err != nil || delProjResp.StatusCode != http.StatusNoContent {
		t.Fatalf("replica A delete project status=%d err=%v", delProjResp.StatusCode, err)
	}
	_ = delProjResp.Body.Close()

	deadline = time.Now().Add(time.Second)
	for postLog(gwB, token2) != http.StatusUnauthorized {
		if time.Now().After(deadline) {
			t.Fatal("replica B still accepted deleted project's token after 1s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// isolatedRevisionMetaStore keeps Revision() constant so tests prove
// cross-replica invalidation flows through AuthInvalidator rather than sharing
// the in-process meta.Store revision counter.
type isolatedRevisionMetaStore struct {
	*meta.Store
}

func (*isolatedRevisionMetaStore) Revision() uint64 { return 1 }
