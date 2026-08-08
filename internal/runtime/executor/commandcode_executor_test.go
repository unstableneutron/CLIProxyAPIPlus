package executor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

type commandCodeFailingReader struct{}

func (commandCodeFailingReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}

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
	if got := strings.Join(models[0].Thinking.Levels, ","); got != "high,max" {
		t.Fatalf("deepseek thinking levels = %q, want static metadata", got)
	}
	if got := strings.Join(models[0].SupportedInputModalities, ","); got != "text" {
		t.Fatalf("deepseek modalities = %q, want text-only static metadata", got)
	}
	if got := models[1].ID; got != "MiniMaxAI/MiniMax-M3" {
		t.Fatalf("second id = %q", got)
	}
	if models[1].Thinking != nil {
		t.Fatalf("MiniMax should not overclaim thinking support: %+v", models[1].Thinking)
	}
	if got := strings.Join(models[1].SupportedInputModalities, ","); got != "text,image" {
		t.Fatalf("MiniMax modalities = %q, want static metadata", got)
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
	if got := root.Get("params.max_tokens").Int(); got != 500000 {
		t.Fatalf("max_tokens = %d, want explicit 500000", got)
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
	if got := root.Get("params.messages.1.content.#(toolCallId==\"orphan\")").Raw; got == "" {
		t.Fatalf("expected orphaned assistant tool call to be preserved")
	}
	if got := root.Get("params.messages.2.content.0.output.value").String(); !strings.Contains(got, "No result") {
		t.Fatalf("synthetic orphan tool result = %q", got)
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
	if usageDetail.InputTokens != 0 || usageDetail.OutputTokens != 0 || usageDetail.TotalTokens != 0 {
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

	additive := commandCodeUsage{InputTokens: 100, CacheReadTokens: 40, CacheWriteTokens: 10, OutputTokens: 30, ReasoningTokens: 12, TotalTokens: 180}.openAIUsage()
	if additive["prompt_tokens"] != int64(150) || additive["completion_tokens"] != int64(30) || additive["total_tokens"] != int64(180) {
		t.Fatalf("additive OpenAI usage = %#v", additive)
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

func TestCommandCodeBuildPayloadMatchesV115WireDefaults(t *testing.T) {
	input := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"reasoning_effort":"xhigh"}`)
	payload, err := buildCommandCodePayload(commandCodePayloadOptions{Model: "gpt-5.5", Payload: input, ThreadID: "not-a-uuid"})
	if err != nil {
		t.Fatalf("buildCommandCodePayload() error = %v", err)
	}
	root := gjson.ParseBytes(payload)
	if value := root.Get("memory"); !value.Exists() || value.Type != gjson.Null {
		t.Fatalf("memory = %s, want null", value.Raw)
	}
	if value := root.Get("taste"); !value.Exists() || value.Type != gjson.Null {
		t.Fatalf("taste = %s, want null", value.Raw)
	}
	if value := root.Get("skills"); !value.Exists() || value.Type != gjson.Null {
		t.Fatalf("skills = %s, want null", value.Raw)
	}
	if got := root.Get("threadId").String(); got == "" || !strings.Contains(got, "-") {
		t.Fatalf("threadId = %q, want UUID", got)
	}
	if got := root.Get("params.max_tokens").Int(); got != commandCodeMaxTokensCap {
		t.Fatalf("default max_tokens = %d, want %d", got, commandCodeMaxTokensCap)
	}
	if got := root.Get("params.reasoning_effort").String(); got != "xhigh" {
		t.Fatalf("reasoning_effort = %q, want xhigh", got)
	}
	if got := root.Get("params.messages.0.content.0.type").String(); got != "text" {
		t.Fatalf("text part type = %q, want text", got)
	}
}

func TestCommandCodeImageContentChatAndResponsesPrepareRequest(t *testing.T) {
	imageURL := "data:image/png;base64,AAAA"
	chat := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"` + imageURL + `"}}]}]}`)
	payload, err := buildCommandCodePayload(commandCodePayloadOptions{Model: "gpt-5.5", Payload: chat})
	if err != nil {
		t.Fatalf("chat payload error = %v", err)
	}
	root := gjson.ParseBytes(payload)
	if got := root.Get("params.messages.0.content.1.type").String(); got != "image" {
		t.Fatalf("chat image type = %q", got)
	}
	if got := root.Get("params.messages.0.content.1.image").String(); got != imageURL {
		t.Fatalf("chat image = %q", got)
	}
	if got := root.Get("params.messages.0.content.1.mimeType").String(); got != "image/png" {
		t.Fatalf("chat mimeType = %q", got)
	}

	responses := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"` + imageURL + `"}]}]}`)
	exec := NewCommandCodeExecutor(&config.Config{})
	prepared, err := exec.prepareRequest(context.Background(), &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test"}}, cliproxyexecutor.Request{Model: "gpt-5.5", Payload: responses}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), OriginalRequest: responses}, true)
	if err != nil {
		t.Fatalf("responses prepareRequest error = %v", err)
	}
	root = gjson.ParseBytes(prepared.commandBody)
	if got := root.Get("params.messages.0.content.1.type").String(); got != "image" {
		t.Fatalf("responses image type = %q, body=%s", got, prepared.commandBody)
	}
	if got := root.Get("params.messages.0.content.1.image").String(); got != imageURL {
		t.Fatalf("responses image = %q", got)
	}
}

func TestCommandCodeHeadersUseV115FacadeAndStableSession(t *testing.T) {
	t.Setenv("CMD_ZDR", "1")
	t.Setenv("COMMAND_CODE_API_KEY", "canonical-key")
	t.Setenv("COMMANDCODE_API_KEY", "legacy-key")
	t.Setenv("COMMANDCODE_API_URL", "https://canonical.example")
	t.Setenv("COMMANDCODE_API_BASE", "https://legacy.example")
	identity := "client-session-secret-like"
	opts := cliproxyexecutor.Options{Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: identity}}
	first := commandCodeThreadID(opts, []byte(`{"messages":[{"role":"user","content":"hello"}]}`))
	second := commandCodeThreadID(opts, []byte(`{"messages":[{"role":"user","content":"different"}]}`))
	if first != second || first == identity || !isUUID(first) {
		t.Fatalf("thread IDs = %q/%q, want stable derived UUID", first, second)
	}
	base, key := resolveCommandCodeCredentials(nil)
	if key != "canonical-key" || base != "https://canonical.example" {
		t.Fatalf("env credentials = %q/%q", base, key)
	}
	request := httptest.NewRequest(http.MethodPost, "http://example.test", nil)
	request.Header.Set("x-session-id", first)
	applyCommandCodeHeaders(request, nil)
	if got := request.Header.Get("User-Agent"); got != "cli" {
		t.Fatalf("User-Agent = %q", got)
	}
	if got := request.Header.Get("x-command-code-version"); got != "1.15.0" {
		t.Fatalf("version = %q", got)
	}
	if got := request.Header.Get("x-cmd-zdr"); got != "1" {
		t.Fatalf("x-cmd-zdr = %q", got)
	}
	if got := request.Header.Get("x-session-id"); got != first {
		t.Fatalf("session = %q, want %q", got, first)
	}
}

func TestCommandCodeProviderExecutedToolIsNotReemitted(t *testing.T) {
	input := []byte(`{"model":"gpt-5.5","messages":[{"role":"assistant","content":"","tool_calls":[{"id":"server-1","type":"function","providerExecuted":true,"function":{"name":"web_search","arguments":"{}"}}]},{"role":"tool","tool_call_id":"server-1","providerExecuted":true,"name":"web_search","content":"server result"}]}`)
	payload, err := buildCommandCodePayload(commandCodePayloadOptions{Model: "gpt-5.5", Payload: input})
	if err != nil {
		t.Fatalf("payload error = %v", err)
	}
	messages := gjson.GetBytes(payload, "params.messages")
	if messages.IsArray() && len(messages.Array()) != 0 {
		t.Fatalf("provider-executed history should not be re-emitted: %s", messages.Raw)
	}
	state := newCommandCodeStreamState("gpt-5.5")
	chunks, _, err := commandCodeLineToOpenAIChunks([]byte(`{"type":"tool-call","toolCallId":"server-1","toolName":"web_search","providerExecuted":true,"input":{}}`), state)
	if err != nil || len(chunks) != 0 {
		t.Fatalf("provider-executed stream tool chunks = %d, err=%v", len(chunks), err)
	}
	chunks, _, err = commandCodeLineToOpenAIChunks([]byte(`{"type":"tool-result","toolCallId":"server-1","toolName":"web_search","providerExecuted":true,"output":{"type":"text","value":"server result"}}`), state)
	if err != nil || len(chunks) != 0 {
		t.Fatalf("provider-executed result policy = %d chunks, err=%v; want safely consumed without a client tool", len(chunks), err)
	}
}

func TestCommandCodeMissingFinishFailsStreamAndNonStream(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `{"type":"text-delta","text":"partial"}`+"\n")
	}))
	defer server.Close()
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	input := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`)
	exec := NewCommandCodeExecutor(&config.Config{})
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.5", Payload: input}, cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FromString("openai"), OriginalRequest: input})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var streamErr error
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			streamErr = chunk.Err
		}
	}
	if streamErr == nil {
		t.Fatal("missing finish stream should return an error chunk")
	}
	if status, ok := streamErr.(interface{ StatusCode() int }); !ok || status.StatusCode() != http.StatusBadGateway {
		t.Fatalf("stream error = %v, want status 502", streamErr)
	}
	if _, err = exec.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.5", Payload: input}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), OriginalRequest: input}); err == nil {
		t.Fatal("missing finish non-stream should fail")
	}
}

