package outbound

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

type staticResolver struct {
	addrs []netip.Addr
	err   error
	calls int
}

func (r *staticResolver) LookupNetIP(_ context.Context, _, _ string) ([]netip.Addr, error) {
	r.calls++
	if r.err != nil {
		return nil, r.err
	}
	return r.addrs, nil
}

func TestIsBlockedIPAndCheckIP(t *testing.T) {
	blocked := []string{
		"0.0.0.0",
		"0.1.2.3",
		"127.0.0.1",
		"127.255.255.254",
		"10.0.0.1",
		"172.16.0.1",
		"192.168.1.1",
		"169.254.169.254",
		"100.64.0.1",
		"100.100.100.200",
		"198.18.0.1",
		"224.0.0.1",
		"255.255.255.255",
		"::",
		"::1",
		"fc00::1",
		"fe80::1",
		"ff02::1",
		"::ffff:127.0.0.1",
		"::ffff:169.254.169.254",
		"::ffff:10.0.0.5",
	}
	var strict Policy
	for _, raw := range blocked {
		ip := netip.MustParseAddr(raw)
		if !IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%s) = false, want true", raw)
		}
		if err := strict.CheckIP(ip, raw, 80); !errors.Is(err, ErrBlockedAddress) {
			t.Errorf("strict.CheckIP(%s) = %v, want ErrBlockedAddress", raw, err)
		}
	}

	allowed := []string{"1.1.1.1", "8.8.8.8", "93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"}
	for _, raw := range allowed {
		ip := netip.MustParseAddr(raw)
		if IsBlockedIP(ip) {
			t.Errorf("IsBlockedIP(%s) = true, want false", raw)
		}
		if err := strict.CheckIP(ip, raw, 443); err != nil {
			t.Errorf("strict.CheckIP(%s) = %v, want nil", raw, err)
		}
	}
}

func TestAllowlistAndBlockedPlatformPorts(t *testing.T) {
	policy, err := ParseAllowlist([]string{"127.0.0.1:11434", "10.0.0.0/8", "ollama.internal:11434"})
	if err != nil {
		t.Fatal(err)
	}
	policy.BlockedPorts = map[int]struct{}{18428: {}, 3100: {}}

	if err := policy.CheckIP(netip.MustParseAddr("127.0.0.1"), "127.0.0.1", 11434); err != nil {
		t.Fatalf("allowlisted 127.0.0.1:11434 rejected: %v", err)
	}
	if err := policy.CheckIP(netip.MustParseAddr("127.0.0.1"), "127.0.0.1", 18428); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("loopback platform port 18428 should be blocked: %v", err)
	}
	if err := policy.CheckIP(netip.MustParseAddr("127.0.0.1"), "127.0.0.1", 8080); !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("non-allowlisted loopback port 8080 should be blocked: %v", err)
	}
	if err := policy.CheckIP(netip.MustParseAddr("10.1.2.3"), "internal.svc", 443); err != nil {
		t.Fatalf("allowlisted CIDR 10.1.2.3 rejected: %v", err)
	}
	if err := policy.CheckIP(netip.MustParseAddr("172.16.0.1"), "ollama.internal", 11434); err != nil {
		t.Fatalf("allowlisted host ollama.internal:11434 rejected: %v", err)
	}
	// Hostname allowlist must never permit cloud metadata IP.
	if err := policy.CheckIP(
		netip.MustParseAddr("169.254.169.254"),
		"ollama.internal",
		11434,
	); !errors.Is(
		err,
		ErrBlockedAddress,
	) {
		t.Fatalf("hostname allowlist must not allow metadata IP: %v", err)
	}
	// Even with loopback CIDR allowlisted, blocked platform ports remain denied.
	loopbackCIDR, err := ParseAllowlist([]string{"127.0.0.1/32"})
	if err != nil {
		t.Fatal(err)
	}
	loopbackCIDR.BlockedPorts = map[int]struct{}{18428: {}}
	if err := loopbackCIDR.CheckIP(
		netip.MustParseAddr("127.0.0.1"),
		"127.0.0.1",
		18428,
	); !errors.Is(
		err,
		ErrBlockedAddress,
	) {
		t.Fatalf("blocked platform port must override loopback CIDR: %v", err)
	}
}

func TestNormalizePortAndValidateURL(t *testing.T) {
	badURLs := []string{
		"",
		"ftp://example.com/hook",
		"file:///etc/passwd",
		"http://example.com:080/hook",
		"http://example.com:00443/hook",
		"http://example.com:0/hook",
		"http://example.com:70000/hook",
		"http://example.com:/hook",
		"http://127.0.0.1:8080/hook",
		"http://localhost:8080/hook",
		"http://169.254.169.254/latest/meta-data/",
		"http://[::ffff:127.0.0.1]:8080/hook",
		"http://10.0.0.1/hook",
	}
	for _, raw := range badURLs {
		if _, err := ValidateURL(raw, Policy{}); err == nil {
			t.Errorf("ValidateURL(%q) succeeded, want error", raw)
		}
	}

	if _, err := ValidateURL("https://hooks.example.com:443/notify", Policy{}); err != nil {
		t.Fatalf("valid public URL rejected: %v", err)
	}
}

