package logging

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
)

const (
	ProxyName                = "cpa"
	defaultDownstream        = "http"
	defaultProtocol          = "unk"
	defaultUpstreamTransport = "not_attempted"
	defaultSlot              = "none"
	shortSlotLength          = 7
)

type traceContextKey struct{}
type proxyStatusKey struct{}

// TraceContext captures the proxy-local W3C trace context used for logs and
// response diagnostics. It is not forwarded upstream.
type TraceContext struct {
	Traceparent             string
	TraceID                 string
	SpanID                  string
	Generated               bool
	ProxyWebsocketSessionID string
}

// ProxyStatus captures proxy diagnostics emitted in the Server-Timing envelope.
type ProxyStatus struct {
	DownstreamTransport string
	DownstreamProtocol  string
	UpstreamTransport   string
	UpstreamProtocol    string
	Slot                string
	Error               string
	Details             string
	AuthState           string
	UpstreamWSStatus    string
	FallbackReason      string
}

type proxyStatusHolder struct {
	mu     sync.RWMutex
	status ProxyStatus
}

// NewTraceContext parses a valid W3C traceparent or generates a fresh v00
// traceparent when the inbound value is absent or invalid.
func NewTraceContext(rawTraceparent string) TraceContext {
	if trace, ok := parseTraceparent(rawTraceparent); ok {
		return trace
	}
	return generateTraceContext()
}

// IsValidTraceparent reports whether the value is a valid W3C v00 traceparent.
func IsValidTraceparent(rawTraceparent string) bool {
	_, ok := parseTraceparent(rawTraceparent)
	return ok
}

func parseTraceparent(rawTraceparent string) (TraceContext, bool) {
	traceparent := strings.TrimSpace(rawTraceparent)
	parts := strings.Split(traceparent, "-")
	if len(parts) != 4 || parts[0] != "00" {
		return TraceContext{}, false
	}
	if !isLowerHex(parts[1], 32) || !isLowerHex(parts[2], 16) || !isLowerHex(parts[3], 2) {
		return TraceContext{}, false
	}
	if parts[1] == "00000000000000000000000000000000" || parts[2] == "0000000000000000" {
		return TraceContext{}, false
	}
	return TraceContext{
		Traceparent: traceparent,
		TraceID:     parts[1],
		SpanID:      parts[2],
	}, true
}

func generateTraceContext() TraceContext {
	traceID := randomNonZeroHex(16)
	spanID := randomNonZeroHex(8)
	traceparent := fmt.Sprintf("00-%s-%s-01", traceID, spanID)
	return TraceContext{
		Traceparent: traceparent,
		TraceID:     traceID,
		SpanID:      spanID,
		Generated:   true,
	}
}

func randomNonZeroHex(byteCount int) string {
	if byteCount <= 0 {
		return ""
	}
	buf := make([]byte, byteCount)
	for attempts := 0; attempts < 3; attempts++ {
		if _, err := rand.Read(buf); err == nil && !allZero(buf) {
			return hex.EncodeToString(buf)
		}
	}
	buf[len(buf)-1] = 1
	return hex.EncodeToString(buf)
}

func allZero(buf []byte) bool {
	for _, b := range buf {
		if b != 0 {
			return false
		}
	}
	return len(buf) > 0
}

func isLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func WithTraceContext(ctx context.Context, trace TraceContext) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if trace.Traceparent == "" {
		return ctx
	}
	return context.WithValue(ctx, traceContextKey{}, trace)
}

func GetTraceContext(ctx context.Context) TraceContext {
	if ctx == nil {
		return TraceContext{}
	}
	trace, _ := ctx.Value(traceContextKey{}).(TraceContext)
	return trace
}

func WithProxyStatusHolder(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder, ok := ctx.Value(proxyStatusKey{}).(*proxyStatusHolder); ok && holder != nil {
		return ctx
	}
	return context.WithValue(ctx, proxyStatusKey{}, &proxyStatusHolder{status: defaultProxyStatus()})
}

func WithProxyStatusHolderFrom(ctx context.Context, source context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder := proxyStatusHolderFromContext(source); holder != nil {
		return context.WithValue(ctx, proxyStatusKey{}, holder)
	}
	return ctx
}

func GetProxyStatus(ctx context.Context) ProxyStatus {
	holder := proxyStatusHolderFromContext(ctx)
	if holder == nil {
		return defaultProxyStatus()
	}
	holder.mu.RLock()
	defer holder.mu.RUnlock()
	return normalizeProxyStatus(holder.status)
}

func SetDownstreamTransport(ctx context.Context, transport string) {
	updateProxyStatus(ctx, func(status *ProxyStatus) {
		status.DownstreamTransport = sanitizeTokenValue(transport, status.DownstreamTransport)
	})
}

