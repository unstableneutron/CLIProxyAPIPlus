package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorExecute_NonEmptyCompletionOutputHydratesMissingItemID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.output_item.done","item":{"id":"fc_123","type":"function_call","call_id":"call_123","name":"weather","arguments":"{}"},"output_index":0}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.output_item.done","item":{"id":"fc_done_existing","type":"function_call","call_id":"call_existing","name":"other","arguments":"{}"},"output_index":1}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","output":[{"id":null,"type":"function_call","call_id":"call_123","name":"weather-terminal","arguments":"{}"},{"id":"fc_existing","type":"function_call","call_id":"call_existing","name":"preserved","arguments":"{}"}]}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","input":"What is the weather?"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	if got := gjson.GetBytes(resp.Payload, "output.0.id").String(); got != "fc_123" {
		t.Fatalf("output[0].id = %q, want %q; payload=%s", got, "fc_123", resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.name").String(); got != "weather-terminal" {
		t.Fatalf("output[0].name = %q, want terminal value; payload=%s", got, resp.Payload)
	}
	if got := gjson.GetBytes(resp.Payload, "output.1.id").String(); got != "fc_existing" {
		t.Fatalf("output[1].id = %q, want existing value; payload=%s", got, resp.Payload)
	}
}

func TestCodexExecutorExecute_EmptyStreamCompletionOutputUsesOutputItemDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1775555723,\"status\":\"completed\",\"model\":\"gpt-5.4-mini-2026-03-17\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":28,\"total_tokens\":36}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","messages":[{"role":"user","content":"Say ok"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}

	gotContent := gjson.GetBytes(resp.Payload, "choices.0.message.content").String()
	if gotContent != "ok" {
		t.Fatalf("choices.0.message.content = %q, want %q; payload=%s", gotContent, "ok", string(resp.Payload))
	}
}

func TestCodexExecutorExecuteSurfacesTerminalStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}` + "\n\n"))
		_, _ = w.Write([]byte("event: error\n"))
		_, _ = w.Write([]byte(`data: {"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","param":"input"},"sequence_number":2}` + "\n\n"))
		_, _ = w.Write([]byte("event: response.failed\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err == nil {
		t.Fatal("expected terminal stream error, got nil")
	}
	if got := statusCodeFromTestError(t, err); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusBadRequest, err)
	}
	assertCodexErrorCode(t, err.Error(), "invalid_request_error", "context_too_large")
	if !strings.Contains(err.Error(), "Your input exceeds the context window") {
		t.Fatalf("error message missing upstream context text: %v", err)
	}
}

func TestCodexExecutorExecuteIncompleteResponseIsSuccessful(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.incomplete","response":{"id":"resp_1","model":"gpt-5.5","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "stop_reason").String(); got != "max_tokens" {
		t.Fatalf("stop_reason = %q, want %q; payload=%s", got, "max_tokens", resp.Payload)
	}
}

func TestCodexExecutorExecuteExplicitTerminalFailureIsNotRequestScoped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"error","error":{"type":"invalid_request_error","code":"invalid_value","message":"Invalid input."}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err == nil {
		t.Fatal("expected explicit terminal failure, got nil")
	}
	if got := statusCodeFromTestError(t, err); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusBadRequest, err)
	}
	assertNotRequestScopedTestError(t, err)
}

func TestCodexExecutorExecuteMissingCompletionIsRequestScoped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\"}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err == nil {
		t.Fatal("expected missing-completion error, got nil")
	}
	if got := statusCodeFromTestError(t, err); got != http.StatusRequestTimeout {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusRequestTimeout, err)
	}
	assertRequestScopedTestError(t, err)
}

func TestCodexExecutorExecuteStreamMissingCompletionIsRequestScoped(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\"}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("expected missing-completion stream error, got nil")
	}
	if got := statusCodeFromTestError(t, streamErr); got != http.StatusRequestTimeout {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusRequestTimeout, streamErr)
	}
	assertRequestScopedTestError(t, streamErr)
}

func TestCodexExecutorExecuteStreamExplicitTerminalFailureIsNotSuccessful(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\"}}\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"error","error":{"type":"invalid_request_error","code":"invalid_value","message":"Invalid input."}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("expected explicit terminal stream error, got nil")
	}
	if got := statusCodeFromTestError(t, streamErr); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusBadRequest, streamErr)
	}
	assertNotRequestScopedTestError(t, streamErr)
}

func TestCodexExecutorExecuteStreamSignalsActivityOnceBeforeDataPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte(": keep-alive\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.in_progress","response":{"id":"resp_1","model":"gpt-5.5"}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.5","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	activityCount := 0
	ctx := cliproxyexecutor.WithStreamActivityCallback(context.Background(), func() { activityCount++ })
	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	dataPayloadSeen := false
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		if chunk.Bootstrap {
			t.Fatal("activity committed the auth stream with a bootstrap marker")
		}
		payload := bytes.TrimSpace(chunk.Payload)
		if bytes.HasPrefix(payload, dataTag) {
			if activityCount == 0 {
				t.Fatal("data payload emitted before activity callback")
			}
			dataPayloadSeen = true
		}
	}
	if activityCount != 1 {
		t.Fatalf("activity callback count = %d, want 1", activityCount)
	}
	if !dataPayloadSeen {
		t.Fatal("expected downstream data payload")
	}
}

func TestCodexExecutorExecuteStreamImmediateTerminalErrorDoesNotBootstrap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: error\n"))
		_, _ = w.Write([]byte(`data: {"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Rate limit reached."}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	activityCount := 0
	ctx := cliproxyexecutor.WithStreamActivityCallback(context.Background(), func() { activityCount++ })
	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Bootstrap {
			t.Fatal("bootstrap marker emitted before immediate terminal error")
		}
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("expected terminal stream error")
	}
	if activityCount != 0 {
		t.Fatalf("activity callback count = %d, want 0 before immediate terminal error", activityCount)
	}
	if got := statusCodeFromTestError(t, streamErr); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusTooManyRequests, streamErr)
	}
}

