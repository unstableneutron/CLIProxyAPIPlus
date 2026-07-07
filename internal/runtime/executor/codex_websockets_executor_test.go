package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestBuildCodexWebsocketRequestBodyPreservesPreviousResponseID(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-1"}]}`)

	wsReqBody := buildCodexWebsocketRequestBody(body, "wss://chatgpt.com/backend-api/codex/responses")

	if got := gjson.GetBytes(wsReqBody, "type").String(); got != "response.create" {
		t.Fatalf("type = %s, want response.create", got)
	}
	if got := gjson.GetBytes(wsReqBody, "previous_response_id").String(); got != "resp-1" {
		t.Fatalf("previous_response_id = %s, want resp-1", got)
	}
	if gjson.GetBytes(wsReqBody, "input.0.id").String() != "msg-1" {
		t.Fatalf("input item id mismatch")
	}
	if got := gjson.GetBytes(wsReqBody, "type").String(); got == "response.append" {
		t.Fatalf("unexpected websocket request type: %s", got)
	}
}

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

func TestCodexWebsocketsExecutePreservesPreviousResponseIDUpstream(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayload := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("request path = %s, want /responses", r.URL.Path)
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

		completed := []byte(`{"type":"response.completed","response":{"id":"resp-2","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Fatalf("write completed websocket message: %v", errWrite)
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","previous_response_id":"resp-1","input":[{"type":"message","id":"msg-1"}]}`),
	}
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")}

	if _, err := exec.Execute(context.Background(), auth, req, opts); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	select {
	case payload := <-capturedPayload:
		if got := gjson.GetBytes(payload, "type").String(); got != "response.create" {
			t.Fatalf("upstream type = %s, want response.create; payload=%s", got, payload)
		}
		if got := gjson.GetBytes(payload, "previous_response_id").String(); got != "resp-1" {
			t.Fatalf("upstream previous_response_id = %s, want resp-1; payload=%s", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
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

func TestCodexWebsocketsExecuteStreamPassesThroughUpstreamWebsocketPayloadForDownstreamWebsocket(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	capturedPayload := make(chan []byte, 1)
	delta := []byte(`{"type":"response.output_text.delta","delta":"hello"}`)
	completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		capturedPayload <- bytes.Clone(payload)
		if errWrite := conn.WriteMessage(websocket.TextMessage, delta); errWrite != nil {
			t.Errorf("write delta websocket message: %v", errWrite)
			return
		}
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Errorf("write completed websocket message: %v", errWrite)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"prolite/gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	result, err := exec.ExecuteStream(ctx, auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	select {
	case chunk, ok := <-result.Chunks:
		if !ok {
			t.Fatal("stream closed before first chunk")
		}
		if chunk.Err != nil {
			t.Fatalf("first chunk error = %v", chunk.Err)
		}
		if !bytes.Equal(bytes.TrimSpace(chunk.Payload), delta) {
			t.Fatalf("first chunk = %q, want raw upstream websocket payload %q", chunk.Payload, delta)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for first stream chunk")
	}

	select {
	case payload := <-capturedPayload:
		if got := gjson.GetBytes(payload, "model").String(); got != "gpt-5-codex" {
			t.Fatalf("upstream model = %s, want gpt-5-codex; payload=%s", got, payload)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for upstream websocket payload")
	}
}

func TestCodexWebsocketsExecuteStreamRecordsStreamedOutputItemsForCompaction(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	const sessionID = "codex-streamed-tool-compaction-session"
	const callID = "call_streamed_tool"
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
			for turn := 0; turn < 2; turn++ {
				_, payload, errRead := conn.ReadMessage()
				if errRead != nil {
					t.Errorf("read upstream websocket message %d: %v", turn+1, errRead)
					return
				}
				wsPayloads <- bytes.Clone(payload)
				switch turn {
				case 0:
					added := []byte(`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc-streamed","call_id":"call_streamed_tool","name":"create_goal","arguments":"{}","status":"in_progress"}}`)
					if errWrite := conn.WriteMessage(websocket.TextMessage, added); errWrite != nil {
						t.Errorf("write output_item.added websocket message: %v", errWrite)
						return
					}
					done := []byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc-streamed","call_id":"call_streamed_tool","name":"create_goal","arguments":"{}","status":"completed"}}`)
					if errWrite := conn.WriteMessage(websocket.TextMessage, done); errWrite != nil {
						t.Errorf("write output_item.done websocket message: %v", errWrite)
						return
					}
					completed := []byte(`{"type":"response.completed","response":{"id":"resp-1","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`)
					if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
						t.Errorf("write first completed websocket message: %v", errWrite)
						return
					}
				case 1:
					completed := []byte(`{"type":"response.completed","response":{"id":"resp-2","output":[],"usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}}`)
					if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
						t.Errorf("write second completed websocket message: %v", errWrite)
						return
					}
				}
			}
		case "/responses/compact":
			body, errRead := io.ReadAll(r.Body)
			if errRead != nil {
				t.Errorf("read compact body: %v", errRead)
			}
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
	t.Cleanup(func() { deleteXAIWebsocketIDState(exec.idStore, sessionID) })
	auth := &cliproxyauth.Auth{ID: "auth-streamed-tool", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
		Stream:         true,
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: sessionID,
		},
	}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())

	firstResult, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","input":[{"type":"message","id":"msg-1","role":"user","content":"start"}]}`),
	}, opts)
	if err != nil {
		t.Fatalf("first ExecuteStream error: %v", err)
	}
	drainCodexWebsocketStreamResult(t, firstResult)

	state := getXAIWebsocketIDState(exec.idStore, sessionID)
	transcriptAfterCall := state.snapshotTranscriptInput()
	if !transcriptHasItem(transcriptAfterCall, "function_call", callID) {
		t.Fatalf("transcript missing streamed function_call %s after first turn: %s", callID, transcriptAfterCall)
	}

	secondResult, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","previous_response_id":"resp-1","input":[{"type":"function_call_output","call_id":"call_streamed_tool","output":"{\"ok\":true}"}]}`),
	}, opts)
	if err != nil {
		t.Fatalf("second ExecuteStream error: %v", err)
	}
	drainCodexWebsocketStreamResult(t, secondResult)

	transcriptAfterOutput := state.snapshotTranscriptInput()
	if !transcriptHasItem(transcriptAfterOutput, "function_call", callID) || !transcriptHasItem(transcriptAfterOutput, "function_call_output", callID) {
		t.Fatalf("transcript missing paired tool items for %s after second turn: %s", callID, transcriptAfterOutput)
	}

	compactResult, err := exec.ExecuteStream(ctx, auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","previous_response_id":"resp-2","input":[{"type":"compaction_trigger"}]}`),
	}, opts)
	if err != nil {
		t.Fatalf("compact ExecuteStream error: %v", err)
	}
	drainCodexWebsocketStreamResult(t, compactResult)

	select {
	case body := <-compactBody:
		input := gjson.GetBytes(body, "input")
		if !transcriptHasItem([]byte(input.Raw), "function_call", callID) {
			t.Fatalf("compact replay missing function_call %s: %s", callID, body)
		}
		if !transcriptHasItem([]byte(input.Raw), "function_call_output", callID) {
			t.Fatalf("compact replay missing function_call_output %s: %s", callID, body)
		}
		assertNoOrphanFunctionCallOutputs(t, input)
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for compact request body")
	}

	for i := 0; i < 2; i++ {
		select {
		case <-wsPayloads:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for websocket payload %d", i+1)
		}
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

func TestCodexWebsocketsExecuteStreamRejectsCompactionTriggerWithoutTranscript(t *testing.T) {
	var upstreamCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		http.Error(w, "unexpected upstream websocket call", http.StatusTeapot)
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: "auth-compact-missing-context", Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
		Stream:         true,
	}

	result, err := exec.ExecuteStream(cliproxyexecutor.WithDownstreamWebsocket(context.Background()), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5",
		Payload: []byte(`{"model":"gpt-5.5","previous_response_id":"resp-1","input":[{"type":"compaction_trigger"}]}`),
	}, opts)
	if err == nil {
		if result != nil {
			for range result.Chunks {
			}
		}
		t.Fatalf("ExecuteStream error = nil, want missing compaction context status error")
	}

	var status interface{ StatusCode() int }
	if !errors.As(err, &status) {
		t.Fatalf("ExecuteStream error type = %T, want StatusCode", err)
	}
	if got := status.StatusCode(); got != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want %d; err=%v", got, http.StatusBadRequest, err)
	}
	if got := gjson.Get(err.Error(), "error.code").String(); got != "missing_compaction_context" {
		t.Fatalf("error.code = %q, want missing_compaction_context; err=%v", got, err)
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
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

func TestCodexWebsocketsExecuteStreamMapsMessageTooBigClose(t *testing.T) {
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
		deadline := time.Now().Add(time.Second)
		closeMessage := websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "message too big")
		if errWrite := conn.WriteControl(websocket.CloseMessage, closeMessage, deadline); errWrite != nil {
			t.Errorf("write close websocket message: %v", errWrite)
			return
		}
	}))
	defer server.Close()

	exec := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "sk-test", "base_url": server.URL}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"model":"gpt-5-codex","input":[{"type":"message","role":"user","content":"hello"}]}`),
	}
	opts := cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
	}

	result, err := exec.ExecuteStream(context.Background(), auth, req, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	select {
	case chunk, ok := <-result.Chunks:
		if !ok {
			t.Fatal("stream closed before error chunk")
		}
		if chunk.Err == nil {
			t.Fatal("error chunk Err = nil, want message-too-big error")
		}
		statusErr, ok := chunk.Err.(interface{ StatusCode() int })
		if !ok {
			t.Fatalf("error type %T does not expose StatusCode", chunk.Err)
		}
		if got := statusErr.StatusCode(); got != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want %d", got, http.StatusRequestEntityTooLarge)
		}
		if got := gjson.Get(chunk.Err.Error(), "error.code").String(); got != "message_too_big" {
			t.Fatalf("error code = %q, want message_too_big; err=%v", got, chunk.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for error stream chunk")
	}
}

func TestCodexWebsocketsUpstreamDisconnectChanSignalsOnInvalidate(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			if _, _, errRead := conn.ReadMessage(); errRead != nil {
				return
			}
		}
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer func() { _ = conn.Close() }()

	exec := NewCodexWebsocketsExecutor(&config.Config{})
	sessionID := "sess-1"
	disconnectCh := exec.UpstreamDisconnectChan(sessionID)
	if disconnectCh == nil {
		t.Fatal("expected disconnect channel")
	}

	sess := exec.getOrCreateSession(sessionID)
	if sess == nil {
		t.Fatal("expected session")
	}
	sess.connMu.Lock()
	sess.conn = conn
	sess.authID = "auth-1"
	sess.wsURL = "ws://example.test/responses"
	sess.readerConn = conn
	sess.connMu.Unlock()

	upstreamErr := errors.New("upstream gone")
	exec.invalidateUpstreamConn(sess, conn, "test_invalidate", upstreamErr)

	select {
	case errRead, ok := <-disconnectCh:
		if !ok {
			t.Fatal("expected disconnect channel to deliver error before closing")
		}
		if errRead == nil || errRead.Error() != upstreamErr.Error() {
			t.Fatalf("disconnect error = %v, want %v", errRead, upstreamErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for disconnect signal")
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

func TestApplyCodexWebsocketHeadersDefaultsToCurrentResponsesBeta(t *testing.T) {
	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, nil, "", nil)

	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
	if got := headers.Get("User-Agent"); got != codexUserAgent {
		t.Fatalf("User-Agent = %s, want %s", got, codexUserAgent)
	}
	if !strings.HasPrefix(codexUserAgent, codexOriginator+"/") {
		t.Fatalf("default Codex User-Agent = %s, want prefix %s/", codexUserAgent, codexOriginator)
	}
	if !strings.HasPrefix(codexUserAgent, "codex-tui/") {
		t.Fatalf("default Codex User-Agent = %s, want codex-tui prefix", codexUserAgent)
	}
	if !strings.Contains(codexUserAgent, "(codex-tui;") {
		t.Fatalf("default Codex User-Agent = %s, want codex-tui suffix", codexUserAgent)
	}
	if got := headers.Get("Originator"); got != codexOriginator {
		t.Fatalf("Originator = %s, want %s", got, codexOriginator)
	}
	if got := headers.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty", got)
	}
	if got := headers.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
	if got := headers.Get("X-Codex-Turn-Metadata"); got != "" {
		t.Fatalf("X-Codex-Turn-Metadata = %q, want empty", got)
	}
	if got := headers.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("X-Client-Request-Id = %q, want empty", got)
	}
}

func TestApplyCodexWebsocketHeadersPassesThroughClientIdentityHeaders(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"Originator":            "Codex Desktop",
		"User-Agent":            "codex_cli_rs/0.1.0",
		"Version":               "0.115.0-alpha.27",
		"X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`,
		"X-Client-Request-Id":   "019d2233-e240-7162-992d-38df0a2a0e0d",
		"session-id":            "legacy-session",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", nil)

	if got := headers.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %s, want %s", got, "Codex Desktop")
	}
	if got := headers.Get("User-Agent"); got != "codex_cli_rs/0.1.0" {
		t.Fatalf("User-Agent = %s, want %s", got, "codex_cli_rs/0.1.0")
	}
	if got := headers.Get("Version"); got != "0.115.0-alpha.27" {
		t.Fatalf("Version = %s, want %s", got, "0.115.0-alpha.27")
	}
	if got := headers.Get("X-Codex-Turn-Metadata"); got != `{"turn_id":"turn-1"}` {
		t.Fatalf("X-Codex-Turn-Metadata = %s, want %s", got, `{"turn_id":"turn-1"}`)
	}
	if got := headers.Get("X-Client-Request-Id"); got != "019d2233-e240-7162-992d-38df0a2a0e0d" {
		t.Fatalf("X-Client-Request-Id = %s, want %s", got, "019d2233-e240-7162-992d-38df0a2a0e0d")
	}
	if got := headers["session_id"]; len(got) != 1 || got[0] != "legacy-session" {
		t.Fatalf("session_id = %#v, want [legacy-session]", got)
	}
	if got := headers.Get("Session-Id"); got != "" {
		t.Fatalf("Session-Id = %s, want empty", got)
	}
}

