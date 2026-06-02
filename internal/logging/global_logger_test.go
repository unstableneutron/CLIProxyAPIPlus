package logging

import (
	"strings"
	"testing"
	"time"

	log "github.com/sirupsen/logrus"
)

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

	formatted, err := (&LogFormatter{}).Format(entry)
	if err != nil {
		t.Fatalf("format: %v", err)
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