func TestCodexExecutorExecuteStreamContinueFoldSignalsActivityOnceBeforeDataPayload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte(": keep-alive\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.in_progress","response":{"id":"resp_1","model":"gpt-5.5"}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","model":"gpt-5.5","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2,"output_tokens_details":{"reasoning_tokens":0}}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{Codex: config.CodexConfig{ContinueThinking: config.CodexContinueThinking{Enabled: true}}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	activityCount := 0
	ctx := cliproxyexecutor.WithStreamActivityCallback(context.Background(), func() { activityCount++ })
	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	dataPayloadSeen := false
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		if chunk.Bootstrap {
			t.Fatal("activity committed the auth stream with a bootstrap marker")
		}
		payload := bytes.TrimSpace(chunk.Payload)
		if bytes.HasPrefix(payload, dataTag) {
			if activityCount == 0 {
				t.Fatal("data payload emitted before activity callback")
			}
			dataPayloadSeen = true
		}
	}
	if activityCount != 1 {
		t.Fatalf("activity callback count = %d, want 1", activityCount)
	}
	if !dataPayloadSeen {
		t.Fatal("expected downstream data payload")
	}
}

func TestCodexExecutorExecuteStreamContinueFoldImmediateGenericTerminalErrorDoesNotBootstrap(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: error\n"))
		_, _ = w.Write([]byte(`data: {"type":"error","error":{"type":"upstream_error","status_code":429,"message":"Try again later."}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{Codex: config.CodexConfig{ContinueThinking: config.CodexContinueThinking{Enabled: true}}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	activityCount := 0
	ctx := cliproxyexecutor.WithStreamActivityCallback(context.Background(), func() { activityCount++ })
	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Bootstrap {
			t.Fatal("bootstrap marker emitted before immediate terminal error")
		}
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("expected terminal stream error")
	}
	if activityCount != 0 {
		t.Fatalf("activity callback count = %d, want 0 before immediate terminal error", activityCount)
	}
	if got := statusCodeFromTestError(t, streamErr); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusTooManyRequests, streamErr)
	}
}

func TestCodexExecutorContinueFoldMetadataPrefixedGenericTerminalErrorFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Rate limit reached."}}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{Codex: config.CodexConfig{ContinueThinking: config.CodexContinueThinking{Enabled: true}}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	activityCount := 0
	ctx := cliproxyexecutor.WithStreamActivityCallback(context.Background(), func() { activityCount++ })
	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Bootstrap {
			t.Fatal("metadata activity committed the auth stream")
		}
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("expected metadata-prefixed terminal stream error")
	}
	if activityCount != 1 {
		t.Fatalf("activity callback count = %d, want 1", activityCount)
	}
	if got := statusCodeFromTestError(t, streamErr); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusTooManyRequests, streamErr)
	}
}

func TestCodexExecutorMetadataPrefixedEmptyCompletionRotatesAuth(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
	}{
		{name: "normal", cfg: &config.Config{}},
		{name: "continue fold", cfg: &config.Config{Codex: config.CodexConfig{ContinueThinking: config.CodexContinueThinking{Enabled: true}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				writeEvent := func(eventType, payload string) {
					_, _ = io.WriteString(w, "event: "+eventType+"\n")
					_, _ = io.WriteString(w, "data: "+payload+"\n\n")
				}
				requestNumber := requests.Add(1)
				writeEvent("response.queued", `{"type":"response.queued","response":{"id":"resp_1","status":"queued","model":"gpt-5.5"}}`)
				if requestNumber == 1 {
					writeEvent("response.completed", `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1,"output_tokens_details":{"reasoning_tokens":0}}}}`)
					return
				}
				writeEvent("response.created", `{"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}`)
				writeEvent("response.in_progress", `{"type":"response.in_progress","response":{"id":"resp_1","model":"gpt-5.5"}}`)
				writeEvent("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hello"}`)
				writeEvent("response.completed", `{"type":"response.completed","response":{"id":"resp_2","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2,"output_tokens_details":{"reasoning_tokens":0}}}}`)
			}))
			defer server.Close()

			manager := cliproxyauth.NewManager(nil, nil, nil)
			manager.SetRetryConfig(0, 0, 0)
			manager.RegisterExecutor(NewCodexExecutor(tt.cfg))
			model := "codex-empty-activity-" + uuid.NewString()
			for i := 0; i < 2; i++ {
				token := "test-" + uuid.NewString()
				auth := &cliproxyauth.Auth{
					ID:       "codex-empty-activity-auth-" + uuid.NewString(),
					Provider: "codex",
					Attributes: map[string]string{
						"auth_kind": "oauth",
						"base_url":  server.URL,
						"api_key":   token,
					},
					Metadata: map[string]any{
						"access_token":  token,
						"refresh_token": "refresh-" + token,
						"request_retry": float64(0),
					},
				}
				registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
				if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
					t.Fatalf("Register() error = %v", errRegister)
				}
			}

			var activityCount atomic.Int32
			ctx := cliproxyexecutor.WithStreamActivityCallback(context.Background(), func() { activityCount.Add(1) })
			result, err := manager.ExecuteStream(ctx, []string{"codex"}, cliproxyexecutor.Request{
				Model:   model,
				Payload: []byte(`{"model":"gpt-5.5","input":"hello","reasoning":{"effort":"high"}}`),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
			if err != nil {
				t.Fatalf("ExecuteStream error: %v", err)
			}
			var payload strings.Builder
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("stream error: %v", chunk.Err)
				}
				if chunk.Bootstrap {
					t.Fatal("metadata activity committed the empty auth attempt")
				}
				payload.Write(chunk.Payload)
			}
			if got := requests.Load(); got != 2 {
				t.Fatalf("upstream request count = %d, want 2 after empty-completion failover; payload=%q", got, payload.String())
			}
			if got := activityCount.Load(); got != 2 {
				t.Fatalf("activity callback count = %d, want one per auth attempt", got)
			}
			if !strings.Contains(payload.String(), "hello") {
				t.Fatalf("final stream payload = %q, want content from second auth", payload.String())
			}
			if !strings.Contains(payload.String(), "event: response.created\ndata:") || strings.Contains(payload.String(), "response.createddata:") {
				t.Fatalf("final stream payload lost canonical SSE field boundary: %q", payload.String())
			}
		})
	}
}

