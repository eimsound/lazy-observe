package errortrack

import "time"

type Frame struct {
	Function     string `json:"function"`
	Filename     string `json:"filename"`
	Lineno       *int   `json:"lineno,omitempty"`
	Colno        *int   `json:"colno,omitempty"`
	AbsPath      string `json:"abs_path,omitempty"`
	Module       string `json:"module,omitempty"`
	InApp        bool   `json:"in_app"`
	Symbolicated bool   `json:"symbolicated,omitempty"`
}

type Stacktrace struct {
	Frames []Frame `json:"frames"`
}

type Exception struct {
	Type       string     `json:"type"`
	Value      string     `json:"value"`
	Stacktrace Stacktrace `json:"stacktrace"`
}

type TraceContext struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
}

type Contexts struct {
	Trace *TraceContext `json:"trace,omitempty"`
}

type Event struct {
	EventID     string            `json:"event_id"`
	Timestamp   time.Time         `json:"timestamp"`
	Platform    string            `json:"platform"`
	Release     string            `json:"release,omitempty"`
	Environment string            `json:"environment,omitempty"`
	Level       string            `json:"level"`
	Message     string            `json:"message,omitempty"`
	Exception   Exception         `json:"exception"`
	Tags        map[string]string `json:"tags,omitempty"`
	Contexts    *Contexts         `json:"contexts,omitempty"`
	SessionID   string            `json:"session_id,omitempty"`
	URL         string            `json:"url,omitempty"`
}

func (e Event) Title() string {
	title := e.Exception.Type
	if e.Exception.Value != "" {
		if title != "" {
			title += ": "
		}
		title += e.Exception.Value
	}
	if title == "" {
		title = e.Message
	}
	const max = 200
	runes := []rune(title)
	if len(runes) > max {
		title = string(runes[:max])
	}
	return title
}
