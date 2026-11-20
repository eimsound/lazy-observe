package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/storage"
)

type failingTenantBackend struct {
	storage.SignalBackend
	tracesBase string
	tenantErr  error
}

func (b failingTenantBackend) TracesQueryBase() string { return b.tracesBase }

func (b failingTenantBackend) ApplyTenantHeaders(http.Header, int) error { return b.tenantErr }

func TestLLMTracesQuerierFailClosedOnInvalidProjectOrBackend(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = w.Write([]byte("{}\n"))
	}))
	defer upstream.Close()

	backend, err := storage.New(storage.MetricsVM, upstream.URL, upstream.URL, upstream.URL, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	querier := newLLMTracesQuerier(backend, upstream.Client(), 1024, time.Second)
	for _, id := range []int{0, -1, -42} {
		err := querier.StreamLogsQL(
			context.Background(),
			meta.ProjectInfo{ID: id, Slug: "alpha"},
			url.Values{"query": {"*"}},
			func(map[string]json.RawMessage) error { return nil },
		)
		if !errors.Is(err, storage.ErrInvalidTenant) {
			t.Fatalf("StreamLogsQL(id=%d) err=%v want ErrInvalidTenant", id, err)
		}
	}

	var nilQuerier *llmTracesQuerier
	if err := nilQuerier.StreamLogsQL(
		context.Background(),
		meta.ProjectInfo{ID: 1},
		nil,
		func(map[string]json.RawMessage) error { return nil },
	); err == nil {
		t.Fatal("nil querier succeeded; want error")
	}

	tenantErr := errors.New("tenant header failure")
	failing := newLLMTracesQuerier(
		failingTenantBackend{tracesBase: upstream.URL, tenantErr: tenantErr},
		upstream.Client(),
		1024,
		time.Second,
	)
	if err := failing.StreamLogsQL(
		context.Background(),
		meta.ProjectInfo{ID: 9},
		url.Values{"query": {"*"}},
		func(map[string]json.RawMessage) error { return nil },
	); !errors.Is(err, tenantErr) {
		t.Fatalf("failing tenant err=%v want %v", err, tenantErr)
	}

	emptyBase := newLLMTracesQuerier(
		failingTenantBackend{tracesBase: ""},
		upstream.Client(),
		1024,
		time.Second,
	)
	if err := emptyBase.StreamLogsQL(
		context.Background(),
		meta.ProjectInfo{ID: 9},
		url.Values{"query": {"*"}},
		func(map[string]json.RawMessage) error { return nil },
	); err == nil {
		t.Fatal("empty TracesQueryBase succeeded; want error")
	}

	if called {
		t.Fatal("upstream must not be contacted on fail-closed errors")
	}
}

func TestLLMTracesQuerierAppliesTenantHeadersDisablesLatencyOffsetAndClonesQuery(t *testing.T) {
	var (
		gotPath    string
		gotAccount string
		gotProject string
		gotQuery   url.Values
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAccount = r.Header.Get("AccountID")
		gotProject = r.Header.Get("ProjectID")
		gotQuery = r.URL.Query()
		_, _ = w.Write([]byte(`{"row":"1"}` + "\n" + `{"row":"2"}` + "\n"))
	}))
	defer upstream.Close()

	backend, err := storage.New(storage.MetricsVM, upstream.URL, upstream.URL, upstream.URL, upstream.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	querier := newLLMTracesQuerier(backend, upstream.Client(), 1<<20, 5*time.Second)

	input := url.Values{"query": {llmBaseFilter}}
	var rows []string
	err = querier.StreamLogsQL(
		context.Background(),
		meta.ProjectInfo{ID: 77, Slug: "beta"},
		input,
		func(row map[string]json.RawMessage) error {
			rows = append(rows, rawString(row, "row"))
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/select/logsql/query" || gotAccount != "0" || gotProject != "77" {
		t.Fatalf("path=%s account=%s project=%s", gotPath, gotAccount, gotProject)
	}
	if gotQuery.Get("disable_latency_offset") != "true" || gotQuery.Get("query") != llmBaseFilter {
		t.Fatalf("gotQuery=%v", gotQuery)
	}
	if input.Has("disable_latency_offset") {
		t.Fatalf("caller query was mutated: %v", input)
	}
	if len(rows) != 2 || rows[0] != "1" || rows[1] != "2" {
		t.Fatalf("rows=%v", rows)
	}
}

func TestLLMTracesQuerierEnforcesConfigurableResponseLimitAndTimeout(t *testing.T) {
	t.Run("maxResponseBody", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = fmt.Fprintf(w, `{"payload":%q}`+"\n", strings.Repeat("x", 512))
		}))
		defer upstream.Close()

		backend, err := storage.New(storage.MetricsVM, upstream.URL, upstream.URL, upstream.URL, upstream.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		querier := newLLMTracesQuerier(backend, upstream.Client(), 128, 5*time.Second)
		err = querier.StreamLogsQL(
			context.Background(),
			meta.ProjectInfo{ID: 12},
			url.Values{"query": {"*"}},
			func(map[string]json.RawMessage) error { return nil },
		)
		if !errors.Is(err, errLLMResponseTooLarge) {
			t.Fatalf("err=%v want errLLMResponseTooLarge", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		release := make(chan struct{})
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
		}))
		defer func() {
			close(release)
			upstream.Close()
		}()

		backend, err := storage.New(storage.MetricsVM, upstream.URL, upstream.URL, upstream.URL, upstream.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		querier := newLLMTracesQuerier(backend, upstream.Client(), 1<<20, 50*time.Millisecond)
		err = querier.StreamLogsQL(
			context.Background(),
			meta.ProjectInfo{ID: 12},
			url.Values{"query": {"*"}},
			func(map[string]json.RawMessage) error { return nil },
		)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err=%v want context.DeadlineExceeded", err)
		}
	})

	t.Run("non-2xx snippet truncated by runes", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(strings.Repeat("错", 600)))
		}))
		defer upstream.Close()

		backend, err := storage.New(storage.MetricsVM, upstream.URL, upstream.URL, upstream.URL, upstream.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		querier := newLLMTracesQuerier(backend, upstream.Client(), 1<<20, 5*time.Second)
		err = querier.StreamLogsQL(
			context.Background(),
			meta.ProjectInfo{ID: 12},
			url.Values{"query": {"*"}},
			func(map[string]json.RawMessage) error { return nil },
		)
		if err == nil || !strings.Contains(err.Error(), "HTTP 500") ||
			strings.ToValidUTF8(err.Error(), "?") != err.Error() {
			t.Fatalf("err=%v", err)
		}
	})
}
