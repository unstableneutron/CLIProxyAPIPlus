package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestBuildCodexWebsocketRequestBodyStripsTokenLimitsOnlyForChatGPTBackend(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":[],"max_output_tokens":123,"max_completion_tokens":456,"max_tokens":789}`)

	chatgptPayload := buildCodexWebsocketRequestBody(body, "wss://chatgpt.com/backend-api/codex/responses")

	if got := gjson.GetBytes(chatgptPayload, "type").String(); got != "response.create" {
		t.Fatalf("type = %s, want response.create", got)
	}
	for _, field := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		if gjson.GetBytes(chatgptPayload, field).Exists() {
			t.Fatalf("%s should be stripped for ChatGPT Codex websocket payload: %s", field, chatgptPayload)
		}
	}
	if got := gjson.GetBytes(chatgptPayload, "model").String(); got != "gpt-5-codex" {
		t.Fatalf("model = %s, want gpt-5-codex", got)
	}

	customPayload := buildCodexWebsocketRequestBody(body, "wss://example.test/backend-api/codex/responses")

	for _, field := range []string{"max_output_tokens", "max_completion_tokens", "max_tokens"} {
		if !gjson.GetBytes(customPayload, field).Exists() {
			t.Fatalf("%s should be preserved for non-ChatGPT websocket payload: %s", field, customPayload)
		}
	}
}