func TestCodexExecutorIncompleteEventPreambleDoesNotSignalActivity(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Config
	}{
		{name: "normal", cfg: &config.Config{}},
		{name: "continue fold", cfg: &config.Config{Codex: config.CodexConfig{ContinueThinking: config.CodexContinueThinking{Enabled: true}}}},
	}
	for _, tt := range tests {
		for _, eventType := range []string{"response.created", "error", "response.unknown", "not-a-response-event"} {
			t.Run(tt.name+"/"+eventType, func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: "+eventType+"\n")
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}))
				defer server.Close()

				executor := NewCodexExecutor(tt.cfg)
				auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
				ctx, cancel := context.WithCancel(context.Background())
				var activityCount atomic.Int32
				ctx = cliproxyexecutor.WithStreamActivityCallback(ctx, func() { activityCount.Add(1) })
				result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
					Model:   "gpt-5.5",
					Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
				}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
				if err != nil {
					cancel()
					t.Fatalf("ExecuteStream error: %v", err)
				}

				select {
				case chunk := <-result.Chunks:
					if !strings.Contains(string(chunk.Payload), "event: "+eventType) {
						cancel()
						t.Fatalf("first chunk = %q, want incomplete event preamble", chunk.Payload)
					}
				case <-time.After(time.Second):
					cancel()
					t.Fatal("incomplete event preamble was not emitted")
				}
				select {
				case <-time.After(25 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal("stream context ended before cancellation")
				}
				if got := activityCount.Load(); got != 0 {
					cancel()
					t.Fatalf("incomplete event preamble activity count = %d, want 0", got)
				}
				cancel()
				for range result.Chunks {
				}
			})
		}
	}
}

