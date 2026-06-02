package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestFetchCommandCodeModelsUsesProviderCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/provider/v1/models" {
			t.Fatalf("path = %s, want /provider/v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer user_test" {
			t.Fatalf("Authorization = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"deepseek/deepseek-v4-flash","object":"model","created":1780357901,"owned_by":"command-code","name":"DeepSeek V4 Flash","context_length":1000000},{"id":"MiniMaxAI/MiniMax-M3","object":"model","created":1780357901,"owned_by":"command-code","name":"MiniMax M3","context_length":1000000}]}`))
	}))
	defer server.Close()

	models := FetchCommandCodeModels(context.Background(), &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "user_test", "base_url": server.URL}}, &config.Config{})
	if len(models) != 2 {
		t.Fatalf("models = %d, want 2: %+v", len(models), models)
	}
	if got := models[0].ID; got != "deepseek/deepseek-v4-flash" {
		t.Fatalf("first id = %q", got)
	}
	if got := models[0].DisplayName; got != "DeepSeek V4 Flash (CC)" {
		t.Fatalf("display name = %q", got)
	}
	if got := models[0].ContextLength; got != 1000000 {
		t.Fatalf("context length = %d", got)
	}
	if got := models[0].MaxCompletionTokens; got != commandCodeMaxTokensCap {
		t.Fatalf("max completion tokens = %d, want cap %d", got, commandCodeMaxTokensCap)
	}
	if models[0].Thinking == nil || len(models[0].Thinking.Levels) == 0 {
		t.Fatalf("expected thinking support on live CommandCode model: %+v", models[0])
	}
	if got := models[1].ID; got != "MiniMaxAI/MiniMax-M3" {
		t.Fatalf("second id = %q", got)
	}
}

