package logging

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const validTraceparentForTest = "00-1234567890abcdef1234567890abcdef-abcdef1234567890-01"

func TestNewTraceContextUsesValidInboundTraceparent(t *testing.T) {
	trace := NewTraceContext(validTraceparentForTest)

	if trace.Traceparent != validTraceparentForTest {
		t.Fatalf("traceparent = %q", trace.Traceparent)
	}
	if trace.TraceID != "1234567890abcdef1234567890abcdef" {
		t.Fatalf("trace id = %q", trace.TraceID)
	}
	if trace.SpanID != "abcdef1234567890" {
		t.Fatalf("span id = %q", trace.SpanID)
	}
	if trace.Generated {
		t.Fatal("valid inbound traceparent should not be marked generated")
	}
}

func TestNewTraceContextGeneratesForInvalidInboundTraceparent(t *testing.T) {
	trace := NewTraceContext("not-a-valid-traceparent")

	if trace.Traceparent == "not-a-valid-traceparent" {
		t.Fatal("invalid inbound traceparent should not be preserved")
	}
	if !trace.Generated {
		t.Fatal("invalid inbound traceparent should generate a replacement")
	}
	if !IsValidTraceparent(trace.Traceparent) {
		t.Fatalf("generated traceparent is invalid: %q", trace.Traceparent)
	}
}

func TestShortSlotIdentifierUsesGitStylePrefix(t *testing.T) {
	if got := ShortSlotIdentifier("6f2a9c0e1b3d4a55"); got != "6f2a9c0" {
		t.Fatalf("short slot identifier = %q", got)
	}
	if got := ShortSlotIdentifier(""); got != "none" {
		t.Fatalf("empty slot identifier = %q", got)
	}
}

func TestApplyProxyObservabilityHeadersUsesServerTimingEnvelope(t *testing.T) {
	ctx := WithTraceContext(context.Background(), NewTraceContext(validTraceparentForTest))
	ctx = WithProxyStatusHolder(ctx)
	SetDownstreamTransport(ctx, "http")
	SetDownstreamProtocol(ctx, "h2")
	SetUpstreamTransport(ctx, "sse")
	SetUpstreamProtocol(ctx, "h1")
	SetSlot(ctx, "6f2a9c0e1b3d4a55")
	SetFallbackReason(ctx, "ws_disabled")

	headers := http.Header{}
	ApplyProxyObservabilityHeaders(ctx, headers)

	if got := headers.Get("Via"); got != "" {
		t.Fatalf("Via should not be emitted: %q", got)
	}
	if got := headers.Get("Proxy-Status"); got != "" {
		t.Fatalf("Proxy-Status should not be emitted: %q", got)
	}
	serverTiming := headers.Get("Server-Timing")
	for _, want := range []string{
		`cpa.trace;desc="` + validTraceparentForTest + `"`,
		`cpa.path;desc="http.h2>sse.h1"`,
		`cpa.slot;desc="6f2a9c0"`,
		`cpa.fallback;desc="ws_disabled"`,
	} {
		if !strings.Contains(serverTiming, want) {
			t.Fatalf("Server-Timing missing %q: %q", want, serverTiming)
		}
	}
}
