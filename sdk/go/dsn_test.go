package lazyobserve

import (
	"strings"
	"testing"
)

func TestParseDSN(t *testing.T) {
	t.Parallel()
	got, err := parseDSN("https://observe.example:3100/p/myapp@lo_secret")
	if err != nil {
		t.Fatal(err)
	}
	if got.endpoint != "https://observe.example:3100" || got.project != "myapp" || got.token != "lo_secret" {
		t.Fatalf("parseDSN() = %#v", got)
	}
}

func TestParseDSNErrors(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{
		"", "ftp://host/p/app@token", "http:///p/app@token", "http://host/app@token",
		"http://host/p/@token", "http://host/p/app@", "http://host/p/app@token/extra",
		"http://host/p/app@token?q=1", "http://user@host/p/app@token",
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := parseDSN(raw); err == nil {
				t.Fatalf("parseDSN(%q) unexpectedly succeeded", raw)
			}
		})
	}
}

func TestParseDSNErrorsDoNotLeakToken(t *testing.T) {
	t.Parallel()
	const secret = "lo_super_secret_token_987654321"
	cases := []struct {
		name string
		raw  string
	}{
		{"invalid port", "http://host:badport/p/app@" + secret},
		{"control char", "http://host/p/app@" + secret + "\x7f"},
		{"missing scheme", "://host/p/app@" + secret},
		{"invalid percent escape", "http://host/p/app@%ZZ" + secret},
		{"query string", "http://host/p/app@" + secret + "?debug=1"},
		{"unsupported scheme", "ftp://host/p/app@" + secret},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseDSN(tc.raw); err == nil {
				t.Fatalf("parseDSN(%q) unexpectedly succeeded", tc.name)
			} else if strings.Contains(err.Error(), secret) {
				t.Fatalf("parseDSN error leaked token: %v", err)
			}
			disabled := false
			lo := Init(Config{DSN: tc.raw, TeeToStderr: &disabled})
			if lo.Err() == nil {
				t.Fatalf("Init(%q) unexpectedly succeeded", tc.name)
			}
			if strings.Contains(lo.Err().Error(), secret) {
				t.Fatalf("lo.Err() leaked token: %v", lo.Err())
			}
		})
	}
}
