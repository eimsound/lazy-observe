package alert

import (
	"net/url"
	"strconv"
	"strings"
)

// NotificationLinks derives absolute console URLs for n and its alerts using
// base. It is pure and never mutates the caller's Alerts slice. When base is
// empty, n is returned unchanged.
func NotificationLinks(base string, n Notification) Notification {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return n
	}
	if len(n.Alerts) > 0 {
		alerts := make([]Alert, len(n.Alerts))
		copy(alerts, n.Alerts)
		n.Alerts = alerts
	}
	if n.Test {
		n.URL = base + "/alerts/channels"
		for i := range n.Alerts {
			n.Alerts[i].URL = ""
		}
		return n
	}
	if n.Storm != nil {
		params := projectQueryParams(n.Project)
		params.Set("status", "firing")
		n.URL = buildConsoleURL(base, "/alerts", params)
		for i := range n.Alerts {
			n.Alerts[i].URL = ""
		}
		return n
	}

	hasPlatform := false
	for i := range n.Alerts {
		if n.Alerts[i].Labels["source"] == "platform" {
			hasPlatform = true
		}
		n.Alerts[i].URL = deriveAlertLink(base, n.Project, n.Rule, n.Alerts[i])
	}

	switch {
	case len(n.Alerts) == 0:
		n.URL = buildConsoleURL(base, "/alerts", projectQueryParams(n.Project))
	case allAlertURLsEqual(n.Alerts):
		n.URL = n.Alerts[0].URL
	case hasPlatform:
		n.URL = buildConsoleURL(base, "/issues", projectQueryParams(n.Project))
	default:
		params := projectQueryParams(n.Project)
		if n.Rule != "" {
			params.Set("rule", n.Rule)
		}
		n.URL = buildConsoleURL(base, "/alerts", params)
	}
	return n
}

func deriveAlertLink(base, project, rule string, a Alert) string {
	if a.Labels["source"] == "platform" {
		kind := a.Labels["kind"]
		if kind == "" {
			kind = rule
		}
		if kind == "new_issue" || kind == "regression" {
			if id, err := strconv.Atoi(strings.TrimSpace(a.Labels["issue_id"])); err == nil && id > 0 {
				return buildConsoleURL(base, "/issues/"+strconv.Itoa(id), projectQueryParams(project))
			}
		}
		return buildConsoleURL(base, "/issues", projectQueryParams(project))
	}

	params := projectQueryParams(project)
	if a.Fingerprint != "" {
		params.Set("fingerprint", a.Fingerprint)
	} else if rule != "" {
		params.Set("rule", rule)
	}
	return buildConsoleURL(base, "/alerts", params)
}

func allAlertURLsEqual(alerts []Alert) bool {
	if len(alerts) == 0 {
		return false
	}
	first := alerts[0].URL
	for _, a := range alerts[1:] {
		if a.URL != first {
			return false
		}
	}
	return true
}

func projectQueryParams(project string) url.Values {
	params := url.Values{}
	if project != "" {
		params.Set("project", project)
	}
	return params
}

func buildConsoleURL(base, path string, params url.Values) string {
	encoded := params.Encode()
	if encoded == "" {
		return base + path
	}
	return base + path + "?" + encoded
}
