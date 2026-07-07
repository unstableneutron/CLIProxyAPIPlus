package logging

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	log "github.com/sirupsen/logrus"
)

const (
	defaultRequestEventQueueSize           = 32768
	defaultRequestEventMaxQueuedPayload    = 256 << 20
	defaultRequestEventWriteBufferSize     = 1024 << 10
	defaultRequestEventFlushInterval       = time.Second
	defaultRequestEventShutdownGracePeriod = 5 * time.Second
	defaultRequestEventMaxFileSize         = 128 << 20
	defaultRequestEventFilePrefix          = "requests-"
)

// RequestEventSink receives already serialized JSONL request event lines.
type RequestEventSink interface {
	Write(ctx context.Context, line []byte) error
	Flush(ctx context.Context) error
	Close(ctx context.Context) error
}

// RequestEventLoggerOptions configures the async JSONL request event logger.
type RequestEventLoggerOptions struct {
	Enabled              bool
	QueueSize            int
	MaxQueuedPayloadSize int64
	FlushInterval        time.Duration
	ShutdownGracePeriod  time.Duration
	Clock                func() time.Time
}

// FileRequestEventConfig configures the optional JSONL child owned by FileRequestLogger.
type FileRequestEventConfig struct {
	LoggerOptions   RequestEventLoggerOptions
	MaxFileSize     int64
	WriteBufferSize int
	Prefix          string
}

// AsyncRequestEventLogger owns request-event queueing, payload budgeting, and serialization.
type AsyncRequestEventLogger struct {
	enabled             bool
	events              chan *RequestEvent
	sink                RequestEventSink
	flushInterval       time.Duration
	shutdownGracePeriod time.Duration
	clock               func() time.Time
	pool                sync.Pool
	queuedPayloadBytes  atomic.Int64
	maxQueuedPayload    int64
	droppedEvents       atomic.Uint64
	closeMu             sync.Mutex
	closing             bool
	closeOnce           sync.Once
	done                chan struct{}
}

// RequestEventLoggerFromRequestLogger returns the optional JSONL event logger if present.
func RequestEventLoggerFromRequestLogger(logger RequestLogger) *AsyncRequestEventLogger {
	provider, ok := logger.(interface {
		RequestEventLogger() *AsyncRequestEventLogger
	})
	if !ok || provider == nil {
		return nil
	}
	eventLogger := provider.RequestEventLogger()
	if eventLogger == nil || !eventLogger.IsEnabled() {
		return nil
	}
	return eventLogger
}

// ConfigureRequestEvents replaces the optional JSONL event logger child.
func (l *FileRequestLogger) ConfigureRequestEvents(cfg FileRequestEventConfig) {
	if l == nil {
		return
	}
	if !cfg.LoggerOptions.Enabled {
		l.closePreviousRequestEventLogger(l.swapRequestEventLogger(nil))
		return
	}
	next := l.newRequestEventLogger(cfg)
	if next == nil {
		return
	}
	l.closePreviousRequestEventLogger(l.swapRequestEventLogger(next))
}

func (l *FileRequestLogger) newRequestEventLogger(cfg FileRequestEventConfig) *AsyncRequestEventLogger {
	sink, errSink := NewRollingJSONLFileSink(RollingJSONLFileSinkOptions{
		Dir:              filepath.Join(l.logsDir, "events"),
		Prefix:           cfg.Prefix,
		MaxFileSizeBytes: cfg.MaxFileSize,
		WriteBufferSize:  cfg.WriteBufferSize,
		Clock:            cfg.LoggerOptions.Clock,
	})
	if errSink != nil {
		log.WithError(errSink).Warn("failed to configure request event sink")
		return nil
	}
	return NewAsyncRequestEventLogger(cfg.LoggerOptions, sink)
}

func (l *FileRequestLogger) closePreviousRequestEventLogger(old *AsyncRequestEventLogger) {
	if old == nil {
		return
	}
	if errClose := old.Close(context.Background()); errClose != nil {
		log.WithError(errClose).Warn("failed to close previous request event logger")
	}
}

