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