func TestApplyCodexWebsocketHeadersCanonicalizesLegacyUnderscoreSessionHeader(t *testing.T) {
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"Originator": "Codex Desktop",
		"User-Agent": "codex_cli_rs/0.1.0",
		"Session_id": "legacy-underscore-session",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", nil)

	if got := headers["session_id"]; len(got) != 1 || got[0] != "legacy-underscore-session" {
		t.Fatalf("session_id = %#v, want [legacy-underscore-session]", got)
	}
	if got := headers.Get("Session-Id"); got != "" {
		t.Fatalf("Session-Id = %s, want empty", got)
	}
}

func TestApplyCodexWebsocketHeadersUsesConfigDefaultsForOAuth(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "my-codex-client/1.0",
			BetaFeatures: "feature-a,feature-b",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "", cfg)

	if got := headers.Get("User-Agent"); got != "my-codex-client/1.0" {
		t.Fatalf("User-Agent = %s, want %s", got, "my-codex-client/1.0")
	}
	if got := headers.Get("x-codex-beta-features"); got != "feature-a,feature-b" {
		t.Fatalf("x-codex-beta-features = %s, want %s", got, "feature-a,feature-b")
	}
	if got := headers.Get("OpenAI-Beta"); got != codexResponsesWebsocketBetaHeaderValue {
		t.Fatalf("OpenAI-Beta = %s, want %s", got, codexResponsesWebsocketBetaHeaderValue)
	}
}