func TestCommandCodeBuildPayloadConvertsOpenAIChat(t *testing.T) {
	input := []byte(`{
		"model":"deepseek/deepseek-v4-flash",
		"messages":[
			{"role":"system","content":"You are concise."},
			{"role":"user","content":"hello"},
			{"role":"assistant","content":"I will call a tool","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"go\"}"}},{"id":"orphan","type":"function","function":{"name":"dangling","arguments":"{\"q\":\"bad\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","name":"lookup","content":"result text"}
		],
		"tools":[{"type":"function","function":{"name":"lookup","description":"Lookup docs","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}}],
		"max_tokens":500000
	}`)

	payload, err := buildCommandCodePayload(commandCodePayloadOptions{
		Model:       "deepseek/deepseek-v4-flash",
		Payload:     input,
		WorkingDir:  "/repo",
		Environment: "test-env",
		Now:         func() time.Time { return time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("buildCommandCodePayload() error = %v", err)
	}

	root := gjson.ParseBytes(payload)
	if got := root.Get("config.workingDir").String(); got != "/repo" {
		t.Fatalf("workingDir = %q, want /repo", got)
	}
	if got := root.Get("config.date").String(); got != "2026-05-05" {
		t.Fatalf("date = %q, want 2026-05-05", got)
	}
	if got := root.Get("config.environment").String(); got != "test-env" {
		t.Fatalf("environment = %q, want test-env", got)
	}
	if got := root.Get("params.model").String(); got != "deepseek/deepseek-v4-flash" {
		t.Fatalf("model = %q", got)
	}
	if got := root.Get("params.system").String(); got != "You are concise." {
		t.Fatalf("system = %q", got)
	}
	if got := root.Get("params.max_tokens").Int(); got != 200000 {
		t.Fatalf("max_tokens = %d, want capped 200000", got)
	}
	if !root.Get("params.stream").Bool() {
		t.Fatal("expected params.stream=true")
	}
	if got := root.Get("params.messages.1.content.1.type").String(); got != "tool-call" {
		t.Fatalf("assistant tool call type = %q", got)
	}
	if got := root.Get("params.messages.1.content.1.input.q").String(); got != "go" {
		t.Fatalf("tool call input q = %q", got)
	}
	if got := root.Get("params.messages.1.content.#(toolCallId==\"orphan\")").Raw; got != "" {
		t.Fatalf("orphaned assistant tool call should be dropped, got %s", got)
	}
	if got := root.Get("params.messages.2.role").String(); got != "tool" {
		t.Fatalf("tool result role = %q", got)
	}
	if got := root.Get("params.tools.0.input_schema.required.0").String(); got != "q" {
		t.Fatalf("tool schema required[0] = %q", got)
	}
}

func TestCommandCodeBuildPayloadForwardsOptionalParams(t *testing.T) {
	input := []byte(`{
		"model":"deepseek/deepseek-v4-flash",
		"messages":[{"role":"user","content":"hello"}],
		"temperature":0.2,
		"top_p":0.8,
		"stop":["END"],
		"max_tokens":32
	}`)

	payload, err := buildCommandCodePayload(commandCodePayloadOptions{
		Model:       "deepseek/deepseek-v4-flash",
		Payload:     input,
		WorkingDir:  "/repo",
		Environment: "test-env",
		Now:         func() time.Time { return time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("buildCommandCodePayload() error = %v", err)
	}

	root := gjson.ParseBytes(payload)
	if got := root.Get("params.temperature").Float(); got != 0.2 {
		t.Fatalf("temperature = %v, want 0.2; payload=%s", got, payload)
	}
	if got := root.Get("params.top_p").Float(); got != 0.8 {
		t.Fatalf("top_p = %v, want 0.8; payload=%s", got, payload)
	}
	if got := root.Get("params.stop.0").String(); got != "END" {
		t.Fatalf("stop[0] = %q, want END; payload=%s", got, payload)
	}
}

func TestCommandCodeLineToOpenAIChunksHandlesIncrementalToolInput(t *testing.T) {
	state := newCommandCodeStreamState("deepseek/deepseek-v4-flash")

	chunks, usageDetail, err := commandCodeLineToOpenAIChunks([]byte(`{"type":"tool-input-start","id":"call_1","toolName":"lookup"}`), state)
	if err != nil {
		t.Fatalf("tool-input-start error = %v", err)
	}
	if hasUsageDetail(usageDetail) {
		t.Fatalf("tool-input-start usage = %+v, want empty", usageDetail)
	}
	if len(chunks) != 1 {
		t.Fatalf("tool-input-start chunks = %d, want 1", len(chunks))
	}
	if got := gjson.GetBytes(chunks[0], "choices.0.delta.tool_calls.0.function.name").String(); got != "lookup" {
		t.Fatalf("tool name = %q, want lookup; chunk=%s", got, chunks[0])
	}
	if got := gjson.GetBytes(chunks[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != "" {
		t.Fatalf("initial arguments = %q, want empty", got)
	}

	chunks, _, err = commandCodeLineToOpenAIChunks([]byte(`{"type":"tool-input-delta","id":"call_1","delta":"{\"q\":"}`), state)
	if err != nil {
		t.Fatalf("tool-input-delta error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("tool-input-delta chunks = %d, want 1", len(chunks))
	}
	if got := gjson.GetBytes(chunks[0], "choices.0.delta.tool_calls.0.function.arguments").String(); got != `{"q":` {
		t.Fatalf("delta arguments = %q, want JSON fragment", got)
	}

	chunks, _, err = commandCodeLineToOpenAIChunks([]byte(`{"type":"tool-input-delta","id":"call_1","delta":"\"Paris\"}"}`), state)
	if err != nil {
		t.Fatalf("second tool-input-delta error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("second tool-input-delta chunks = %d, want 1", len(chunks))
	}

	chunks, _, err = commandCodeLineToOpenAIChunks([]byte(`{"type":"tool-input-end","id":"call_1"}`), state)
	if err != nil {
		t.Fatalf("tool-input-end error = %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("tool-input-end chunks = %d, want 0", len(chunks))
	}

	chunks, _, err = commandCodeLineToOpenAIChunks([]byte(`{"type":"tool-call","toolCallId":"call_1","toolName":"lookup","input":{"q":"Paris"}}`), state)
	if err != nil {
		t.Fatalf("tool-call error = %v", err)
	}
	if len(chunks) != 0 {
		t.Fatalf("final tool-call after incremental input should not duplicate chunks, got %d: %s", len(chunks), chunks[0])
	}
	if len(state.ToolCalls) != 1 {
		t.Fatalf("state tool calls = %d, want 1", len(state.ToolCalls))
	}
	if got := state.ToolCalls[0].Arguments; got != `{"q":"Paris"}` {
		t.Fatalf("stored arguments = %q, want final JSON", got)
	}

	chunks, usageDetail, err = commandCodeLineToOpenAIChunks([]byte(`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":10,"outputTokens":2,"totalTokens":12}}`), state)
	if err != nil {
		t.Fatalf("finish error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("finish chunks = %d, want 1", len(chunks))
	}
	if got := gjson.GetBytes(chunks[0], "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls; chunk=%s", got, chunks[0])
	}
	if usageDetail.TotalTokens != 12 {
		t.Fatalf("usage total = %d, want 12", usageDetail.TotalTokens)
	}
}

func TestCommandCodeUsageUsesTotalTokensWithoutDoubleCountingCache(t *testing.T) {
	state := newCommandCodeStreamState("deepseek/deepseek-v4-flash")

	chunks, usageDetail, err := commandCodeLineToOpenAIChunks([]byte(`{"type":"finish","finishReason":"length","totalUsage":{"inputTokens":7528,"inputTokenDetails":{"cacheReadTokens":7424},"outputTokens":16,"outputTokenDetails":{"reasoningTokens":16},"totalTokens":7544,"reasoningTokens":16,"cachedInputTokens":7424}}`), state)
	if err != nil {
		t.Fatalf("finish error = %v", err)
	}
	if len(chunks) != 1 {
		t.Fatalf("finish chunks = %d, want 1", len(chunks))
	}
	if usageDetail.InputTokens != 7528 || usageDetail.OutputTokens != 16 || usageDetail.TotalTokens != 7544 {
		t.Fatalf("usage detail = %+v, want input=7528 output=16 total=7544", usageDetail)
	}
	if usageDetail.CacheReadTokens != 7424 || usageDetail.CachedTokens != 7424 {
		t.Fatalf("cache usage detail = %+v, want cache read/cached=7424", usageDetail)
	}
	if usageDetail.ReasoningTokens != 16 {
		t.Fatalf("reasoning tokens = %d, want 16", usageDetail.ReasoningTokens)
	}
	if got := gjson.GetBytes(chunks[0], "usage.prompt_tokens").Int(); got != 7528 {
		t.Fatalf("prompt_tokens = %d, want 7528; chunk=%s", got, chunks[0])
	}
	if got := gjson.GetBytes(chunks[0], "usage.total_tokens").Int(); got != 7544 {
		t.Fatalf("total_tokens = %d, want 7544; chunk=%s", got, chunks[0])
	}
	if got := gjson.GetBytes(chunks[0], "usage.prompt_tokens_details.cached_tokens").Int(); got != 7424 {
		t.Fatalf("cached_tokens = %d, want 7424; chunk=%s", got, chunks[0])
	}
	if got := gjson.GetBytes(chunks[0], "usage.completion_tokens_details.reasoning_tokens").Int(); got != 16 {
		t.Fatalf("reasoning_tokens = %d, want 16; chunk=%s", got, chunks[0])
	}
}

func TestCommandCodeExecuteStreamSendsHeadersAndTranslatesEvents(t *testing.T) {
	var capturedAuth string
	var capturedVersion string
	var capturedSession string
	var capturedBody []byte

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/alpha/generate" {
			t.Fatalf("path = %s, want /alpha/generate", r.URL.Path)
		}
		capturedAuth = r.Header.Get("Authorization")
		capturedVersion = r.Header.Get("x-command-code-version")
		capturedSession = r.Header.Get("x-session-id")
		var err error
		capturedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if errClose := r.Body.Close(); errClose != nil {
			t.Fatalf("close request body: %v", errClose)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"text-delta\",\"text\":\"Hi\"}\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":3,\"outputTokens\":1}}\n"))
	}))
	defer server.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{
		ID:       "cc-auth",
		Provider: "commandcode",
		Attributes: map[string]string{
			"api_key":  "user_test",
			"base_url": server.URL,
		},
	}
	input := []byte(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hello"}],"stream":true}`)
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek/deepseek-v4-flash",
		Payload: input,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai"),
		OriginalRequest: input,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var chunks []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		if len(chunk.Payload) > 0 {
			chunks = append(chunks, string(chunk.Payload))
		}
	}

	if capturedAuth != "Bearer user_test" {
		t.Fatalf("Authorization = %q", capturedAuth)
	}
	if capturedVersion != commandCodeVersionHeader {
		t.Fatalf("x-command-code-version = %q", capturedVersion)
	}
	if strings.TrimSpace(capturedSession) == "" {
		t.Fatal("expected x-session-id header")
	}
	if got := gjson.GetBytes(capturedBody, "params.model").String(); got != "deepseek/deepseek-v4-flash" {
		t.Fatalf("request params.model = %q", got)
	}
	if len(chunks) < 2 {
		t.Fatalf("expected at least text and finish chunks, got %d: %v", len(chunks), chunks)
	}
	if got := gjson.Get(chunks[0], "choices.0.delta.content").String(); got != "Hi" {
		t.Fatalf("first delta content = %q; chunks=%v", got, chunks)
	}
	if got := gjson.Get(chunks[len(chunks)-1], "choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("finish reason = %q; chunks=%v", got, chunks)
	}
}

func TestCommandCodeExecuteStreamEmitsResponsesCompleted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"text-delta\",\"text\":\"ws-ok\"}\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"finish\",\"finishReason\":\"stop\",\"totalUsage\":{\"inputTokens\":3,\"outputTokens\":1,\"totalTokens\":4}}\n"))
	}))
	defer server.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "user_test", "base_url": server.URL}}
	input := []byte(`{"model":"deepseek/deepseek-v4-flash","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"stream":true}`)
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "deepseek/deepseek-v4-flash",
		Payload: input,
	}, cliproxyexecutor.Options{
		Stream:          true,
		SourceFormat:    sdktranslator.FromString("openai-response"),
		OriginalRequest: input,
	})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}

	var eventTypes []string
	var completed bool
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
		if len(chunk.Payload) == 0 {
			continue
		}
		eventType := gjson.GetBytes(chunk.Payload, "type").String()
		if eventType != "" {
			eventTypes = append(eventTypes, eventType)
		}
		if eventType == "response.completed" {
			completed = true
		}
	}
	if !completed {
		t.Fatalf("expected response.completed event, got event types %v", eventTypes)
	}
}