func TestSafeDialerBlocksDNSRebindingAndDualRecords(t *testing.T) {
	// Host resolves to both a public IP and loopback -> must be rejected.
	resolver := &staticResolver{
		addrs: []netip.Addr{
			netip.MustParseAddr("93.184.216.34"),
			netip.MustParseAddr("127.0.0.1"),
		},
	}
	dialer := &SafeDialer{Resolver: resolver}
	if _, err := dialer.DialContext(context.Background(), "tcp", "rebind.example:443"); !errors.Is(
		err,
		ErrBlockedAddress,
	) {
		t.Fatalf("dual-record DNS rebinding err=%v want ErrBlockedAddress", err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls=%d want 1", resolver.calls)
	}

	// Leading-zero port must be rejected before resolution.
	resolver.calls = 0
	if _, err := dialer.DialContext(context.Background(), "tcp", "rebind.example:0443"); !errors.Is(
		err,
		ErrInvalidTarget,
	) {
		t.Fatalf("leading-zero port err=%v want ErrInvalidTarget", err)
	}
	if resolver.calls != 0 {
		t.Fatalf("resolver should not be called on invalid port, calls=%d", resolver.calls)
	}
}

func TestSafeHTTPClientBlocksRedirectsAndSecrets(t *testing.T) {
	var secondHit bool
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL, http.StatusFound)
	}))
	defer first.Close()

	client := NewHTTPClient(2*time.Second, Policy{AllowPrivate: true})
	req, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		first.URL+"?access_token=top-secret",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", "anthropic-secret")
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, ErrRedirectBlocked) {
		t.Fatalf("redirect err=%v want ErrRedirectBlocked", err)
	}
	if secondHit {
		t.Fatal("redirect target was contacted")
	}
	sanitized := SanitizeError(err)
	if strings.Contains(sanitized, "top-secret") || strings.Contains(sanitized, first.URL) {
		t.Fatalf("sanitized error leaked URL/token: %q", sanitized)
	}
}

func TestSanitizeURLAndError(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		noLeaks []string
	}{
		{
			in:      "https://oapi.dingtalk.com/robot/send?access_token=ding-secret-123&sign=sig-xyz",
			want:    "https://oapi.dingtalk.com/robot/send?access_token=REDACTED&sign=REDACTED",
			noLeaks: []string{"ding-secret-123", "sig-xyz"},
		},
		{
			in:      "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=wecom-secret-456",
			want:    "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=REDACTED",
			noLeaks: []string{"wecom-secret-456"},
		},
		{
			in:      "https://open.feishu.cn/open-apis/bot/v2/hook/feishu-uuid-secret",
			want:    "https://open.feishu.cn/open-apis/bot/v2/hook/REDACTED",
			noLeaks: []string{"feishu-uuid-secret"},
		},
		{
			in:      "https://hooks.slack.com/services/T123/B456/slack-secret-789",
			want:    "https://hooks.slack.com/services/REDACTED",
			noLeaks: []string{"T123", "B456", "slack-secret-789"},
		},
		{
			in:      "https://user:pass@example.com/notify?token=abc#frag",
			want:    "https://example.com/notify?token=REDACTED",
			noLeaks: []string{"user", "pass", "abc", "frag"},
		},
		{
			in:   "http://127.0.0.1:12345",
			want: "http://127.0.0.1:12345",
		},
	}
	for _, tc := range cases {
		got := SanitizeURL(tc.in)
		if got != tc.want {
			t.Errorf("SanitizeURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
		for _, secret := range tc.noLeaks {
			if strings.Contains(got, secret) {
				t.Errorf("SanitizeURL(%q) leaked %q in %q", tc.in, secret, got)
			}
		}
	}

	urlErr := &url.Error{
		Op:  "Post",
		URL: "https://oapi.dingtalk.com/robot/send?access_token=ding-secret-999",
		Err: &net.OpError{
			Op:   "dial",
			Net:  "tcp",
			Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.5"), Port: 6379},
			Err:  errors.New("connect: connection refused"),
		},
	}
	gotErr := SanitizeError(urlErr)
	for _, forbidden := range []string{"ding-secret-999", "oapi.dingtalk.com", "10.0.0.5", "6379", "connection refused"} {
		if strings.Contains(gotErr, forbidden) {
			t.Errorf("SanitizeError leaked %q in %q", forbidden, gotErr)
		}
	}
}
