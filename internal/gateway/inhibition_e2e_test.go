package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/alert"
	"github.com/eimsound/lazy-observe/internal/meta"
	"github.com/eimsound/lazy-observe/internal/outbound"
)

func TestInhibitionPipelineRealStoreAndRBAC(t *testing.T) {
	ctx := context.Background()
	store := testStore(t)
	if _, err := store.CreateProject(ctx, "Inhibition", "inhibition"); err != nil {
		t.Fatal(err)
	}
	member := sessionCookie(t, store, "inhibition-member", "password", "member")
	viewer := sessionCookie(t, store, "inhibition-viewer", "password", "viewer")

	notifications := make(chan alert.Notification, 10)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var n alert.Notification
		if err := json.NewDecoder(r.Body).Decode(&n); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		notifications <- n
		w.WriteHeader(http.StatusNoContent)
	}))
	defer webhook.Close()

	alerts := alert.NewService(alert.Config{
		Store:          store,
		OutboundPolicy: outbound.Policy{AllowPrivate: true},
		GroupWait:      10 * time.Millisecond,
		RepeatInterval: time.Hour,
		Evaluator: alert.RuleEvaluatorConfig{
			Query:           inhibitionRuleQuery{},
			MemberID:        "inhibition-engine",
			TickInterval:    5 * time.Millisecond,
			MetricsInterval: 5 * time.Millisecond,
		},
	})
	t.Cleanup(alerts.Close)
	channel, err := alerts.CreateChannel(
		ctx,
		alert.ChannelInput{Type: "webhook", Name: "inhibition-webhook", Config: map[string]any{"url": webhook.URL}},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"HostDown", "ServiceDown"} {
		if _, err := alerts.CreateRule(
			ctx,
			alert.RuleInput{Project: "inhibition", Name: name, Expr: "vector(1)", ChannelIDs: []int{channel.ID}},
		); err != nil {
			t.Fatal(err)
		}
	}
	app := newMetadataTestGateway(store, Config{Tokens: store, Store: store, Alerts: alerts}).App()
	body := map[string]any{
		"source_matchers": []map[string]string{{"name": "alertname", "op": "=", "value": "HostDown"}},
		"target_matchers": []map[string]string{{"name": "alertname", "op": "=", "value": "ServiceDown"}},
		"equal_labels":    []string{"instance"}, "comment": "host suppresses services",
	}
	payload, _ := json.Marshal(body)
	request := func(method, path string, cookie *http.Cookie, data []byte) *http.Response {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-LO-Project", "inhibition")
		req.AddCookie(cookie)
		resp, requestErr := app.Test(req)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		return resp
	}
	resp := request(http.MethodPost, "/api/inhibition-rules", viewer, payload)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer create status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = request(http.MethodPost, "/api/inhibition-rules", member, payload)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("member create status=%d", resp.StatusCode)
	}
	var inhibition meta.InhibitionRuleView
	if err := json.NewDecoder(resp.Body).Decode(&inhibition); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case n := <-notifications:
		if n.Rule != "HostDown" {
			t.Fatalf("first notification=%#v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("source notification timeout")
	}
	select {
	case n := <-notifications:
		t.Fatalf("inhibited target notified: %#v", n)
	case <-time.After(60 * time.Millisecond):
	}
	resp = request(http.MethodGet, "/api/alerts?inhibited=true", viewer, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("filtered events status=%d", resp.StatusCode)
	}
	var events []alertEventResponse
	if err := json.NewDecoder(resp.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(events) != 1 || events[0].Labels["alertname"] != "ServiceDown" || !events[0].Inhibited {
		t.Fatalf("inhibited events=%#v", events)
	}

	resp = request(http.MethodDelete, fmt.Sprintf("/api/inhibition-rules/%d", inhibition.ID), member, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status=%d", resp.StatusCode)
	}
	resp.Body.Close()
	select {
	case n := <-notifications:
		if n.Rule != "ServiceDown" {
			t.Fatalf("post-delete notification=%#v", n)
		}
	case <-time.After(time.Second):
		t.Fatal("target did not resume after inhibition deletion")
	}
}

type inhibitionRuleQuery struct{}

func (inhibitionRuleQuery) Evaluate(
	_ context.Context,
	rule *meta.AlertRuleView,
	project *meta.ProjectInfo,
	_ time.Time,
) ([]alert.RuleSample, error) {
	labels := map[string]string{"project_id": project.Slug, "alertname": rule.Name, "instance": "node-1"}
	return []alert.RuleSample{
		{Fingerprint: alert.Fingerprint(labels), Labels: labels, Annotations: map[string]string{}, Value: 1},
	}, nil
}