func TestApplyCodexWebsocketHeadersPrefersExistingHeadersOverClientAndConfig(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":            "client-ua",
		"X-Codex-Beta-Features": "client-beta",
	})
	headers := http.Header{}
	headers.Set("User-Agent", "existing-ua")
	headers.Set("X-Codex-Beta-Features", "existing-beta")

	got := applyCodexWebsocketHeaders(ctx, headers, auth, "", cfg)

	if gotVal := got.Get("User-Agent"); gotVal != "existing-ua" {
		t.Fatalf("User-Agent = %s, want %s", gotVal, "existing-ua")
	}
	if gotVal := got.Get("x-codex-beta-features"); gotVal != "existing-beta" {
		t.Fatalf("x-codex-beta-features = %s, want %s", gotVal, "existing-beta")
	}
}

func TestApplyCodexWebsocketHeadersConfigUserAgentOverridesClientHeader(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	ctx := contextWithGinHeaders(map[string]string{
		"User-Agent":            "client-ua",
		"X-Codex-Beta-Features": "client-beta",
	})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "", cfg)

	if got := headers.Get("User-Agent"); got != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", got, "config-ua")
	}
	if got := headers.Get("x-codex-beta-features"); got != "client-beta" {
		t.Fatalf("x-codex-beta-features = %s, want %s", got, "client-beta")
	}
}