func (l *FileRequestLogger) swapRequestEventLogger(next *AsyncRequestEventLogger) *AsyncRequestEventLogger {
	if l == nil {
		return nil
	}
	l.requestEventsMu.Lock()
	defer l.requestEventsMu.Unlock()
	old := l.requestEvents
	l.requestEvents = next
	return old
}

// RequestEventLogger returns the optional JSONL event logger.
func (l *FileRequestLogger) RequestEventLogger() *AsyncRequestEventLogger {
	if l == nil {
		return nil
	}
	l.requestEventsMu.RLock()
	defer l.requestEventsMu.RUnlock()
	return l.requestEvents
}

// RequestEventsEnabled reports whether JSONL request event logging is active.
func (l *FileRequestLogger) RequestEventsEnabled() bool {
	eventLogger := l.RequestEventLogger()
	return eventLogger != nil && eventLogger.IsEnabled()
}

// Close drains the optional JSONL event logger.
func (l *FileRequestLogger) Close(ctx context.Context) error {
	if l == nil {
		return nil
	}
	old := l.swapRequestEventLogger(nil)
	if old == nil {
		return nil
	}
	return old.Close(ctx)
}

// RequestEvent is a provider-agnostic JSONL request event envelope.
type RequestEvent struct {
	SchemaVersion int                    `json:"schema_version"`
	Timestamp     time.Time              `json:"timestamp"`
	EventID       string                 `json:"event_id,omitempty"`
	RequestID     string                 `json:"request_id,omitempty"`
	Sequence      uint64                 `json:"sequence,omitempty"`
	Event         string                 `json:"event"`
	Boundary      string                 `json:"boundary,omitempty"`
	Direction     string                 `json:"direction,omitempty"`
	Protocol      string                 `json:"protocol,omitempty"`
	ContentType   string                 `json:"content_type,omitempty"`
	HTTP          *RequestEventHTTP      `json:"http,omitempty"`
	Upstream      *RequestEventUpstream  `json:"upstream,omitempty"`
	Error         *RequestEventError     `json:"error,omitempty"`
	Payload       *RequestEventPayload   `json:"payload,omitempty"`
	Attributes    map[string]interface{} `json:"attributes,omitempty"`

	payloadBytes      []byte
	queuedPayloadSize int64
}

// RequestEventHTTP contains HTTP request/response context.
type RequestEventHTTP struct {
	Method          string              `json:"method,omitempty"`
	URL             string              `json:"url,omitempty"`
	StatusCode      int                 `json:"status_code,omitempty"`
	RequestHeaders  map[string][]string `json:"request_headers,omitempty"`
	ResponseHeaders map[string][]string `json:"response_headers,omitempty"`
}

// RequestEventUpstream contains upstream/provider context without secrets.
type RequestEventUpstream struct {
	URL       string `json:"url,omitempty"`
	Method    string `json:"method,omitempty"`
	Provider  string `json:"provider,omitempty"`
	AuthID    string `json:"auth_id,omitempty"`
	AuthLabel string `json:"auth_label,omitempty"`
	AuthType  string `json:"auth_type,omitempty"`
}

// RequestEventError stores errors under payload-adjacent structured context.
type RequestEventError struct {
	Stage   string `json:"stage,omitempty"`
	Message string `json:"message,omitempty"`
}

// RequestEventPayload stores the event payload as a JSON string.
type RequestEventPayload struct {
	Encoding string `json:"encoding,omitempty"`
	Body     string `json:"body,omitempty"`
	Bytes    int    `json:"bytes"`
}

// SetPayloadBytes copies the payload into the event so callers can safely reuse buffers.
func (e *RequestEvent) SetPayloadBytes(payload []byte) {
	if e == nil || len(payload) == 0 {
		return
	}
	e.payloadBytes = append(e.payloadBytes[:0], payload...)
	e.Payload = &RequestEventPayload{Bytes: len(payload)}
}