func SetDownstreamProtocol(ctx context.Context, protocol string) {
	updateProxyStatus(ctx, func(status *ProxyStatus) {
		status.DownstreamProtocol = sanitizeTokenValue(protocol, status.DownstreamProtocol)
	})
}

func SetUpstreamTransport(ctx context.Context, transport string) {
	updateProxyStatus(ctx, func(status *ProxyStatus) {
		status.UpstreamTransport = sanitizeTokenValue(transport, status.UpstreamTransport)
	})
}

func SetUpstreamProtocol(ctx context.Context, protocol string) {
	updateProxyStatus(ctx, func(status *ProxyStatus) {
		status.UpstreamProtocol = sanitizeTokenValue(protocol, status.UpstreamProtocol)
	})
}

func SetSlot(ctx context.Context, slot string) {
	updateProxyStatus(ctx, func(status *ProxyStatus) {
		status.Slot = ShortSlotIdentifier(slot)
	})
}

func SetProxyError(ctx context.Context, errCode string, details string) {
	updateProxyStatus(ctx, func(status *ProxyStatus) {
		status.Error = sanitizeTokenValue(errCode, status.Error)
		status.Details = strings.TrimSpace(details)
	})
}

func SetAuthState(ctx context.Context, authState string) {
	updateProxyStatus(ctx, func(status *ProxyStatus) {
		status.AuthState = sanitizeTokenValue(authState, status.AuthState)
	})
}

func SetUpstreamWebsocketStatus(ctx context.Context, statusCode int) {
	if statusCode <= 0 {
		return
	}
	updateProxyStatus(ctx, func(status *ProxyStatus) {
		status.UpstreamWSStatus = fmt.Sprintf("%d", statusCode)
	})
}

func SetFallbackReason(ctx context.Context, reason string) {
	updateProxyStatus(ctx, func(status *ProxyStatus) {
		status.FallbackReason = sanitizeTokenValue(reason, status.FallbackReason)
	})
}

func updateProxyStatus(ctx context.Context, fn func(*ProxyStatus)) {
	if fn == nil {
		return
	}
	holder := proxyStatusHolderFromContext(ctx)
	if holder == nil {
		return
	}
	holder.mu.Lock()
	defer holder.mu.Unlock()
	status := normalizeProxyStatus(holder.status)
	fn(&status)
	holder.status = normalizeProxyStatus(status)
}

func proxyStatusHolderFromContext(ctx context.Context) *proxyStatusHolder {
	if ctx == nil {
		return nil
	}
	holder, _ := ctx.Value(proxyStatusKey{}).(*proxyStatusHolder)
	return holder
}

func defaultProxyStatus() ProxyStatus {
	return ProxyStatus{
		DownstreamTransport: defaultDownstream,
		DownstreamProtocol:  defaultProtocol,
		UpstreamTransport:   defaultUpstreamTransport,
		UpstreamProtocol:    defaultProtocol,
		Slot:                defaultSlot,
	}
}

func normalizeProxyStatus(status ProxyStatus) ProxyStatus {
	status.DownstreamTransport = sanitizeTokenValue(status.DownstreamTransport, defaultDownstream)
	status.DownstreamProtocol = sanitizeTokenValue(status.DownstreamProtocol, defaultProtocol)
	status.UpstreamTransport = sanitizeTokenValue(status.UpstreamTransport, defaultUpstreamTransport)
	status.UpstreamProtocol = sanitizeTokenValue(status.UpstreamProtocol, defaultProtocol)
	status.Slot = ShortSlotIdentifier(status.Slot)
	status.Error = sanitizeTokenValue(status.Error, "")
	status.AuthState = sanitizeTokenValue(status.AuthState, "")
	status.UpstreamWSStatus = sanitizeTokenValue(status.UpstreamWSStatus, "")
	status.FallbackReason = sanitizeTokenValue(status.FallbackReason, "")
	status.Details = sanitizeTokenValue(status.Details, "")
	return status
}

// ShortSlotIdentifier returns the git-style seven-character prefix used in
// client-visible proxy diagnostics.
func ShortSlotIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.EqualFold(value, defaultSlot) {
		return defaultSlot
	}
	if len(value) > shortSlotLength {
		return value[:shortSlotLength]
	}
	return value
}

func ApplyProxyObservabilityHeaders(ctx context.Context, headers http.Header) {
	if headers == nil {
		return
	}
	trace := GetTraceContext(ctx)
	status := GetProxyStatus(ctx)
	if serverTiming := FormatServerTiming(trace, status); serverTiming != "" {
		appendHeader(headers, "Server-Timing", serverTiming)
	}
}

func appendHeader(headers http.Header, key string, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}

	values := headers.Values(key)
	if len(values) == 0 {
		headers.Set(key, value)
		return
	}

	existing := strings.Join(values, ", ")
	if strings.Contains(existing, value) {
		return
	}
	headers.Set(key, existing+", "+value)
}