func TestCommandCodeMidStreamReadFailureHasBadGatewayStatus(t *testing.T) {
	_, _, err := collectCommandCodeResponse(context.Background(), &config.Config{}, commandCodeFailingReader{}, "gpt-5.5")
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusBadGateway {
		t.Fatalf("mid-stream read error = %v, want status 502", err)
	}
}

func TestCommandCodeStructuredStreamErrorPreservesStatus(t *testing.T) {
	state := newCommandCodeStreamState("gpt-5.5")
	_, _, err := commandCodeLineToOpenAIChunks([]byte(`{"type":"error","statusCode":429,"error":{"message":"rate limited"}}`), state)
	if err == nil {
		t.Fatal("expected structured error")
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("error = %v, want status 429", err)
	}

	_, _, err = commandCodeLineToOpenAIChunks([]byte(`{"type":"error","message":"mock mid-stream failure","statusCode":503,"isRetryable":false}`), state)
	status, ok = err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusServiceUnavailable || !strings.Contains(err.Error(), "mock mid-stream failure") {
		t.Fatalf("root-message error = %v, want message and status 503", err)
	}

	_, _, err = commandCodeLineToOpenAIChunks([]byte(`{"type":"error","error":{"message":"unclassified failure"}}`), state)
	status, ok = err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusInternalServerError {
		t.Fatalf("unclassified error = %v, want status 500", err)
	}

	_, _, err = commandCodeLineToOpenAIChunks([]byte(`{"type":"error","error":"401 {\"error\":{\"message\":\"unauthorized\"}}"}`), state)
	status, ok = err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusUnauthorized || !strings.Contains(err.Error(), "error: unauthorized") {
		t.Fatalf("embedded error = %v, want formatted message and status 401", err)
	}

	_, _, err = commandCodeLineToOpenAIChunks([]byte(`{"type":"error","statusCode":200,"message":"not actually successful"}`), state)
	status, ok = err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusBadGateway {
		t.Fatalf("invalid-2xx error = %v, want status 502", err)
	}
}

