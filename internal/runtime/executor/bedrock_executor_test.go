package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"hash/crc32"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestBedrockExecutorExecute_translatesOpenAIChatToConverse_whenConfigured(t *testing.T) {
	// Given
	var gotPath string
	var gotAuth string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody = readBedrockTestBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":{"message":{"role":"assistant","content":[{"text":"bedrock-ok"}]}},"stopReason":"end_turn","usage":{"inputTokens":3,"outputTokens":2,"totalTokens":5}}`))
	}))
	defer server.Close()
	exec := NewBedrockExecutor(&config.Config{})
	auth := bedrockTestAuth(server.URL)
	req := cliproxyexecutor.Request{
		Model:   "sonnet-5",
		Payload: []byte(`{"model":"sonnet-5","messages":[{"role":"system","content":"be terse"},{"role":"user","content":"say ok"}],"max_tokens":64}`),
	}

	// When
	resp, err := exec.Execute(context.Background(), auth, req, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
	})

	// Then
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if gotPath != "/model/global.anthropic.claude-sonnet-5/converse" {
		t.Fatalf("path = %q, want Bedrock Converse path", gotPath)
	}
	if gotAuth != "raw-token" {
		t.Fatalf("Authorization = %q, want raw-token", gotAuth)
	}
	if got := gjson.GetBytes(gotBody, "messages.0.role").String(); got != "user" {
		t.Fatalf("bedrock request first role = %q, want user; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "messages.0.content.0.text").String(); got != "say ok" {
		t.Fatalf("bedrock user text = %q, want say ok; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "system.0.text").String(); got != "be terse" {
		t.Fatalf("bedrock system text = %q, want be terse; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(gotBody, "inferenceConfig.maxTokens").Int(); got != 64 {
		t.Fatalf("maxTokens = %d, want 64; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "bedrock-ok" {
		t.Fatalf("chat content = %q, want bedrock-ok; payload=%s", got, string(resp.Payload))
	}
	if got := gjson.GetBytes(resp.Payload, "usage.total_tokens").Int(); got != 5 {
		t.Fatalf("total_tokens = %d, want 5; payload=%s", got, string(resp.Payload))
	}
}

func TestBedrockExecutorExecute_translatesOpenAIResponsesToConverse_whenConfigured(t *testing.T) {
	// Given
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/global.anthropic.claude-sonnet-5/converse" {
			t.Fatalf("path = %q, want Bedrock Converse path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":{"message":{"role":"assistant","content":[{"text":"responses-ok"}]}},"stopReason":"end_turn","usage":{"inputTokens":4,"outputTokens":3,"totalTokens":7}}`))
	}))
	defer server.Close()
	exec := NewBedrockExecutor(&config.Config{})
	auth := bedrockTestAuth(server.URL)
	req := cliproxyexecutor.Request{
		Model:   "sonnet-5",
		Payload: []byte(`{"model":"sonnet-5","input":[{"role":"user","content":[{"type":"input_text","text":"say ok"}]}],"max_output_tokens":64}`),
	}

	// When
	resp, err := exec.Execute(context.Background(), auth, req, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
	})

	// Then
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "status").String(); got != "completed" {
		t.Fatalf("response status = %q, want completed; payload=%s", got, string(resp.Payload))
	}
	if got := gjson.GetBytes(resp.Payload, "output.0.content.0.text").String(); got != "responses-ok" {
		t.Fatalf("responses text = %q, want responses-ok; payload=%s", got, string(resp.Payload))
	}
	if got := gjson.GetBytes(resp.Payload, "usage.total_tokens").Int(); got != 7 {
		t.Fatalf("total_tokens = %d, want 7; payload=%s", got, string(resp.Payload))
	}
}

func TestBedrockExecutorExecute_translatesClaudeMessagesToConverse_whenConfigured(t *testing.T) {
	// Given
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody = readBedrockTestBody(t, r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"output":{"message":{"role":"assistant","content":[{"text":"claude-ok"}]}},"stopReason":"end_turn","usage":{"inputTokens":5,"outputTokens":2,"totalTokens":7}}`))
	}))
	defer server.Close()
	exec := NewBedrockExecutor(&config.Config{})
	auth := bedrockTestAuth(server.URL)
	req := cliproxyexecutor.Request{
		Model:   "sonnet-5",
		Payload: []byte(`{"model":"sonnet-5","system":"be terse","max_tokens":64,"messages":[{"role":"user","content":"say ok"}]}`),
	}

	// When
	resp, err := exec.Execute(context.Background(), auth, req, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
	})

	// Then
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if gotPath != "/model/global.anthropic.claude-sonnet-5/converse" {
		t.Fatalf("path = %q, want Bedrock Converse path", gotPath)
	}
	if got := gjson.GetBytes(gotBody, "messages.0.content.0.text").String(); got != "say ok" {
		t.Fatalf("bedrock user text = %q, want say ok; body=%s", got, string(gotBody))
	}
	if got := gjson.GetBytes(resp.Payload, "content.0.text").String(); got != "claude-ok" {
		t.Fatalf("claude content = %q, want claude-ok; payload=%s", got, string(resp.Payload))
	}
	if got := gjson.GetBytes(resp.Payload, "usage.input_tokens").Int(); got != 5 {
		t.Fatalf("input tokens = %d, want 5; payload=%s", got, string(resp.Payload))
	}
}