func TestApplyCodexWebsocketHeadersIgnoresConfigForAPIKeyAuth(t *testing.T) {
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider:   "codex",
		Attributes: map[string]string{"api_key": "sk-test"},
	}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "sk-test", cfg)

	if got := headers.Get("User-Agent"); got != "" {
		t.Fatalf("User-Agent = %s, want empty", got)
	}
	if got := headers.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
	if got := headers.Get("Originator"); got != "" {
		t.Fatalf("Originator = %s, want empty", got)
	}
}

func TestApplyCodexWebsocketHeadersPreservesExplicitAPIKeyUserAgent(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"api_key": "sk-test"}}
	ctx := contextWithGinHeaders(map[string]string{"User-Agent": "api-key-client/1.0", "Originator": "explicit-origin"})

	headers := applyCodexWebsocketHeaders(ctx, http.Header{}, auth, "sk-test", nil)

	if got := headers.Get("User-Agent"); got != "api-key-client/1.0" {
		t.Fatalf("User-Agent = %s, want api-key-client/1.0", got)
	}
	if got := headers.Get("Originator"); got != "explicit-origin" {
		t.Fatalf("Originator = %s, want explicit-origin", got)
	}
}

func TestApplyCodexWebsocketHeadersUsesCanonicalAccountHeader(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"account_id": "acct-1"}}

	headers := applyCodexWebsocketHeaders(context.Background(), http.Header{}, auth, "", nil)

	if got := headerValueCaseInsensitive(headers, "ChatGPT-Account-ID"); got != "acct-1" {
		t.Fatalf("ChatGPT-Account-ID = %s, want acct-1", got)
	}
	values, ok := headers["ChatGPT-Account-ID"]
	if !ok {
		t.Fatalf("expected exact ChatGPT-Account-ID key, got %#v", headers)
	}
	if len(values) != 1 || values[0] != "acct-1" {
		t.Fatalf("ChatGPT-Account-ID values = %#v, want [acct-1]", values)
	}
}

