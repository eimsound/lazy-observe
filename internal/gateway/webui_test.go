package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
)

func TestWebUIServesAssetsAndSPAFallback(t *testing.T) {
	ui := fstest.MapFS{
		"index.html":        &fstest.MapFile{Data: []byte(`<html><body><div id="root"></div></body></html>`)},
		"assets/app-123.js": &fstest.MapFile{Data: []byte(`console.log("ok")`)},
	}
	app := newTestGateway(Config{WebUI: ui}).App()
	for _, path := range []string{"/", "/issues"} {
		resp, err := app.Test(newRequest(t, path))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), `id="root"`) {
			t.Fatalf("GET %s: status=%d body=%q", path, resp.StatusCode, body)
		}
	}
	asset, err := app.Test(newRequest(t, "/assets/app-123.js"))
	if err != nil {
		t.Fatal(err)
	}
	_ = asset.Body.Close()
	if asset.StatusCode != http.StatusOK || !strings.Contains(asset.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("asset: status=%d content-type=%q", asset.StatusCode, asset.Header.Get("Content-Type"))
	}
	for _, path := range []string{"/assets/missing.js"} {
		resp, err := app.Test(newRequest(t, path))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s: status=%d, want 404", path, resp.StatusCode)
		}
	}
	api, err := app.Test(newRequest(t, "/api/missing"))
	if err != nil {
		t.Fatal(err)
	}
	_ = api.Body.Close()
	if api.StatusCode == http.StatusOK {
		t.Fatal("unknown API route was incorrectly served by the SPA fallback")
	}
}

func TestWebUIDisabledReturnsNotFound(t *testing.T) {
	ui := fstest.MapFS{
		"index.html":        &fstest.MapFile{Data: []byte(`<html><body><div id="root"></div></body></html>`)},
		"assets/app-123.js": &fstest.MapFile{Data: []byte(`console.log("ok")`)},
	}
	app := newTestGateway(Config{WebUI: ui, DisableWebUI: true}).App()
	for _, requestPath := range []string{"/", "/issues", "/assets/app-123.js"} {
		resp, err := app.Test(newRequest(t, requestPath))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s: status=%d, want 404", requestPath, resp.StatusCode)
		}
	}
}

func newRequest(t *testing.T, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://lazy-observe.test"+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