func TestBedrockExecutorExecuteStream_translatesConverseStreamToOpenAIChat_whenConfigured(t *testing.T) {
	// Given
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/global.anthropic.claude-sonnet-5/converse-stream" {
			t.Fatalf("path = %q, want Bedrock ConverseStream path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"messageStart\":{\"role\":\"assistant\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"contentBlockDelta\":{\"contentBlockIndex\":0,\"delta\":{\"text\":\"stream-ok\"}}}\n\n"))
		_, _ = w.Write([]byte("data: {\"messageStop\":{\"stopReason\":\"end_turn\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"metadata\":{\"usage\":{\"inputTokens\":1,\"outputTokens\":2,\"totalTokens\":3}}}\n\n"))
	}))
	defer server.Close()
	exec := NewBedrockExecutor(&config.Config{})
	auth := bedrockTestAuth(server.URL)
	req := cliproxyexecutor.Request{
		Model:   "sonnet-5",
		Payload: []byte(`{"model":"sonnet-5","messages":[{"role":"user","content":"stream"}]}`),
	}

	// When
	result, err := exec.ExecuteStream(context.Background(), auth, req, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var joined bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		joined.Write(chunk.Payload)
		joined.WriteByte('\n')
	}

	// Then
	if !bytes.Contains(joined.Bytes(), []byte("stream-ok")) {
		t.Fatalf("stream output missing text: %s", joined.String())
	}
	if !bytes.Contains(joined.Bytes(), []byte(`"finish_reason":"stop"`)) {
		t.Fatalf("stream output missing finish chunk: %s", joined.String())
	}
}

