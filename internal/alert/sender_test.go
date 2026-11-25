package alert

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eimsound/lazy-observe/internal/outbound"
)

func TestChannelPayloadGolden(t *testing.T) {
	n := Notification{
		Project: "alpha",
		Rule:    "DiskFull",
		Status:  "firing",
		Alerts: []Alert{
			{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "DiskFull", "instance": "node-1", "project_id": "alpha"},
				Annotations: map[string]string{"summary": "disk full"},
				StartsAt:    time.Date(2026, 7, 17, 1, 2, 3, 0, time.UTC),
				Fingerprint: "abc",
			},
		},
	}
	for _, kind := range []string{"webhook", "feishu", "dingtalk", "wecom", "slack"} {
		t.Run(kind, func(t *testing.T) {
			got, err := ChannelPayload(kind, n)
			if err != nil {
				t.Fatal(err)
			}
			assertGolden(t, kind+".golden", got)
		})
	}
	assertGolden(t, "smtp.golden", EmailPayload("alerts@example.com", []string{"ops@example.com"}, n))
}

func TestStormPayloadIncludesSummary(t *testing.T) {
	n := Notification{
		Project: "alpha",
		Rule:    "alert_storm",
		Status:  "firing",
		Storm: &StormSummary{
			GroupCount: 12,
			Severities: []Count{{Name: "critical", Count: 8}},
			Rules:      []Count{{Name: "HostDown", Count: 5}},
		},
	}
	for _, kind := range []string{"feishu", "dingtalk", "wecom"} {
		payload, err := ChannelPayload(kind, n)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range [][]byte{[]byte("Active alert groups: 12"), []byte("critical=8"), []byte("HostDown=5")} {
			if !bytes.Contains(payload, want) {
				t.Errorf("%s payload missing %q: %s", kind, want, payload)
			}
		}
	}
}
func assertGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	want, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	got = bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n"))
	want = bytes.TrimSuffix(want, []byte("\n"))
	if !bytes.Equal(got, want) {
		t.Fatalf("%s mismatch\ngot:  %s\nwant: %s", name, got, want)
	}
}

func TestSenderBlocksSSRFRedirectsAndSanitizesErrors(t *testing.T) {
	var redirectTargetHit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectTargetHit = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirector.Close()

	// Default sender blocks loopback / metadata / leading-zero port.
	strictSender := &Sender{Timeout: time.Second}
	for _, badURL := range []string{
		target.URL + "?access_token=top-secret-token",
		"http://169.254.169.254/latest/meta-data/?key=meta-secret",
		"http://example.com:080/hook?token=port-secret",
	} {
		attempts, err := strictSender.Send(
			context.Background(),
			Channel{Type: "webhook", Config: map[string]any{"url": badURL}},
			Notification{Project: "p", Rule: "r", Status: "firing"},
		)
		if attempts != 1 || err == nil {
			t.Fatalf("url=%q attempts=%d err=%v want single-attempt error", badURL, attempts, err)
		}
		for _, leaked := range []string{"top-secret-token", "meta-secret", "port-secret", "169.254.169.254"} {
			if strings.Contains(err.Error(), leaked) {
				t.Fatalf("error leaked %q: %v", leaked, err)
			}
		}
	}

	// Even with private targets allowed, HTTP 3xx redirects are rejected.
	privateSender := &Sender{Outbound: outbound.Policy{AllowPrivate: true}, Timeout: time.Second}
	_, err := privateSender.Send(
		context.Background(),
		Channel{Type: "dingtalk", Config: map[string]any{"url": redirector.URL + "?access_token=redir-secret"}},
		Notification{Project: "p", Rule: "r", Status: "firing"},
	)
	if !errors.Is(err, outbound.ErrRedirectBlocked) {
		t.Fatalf("redirect err=%v want ErrRedirectBlocked", err)
	}
	if redirectTargetHit {
		t.Fatal("redirect target must not be contacted")
	}
	if strings.Contains(err.Error(), "redir-secret") {
		t.Fatalf("redirect error leaked token: %v", err)
	}
}