func TestCommandCodePauseTurnContinuationAccumulatesUsageAndThread(t *testing.T) {
	var mu sync.Mutex
	var count int
	var sessions []string
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		count++
		n := count
		sessions = append(sessions, r.Header.Get("x-session-id"))
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			_, _ = io.WriteString(w, `{"type":"text-delta","text":"one"}`+"\n")
			_, _ = io.WriteString(w, `{"type":"finish","finishReason":"pause_turn","totalUsage":{"inputTokens":2,"outputTokens":1,"totalTokens":3}}`+"\n")
			return
		}
		_, _ = io.WriteString(w, `{"type":"text-delta","text":"two"}`+"\n")
		_, _ = io.WriteString(w, `{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":4,"outputTokens":2,"totalTokens":6}}`+"\n")
	}))
	defer server.Close()
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	input := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`)
	exec := NewCommandCodeExecutor(&config.Config{})
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.5", Payload: input}, cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FromString("openai"), OriginalRequest: input, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: "pause-session"}})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var chunks []string
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("continuation stream error: %v", chunk.Err)
		}
		chunks = append(chunks, string(chunk.Payload))
	}
	mu.Lock()
	defer mu.Unlock()
	if count != 2 {
		t.Fatalf("request count = %d, want 2", count)
	}
	if sessions[0] == "" || sessions[0] != sessions[1] {
		t.Fatalf("sessions = %#v, want one stable session", sessions)
	}
	if gjson.GetBytes(bodies[0], "threadId").String() != gjson.GetBytes(bodies[1], "threadId").String() {
		t.Fatalf("thread IDs differ: %s / %s", bodies[0], bodies[1])
	}
	if got := strings.Join(chunks, ""); !strings.Contains(got, "one") || !strings.Contains(got, "two") {
		t.Fatalf("continuation chunks missing content: %v", chunks)
	}
	var usageFound bool
	for _, chunk := range chunks {
		if gjson.Get(chunk, "usage.total_tokens").Int() == 9 {
			usageFound = true
		}
	}
	if !usageFound {
		t.Fatalf("expected accumulated usage total 9 in chunks: %v", chunks)
	}
}

func TestCommandCodePauseTurnExhaustionReturnsStop(t *testing.T) {
	var mu sync.Mutex
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requestCount++
		mu.Unlock()
		_, _ = io.WriteString(w, `{"type":"finish","finishReason":"pause_turn","totalUsage":{"inputTokens":1}}`+"\n")
	}))
	defer server.Close()

	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	input := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`)
	exec := NewCommandCodeExecutor(&config.Config{})
	result, err := exec.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.5", Payload: input}, cliproxyexecutor.Options{Stream: true, SourceFormat: sdktranslator.FromString("openai"), OriginalRequest: input})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	finishReason := ""
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("pause exhaustion stream error: %v", chunk.Err)
		}
		if reason := gjson.GetBytes(chunk.Payload, "choices.0.finish_reason").String(); reason != "" {
			finishReason = reason
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if requestCount != commandCodeContinuationLimit+1 {
		t.Fatalf("request count = %d, want %d", requestCount, commandCodeContinuationLimit+1)
	}
	if finishReason != "stop" {
		t.Fatalf("pause exhaustion finish_reason = %q, want stop", finishReason)
	}
}