func FormatServerTiming(trace TraceContext, status ProxyStatus) string {
	parts := make([]string, 0, 8)
	if trace.Traceparent != "" {
		parts = append(parts, `cpa.trace;desc="`+quoteHeaderValue(trace.Traceparent)+`"`)
	}
	status = normalizeProxyStatus(status)
	parts = append(parts, `cpa.path;desc="`+quoteHeaderValue(formatProxyPath(status))+`"`)
	parts = append(parts, `cpa.slot;desc="`+quoteHeaderValue(status.Slot)+`"`)
	if trace.ProxyWebsocketSessionID != "" {
		parts = append(parts, `cpa.session;desc="`+quoteHeaderValue(ShortSlotIdentifier(trace.ProxyWebsocketSessionID))+`"`)
	}
	if status.FallbackReason != "" {
		parts = append(parts, `cpa.fallback;desc="`+quoteHeaderValue(status.FallbackReason)+`"`)
	}
	if status.UpstreamWSStatus != "" {
		parts = append(parts, `cpa.ws;desc="`+quoteHeaderValue(status.UpstreamWSStatus)+`"`)
	}
	if status.Error != "" {
		parts = append(parts, `cpa.error;desc="`+quoteHeaderValue(status.Error)+`"`)
	}
	if status.Details != "" {
		parts = append(parts, `cpa.detail;desc="`+quoteHeaderValue(status.Details)+`"`)
	}
	if status.AuthState != "" {
		parts = append(parts, `cpa.auth;desc="`+quoteHeaderValue(status.AuthState)+`"`)
	}
	return strings.Join(parts, ", ")
}

func formatProxyPath(status ProxyStatus) string {
	return formatProxyEdge(status.DownstreamTransport, status.DownstreamProtocol) + ">" + formatProxyEdge(status.UpstreamTransport, status.UpstreamProtocol)
}

func formatProxyEdge(transport string, protocol string) string {
	transport = edgeTransportToken(transport)
	if transport == "none" {
		return transport
	}
	protocol = sanitizeTokenValue(protocol, defaultProtocol)
	return transport + "." + protocol
}

func HTTPProtocolToken(protoMajor int) string {
	switch protoMajor {
	case 1:
		return "h1"
	case 2:
		return "h2"
	case 3:
		return "h3"
	default:
		return defaultProtocol
	}
}

func edgeTransportToken(transport string) string {
	switch strings.ToLower(strings.TrimSpace(transport)) {
	case "", "unknown":
		return "unk"
	case "not_attempted", "none":
		return "none"
	case "websocket", "ws", "wss":
		return "ws"
	case "server_sent_events", "event-stream", "event_stream", "sse":
		return "sse"
	default:
		return sanitizeTokenValue(transport, "unk")
	}
}

func sanitizeTokenValue(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return strings.TrimSpace(fallback)
	}
	var builder strings.Builder
	for _, r := range value {
		if isTokenRune(r) {
			builder.WriteRune(r)
		} else if r == ' ' || r == '_' {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return strings.TrimSpace(fallback)
	}
	return builder.String()
}

func isTokenRune(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') ||
		r == '-' || r == '_' || r == '.' || r == '~'
}

func quoteHeaderValue(value string) string {
	var builder strings.Builder
	for _, r := range value {
		switch r {
		case '\\', '"':
			builder.WriteByte('\\')
			builder.WriteRune(r)
		case '\r', '\n':
			builder.WriteByte(' ')
		default:
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func (trace TraceContext) LogFields() map[string]any {
	fields := map[string]any{}
	if trace.Traceparent != "" {
		fields["traceparent"] = trace.Traceparent
	}
	if trace.TraceID != "" {
		fields["trace_id"] = trace.TraceID
	}
	if trace.SpanID != "" {
		fields["span_id"] = trace.SpanID
	}
	if trace.ProxyWebsocketSessionID != "" {
		fields["proxy_websocket_session_id"] = trace.ProxyWebsocketSessionID
	}
	return fields
}

func WriteTraceContext(builder *strings.Builder, trace TraceContext) {
	if builder == nil {
		return
	}
	if trace.Traceparent != "" {
		builder.WriteString("traceparent: ")
		builder.WriteString(trace.Traceparent)
		builder.WriteString("\n")
	}
	if trace.TraceID != "" {
		builder.WriteString("trace_id: ")
		builder.WriteString(trace.TraceID)
		builder.WriteString("\n")
	}
	if trace.SpanID != "" {
		builder.WriteString("span_id: ")
		builder.WriteString(trace.SpanID)
		builder.WriteString("\n")
	}
	if trace.ProxyWebsocketSessionID != "" {
		builder.WriteString("proxy_websocket_session_id: ")
		builder.WriteString(trace.ProxyWebsocketSessionID)
		builder.WriteString("\n")
	}
}
