package lazyobserve

import (
	"fmt"
	"net/url"
	"strings"
)

type dsn struct {
	endpoint string
	project  string
	token    string
}

func parseDSN(raw string) (dsn, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return dsn{}, fmt.Errorf("parse DSN: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return dsn{}, fmt.Errorf("parse DSN: scheme must be http or https")
	}
	if u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return dsn{}, fmt.Errorf("parse DSN: expected http(s)://host/p/<slug>@<token>")
	}
	path := strings.TrimPrefix(u.EscapedPath(), "/p/")
	if path == u.EscapedPath() || strings.Contains(path, "/") {
		return dsn{}, fmt.Errorf("parse DSN: path must be /p/<slug>@<token>")
	}
	slugRaw, tokenRaw, ok := strings.Cut(path, "@")
	if !ok || slugRaw == "" || tokenRaw == "" || strings.Contains(tokenRaw, "@") {
		return dsn{}, fmt.Errorf("parse DSN: path must be /p/<slug>@<token>")
	}
	slug, err := url.PathUnescape(slugRaw)
	if err != nil || slug == "" {
		return dsn{}, fmt.Errorf("parse DSN: invalid project slug")
	}
	token, err := url.PathUnescape(tokenRaw)
	if err != nil || strings.TrimSpace(token) == "" {
		return dsn{}, fmt.Errorf("parse DSN: invalid token")
	}
	return dsn{endpoint: u.Scheme + "://" + u.Host, project: slug, token: token}, nil
}