func TestApplyCodexPromptCacheHeadersSetsSessionIDAndLegacyConversation(t *testing.T) {
	req := cliproxyexecutor.Request{Model: "gpt-5-codex", Payload: []byte(`{"prompt_cache_key":"cache-1"}`)}

	_, headers := applyCodexPromptCacheHeaders("openai-response", req, []byte(`{"model":"gpt-5-codex"}`))

	if got := headers["session_id"]; len(got) != 1 || got[0] != "cache-1" {
		t.Fatalf("session_id = %#v, want [cache-1]", got)
	}
	if got := headers.Get("Session-Id"); got != "" {
		t.Fatalf("Session-Id = %s, want empty", got)
	}
	if got := headers.Get("Conversation_id"); got != "cache-1" {
		t.Fatalf("Conversation_id = %s, want cache-1", got)
	}
}

func TestApplyCodexPromptCacheHeadersClaudeUsesClaudeCodeSessionID(t *testing.T) {
	firstReq := cliproxyexecutor.Request{
		Model: "gpt-5-codex-claude-ws-cache-session",
		Payload: []byte(`{
			"metadata":{"user_id":"{\"device_id\":\"device-a\",\"account_uuid\":\"\",\"session_id\":\"ws-cache-session-1\"}"},
			"messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]
		}`),
	}
	secondReq := cliproxyexecutor.Request{
		Model: "gpt-5-codex-claude-ws-cache-session",
		Payload: []byte(`{
			"metadata":{"user_id":"{\"device_id\":\"device-b\",\"account_uuid\":\"\",\"session_id\":\"ws-cache-session-1\"}"},
			"messages":[{"role":"user","content":[{"type":"text","text":"next"}]}]
		}`),
	}

	firstBody, firstHeaders := applyCodexPromptCacheHeaders("claude", firstReq, []byte(`{"model":"gpt-5-codex"}`))
	secondBody, secondHeaders := applyCodexPromptCacheHeaders("claude", secondReq, []byte(`{"model":"gpt-5-codex"}`))

	firstKey := gjson.GetBytes(firstBody, "prompt_cache_key").String()
	secondKey := gjson.GetBytes(secondBody, "prompt_cache_key").String()
	if firstKey == "" {
		t.Fatalf("first prompt_cache_key is empty; body=%s", string(firstBody))
	}
	if secondKey != firstKey {
		t.Fatalf("same Claude Code session_id produced different websocket prompt_cache_key: first=%q second=%q", firstKey, secondKey)
	}
	if got := firstHeaders["session_id"]; len(got) != 1 || got[0] != firstKey {
		t.Fatalf("first session_id = %#v, want [%q]", got, firstKey)
	}
	if got := secondHeaders["session_id"]; len(got) != 1 || got[0] != firstKey {
		t.Fatalf("second session_id = %#v, want [%q]", got, firstKey)
	}
}

func TestApplyCodexPromptCacheHeadersClaudeRejectsBareUserID(t *testing.T) {
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex-claude-ws-cache-bare-user",
		Payload: []byte(`{"metadata":{"user_id":"same-user-across-chats"},"messages":[{"role":"user","content":[{"type":"text","text":"first"}]}]}`),
	}

	body, headers := applyCodexPromptCacheHeaders("claude", req, []byte(`{"model":"gpt-5-codex"}`))

	if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != "" {
		t.Fatalf("bare metadata.user_id must not create websocket prompt_cache_key, got %q; body=%s", got, string(body))
	}
	if got := headers["session_id"]; len(got) != 0 {
		t.Fatalf("bare metadata.user_id must not create websocket session_id, got %#v", got)
	}
	if got := headers.Get("Session-Id"); got != "" {
		t.Fatalf("bare metadata.user_id must not create websocket Session-Id, got %q", got)
	}
	if got := headers.Get("Conversation_id"); got != "" {
		t.Fatalf("bare metadata.user_id must not create websocket Conversation_id, got %q", got)
	}
}

