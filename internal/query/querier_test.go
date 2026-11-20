package query

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/storage"
)

func TestStorageQuerierProjectIsolationFailClosedAndLimits(t *testing.T) {
	var seenHeaders http.Header
	var seenQuery url.Values
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenHeaders = r.Header.Clone()
		seenQuery = r.URL.Query()
		switch r.URL.Path {
		case "/api/v1/labels":
			_, _ = w.Write([]byte(`{"status":"success","data":["job","service"]}`))
		case "/select/logsql/query":
			_, _ = w.Write([]byte(`{"_msg":"ok"}` + "\n"))
		case "/large":
			_, _ = w.Write(bytes.Repeat([]byte("a"), 2048))
		case "/fail":
			http.Error(w, "upstream exploded", http.StatusBadGateway)
		default:
			_, _ = w.Write([]byte(`{"ok":true}`))
		}
	}))
	defer upstream.Close()

	backend, err := storage.New(
		storage.MetricsVM,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	q := NewStorageQuerier(StorageQuerierConfig{
		Backend: backend,
		Client:  upstream.Client(),
		IsolateMetrics: func(values url.Values, slug string, defaultMatcher bool) error {
			if slug == "" {
				return errors.New("empty slug")
			}
			values.Set("extra_filters[]", fmt.Sprintf(`{project_id=%q}`, slug))
			if defaultMatcher {
				values.Set("match[]", fmt.Sprintf(`{project_id=%q}`, slug))
			}
			return nil
		},
	})

	// 1. Metrics isolation must not mutate caller params and must fail closed on empty slug.
	params := url.Values{"limit": {"50"}}
	labels, err := q.MetricsStringList(
		context.Background(),
		meta.ProjectInfo{ID: 7, Slug: "alpha"},
		"/api/v1/labels",
		params,
	)
	if err != nil || len(labels) != 2 {
		t.Fatalf("MetricsStringList=%v err=%v", labels, err)
	}
	if params.Get("extra_filters[]") != "" {
		t.Fatalf("caller params mutated: %v", params)
	}
	if seenQuery.Get("extra_filters[]") != `{project_id="alpha"}` {
		t.Fatalf("missing metrics isolation: %v", seenQuery)
	}
	if _, err := q.Query(
		context.Background(),
		meta.ProjectInfo{ID: 7, Slug: ""},
		SignalMetrics,
		"/api/v1/query",
		nil,
	); err == nil {
		t.Fatal("expected fail-closed error for empty project slug")
	}

	// 2. Logs and traces fail closed when project.ID <= 0 and set tenant headers when > 0.
	for _, sig := range []Signal{SignalLogs, SignalTraces} {
		if _, err := q.Query(
			context.Background(),
			meta.ProjectInfo{ID: 0, Slug: "alpha"},
			sig,
			"/select/logsql/query",
			nil,
		); err == nil {
			t.Fatalf("expected fail-closed error for %s with project ID 0", sig)
		}
	}
	if _, err := q.Query(
		context.Background(),
		meta.ProjectInfo{ID: 9, Slug: "alpha"},
		SignalTraces,
		"/select/logsql/query",
		url.Values{"query": {"*"}},
	); err != nil {
		t.Fatalf("traces query err=%v", err)
	}
	if seenHeaders.Get("AccountID") != "0" || seenHeaders.Get("ProjectID") != "9" ||
		seenQuery.Get("disable_latency_offset") != "true" {
		t.Fatalf("traces headers=%v query=%v", seenHeaders, seenQuery)
	}

	// 3. Size ceiling and non-2xx errors.
	if _, err := q.QueryWithLimit(
		context.Background(),
		meta.ProjectInfo{ID: 9, Slug: "alpha"},
		SignalLogs,
		"/large",
		nil,
		1024,
	); !errors.Is(
		err,
		ErrResponseTooLarge,
	) {
		t.Fatalf("large response err=%v want ErrResponseTooLarge", err)
	}
	if _, err := q.Query(
		context.Background(),
		meta.ProjectInfo{ID: 9, Slug: "alpha"},
		SignalLogs,
		"/fail",
		nil,
	); err == nil ||
		!strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("502 err=%v want HTTP 502", err)
	}
}

func TestValidateTraceIDAndIssueEventsLogsQL(t *testing.T) {
	for _, good := range []string{"0", "deadbeef", "0123456789abcdefABCDEF0123456789"} {
		if len(good) <= 32 && !ValidateTraceID(good) {
			t.Fatalf("ValidateTraceID(%q)=false want true", good)
		}
	}
	for _, bad := range []string{"", " ", "../issues", "services", "trace-1", strings.Repeat("a", 33)} {
		if ValidateTraceID(bad) {
			t.Fatalf("ValidateTraceID(%q)=true want false", bad)
		}
	}
	expr := IssueEventsLogsQL("fp-1", "ev-2", 10)
	if !strings.Contains(expr, `fingerprint:="fp-1"`) || !strings.Contains(expr, `event_id:="ev-2"`) ||
		!strings.Contains(expr, "uniq by") || !strings.Contains(expr, "sort by (_time) desc") ||
		!strings.Contains(expr, "limit 10") {
		t.Fatalf("IssueEventsLogsQL=%q", expr)
	}
	var lines []string
	err := ReadVLogsLines(strings.NewReader("{\"a\":1}\n\n{\"b\":2}\n"), 1024, func(line []byte) {
		lines = append(lines, string(line))
	})
	if err != nil || len(lines) != 2 {
		t.Fatalf("ReadVLogsLines lines=%v err=%v", lines, err)
	}
}
