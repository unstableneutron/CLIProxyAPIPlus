package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type captureRequestEventSink struct {
	lines [][]byte
}

func (s *captureRequestEventSink) Write(_ context.Context, line []byte) error {
	copied := append([]byte(nil), line...)
	s.lines = append(s.lines, copied)
	return nil
}

func (s *captureRequestEventSink) Flush(context.Context) error {
	return nil
}

func (s *captureRequestEventSink) Close(context.Context) error {
	return nil
}

func TestAsyncRequestEventLogger_EmitsJSONLWithOwnedPayload(t *testing.T) {
	t.Parallel()

	sink := &captureRequestEventSink{}
	logger := NewAsyncRequestEventLogger(RequestEventLoggerOptions{
		Enabled:              true,
		QueueSize:            4,
		MaxQueuedPayloadSize: 1 << 20,
		FlushInterval:        time.Hour,
	}, sink)

	payload := []byte("event: response.output_text.delta\ndata: {\"delta\":\"hello\"}\n\n")
	event := logger.AcquireEvent()
	event.RequestID = "req-jsonl"
	event.Event = "stream.frame"
	event.Boundary = "provider_to_proxy"
	event.Direction = "inbound"
	event.Protocol = "sse"
	event.ContentType = "text/event-stream"
	event.SetPayloadBytes(payload)
	logger.Emit(event)
	payload[0] = 'X'

	if err := logger.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if len(sink.lines) != 1 {
		t.Fatalf("sink lines = %d, want 1", len(sink.lines))
	}
	if !strings.HasSuffix(string(sink.lines[0]), "\n") {
		t.Fatalf("line %q does not end with newline", string(sink.lines[0]))
	}

	var got struct {
		SchemaVersion int    `json:"schema_version"`
		RequestID     string `json:"request_id"`
		Event         string `json:"event"`
		Boundary      string `json:"boundary"`
		Direction     string `json:"direction"`
		Protocol      string `json:"protocol"`
		ContentType   string `json:"content_type"`
		Payload       struct {
			Encoding string `json:"encoding"`
			Body     string `json:"body"`
			Bytes    int    `json:"bytes"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(sink.lines[0], &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v line=%s", err, string(sink.lines[0]))
	}
	if got.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", got.SchemaVersion)
	}
	if got.RequestID != "req-jsonl" || got.Event != "stream.frame" {
		t.Fatalf("request/event = %q/%q, want req-jsonl/stream.frame", got.RequestID, got.Event)
	}
	if got.Boundary != "provider_to_proxy" || got.Direction != "inbound" || got.Protocol != "sse" {
		t.Fatalf("boundary/direction/protocol = %q/%q/%q", got.Boundary, got.Direction, got.Protocol)
	}
	if got.ContentType != "text/event-stream" {
		t.Fatalf("content_type = %q, want text/event-stream", got.ContentType)
	}
	if got.Payload.Encoding != "utf8" {
		t.Fatalf("payload.encoding = %q, want utf8", got.Payload.Encoding)
	}
	if got.Payload.Body != "event: response.output_text.delta\ndata: {\"delta\":\"hello\"}\n\n" {
		t.Fatalf("payload.body = %q", got.Payload.Body)
	}
	if got.Payload.Bytes != len("event: response.output_text.delta\ndata: {\"delta\":\"hello\"}\n\n") {
		t.Fatalf("payload.bytes = %d", got.Payload.Bytes)
	}
}

func TestAsyncRequestEventLogger_EncodesBinaryPayloadAsBase64(t *testing.T) {
	t.Parallel()

	sink := &captureRequestEventSink{}
	logger := NewAsyncRequestEventLogger(RequestEventLoggerOptions{
		Enabled:              true,
		QueueSize:            4,
		MaxQueuedPayloadSize: 1 << 20,
		FlushInterval:        time.Hour,
	}, sink)

	event := logger.AcquireEvent()
	event.RequestID = "req-binary"
	event.Event = "body.chunk"
	event.SetPayloadBytes([]byte{0xff, 0x00, 0x01})
	logger.Emit(event)

	if err := logger.Close(context.Background()); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	var got struct {
		Payload struct {
			Encoding string `json:"encoding"`
			Body     string `json:"body"`
			Bytes    int    `json:"bytes"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(sink.lines[0], &got); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}
	if got.Payload.Encoding != "base64" {
		t.Fatalf("payload.encoding = %q, want base64", got.Payload.Encoding)
	}
	if got.Payload.Body != "/wAB" {
		t.Fatalf("payload.body = %q, want /wAB", got.Payload.Body)
	}
	if got.Payload.Bytes != 3 {
		t.Fatalf("payload.bytes = %d, want 3", got.Payload.Bytes)
	}
}

func TestFileRequestLoggerConfigureRequestEventsWritesHourlyJSONL(t *testing.T) {
	t.Parallel()

	logsDir := t.TempDir()
	fixedClock := func() time.Time {
		return time.Date(2026, 7, 8, 9, 30, 0, 0, time.UTC)
	}
	logger := NewFileRequestLogger(false, logsDir, "", 0)
	logger.ConfigureRequestEvents(FileRequestEventConfig{
		LoggerOptions: RequestEventLoggerOptions{
			Enabled:              true,
			QueueSize:            4,
			MaxQueuedPayloadSize: 1 << 20,
			FlushInterval:        time.Hour,
			Clock:                fixedClock,
		},
		MaxFileSize:     1 << 20,
		WriteBufferSize: 1024,
	})

	eventLogger := logger.RequestEventLogger()
	if eventLogger == nil {
		t.Fatal("RequestEventLogger() = nil, want enabled logger")
	}
	event := eventLogger.AcquireEvent()
	event.RequestID = "req-file"
	event.Event = "request.start"
	eventLogger.Emit(event)

	if errClose := logger.Close(context.Background()); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}

	eventsDir := filepath.Join(logsDir, "events")
	entries, errRead := os.ReadDir(eventsDir)
	if errRead != nil {
		t.Fatalf("ReadDir(%s): %v", eventsDir, errRead)
	}
	if len(entries) != 1 {
		t.Fatalf("events files = %d, want 1", len(entries))
	}
	if gotName := entries[0].Name(); gotName != "requests-2026-07-08T09-000001.jsonl" {
		t.Fatalf("event file = %q, want requests-2026-07-08T09-000001.jsonl", gotName)
	}

	raw, errReadFile := os.ReadFile(filepath.Join(eventsDir, entries[0].Name()))
	if errReadFile != nil {
		t.Fatalf("ReadFile: %v", errReadFile)
	}
	var got struct {
		RequestID string `json:"request_id"`
		Event     string `json:"event"`
	}
	if errUnmarshal := json.Unmarshal(raw, &got); errUnmarshal != nil {
		t.Fatalf("json.Unmarshal() error = %v line=%s", errUnmarshal, string(raw))
	}
	if got.RequestID != "req-file" || got.Event != "request.start" {
		t.Fatalf("event = %q/%q, want req-file/request.start", got.RequestID, got.Event)
	}
}