func TestCodexWebsocketsExecuteStreamRewritesPayloadModelToExecutionModel(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayload := make(chan []byte, 1)
	capturedQuery := make(chan map[string]string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("request path = %s, want /responses", r.URL.Path)
		}
		query := r.URL.Query()
		capturedQuery <- map[string]string{
			"api-version":           query.Get("api-version"),
			"deployment":            query.Get("deployment"),
			"region":                query.Get("region"),
			"azure-resource-bucket": query.Get("azure-resource-bucket"),
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade websocket: %v", err)
		}
		defer func() { _ = conn.Close() }()

		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("read upstream websocket message: %v", err)
		}
		if msgType != websocket.TextMessage {
			t.Fatalf("message type = %d, want text", msgType)
		}
		capturedPayload <- bytes.Clone(payload)

		if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp-1","output":[]}}`)); errWrite != nil {
			t.Fatalf("write created websocket message: %v", errWrite)
		}
		completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Fatalf("write completed websocket message: %v", errWrite)
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":                     "sk-test",
		"base_url":                    server.URL,
		"query:api-version":           "preview",
		"query:deployment":            "gpt-5.4-nomoderation",
		"query:region":                "global",
		"query:azure-resource-bucket": "internal-productivity",
	}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4-nomoderation",
		Payload: []byte(`{"model":"prototype/gpt-5.4","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}

	streamResult, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
	}

	select {
	case payload := <-capturedPayload:
		if got := gjson.GetBytes(payload, "model").String(); got != "gpt-5.4-nomoderation" {
			t.Fatalf("upstream model = %s, want gpt-5.4-nomoderation; payload=%s", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
	}

	select {
	case query := <-capturedQuery:
		want := map[string]string{
			"api-version":           "preview",
			"deployment":            "gpt-5.4-nomoderation",
			"region":                "global",
			"azure-resource-bucket": "internal-productivity",
		}
		for key, wantValue := range want {
			if got := query[key]; got != wantValue {
				t.Fatalf("query %s = %q, want %q", key, got, wantValue)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket query")
	}
}

func TestCodexWebsocketsExecuteReturnsOnResponseFailed(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		_, _, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		failed := []byte(`{"type":"response.failed","response":{"id":"resp-1","status":"failed","error":{"code":"server_error","message":"upstream failed"}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, failed); errWrite != nil {
			t.Errorf("write failed websocket message: %v", errWrite)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response")}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := exec.Execute(ctx, auth, req, opts)
	if err == nil {
		t.Fatal("expected response.failed error")
	}
	if got := statusCodeFromTestError(t, err); got != http.StatusInternalServerError {
		t.Fatalf("status code = %d, want %d; err=%v", got, http.StatusInternalServerError, err)
	}
	if !strings.Contains(err.Error(), "upstream failed") {
		t.Fatalf("error missing upstream message: %v", err)
	}
}

func TestCodexWebsocketsExecuteStreamPatchesCompletedOutputForDownstreamWebsocket(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		events := [][]byte{
			[]byte(`{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"idx-1","role":"assistant","content":[{"type":"output_text","text":"second"}]}}`),
			[]byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"idx-0","call_id":"call-ordered","name":"lookup","arguments":"{}","status":"completed"}}`),
			[]byte(`{"type":"response.output_item.done","item":{"type":"message","id":"fallback-1","role":"assistant","content":[{"type":"output_text","text":"fallback"}]}}`),
			[]byte(`{"type":"response.completed","response":{"id":"resp-ordered","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`),
		}
		for _, event := range events {
			if errWrite := conn.WriteMessage(websocket.TextMessage, event); errWrite != nil {
				t.Errorf("write websocket event: %v", errWrite)
				return
			}
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: "auth-downstream-patch", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":[{"type":"message","id":"msg-1","role":"user","content":"order"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	completedPayload := readCodexWebsocketCompletedPayload(t, result)
	output := gjson.GetBytes(completedPayload, "response.output").Array()
	if len(output) != 3 {
		t.Fatalf("completed output len = %d, want 3: %s", len(output), completedPayload)
	}
	for i, wantID := range []string{"idx-0", "idx-1", "fallback-1"} {
		if got := output[i].Get("id").String(); got != wantID {
			t.Fatalf("completed output[%d].id = %q, want %q: %s", i, got, wantID, completedPayload)
		}
	}
}

func TestCodexWebsocketsExecutePatchesCompletedOutputFromStreamedItems(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		done := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg-out","role":"assistant","content":[{"type":"output_text","text":"patched"}]}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, done); errWrite != nil {
			t.Errorf("write output_item.done websocket message: %v", errWrite)
			return
		}
		completed := []byte(`{"type":"response.completed","response":{"id":"resp-nonstream","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Errorf("write completed websocket message: %v", errWrite)
			return
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: "auth-nonstream-patch", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":[{"type":"message","id":"msg-1","role":"user","content":"nonstream"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.id").String(); got != "msg-out" {
		t.Fatalf("translated non-stream response missing patched output item, id=%q payload=%s", got, resp.Payload)
	}
}

func drainCodexWebsocketStreamResult(t *testing.T, result *cliproxyexecutor.StreamResult) {
	t.Helper()
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}
}

func readCodexWebsocketCompletedPayload(t *testing.T, result *cliproxyexecutor.StreamResult) []byte {
	t.Helper()
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		payload := bytes.TrimSpace(chunk.Payload)
		if gjson.GetBytes(payload, "type").String() == "response.completed" {
			return bytes.Clone(payload)
		}
	}
	t.Fatal("stream closed before response.completed")
	return nil
}

func transcriptHasItem(input []byte, itemType string, callID string) bool {
	for _, item := range gjson.ParseBytes(input).Array() {
		if item.Get("type").String() == itemType && item.Get("call_id").String() == callID {
			return true
		}
	}
	return false
}

func assertNoOrphanFunctionCallOutputs(t *testing.T, input gjson.Result) {
	t.Helper()
	calls := make(map[string]struct{})
	for _, item := range input.Array() {
		if item.Get("type").String() == "function_call" {
			callID := item.Get("call_id").String()
			if callID != "" {
				calls[callID] = struct{}{}
			}
		}
	}
	for _, item := range input.Array() {
		if item.Get("type").String() != "function_call_output" {
			continue
		}
		callID := item.Get("call_id").String()
		if _, ok := calls[callID]; !ok {
			t.Fatalf("function_call_output %s has no matching function_call in compact replay input: %s", callID, input.Raw)
		}
	}
}

func TestCodexWebsocketsExecuteStreamCompactionTriggerUsesTranscriptFallback(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	wsPayloads := make(chan []byte, 2)
	compactBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade websocket: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()
			_, payload, errRead := conn.ReadMessage()
			if errRead != nil {
				t.Errorf("read upstream websocket message: %v", errRead)
				return
			}
			wsPayloads <- bytes.Clone(payload)
			completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"message","id":"out-1","role":"assistant","content":[{"type":"output_text","text":"first"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
			if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
				t.Errorf("write completed websocket message: %v", errWrite)
			}
		case "/responses/compact":
			body, errRead := io.ReadAll(r.Body)
			if errRead != nil {
				t.Errorf("read compact body: %v", errRead)
			}
			compactBody <- bytes.Clone(body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
  "id": "resp-compact",
  "object": "response",
  "created_at": 1775555723,
  "status": "completed",
  "output": [
    {
      "type": "compaction",
      "id": "cmp-1",
      "summary": "compressed"
    }
  ],
  "usage": {"input_tokens": 2, "output_tokens": 1, "total_tokens": 3}
}`))
		default:
			t.Errorf("request path = %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: "auth-compact", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
		Stream:         true,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "codex-compact-session",
		},
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	firstResult, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":[{"type":"message","id":"msg-1","role":"user","content":"first"}]}`),
	}, opts)
	if err != nil {
		t.Fatalf("first ExecuteStream error: %v", err)
	}
	for chunk := range firstResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("first stream chunk error: %v", chunk.Err)
		}
	}

	select {
	case payload := <-wsPayloads:
		if gjson.GetBytes(payload, "input.0.id").String() != "msg-1" {
			t.Fatalf("first websocket payload = %s", payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first websocket payload")
	}

	compactResult, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","previous_response_id":"resp-1","stream":true,"stream_options":{"include_usage":true},"store":false,"include":["reasoning.encrypted_content"],"tools":[],"tool_choice":"auto","text":{"verbosity":"low"},"client_metadata":{"x":"y"},"prompt_cache_key":"cache-key","input":[{"type":"compaction_trigger"}]}`),
	}, opts)
	if err != nil {
		t.Fatalf("compact ExecuteStream error: %v", err)
	}
	var sawCompleted bool
	for chunk := range compactResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("compact stream chunk error: %v", chunk.Err)
		}
		for _, line := range bytes.Split(chunk.Payload, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			data := bytes.TrimSpace(line[len("data:"):])
			if len(data) > 0 && !json.Valid(data) {
				t.Fatalf("compact stream emitted invalid data JSON: %q", data)
			}
		}
		if strings.Contains(string(chunk.Payload), "response.completed") {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Fatal("compact fallback did not emit response.completed")
	}

	select {
	case body := <-compactBody:
		for _, field := range []string{"previous_response_id", "stream", "stream_options", "store", "include", "tools", "tool_choice", "text", "client_metadata", "prompt_cache_key"} {
			if gjson.GetBytes(body, field).Exists() {
				t.Fatalf("compact fallback leaked %s: %s", field, body)
			}
		}
		inputRaw := gjson.GetBytes(body, "input").Raw
		if strings.Contains(inputRaw, `"id":`) {
			t.Fatalf("compact fallback should strip non-portable item ids: %s", inputRaw)
		}
		for _, want := range []string{`"text":"first"`, `"role":"user"`, `"role":"assistant"`} {
			if !strings.Contains(inputRaw, want) {
				t.Fatalf("compact fallback input missing %s: %s", want, inputRaw)
			}
		}
		if strings.Contains(inputRaw, "compaction_trigger") {
			t.Fatalf("compact fallback input kept compaction_trigger: %s", inputRaw)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for compact request body")
	}

	select {
	case payload := <-wsPayloads:
		t.Fatalf("compaction trigger reached upstream websocket instead of compact fallback: %s", payload)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCodexWebsocketCompactionPayloadKeepsPendingTriggerInput(t *testing.T) {
	transcriptInput := []byte(`[{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"before"}]},{"id":"fc-1","type":"function_call","call_id":"call_pending","name":"exec_command","arguments":"{}"}]`)
	requestPayload := []byte(`{"model":"gpt-5.5","previous_response_id":"resp-1","input":[{"id":"out-1","type":"function_call_output","call_id":"call_pending","output":"done"},{"type":"compaction_trigger"},{"id":"out-2","type":"function_call_output","call_id":"call_second","output":"ok"}]}`)

	replayInput := codexWebsocketCompactionReplayInput(transcriptInput, requestPayload)
	compactPayload, err := buildCodexWebsocketCompactionPayloadWithOptions(requestPayload, replayInput, codexWebsocketCompactionPayloadOptions{})
	if err != nil {
		t.Fatalf("build compact payload: %v", err)
	}

	input := gjson.GetBytes(compactPayload, "input").Array()
	if got, want := len(input), 4; got != want {
		t.Fatalf("compact input length = %d, want %d: %s", got, want, compactPayload)
	}
	wantTypes := []string{"message", "function_call", "function_call_output", "function_call_output"}
	for index, wantType := range wantTypes {
		if got := input[index].Get("type").String(); got != wantType {
			t.Fatalf("input[%d].type = %q, want %q: %s", index, got, wantType, compactPayload)
		}
	}
	if got := input[2].Get("call_id").String(); got != "call_pending" {
		t.Fatalf("input[2].call_id = %q, want call_pending: %s", got, compactPayload)
	}
	if got := input[3].Get("call_id").String(); got != "call_second" {
		t.Fatalf("input[3].call_id = %q, want call_second: %s", got, compactPayload)
	}
	for index, item := range input {
		if item.Get("type").String() == "compaction_trigger" {
			t.Fatalf("compact input kept compaction_trigger at input[%d]: %s", index, compactPayload)
		}
		if item.Get("id").Exists() {
			t.Fatalf("compact input kept sanitized id at input[%d]: %s", index, compactPayload)
		}
	}
}

func TestSanitizeCodexWebsocketCompactionReplayPayloadPreservesEncryptedContent(t *testing.T) {
	payload := []byte(`{"model":"gpt-5.5","stream":true,"include":["reasoning.encrypted_content"],"input":[{"type":"compaction","id":"cmp-1","encrypted_content":"sealed-compaction"},{"type":"reasoning","id":"rs-1","summary":[],"encrypted_content":"sealed-reasoning"}]}`)

	got := sanitizeCodexWebsocketCompactionReplayPayload(payload)

	if gjson.GetBytes(got, "input.0.id").Exists() || gjson.GetBytes(got, "input.1.id").Exists() {
		t.Fatalf("compact replay kept item ids: %s", got)
	}
	if gotValue := gjson.GetBytes(got, "input.0.encrypted_content").String(); gotValue != "sealed-compaction" {
		t.Fatalf("compaction encrypted_content = %q, want sealed-compaction; payload=%s", gotValue, got)
	}
	if gotValue := gjson.GetBytes(got, "input.1.encrypted_content").String(); gotValue != "sealed-reasoning" {
		t.Fatalf("reasoning encrypted_content = %q, want sealed-reasoning; payload=%s", gotValue, got)
	}
}

func TestSanitizeCodexWebsocketCompactionReplayPayloadCanStripEncryptedContent(t *testing.T) {
	payload := []byte(`{"model":"gpt-5.5","stream":true,"input":[{"type":"compaction","id":"cmp-1","encrypted_content":"sealed-compaction"},{"type":"reasoning","id":"rs-1","summary":[],"encrypted_content":"sealed-reasoning"}]}`)

	got := sanitizeCodexWebsocketCompactionReplayPayloadWithOptions(payload, codexWebsocketCompactionReplaySanitizeOptions{StripEncryptedContent: true})

	if strings.Contains(string(got), "encrypted_content") {
		t.Fatalf("compact replay kept encrypted_content after strip option: %s", got)
	}
	if strings.Contains(string(got), `"id":`) {
		t.Fatalf("compact replay kept item ids: %s", got)
	}
}

func TestCodexWebsocketsCompactionRetryStripsEncryptedContentWhenUnsupported(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	compactBodies := make(chan []byte, 2)
	var compactCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade websocket: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				t.Errorf("read upstream websocket message: %v", errRead)
				return
			}
			completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"compaction","id":"cmp-state","summary":"state","encrypted_content":"sealed-state"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
			if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
				t.Errorf("write completed websocket message: %v", errWrite)
			}
		case "/responses/compact":
			body, errRead := io.ReadAll(r.Body)
			if errRead != nil {
				t.Errorf("read compact body: %v", errRead)
			}
			compactBodies <- bytes.Clone(body)
			call := compactCalls.Add(1)
			if call == 1 {
				if !strings.Contains(string(body), "encrypted_content") {
					t.Errorf("first compact body should keep encrypted_content: %s", body)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"Unknown parameter: 'input[1].encrypted_content'."}}`))
				return
			}
			if strings.Contains(string(body), "encrypted_content") {
				t.Errorf("retry compact body should strip encrypted_content: %s", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-compact","object":"response","created_at":1775555723,"status":"completed","output":[{"type":"compaction","id":"cmp-1","summary":"compressed"}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`))
		default:
			t.Errorf("request path = %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: "auth-compact", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
		Stream:         true,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "codex-compact-retry-session",
		},
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	firstResult, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":[{"type":"message","id":"msg-1","role":"user","content":"first"}]}`),
	}, opts)
	if err != nil {
		t.Fatalf("first ExecuteStream error: %v", err)
	}
	for chunk := range firstResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("first stream chunk error: %v", chunk.Err)
		}
	}

	compactResult, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","previous_response_id":"resp-1","input":[{"type":"compaction_trigger"}]}`),
	}, opts)
	if err != nil {
		t.Fatalf("compact ExecuteStream error: %v", err)
	}
	for chunk := range compactResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("compact stream chunk error: %v", chunk.Err)
		}
	}

	if got := compactCalls.Load(); got != 2 {
		t.Fatalf("compact calls = %d, want 2", got)
	}
	firstBody := <-compactBodies
	secondBody := <-compactBodies
	if strings.Contains(gjson.GetBytes(firstBody, "input").Raw, `"id":`) {
		t.Fatalf("first compact body kept item ids: %s", firstBody)
	}
	if !strings.Contains(string(firstBody), "encrypted_content") {
		t.Fatalf("first compact body missing encrypted_content: %s", firstBody)
	}
	if strings.Contains(string(secondBody), "encrypted_content") {
		t.Fatalf("second compact body kept encrypted_content: %s", secondBody)
	}
}