// NewAsyncRequestEventLogger creates an async JSONL request event logger.
func NewAsyncRequestEventLogger(opts RequestEventLoggerOptions, sink RequestEventSink) *AsyncRequestEventLogger {
	opts = normalizeRequestEventLoggerOptions(opts)
	logger := &AsyncRequestEventLogger{
		enabled:             opts.Enabled && sink != nil,
		sink:                sink,
		flushInterval:       opts.FlushInterval,
		shutdownGracePeriod: opts.ShutdownGracePeriod,
		clock:               opts.Clock,
		maxQueuedPayload:    opts.MaxQueuedPayloadSize,
		done:                make(chan struct{}),
	}
	logger.pool.New = func() interface{} {
		return &RequestEvent{}
	}
	if !logger.enabled {
		close(logger.done)
		return logger
	}
	logger.events = make(chan *RequestEvent, opts.QueueSize)
	go logger.run()
	return logger
}

func normalizeRequestEventLoggerOptions(opts RequestEventLoggerOptions) RequestEventLoggerOptions {
	if opts.QueueSize <= 0 {
		opts.QueueSize = defaultRequestEventQueueSize
	}
	if opts.MaxQueuedPayloadSize <= 0 {
		opts.MaxQueuedPayloadSize = defaultRequestEventMaxQueuedPayload
	}
	if opts.FlushInterval <= 0 {
		opts.FlushInterval = defaultRequestEventFlushInterval
	}
	if opts.ShutdownGracePeriod <= 0 {
		opts.ShutdownGracePeriod = defaultRequestEventShutdownGracePeriod
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return opts
}

// IsEnabled reports whether JSONL request events are enabled.
func (l *AsyncRequestEventLogger) IsEnabled() bool {
	return l != nil && l.enabled
}

// DroppedEvents returns the number of events dropped because the queue or payload budget was full.
func (l *AsyncRequestEventLogger) DroppedEvents() uint64 {
	if l == nil {
		return 0
	}
	return l.droppedEvents.Load()
}

// AcquireEvent returns a pooled event initialized with schema and timestamp defaults.
func (l *AsyncRequestEventLogger) AcquireEvent() *RequestEvent {
	if l == nil {
		return &RequestEvent{SchemaVersion: 1, Timestamp: time.Now()}
	}
	event := l.pool.Get().(*RequestEvent)
	event.reset()
	event.SchemaVersion = 1
	event.Timestamp = l.clock()
	return event
}

// Emit queues an event for asynchronous serialization and writing.
func (l *AsyncRequestEventLogger) Emit(event *RequestEvent) bool {
	if l == nil || !l.enabled || event == nil {
		return false
	}
	payloadSize := int64(len(event.payloadBytes))
	if payloadSize > 0 && l.maxQueuedPayload > 0 {
		if queued := l.queuedPayloadBytes.Add(payloadSize); queued > l.maxQueuedPayload {
			l.queuedPayloadBytes.Add(-payloadSize)
			l.droppedEvents.Add(1)
			l.releaseEvent(event)
			return false
		}
		event.queuedPayloadSize = payloadSize
	}

	l.closeMu.Lock()
	defer l.closeMu.Unlock()
	if l.closing {
		if payloadSize > 0 {
			l.queuedPayloadBytes.Add(-payloadSize)
		}
		l.droppedEvents.Add(1)
		l.releaseEvent(event)
		return false
	}
	select {
	case l.events <- event:
		return true
	default:
		if payloadSize > 0 {
			l.queuedPayloadBytes.Add(-payloadSize)
		}
		l.droppedEvents.Add(1)
		l.releaseEvent(event)
		return false
	}
}

// Close stops accepting events and waits for queued events to flush.
func (l *AsyncRequestEventLogger) Close(ctx context.Context) error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		if l.enabled {
			l.closeMu.Lock()
			l.closing = true
			close(l.events)
			l.closeMu.Unlock()
		}
	})
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, l.shutdownGracePeriod)
	defer cancel()
	select {
	case <-l.done:
		return nil
	case <-waitCtx.Done():
		return waitCtx.Err()
	}
}

