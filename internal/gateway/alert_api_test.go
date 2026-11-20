package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	alertpkg "github.com/eimsound/lazy-observe/internal/alert"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
	"github.com/eimsound/lazy-observe/internal/secret"
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
		Store:          store,
		OutboundPolicy: outbound.Policy{AllowPrivate: true},
		Sender:         &alertpkg.Sender{Client: receiver.Client()},
		GroupWait:      time.Millisecond,
		RepeatInterval: time.Hour,
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

func TestChannelCredentialsRedactedAndSSRFBlocked(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	box, err := secret.LoadOrCreate(t.TempDir() + "/secret.key")
	if err != nil {
		t.Fatal(err)
	}
	store.SetSecretBox(box)
	if _, err := store.CreateProject(ctx, "Sec", "sec"); err != nil {
		t.Fatal(err)
	}
	admin := sessionCookie(t, store, "sec-admin", "password", "admin")

	alerts := alertpkg.NewService(alertpkg.Config{
		Store:          store,
		GroupWait:      time.Millisecond,
		RepeatInterval: time.Hour,
	})
	t.Cleanup(alerts.Close)
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Alerts: alerts, Notifications: store})
	t.Cleanup(func() { _ = gw.Shutdown() })

	// SSRF / bad port / SMTP header injection are rejected on create with 400.
	badPayloads := []map[string]any{
		{
			"type":   "webhook",
			"name":   "meta-ssrf",
			"config": map[string]any{"url": "http://169.254.169.254/latest/meta-data/"},
		},
		{"type": "webhook", "name": "loopback-ssrf", "config": map[string]any{"url": "http://127.0.0.1:18428/metrics"}},
		{
			"type":   "webhook",
			"name":   "leading-zero-port",
			"config": map[string]any{"url": "https://example.com:0443/hook"},
		},
		{
			"type":   "smtp",
			"name":   "smtp-loopback",
			"config": map[string]any{"host": "127.0.0.1", "port": "25", "from": "a@example.com", "to": "b@example.com"},
		},
		{
			"type": "smtp",
			"name": "smtp-crlf",
			"config": map[string]any{
				"host": "smtp.example.com",
				"port": "587",
				"from": "a@example.com\r\nBcc: evil@example.com",
				"to":   "b@example.com",
			},
		},
	}
	for _, payload := range badPayloads {
		resp, body := requestJSON(t, gw, http.MethodPost, "/api/channels", admin, payload)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad channel %v status=%d want 400 body=%s", payload["name"], resp.StatusCode, body)
		}
	}

	// Create DingTalk and SMTP channels containing secrets.
	resp, dingBody := requestJSON(t, gw, http.MethodPost, "/api/channels", admin, map[string]any{
		"type": "dingtalk",
		"name": "ding-sec",
		"config": map[string]any{
			"url":     "https://oapi.dingtalk.com/robot/send?access_token=ding-api-secret-value",
			"secret":  "ding-sign-secret",
			"headers": map[string]any{"Authorization": "Bearer custom-hdr-secret"},
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create dingtalk status=%d body=%s", resp.StatusCode, dingBody)
	}
	for _, leaked := range []string{"ding-api-secret-value", "ding-sign-secret", "custom-hdr-secret"} {
		if strings.Contains(string(dingBody), leaked) {
			t.Fatalf("POST /api/channels leaked %q: %s", leaked, dingBody)
		}
	}
	var createdDing notificationChannelResponse
	if err := json.Unmarshal(dingBody, &createdDing); err != nil {
		t.Fatal(err)
	}

	resp, smtpBody := requestJSON(t, gw, http.MethodPost, "/api/channels", admin, map[string]any{
		"type": "smtp",
		"name": "smtp-sec",
		"config": map[string]any{
			"host":     "smtp.example.com",
			"port":     "587",
			"from":     "alerts@example.com",
			"to":       "ops@example.com",
			"username": "mailer",
			"password": "smtp-top-secret-password",
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create smtp status=%d body=%s", resp.StatusCode, smtpBody)
	}
	if strings.Contains(string(smtpBody), "smtp-top-secret-password") {
		t.Fatalf("POST /api/channels leaked smtp password: %s", smtpBody)
	}

	// GET /api/channels must return redacted values only.
	resp, listBody := requestJSON(t, gw, http.MethodGet, "/api/channels", admin, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list channels status=%d body=%s", resp.StatusCode, listBody)
	}
	for _, leaked := range []string{"ding-api-secret-value", "ding-sign-secret", "custom-hdr-secret", "smtp-top-secret-password"} {
		if strings.Contains(string(listBody), leaked) {
			t.Fatalf("GET /api/channels leaked %q: %s", leaked, listBody)
		}
	}

	// Updating channel name while echoing back the redacted config preserves real secrets.
	resp, patchBody := requestJSON(t, gw, http.MethodPatch, "/api/channels/"+createdDing.ID, admin, map[string]any{
		"name":   "ding-sec-renamed",
		"type":   "dingtalk",
		"config": createdDing.Config,
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch channel status=%d body=%s", resp.StatusCode, patchBody)
	}
	if strings.Contains(string(patchBody), "ding-api-secret-value") ||
		strings.Contains(string(patchBody), "ding-sign-secret") {
		t.Fatalf("PATCH /api/channels leaked secret: %s", patchBody)
	}
	dingID, _ := strconv.Atoi(createdDing.ID)
	stored, err := store.NotificationChannel(ctx, dingID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Config["url"] != "https://oapi.dingtalk.com/robot/send?access_token=ding-api-secret-value" ||
		stored.Config["secret"] != "ding-sign-secret" {
		t.Fatalf("echoing redacted config corrupted stored secrets: %#v", stored.Config)
	}

	// TestChannel must sanitize transport errors and never leak tokens or raw dial details.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := ln.Addr().String()
	_ = ln.Close()
	allowAlerts := alertpkg.NewService(alertpkg.Config{
		Store:          store,
		OutboundPolicy: outbound.Policy{AllowPrivate: true},
		Sender: &alertpkg.Sender{
			Outbound: outbound.Policy{AllowPrivate: true},
			Timeout:  200 * time.Millisecond,
		},
		GroupWait:      time.Millisecond,
		RepeatInterval: time.Hour,
	})
	t.Cleanup(allowAlerts.Close)
	allowGW := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Alerts: allowAlerts})
	t.Cleanup(func() { _ = allowGW.Shutdown() })
	failCh, err := allowAlerts.CreateChannel(ctx, alertpkg.ChannelInput{
		Type: "wecom",
		Name: "wecom-fail",
		Config: map[string]any{
			"url": "http://" + closedAddr + "/cgi-bin/webhook/send?key=wecom-test-secret",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, testBody := requestJSON(
		t,
		allowGW,
		http.MethodPost,
		"/api/channels/"+strconv.Itoa(failCh.ID)+"/test",
		admin,
		nil,
	)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("test failing channel status=%d want 502 body=%s", resp.StatusCode, testBody)
	}
	for _, leaked := range []string{"wecom-test-secret", closedAddr, "connection refused"} {
		if strings.Contains(string(testBody), leaked) {
			t.Fatalf("testChannel response leaked %q: %s", leaked, testBody)
		}
	}
}

func TestAlertAPIValidationDeleteAndChannelPatchNormalization(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if _, err := store.CreateProject(ctx, "Validate", "validate"); err != nil {
		t.Fatal(err)
	}
	admin := sessionCookie(t, store, "val-admin", "password", "admin")
	alerts := alertpkg.NewService(alertpkg.Config{
		Store:          store,
		GroupWait:      time.Millisecond,
		RepeatInterval: time.Hour,
	})
	t.Cleanup(alerts.Close)
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Alerts: alerts})
	t.Cleanup(func() { _ = gw.Shutdown() })

	doReq := func(method, path string, payload any) (*http.Response, []byte) {
		t.Helper()
		var body io.Reader
		if payload != nil {
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			body = bytes.NewReader(raw)
		}
		req := httptest.NewRequest(method, path, body)
		req.AddCookie(admin)
		req.Header.Set("X-LO-Project", "validate")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := gw.App().Test(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp, out
	}

	// 1. Invalid evaluation_engine and invalid `for` duration must return 400 (not 500).
	for _, payload := range []map[string]any{
		{"name": "BadEngine", "expr": "up == 0", "evaluation_engine": "vmalert"},
		{"name": "BadFor", "expr": "up == 0", "for": "not-a-duration"},
	} {
		resp, body := doReq(http.MethodPost, "/api/alert-rules", payload)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST /api/alert-rules %v status=%d want 400 body=%s", payload["name"], resp.StatusCode, body)
		}
	}

	// Create valid rule, attach firing alert_event, verify PATCH rejects bad `for`/`evaluation_engine`,
	// and verify DELETE succeeds with 204 even while alert_events exist.
	resp, ruleBody := doReq(http.MethodPost, "/api/alert-rules", map[string]any{
		"name": "ValidRule", "expr": "up == 0", "for": "1m",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create valid rule status=%d body=%s", resp.StatusCode, ruleBody)
	}
	var createdRule alertRuleResponse
	if err := json.Unmarshal(ruleBody, &createdRule); err != nil {
		t.Fatal(err)
	}
	resp, body := doReq(http.MethodPatch, "/api/alert-rules/"+createdRule.ID, map[string]any{"for": "bad-dur"})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PATCH bad for status=%d want 400 body=%s", resp.StatusCode, body)
	}
	resp, body = doReq(
		http.MethodPatch,
		"/api/alert-rules/"+createdRule.ID,
		map[string]any{"evaluation_engine": "legacy"},
	)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PATCH bad evaluation_engine status=%d want 400 body=%s", resp.StatusCode, body)
	}
	numRuleID, _ := strconv.Atoi(createdRule.ID)
	if _, err := store.CreateAlertEvent(ctx, meta.AlertEventCreate{
		RuleID:      numRuleID,
		Fingerprint: "del-fp",
		Status:      "firing",
		Labels: map[string]string{
			"alertname": "ValidRule",
		},
		Annotations: map[string]string{},
		StartsAt:    time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	resp, body = doReq(http.MethodDelete, "/api/alert-rules/"+createdRule.ID, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("DELETE rule with firing event status=%d want 204 body=%s", resp.StatusCode, body)
	}

	// 2. Silence create & update validate matchers, regex syntax, and time windows.
	now := time.Now().UTC().Truncate(time.Second)
	for _, badSilence := range []map[string]any{
		{
			"matchers":  []map[string]string{{"name": "", "op": "=", "value": "x"}},
			"starts_at": now, "ends_at": now.Add(time.Hour),
		},
		{
			"matchers":  []map[string]string{{"name": "alertname", "op": "=~", "value": "[invalid"}},
			"starts_at": now, "ends_at": now.Add(time.Hour),
		},
		{
			"matchers":  []map[string]string{{"name": "alertname", "op": "!=", "value": "x"}},
			"starts_at": now, "ends_at": now.Add(time.Hour),
		},
	} {
		resp, body = doReq(http.MethodPost, "/api/silences", badSilence)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST invalid silence status=%d want 400 body=%s", resp.StatusCode, body)
		}
	}
	resp, silBody := doReq(http.MethodPost, "/api/silences", map[string]any{
		"matchers":  []map[string]string{{"name": "alertname", "op": "=~", "value": "High.*"}},
		"starts_at": now, "ends_at": now.Add(time.Hour), "comment": "ok",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST valid silence status=%d body=%s", resp.StatusCode, silBody)
	}
	var createdSilence silenceResponse
	if err := json.Unmarshal(silBody, &createdSilence); err != nil {
		t.Fatal(err)
	}
	for _, badPatch := range []map[string]any{
		{"matchers": []map[string]string{}},
		{"matchers": []map[string]string{{"name": "alertname", "op": "=~", "value": "(unclosed"}}},
		{"starts_at": now.Add(2 * time.Hour)},
		{"ends_at": now.Add(-time.Minute)},
	} {
		resp, body = doReq(http.MethodPatch, "/api/silences/"+createdSilence.ID, badPatch)
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("PATCH invalid silence %v status=%d want 400 body=%s", badPatch, resp.StatusCode, body)
		}
	}

	// 3. PATCH /api/channels/:id omitting `type` still normalizes smtp_host/smtp_port
	//    and enforces SSRF validation.
	resp, smtpBody := doReq(http.MethodPost, "/api/channels", map[string]any{
		"type": "smtp",
		"name": "patch-smtp",
		"config": map[string]any{
			"host": "smtp.example.com", "port": "587", "from": "a@example.com", "to": "b@example.com",
		},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create smtp channel status=%d body=%s", resp.StatusCode, smtpBody)
	}
	var smtpCh notificationChannelResponse
	if err := json.Unmarshal(smtpBody, &smtpCh); err != nil {
		t.Fatal(err)
	}
	// Omitting `type` with loopback smtp_host must be normalized and rejected (400).
	resp, body = doReq(http.MethodPatch, "/api/channels/"+smtpCh.ID, map[string]any{
		"config": map[string]any{
			"smtp_host": "127.0.0.1", "smtp_port": "25", "from": "a@example.com", "to": "b@example.com",
		},
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PATCH smtp channel without type (loopback) status=%d want 400 body=%s", resp.StatusCode, body)
	}
	// Omitting `type` with valid public smtp_host normalizes to `host`/`port`.
	resp, patchedBody := doReq(http.MethodPatch, "/api/channels/"+smtpCh.ID, map[string]any{
		"config": map[string]any{
			"smtp_host": "mail.example.com", "smtp_port": "465", "from": "a@example.com", "to": "b@example.com",
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH smtp channel without type (valid) status=%d body=%s", resp.StatusCode, patchedBody)
	}
	var patchedCh notificationChannelResponse
	if err := json.Unmarshal(patchedBody, &patchedCh); err != nil {
		t.Fatal(err)
	}
	if patchedCh.Config["host"] != "mail.example.com" || patchedCh.Config["port"] != "465" {
		t.Fatalf("smtp_host/smtp_port not normalized on typeless PATCH: %#v", patchedCh.Config)
	}
	if _, leaked := patchedCh.Config["smtp_host"]; leaked {
		t.Fatalf("legacy smtp_host leaked after typeless PATCH: %#v", patchedCh.Config)
	}
}