func TestRollingJSONLFileSinkSkipsFullExistingPart(t *testing.T) {
	t.Parallel()

	eventsDir := t.TempDir()
	existingPath := filepath.Join(eventsDir, "requests-2026-07-08T09-000001.jsonl")
	if errWrite := os.WriteFile(existingPath, bytes.Repeat([]byte("x"), 32), 0644); errWrite != nil {
		t.Fatalf("WriteFile(existing): %v", errWrite)
	}
	sink, errSink := NewRollingJSONLFileSink(RollingJSONLFileSinkOptions{
		Dir:              eventsDir,
		MaxFileSizeBytes: 32,
		WriteBufferSize:  128,
		Clock: func() time.Time {
			return time.Date(2026, 7, 8, 9, 45, 0, 0, time.UTC)
		},
	})
	if errSink != nil {
		t.Fatalf("NewRollingJSONLFileSink: %v", errSink)
	}
	if errWrite := sink.Write(context.Background(), []byte("{\"event\":\"next\"}\n")); errWrite != nil {
		t.Fatalf("Write: %v", errWrite)
	}
	if errClose := sink.Close(context.Background()); errClose != nil {
		t.Fatalf("Close: %v", errClose)
	}

	nextPath := filepath.Join(eventsDir, "requests-2026-07-08T09-000002.jsonl")
	raw, errRead := os.ReadFile(nextPath)
	if errRead != nil {
		t.Fatalf("ReadFile(next): %v", errRead)
	}
	if got := string(raw); got != "{\"event\":\"next\"}\n" {
		t.Fatalf("next part = %q, want JSON line", got)
	}
	existing, errReadExisting := os.ReadFile(existingPath)
	if errReadExisting != nil {
		t.Fatalf("ReadFile(existing): %v", errReadExisting)
	}
	if len(existing) != 32 {
		t.Fatalf("existing part len = %d, want 32", len(existing))
	}
}

func TestFileRequestLoggerConfigureRequestEventsKeepsOldLoggerWhenNewSinkFails(t *testing.T) {
	t.Parallel()

	logsDir := t.TempDir()
	logger := NewFileRequestLogger(false, logsDir, "", 0)
	logger.ConfigureRequestEvents(FileRequestEventConfig{
		LoggerOptions: RequestEventLoggerOptions{
			Enabled:              true,
			QueueSize:            4,
			MaxQueuedPayloadSize: 1 << 20,
			FlushInterval:        time.Hour,
		},
		MaxFileSize:     1 << 20,
		WriteBufferSize: 1024,
	})
	original := logger.RequestEventLogger()
	if original == nil {
		t.Fatal("initial RequestEventLogger() = nil")
	}

	blockingPath := filepath.Join(logsDir, "events")
	if errRemove := os.RemoveAll(blockingPath); errRemove != nil {
		t.Fatalf("RemoveAll(events): %v", errRemove)
	}
	if errWrite := os.WriteFile(blockingPath, []byte("not a directory"), 0644); errWrite != nil {
		t.Fatalf("WriteFile(events): %v", errWrite)
	}

	logger.ConfigureRequestEvents(FileRequestEventConfig{
		LoggerOptions: RequestEventLoggerOptions{
			Enabled:              true,
			QueueSize:            4,
			MaxQueuedPayloadSize: 1 << 20,
			FlushInterval:        time.Hour,
		},
		MaxFileSize:     1 << 20,
		WriteBufferSize: 1024,
	})

	if got := logger.RequestEventLogger(); got != original {
		t.Fatalf("RequestEventLogger() replaced old logger on sink failure")
	}
	if errClose := logger.Close(context.Background()); errClose != nil {
		t.Fatalf("Close logger: %v", errClose)
	}
}