func TestApplyCodexWebsocketHeadersIdentityConfuseRemapsPromptCacheKey(t *testing.T) {
	cfg := &config.Config{
		Routing: config.RoutingConfig{SessionAffinity: true},
		Codex:   config.CodexConfig{IdentityConfuse: true},
	}
	auth := &cliproxyauth.Auth{ID: "auth-ws-1", Provider: "codex"}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5-codex",
		Payload: []byte(`{"prompt_cache_key":"cache-ws-1","client_metadata":{"x-codex-installation-id":"install-ws-1"}}`),
	}

	body, headers := applyCodexPromptCacheHeaders("openai-response", req, []byte(`{"model":"gpt-5-codex"}`))
	body, identityState := applyCodexIdentityConfuseBody(cfg, auth, req.Payload, body)
	ctx := contextWithGinHeaders(map[string]string{
		"X-Codex-Turn-Metadata": `{"prompt_cache_key":"cache-ws-1","turn_id":"turn-ws-1","window_id":"cache-ws-1:0"}`,
		"X-Client-Request-Id":   "client-request-1",
	})
	headers = applyCodexWebsocketHeaders(ctx, headers, auth, "oauth-token", cfg)
	applyCodexIdentityConfuseHeaders(headers, &identityState)

	expectedPromptCacheKey := codexIdentityConfuseUUID("auth-ws-1", "prompt-cache", "cache-ws-1")
	expectedTurnID := codexIdentityConfuseUUID("auth-ws-1", "turn", "turn-ws-1")
	if gotKey := gjson.GetBytes(body, "prompt_cache_key").String(); gotKey != expectedPromptCacheKey {
		t.Fatalf("prompt_cache_key = %q, want %q", gotKey, expectedPromptCacheKey)
	}
	if gotSession := headers["session_id"]; len(gotSession) != 1 || gotSession[0] != expectedPromptCacheKey {
		t.Fatalf("session_id = %#v, want [%q]", gotSession, expectedPromptCacheKey)
	}
	if gotCanonicalSession := headers.Get("Session-Id"); gotCanonicalSession != "" {
		t.Fatalf("Session-Id = %q, want empty", gotCanonicalSession)
	}
	if gotRequestID := headers.Get("X-Client-Request-Id"); gotRequestID != expectedPromptCacheKey {
		t.Fatalf("X-Client-Request-Id = %q, want %q", gotRequestID, expectedPromptCacheKey)
	}
	if gotThreadID := headers.Get("Thread-Id"); gotThreadID != expectedPromptCacheKey {
		t.Fatalf("Thread-Id = %q, want %q", gotThreadID, expectedPromptCacheKey)
	}
	if gotConversation := headers.Get("Conversation_id"); gotConversation != expectedPromptCacheKey {
		t.Fatalf("Conversation_id = %q, want %q", gotConversation, expectedPromptCacheKey)
	}
	if gotWindowID := headers.Get("X-Codex-Window-Id"); gotWindowID != expectedPromptCacheKey+":0" {
		t.Fatalf("X-Codex-Window-Id = %q, want %q", gotWindowID, expectedPromptCacheKey+":0")
	}
	gotMetadata := headers.Get("X-Codex-Turn-Metadata")
	if gotMetadataPromptCacheKey := gjson.Get(gotMetadata, "prompt_cache_key").String(); gotMetadataPromptCacheKey != expectedPromptCacheKey {
		t.Fatalf("X-Codex-Turn-Metadata.prompt_cache_key = %q, want %q", gotMetadataPromptCacheKey, expectedPromptCacheKey)
	}
	if gotMetadataTurnID := gjson.Get(gotMetadata, "turn_id").String(); gotMetadataTurnID != expectedTurnID {
		t.Fatalf("X-Codex-Turn-Metadata.turn_id = %q, want %q", gotMetadataTurnID, expectedTurnID)
	}
	if gotMetadataWindowID := gjson.Get(gotMetadata, "window_id").String(); gotMetadataWindowID != expectedPromptCacheKey+":0" {
		t.Fatalf("X-Codex-Turn-Metadata.window_id = %q, want %q", gotMetadataWindowID, expectedPromptCacheKey+":0")
	}
	expectedInstallationID := codexIdentityConfuseUUID("auth-ws-1", "installation", "install-ws-1")
	if gotInstallationID := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(); gotInstallationID != expectedInstallationID {
		t.Fatalf("installation id = %q, want %q", gotInstallationID, expectedInstallationID)
	}
}

