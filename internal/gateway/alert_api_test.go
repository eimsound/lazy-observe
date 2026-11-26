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
	if rule["evaluation_interval"] != "" || rule["effective_evaluation_interval"] != "1m" {
		t.Fatalf(
			"rule evaluation_interval=%#v effective=%#v want \"\" / \"1m\"",
			rule["evaluation_interval"],
			rule["effective_evaluation_interval"],
		)
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
	if rule["evaluation_interval"] != "" || rule["effective_evaluation_interval"] != "1m" {
		t.Fatalf(
			"patched rule evaluation_interval=%#v effective=%#v",
			rule["evaluation_interval"],
			rule["effective_evaluation_interval"],
		)
	}

	rules := arrayValue(t, request(http.MethodGet, "/api/alert-rules", nil, http.StatusOK))
	if len(rules) != 1 {
		t.Fatalf("rules=%#v", rules)
	}
	listedRule := objectValue(t, rules[0])
	if listedRule["evaluation_interval"] != "" || listedRule["effective_evaluation_interval"] != "1m" {
		t.Fatalf(
			"listed rule evaluation_interval=%#v effective=%#v",
			listedRule["evaluation_interval"],
			listedRule["effective_evaluation_interval"],
		)
	}
	assertStringIDs(t, listedRule["channel_ids"], smtpID)
	assertNoEntFields(t, listedRule, "edges", "project_id")
	request(http.MethodPost, "/api/alert-rules", map[string]any{
		"name": "BadSignal", "signal": "traces", "expr": "vector(1)",
	}, http.StatusBadRequest)
	request(http.MethodPost, "/api/alert-rules", map[string]any{
		"name": "BadSeverity", "expr": "vector(1)", "severity": "page",
	}, http.StatusBadRequest)
	request(http.MethodPost, "/api/alert-rules", map[string]any{
		"name": "BadTemplate", "expr": "vector(1)", "annotations": map[string]string{"summary": "{{$labels.x"},
	}, http.StatusBadRequest)
	request(http.MethodPatch, "/api/alert-rules/"+ruleID, map[string]any{"severity": "error"}, http.StatusBadRequest)
	request(http.MethodPatch, "/api/alert-rules/"+ruleID, map[string]any{
		"annotations": map[string]string{"summary": "{{end}}"},
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

func TestChannelsRBACMemberReadOnlyRedactedList(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	box, err := secret.LoadOrCreate(t.TempDir() + "/secret.key")
	if err != nil {
		t.Fatal(err)
	}
	store.SetSecretBox(box)
	if _, err := store.CreateProject(ctx, "RBAC", "rbac"); err != nil {
		t.Fatal(err)
	}
	admin := sessionCookie(t, store, "ch-rbac-admin", "password", "admin")
	member := sessionCookie(t, store, "ch-rbac-member", "password", "member")
	viewer := sessionCookie(t, store, "ch-rbac-viewer", "password", "viewer")

	alerts := alertpkg.NewService(alertpkg.Config{
		Store:          store,
		GroupWait:      time.Millisecond,
		RepeatInterval: time.Hour,
	})
	t.Cleanup(alerts.Close)
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Alerts: alerts, Notifications: store})
	t.Cleanup(func() { _ = gw.Shutdown() })

	// Admin creates a webhook channel with a secret URL & signing secret.
	resp, createdBody := requestJSON(t, gw, http.MethodPost, "/api/channels", admin, map[string]any{
		"type":    "webhook",
		"name":    "OnCall Webhook",
		"enabled": true,
		"config":  map[string]any{"url": "https://hooks.example.com/secret-path", "secret": "whsec_topsecret"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("admin create channel status=%d body=%s", resp.StatusCode, createdBody)
	}
	var created notificationChannelResponse
	if err := json.Unmarshal(createdBody, &created); err != nil {
		t.Fatal(err)
	}

	// Viewer is forbidden from listing channels.
	resp, body := requestJSON(t, gw, http.MethodGet, "/api/channels", viewer, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer GET /api/channels status=%d want=403 body=%s", resp.StatusCode, body)
	}

	// Member can read minimal redacted list (id/name/type/enabled only, zero config leakage).
	resp, body = requestJSON(t, gw, http.MethodGet, "/api/channels", member, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member GET /api/channels status=%d want=200 body=%s", resp.StatusCode, body)
	}
	for _, forbidden := range []string{"config", "hooks.example.com", "secret-path", "whsec_topsecret", "has_secret"} {
		if bytes.Contains(body, []byte(forbidden)) {
			t.Fatalf("member GET /api/channels leaked %q: %s", forbidden, body)
		}
	}
	var memberList []map[string]any
	if err := json.Unmarshal(body, &memberList); err != nil || len(memberList) != 1 {
		t.Fatalf("member list decode err=%v body=%s", err, body)
	}
	item := memberList[0]
	if len(item) != 4 || item["id"] != created.ID || item["name"] != "OnCall Webhook" ||
		item["type"] != "webhook" || item["enabled"] != true {
		t.Fatalf("member channel item=%#v want only id/name/type/enabled", item)
	}

	// Member cannot create, update, delete, or test channels.
	for _, reqSpec := range []struct {
		method string
		path   string
		body   any
	}{
		{http.MethodPost, "/api/channels", map[string]any{"type": "webhook", "name": "m", "config": map[string]any{"url": "https://example.com"}}},
		{http.MethodPatch, "/api/channels/" + created.ID, map[string]any{"name": "hijack"}},
		{http.MethodPut, "/api/channels/" + created.ID, map[string]any{"name": "hijack"}},
		{http.MethodDelete, "/api/channels/" + created.ID, nil},
		{http.MethodPost, "/api/channels/" + created.ID + "/test", nil},
	} {
		r, b := requestJSON(t, gw, reqSpec.method, reqSpec.path, member, reqSpec.body)
		if r.StatusCode != http.StatusForbidden {
			t.Fatalf("member %s %s status=%d want=403 body=%s", reqSpec.method, reqSpec.path, r.StatusCode, b)
		}
	}

	// Admin still receives full redacted config view.
	resp, body = requestJSON(t, gw, http.MethodGet, "/api/channels", admin, nil)
	if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"config"`)) ||
		!bytes.Contains(body, []byte(`"secret":"[REDACTED]"`)) ||
		bytes.Contains(body, []byte("whsec_topsecret")) {
		t.Fatalf("admin GET /api/channels status=%d body=%s", resp.StatusCode, body)
	}
}

func TestUpdateSilenceAllowsClearingComment(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if _, err := store.CreateProject(ctx, "SilenceProj", "silence-proj"); err != nil {
		t.Fatal(err)
	}
	member := sessionCookie(t, store, "silence-member", "password", "member")
	alerts := alertpkg.NewService(alertpkg.Config{
		Store:          store,
		GroupWait:      time.Millisecond,
		RepeatInterval: time.Hour,
	})
	t.Cleanup(alerts.Close)
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Alerts: alerts, Notifications: store})
	t.Cleanup(func() { _ = gw.Shutdown() })

	doReq := func(method, path string, payload any) (*http.Response, []byte) {
		var reader io.Reader
		if payload != nil {
			raw, _ := json.Marshal(payload)
			reader = bytes.NewReader(raw)
		}
		req := httptest.NewRequest(method, path, reader)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-LO-Project", "silence-proj")
		req.AddCookie(member)
		resp, err := gw.App().Test(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, body
	}

	now := time.Now().UTC().Truncate(time.Second)
	resp, body := doReq(http.MethodPost, "/api/silences", map[string]any{
		"matchers":  []map[string]string{{"name": "alertname", "op": "=", "value": "HighCPU"}},
		"starts_at": now,
		"ends_at":   now.Add(time.Hour),
		"comment":   "planned maintenance",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create silence status=%d body=%s", resp.StatusCode, body)
	}
	var created silenceResponse
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatal(err)
	}
	if created.Comment != "planned maintenance" {
		t.Fatalf("created comment=%q", created.Comment)
	}

	// 1. Omitting comment on PATCH must preserve the existing comment.
	resp, body = doReq(http.MethodPatch, "/api/silences/"+created.ID, map[string]any{
		"ends_at": now.Add(2 * time.Hour),
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch without comment status=%d body=%s", resp.StatusCode, body)
	}
	var kept silenceResponse
	if err := json.Unmarshal(body, &kept); err != nil {
		t.Fatal(err)
	}
	if kept.Comment != "planned maintenance" {
		t.Fatalf("omitted comment was not preserved: %q", kept.Comment)
	}

	// 2. Explicitly sending comment: "" on PATCH must clear the comment.
	resp, body = doReq(http.MethodPatch, "/api/silences/"+created.ID, map[string]any{
		"comment": "",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch clear comment status=%d body=%s", resp.StatusCode, body)
	}
	var cleared silenceResponse
	if err := json.Unmarshal(body, &cleared); err != nil {
		t.Fatal(err)
	}
	if cleared.Comment != "" {
		t.Fatalf("PATCH explicit empty comment did not clear comment: %q", cleared.Comment)
	}

	// 3. Set comment back and clear via PUT as well.
	resp, body = doReq(http.MethodPut, "/api/silences/"+created.ID, map[string]any{
		"comment": "temp note",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put restore comment status=%d body=%s", resp.StatusCode, body)
	}
	resp, body = doReq(http.MethodPut, "/api/silences/"+created.ID, map[string]any{
		"comment": "",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put clear comment status=%d body=%s", resp.StatusCode, body)
	}
	var putCleared silenceResponse
	if err := json.Unmarshal(body, &putCleared); err != nil {
		t.Fatal(err)
	}
	if putCleared.Comment != "" {
		t.Fatalf("PUT explicit empty comment did not clear comment: %q", putCleared.Comment)
	}
}

func TestAlertAPISessionProjectAuditAndValidation(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	proj, err := store.CreateProject(ctx, "AuditProj", "audit-proj")
	if err != nil {
		t.Fatal(err)
	}
	admin := sessionCookie(t, store, "audit-alert-admin", "password", "admin")
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Alerts: alerts, Notifications: store})
	t.Cleanup(func() { _ = gw.Shutdown() })

	doReq := func(method, path, projectHeader string, payload any) (*http.Response, []byte) {
		t.Helper()
		var reader io.Reader
		if payload != nil {
			raw, marshalErr := json.Marshal(payload)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			reader = bytes.NewReader(raw)
		}
		req := httptest.NewRequest(method, path, reader)
		req.AddCookie(admin)
		if projectHeader != "" {
			req.Header.Set("X-LO-Project", projectHeader)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, reqErr := gw.App().Test(req)
		if reqErr != nil {
			t.Fatal(reqErr)
		}
		out, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		return resp, out
	}

	// 1. Project-scoped alert routes must fail closed with 404 for unknown X-LO-Project.
	for _, ep := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/api/alert-rules", nil},
		{http.MethodPost, "/api/alert-rules", map[string]any{"name": "R", "expr": "up == 0"}},
		{http.MethodGet, "/api/silences", nil},
		{http.MethodGet, "/api/inhibition-rules", nil},
		{http.MethodGet, "/api/alerts", nil},
	} {
		resp, body := doReq(ep.method, ep.path, "nonexistent-project", ep.body)
		if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "project_not_found") {
			t.Fatalf("%s %s missing project status=%d body=%s", ep.method, ep.path, resp.StatusCode, body)
		}
	}

	// 2. Centralized validation rejects invalid PromQL and oversized matcher regexes with 400.
	resp, body := doReq(http.MethodPost, "/api/alert-rules", proj.Slug, map[string]any{
		"name": "BadPromQL", "signal": "metrics", "expr": "sum(rate(up[))",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid PromQL status=%d want=400 body=%s", resp.StatusCode, body)
	}
	now := time.Now().UTC().Truncate(time.Second)
	resp, body = doReq(http.MethodPost, "/api/silences", proj.Slug, map[string]any{
		"matchers":  []map[string]string{{"name": "alertname", "op": "=~", "value": strings.Repeat("a", 1025)}},
		"starts_at": now, "ends_at": now.Add(time.Hour),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("overlong silence regex status=%d want=400 body=%s", resp.StatusCode, body)
	}

	// 3. Exercise every alert write operation and confirm full audit trail persistence.
	resp, body = doReq(http.MethodPost, "/api/channels", proj.Slug, map[string]any{
		"type": "webhook", "name": "audit-hook", "config": map[string]any{"url": receiver.URL},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create channel status=%d body=%s", resp.StatusCode, body)
	}
	var ch notificationChannelResponse
	if err := json.Unmarshal(body, &ch); err != nil {
		t.Fatal(err)
	}
	resp, _ = doReq(http.MethodPatch, "/api/channels/"+ch.ID, proj.Slug, map[string]any{"name": "audit-hook-2"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update channel status=%d", resp.StatusCode)
	}
	resp, _ = doReq(http.MethodPost, "/api/channels/"+ch.ID+"/test", proj.Slug, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test channel status=%d", resp.StatusCode)
	}

	resp, body = doReq(
		http.MethodPut,
		"/api/projects/"+strconv.Itoa(proj.ID)+"/alert-config",
		proj.Slug,
		map[string]any{
			"new_issue": true, "regression": true, "error_rate_threshold_per_min": 5, "channel_ids": []string{ch.ID},
		},
	)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("put project alert config status=%d body=%s", resp.StatusCode, body)
	}

	resp, body = doReq(http.MethodPost, "/api/alert-rules", proj.Slug, map[string]any{
		"name": "AuditRule", "expr": "up == 0", "for": "1m", "channel_ids": []string{ch.ID},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create rule status=%d body=%s", resp.StatusCode, body)
	}
	var rule alertRuleResponse
	if err := json.Unmarshal(body, &rule); err != nil {
		t.Fatal(err)
	}
	resp, _ = doReq(http.MethodPatch, "/api/alert-rules/"+rule.ID, proj.Slug, map[string]any{"severity": "critical"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update rule status=%d", resp.StatusCode)
	}
	resp, _ = doReq(http.MethodDelete, "/api/alert-rules/"+rule.ID, proj.Slug, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete rule status=%d", resp.StatusCode)
	}

	resp, body = doReq(http.MethodPost, "/api/silences", proj.Slug, map[string]any{
		"matchers":  []map[string]string{{"name": "alertname", "op": "=", "value": "AuditRule"}},
		"starts_at": now, "ends_at": now.Add(time.Hour), "comment": "audit",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create silence status=%d body=%s", resp.StatusCode, body)
	}
	var sil silenceResponse
	if err := json.Unmarshal(body, &sil); err != nil {
		t.Fatal(err)
	}
	resp, _ = doReq(http.MethodPatch, "/api/silences/"+sil.ID, proj.Slug, map[string]any{"comment": "audit-2"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update silence status=%d", resp.StatusCode)
	}
	resp, _ = doReq(http.MethodDelete, "/api/silences/"+sil.ID, proj.Slug, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete silence status=%d", resp.StatusCode)
	}

	resp, body = doReq(http.MethodPost, "/api/inhibition-rules", proj.Slug, map[string]any{
		"source_matchers": []map[string]string{{"name": "alertname", "op": "=", "value": "HostDown"}},
		"target_matchers": []map[string]string{{"name": "alertname", "op": "=", "value": "ServiceDown"}},
		"equal_labels":    []string{"instance"},
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create inhibition rule status=%d body=%s", resp.StatusCode, body)
	}
	var inhib meta.InhibitionRuleView
	if err := json.Unmarshal(body, &inhib); err != nil {
		t.Fatal(err)
	}
	resp, _ = doReq(
		http.MethodPatch,
		"/api/inhibition-rules/"+strconv.Itoa(inhib.ID),
		proj.Slug,
		map[string]any{"comment": "updated"},
	)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update inhibition rule status=%d", resp.StatusCode)
	}
	resp, _ = doReq(http.MethodDelete, "/api/inhibition-rules/"+strconv.Itoa(inhib.ID), proj.Slug, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete inhibition rule status=%d", resp.StatusCode)
	}

	// Seed a dead outbox item and exercise requeue audit.
	chID, _ := strconv.Atoi(ch.ID)
	outboxID, _, err := store.EnqueueNotification(ctx, meta.NotificationOutboxEnqueue{
		IdempotencyKey: "audit-requeue", ProjectID: proj.ID, ChannelID: chID,
		Payload: json.RawMessage(`{"status":"firing"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimNotifications(ctx, "audit-worker", 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RetryNotification(ctx, outboxID, "audit-worker", "fail", time.Minute, 1); err != nil {
		t.Fatal(err)
	}
	resp, _ = doReq(http.MethodPost, "/api/notifications/"+strconv.FormatInt(outboxID, 10)+"/retry", proj.Slug, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("requeue notification status=%d", resp.StatusCode)
	}

	resp, _ = doReq(http.MethodDelete, "/api/channels/"+ch.ID, proj.Slug, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete channel status=%d", resp.StatusCode)
	}

	records, err := store.ListAudit(ctx, meta.AuditListOptions{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	seenSuccess := make(map[string]bool)
	for _, rec := range records {
		if rec.Success {
			seenSuccess[rec.Action] = true
		}
	}
	wantActions := []string{
		"notification_channel.create",
		"notification_channel.update",
		"notification_channel.test",
		"notification_channel.delete",
		"project_alert_config.update",
		"alert_rule.create",
		"alert_rule.update",
		"alert_rule.delete",
		"silence.create",
		"silence.update",
		"silence.delete",
		"inhibition_rule.create",
		"inhibition_rule.update",
		"inhibition_rule.delete",
		"notification.requeue",
	}
	for _, action := range wantActions {
		if !seenSuccess[action] {
			t.Errorf("missing successful audit event for action %q (seen=%v)", action, seenSuccess)
		}
	}

	// 4. Rule-level last_error is surfaced in GET /api/alert-rules when native evaluation fails.
	failingAlerts := alertpkg.NewService(alertpkg.Config{
		Store:          store,
		GroupWait:      time.Millisecond,
		RepeatInterval: time.Hour,
		Evaluator: alertpkg.RuleEvaluatorConfig{
			Query:           failingTestRuleQuery{err: &net.OpError{Op: "dial", Net: "tcp", Err: io.EOF}},
			MemberID:        "api-last-err-member",
			TickInterval:    10 * time.Millisecond,
			MetricsInterval: 10 * time.Millisecond,
			LeaseTTL:        time.Minute,
			MemberTTL:       time.Minute,
		},
	})
	t.Cleanup(failingAlerts.Close)
	failGW := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Alerts: failingAlerts})
	t.Cleanup(func() { _ = failGW.Shutdown() })

	createReq := httptest.NewRequest(
		http.MethodPost,
		"/api/alert-rules",
		bytes.NewReader([]byte(`{"name":"BrokenQueryRule","expr":"up == 0","for":"0s"}`)),
	)
	createReq.AddCookie(admin)
	createReq.Header.Set("X-LO-Project", proj.Slug)
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := failGW.App().Test(createReq)
	if err != nil || createResp.StatusCode != http.StatusCreated {
		t.Fatalf("create failing rule status=%v err=%v", createResp, err)
	}
	createResp.Body.Close()

	deadline := time.Now().Add(2 * time.Second)
	var surfacedLastErr string
	for time.Now().Before(deadline) {
		listReq := httptest.NewRequest(http.MethodGet, "/api/alert-rules", nil)
		listReq.AddCookie(admin)
		listReq.Header.Set("X-LO-Project", proj.Slug)
		listResp, listErr := failGW.App().Test(listReq)
		if listErr != nil {
			t.Fatal(listErr)
		}
		var rules []alertRuleResponse
		_ = json.NewDecoder(listResp.Body).Decode(&rules)
		listResp.Body.Close()
		if len(rules) == 1 && rules[0].LastError != "" {
			surfacedLastErr = rules[0].LastError
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if surfacedLastErr == "" {
		t.Fatal("expected GET /api/alert-rules to surface non-empty last_error for failing rule")
	}
}

type failingTestRuleQuery struct {
	err error
}

func (q failingTestRuleQuery) Evaluate(
	_ context.Context,
	_ *meta.AlertRuleView,
	_ *meta.ProjectInfo,
	_ time.Time,
) ([]alertpkg.RuleSample, error) {
	return nil, q.err
}

func TestAlertRuleEvaluationIntervalAPIContract(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if _, err := store.CreateProject(ctx, "EvalAPI", "eval-api"); err != nil {
		t.Fatal(err)
	}
	admin := sessionCookie(t, store, "eval-api-admin", "password", "admin")
	alerts := alertpkg.NewService(alertpkg.Config{
		Store:          store,
		GroupWait:      time.Millisecond,
		RepeatInterval: time.Hour,
	})
	t.Cleanup(alerts.Close)
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Alerts: alerts})
	t.Cleanup(func() { _ = gw.Shutdown() })

	doRaw := func(method, path string, rawBody []byte) (*http.Response, []byte) {
		t.Helper()
		var reader io.Reader
		if rawBody != nil {
			reader = bytes.NewReader(rawBody)
		}
		req := httptest.NewRequest(method, path, reader)
		req.AddCookie(admin)
		req.Header.Set("X-LO-Project", "eval-api")
		if rawBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := gw.App().Test(req)
		if err != nil {
			t.Fatal(err)
		}
		out, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		return resp, out
	}

	// POST without evaluation_interval -> default "" and signal-specific effective interval.
	resp, body := doRaw(http.MethodPost, "/api/alert-rules", []byte(`{"name":"MetricsDefault","expr":"up == 0"}`))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST metrics default status=%d body=%s", resp.StatusCode, body)
	}
	var metricsRule alertRuleResponse
	if err := json.Unmarshal(body, &metricsRule); err != nil {
		t.Fatal(err)
	}
	if metricsRule.EvaluationInterval != "" || metricsRule.EffectiveEvaluationInterval != "5s" {
		t.Fatalf("metricsRule=%+v want \"\" / \"5s\"", metricsRule)
	}
	if !bytes.Contains(body, []byte(`"evaluation_interval":""`)) {
		t.Fatalf("expected explicit evaluation_interval field in JSON: %s", body)
	}

	resp, body = doRaw(
		http.MethodPost,
		"/api/alert-rules",
		[]byte(`{"name":"LogsDefault","signal":"logs","expr":"* | stats count() as c"}`),
	)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST logs default status=%d body=%s", resp.StatusCode, body)
	}
	var logsRule alertRuleResponse
	if err := json.Unmarshal(body, &logsRule); err != nil {
		t.Fatal(err)
	}
	if logsRule.EvaluationInterval != "" || logsRule.EffectiveEvaluationInterval != "1m" {
		t.Fatalf("logsRule=%+v want \"\" / \"1m\"", logsRule)
	}

	// POST with canonicalizable duration ("60s" -> "1m").
	resp, body = doRaw(
		http.MethodPost,
		"/api/alert-rules",
		[]byte(`{"name":"CustomInterval","expr":"up == 0","evaluation_interval":"60s"}`),
	)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST custom interval status=%d body=%s", resp.StatusCode, body)
	}
	var custom alertRuleResponse
	if err := json.Unmarshal(body, &custom); err != nil {
		t.Fatal(err)
	}
	if custom.EvaluationInterval != "1m" || custom.EffectiveEvaluationInterval != "1m" {
		t.Fatalf("custom=%+v want 1m / 1m", custom)
	}

	// PATCH omitting evaluation_interval keeps existing override.
	resp, body = doRaw(http.MethodPatch, "/api/alert-rules/"+custom.ID, []byte(`{"severity":"critical"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH omit evaluation_interval status=%d body=%s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &custom); err != nil {
		t.Fatal(err)
	}
	if custom.EvaluationInterval != "1m" || custom.EffectiveEvaluationInterval != "1m" {
		t.Fatalf("patched omit custom=%+v want 1m / 1m", custom)
	}

	// PATCH with "" resets to default.
	resp, body = doRaw(http.MethodPatch, "/api/alert-rules/"+custom.ID, []byte(`{"evaluation_interval":""}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH empty reset status=%d body=%s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &custom); err != nil {
		t.Fatal(err)
	}
	if custom.EvaluationInterval != "" || custom.EffectiveEvaluationInterval != "5s" {
		t.Fatalf("patched empty reset custom=%+v want \"\" / \"5s\"", custom)
	}

	// PATCH to "10s", then PATCH with null resets to default.
	resp, body = doRaw(http.MethodPatch, "/api/alert-rules/"+custom.ID, []byte(`{"evaluation_interval":"10s"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH set 10s status=%d body=%s", resp.StatusCode, body)
	}
	// Switching metrics(10s) -> logs without resetting evaluation_interval fails with 400.
	resp, body = doRaw(
		http.MethodPatch,
		"/api/alert-rules/"+custom.ID,
		[]byte(`{"signal":"logs","expr":"* | stats count() as c"}`),
	)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("PATCH signal switch with 10s override status=%d want 400 body=%s", resp.StatusCode, body)
	}

	resp, body = doRaw(http.MethodPatch, "/api/alert-rules/"+custom.ID, []byte(`{"evaluation_interval":null}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH null reset status=%d body=%s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &custom); err != nil {
		t.Fatal(err)
	}
	if custom.EvaluationInterval != "" || custom.EffectiveEvaluationInterval != "5s" {
		t.Fatalf("patched null reset custom=%+v want \"\" / \"5s\"", custom)
	}

	// Invalid payloads on POST and PATCH must return 400.
	badPayloads := []string{
		`{"name":"BadTypeNum","expr":"up == 0","evaluation_interval":30}`,
		`{"name":"BadTypeBool","expr":"up == 0","evaluation_interval":true}`,
		`{"name":"BadTypeObj","expr":"up == 0","evaluation_interval":{}}`,
		`{"name":"BadDur","expr":"up == 0","evaluation_interval":"abc"}`,
		`{"name":"BadZero","expr":"up == 0","evaluation_interval":"0s"}`,
		`{"name":"BadStep","expr":"up == 0","evaluation_interval":"7s"}`,
		`{"name":"BadMinMetrics","expr":"up == 0","evaluation_interval":"4s"}`,
		`{"name":"BadMax","expr":"up == 0","evaluation_interval":"1h5s"}`,
		`{"name":"BadMinLogs","signal":"logs","expr":"* | stats count() as c","evaluation_interval":"25s"}`,
	}
	for _, raw := range badPayloads {
		resp, body = doRaw(http.MethodPost, "/api/alert-rules", []byte(raw))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST invalid %s status=%d want 400 body=%s", raw, resp.StatusCode, body)
		}
	}
	for _, raw := range []string{
		`{"evaluation_interval":30}`,
		`{"evaluation_interval":"7s"}`,
		`{"evaluation_interval":"4s"}`,
	} {
		resp, body = doRaw(http.MethodPatch, "/api/alert-rules/"+custom.ID, []byte(raw))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("PATCH invalid %s status=%d want 400 body=%s", raw, resp.StatusCode, body)
		}
	}
}

func TestAlertRuleRepeatIntervalAPIContract(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if _, err := store.CreateProject(ctx, "RepeatAPI", "repeat-api"); err != nil {
		t.Fatal(err)
	}
	admin := sessionCookie(t, store, "repeat-api-admin", "password", "admin")
	alerts := alertpkg.NewService(alertpkg.Config{
		Store:          store,
		GroupWait:      time.Millisecond,
		RepeatInterval: 2 * time.Hour,
	})
	t.Cleanup(alerts.Close)
	gw := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Alerts: alerts})
	t.Cleanup(func() { _ = gw.Shutdown() })

	doRaw := func(method, path string, rawBody []byte) (*http.Response, []byte) {
		t.Helper()
		var reader io.Reader
		if rawBody != nil {
			reader = bytes.NewReader(rawBody)
		}
		req := httptest.NewRequest(method, path, reader)
		req.AddCookie(admin)
		req.Header.Set("X-LO-Project", "repeat-api")
		if rawBody != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := gw.App().Test(req)
		if err != nil {
			t.Fatal(err)
		}
		out, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		return resp, out
	}
	decode := func(body []byte) alertRuleResponse {
		t.Helper()
		var rule alertRuleResponse
		if err := json.Unmarshal(body, &rule); err != nil {
			t.Fatal(err)
		}
		return rule
	}

	// POST without repeat_interval inherits the configured global default.
	resp, body := doRaw(http.MethodPost, "/api/alert-rules", []byte(`{"name":"Inherited","expr":"up == 0"}`))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST inherited status=%d body=%s", resp.StatusCode, body)
	}
	if rule := decode(body); rule.RepeatInterval != "" || rule.EffectiveRepeatInterval != "2h" {
		t.Fatalf("inherited=%+v want \"\" / \"2h\"", rule)
	}
	if !bytes.Contains(body, []byte(`"repeat_interval":""`)) ||
		!bytes.Contains(body, []byte(`"effective_repeat_interval":"2h"`)) {
		t.Fatalf("expected explicit repeat_interval fields in JSON: %s", body)
	}

	// POST canonicalizes "60m" -> "1h".
	resp, body = doRaw(
		http.MethodPost,
		"/api/alert-rules",
		[]byte(`{"name":"Custom","expr":"up == 0","repeat_interval":"60m","evaluation_interval":"30s"}`),
	)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST custom status=%d body=%s", resp.StatusCode, body)
	}
	custom := decode(body)
	if custom.RepeatInterval != "1h" || custom.EffectiveRepeatInterval != "1h" || custom.EvaluationInterval != "30s" {
		t.Fatalf("custom=%+v want 1h / 1h / eval 30s", custom)
	}

	// PATCH omitting repeat_interval keeps the stored override.
	resp, body = doRaw(http.MethodPatch, "/api/alert-rules/"+custom.ID, []byte(`{"severity":"critical"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH omit status=%d body=%s", resp.StatusCode, body)
	}
	if rule := decode(body); rule.RepeatInterval != "1h" || rule.EvaluationInterval != "30s" {
		t.Fatalf("PATCH omit rule=%+v want 1h kept", rule)
	}

	// PUT omitting repeat_interval keeps it too.
	resp, body = doRaw(
		http.MethodPut,
		"/api/alert-rules/"+custom.ID,
		[]byte(`{"name":"Custom","expr":"up == 1","severity":"info"}`),
	)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT omit status=%d body=%s", resp.StatusCode, body)
	}
	if rule := decode(body); rule.RepeatInterval != "1h" || rule.Expr != "up == 1" {
		t.Fatalf("PUT omit rule=%+v want 1h kept", rule)
	}

	// PATCH sets a new value; "" and null reset to the global default.
	resp, body = doRaw(http.MethodPatch, "/api/alert-rules/"+custom.ID, []byte(`{"repeat_interval":"168h"}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH 168h status=%d body=%s", resp.StatusCode, body)
	}
	if rule := decode(body); rule.RepeatInterval != "7d" || rule.EffectiveRepeatInterval != "7d" {
		t.Fatalf("PATCH 168h rule=%+v want 7d", rule)
	}
	resp, body = doRaw(http.MethodPatch, "/api/alert-rules/"+custom.ID, []byte(`{"repeat_interval":""}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH empty status=%d body=%s", resp.StatusCode, body)
	}
	if rule := decode(body); rule.RepeatInterval != "" || rule.EffectiveRepeatInterval != "2h" {
		t.Fatalf("PATCH empty rule=%+v want \"\" / 2h", rule)
	}
	resp, body = doRaw(http.MethodPut, "/api/alert-rules/"+custom.ID, []byte(`{"repeat_interval":"15m"}`))
	if resp.StatusCode != http.StatusOK || decode(body).RepeatInterval != "15m" {
		t.Fatalf("PUT 15m status=%d body=%s", resp.StatusCode, body)
	}
	resp, body = doRaw(http.MethodPut, "/api/alert-rules/"+custom.ID, []byte(`{"repeat_interval":null}`))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PUT null status=%d body=%s", resp.StatusCode, body)
	}
	if rule := decode(body); rule.RepeatInterval != "" || rule.EffectiveRepeatInterval != "2h" {
		t.Fatalf("PUT null rule=%+v want \"\" / 2h", rule)
	}

	// GET lists both fields.
	resp, body = doRaw(http.MethodGet, "/api/alert-rules", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", resp.StatusCode, body)
	}
	var listed []alertRuleResponse
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed=%+v want 2 rules", listed)
	}
	for _, rule := range listed {
		if rule.RepeatInterval != "" || rule.EffectiveRepeatInterval != "2h" {
			t.Fatalf("listed rule=%+v want \"\" / 2h", rule)
		}
	}

	// Invalid type, duration, or range returns 400 on POST, PATCH, and PUT.
	for _, raw := range []string{
		`{"name":"BadNum","expr":"up == 0","repeat_interval":60}`,
		`{"name":"BadBool","expr":"up == 0","repeat_interval":true}`,
		`{"name":"BadDur","expr":"up == 0","repeat_interval":"hourly"}`,
		`{"name":"BadZero","expr":"up == 0","repeat_interval":"0s"}`,
		`{"name":"BadMin","expr":"up == 0","repeat_interval":"59s"}`,
		`{"name":"BadMax","expr":"up == 0","repeat_interval":"7d1m"}`,
		`{"name":"BadFraction","expr":"up == 0","repeat_interval":"90500ms"}`,
	} {
		resp, body = doRaw(http.MethodPost, "/api/alert-rules", []byte(raw))
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("POST invalid %s status=%d want 400 body=%s", raw, resp.StatusCode, body)
		}
	}
	for _, method := range []string{http.MethodPatch, http.MethodPut} {
		for _, raw := range []string{
			`{"repeat_interval":30}`,
			`{"repeat_interval":"30s"}`,
			`{"repeat_interval":"8d"}`,
		} {
			resp, body = doRaw(method, "/api/alert-rules/"+custom.ID, []byte(raw))
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s invalid %s status=%d want 400 body=%s", method, raw, resp.StatusCode, body)
			}
		}
	}
}
