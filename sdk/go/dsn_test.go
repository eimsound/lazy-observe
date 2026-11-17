package lazyobserve

import "testing"

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