func TestCodexWebsocketsCompactionProvenanceChangeStripsEncryptedContent(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	compactBody := make(chan []byte, 1)
	var compactCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses":
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				t.Errorf("upgrade websocket: %v", err)
				return
			}
			defer func() { _ = conn.Close() }()
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				t.Errorf("read upstream websocket message: %v", errRead)
				return
			}
			completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[{"type":"compaction","id":"cmp-state","summary":"state","encrypted_content":"sealed-state"}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
			if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
				t.Errorf("write completed websocket message: %v", errWrite)
			}
		case "/responses/compact":
			body, errRead := io.ReadAll(r.Body)
			if errRead != nil {
				t.Errorf("read compact body: %v", errRead)
			}
			compactCalls.Add(1)
			compactBody <- bytes.Clone(body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-compact","object":"response","created_at":1775555723,"status":"completed","output":[{"type":"compaction","id":"cmp-1","summary":"compressed"}],"usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}`))
		default:
			t.Errorf("request path = %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	authA := &cliproxyauth.Auth{ID: "auth-a", Attributes: map[string]string{"api_key": "sk-test-a", "base_url": server.URL}}
	authB := &cliproxyauth.Auth{ID: "auth-b", Attributes: map[string]string{"api_key": "sk-test-b", "base_url": server.URL}}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
		Stream:         true,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "codex-compact-provenance-session",
		},
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	firstResult, err := exec.ExecuteStream(ctx, authA, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":[{"type":"message","id":"msg-1","role":"user","content":"first"}]}`),
	}, opts)
	if err != nil {
		t.Fatalf("first ExecuteStream error: %v", err)
	}
	for chunk := range firstResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("first stream chunk error: %v", chunk.Err)
		}
	}

	compactResult, err := exec.ExecuteStream(ctx, authB, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","previous_response_id":"resp-1","input":[{"type":"compaction_trigger"}]}`),
	}, opts)
	if err != nil {
		t.Fatalf("compact ExecuteStream error: %v", err)
	}
	for chunk := range compactResult.Chunks {
		if chunk.Err != nil {
			t.Fatalf("compact stream chunk error: %v", chunk.Err)
		}
	}

	if got := compactCalls.Load(); got != 1 {
		t.Fatalf("compact calls = %d, want 1", got)
	}
	body := <-compactBody
	if strings.Contains(string(body), "encrypted_content") {
		t.Fatalf("provenance-changed compact body kept encrypted_content: %s", body)
	}
	if strings.Contains(gjson.GetBytes(body, "input").Raw, `"id":`) {
		t.Fatalf("provenance-changed compact body kept item ids: %s", body)
	}
}