func TestCodexIdentityConfuseResponsePayloadHidesUpstreamAndRestoresClient(t *testing.T) {
	state := codexIdentityConfuseState{
		enabled:                true,
		authID:                 "auth-ws-1",
		originalPromptCacheKey: "cache-ws-1",
		promptCacheKey:         codexIdentityConfuseUUID("auth-ws-1", "prompt-cache", "cache-ws-1"),
	}
	expectedTurnID := state.confuseTurnID("turn-ws-1")
	rawPayload := []byte(`{"type":"response.completed","response":{"prompt_cache_key":"cache-ws-1","turn_id":"turn-ws-1"},"prompt_cache_key":"cache-ws-1","turn_id":"turn-ws-1"}`)

	upstreamPayload := applyCodexIdentityConfuseResponsePayload(rawPayload, state)
	if bytes.Contains(upstreamPayload, []byte(`cache-ws-1`)) {
		t.Fatalf("upstream payload still contains original prompt_cache_key: %s", string(upstreamPayload))
	}
	if bytes.Contains(upstreamPayload, []byte(`turn-ws-1`)) {
		t.Fatalf("upstream payload still contains original turn_id: %s", string(upstreamPayload))
	}
	if !bytes.Contains(upstreamPayload, []byte(state.promptCacheKey)) {
		t.Fatalf("upstream payload missing confused prompt_cache_key: %s", string(upstreamPayload))
	}
	if !bytes.Contains(upstreamPayload, []byte(expectedTurnID)) {
		t.Fatalf("upstream payload missing confused turn_id: %s", string(upstreamPayload))
	}

	clientPayload := applyCodexIdentityExposeResponsePayload(upstreamPayload, state)
	if bytes.Contains(clientPayload, []byte(state.promptCacheKey)) {
		t.Fatalf("client payload still contains confused prompt_cache_key: %s", string(clientPayload))
	}
	if bytes.Contains(clientPayload, []byte(expectedTurnID)) {
		t.Fatalf("client payload still contains confused turn_id: %s", string(clientPayload))
	}
	if !bytes.Contains(clientPayload, []byte(`cache-ws-1`)) {
		t.Fatalf("client payload missing original prompt_cache_key: %s", string(clientPayload))
	}
	if !bytes.Contains(clientPayload, []byte(`turn-ws-1`)) {
		t.Fatalf("client payload missing original turn_id: %s", string(clientPayload))
	}

	rawSSE := []byte(`data: {"type":"response.completed","response":{"prompt_cache_key":"cache-ws-1","turn_id":"turn-ws-1"}}`)
	upstreamSSE := applyCodexIdentityConfuseResponsePayload(rawSSE, state)
	if bytes.Contains(upstreamSSE, []byte(`cache-ws-1`)) {
		t.Fatalf("upstream SSE still contains original prompt_cache_key: %s", string(upstreamSSE))
	}
	if bytes.Contains(upstreamSSE, []byte(`turn-ws-1`)) {
		t.Fatalf("upstream SSE still contains original turn_id: %s", string(upstreamSSE))
	}
	clientSSE := applyCodexIdentityExposeResponsePayload(upstreamSSE, state)
	if !bytes.Contains(clientSSE, []byte(`cache-ws-1`)) || bytes.Contains(clientSSE, []byte(state.promptCacheKey)) {
		t.Fatalf("client SSE prompt_cache_key was not restored: %s", string(clientSSE))
	}
	if !bytes.Contains(clientSSE, []byte(`turn-ws-1`)) || bytes.Contains(clientSSE, []byte(expectedTurnID)) {
		t.Fatalf("client SSE turn_id was not restored: %s", string(clientSSE))
	}
}

func TestBuildCodexResponsesWebsocketURLRequiresHTTPURL(t *testing.T) {
	if got, err := buildCodexResponsesWebsocketURL("https://example.com/backend/responses"); err != nil || got != "wss://example.com/backend/responses" {
		t.Fatalf("https URL = %q, %v; want wss URL", got, err)
	}
	if _, err := buildCodexResponsesWebsocketURL("ftp://example.com/responses"); err == nil {
		t.Fatalf("expected unsupported scheme error")
	}
	if _, err := buildCodexResponsesWebsocketURL("https:///responses"); err == nil {
		t.Fatalf("expected empty host error")
	}
}

func TestParseCodexWebsocketErrorMarksConnectionLimitRetryable(t *testing.T) {
	err, ok := parseCodexWebsocketError([]byte(`{"type":"error","status":429,"error":{"code":"websocket_connection_limit_reached","message":"too many websockets"},"headers":{"retry-after":"1"}}`))
	if !ok {
		t.Fatalf("expected websocket error")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("status = %#v, want 429", err)
	}
	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatalf("expected retryable websocket connection limit error")
	}
	if got := *retryable.RetryAfter(); got != 0 {
		t.Fatalf("retryAfter = %v, want connection-limit fallback 0", got)
	}
	withHeaders, ok := err.(interface{ Headers() http.Header })
	if !ok || withHeaders.Headers().Get("retry-after") != "1" {
		t.Fatalf("headers = %#v, want retry-after", err)
	}
}

func TestParseCodexWebsocketErrorUsesUsageLimitRetryMetadata(t *testing.T) {
	err, ok := parseCodexWebsocketError([]byte(`{"type":"error","status":429,"body":{"error":{"type":"usage_limit_reached","message":"usage limit reached","resets_in_seconds":7}}}`))
	if !ok {
		t.Fatalf("expected websocket error")
	}

	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatalf("expected retryable usage limit websocket error")
	}
	if got := *retryable.RetryAfter(); got != 7*time.Second {
		t.Fatalf("retryAfter = %v, want 7s", got)
	}
}