func TestCodexExecutorContinueFoldSignalsActivityOnceAcrossMultipleRounds(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestNumber := requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","status":"in_progress"}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"reasoning","id":"rs_1"}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.output_item.done","sequence_number":2,"output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc"}}` + "\n\n"))
		if requestNumber <= 2 {
			_, _ = w.Write([]byte(`data: {"type":"response.completed","sequence_number":3,"response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":516,"total_tokens":517,"output_tokens_details":{"reasoning_tokens":516}}}}` + "\n\n"))
			return
		}
		_, _ = w.Write([]byte(`data: {"type":"response.output_item.added","sequence_number":3,"output_index":1,"item":{"type":"message","id":"msg_1"}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.output_item.done","sequence_number":4,"output_index":1,"item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"final"}]}}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.completed","sequence_number":5,"response":{"id":"resp_3","status":"completed","usage":{"input_tokens":1,"output_tokens":10,"total_tokens":11,"output_tokens_details":{"reasoning_tokens":8}}}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{Codex: config.CodexConfig{ContinueThinking: config.CodexContinueThinking{Enabled: true}}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	var activityCount atomic.Int32
	ctx := cliproxyexecutor.WithStreamActivityCallback(context.Background(), func() { activityCount.Add(1) })
	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello","reasoning":{"effort":"high"}}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	var payload strings.Builder
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		if chunk.Bootstrap {
			t.Fatal("activity committed the auth stream with a bootstrap marker")
		}
		payload.Write(chunk.Payload)
	}
	if got := requests.Load(); got != 3 {
		t.Fatalf("continuation request count = %d, want 3", got)
	}
	if got := activityCount.Load(); got != 1 {
		t.Fatalf("activity callback count = %d, want 1 across all continuation rounds", got)
	}
	if !strings.Contains(payload.String(), "final") {
		t.Fatalf("folded stream payload = %q, want final content", payload.String())
	}
}

