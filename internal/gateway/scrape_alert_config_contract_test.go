package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/alert"
	"github.com/eimsound/lazy-observe/internal/scrape"
)

func TestScrapeTargetHTTPContract(t *testing.T) {
	store := testStore(t)
	if _, err := store.CreateProject(context.Background(), "Default", "default"); err != nil {
		t.Fatal(err)
	}
	member := sessionCookie(t, store, "scrape-contract", "password", "member")
	metrics := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(
			[]byte(
				`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1752762000.25,"1"]}]}}`,
			),
		)
	}))
	defer metrics.Close()
	service := scrape.NewService(scrape.Config{Store: store, ConfigPath: filepath.Join(t.TempDir(), "scrape.yml")})
	app := newMetadataTestGateway(
		store,
		Config{Tokens: store, Store: store, Scrapes: service, Metrics: metrics.URL},
	).App()

	request := func(method, path, body string) (int, any) {
		t.Helper()
		var payload *bytes.Reader
		if body != "" {
			payload = bytes.NewReader([]byte(body))
		} else {
			payload = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, payload)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		req.AddCookie(member)
		resp, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var decoded any
		if resp.StatusCode != http.StatusNoContent {
			if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, decoded
	}

	status, raw := request(
		http.MethodPost,
		"/api/scrape-targets",
		`{"name":"node","url":"http://node:9100/metrics","interval":"15s","labels":{"env":"test"},"enabled":true}`,
	)
	if status != http.StatusCreated {
		t.Fatalf("create status=%d response=%#v", status, raw)
	}
	target := requireScrapeTargetContract(t, raw)
	id := target["id"].(string)

	status, raw = request(http.MethodGet, "/api/scrape-targets", "")
	list, ok := raw.([]any)
	if status != http.StatusOK || !ok || len(list) != 1 {
		t.Fatalf("list status=%d response=%#v", status, raw)
	}
	if got := requireScrapeTargetContract(t, list[0])["id"]; got != id {
		t.Fatalf("list id=%v want=%s", got, id)
	}

	status, raw = request(http.MethodGet, "/api/scrape-targets/"+id, "")
	if status != http.StatusOK || requireScrapeTargetContract(t, raw)["id"] != id {
		t.Fatalf("get status=%d response=%#v", status, raw)
	}

	status, raw = request(http.MethodPatch, "/api/scrape-targets/"+id, `{"enabled":false}`)
	if status != http.StatusOK || requireScrapeTargetContract(t, raw)["enabled"] != false {
		t.Fatalf("patch status=%d response=%#v", status, raw)
	}

	status, raw = request(http.MethodGet, "/api/scrape-targets/"+id+"/health", "")
	health, ok := raw.(map[string]any)
	if status != http.StatusOK || !ok || health["status"] != "up" ||
		health["last_scrape_at"] != "2025-07-17T14:20:00.25Z" {
		t.Fatalf("health status=%d response=%#v", status, raw)
	}
	if _, exists := health["last_scrape"]; exists {
		t.Fatalf("legacy last_scrape leaked into response: %#v", health)
	}
}

func requireScrapeTargetContract(t *testing.T, raw any) map[string]any {
	t.Helper()
	target, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("scrape target is not an object: %#v", raw)
	}
	if _, ok := target["id"].(string); !ok {
		t.Fatalf("scrape target id must be a string: %#v", target)
	}
	for _, field := range []string{"name", "url", "interval", "labels", "enabled"} {
		if _, exists := target[field]; !exists {
			t.Fatalf("scrape target missing %s: %#v", field, target)
		}
	}
	for _, field := range []string{"project_id", "edges", "created_at", "updated_at"} {
		if _, exists := target[field]; exists {
			t.Fatalf("storage field %s leaked into scrape target: %#v", field, target)
		}
	}
	if len(target) != 6 {
		t.Fatalf("unexpected scrape target fields: %#v", target)
	}
	return target
}

func TestProjectAlertConfigHTTPContractUsesStringChannelIDs(t *testing.T) {
	store := testStore(t)
	project, err := store.CreateProject(context.Background(), "Default", "default")
	if err != nil {
		t.Fatal(err)
	}
	member := sessionCookie(t, store, "alert-config-contract", "password", "member")
	service := alert.NewService(alert.Config{Store: store, GroupWait: time.Millisecond})
	t.Cleanup(service.Close)
	channel, err := service.CreateChannel(context.Background(), alert.ChannelInput{
		Type: "webhook", Name: "contract", Config: map[string]any{"url": "https://example.com/hook"},
	})
	if err != nil {
		t.Fatal(err)
	}
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Alerts: service}).App()

	put := func(channelIDs any) map[string]any {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{
			"new_issue": true, "regression": false, "error_rate_threshold_per_min": 3, "channel_ids": channelIDs,
		})
		req := httptest.NewRequest(
			http.MethodPut,
			fmt.Sprintf("/api/projects/%d/alert-config", project.ID),
			bytes.NewReader(payload),
		)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(member)
		resp, requestErr := app.Test(req)
		if requestErr != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("put status=%d err=%v", resp.StatusCode, requestErr)
		}
		defer resp.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	want := strconv.Itoa(channel.ID)
	for _, input := range []any{[]string{want}, []int{channel.ID}} {
		body := put(input)
		ids, ok := body["channel_ids"].([]any)
		if !ok || len(ids) != 1 || ids[0] != want {
			t.Fatalf("channel_ids=%#v want []string{%q}", body["channel_ids"], want)
		}
	}
}
