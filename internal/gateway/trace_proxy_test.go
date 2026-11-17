package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/storage"
)

func TestTraceSearchForwardsReproducibleQuery(t *testing.T) {
	captured := make(chan *http.Request, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured <- r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer upstream.Close()

	backend, err := storage.New(
		storage.MetricsPrometheus,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.URL,
		upstream.Client(),
	)
	if err != nil {
		t.Fatal(err)
	}
	app := newTestGateway(Config{
		Tokens:  &fakeTokens{project: meta.ProjectInfo{ID: 42, Slug: "alpha"}},
		Storage: backend,
		Client:  upstream.Client(),
	}).App()

	query := url.Values{
		"service":     {"checkout api"},
		"operation":   {"GET /orders/:id"},
		"start":       {"1787472000000000"},
		"end":         {"1787475600000000"},
		"minDuration": {"12345us"},
		"limit":       {"20"},
		"tags":        {"error=true"},
		"untrusted":   {"drop-me"},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/traces/search?"+query.Encode(), nil)
	req.Header.Set("Authorization", "Bearer lou_valid")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}

	got := <-captured
	if got.URL.Path != "/select/jaeger/api/traces" {
		t.Fatalf("path=%q", got.URL.Path)
	}
	want := query
	want.Del("untrusted")
	if got.URL.Query().Encode() != want.Encode() {
		t.Fatalf("query=%q want=%q", got.URL.RawQuery, want.Encode())
	}
	if got.Header.Get("AccountID") != "0" || got.Header.Get("ProjectID") != "42" {
		t.Fatalf(
			"tenant headers AccountID=%q ProjectID=%q",
			got.Header.Get("AccountID"),
			got.Header.Get("ProjectID"),
		)
	}
}
