package alert

import (
	"bufio"
	"bytes"
	"context"
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
