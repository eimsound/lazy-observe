package alert

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/eimsound/lazy-observe/internal/outbound"
)

const (
	defaultSendTimeout     = 10 * time.Second
	maxNotifyResponseBytes = 64 << 10
)

type deliveryError struct {
	msg   string
	cause error
}

func (e *deliveryError) Error() string { return e.msg }
func (e *deliveryError) Unwrap() error { return e.cause }

func sanitizedDeliveryError(err error) error {
	if err == nil {
		return nil
	}
	return &deliveryError{msg: outbound.SanitizeError(err), cause: err}
}

type Sender struct {
	Client      *http.Client
	Outbound    outbound.Policy
	ExternalURL string
	Timeout     time.Duration
	DialContext func(ctx context.Context, network, addr string) (net.Conn, error)
	Sleep       func(time.Duration)

	clientOnce    sync.Once
	defaultClient *http.Client
}

func (s *Sender) timeout() time.Duration {
	if s != nil && s.Timeout > 0 {
		return s.Timeout
	}
	return defaultSendTimeout
}

func (s *Sender) httpClient() *http.Client {
	if s.Client != nil {
		clientCopy := *s.Client
		if clientCopy.Timeout <= 0 {
			clientCopy.Timeout = s.timeout()
		}
		if clientCopy.CheckRedirect == nil {
			clientCopy.CheckRedirect = outbound.RejectRedirects
		}
		return &clientCopy
	}
	s.clientOnce.Do(func() {
		s.defaultClient = outbound.NewHTTPClient(s.timeout(), s.Outbound)
	})
	return s.defaultClient
}

func (s *Sender) dialContext() func(context.Context, string, string) (net.Conn, error) {
	if s.DialContext != nil {
		return s.DialContext
	}
	return outbound.NewDialer(s.Outbound).DialContext
}

// Send performs a single bounded delivery attempt. Retries and exponential
// backoff are owned solely by OutboxWorker so retries never nest.
func (s *Sender) Send(ctx context.Context, ch Channel, n Notification) (int, error) {
	return 1, s.sendOnce(ctx, ch, n)
}

func (s *Sender) sendOnce(ctx context.Context, ch Channel, n Notification) error {
	n = NotificationLinks(s.ExternalURL, n)
	if ch.Type == "smtp" {
		return s.sendSMTP(ctx, ch.Config, n)
	}
	payload, err := ChannelPayload(ch.Type, n)
	if err != nil {
		return err
	}
	endpoint, _ := ch.Config["url"].(string)
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return errors.New("channel url is required")
	}
	if s.Client == nil {
		if _, err := outbound.ValidateURL(endpoint, s.Outbound); err != nil {
			return sanitizedDeliveryError(err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return sanitizedDeliveryError(err)
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
	resp, err := s.httpClient().Do(req)
	if err != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return sanitizedDeliveryError(err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxNotifyResponseBytes))
		_ = resp.Body.Close()
	}()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxNotifyResponseBytes))
	if err != nil {
		return sanitizedDeliveryError(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("notification endpoint status %d", resp.StatusCode)
	}
	return checkChannelResponse(ch.Type, body)
}

func checkChannelResponse(kind string, body []byte) error {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil
	}
	switch kind {
	case "feishu":
		var resp struct {
			Code       *int `json:"code"`
			StatusCode *int `json:"StatusCode"`
		}
		if err := json.Unmarshal(trimmed, &resp); err != nil {
			return errors.New("feishu notification returned invalid JSON response")
		}
		if resp.Code != nil && *resp.Code != 0 {
			return fmt.Errorf("feishu notification rejected (code %d)", *resp.Code)
		}
		if resp.StatusCode != nil && *resp.StatusCode != 0 {
			return fmt.Errorf("feishu notification rejected (code %d)", *resp.StatusCode)
		}
	case "dingtalk", "wecom":
		var resp struct {
			ErrCode *int `json:"errcode"`
		}
		if err := json.Unmarshal(trimmed, &resp); err != nil {
			return fmt.Errorf("%s notification returned invalid JSON response", kind)
		}
		if resp.ErrCode != nil && *resp.ErrCode != 0 {
			return fmt.Errorf("%s notification rejected (errcode %d)", kind, *resp.ErrCode)
		}
	}
	return nil
}

const (
	consoleLinkCTA   = "View in lazy-observe"
	perAlertLinkText = "View"
)

var (
	mdURLReplacer = strings.NewReplacer("(", "%28", ")", "%29", " ", "%20")
	slackReplacer = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
)