func TestSenderChecksBusinessErrorCodesAndDrainsBody(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		body    string
		wantErr bool
	}{
		{"feishu ok", "feishu", `{"code":0,"msg":"success"}`, false},
		{"feishu code err", "feishu", `{"code":19024,"msg":"keyword mismatch"}`, true},
		{"feishu legacy StatusCode err", "feishu", `{"StatusCode":19024,"StatusMessage":"bad"}`, true},
		{"feishu invalid json", "feishu", `<html>waf</html>`, true},
		{"dingtalk ok", "dingtalk", `{"errcode":0,"errmsg":"ok"}`, false},
		{"dingtalk err", "dingtalk", `{"errcode":310000,"errmsg":"sign invalid"}`, true},
		{"wecom ok", "wecom", `{"errcode":0,"errmsg":"ok"}`, false},
		{"wecom err", "wecom", `{"errcode":93000,"errmsg":"invalid key"}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			sender := &Sender{Client: srv.Client(), Timeout: time.Second}
			attempts, err := sender.Send(
				context.Background(),
				Channel{Type: tc.kind, Config: map[string]any{"url": srv.URL}},
				Notification{Project: "alpha", Rule: "HighCPU", Status: "firing"},
			)
			if attempts != 1 {
				t.Fatalf("attempts=%d want 1", attempts)
			}
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for body %s", tc.body)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestSMTPHeaderInjectionSSRFAndTimeout(t *testing.T) {
	// EmailPayload strips CR/LF to prevent SMTP header injection.
	payload := EmailPayload(
		"alerts@example.com\r\nBcc: evil@example.com",
		[]string{"ops@example.com\r\nCc: leak@example.com"},
		Notification{
			Project: "alpha\r\nX-Injected: yes",
			Rule:    "DiskFull\r\nBcc: attacker@evil.test",
			Status:  "firing",
		},
	)
	headerEnd := bytes.Index(payload, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		t.Fatalf("missing header terminator: %q", payload)
	}
	headers := string(payload[:headerEnd])
	for _, line := range strings.Split(headers, "\r\n") {
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "bcc:") || strings.HasPrefix(lower, "cc:") ||
			strings.HasPrefix(lower, "x-injected:") {
			t.Fatalf("injected header present in %q", headers)
		}
	}

	// Default sender blocks loopback SMTP SSRF.
	strict := &Sender{Timeout: time.Second}
	_, err := strict.Send(context.Background(), Channel{
		Type: "smtp",
		Config: map[string]any{
			"host": "127.0.0.1", "port": 2525, "from": "alerts@example.com", "to": "ops@example.com",
		},
	}, Notification{Project: "alpha", Rule: "DiskFull", Status: "firing"})
	if !errors.Is(err, outbound.ErrBlockedAddress) {
		t.Fatalf("loopback smtp err=%v want ErrBlockedAddress", err)
	}

	// Leading-zero SMTP port is rejected.
	_, err = strict.Send(context.Background(), Channel{
		Type: "smtp",
		Config: map[string]any{
			"host": "smtp.example.com", "port": "0587", "from": "alerts@example.com", "to": "ops@example.com",
		},
	}, Notification{Project: "alpha", Rule: "DiskFull", Status: "firing"})
	if !errors.Is(err, outbound.ErrInvalidTarget) {
		t.Fatalf("leading-zero smtp port err=%v want ErrInvalidTarget", err)
	}

	// Blackhole SMTP server must respect ctx / sender timeout instead of hanging.
	blackhole, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blackhole.Close()
	go func() {
		conn, acceptErr := blackhole.Accept()
		if acceptErr == nil {
			defer conn.Close()
			time.Sleep(2 * time.Second)
		}
	}()
	host, portStr, _ := net.SplitHostPort(blackhole.Addr().String())
	timeoutSender := &Sender{Outbound: outbound.Policy{AllowPrivate: true}, Timeout: 150 * time.Millisecond}
	started := time.Now()
	_, err = timeoutSender.Send(context.Background(), Channel{
		Type: "smtp",
		Config: map[string]any{
			"host": host, "port": portStr, "from": "alerts@example.com", "to": "ops@example.com",
		},
	}, Notification{Project: "alpha", Rule: "DiskFull", Status: "firing"})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("blackhole smtp succeeded, want timeout error")
	}
	if elapsed > time.Second {
		t.Fatalf("blackhole smtp took %v, exceeded bounded timeout", elapsed)
	}

	// Real minimal SMTP server verifies end-to-end delivery and sanitized Subject.
	mockSMTP, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer mockSMTP.Close()
	receivedData := make(chan string, 1)
	go func() {
		conn, acceptErr := mockSMTP.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		_, _ = io.WriteString(conn, "220 mock.smtp ESMTP\r\n")
		var inData bool
		var dataBuf strings.Builder
		for {
			line, readErr := reader.ReadString('\n')
			if readErr != nil {
				return
			}
			if inData {
				if line == ".\r\n" {
					inData = false
					receivedData <- dataBuf.String()
					_, _ = io.WriteString(conn, "250 OK\r\n")
					continue
				}
				dataBuf.WriteString(line)
				continue
			}
			upper := strings.ToUpper(line)
			switch {
			case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"):
				_, _ = io.WriteString(conn, "250-mock.smtp\r\n250 SIZE 1024000\r\n")
			case strings.HasPrefix(upper, "MAIL FROM:"), strings.HasPrefix(upper, "RCPT TO:"):
				_, _ = io.WriteString(conn, "250 OK\r\n")
			case strings.HasPrefix(upper, "DATA"):
				inData = true
				_, _ = io.WriteString(conn, "354 End data with <CR><LF>.<CR><LF>\r\n")
			case strings.HasPrefix(upper, "QUIT"):
				_, _ = io.WriteString(conn, "221 Bye\r\n")
				return
			default:
				_, _ = io.WriteString(conn, "250 OK\r\n")
			}
		}
	}()
	smtpHost, smtpPort, _ := net.SplitHostPort(mockSMTP.Addr().String())
	okSender := &Sender{Outbound: outbound.Policy{AllowPrivate: true}, Timeout: 2 * time.Second}
	if _, err := okSender.Send(context.Background(), Channel{
		Type: "smtp",
		Config: map[string]any{
			"host": smtpHost, "port": smtpPort, "from": "alerts@example.com", "to": "ops@example.com",
		},
	}, Notification{Project: "alpha", Rule: "DiskFull\r\nBcc: evil@example.com", Status: "firing"}); err != nil {
		t.Fatalf("mock smtp send failed: %v", err)
	}
	select {
	case raw := <-receivedData:
		if strings.Contains(raw, "\r\nBcc:") {
			t.Fatalf("injected Bcc header reached SMTP server: %q", raw)
		}
	case <-time.After(time.Second):
		t.Fatal("mock SMTP server did not receive DATA")
	}
}

func TestChannelPayloadWithConsoleLinks(t *testing.T) {
	const base = "https://observe.example.com"
	ts := time.Date(2026, 7, 17, 1, 2, 3, 0, time.UTC)

	t.Run("single rule alert has CTA only and no per-alert View link", func(t *testing.T) {
		n := NotificationLinks(base, Notification{
			Project: "alpha",
			Rule:    "DiskFull",
			Status:  "firing",
			Alerts: []Alert{{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "DiskFull"},
				StartsAt:    ts,
				Fingerprint: "fp-1",
			}},
		})
		wantURL := base + "/alerts?fingerprint=fp-1&project=alpha"

		// Webhook
		whRaw, err := ChannelPayload("webhook", n)
		if err != nil {
			t.Fatal(err)
		}
		var whGot Notification
		if err := json.Unmarshal(whRaw, &whGot); err != nil {
			t.Fatal(err)
		}
		if whGot.URL != wantURL || len(whGot.Alerts) != 1 || whGot.Alerts[0].URL != wantURL {
			t.Fatalf("webhook links mismatch: %+v", whGot)
		}

		// Feishu: action button present, no [View](...) in lark_md
		feishuRaw, err := ChannelPayload("feishu", n)
		if err != nil {
			t.Fatal(err)
		}
		var feishuGot struct {
			Card struct {
				Elements []struct {
					Tag  string `json:"tag"`
					Text struct {
						Content string `json:"content"`
					} `json:"text"`
					Actions []struct {
						Tag  string `json:"tag"`
						Type string `json:"type"`
						URL  string `json:"url"`
						Text struct {
							Tag     string `json:"tag"`
							Content string `json:"content"`
						} `json:"text"`
					} `json:"actions"`
				} `json:"elements"`
			} `json:"card"`
		}
		if err := json.Unmarshal(feishuRaw, &feishuGot); err != nil {
			t.Fatal(err)
		}
		if len(feishuGot.Card.Elements) != 2 {
			t.Fatalf("feishu elements len = %d, want 2: %s", len(feishuGot.Card.Elements), feishuRaw)
		}
		if strings.Contains(feishuGot.Card.Elements[0].Text.Content, "[View](") {
			t.Fatalf(
				"single alert feishu should not include per-alert View link: %q",
				feishuGot.Card.Elements[0].Text.Content,
			)
		}
		action := feishuGot.Card.Elements[1]
		if action.Tag != "action" || len(action.Actions) != 1 ||
			action.Actions[0].Tag != "button" ||
			action.Actions[0].Type != "primary" ||
			action.Actions[0].Text.Tag != "plain_text" ||
			action.Actions[0].Text.Content != "View in lazy-observe" ||
			action.Actions[0].URL != wantURL {
			t.Fatalf("unexpected feishu action element: %+v", action)
		}

		// DingTalk & WeCom
		for _, kind := range []string{"dingtalk", "wecom"} {
			raw, err := ChannelPayload(kind, n)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Markdown struct {
					Text    string `json:"text"`
					Content string `json:"content"`
				} `json:"markdown"`
			}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			body := decoded.Markdown.Text
			if kind == "wecom" {
				body = decoded.Markdown.Content
			}
			if strings.Contains(body, "[View](") {
				t.Fatalf("%s single alert should not include per-alert View link: %q", kind, body)
			}
			if !strings.HasSuffix(body, "\n\n[View in lazy-observe]("+wantURL+")") {
				t.Fatalf("%s missing CTA suffix: %q", kind, body)
			}
		}

		// Slack
		slackRaw, err := ChannelPayload("slack", n)
		if err != nil {
			t.Fatal(err)
		}
		var slackGot struct {
			Blocks []struct {
				Text struct {
					Text string `json:"text"`
				} `json:"text"`
			} `json:"blocks"`
		}
		if err := json.Unmarshal(slackRaw, &slackGot); err != nil {
			t.Fatal(err)
		}
		slackText := slackGot.Blocks[0].Text.Text
		if strings.Contains(slackText, "|View>") {
			t.Fatalf("slack single alert should not include per-alert View link: %q", slackText)
		}
		wantSlackCTA := "\n\n<https://observe.example.com/alerts?fingerprint=fp-1&amp;project=alpha|View in lazy-observe>"
		if !strings.HasSuffix(slackText, wantSlackCTA) {
			t.Fatalf("slack text = %q, want suffix %q", slackText, wantSlackCTA)
		}

		// Email
		emailRaw := string(EmailPayload("alerts@example.com", []string{"ops@example.com"}, n))
		sep := "\r\n\r\n"
		idx := strings.Index(emailRaw, sep)
		if idx < 0 {
			t.Fatalf("email missing header separator: %q", emailRaw)
		}
		emailBody := emailRaw[idx+len(sep):]
		wantPrefix := "View in lazy-observe: " + wantURL + "\r\n\r\n{\n"
		if !strings.HasPrefix(emailBody, wantPrefix) {
			t.Fatalf("email body = %q, want prefix %q", emailBody, wantPrefix)
		}
		if strings.Contains(emailBody, `\u0026`) {
			t.Fatalf("email body HTML-escaped '&': %q", emailBody)
		}
		if !strings.Contains(emailBody, `"url": "`+wantURL+`"`) {
			t.Fatalf("email JSON body missing unescaped url %q: %q", wantURL, emailBody)
		}
	})

	t.Run("multi rule alerts include per-alert links and CTA across all channels", func(t *testing.T) {
		n := NotificationLinks(base, Notification{
			Project: "alpha",
			Rule:    "DiskFull",
			Status:  "firing",
			Alerts: []Alert{
				{Status: "firing", Labels: map[string]string{"instance": "n1"}, StartsAt: ts, Fingerprint: "fp-1"},
				{Status: "firing", Labels: map[string]string{"instance": "n2"}, StartsAt: ts, Fingerprint: "fp-2"},
			},
		})
		wantTop := base + "/alerts?project=alpha&rule=DiskFull"
		wantA1 := base + "/alerts?fingerprint=fp-1&project=alpha"
		wantA2 := base + "/alerts?fingerprint=fp-2&project=alpha"

		feishuRaw, err := ChannelPayload("feishu", n)
		if err != nil {
			t.Fatal(err)
		}
		var feishuGot struct {
			Card struct {
				Elements []struct {
					Text struct {
						Content string `json:"content"`
					} `json:"text"`
					Actions []struct {
						URL string `json:"url"`
					} `json:"actions"`
				} `json:"elements"`
			} `json:"card"`
		}
		if err := json.Unmarshal(feishuRaw, &feishuGot); err != nil {
			t.Fatal(err)
		}
		feishuText := feishuGot.Card.Elements[0].Text.Content
		if !strings.Contains(feishuText, "[View]("+wantA1+")") || !strings.Contains(feishuText, "[View]("+wantA2+")") {
			t.Fatalf("feishu missing per-alert links: %q", feishuText)
		}
		if feishuGot.Card.Elements[1].Actions[0].URL != wantTop {
			t.Fatalf("feishu button url = %q, want %q", feishuGot.Card.Elements[1].Actions[0].URL, wantTop)
		}

		for _, kind := range []string{"dingtalk", "wecom"} {
			raw, err := ChannelPayload(kind, n)
			if err != nil {
				t.Fatal(err)
			}
			var decoded struct {
				Markdown struct {
					Text    string `json:"text"`
					Content string `json:"content"`
				} `json:"markdown"`
			}
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			body := decoded.Markdown.Text
			if kind == "wecom" {
				body = decoded.Markdown.Content
			}
			for _, want := range []string{
				"\n\n[View](" + wantA1 + ")",
				"\n\n[View](" + wantA2 + ")",
				"\n\n[View in lazy-observe](" + wantTop + ")",
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("%s missing %q in %q", kind, want, body)
				}
			}
		}

		slackRaw, err := ChannelPayload("slack", n)
		if err != nil {
			t.Fatal(err)
		}
		var slackGot struct {
			Blocks []struct {
				Text struct {
					Text string `json:"text"`
				} `json:"text"`
			} `json:"blocks"`
		}
		if err := json.Unmarshal(slackRaw, &slackGot); err != nil {
			t.Fatal(err)
		}
		slackText := slackGot.Blocks[0].Text.Text
		for _, want := range []string{
			"<https://observe.example.com/alerts?fingerprint=fp-1&amp;project=alpha|View>",
			"<https://observe.example.com/alerts?fingerprint=fp-2&amp;project=alpha|View>",
			"\n\n<https://observe.example.com/alerts?project=alpha&amp;rule=DiskFull|View in lazy-observe>",
		} {
			if !strings.Contains(slackText, want) {
				t.Fatalf("slack missing %q in %q", want, slackText)
			}
		}
	})

	t.Run("issue storm and test notifications carry expected links", func(t *testing.T) {
		issueN := NotificationLinks(base, Notification{
			Project: "alpha",
			Rule:    "new_issue",
			Status:  "firing",
			Alerts: []Alert{{
				Status:   "firing",
				Labels:   map[string]string{"source": "platform", "kind": "new_issue", "issue_id": "42"},
				StartsAt: ts,
			}},
		})
		if issueN.URL != base+"/issues/42?project=alpha" {
			t.Fatalf("issue URL = %q", issueN.URL)
		}
		stormN := NotificationLinks(base, Notification{
			Project: "alpha",
			Rule:    "alert_storm",
			Status:  "firing",
			Storm:   &StormSummary{GroupCount: 5},
		})
		if stormN.URL != base+"/alerts?project=alpha&status=firing" {
			t.Fatalf("storm URL = %q", stormN.URL)
		}
		testN := NotificationLinks(base, Notification{
			Project: "test",
			Rule:    "test",
			Status:  "firing",
			Test:    true,
			Alerts:  []Alert{{Status: "firing", Labels: map[string]string{"test": "true"}}},
		})
		if testN.URL != base+"/alerts/channels" || testN.Alerts[0].URL != "" {
			t.Fatalf("test notification links = %+v", testN)
		}
	})

	t.Run("markdown paren/space encoding slack escaping and email CR/LF defense", func(t *testing.T) {
		if got := mdURL(
			"https://example.com/alerts?rule=a b(c)&d",
		); got != "https://example.com/alerts?rule=a%20b%28c%29&d" {
			t.Fatalf("mdURL = %q", got)
		}
		if got := slackLink(
			"https://example.com/alerts?a=1&b=<2>",
			"A&B<C>",
		); got != "<https://example.com/alerts?a=1&amp;b=&lt;2&gt;|A&amp;B&lt;C&gt;>" {
			t.Fatalf("slackLink = %q", got)
		}
		payload := string(EmailPayload("alerts@example.com", []string{"ops@example.com"}, Notification{
			Project: "alpha",
			Rule:    "DiskFull",
			Status:  "firing",
			URL:     "https://observe.example.com/alerts?a=1&b=2\r\nInjected: bad",
		}))
		if strings.Contains(payload, "\r\nInjected:") {
			t.Fatalf("email URL line did not strip CR/LF: %q", payload)
		}
		if !strings.Contains(
			payload,
			"View in lazy-observe: https://observe.example.com/alerts?a=1&b=2Injected: bad\r\n\r\n",
		) {
			t.Fatalf("email missing sanitized URL line: %q", payload)
		}
	})
}

func TestSenderSendPopulatesLinksForWebhookAndSMTP(t *testing.T) {
	ts := time.Date(2026, 7, 17, 1, 2, 3, 0, time.UTC)

	t.Run("webhook with ExternalURL set vs empty", func(t *testing.T) {
		received := make(chan []byte, 2)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			received <- body
			w.WriteHeader(http.StatusNoContent)
		}))
		defer srv.Close()

		input := Notification{
			Project: "alpha",
			Rule:    "DiskFull",
			Status:  "firing",
			Alerts: []Alert{{
				Status:      "firing",
				Labels:      map[string]string{"alertname": "DiskFull"},
				StartsAt:    ts,
				Fingerprint: "fp-http",
			}},
		}

		// With ExternalURL set.
		withLinks := &Sender{Client: srv.Client(), ExternalURL: "https://observe.example.com", Timeout: time.Second}
		if _, err := withLinks.Send(
			context.Background(),
			Channel{Type: "webhook", Config: map[string]any{"url": srv.URL}},
			input,
		); err != nil {
			t.Fatalf("Send with ExternalURL failed: %v", err)
		}
		rawWith := <-received
		var gotWith Notification
		if err := json.Unmarshal(rawWith, &gotWith); err != nil {
			t.Fatal(err)
		}
		wantURL := "https://observe.example.com/alerts?fingerprint=fp-http&project=alpha"
		if gotWith.URL != wantURL || len(gotWith.Alerts) != 1 || gotWith.Alerts[0].URL != wantURL {
			t.Fatalf("webhook payload with ExternalURL = %+v, want %q", gotWith, wantURL)
		}
		// Input slice must remain untouched.
		if input.URL != "" || input.Alerts[0].URL != "" {
			t.Fatalf("Send mutated caller notification: %+v", input)
		}

		// With ExternalURL empty: no url field serialized.
		noLinks := &Sender{Client: srv.Client(), Timeout: time.Second}
		if _, err := noLinks.Send(
			context.Background(),
			Channel{Type: "webhook", Config: map[string]any{"url": srv.URL}},
			input,
		); err != nil {
			t.Fatalf("Send without ExternalURL failed: %v", err)
		}
		rawWithout := <-received
		if bytes.Contains(rawWithout, []byte(`"url"`)) {
			t.Fatalf("webhook payload without ExternalURL must omit url: %s", rawWithout)
		}
	})

	t.Run("smtp includes console link first line", func(t *testing.T) {
		mockSMTP, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer mockSMTP.Close()
		receivedData := make(chan string, 1)
		go func() {
			conn, acceptErr := mockSMTP.Accept()
			if acceptErr != nil {
				return
			}
			defer conn.Close()
			reader := bufio.NewReader(conn)
			_, _ = io.WriteString(conn, "220 mock.smtp ESMTP\r\n")
			var inData bool
			var dataBuf strings.Builder
			for {
				line, readErr := reader.ReadString('\n')
				if readErr != nil {
					return
				}
				if inData {
					if line == ".\r\n" {
						receivedData <- dataBuf.String()
						_, _ = io.WriteString(conn, "250 OK\r\n")
						return
					}
					dataBuf.WriteString(line)
					continue
				}
				upper := strings.ToUpper(line)
				switch {
				case strings.HasPrefix(upper, "EHLO"), strings.HasPrefix(upper, "HELO"),
					strings.HasPrefix(upper, "MAIL FROM:"), strings.HasPrefix(upper, "RCPT TO:"):
					_, _ = io.WriteString(conn, "250 OK\r\n")
				case strings.HasPrefix(upper, "DATA"):
					inData = true
					_, _ = io.WriteString(conn, "354 End data\r\n")
				default:
					_, _ = io.WriteString(conn, "250 OK\r\n")
				}
			}
		}()
		smtpHost, smtpPort, _ := net.SplitHostPort(mockSMTP.Addr().String())
		sender := &Sender{
			Outbound:    outbound.Policy{AllowPrivate: true},
			ExternalURL: "https://observe.example.com",
			Timeout:     2 * time.Second,
		}
		if _, err := sender.Send(context.Background(), Channel{
			Type: "smtp",
			Config: map[string]any{
				"host": smtpHost, "port": smtpPort, "from": "alerts@example.com", "to": "ops@example.com",
			},
		}, Notification{
			Project: "alpha",
			Rule:    "DiskFull",
			Status:  "firing",
			Alerts: []Alert{{
				Status:      "firing",
				Fingerprint: "fp-smtp",
				StartsAt:    ts,
			}},
		}); err != nil {
			t.Fatalf("smtp send failed: %v", err)
		}
		select {
		case raw := <-receivedData:
			wantLine := "View in lazy-observe: https://observe.example.com/alerts?fingerprint=fp-smtp&project=alpha\r\n\r\n"
			if !strings.Contains(raw, wantLine) {
				t.Fatalf("smtp DATA missing %q: %q", wantLine, raw)
			}
		case <-time.After(time.Second):
			t.Fatal("mock SMTP server did not receive DATA")
		}
	})
}
