package gateway

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
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