func mdURL(raw string) string {
	return mdURLReplacer.Replace(raw)
}

func slackEscape(s string) string {
	return slackReplacer.Replace(s)
}

func slackLink(rawURL, text string) string {
	return "<" + slackEscape(rawURL) + "|" + slackEscape(text) + ">"
}

func buildNotificationText(
	title string,
	n Notification,
	formatAlertLink func(string) string,
	doubleNewlineBeforeAlertLink bool,
) string {
	lines := []string{title}
	if n.Storm != nil {
		lines = append(lines,
			fmt.Sprintf("Active alert groups: %d", n.Storm.GroupCount),
			"Top severities: "+formatCounts(n.Storm.Severities),
			"Top rules: "+formatCounts(n.Storm.Rules),
		)
	}
	showPerAlert := len(n.Alerts) > 1
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
		if showPerAlert && a.URL != "" && formatAlertLink != nil {
			link := formatAlertLink(a.URL)
			if doubleNewlineBeforeAlertLink {
				lines = append(lines, "\n"+link)
			} else {
				lines = append(lines, link)
			}
		}
	}
	return strings.Join(lines, "\n")
}

func buildMarkdownNotificationText(title string, n Notification) string {
	text := buildNotificationText(
		title,
		n,
		func(u string) string { return "[" + perAlertLinkText + "](" + mdURL(u) + ")" },
		true,
	)
	if n.URL != "" {
		text += "\n\n[" + consoleLinkCTA + "](" + mdURL(n.URL) + ")"
	}
	return text
}

func ChannelPayload(kind string, n Notification) ([]byte, error) {
	title := fmt.Sprintf("[%s] %s / %s", strings.ToUpper(n.Status), n.Project, n.Rule)
	var payload any
	switch kind {
	case "webhook":
		payload = n
	case "feishu":
		text := buildNotificationText(
			title,
			n,
			func(u string) string { return "[" + perAlertLinkText + "](" + mdURL(u) + ")" },
			false,
		)
		elements := []any{
			map[string]any{"tag": "div", "text": map[string]string{"tag": "lark_md", "content": text}},
		}
		if n.URL != "" {
			elements = append(elements, map[string]any{
				"tag": "action",
				"actions": []any{
					map[string]any{
						"tag":  "button",
						"text": map[string]string{"tag": "plain_text", "content": consoleLinkCTA},
						"type": "primary",
						"url":  n.URL,
					},
				},
			})
		}
		payload = map[string]any{
			"msg_type": "interactive",
			"card": map[string]any{
				"header":   map[string]any{"title": map[string]string{"tag": "plain_text", "content": title}},
				"elements": elements,
			},
		}
	case "dingtalk":
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]string{"title": title, "text": buildMarkdownNotificationText(title, n)},
		}
	case "wecom":
		payload = map[string]any{
			"msgtype":  "markdown",
			"markdown": map[string]string{"content": buildMarkdownNotificationText(title, n)},
		}
	case "slack":
		text := buildNotificationText(
			title,
			n,
			func(u string) string { return slackLink(u, perAlertLinkText) },
			false,
		)
		if n.URL != "" {
			text += "\n\n" + slackLink(n.URL, consoleLinkCTA)
		}
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