func (l *AsyncRequestEventLogger) run() {
	defer close(l.done)
	ticker := time.NewTicker(l.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case event, ok := <-l.events:
			if !ok {
				l.drainAndClose()
				return
			}
			l.writeEvent(event)
		case <-ticker.C:
			if err := l.sink.Flush(context.Background()); err != nil {
				log.WithError(err).Warn("failed to flush request event sink")
			}
		}
	}
}

func (l *AsyncRequestEventLogger) drainAndClose() {
	for event := range l.events {
		l.writeEvent(event)
	}
	ctx := context.Background()
	if err := l.sink.Flush(ctx); err != nil {
		log.WithError(err).Warn("failed to flush request event sink")
	}
	if err := l.sink.Close(ctx); err != nil {
		log.WithError(err).Warn("failed to close request event sink")
	}
}

func (l *AsyncRequestEventLogger) writeEvent(event *RequestEvent) {
	defer l.releaseEvent(event)
	if event == nil {
		return
	}
	if event.SchemaVersion == 0 {
		event.SchemaVersion = 1
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = l.clock()
	}
	if event.EventID == "" && event.RequestID != "" && event.Sequence > 0 {
		event.EventID = fmt.Sprintf("%s:%d", event.RequestID, event.Sequence)
	}
	if len(event.payloadBytes) > 0 {
		if event.Payload == nil {
			event.Payload = &RequestEventPayload{}
		}
		event.Payload.Bytes = len(event.payloadBytes)
		if utf8.Valid(event.payloadBytes) {
			event.Payload.Encoding = "utf8"
			event.Payload.Body = string(event.payloadBytes)
		} else {
			event.Payload.Encoding = "base64"
			event.Payload.Body = base64.StdEncoding.EncodeToString(event.payloadBytes)
		}
	}
	raw, errMarshal := json.Marshal(event)
	if event.queuedPayloadSize > 0 {
		l.queuedPayloadBytes.Add(-event.queuedPayloadSize)
		event.queuedPayloadSize = 0
	}
	if errMarshal != nil {
		log.WithError(errMarshal).Warn("failed to marshal request event")
		return
	}
	raw = append(raw, '\n')
	if errWrite := l.sink.Write(context.Background(), raw); errWrite != nil {
		log.WithError(errWrite).Warn("failed to write request event")
	}
}

func (l *AsyncRequestEventLogger) releaseEvent(event *RequestEvent) {
	if event == nil {
		return
	}
	event.reset()
	if l != nil {
		l.pool.Put(event)
	}
}

func (e *RequestEvent) reset() {
	if e == nil {
		return
	}
	e.SchemaVersion = 0
	e.Timestamp = time.Time{}
	e.EventID = ""
	e.RequestID = ""
	e.Sequence = 0
	e.Event = ""
	e.Boundary = ""
	e.Direction = ""
	e.Protocol = ""
	e.ContentType = ""
	e.HTTP = nil
	e.Upstream = nil
	e.Error = nil
	e.Payload = nil
	e.Attributes = nil
	e.payloadBytes = e.payloadBytes[:0]
	e.queuedPayloadSize = 0
}

// RollingJSONLFileSink writes JSONL events into hourly files with size spillover parts.
type RollingJSONLFileSink struct {
	mu           sync.Mutex
	dir          string
	prefix       string
	maxFileSize  int64
	writeBufSize int
	clock        func() time.Time
	currentHour  string
	currentPart  int
	currentSize  int64
	file         *os.File
	writer       *bufio.Writer
}

// RollingJSONLFileSinkOptions configures the file sink.
type RollingJSONLFileSinkOptions struct {
	Dir              string
	Prefix           string
	MaxFileSizeBytes int64
	WriteBufferSize  int
	Clock            func() time.Time
}

