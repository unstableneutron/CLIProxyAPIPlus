package config

import "testing"

func TestParseConfigBytes_RequestEventsDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfigBytes([]byte(`request-log: true`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	if cfg.RequestEvents.IsEnabled() {
		t.Fatal("RequestEvents.IsEnabled() = true, want false")
	}
	assertDefaultRequestEventsConfig(t, cfg.RequestEvents)
}

func TestParseConfigBytes_RequestEventsExplicitValues(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfigBytes([]byte(`
request-events:
  enabled: true
  max-file-size-mb: 64
  queue-size: 2048
  max-queued-payload-mb: 32
  write-buffer-size-kb: 512
  flush-interval-ms: 250
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	if !cfg.RequestEvents.IsEnabled() {
		t.Fatal("RequestEvents.IsEnabled() = false, want true")
	}
	if cfg.RequestEvents.MaxFileSizeMB != 64 {
		t.Fatalf("MaxFileSizeMB = %d, want 64", cfg.RequestEvents.MaxFileSizeMB)
	}
	if cfg.RequestEvents.QueueSize != 2048 {
		t.Fatalf("QueueSize = %d, want 2048", cfg.RequestEvents.QueueSize)
	}
	if cfg.RequestEvents.MaxQueuedPayloadMB != 32 {
		t.Fatalf("MaxQueuedPayloadMB = %d, want 32", cfg.RequestEvents.MaxQueuedPayloadMB)
	}
	if cfg.RequestEvents.WriteBufferSizeKB != 512 {
		t.Fatalf("WriteBufferSizeKB = %d, want 512", cfg.RequestEvents.WriteBufferSizeKB)
	}
	if cfg.RequestEvents.FlushIntervalMS != 250 {
		t.Fatalf("FlushIntervalMS = %d, want 250", cfg.RequestEvents.FlushIntervalMS)
	}
}

func TestParseConfigBytes_RequestEventsInvalidNumericValuesUseDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfigBytes([]byte(`
request-events:
  enabled: true
  max-file-size-mb: -1
  queue-size: 0
  max-queued-payload-mb: -5
  write-buffer-size-kb: 0
  flush-interval-ms: -10
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	if !cfg.RequestEvents.IsEnabled() {
		t.Fatal("RequestEvents.IsEnabled() = false, want true")
	}
	assertDefaultRequestEventsConfig(t, cfg.RequestEvents)
}

func assertDefaultRequestEventsConfig(t *testing.T, got RequestEventsConfig) {
	t.Helper()

	if got.MaxFileSizeMB != DefaultRequestEventsMaxFileSizeMB {
		t.Fatalf("MaxFileSizeMB = %d, want %d", got.MaxFileSizeMB, DefaultRequestEventsMaxFileSizeMB)
	}
	if got.QueueSize != DefaultRequestEventsQueueSize {
		t.Fatalf("QueueSize = %d, want %d", got.QueueSize, DefaultRequestEventsQueueSize)
	}
	if got.MaxQueuedPayloadMB != DefaultRequestEventsMaxQueuedPayloadMB {
		t.Fatalf("MaxQueuedPayloadMB = %d, want %d", got.MaxQueuedPayloadMB, DefaultRequestEventsMaxQueuedPayloadMB)
	}
	if got.WriteBufferSizeKB != DefaultRequestEventsWriteBufferSizeKB {
		t.Fatalf("WriteBufferSizeKB = %d, want %d", got.WriteBufferSizeKB, DefaultRequestEventsWriteBufferSizeKB)
	}
	if got.FlushIntervalMS != DefaultRequestEventsFlushIntervalMS {
		t.Fatalf("FlushIntervalMS = %d, want %d", got.FlushIntervalMS, DefaultRequestEventsFlushIntervalMS)
	}
}