func (s *Sender) sendSMTP(ctx context.Context, cfg map[string]any, n Notification) error {
	host, _ := cfg["host"].(string)
	host = strings.TrimSpace(host)
	from, _ := cfg["from"].(string)
	from = strings.TrimSpace(from)
	rawTo := stringList(cfg["to"])
	to := make([]string, 0, len(rawTo))
	for _, recipient := range rawTo {
		if trimmed := strings.TrimSpace(recipient); trimmed != "" {
			to = append(to, trimmed)
		}
	}
	if host == "" || from == "" || len(to) == 0 {
		return errors.New("smtp host, from and to are required")
	}
	if hasHeaderInjection(from) {
		return errors.New("smtp from contains invalid control characters")
	}
	for _, recipient := range to {
		if hasHeaderInjection(recipient) {
			return errors.New("smtp recipient contains invalid control characters")
		}
	}
	port, err := parseSMTPPort(cfg["port"], 587)
	if err != nil {
		return err
	}
	if s.DialContext == nil {
		if err := outbound.ValidateHostPort(host, port, s.Outbound); err != nil {
			return sanitizedDeliveryError(err)
		}
	}

	sendCtx, cancel := context.WithTimeout(ctx, s.timeout())
	defer cancel()

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := s.dialContext()(sendCtx, "tcp", addr)
	if err != nil {
		return sanitizedDeliveryError(err)
	}
	if deadline, ok := sendCtx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-sendCtx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-done:
		}
	}()

	implicit, _ := cfg["tls"].(bool)
	if implicit {
		tlsConn := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if err := tlsConn.HandshakeContext(sendCtx); err != nil {
			_ = conn.Close()
			return sanitizedDeliveryError(err)
		}
		conn = tlsConn
	}

	client, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return sanitizedSMTPError(sendCtx, err, "smtp handshake failed")
	}
	defer client.Close()

	if !implicit {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return sanitizedSMTPError(sendCtx, err, "smtp STARTTLS failed")
			}
		}
	}

	user, _ := cfg["username"].(string)
	password, _ := cfg["password"].(string)
	if user != "" {
		auth := smtp.PlainAuth("", user, password, host)
		if err := client.Auth(auth); err != nil {
			return sanitizedSMTPError(sendCtx, err, "smtp authentication failed")
		}
	}
	if err := client.Mail(from); err != nil {
		return sanitizedSMTPError(sendCtx, err, "smtp sender rejected")
	}
	for _, recipient := range to {
		if err := client.Rcpt(recipient); err != nil {
			return sanitizedSMTPError(sendCtx, err, "smtp recipient rejected")
		}
	}
	w, err := client.Data()
	if err != nil {
		return sanitizedSMTPError(sendCtx, err, "smtp data rejected")
	}
	if _, err := w.Write(EmailPayload(from, to, n)); err != nil {
		_ = w.Close()
		return sanitizedSMTPError(sendCtx, err, "smtp write failed")
	}
	if err := w.Close(); err != nil {
		return sanitizedSMTPError(sendCtx, err, "smtp commit failed")
	}
	_ = client.Quit()
	return nil
}

func sanitizedSMTPError(ctx context.Context, err error, fallback string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return &deliveryError{msg: "smtp delivery timed out", cause: context.DeadlineExceeded}
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
		return &deliveryError{msg: "smtp delivery canceled", cause: context.Canceled}
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &deliveryError{msg: "smtp delivery timed out", cause: err}
	}
	return &deliveryError{msg: fallback, cause: err}
}

func EmailPayload(from string, to []string, n Notification) []byte {
	safeFrom := sanitizeEmailHeader(from)
	safeTo := make([]string, 0, len(to))
	for _, recipient := range to {
		safeTo = append(safeTo, sanitizeEmailHeader(recipient))
	}
	title := sanitizeEmailHeader(fmt.Sprintf("[%s] %s / %s", strings.ToUpper(n.Status), n.Project, n.Rule))
	var jsonBuf bytes.Buffer
	enc := json.NewEncoder(&jsonBuf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(n)
	body := string(bytes.TrimSuffix(jsonBuf.Bytes(), []byte("\n")))
	if safeURL := sanitizeEmailHeader(n.URL); safeURL != "" {
		body = consoleLinkCTA + ": " + safeURL + "\r\n\r\n" + body
	}
	return []byte(
		"From: " + safeFrom + "\r\nTo: " + strings.Join(
			safeTo,
			",",
		) + "\r\nSubject: " + title + "\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n" + body,
	)
}

func hasHeaderInjection(value string) bool {
	return strings.ContainsAny(value, "\r\n\x00")
}

func sanitizeEmailHeader(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\r' || r == '\n' || r == 0 {
			return -1
		}
		return r
	}, value)
}

func parseSMTPPort(v any, fallback int) (int, error) {
	if v == nil {
		return fallback, nil
	}
	switch x := v.(type) {
	case string:
		trimmed := strings.TrimSpace(x)
		if trimmed == "" {
			return fallback, nil
		}
		port, _, err := outbound.NormalizePort(trimmed)
		if err != nil {
			return 0, fmt.Errorf("smtp port is invalid: %w", err)
		}
		return port, nil
	case int:
		if x < 1 || x > 65535 {
			return 0, fmt.Errorf("smtp port out of range: %w", outbound.ErrInvalidTarget)
		}
		return x, nil
	case float64:
		if math.Trunc(x) != x || x < 1 || x > 65535 {
			return 0, fmt.Errorf("smtp port out of range: %w", outbound.ErrInvalidTarget)
		}
		return int(x), nil
	default:
		return 0, fmt.Errorf("smtp port is invalid: %w", outbound.ErrInvalidTarget)
	}
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