// NewRollingJSONLFileSink creates a rolling JSONL file sink.
func NewRollingJSONLFileSink(opts RollingJSONLFileSinkOptions) (*RollingJSONLFileSink, error) {
	opts = normalizeRollingJSONLFileSinkOptions(opts)
	if strings.TrimSpace(opts.Dir) == "" {
		return nil, fmt.Errorf("request event sink directory is required")
	}
	if errMkdir := os.MkdirAll(opts.Dir, 0755); errMkdir != nil {
		return nil, errMkdir
	}
	return &RollingJSONLFileSink{
		dir:          opts.Dir,
		prefix:       opts.Prefix,
		maxFileSize:  opts.MaxFileSizeBytes,
		writeBufSize: opts.WriteBufferSize,
		clock:        opts.Clock,
	}, nil
}

func normalizeRollingJSONLFileSinkOptions(opts RollingJSONLFileSinkOptions) RollingJSONLFileSinkOptions {
	if opts.Prefix == "" {
		opts.Prefix = defaultRequestEventFilePrefix
	}
	if opts.MaxFileSizeBytes <= 0 {
		opts.MaxFileSizeBytes = defaultRequestEventMaxFileSize
	}
	if opts.WriteBufferSize <= 0 {
		opts.WriteBufferSize = defaultRequestEventWriteBufferSize
	}
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return opts
}

// Write appends one JSONL event line to the current file.
func (s *RollingJSONLFileSink) Write(ctx context.Context, line []byte) error {
	if s == nil {
		return nil
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	if len(line) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if errOpen := s.ensureFileLocked(int64(len(line))); errOpen != nil {
		return errOpen
	}
	if _, errWrite := s.writer.Write(line); errWrite != nil {
		return errWrite
	}
	s.currentSize += int64(len(line))
	return nil
}

// Flush flushes buffered event lines to the current file.
func (s *RollingJSONLFileSink) Flush(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writer == nil {
		return nil
	}
	return s.writer.Flush()
}

// Close flushes and closes the current file.
func (s *RollingJSONLFileSink) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if err := ctxErr(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeFileLocked()
}

func (s *RollingJSONLFileSink) ensureFileLocked(nextLineSize int64) error {
	hour := s.clock().Format("2006-01-02T15")
	shouldRotate := s.file == nil || s.currentHour != hour
	if !shouldRotate && s.maxFileSize > 0 && s.currentSize > 0 && s.currentSize+nextLineSize > s.maxFileSize {
		shouldRotate = true
	}
	if !shouldRotate {
		return nil
	}
	if errClose := s.closeFileLocked(); errClose != nil {
		return errClose
	}
	if s.currentHour != hour {
		s.currentHour = hour
		s.currentPart = 0
	}
	for {
		s.currentPart++
		filename := fmt.Sprintf("%s%s-%06d.jsonl", s.prefix, s.currentHour, s.currentPart)
		path := filepath.Join(s.dir, filename)
		info, errStat := os.Stat(path)
		if errStat != nil && !os.IsNotExist(errStat) {
			return errStat
		}
		if errStat == nil && s.maxFileSize > 0 && info.Size() > 0 && info.Size()+nextLineSize > s.maxFileSize {
			continue
		}
		file, errOpen := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if errOpen != nil {
			return errOpen
		}
		info, errStat = file.Stat()
		if errStat != nil {
			_ = file.Close()
			return errStat
		}
		s.file = file
		s.writer = bufio.NewWriterSize(file, s.writeBufSize)
		s.currentSize = info.Size()
		return nil
	}
}

func (s *RollingJSONLFileSink) closeFileLocked() error {
	var firstErr error
	if s.writer != nil {
		if errFlush := s.writer.Flush(); errFlush != nil {
			firstErr = errFlush
		}
		s.writer = nil
	}
	if s.file != nil {
		if errClose := s.file.Close(); errClose != nil && firstErr == nil {
			firstErr = errClose
		}
		s.file = nil
	}
	return firstErr
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