func TestCodexExecutorContinueFoldHiddenTerminalFailuresFlushRetainedDraft(t *testing.T) {
	tests := []struct {
		name          string
		beforeFailure []string
		event         string
	}{
		{name: "generic", event: `{"type":"response.failed","sequence_number":0,"response":{"id":"resp_2","status":"failed","error":{"type":"upstream_error","code":"unknown","message":"hidden continuation failed"}}}`},
		{name: "context length", event: `{"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"context length exceeded"}}`},
		{name: "usage limit", event: `{"type":"error","error":{"type":"usage_limit_reached","message":"You've hit your usage limit.","resets_in_seconds":300}}`},
		{name: "model capacity", event: `{"type":"response.failed","response":{"id":"resp_2","status":"failed","error":{"type":"server_error","message":"Selected model is at capacity. Please try a different model."}}}`},
		{name: "invalid signature", event: `{"type":"response.failed","response":{"id":"resp_2","status":"failed","error":{"type":"invalid_request_error","code":"invalid_request_error","message":"Invalid signature in thinking block"}}}`},
		{
			name: "generic after message item",
			beforeFailure: []string{
				`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"type":"message","id":"msg_hidden","status":"in_progress"}}`,
				`{"type":"response.output_item.done","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg_hidden","status":"completed","content":[{"type":"output_text","text":"discarded hidden draft"}]}}`,
			},
			event: `{"type":"response.failed","sequence_number":2,"response":{"id":"resp_2","status":"failed","error":{"type":"upstream_error","code":"unknown","message":"hidden continuation failed"}}}`,
		},
		{
			name: "classified after reasoning item",
			beforeFailure: []string{
				`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"type":"reasoning","id":"rs_hidden","status":"in_progress"}}`,
				`{"type":"response.output_item.done","sequence_number":1,"output_index":0,"item":{"type":"reasoning","id":"rs_hidden","status":"completed","encrypted_content":"hidden"}}`,
			},
			event: `{"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"context length exceeded"}}`,
		},
		{
			name: "incomplete after message item",
			beforeFailure: []string{
				`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"type":"message","id":"msg_hidden","status":"in_progress"}}`,
				`{"type":"response.output_item.done","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg_hidden","status":"completed","content":[{"type":"output_text","text":"discarded hidden draft"}]}}`,
			},
			event: `{"type":"response.incomplete","sequence_number":2,"response":{"id":"resp_2","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		},
		{
			name: "cancelled after message item",
			beforeFailure: []string{
				`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"type":"message","id":"msg_hidden","status":"in_progress"}}`,
				`{"type":"response.output_item.done","sequence_number":1,"output_index":0,"item":{"type":"message","id":"msg_hidden","status":"completed","content":[{"type":"output_text","text":"discarded hidden draft"}]}}`,
			},
			event: `{"type":"response.cancelled","sequence_number":2,"response":{"id":"resp_2","status":"cancelled","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestNumber := requests.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if requestNumber == 1 {
					_, _ = w.Write([]byte(`data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","status":"in_progress"}}` + "\n\n"))
					_, _ = w.Write([]byte(`data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"reasoning","id":"rs_1"}}` + "\n\n"))
					_, _ = w.Write([]byte(`data: {"type":"response.output_item.done","sequence_number":2,"output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc"}}` + "\n\n"))
					_, _ = w.Write([]byte(`data: {"type":"response.output_item.added","sequence_number":3,"output_index":1,"item":{"type":"message","id":"msg_1"}}` + "\n\n"))
					_, _ = w.Write([]byte(`data: {"type":"response.output_item.done","sequence_number":4,"output_index":1,"item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"retained draft"}]}}` + "\n\n"))
					_, _ = w.Write([]byte(`data: {"type":"response.completed","sequence_number":5,"response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":516,"total_tokens":517,"output_tokens_details":{"reasoning_tokens":516}}}}` + "\n\n"))
					return
				}
				for _, event := range tt.beforeFailure {
					_, _ = io.WriteString(w, "data: "+event+"\n\n")
				}
				_, _ = io.WriteString(w, "data: "+tt.event+"\n\n")
			}))
			defer server.Close()

			executor := NewCodexExecutor(&config.Config{Codex: config.CodexConfig{ContinueThinking: config.CodexContinueThinking{Enabled: true}}})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
			var activityCount atomic.Int32
			ctx := cliproxyexecutor.WithStreamActivityCallback(context.Background(), func() { activityCount.Add(1) })
			result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
				Model:   "gpt-5.5",
				Payload: []byte(`{"model":"gpt-5.5","input":"hello","reasoning":{"effort":"high"}}`),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true})
			if err != nil {
				t.Fatalf("ExecuteStream error: %v", err)
			}
			var payload strings.Builder
			for chunk := range result.Chunks {
				if chunk.Err != nil {
					t.Fatalf("hidden continuation error leaked downstream: %v", chunk.Err)
				}
				payload.Write(chunk.Payload)
			}
			if got := requests.Load(); got != 2 {
				t.Fatalf("continuation request count = %d, want 2", got)
			}
			if got := activityCount.Load(); got != 1 {
				t.Fatalf("activity callback count = %d, want 1", got)
			}
			if !strings.Contains(payload.String(), "retained draft") || !strings.Contains(payload.String(), "response.completed") {
				t.Fatalf("fallback payload = %q, want retained draft completion", payload.String())
			}
			if strings.Contains(payload.String(), "discarded hidden draft") {
				t.Fatalf("fallback payload exposed hidden continuation output: %q", payload.String())
			}
		})
	}
}

