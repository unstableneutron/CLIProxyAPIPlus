package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestWebSocketSmokeStopsAtResponseCompleted(t *testing.T) {
	const marker = "WEBSOCKET_MARKER_123"
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read request: %v", errRead)
			return
		}
		if errWrite := conn.WriteJSON(map[string]any{
			"type":  "response.output_text.delta",
			"delta": marker,
		}); errWrite != nil {
			t.Errorf("write delta: %v", errWrite)
			return
		}
		if errWrite := conn.WriteJSON(map[string]any{"type": "response.completed"}); errWrite != nil {
			t.Errorf("write completion: %v", errWrite)
			return
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	result, err := runResponses(context.Background(), responsesConfig{
		BaseURL:    server.URL,
		Model:      "test-model",
		Marker:     marker,
		APIKey:     "test-key",
		Transport:  "websocket",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("websocket smoke failed: %v", err)
	}
	if result.TerminalEvent != "response.completed" {
		t.Fatalf("terminal event = %q, want response.completed", result.TerminalEvent)
	}
	if !result.MarkerMatched || result.Outcome != outcomePassed {
		t.Fatalf("unexpected result: %+v", result)
	}
}

func TestWebSocketSmokeFailsAtResponseFailed(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"error": map[string]any{"message": "provider unavailable"},
			},
		})
	}))
	defer server.Close()

	result, err := runResponses(context.Background(), responsesConfig{
		BaseURL:    server.URL,
		Model:      "test-model",
		Marker:     "unused-marker",
		APIKey:     "test-key",
		Transport:  "websocket",
		HTTPClient: server.Client(),
	})
	if err == nil {
		t.Fatal("websocket smoke passed after response.failed")
	}
	if result.TerminalEvent != "response.failed" || result.Outcome != outcomeFailed {
		t.Fatalf("unexpected failure result: %+v", result)
	}
}

func TestSSESmokeStopsAtDone(t *testing.T) {
	const marker = "SSE_MARKER_456"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":%q}\n\n", marker)
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	result, err := runResponses(context.Background(), responsesConfig{
		BaseURL:    server.URL,
		Model:      "test-model",
		Marker:     marker,
		APIKey:     "test-key",
		Transport:  "sse",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("SSE smoke failed: %v", err)
	}
	if result.TerminalEvent != "response.completed" || !result.MarkerMatched {
		t.Fatalf("unexpected SSE result: %+v", result)
	}
}

func TestMarkerValidationRejectsWrongOutput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"status":"completed","output_text":"a different value"}`)
	}))
	defer server.Close()

	result, err := runResponses(context.Background(), responsesConfig{
		BaseURL:    server.URL,
		Model:      "test-model",
		Marker:     "EXPECTED_MARKER",
		APIKey:     "test-key",
		Transport:  "rest",
		HTTPClient: server.Client(),
	})
	if err == nil {
		t.Fatal("REST smoke accepted output without the marker")
	}
	if result.MarkerMatched || result.Outcome != outcomeFailed {
		t.Fatalf("unexpected marker failure result: %+v", result)
	}
}

func TestJWTPreflightRejectsExpiredToken(t *testing.T) {
	now := time.Unix(2_000_000_000, 0)
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1999999999}`))
	token := strings.Join([]string{header, payload, "signature"}, ".")

	_, err := preflightJWT(token, 5*time.Minute, now)
	if err == nil {
		t.Fatal("expired JWT passed preflight")
	}
}

func TestOutputRedactsAuthorizationValues(t *testing.T) {
	const secret = "secret-token-value"
	var output bytes.Buffer
	result := smokeResult{
		Command: "responses",
		Outcome: outcomeExternalAuthBlocked,
		Error:   "request rejected: Authorization: Bearer " + secret,
	}
	if err := writeResult(&output, result, secret); err != nil {
		t.Fatalf("write result: %v", err)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("result leaked authorization value: %s", output.String())
	}
	if !strings.Contains(output.String(), "[REDACTED]") {
		t.Fatalf("result did not include redaction marker: %s", output.String())
	}
}
