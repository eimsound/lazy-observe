package alert

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/eimsound/lazy-observe/internal/backoff"
)

type Sender struct {
	Client *http.Client
	Sleep  func(time.Duration)
}

func defaultHTTPClient() *http.Client { return &http.Client{Timeout: 10 * time.Second} }

func (s *Sender) Send(ctx context.Context, ch Channel, n Notification) (int, error) {
	if s.Client == nil {
		s.Client = defaultHTTPClient()
	}
	if s.Sleep == nil {
		s.Sleep = time.Sleep
	}
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		err = s.sendOnce(ctx, ch, n)
		if err == nil {
			return attempt, nil
		}
		if attempt < 3 {
			delay, delayErr := backoff.Delay(attempt-1, time.Second, 2*time.Second, 1, 0, 0)
			if delayErr != nil {
				return attempt, errors.Join(err, delayErr)
			}
			s.Sleep(delay)
		}
	}
	return 3, err
}

func (s *Sender) sendOnce(ctx context.Context, ch Channel, n Notification) error {
	if ch.Type == "smtp" {
		return sendSMTP(ch.Config, n)
	}
	payload, err := ChannelPayload(ch.Type, n)
	if err != nil {
		return err
	}
	endpoint, _ := ch.Config["url"].(string)
	if endpoint == "" {
		return errors.New("channel url is required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if headers, ok := ch.Config["headers"].(map[string]any); ok {
		for k, v := range headers {
			req.Header.Set(k, fmt.Sprint(v))
		}
	}
	if n.IdempotencyKey != "" {
		req.Header.Set("Idempotency-Key", n.IdempotencyKey)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notification endpoint status %d", resp.StatusCode)
	}
	return nil
}

func ChannelPayload(kind string, n Notification) ([]byte, error) {
	title := fmt.Sprintf("[%s] %s / %s", strings.ToUpper(n.Status), n.Project, n.Rule)
	lines := []string{title}
	if n.Storm != nil {
		lines = append(lines,
			fmt.Sprintf("Active alert groups: %d", n.Storm.GroupCount),
			"Top severities: "+formatCounts(n.Storm.Severities),
			"Top rules: "+formatCounts(n.Storm.Rules),
		)
	}
	for _, a := range n.Alerts {
		lines = append(lines, formatLabels(a.Labels))
		for _, field := range []struct{ key, label string }{
			{"ai_summary", "AI analysis"},
			{"ai_probable_causes", "AI probable causes"},
			{"ai_suggested_actions", "AI suggested actions"},
		} {
			if value := strings.TrimSpace(a.Annotations[field.key]); value != "" {
				lines = append(lines, field.label+": "+value)
			}
		}
	}
	text := strings.Join(lines, "\n")
	var payload any
	switch kind {
	case "webhook":
		payload = n
	case "feishu":
		payload = map[string]any{
			"msg_type": "interactive",
			"card": map[string]any{
				"header": map[string]any{"title": map[string]string{"tag": "plain_text", "content": title}},
				"elements": []any{
					map[string]any{"tag": "div", "text": map[string]string{"tag": "lark_md", "content": text}},
				},
			},
		}
	case "dingtalk":
		payload = map[string]any{"msgtype": "markdown", "markdown": map[string]string{"title": title, "text": text}}
	case "wecom":
		payload = map[string]any{"msgtype": "markdown", "markdown": map[string]string{"content": text}}
	case "slack":
		payload = map[string]any{"text": title, "blocks": []any{
			map[string]any{
				"type": "section",
				"text": map[string]string{"type": "mrkdwn", "text": "*" + title + "*\n" + text},
			},
		}}
	default:
		return nil, fmt.Errorf("unsupported channel type %q", kind)
	}
	return json.Marshal(payload)
}

func formatCounts(counts []Count) string {
	parts := make([]string, 0, len(counts))
	for _, item := range counts {
		parts = append(parts, fmt.Sprintf("%s=%d", item.Name, item.Count))
	}
	return strings.Join(parts, ", ")
}

func formatLabels(labels map[string]string) string {
	data, _ := json.Marshal(labels)
	return string(data)
}

func sendSMTP(cfg map[string]any, n Notification) error {
	host, _ := cfg["host"].(string)
	from, _ := cfg["from"].(string)
	to := stringList(cfg["to"])
	if host == "" || from == "" || len(to) == 0 {
		return errors.New("smtp host, from and to are required")
	}
	port := intValue(cfg["port"], 587)
	addr := host + ":" + strconv.Itoa(port)
	user, _ := cfg["username"].(string)
	password, _ := cfg["password"].(string)
	msg := EmailPayload(from, to, n)
	var auth smtp.Auth
	if user != "" {
		auth = smtp.PlainAuth("", user, password, host)
	}
	if implicit, _ := cfg["tls"].(bool); implicit {
		conn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err != nil {
			return err
		}
		client, err := smtp.NewClient(conn, host)
		if err != nil {
			return err
		}
		defer client.Close()
		if auth != nil {
			if err := client.Auth(auth); err != nil {
				return err
			}
		}
		if err := client.Mail(from); err != nil {
			return err
		}
		for _, recipient := range to {
			if err := client.Rcpt(recipient); err != nil {
				return err
			}
		}
		w, err := client.Data()
		if err != nil {
			return err
		}
		if _, err := w.Write(msg); err != nil {
			return err
		}
		return w.Close()
	}
	return smtp.SendMail(addr, auth, from, to, msg)
}

func EmailPayload(from string, to []string, n Notification) []byte {
	title := fmt.Sprintf("[%s] %s / %s", strings.ToUpper(n.Status), n.Project, n.Rule)
	body, _ := json.MarshalIndent(n, "", "  ")
	return []byte(
		"From: " + from + "\r\nTo: " + strings.Join(
			to,
			",",
		) + "\r\nSubject: " + title + "\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n" + string(
			body,
		),
	)
}

func stringList(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, v := range x {
			out = append(out, fmt.Sprint(v))
		}
		return out
	case []string:
		return x
	case string:
		if x == "" {
			return nil
		}
		return strings.Split(x, ",")
	default:
		return nil
	}
}
func intValue(v any, fallback int) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		if n, e := strconv.Atoi(x); e == nil {
			return n
		}
	}
	return fallback
}
