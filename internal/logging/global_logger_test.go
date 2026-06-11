package logging

import (
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

func TestLogFormatterPrintsVersionField(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Date(2026, 6, 9, 11, 10, 2, 0, time.Local)
	entry.Level = log.InfoLevel
	entry.Message = "fetched latest antigravity version"
	entry.Data["version"] = "2.1.0"

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	if !strings.Contains(line, "version=2.1.0") {
		t.Fatalf("formatted line %q missing version field", line)
	}
}

func TestLogFormatterIncludesWebsocketTraceFields(t *testing.T) {
	entry := log.NewEntry(log.New())
	entry.Time = time.Unix(0, 0).UTC()
	entry.Level = log.InfoLevel
	entry.Message = "responses websocket: client disconnected"
	entry.Data = log.Fields{
		"trace_id":                   "1234567890abcdef1234567890abcdef",
		"span_id":                    "abcdef1234567890",
		"proxy_websocket_session_id": "proxy-session-1",
	}

	formatted, errFormat := (&LogFormatter{}).Format(entry)
	if errFormat != nil {
		t.Fatalf("Format() error = %v", errFormat)
	}

	line := string(formatted)
	for _, want := range []string{
		"trace_id=1234567890abcdef1234567890abcdef",
		"span_id=abcdef1234567890",
		"proxy_websocket_session_id=proxy-session-1",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("formatted log missing %q: %s", want, line)
		}
	}
}