func TestBedrockRequestForLogMasksQueryParams(t *testing.T) {
	req, err := http.NewRequest(http.MethodPost, "https://bedrock.example/model/m/converse?trace=secret&debug=true", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	logReq := bedrockRequestForLog(req)

	if logReq == req {
		t.Fatal("bedrockRequestForLog() returned original request")
	}
	if got := logReq.URL.Query(); got.Get("trace") != "***" || got.Get("debug") != "***" {
		t.Fatalf("masked query = %q, want all query values masked", logReq.URL.RawQuery)
	}
	if got := req.URL.Query(); got.Get("trace") != "secret" || got.Get("debug") != "true" {
		t.Fatalf("original query mutated = %q", req.URL.RawQuery)
	}
}

func TestMaskBedrockQueryForLogPreservesRepeatedKeysAsSingleMask(t *testing.T) {
	values := url.Values{"trace": []string{"first", "second"}}

	masked := maskBedrockQueryForLog(values)

	if masked != "trace=%2A%2A%2A" {
		t.Fatalf("maskBedrockQueryForLog() = %q, want trace mask", masked)
	}
}

func TestBedrockEventStreamExceptionIncludesTypeAndMessage(t *testing.T) {
	err := bedrockEventStreamException(helps.BedrockEventStreamMessage{
		Headers: map[string]string{
			":message-type":   "exception",
			":exception-type": "modelStreamErrorException",
		},
		Payload: []byte(`{"message":"upstream failed"}`),
	})

	if err == nil {
		t.Fatal("bedrockEventStreamException() error = nil, want exception")
	}
	if got := err.Error(); got != "modelStreamErrorException: upstream failed" {
		t.Fatalf("error = %q, want exception type and message", got)
	}
}

func TestPrepareRequest_rejectsUnsupportedBedrockAuthType(t *testing.T) {
	exec := NewBedrockExecutor(&config.Config{})
	req, err := http.NewRequest(http.MethodPost, "https://bedrock.example/model/m/converse", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	auth := bedrockTestAuth("https://bedrock.example")
	auth.Attributes["auth_type"] = "sigv4"

	err = exec.PrepareRequest(req, auth)

	if err == nil {
		t.Fatal("PrepareRequest() error = nil, want unsupported auth type error")
	}
	if got := err.Error(); !bytes.Contains([]byte(got), []byte("unsupported auth type")) || !bytes.Contains([]byte(got), []byte("sigv4")) {
		t.Fatalf("PrepareRequest() error = %q, want unsupported sigv4 error", got)
	}
}

func TestResolveBedrockAPI_usesInvokeStreamWhenStreamMapMissing(t *testing.T) {
	auth := bedrockTestAuth("https://bedrock.example")
	delete(auth.Attributes, "bedrock_stream_map")
	auth.Attributes["bedrock_model_map"] = `{"sonnet-legacy":"anthropic.claude-3-5-sonnet-20241022-v2:0"}`
	auth.Attributes["bedrock_api_map"] = `{"sonnet-legacy":"invoke","anthropic.claude-3-5-sonnet-20241022-v2:0":"invoke"}`

	if got := resolveBedrockAPI(auth, "sonnet-legacy", true); got != "invoke-stream" {
		t.Fatalf("resolveBedrockAPI() = %q, want invoke-stream", got)
	}
}

func TestBedrockExecutorExecuteStream_translatesEventStreamToOpenAIChat_whenConfigured(t *testing.T) {
	// Given
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model/global.anthropic.claude-sonnet-5/converse-stream" {
			t.Fatalf("path = %q, want Bedrock ConverseStream path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(bedrockEventStreamFrameForExecutorTest([]byte(`{"messageStart":{"role":"assistant"}}`)))
		_, _ = w.Write(bedrockEventStreamFrameForExecutorTest([]byte(`{"contentBlockIndex":0,"delta":{"text":"eventstream-ok"}}`)))
		_, _ = w.Write(bedrockEventStreamFrameForExecutorTest([]byte(`{"stopReason":"end_turn"}`)))
		_, _ = w.Write(bedrockEventStreamFrameForExecutorTest([]byte(`{"usage":{"inputTokens":1,"outputTokens":2,"totalTokens":3}}`)))
	}))
	defer server.Close()
	exec := NewBedrockExecutor(&config.Config{})
	auth := bedrockTestAuth(server.URL)
	req := cliproxyexecutor.Request{
		Model:   "sonnet-5",
		Payload: []byte(`{"model":"sonnet-5","messages":[{"role":"user","content":"stream"}]}`),
	}

	// When
	result, err := exec.ExecuteStream(context.Background(), auth, req, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var joined bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		joined.Write(chunk.Payload)
		joined.WriteByte('\n')
	}

	// Then
	if !bytes.Contains(joined.Bytes(), []byte("eventstream-ok")) {
		t.Fatalf("stream output missing text: %s", joined.String())
	}
	if !bytes.Contains(joined.Bytes(), []byte(`"finish_reason":"stop"`)) {
		t.Fatalf("stream output missing finish chunk: %s", joined.String())
	}
}

func TestBedrockExecutorExecuteStream_surfacesEventStreamException(t *testing.T) {
	// Given
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
		_, _ = w.Write(bedrockEventStreamFrameWithHeadersForExecutorTest(map[string]string{
			":message-type":   "exception",
			":exception-type": "throttlingException",
		}, []byte(`{"message":"slow down"}`)))
	}))
	defer server.Close()
	exec := NewBedrockExecutor(&config.Config{})
	auth := bedrockTestAuth(server.URL)
	req := cliproxyexecutor.Request{
		Model:   "sonnet-5",
		Payload: []byte(`{"model":"sonnet-5","messages":[{"role":"user","content":"stream"}]}`),
	}

	// When
	result, err := exec.ExecuteStream(context.Background(), auth, req, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var gotErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			gotErr = chunk.Err
			break
		}
	}

	// Then
	if gotErr == nil {
		t.Fatal("expected stream exception error")
	}
	if got := gotErr.Error(); !bytes.Contains([]byte(got), []byte("throttlingException")) || !bytes.Contains([]byte(got), []byte("slow down")) {
		t.Fatalf("stream error = %q, want exception type and message", got)
	}
}