func TestCodexWebsocketsExecuteStreamFallsBackForCompactionTriggerWithoutTranscript(t *testing.T) {
	var compactCalls atomic.Int32
	var websocketCalls atomic.Int32
	compactBody := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses/compact":
			compactCalls.Add(1)
			body, errRead := io.ReadAll(r.Body)
			if errRead != nil {
				t.Errorf("read compact body: %v", errRead)
			}
			compactBody <- bytes.Clone(body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
  "id": "resp-compact",
  "object": "response",
  "created_at": 1775555723,
  "status": "completed",
  "output": [{"type": "compaction", "id": "cmp-1", "summary": "compressed"}],
  "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}
}`))
		case "/responses":
			websocketCalls.Add(1)
			http.Error(w, "unexpected upstream websocket call", http.StatusTeapot)
		default:
			t.Errorf("request path = %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: "auth-compact-missing-context", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
		Stream:         true,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "codex-compact-empty-session",
		},
	}

	result, err := exec.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","previous_response_id":"resp-1","stream":true,"input":[{"type":"message","id":"msg-pending","role":"user","content":"pending"},{"type":"compaction_trigger"}]}`),
	}, opts)
	if err != nil {
		t.Fatalf("ExecuteStream error: %v", err)
	}
	if result == nil {
		t.Fatal("ExecuteStream result = nil")
	}
	var sawCompleted bool
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		if strings.Contains(string(chunk.Payload), "response.completed") {
			sawCompleted = true
		}
	}
	if !sawCompleted {
		t.Fatal("fallback compact stream did not emit response.completed")
	}

	select {
	case body := <-compactBody:
		if xaiInputHasItemType(body, "compaction_trigger") {
			t.Fatalf("compact fallback kept compaction_trigger: %s", body)
		}
		if gjson.GetBytes(body, "previous_response_id").Exists() {
			t.Fatalf("compact fallback kept previous_response_id: %s", body)
		}
		if gjson.GetBytes(body, "stream").Exists() {
			t.Fatalf("compact fallback kept stream: %s", body)
		}
		if strings.Contains(gjson.GetBytes(body, "input").Raw, `"id":`) {
			t.Fatalf("compact fallback kept non-portable item ids: %s", body)
		}
		if got := gjson.GetBytes(body, "input.0.content").String(); got != "pending" {
			t.Fatalf("compact fallback input.0.content = %q, want pending; body=%s", got, body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for compact request body")
	}
	if got := compactCalls.Load(); got != 1 {
		t.Fatalf("compact calls = %d, want 1", got)
	}
	if got := websocketCalls.Load(); got != 0 {
		t.Fatalf("websocket calls = %d, want 0", got)
	}
}

func TestCodexWebsocketsExecuteStreamStopsOnResponseFailed(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()

		if _, _, errRead := conn.ReadMessage(); errRead != nil {
			t.Errorf("read upstream websocket message: %v", errRead)
			return
		}
		failed := []byte(`{"type":"response.failed","response":{"id":"resp-1","status":"failed","error":{"code":"server_error","message":"upstream failed"}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, failed); errWrite != nil {
			t.Errorf("write failed websocket message: %v", errWrite)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: "auth-1", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":"hello"}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Stream: true}

	streamResult, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var gotFailed bool
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case chunk, ok := <-streamResult.Chunks:
			if !ok {
				if !gotFailed {
					t.Fatal("stream closed without forwarding response.failed")
				}
				return
			}
			if chunk.Err != nil {
				t.Fatalf("unexpected stream chunk error: %v", chunk.Err)
			}
			if strings.Contains(string(chunk.Payload), "response.failed") {
				gotFailed = true
			}
		case <-timer.C:
			t.Fatal("timed out waiting for stream to close after response.failed")
		}
	}
}

func TestCodexWebsocketHeartbeatInvalidatesSilentUpstream(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	serverDone := make(chan struct{})
	serverConnCh := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		serverConnCh <- conn
		<-serverDone
		_ = conn.Close()
	}))
	defer server.Close()
	defer close(serverDone)

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	select {
	case serverConn := <-serverConnCh:
		defer func() { _ = serverConn.Close() }()
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server websocket")
	}

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sessionID := "heartbeat-session"
	disconnectCh := exec.UpstreamDisconnectChan(sessionID)
	sess := exec.getOrCreateSession(sessionID)
	sess.connMu.Lock()
	sess.conn = conn
	sess.authID = "auth-1"
	sess.wsURL = wsURL
	sess.readerConn = conn
	sess.connMu.Unlock()

	sess.configureConnWithTimings(conn, 75*time.Millisecond, 20*time.Millisecond)
	go exec.readUpstreamLoopWithPongWait(sess, conn, 75*time.Millisecond)
	go exec.pingUpstreamLoopWithTimings(sess, conn, 10*time.Millisecond, 20*time.Millisecond)

	select {
	case errRead, ok := <-disconnectCh:
		if !ok {
			t.Fatal("expected disconnect channel to deliver timeout error before closing")
		}
		if errRead == nil || !isCodexWebsocketTimeoutError(errRead) {
			t.Fatalf("disconnect error = %v, want timeout", errRead)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for heartbeat disconnect")
	}

	if sess.isCurrentConn(conn) {
		t.Fatal("expected silent connection to be invalidated")
	}
}

func TestNewProxyAwareWebsocketDialerUsesCodexUTLSForDirectWSS(t *testing.T) {
	t.Parallel()

	dialer := newProxyAwareWebsocketDialer(
		&config.Config{},
		&cliproxyauth.Auth{Provider: "codex"},
	)

	if dialer.NetDialTLSContext == nil {
		t.Fatal("expected codex websocket dialer to install uTLS NetDialTLSContext")
	}
	if dialer.Proxy != nil {
		t.Fatal("expected codex websocket uTLS dialer to bypass gorilla proxy wrapping")
	}
}

func TestNewProxyAwareWebsocketDialerUsesCodexUTLSWithConfiguredProxy(t *testing.T) {
	t.Parallel()

	dialer := newProxyAwareWebsocketDialer(
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://127.0.0.1:8080"}},
		&cliproxyauth.Auth{Provider: "codex"},
	)

	if dialer.NetDialTLSContext == nil {
		t.Fatal("expected codex websocket dialer to install uTLS NetDialTLSContext with configured proxy")
	}
	if dialer.Proxy != nil {
		t.Fatal("expected codex websocket uTLS dialer to own proxy tunneling")
	}
}

func TestNewProxyAwareWebsocketDialerLeavesNonCodexTLSStandard(t *testing.T) {
	t.Parallel()

	dialer := newProxyAwareWebsocketDialer(
		&config.Config{},
		&cliproxyauth.Auth{Provider: "openai"},
	)

	if dialer.NetDialTLSContext != nil {
		t.Fatal("expected non-codex websocket dialer to keep standard TLS path")
	}
	if dialer.Proxy == nil {
		t.Fatal("expected non-codex websocket dialer to keep environment proxy behavior")
	}
}
