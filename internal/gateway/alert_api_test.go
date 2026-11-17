package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	alertpkg "github.com/eimsound/lazy-observe/internal/alert"
	"github.com/eimsound/lazy-observe/internal/meta"
)

func TestAlertAPIUsesStableWireContract(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	projectEntity, err := store.CreateProject(ctx, "Alert Contract", "alert-contract")
	if err != nil {
		t.Fatal(err)
	}
	admin := sessionCookie(t, store, "alert-admin", "password", "admin")

	var delivered int
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		delivered++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()

	alerts := alertpkg.NewService(alertpkg.Config{
		Store: store, Sender: &alertpkg.Sender{Client: receiver.Client()},
		GroupWait: time.Millisecond, RepeatInterval: time.Hour,
	})
	t.Cleanup(alerts.Close)
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Alerts: alerts}).App()

	request := func(method, target string, payload any, wantStatus int) any {
		t.Helper()
		var body io.Reader
		if payload != nil {
			encoded, marshalErr := json.Marshal(payload)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			body = bytes.NewReader(encoded)
		}
		req := httptest.NewRequest(method, target, body)
		req.AddCookie(admin)
		req.Header.Set("X-LO-Project", "alert-contract")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, requestErr := app.Test(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		defer resp.Body.Close()
		raw, readErr := io.ReadAll(resp.Body)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if resp.StatusCode != wantStatus {
			t.Fatalf("%s %s: status=%d want=%d body=%s", method, target, resp.StatusCode, wantStatus, raw)
		}
		if len(raw) == 0 {
			return nil
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			t.Fatalf("%s %s returned invalid JSON %q: %v", method, target, raw, err)
		}
		return value
	}

	webhook := objectValue(t, request(http.MethodPost, "/api/channels", map[string]any{
		"type": "webhook", "name": "contract-webhook", "config": map[string]any{"url": receiver.URL}, "enabled": true,
	}, http.StatusCreated))
	webhookID := assertWireEntity(t, webhook, "created_at", "updated_at")
	assertNoEntFields(t, webhook, "edges", "rules", "project_alert_configs")
	webhook = objectValue(t, request(http.MethodPatch, "/api/channels/"+webhookID, map[string]any{
		"name": "contract-webhook-updated",
	}, http.StatusOK))
	if objectValue(t, webhook["config"])["url"] != receiver.URL {
		t.Fatalf("partial channel update cleared config: %#v", webhook)
	}

	testResult := objectValue(t, request(http.MethodPost, "/api/channels/"+webhookID+"/test", nil, http.StatusOK))
	if testResult["ok"] != true || testResult["message"] == "" || delivered != 1 {
		t.Fatalf("channel test contract/result: result=%#v delivered=%d", testResult, delivered)
	}

	// `email` remains accepted for older clients, but the canonical wire/storage
	// value and configuration keys are `smtp`, `host`, and `port`.
	smtp := objectValue(t, request(http.MethodPost, "/api/channels", map[string]any{
		"type":    "email",
		"name":    "contract-email",
		"enabled": false,
		"config": map[string]any{
			"smtp_host": "smtp.example.com",
			"smtp_port": "587",
			"from":      "alerts@example.com",
			"to":        "ops@example.com",
		},
	}, http.StatusCreated))
	smtpID := assertWireEntity(t, smtp, "created_at", "updated_at")
	if smtp["type"] != "smtp" {
		t.Fatalf("canonical channel type=%#v want smtp", smtp["type"])
	}
	smtpConfig := objectValue(t, smtp["config"])
	if smtpConfig["host"] != "smtp.example.com" || smtpConfig["port"] != "587" {
		t.Fatalf("canonical smtp config=%#v", smtpConfig)
	}
	if _, exists := smtpConfig["smtp_host"]; exists {
		t.Fatalf("legacy smtp_host leaked into response: %#v", smtpConfig)
	}

	rule := objectValue(t, request(http.MethodPost, "/api/alert-rules", map[string]any{
		"name":        "ContractRule",
		"signal":      "logs",
		"expr":        "* | stats count() as total | filter total:>0",
		"for":         "1m",
		"severity":    "warning",
		"labels":      map[string]string{"team": "ops"},
		"annotations": map[string]string{"summary": "contract"},
		"enabled":     true,
		"channel_ids": []string{webhookID},
	}, http.StatusCreated))
	ruleID := assertWireEntity(t, rule, "created_at", "updated_at")
	if rule["signal"] != "logs" {
		t.Fatalf("rule signal=%#v want logs", rule["signal"])
	}
	assertStringIDs(t, rule["channel_ids"], webhookID)
	assertNoEntFields(t, rule, "edges", "project_id", "channels", "events", "project")

	// Numeric IDs are accepted too, while responses remain stable string IDs.
	numericSMTPID, err := strconv.Atoi(smtpID)
	if err != nil {
		t.Fatal(err)
	}
	rule = objectValue(t, request(http.MethodPatch, "/api/alert-rules/"+ruleID, map[string]any{
		"severity": "critical", "channel_ids": []int{numericSMTPID},
	}, http.StatusOK))
	assertStringIDs(t, rule["channel_ids"], smtpID)
	if rule["severity"] != "critical" {
		t.Fatalf("updated rule=%#v", rule)
	}

	rules := arrayValue(t, request(http.MethodGet, "/api/alert-rules", nil, http.StatusOK))
	if len(rules) != 1 {
		t.Fatalf("rules=%#v", rules)
	}
	listedRule := objectValue(t, rules[0])
	assertStringIDs(t, listedRule["channel_ids"], smtpID)
	assertNoEntFields(t, listedRule, "edges", "project_id")
	request(http.MethodPost, "/api/alert-rules", map[string]any{
		"name": "BadSignal", "signal": "traces", "expr": "vector(1)",
	}, http.StatusBadRequest)

	numericRuleID, err := strconv.Atoi(ruleID)
	if err != nil {
		t.Fatal(err)
	}
	startsAt := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	event, err := store.CreateAlertEvent(ctx, meta.AlertEventCreate{
		RuleID: numericRuleID, Fingerprint: "contract-fingerprint", Status: "firing",
		Labels:      map[string]string{"alertname": "ContractRule", "severity": "critical"},
		Annotations: map[string]string{"summary": "contract event"}, StartsAt: startsAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	events := arrayValue(t, request(http.MethodGet, "/api/alerts", nil, http.StatusOK))
	if len(events) != 1 {
		t.Fatalf("events=%#v", events)
	}
	listedEvent := objectValue(t, events[0])
	if listedEvent["id"] != strconv.Itoa(event.ID) || listedEvent["rule"] != "ContractRule" ||
		listedEvent["fingerprint"] != "contract-fingerprint" {
		t.Fatalf("event contract=%#v", listedEvent)
	}
	assertRFC3339(t, listedEvent, "starts_at", "updated_at")
	assertNoEntFields(t, listedEvent, "edges", "rule_id", "last_notified_at")

	silenceStart := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	silenceEnd := silenceStart.Add(time.Hour)
	silence := objectValue(t, request(http.MethodPost, "/api/silences", map[string]any{
		"matchers":  []map[string]string{{"name": "alertname", "op": "=", "value": "ContractRule"}},
		"starts_at": silenceStart.Format(time.RFC3339Nano), "ends_at": silenceEnd.Format(time.RFC3339Nano),
		"comment": "contract silence", "creator": "spoofed-client-value",
	}, http.StatusCreated))
	silenceID := assertWireEntity(t, silence, "created_at")
	assertRFC3339(t, silence, "starts_at", "ends_at")
	if silence["creator"] != "alert-admin" {
		t.Fatalf("silence creator=%#v want authenticated user", silence["creator"])
	}
	if _, exists := silence["created_by"]; exists {
		t.Fatalf("legacy created_by leaked into response: %#v", silence)
	}
	assertNoEntFields(t, silence, "edges", "project_id", "project")

	silence = objectValue(t, request(http.MethodPatch, "/api/silences/"+silenceID, map[string]any{
		"comment": "updated contract silence",
	}, http.StatusOK))
	if silence["creator"] != "alert-admin" || silence["comment"] != "updated contract silence" {
		t.Fatalf("updated silence contract=%#v", silence)
	}

	channels := arrayValue(t, request(http.MethodGet, "/api/channels", nil, http.StatusOK))
	if len(channels) != 2 {
		t.Fatalf("channels=%#v", channels)
	}
	for _, value := range channels {
		channel := objectValue(t, value)
		assertWireEntity(t, channel, "created_at", "updated_at")
		assertNoEntFields(t, channel, "edges", "rules", "project_alert_configs")
	}
	silences := arrayValue(t, request(http.MethodGet, "/api/silences", nil, http.StatusOK))
	if len(silences) != 1 || objectValue(t, silences[0])["creator"] != "alert-admin" {
		t.Fatalf("silences=%#v", silences)
	}

	if projectEntity.Slug != "alert-contract" {
		t.Fatalf("unexpected project fixture: %#v", projectEntity)
	}
}

func objectValue(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("value is %T, want JSON object: %#v", value, value)
	}
	return object
}

func arrayValue(t *testing.T, value any) []any {
	t.Helper()
	array, ok := value.([]any)
	if !ok {
		t.Fatalf("value is %T, want JSON array: %#v", value, value)
	}
	return array
}

func assertWireEntity(t *testing.T, object map[string]any, timeFields ...string) string {
	t.Helper()
	id, ok := object["id"].(string)
	if !ok || id == "" {
		t.Fatalf("id must be a non-empty string: %#v", object)
	}
	if _, err := strconv.Atoi(id); err != nil {
		t.Fatalf("id %q is not the decimal database identity: %v", id, err)
	}
	assertRFC3339(t, object, timeFields...)
	return id
}

func assertRFC3339(t *testing.T, object map[string]any, fields ...string) {
	t.Helper()
	for _, field := range fields {
		value, ok := object[field].(string)
		if !ok || value == "" {
			t.Fatalf("%s must be a non-empty RFC3339 string: %#v", field, object)
		}
		if _, err := time.Parse(time.RFC3339Nano, value); err != nil {
			t.Fatalf("%s=%q is not RFC3339: %v", field, value, err)
		}
	}
}

func assertStringIDs(t *testing.T, value any, want ...string) {
	t.Helper()
	items := arrayValue(t, value)
	if len(items) != len(want) {
		t.Fatalf("ids=%#v want=%#v", items, want)
	}
	for index, item := range items {
		if item != want[index] {
			t.Fatalf("ids[%d]=%#v want=%q", index, item, want[index])
		}
	}
}

func assertNoEntFields(t *testing.T, object map[string]any, fields ...string) {
	t.Helper()
	for _, field := range fields {
		if value, exists := object[field]; exists {
			t.Errorf("ent-only field %q leaked into API response with value %#v", field, value)
		}
	}
}
