package logging

import (
	"encoding/json"
	"time"
)

// Stream identifies which log stream an entry belongs to.
type Stream string

const (
	StreamDaemon  Stream = "daemon"
	StreamAccess  Stream = "access"
	StreamKubectl Stream = "kubectl"
)

// Entry is a single log record — both for in-memory ring buffers and for the
// JSON file format. Fields is a grab bag of extra structured fields.
type Entry struct {
	Time    time.Time      `json:"time"`
	Level   string         `json:"level,omitempty"`
	Stream  Stream         `json:"stream"`
	Tunnel  string         `json:"tunnel,omitempty"`
	Event   string         `json:"event,omitempty"`
	Msg     string         `json:"msg,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
	RawLine string         `json:"-"`
}

// MarshalJSON flattens Fields into the top level for easier grepping/jq usage
// while keeping a stable schema.
func (e Entry) MarshalJSON() ([]byte, error) {
	m := map[string]any{
		"time":   e.Time.UTC().Format(time.RFC3339Nano),
		"stream": e.Stream,
	}
	if e.Level != "" {
		m["level"] = e.Level
	}
	if e.Tunnel != "" {
		m["tunnel"] = e.Tunnel
	}
	if e.Event != "" {
		m["event"] = e.Event
	}
	if e.Msg != "" {
		m["msg"] = e.Msg
	}
	for k, v := range e.Fields {
		if _, exists := m[k]; !exists {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

// Field returns a field value by key (checks top-level first, then Fields).
func (e Entry) Field(key string) (any, bool) {
	switch key {
	case "time":
		return e.Time, true
	case "level":
		return e.Level, e.Level != ""
	case "stream":
		return string(e.Stream), true
	case "tunnel":
		return e.Tunnel, e.Tunnel != ""
	case "event":
		return e.Event, e.Event != ""
	case "msg":
		return e.Msg, e.Msg != ""
	}
	if v, ok := e.Fields[key]; ok {
		return v, true
	}
	return nil, false
}
