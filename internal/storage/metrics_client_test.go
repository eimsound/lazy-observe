package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMetricsClientDualModeVectorAndMatrixIsolation(t *testing.T) {
	start := time.Date(2026, 7, 18, 10, 0, 0, 0, time.UTC)
	end := start.Add(5 * time.Minute)

	for _, mode := range []MetricsKind{MetricsVM, MetricsPrometheus} {
		t.Run(string(mode), func(t *testing.T) {
			type capturedReq struct {
				path  string
				query url.Values
			}
			captured := make(chan capturedReq, 4)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured <- capturedReq{path: r.URL.Path, query: r.URL.Query()}
				switch r.URL.Path {
				case "/api/v1/query":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"status": "success",
						"data": map[string]any{
							"resultType": "vector",
							"result": []map[string]any{
								{
									"metric": map[string]string{"instance": "node-1:9100"},
									"value":  []any{1700000500, "42.5"},
								},
								{
									"metric": map[string]string{"instance": "nan-node"},
									"value":  []any{1700000500, "NaN"},
								},
								{
									"metric": map[string]string{"instance": "pinf-node"},
									"value":  []any{1700000500, "+Inf"},
								},
								{
									"metric": map[string]string{"instance": "ninf-node"},
									"value":  []any{1700000500, "-Inf"},
								},
							},
						},
					})
				case "/api/v1/query_range":
					_ = json.NewEncoder(w).Encode(map[string]any{
						"status": "success",
						"data": map[string]any{
							"resultType": "matrix",
							"result": []map[string]any{
								{
									"metric": map[string]string{"service.name": "api"},
									"values": [][]any{
										{start.Unix(), "1.25"},
										{start.Add(time.Minute).Unix(), "NaN"},
										{start.Add(2 * time.Minute).Unix(), "+Inf"},
										{start.Add(3 * time.Minute).Unix(), "2.5"},
									},
								},
							},
						},
					})
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()

			backend, err := New(mode, upstream.URL, upstream.URL, upstream.URL, upstream.URL, upstream.Client())
			if err != nil {
				t.Fatal(err)
			}
			client := NewMetricsClient(MetricsClientConfig{
				Backend:    backend,
				HTTPClient: upstream.Client(),
			})

			// Vector query with hostile project_id selector to verify isolation in both modes.
			samples, err := client.QueryVector(
				context.Background(),
				"alpha",
				`up{project_id=~".*",scrape_job="scrape-1"}`,
				MetricsQueryOptions{Time: end},
			)
			if err != nil {
				t.Fatalf("QueryVector(%s): %v", mode, err)
			}
			if len(samples) != 1 || samples[0].Metric["instance"] != "node-1:9100" || samples[0].Point.Value != 42.5 {
				t.Fatalf("vector samples=%+v want 1 finite sample", samples)
			}
			if ts := samples[0].Point.Time(); ts == nil || ts.Unix() != 1700000500 {
				t.Fatalf("vector sample time=%v want 1700000500", ts)
			}
			vecReq := <-captured
			if vecReq.path != "/api/v1/query" || vecReq.query.Get("time") != end.Format(time.RFC3339Nano) {
				t.Fatalf("vector req=%+v", vecReq)
			}
			assertQueryIsolation(t, mode, vecReq.query, "alpha")

			// Matrix query with UTF-8 quoted metric name to verify Prometheus v3 + VM compatibility.
			matrixExpr := `sum by ("service.name") (rate({"http.server.request.duration_count"}[5m]))`
			series, err := client.QueryMatrix(
				context.Background(),
				"alpha",
				matrixExpr,
				MetricsRangeQueryOptions{Start: start, End: end, Step: time.Minute},
			)
			if err != nil {
				t.Fatalf("QueryMatrix(%s): %v", mode, err)
			}
			if len(series) != 1 || len(series[0].Points) != 2 ||
				series[0].Points[0].Value != 1.25 || series[0].Points[1].Value != 2.5 ||
				series[0].Points[0].UnixMilli() != start.UnixMilli() {
				t.Fatalf("matrix series=%+v want 2 finite points", series)
			}
			matReq := <-captured
			if matReq.path != "/api/v1/query_range" || matReq.query.Get("step") != "60" {
				t.Fatalf("matrix req=%+v", matReq)
			}
			assertQueryIsolation(t, mode, matReq.query, "alpha")

			// Empty project slug must fail closed without touching upstream.
			if _, err := client.QueryVector(context.Background(), "", "up", MetricsQueryOptions{}); !errors.Is(
				err,
				ErrInvalidTenant,
			) {
				t.Fatalf("empty project QueryVector err=%v want ErrInvalidTenant", err)
			}
			if _, err := client.QueryMatrix(
				context.Background(),
				"",
				"up",
				MetricsRangeQueryOptions{Start: start, End: end, Step: time.Minute},
			); !errors.Is(err, ErrInvalidTenant) {
				t.Fatalf("empty project QueryMatrix err=%v want ErrInvalidTenant", err)
			}
		})
	}
}

func assertQueryIsolation(t *testing.T, mode MetricsKind, query url.Values, slug string) {
	t.Helper()
	if mode == MetricsPrometheus {
		if query.Has("extra_filters[]") || query.Has("extra_filters") {
			t.Fatalf("prometheus mode must not send extra_filters: %v", query)
		}
		if !strings.Contains(query.Get("query"), fmt.Sprintf(`project_id=%q`, slug)) {
			t.Fatalf("prometheus mode query missing injected project_id: %q", query.Get("query"))
		}
		return
	}
	if got := query.Get("extra_filters[]"); got != fmt.Sprintf(`{project_id=%q}`, slug) {
		t.Fatalf("vm mode extra_filters[]=%q want {project_id=%q}", got, slug)
	}
}