func TestParseCodexWebsocketErrorPreservesWrappedBodyAndHeaders(t *testing.T) {
	err, ok := parseCodexWebsocketError([]byte(`{"type":"error","status":429,"body":{"error":{"code":"websocket_connection_limit_reached","type":"server_error","message":"too many websocket connections"}},"headers":{"x-request-id":"req-1"}}`))
	if !ok {
		t.Fatalf("expected websocket error")
	}

	parsed := gjson.Parse(err.Error())
	if got := parsed.Get("status").Int(); got != http.StatusTooManyRequests {
		t.Fatalf("wrapped status = %d, want 429; payload=%s", got, err.Error())
	}
	if got := parsed.Get("body.error.code").String(); got != "websocket_connection_limit_reached" {
		t.Fatalf("wrapped body error code = %s, want websocket_connection_limit_reached; payload=%s", got, err.Error())
	}
	if got := parsed.Get("error.code").String(); got != "websocket_connection_limit_reached" {
		t.Fatalf("surface error code = %s, want websocket_connection_limit_reached; payload=%s", got, err.Error())
	}
	retryable, ok := err.(interface{ RetryAfter() *time.Duration })
	if !ok || retryable.RetryAfter() == nil {
		t.Fatalf("expected body.error.code websocket connection limit to be retryable")
	}
	withHeaders, ok := err.(interface{ Headers() http.Header })
	if !ok || withHeaders.Headers().Get("x-request-id") != "req-1" {
		t.Fatalf("headers = %#v, want x-request-id", err)
	}
}

func TestApplyCodexHeadersUsesConfigUserAgentForOAuth(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	cfg := &config.Config{
		CodexHeaderDefaults: config.CodexHeaderDefaults{
			UserAgent:    "config-ua",
			BetaFeatures: "config-beta",
		},
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"User-Agent": "client-ua",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, cfg)

	if got := req.Header.Get("User-Agent"); got != "config-ua" {
		t.Fatalf("User-Agent = %s, want %s", got, "config-ua")
	}
	if got := req.Header.Get("x-codex-beta-features"); got != "" {
		t.Fatalf("x-codex-beta-features = %q, want empty", got)
	}
}

func TestApplyCodexHeadersPassesThroughClientIdentityHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := &cliproxyauth.Auth{
		Provider: "codex",
		Metadata: map[string]any{"email": "user@example.com"},
	}
	req = req.WithContext(contextWithGinHeaders(map[string]string{
		"Originator":            "Codex Desktop",
		"Version":               "0.115.0-alpha.27",
		"X-Codex-Turn-Metadata": `{"turn_id":"turn-1"}`,
		"X-Client-Request-Id":   "019d2233-e240-7162-992d-38df0a2a0e0d",
	}))

	applyCodexHeaders(req, auth, "oauth-token", true, nil)

	if got := req.Header.Get("Originator"); got != "Codex Desktop" {
		t.Fatalf("Originator = %s, want %s", got, "Codex Desktop")
	}
	if got := req.Header.Get("Version"); got != "0.115.0-alpha.27" {
		t.Fatalf("Version = %s, want %s", got, "0.115.0-alpha.27")
	}
	if got := req.Header.Get("X-Codex-Turn-Metadata"); got != `{"turn_id":"turn-1"}` {
		t.Fatalf("X-Codex-Turn-Metadata = %s, want %s", got, `{"turn_id":"turn-1"}`)
	}
	if got := req.Header.Get("X-Client-Request-Id"); got != "019d2233-e240-7162-992d-38df0a2a0e0d" {
		t.Fatalf("X-Client-Request-Id = %s, want %s", got, "019d2233-e240-7162-992d-38df0a2a0e0d")
	}
}

func TestApplyCodexHeadersDoesNotInjectClientOnlyHeadersByDefault(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://example.com/responses", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	applyCodexHeaders(req, nil, "oauth-token", true, nil)

	if got := req.Header.Get("Version"); got != "" {
		t.Fatalf("Version = %q, want empty", got)
	}
	if got := req.Header.Get("X-Codex-Turn-Metadata"); got != "" {
		t.Fatalf("X-Codex-Turn-Metadata = %q, want empty", got)
	}
	if got := req.Header.Get("X-Client-Request-Id"); got != "" {
		t.Fatalf("X-Client-Request-Id = %q, want empty", got)
	}
}

func contextWithGinHeaders(headers map[string]string) context.Context {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	ginCtx.Request.Header = make(http.Header, len(headers))
	for key, value := range headers {
		ginCtx.Request.Header.Set(key, value)
	}
	return context.WithValue(context.Background(), "gin", ginCtx)
}

func TestNewProxyAwareWebsocketDialerDirectDisablesProxy(t *testing.T) {
	t.Parallel()

	dialer := newProxyAwareWebsocketDialer(
		&config.Config{SDKConfig: sdkconfig.SDKConfig{ProxyURL: "http://global-proxy.example.com:8080"}},
		&cliproxyauth.Auth{ProxyURL: "direct"},
	)

	if dialer.Proxy != nil {
		t.Fatal("expected websocket proxy function to be nil for direct mode")
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