func TestCommandCodeNonStreamContinuationTransportFailureReturnsError(t *testing.T) {
	var mu sync.Mutex
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		requestCount++
		current := requestCount
		mu.Unlock()
		if current == 1 {
			_, _ = io.WriteString(w, `{"type":"finish","finishReason":"pause_turn"}`+"\n")
			return
		}
		conn, _, errHijack := w.(http.Hijacker).Hijack()
		if errHijack != nil {
			t.Errorf("hijack continuation response: %v", errHijack)
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()

	auth := &cliproxyauth.Auth{Attributes: map[string]string{"api_key": "test", "base_url": server.URL}}
	input := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`)
	exec := NewCommandCodeExecutor(&config.Config{})
	_, err := exec.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.5", Payload: input}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai"), OriginalRequest: input})
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusBadGateway {
		t.Fatalf("continuation transport failure = %v, want status 502", err)
	}
}

func TestCommandCodeCapabilityGatesImagesAndReasoning(t *testing.T) {
	textOnlyInput := []byte(`{"model":"deepseek/deepseek-v4-flash","reasoning_effort":"low","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	payload, err := buildCommandCodePayload(commandCodePayloadOptions{Model: "deepseek/deepseek-v4-flash", Payload: textOnlyInput})
	if err != nil {
		t.Fatalf("text-only payload error = %v", err)
	}
	root := gjson.ParseBytes(payload)
	if root.Get("params.messages.0.content.0.type").String() != "text" || !strings.Contains(root.Get("params.messages.0.content.0.text").String(), "image omitted") {
		t.Fatalf("text-only image handling = %s", root.Get("params.messages.0.content.0").Raw)
	}
	if root.Get("params.reasoning_effort").Exists() {
		t.Fatalf("text-only model should not receive reasoning_effort: %s", payload)
	}

	visionInput := []byte(`{"model":"gpt-5.5","reasoning_effort":"XHIGH","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`)
	payload, err = buildCommandCodePayload(commandCodePayloadOptions{Model: "gpt-5.5", Payload: visionInput})
	if err != nil {
		t.Fatalf("vision payload error = %v", err)
	}
	root = gjson.ParseBytes(payload)
	if root.Get("params.messages.0.content.0.type").String() != "image" {
		t.Fatalf("vision image handling = %s", root.Get("params.messages.0.content.0").Raw)
	}
	if got := root.Get("params.reasoning_effort").String(); got != "xhigh" {
		t.Fatalf("vision reasoning_effort = %q, want xhigh", got)
	}

	lagunaInput := []byte(`{"model":"poolside/laguna-s-2.1-free","max_tokens":64000,"messages":[{"role":"user","content":"hello"}]}`)
	payload, err = buildCommandCodePayload(commandCodePayloadOptions{Model: "poolside/laguna-s-2.1-free", Payload: lagunaInput})
	if err != nil {
		t.Fatalf("Laguna payload error = %v", err)
	}
	if got := gjson.GetBytes(payload, "params.max_tokens").Int(); got != 64000 {
		t.Fatalf("Laguna explicit max_tokens = %d, want 64000", got)
	}
}

func TestCommandCodeToolInputCoercionMatchesV115(t *testing.T) {
	arraySchema := map[string]any{
		"required":   []any{"paths"},
		"properties": map[string]any{"paths": map[string]any{"type": "array"}},
	}
	if got := commandCodeToolInput(json.RawMessage(`"README.md"`), arraySchema); len(got) != 1 {
		t.Fatalf("array-root string coercion = %#v", got)
	} else if paths, ok := got["paths"].([]string); !ok || len(paths) != 1 || paths[0] != "README.md" {
		t.Fatalf("array-root string coercion = %#v", got)
	}
	if got := commandCodeToolInput(json.RawMessage(`[{"q":"go"}]`), nil); got["q"] != "go" {
		t.Fatalf("single-object array coercion = %#v", got)
	}
	if got := commandCodeToolInput(json.RawMessage(`null`), nil); len(got) != 0 {
		t.Fatalf("null coercion = %#v, want empty object", got)
	}

	state := newCommandCodeStreamState("gpt-5.5")
	state.toolSchemas = map[string]map[string]any{"read_files": arraySchema}
	chunks, _, err := commandCodeLineToOpenAIChunks([]byte(`{"type":"tool-call","toolCallId":"call_1","toolName":"read_files","input":"README.md"}`), state)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("stream string coercion: chunks=%d err=%v", len(chunks), err)
	}
	arguments := gjson.GetBytes(chunks[0], "choices.0.delta.tool_calls.0.function.arguments").String()
	if got := gjson.Get(arguments, "paths.0").String(); got != "README.md" {
		t.Fatalf("stream string coercion arguments = %s", arguments)
	}

	anonymous := newCommandCodeStreamState("gpt-5.5")
	first, _, err := commandCodeLineToOpenAIChunks([]byte(`{"type":"tool-call","toolName":"one","input":{}}`), anonymous)
	if err != nil || len(first) != 1 {
		t.Fatalf("first anonymous tool call: chunks=%d err=%v", len(first), err)
	}
	second, _, err := commandCodeLineToOpenAIChunks([]byte(`{"type":"tool-call","toolName":"two","input":{}}`), anonymous)
	if err != nil || len(second) != 1 {
		t.Fatalf("second anonymous tool call: chunks=%d err=%v", len(second), err)
	}
	firstID := gjson.GetBytes(first[0], "choices.0.delta.tool_calls.0.id").String()
	secondID := gjson.GetBytes(second[0], "choices.0.delta.tool_calls.0.id").String()
	if firstID == "" || secondID == "" || firstID == secondID {
		t.Fatalf("anonymous tool IDs = %q, %q; want distinct", firstID, secondID)
	}
}

func TestCommandCodeIgnoresEventsAfterTerminal(t *testing.T) {
	state := newCommandCodeStreamState("gpt-5.5")
	finish := []byte(`{"type":"finish","finishReason":"stop","totalUsage":{"inputTokens":2,"outputTokens":1,"totalTokens":3}}`)
	chunks, _, err := commandCodeLineToOpenAIChunks(finish, state)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("first finish: chunks=%d err=%v", len(chunks), err)
	}
	chunks, _, err = commandCodeLineToOpenAIChunks(finish, state)
	if err != nil || len(chunks) != 0 {
		t.Fatalf("duplicate finish: chunks=%d err=%v", len(chunks), err)
	}
	chunks, _, err = commandCodeLineToOpenAIChunks([]byte(`{"type":"text-delta","text":"late"}`), state)
	if err != nil || len(chunks) != 0 || state.Text.String() != "" {
		t.Fatalf("post-terminal text: chunks=%d text=%q err=%v", len(chunks), state.Text.String(), err)
	}
	if state.Usage.TotalTokens != 3 {
		t.Fatalf("usage total after duplicate finish = %d, want 3", state.Usage.TotalTokens)
	}
	_, _, err = commandCodeLineToOpenAIChunks([]byte(`{"type":"error","statusCode":503,"message":"late transport failure"}`), state)
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusServiceUnavailable {
		t.Fatalf("post-terminal error = %v, want status 503", err)
	}
}

