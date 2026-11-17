package alert

import "time"

func cloneStrings(values map[string]string) map[string]string {
	cloned := make(map[string]string, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

type Alert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    time.Time         `json:"startsAt"`
	EndsAt      time.Time         `json:"endsAt,omitempty"`
	Fingerprint string            `json:"fingerprint,omitempty"`
	Transient   bool              `json:"-"`
}

type Notification struct {
	Project        string        `json:"project"`
	Rule           string        `json:"rule"`
	Status         string        `json:"status"`
	Alerts         []Alert       `json:"alerts"`
	Storm          *StormSummary `json:"storm,omitempty"`
	IdempotencyKey string        `json:"-"`
}

type Count struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type StormSummary struct {
	GroupCount int     `json:"group_count"`
	Severities []Count `json:"severities"`
	Rules      []Count `json:"rules"`
}

type Matcher struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Op    string `json:"op"`
}

type InhibitionRule struct {
	SourceMatchers []Matcher
	TargetMatchers []Matcher
	EqualLabels    []string
}

type Channel struct {
	ID      int            `json:"id"`
	Type    string         `json:"type"`
	Name    string         `json:"name"`
	Config  map[string]any `json:"config"`
	Enabled bool           `json:"enabled"`
}