func TestCodexAutoExecutorHTTPFallbackForwardsSequentialCutoffReasoningSummaryDelivery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request body: %v", errRead)
			return
		}
		if gjson.GetBytes(body, "stream_options.include_usage").Exists() {
			t.Errorf("unsupported stream option was forwarded: %s", body)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		if delivery := gjson.GetBytes(body, "stream_options.reasoning_summary_delivery").String(); delivery == "sequential_cutoff" {
			_, _ = w.Write([]byte(`data: {"type":"response.reasoning_summary_text.done","item_id":"rs_1","summary_index":0,"text":"Checking"}` + "\n\n"))
		} else {
			_, _ = w.Write([]byte(`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","summary_index":0,"delta":"Checking"}` + "\n\n"))
		}
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"id":"resp_1","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexAutoExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}
	result, err := executor.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.6-sol",
		Payload: []byte(`{"model":"gpt-5.6-sol","input":"hello","reasoning":{"summary":"detailed"},"stream_options":{"reasoning_summary_delivery":"sequential_cutoff","include_usage":true}}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
		Stream:         true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var output bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream error: %v", chunk.Err)
		}
		output.Write(chunk.Payload)
	}
	if !strings.Contains(output.String(), `"type":"response.reasoning_summary_text.done"`) {
		t.Fatalf("missing sequential-cutoff summary event; output=%s", output.String())
	}
}

func TestCodexExecutorTransportFailureBeforeTerminalIsRequestScoped(t *testing.T) {
	tests := []struct {
		name   string
		stream bool
	}{
		{name: "non-streaming"},
		{name: "streaming", stream: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			created := []byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\"}}\n\n")
			ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"text/event-stream"}},
					Body:       io.NopCloser(io.MultiReader(bytes.NewReader(created), unexpectedEOFReader{})),
					Request:    req,
				}, nil
			}))

			executor := NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url": "http://codex.test",
				"api_key":  "test",
			}}
			req := cliproxyexecutor.Request{Model: "gpt-5.5", Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: tc.stream}

			var terminalErr error
			if tc.stream {
				result, errStream := executor.ExecuteStream(ctx, auth, req, opts)
				if errStream != nil {
					t.Fatalf("ExecuteStream error: %v", errStream)
				}
				for chunk := range result.Chunks {
					if chunk.Err != nil {
						terminalErr = chunk.Err
					}
				}
			} else {
				_, terminalErr = executor.Execute(ctx, auth, req, opts)
			}
			if terminalErr == nil {
				t.Fatal("expected transport failure before terminal event")
			}
			if got := statusCodeFromTestError(t, terminalErr); got != http.StatusRequestTimeout {
				t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusRequestTimeout, terminalErr)
			}
			assertRequestScopedTestError(t, terminalErr)
		})
	}
}

func TestCodexExecutorExecuteIgnoresTransportErrorAfterCompletion(t *testing.T) {
	completed := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(io.MultiReader(bytes.NewReader(completed), unexpectedEOFReader{})),
			Request:    req,
		}, nil
	}))

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": "http://codex.test",
		"api_key":  "test",
	}}

	resp, err := executor.Execute(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("unexpected error after response.completed: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "id").String(); got != "resp_1" {
		t.Fatalf("response id = %q, want resp_1; payload=%s", got, resp.Payload)
	}
}

func TestCodexExecutorExecuteStreamIgnoresTransportErrorAfterCompletion(t *testing.T) {
	completed := []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5.5\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(io.MultiReader(bytes.NewReader(completed), unexpectedEOFReader{})),
			Request:    req,
		}, nil
	}))

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": "http://codex.test",
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr != nil {
		t.Fatalf("unexpected error after response.completed: %v", streamErr)
	}
}

func TestCodexExecutorExecuteStreamSurfacesTerminalStreamError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.created\n"))
		_, _ = w.Write([]byte(`data: {"type":"response.created","response":{"id":"resp_1","model":"gpt-5.5"}}` + "\n\n"))
		_, _ = w.Write([]byte("event: error\n"))
		_, _ = w.Write([]byte(`data: {"type":"error","error":{"type":"invalid_request_error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","param":"input"},"sequence_number":2}` + "\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
			break
		}
	}
	if streamErr == nil {
		t.Fatal("missing stream terminal error")
	}
	if got := statusCodeFromTestError(t, streamErr); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusBadRequest, streamErr)
	}
	assertCodexErrorCode(t, streamErr.Error(), "invalid_request_error", "context_too_large")
}

func TestCodexTerminalStreamContextLengthErrFromResponseFailed(t *testing.T) {
	err, ok := codexTerminalStreamContextLengthErr([]byte(`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again."}}}`))
	if !ok {
		t.Fatal("expected context length terminal error")
	}
	if got := statusCodeFromTestError(t, err); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusBadRequest, err)
	}
	assertCodexErrorCode(t, err.Error(), "invalid_request_error", "context_too_large")
}