func TestCommandCodeAbortEmitsTerminalFinish(t *testing.T) {
	state := newCommandCodeStreamState("gpt-5.5")
	chunks, _, err := commandCodeLineToOpenAIChunks([]byte(`{"type":"text-delta","text":"partial"}`), state)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("text delta = %d, err=%v", len(chunks), err)
	}
	chunks, _, err = commandCodeLineToOpenAIChunks([]byte(`{"type":"abort"}`), state)
	if err != nil || len(chunks) != 1 {
		t.Fatalf("abort chunks = %d, err=%v", len(chunks), err)
	}
	if got := gjson.GetBytes(chunks[0], "choices.0.finish_reason").String(); got != "stop" {
		t.Fatalf("abort finish_reason = %q", got)
	}
}

func TestCommandCodeDoneDoesNotHideTruncatedStream(t *testing.T) {
	state := newCommandCodeStreamState("gpt-5.5")
	if _, _, err := commandCodeLineToOpenAIChunks([]byte("data: [DONE]"), state); err != nil {
		t.Fatalf("DONE error = %v", err)
	}
	if state.Terminal || !state.SawDone {
		t.Fatalf("state after DONE = %+v, want diagnostic marker without terminal", state)
	}
	reader := strings.NewReader(`{"type":"text-delta","text":"partial"}` + "\n" + "[DONE]\n")
	if _, _, err := collectCommandCodeResponse(context.Background(), &config.Config{}, reader, "gpt-5.5"); err == nil {
		t.Fatal("text followed only by DONE should be rejected as truncated")
	}
}

func TestCommandCodeAcceptsLargeNDJSONLine(t *testing.T) {
	text := strings.Repeat("x", 5*1024*1024)
	reader := strings.NewReader(`{"type":"text-delta","text":"` + text + `"}` + "\n" + `{"type":"finish","finishReason":"stop"}` + "\n")
	response, _, err := collectCommandCodeResponse(context.Background(), &config.Config{}, reader, "gpt-5.5")
	if err != nil {
		t.Fatalf("large line response error = %v", err)
	}
	if got := gjson.GetBytes(response, "choices.0.message.content").String(); len(got) != len(text) {
		t.Fatalf("content length = %d, want %d", len(got), len(text))
	}
}

func TestCommandCodeRejectsOversizedNDJSONLine(t *testing.T) {
	reader := strings.NewReader(strings.Repeat("x", commandCodeMaxStreamLineBytes+1))
	called := false
	err := commandCodeReadLines(reader, func([]byte) error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatalf("oversized line: err=%v called=%v", err, called)
	}
	status, ok := err.(interface{ StatusCode() int })
	if !ok || status.StatusCode() != http.StatusBadGateway {
		t.Fatalf("oversized line error = %v, want status 502", err)
	}
}
