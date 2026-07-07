package config

const (
	DefaultRequestEventsMaxFileSizeMB      = 128
	DefaultRequestEventsQueueSize          = 32768
	DefaultRequestEventsMaxQueuedPayloadMB = 256
	DefaultRequestEventsWriteBufferSizeKB  = 1024
	DefaultRequestEventsFlushIntervalMS    = 1000
)

// RequestEventsConfig controls line-oriented JSONL request event logging.
type RequestEventsConfig struct {
	Enabled            bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	MaxFileSizeMB      int  `yaml:"max-file-size-mb,omitempty" json:"max-file-size-mb,omitempty"`
	QueueSize          int  `yaml:"queue-size,omitempty" json:"queue-size,omitempty"`
	MaxQueuedPayloadMB int  `yaml:"max-queued-payload-mb,omitempty" json:"max-queued-payload-mb,omitempty"`
	WriteBufferSizeKB  int  `yaml:"write-buffer-size-kb,omitempty" json:"write-buffer-size-kb,omitempty"`
	FlushIntervalMS    int  `yaml:"flush-interval-ms,omitempty" json:"flush-interval-ms,omitempty"`
}

// IsEnabled reports whether JSONL request event logging is enabled.
func (c RequestEventsConfig) IsEnabled() bool {
	return c.Enabled
}

// Normalize fills numeric defaults and clamps invalid values.
func (c *RequestEventsConfig) Normalize() {
	if c == nil {
		return
	}
	if c.MaxFileSizeMB <= 0 {
		c.MaxFileSizeMB = DefaultRequestEventsMaxFileSizeMB
	}
	if c.QueueSize <= 0 {
		c.QueueSize = DefaultRequestEventsQueueSize
	}
	if c.MaxQueuedPayloadMB <= 0 {
		c.MaxQueuedPayloadMB = DefaultRequestEventsMaxQueuedPayloadMB
	}
	if c.WriteBufferSizeKB <= 0 {
		c.WriteBufferSizeKB = DefaultRequestEventsWriteBufferSizeKB
	}
	if c.FlushIntervalMS <= 0 {
		c.FlushIntervalMS = DefaultRequestEventsFlushIntervalMS
	}
}

// Normalized returns a copy with defaults applied.
func (c RequestEventsConfig) Normalized() RequestEventsConfig {
	c.Normalize()
	return c
}