func TestCodexTerminalStreamContextLengthErrFromTopLevelError(t *testing.T) {
	err, ok := codexTerminalStreamContextLengthErr([]byte(`{"type":"error","code":"context_length_exceeded","message":"Your input exceeds the context window of this model. Please adjust your input and try again.","sequence_number":2}`))
	if !ok {
		t.Fatal("expected top-level context length terminal error")
	}
	if got := statusCodeFromTestError(t, err); got != http.StatusBadRequest {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusBadRequest, err)
	}
	assertCodexErrorCode(t, err.Error(), "invalid_request_error", "context_too_large")
	if !strings.Contains(err.Error(), "Your input exceeds the context window") {
		t.Fatalf("error message missing upstream context text: %v", err)
	}
}

func TestCodexTerminalStreamContextLengthErrIgnoresOtherTerminalErrors(t *testing.T) {
	_, ok := codexTerminalStreamContextLengthErr([]byte(`{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Rate limit reached."}}`))
	if ok {
		t.Fatal("rate limit terminal error should not be handled by context length fix")
	}
}

func TestCodexTerminalStreamErrIgnoresRateLimitTerminalErrors(t *testing.T) {
	_, _, ok := codexTerminalStreamErr([]byte(`{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Rate limit reached."}}`))
	if ok {
		t.Fatal("rate limit terminal error should not be handled by replay terminal error path")
	}
}

func TestCodexTerminalFailureErrClassifiesStatus(t *testing.T) {
	tests := []struct {
		name       string
		event      string
		wantStatus int
	}{
		{
			name:       "invalid request",
			event:      `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_value","message":"Invalid input."}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "cyber policy",
			event:      `{"type":"error","error":{"type":"invalid_request","code":"cyber_policy","message":"This content was flagged for possible cybersecurity risk."}}`,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "authentication",
			event:      `{"type":"response.failed","response":{"error":{"type":"authentication_error","code":"invalid_api_key","message":"Invalid token."}}}`,
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "rate limit",
			event:      `{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"Rate limit reached."}}`,
			wantStatus: http.StatusTooManyRequests,
		},
		{
			name:       "unknown upstream failure",
			event:      `{"type":"response.failed","response":{"error":{"type":"upstream_error","code":"unknown","message":"Upstream failed."}}}`,
			wantStatus: http.StatusBadGateway,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			streamErr, _, ok := codexTerminalFailureErr([]byte(tc.event))
			if !ok {
				t.Fatal("expected terminal failure to be handled")
			}
			if got := streamErr.StatusCode(); got != tc.wantStatus {
				t.Fatalf("status code = %d, want %d; err=%v", got, tc.wantStatus, streamErr)
			}
		})
	}
}

func TestCodexTerminalStreamErrHandlesUsageLimitErrorEvent(t *testing.T) {
	streamErr, _, ok := codexTerminalStreamErr([]byte(`{"type":"error","error":{"type":"usage_limit_reached","message":"You've hit your usage limit.","resets_in_seconds":300}}`))
	if !ok {
		t.Fatal("expected usage_limit_reached terminal error to be handled")
	}
	if got := statusCodeFromTestError(t, streamErr); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
	retryAfter := streamErr.RetryAfter()
	if retryAfter == nil {
		t.Fatal("expected retryAfter from usage_limit_reached terminal error")
	}
	if *retryAfter != 300*time.Second {
		t.Fatalf("retryAfter = %v, want %v", *retryAfter, 300*time.Second)
	}
}

func TestCodexTerminalStreamErrHandlesUsageLimitResponseFailed(t *testing.T) {
	streamErr, _, ok := codexTerminalStreamErr([]byte(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"usage limit reached","resets_in_seconds":60}}}`))
	if !ok {
		t.Fatal("expected usage_limit_reached response.failed terminal error to be handled")
	}
	if got := statusCodeFromTestError(t, streamErr); got != http.StatusTooManyRequests {
		t.Fatalf("status code = %d, want %d", got, http.StatusTooManyRequests)
	}
	if streamErr.RetryAfter() == nil {
		t.Fatal("expected retryAfter from usage_limit_reached response.failed terminal error")
	}
}

