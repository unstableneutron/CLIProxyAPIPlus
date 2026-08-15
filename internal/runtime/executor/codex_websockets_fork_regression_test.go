package executor

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
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
			t.Fatalf("function_call_output %s has no matching function_call in continuation replay input: %s", callID, input.Raw)
		}
	}
}

func TestCodexWebsocketContinuationReplayKeepsPendingInput(t *testing.T) {
	transcriptInput := []byte(`[{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"before"}]},{"id":"fc-1","type":"function_call","call_id":"call_pending","name":"exec_command","arguments":"{}"}]`)
	requestPayload := []byte(`{"model":"gpt-5.5","previous_response_id":"resp-1","input":[{"id":"out-1","type":"function_call_output","call_id":"call_pending","output":"done"},{"type":"compaction_trigger"},{"id":"out-2","type":"function_call_output","call_id":"call_second","output":"ok"}]}`)

	replayInput := gjson.ParseBytes(codexWebsocketCompactionReplayInput(transcriptInput, requestPayload))
	input := replayInput.Array()
	if got, want := len(input), 4; got != want {
		t.Fatalf("replay input length = %d, want %d: %s", got, want, replayInput.Raw)
	}
	wantTypes := []string{"message", "function_call", "function_call_output", "function_call_output"}
	for index, wantType := range wantTypes {
		if got := input[index].Get("type").String(); got != wantType {
			t.Fatalf("input[%d].type = %q, want %q: %s", index, got, wantType, replayInput.Raw)
		}
	}
}

func TestCodexWebsocketContinuationReplayDeduplicatesFullInput(t *testing.T) {
	transcriptInput := []byte(`[{"id":"msg-upstream","type":"message","role":"user","content":"before"},{"id":"fc-upstream","type":"function_call","call_id":"call_pending","name":"exec_command","arguments":"{}","metadata":{"provider":"upstream"}}]`)
	requestPayload := []byte(`{"model":"gpt-5.5","previous_response_id":"resp-1","input":[{"id":"msg-downstream","type":"message","role":"user","content":"before"},{"id":"fc-downstream","type":"function_call","call_id":"call_pending","name":"exec_command","arguments":"{}"},{"id":"out-downstream","type":"function_call_output","call_id":"call_pending","output":"done"},{"type":"compaction_trigger"}]}`)

	replayInput := gjson.ParseBytes(codexWebsocketCompactionReplayInput(transcriptInput, requestPayload))
	input := replayInput.Array()
	if got, want := len(input), 3; got != want {
		t.Fatalf("replay input length = %d, want %d: %s", got, want, replayInput.Raw)
	}
	wantTypes := []string{"message", "function_call", "function_call_output"}
	for index, wantType := range wantTypes {
		if got := input[index].Get("type").String(); got != wantType {
			t.Fatalf("input[%d].type = %q, want %q: %s", index, got, wantType, replayInput.Raw)
		}
	}
	assertNoOrphanFunctionCallOutputs(t, replayInput)
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