func TestMetricsClientTimeoutMaxBytesDrainAndErrorSanitization(t *testing.T) {
	t.Run("enforces per-query timeout and sanitizes address", func(t *testing.T) {
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
		backend, err := New(MetricsVM, upstream.URL, upstream.URL, upstream.URL, upstream.URL, upstream.Client())
		if err != nil {
			t.Fatal(err)
		}
		client := NewMetricsClient(MetricsClientConfig{
			Backend:        backend,
			HTTPClient:     upstream.Client(),
			DefaultTimeout: 50 * time.Millisecond,
		})
		_, err = client.QueryVector(context.Background(), "alpha", "up", MetricsQueryOptions{
			Timeout: 30 * time.Millisecond,
		})
		if !errors.Is(err, ErrMetricsTimeout) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout err=%v want ErrMetricsTimeout + DeadlineExceeded", err)
		}
		if strings.Contains(err.Error(), upstream.URL) || strings.Contains(err.Error(), "127.0.0.1") {
			t.Fatalf("timeout error leaked internal address: %q", err.Error())
		}
	})

	t.Run("enforces configurable response limit and drains body for connection reuse", func(t *testing.T) {
		var connections atomic.Int32
		var large atomic.Bool
		upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if large.Load() {
				// Return a response exceeding MaxBytes (256 B) but well inside BodyDrainLimit (64 KiB)
				// so draining allows keep-alive reuse on the next request.
				_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[`)
				_, _ = io.WriteString(w, strings.Repeat(" ", 1024))
				_, _ = io.WriteString(w, `]}}`)
				return
			}
			_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
		}))
		upstream.Config.ConnState = func(_ net.Conn, state http.ConnState) {
			if state == http.StateNew {
				connections.Add(1)
			}
		}
		upstream.Start()
		defer upstream.Close()

		backend, err := New(MetricsVM, upstream.URL, upstream.URL, upstream.URL, upstream.URL, upstream.Client())
		if err != nil {
			t.Fatal(err)
		}
		client := NewMetricsClient(MetricsClientConfig{
			Backend:        backend,
			HTTPClient:     upstream.Client(),
			MaxBytes:       1024,
			BodyDrainLimit: 64 << 10,
		})

		large.Store(true)
		_, err = client.QueryVector(context.Background(), "alpha", "up", MetricsQueryOptions{MaxBytes: 256})
		if !errors.Is(err, ErrMetricsResponseTooLarge) {
			t.Fatalf("oversized err=%v want ErrMetricsResponseTooLarge", err)
		}
		large.Store(false)
		if _, err := client.QueryVector(context.Background(), "alpha", "up", MetricsQueryOptions{}); err != nil {
			t.Fatalf("follow-up query failed: %v", err)
		}
		if connections.Load() != 1 {
			t.Fatalf("expected drained keep-alive connection reuse (1 conn), got %d", connections.Load())
		}
	})

	t.Run("sanitizes transport and HTTP status errors without leaking internal endpoints", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		closedAddr := ln.Addr().String()
		_ = ln.Close()

		closedBackend, err := New(MetricsVM, "http://"+closedAddr, "", "http://"+closedAddr, "http://"+closedAddr, nil)
		if err != nil {
			t.Fatal(err)
		}
		closedClient := NewMetricsClient(MetricsClientConfig{Backend: closedBackend})
		_, err = closedClient.QueryVector(context.Background(), "alpha", "up", MetricsQueryOptions{})
		if !errors.Is(err, ErrMetricsBackend) {
			t.Fatalf("closed backend err=%v want ErrMetricsBackend", err)
		}
		for _, forbidden := range []string{closedAddr, "127.0.0.1", "http://", "/api/v1/query", "connection refused"} {
			if strings.Contains(err.Error(), forbidden) {
				t.Fatalf("transport error leaked %q: %q", forbidden, err.Error())
			}
		}

		statusServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.URL.Query().Get("query") {
			case "http500":
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = io.WriteString(w, `dial tcp 10.96.0.42:8481: secret-internal-host`)
			case "prom_error":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(
					w,
					`{"status":"error","errorType":"bad_data","error":"failed querying http://vmselect.internal:8481"}`,
				)
			case "bad_json":
				_, _ = io.WriteString(w, `not-json-at-all`)
			case "wrong_type":
				_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"scalar","result":[1,"1"]}}`)
			case "malformed_sample":
				_, _ = io.WriteString(
					w,
					`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"not-a-float"]}]}}`,
				)
			}
		}))
		defer statusServer.Close()

		backend, err := New(MetricsVM, statusServer.URL, "", statusServer.URL, statusServer.URL, statusServer.Client())
		if err != nil {
			t.Fatal(err)
		}
		client := NewMetricsClient(MetricsClientConfig{Backend: backend, HTTPClient: statusServer.Client()})
		for _, expr := range []string{"http500", "prom_error", "bad_json", "wrong_type", "malformed_sample"} {
			_, qErr := client.QueryVector(context.Background(), "alpha", expr, MetricsQueryOptions{})
			if !errors.Is(qErr, ErrMetricsBackend) {
				t.Fatalf("expr=%q err=%v want ErrMetricsBackend", expr, qErr)
			}
			for _, forbidden := range []string{
				"10.96.0.42", "8481", "secret-internal-host", "vmselect.internal", statusServer.URL,
			} {
				if strings.Contains(qErr.Error(), forbidden) {
					t.Fatalf("expr=%q error leaked %q: %q", expr, forbidden, qErr.Error())
				}
			}
		}
	})
}