func statusCodeFromTestError(t *testing.T, err error) int {
	t.Helper()

	statusErr, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error %T does not expose StatusCode(): %v", err, err)
	}
	return statusErr.StatusCode()
}

func assertRequestScopedTestError(t *testing.T, err error) {
	t.Helper()

	requestErr, ok := err.(interface{ IsRequestScoped() bool })
	if !ok {
		t.Fatalf("error %T does not expose IsRequestScoped(): %v", err, err)
	}
	if !requestErr.IsRequestScoped() {
		t.Fatalf("error %T is not request-scoped: %v", err, err)
	}
}

func assertNotRequestScopedTestError(t *testing.T, err error) {
	t.Helper()

	requestErr, ok := err.(interface{ IsRequestScoped() bool })
	if ok && requestErr.IsRequestScoped() {
		t.Fatalf("error %T is unexpectedly request-scoped: %v", err, err)
	}
}

type unexpectedEOFReader struct{}

func (unexpectedEOFReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

func TestCodexExecutorExecuteStream_EmptyStreamCompletionOutputUsesOutputItemDone(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"ok\"}]},\"output_index\":0}\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"created_at\":1775555723,\"status\":\"completed\",\"model\":\"gpt-5.4-mini-2026-03-17\",\"output\":[],\"usage\":{\"input_tokens\":8,\"output_tokens\":28,\"total_tokens\":36}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4-mini",
		Payload: []byte(`{"model":"gpt-5.4-mini","input":"Say ok"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}

	var completed []byte
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		payload := bytes.TrimSpace(chunk.Payload)
		if !bytes.HasPrefix(payload, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(payload[5:])
		if gjson.GetBytes(data, "type").String() == "response.completed" {
			completed = append([]byte(nil), data...)
		}
	}

	if len(completed) == 0 {
		t.Fatal("missing response.completed chunk")
	}

	gotContent := gjson.GetBytes(completed, "response.output.0.content.0.text").String()
	if gotContent != "ok" {
		t.Fatalf("response.output[0].content[0].text = %q, want %q; completed=%s", gotContent, "ok", string(completed))
	}
}

func TestCodexExecutorExecuteStreamPreservesPreviousResponseIDWithStateMode(t *testing.T) {
	bodyCh := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Errorf("read request body: %v", errRead)
		}
		bodyCh <- bytes.Clone(body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-next\",\"object\":\"response\",\"created_at\":1775555723,\"status\":\"completed\",\"model\":\"gpt-5.5\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","previous_response_id":"resp-prev","input":[{"type":"message","role":"user","content":"next"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
		Metadata: map[string]any{
			cliproxyexecutor.ResponsesStateModeMetadataKey: cliproxyexecutor.ResponsesStateModeProbe,
		},
	})
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}

	select {
	case body := <-bodyCh:
		if got := gjson.GetBytes(body, "previous_response_id").String(); got != "resp-prev" {
			t.Fatalf("previous_response_id = %q, want resp-prev; body=%s", got, body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for request body")
	}
}