func TestCommandCodeExecuteAggregatesNonStreamingResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("{\"type\":\"reasoning-delta\",\"text\":\"think\"}\n"))
		_, _ = w.Write([]byte("{\"type\":\"reasoning-end\"}\n"))
		_, _ = w.Write([]byte("{\"type\":\"text-delta\",\"text\":\"answer\"}\n"))
		_, _ = w.Write([]byte("{\"type\":\"finish\",\"finishReason\":\"max_tokens\",\"totalUsage\":{\"inputTokens\":5,\"outputTokens\":2,\"inputTokenDetails\":{\"cacheReadTokens\":1,\"cacheWriteTokens\":1}}}\n"))
	}))
	defer server.Close()

	exec := NewCommandCodeExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "user_test", "base_url": server.URL}}
	input := []byte(`{"model":"deepseek/deepseek-v4-flash","messages":[{"role":"user","content":"hello"}]}`)
	resp, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "deepseek/deepseek-v4-flash", Payload: input}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), OriginalRequest: input})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if !json.Valid(resp.Payload) {
		t.Fatalf("response is not valid JSON: %s", resp.Payload)
	}
	root := gjson.ParseBytes(resp.Payload)
	if got := root.Get("choices.0.message.content").String(); got != "answer" {
		t.Fatalf("content = %q, want answer; body=%s", got, resp.Payload)
	}
	if got := root.Get("choices.0.finish_reason").String(); got != "length" {
		t.Fatalf("finish_reason = %q, want length", got)
	}
	if got := root.Get("usage.total_tokens").Int(); got != 9 {
		t.Fatalf("usage.total_tokens = %d, want 9", got)
	}
}
