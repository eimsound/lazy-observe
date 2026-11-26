package alert

import (
	"fmt"
	"sort"
	"strings"
)

const (
	// maxTitleSubjectRunes bounds the rendered summary placed in a message
	// title so a runaway template cannot produce a multi-kilobyte IM header.
	maxTitleSubjectRunes = 200
	// maxNotificationAISummaryRunes bounds the single AI summary line carried
	// by Issue notifications across every channel.
	maxNotificationAISummaryRunes = 500
)

// titleLabelKeys are labels already represented by the message title
// (project, rule name, platform kind), so the fallback `k=v` line omits them.
var titleLabelKeys = map[string]struct{}{
	"alertname":  {},
	"project":    {},
	"project_id": {},
	"source":     {},
	"kind":       {},
}

// severityRank orders alert severities (native rules use `severity`, Issue
// alerts use the SDK `level`) for representative selection.
func severityRank(value string) int {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "critical", "fatal":
		return 4
	case "error":
		return 3
	case "warning", "warn":
		return 2
	case "info":
		return 1
	default:
		return 0
	}
}

// alertSeverity returns the severity of a and the label key it came from.
func alertSeverity(a Alert) (string, string) {
	for _, key := range []string{"severity", "level"} {
		if value := strings.TrimSpace(a.Labels[key]); value != "" {
			return strings.ToLower(value), key
		}
	}
	return "", ""
}

// representativeAlert deterministically picks the alert shown in an
// aggregated IM message: highest severity first, then the longest-running
// (earliest StartsAt), then the smallest Fingerprint. It returns false when n
// carries no alerts (storm summaries).
func representativeAlert(alerts []Alert) (Alert, bool) {
	if len(alerts) == 0 {
		return Alert{}, false
	}
	ordered := append([]Alert(nil), alerts...)
	sort.SliceStable(ordered, func(i, j int) bool {
		si, _ := alertSeverity(ordered[i])
		sj, _ := alertSeverity(ordered[j])
		if ri, rj := severityRank(si), severityRank(sj); ri != rj {
			return ri > rj
		}
		if !ordered[i].StartsAt.Equal(ordered[j].StartsAt) {
			return ordered[i].StartsAt.Before(ordered[j].StartsAt)
		}
		return ordered[i].Fingerprint < ordered[j].Fingerprint
	})
	return ordered[0], true
}

// notificationView is the channel-neutral content of one IM message: a single
// title line plus a few concise body lines.
type notificationView struct {
	Title    string
	Lines    []string
	Severity string
}

func buildNotificationView(n Notification) notificationView {
	status := strings.ToUpper(strings.TrimSpace(n.Status))
	if status == "" {
		status = "FIRING"
	}
	rep, hasRep := representativeAlert(n.Alerts)
	severity, severityKey := "", ""
	if hasRep {
		severity, severityKey = alertSeverity(rep)
	}
	tag := "[" + status + "]"
	if n.Status != "resolved" && severity != "" {
		tag += "[" + strings.ToUpper(severity) + "]"
	}
	subject := n.Rule
	summary := ""
	if hasRep {
		summary = singleLine(rep.Annotations["summary"], maxTitleSubjectRunes)
	}
	if summary != "" {
		subject = summary
		// Issue notifications title themselves with the exception text; keep
		// the platform kind visible so a regression is not mistaken for a new
		// issue.
		if rep.Labels["source"] == "platform" && (n.Rule == "new_issue" || n.Rule == "regression") {
			subject = n.Rule + ": " + summary
		}
	}
	title := fmt.Sprintf("%s %s / %s", tag, n.Project, subject)
	if extra := len(n.Alerts) - 1; extra > 0 {
		title += fmt.Sprintf(" (+%d more)", extra)
	}

	view := notificationView{Title: title, Severity: severity}
	if n.Storm != nil {
		view.Lines = append(view.Lines,
			fmt.Sprintf("Active alert groups: %d", n.Storm.GroupCount),
			"Top severities: "+formatCounts(n.Storm.Severities),
			"Top rules: "+formatCounts(n.Storm.Rules),
		)
	}
	if !hasRep {
		return view
	}
	if summary == "" {
		hidden := titleLabelKeys
		if severityKey != "" && n.Status != "resolved" {
			hidden = make(map[string]struct{}, len(titleLabelKeys)+1)
			for key := range titleLabelKeys {
				hidden[key] = struct{}{}
			}
			hidden[severityKey] = struct{}{}
		}
		if line := formatLabelPairs(rep.Labels, hidden); line != "" {
			view.Lines = append(view.Lines, line)
		}
	}
	if ai := singleLine(rep.Annotations["ai_summary"], maxNotificationAISummaryRunes); ai != "" {
		view.Lines = append(view.Lines, "AI analysis: "+ai)
	}
	return view
}

// feishuHeaderTemplate maps the message state to a Feishu card header color.
func feishuHeaderTemplate(n Notification, severity string) string {
	switch {
	case n.Status == "resolved":
		return "green"
	case n.Storm != nil:
		return "red"
	}
	switch severityRank(severity) {
	case 4, 3:
		return "red"
	case 1:
		return "blue"
	default:
		return "orange"
	}
}

func formatLabelPairs(labels map[string]string, hidden map[string]struct{}) string {
	keys := make([]string, 0, len(labels))
	for key := range labels {
		if _, skip := hidden[key]; skip {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+singleLine(labels[key], maxTitleSubjectRunes))
	}
	return strings.Join(parts, " ")
}

// singleLine collapses all whitespace runs (including newlines) to single
// spaces and clips the result to limit runes.
func singleLine(value string, limit int) string {
	return clipNotificationText(strings.Join(strings.Fields(value), " "), limit)
}

func formatCounts(counts []Count) string {
	parts := make([]string, 0, len(counts))
	for _, item := range counts {
		parts = append(parts, fmt.Sprintf("%s=%d", item.Name, item.Count))
	}
	return strings.Join(parts, ", ")
}