func TestBedrockExecutorExecuteStream_translatesConverseStreamToolUse(t *testing.T) {
	// Given
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"contentBlockStart\":{\"contentBlockIndex\":0,\"start\":{\"toolUse\":{\"toolUseId\":\"toolu_1\",\"name\":\"lookup\"}}}}\n\n"))
		_, _ = w.Write([]byte("data: {\"contentBlockDelta\":{\"contentBlockIndex\":0,\"delta\":{\"toolUse\":{\"input\":\"{\\\"city\\\":\\\"sf\\\"}\"}}}}\n\n"))
		_, _ = w.Write([]byte("data: {\"contentBlockStop\":{\"contentBlockIndex\":0}}\n\n"))
		_, _ = w.Write([]byte("data: {\"messageStop\":{\"stopReason\":\"tool_use\"}}\n\n"))
	}))
	defer server.Close()
	exec := NewBedrockExecutor(&config.Config{})
	auth := bedrockTestAuth(server.URL)
	req := cliproxyexecutor.Request{
		Model:   "sonnet-5",
		Payload: []byte(`{"model":"sonnet-5","messages":[{"role":"user","content":"use tool"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`),
	}

	// When
	result, err := exec.ExecuteStream(context.Background(), auth, req, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("claude"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var joined bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		joined.Write(chunk.Payload)
		joined.WriteByte('\n')
	}

	// Then
	if !bytes.Contains(joined.Bytes(), []byte(`"type":"tool_use"`)) {
		t.Fatalf("stream output missing tool_use block: %s", joined.String())
	}
	if !bytes.Contains(joined.Bytes(), []byte(`"partial_json":"{\"city\":\"sf\"}"`)) {
		t.Fatalf("stream output missing tool input delta: %s", joined.String())
	}
	if !bytes.Contains(joined.Bytes(), []byte(`"stop_reason":"tool_use"`)) {
		t.Fatalf("stream output missing tool_use stop reason: %s", joined.String())
	}
}

func bedrockTestAuth(baseURL string) *cliproxyauth.Auth {
	return &cliproxyauth.Auth{
		ID:       "bedrock-auth",
		Provider: "bedrock",
		Attributes: map[string]string{
			"api_key":            "raw-token",
			"auth_type":          "raw",
			"base_url":           baseURL,
			"bedrock_model_map":  `{"sonnet-5":"global.anthropic.claude-sonnet-5"}`,
			"bedrock_api_map":    `{"sonnet-5":"converse"}`,
			"bedrock_stream_map": `{"sonnet-5":"converse-stream"}`,
		},
	}
}

func readBedrockTestBody(t *testing.T, r *http.Request) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	return body
}

func bedrockEventStreamFrameForExecutorTest(payload []byte) []byte {
	return bedrockEventStreamFrameWithHeadersForExecutorTest(nil, payload)
}

func bedrockEventStreamFrameWithHeadersForExecutorTest(values map[string]string, payload []byte) []byte {
	var headers []byte
	for name, value := range values {
		headers = append(headers, byte(len(name)))
		headers = append(headers, name...)
		headers = append(headers, 7)
		headers = binary.BigEndian.AppendUint16(headers, uint16(len(value)))
		headers = append(headers, value...)
	}
	totalLen := uint32(16 + len(headers) + len(payload))
	frame := make([]byte, totalLen)
	binary.BigEndian.PutUint32(frame[0:4], totalLen)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[0:8]))
	copy(frame[12:], headers)
	copy(frame[12+len(headers):], payload)
	binary.BigEndian.PutUint32(frame[len(frame)-4:], crc32.ChecksumIEEE(frame[:len(frame)-4]))
	return frame
}
