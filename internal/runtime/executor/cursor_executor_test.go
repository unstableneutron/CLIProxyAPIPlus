package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	responsesconverter "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/openai/openai/responses"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCursorClientVersionHeaderAllowsEnvOverride(t *testing.T) {
	t.Setenv("CLIPROXY_CURSOR_CLIENT_VERSION", "9.9.9")
	if got := cursorClientVersionHeader(); got != "cli-9.9.9" {
		t.Fatalf("cursorClientVersionHeader() = %q, want cli-9.9.9", got)
	}

	t.Setenv("CLIPROXY_CURSOR_CLIENT_VERSION", "cli-8.8.8")
	if got := cursorClientVersionHeader(); got != "cli-8.8.8" {
		t.Fatalf("cursorClientVersionHeader() = %q, want cli-8.8.8", got)
	}
}

func TestCursorBuildNonStreamingTextCompletionIncludesReasoningAndUsage(t *testing.T) {
	payload := cursorBuildNonStreamingTextCompletion("chatcmpl-test", 123, "cursor-composer-2.5", "answer", "thinking", 11, 7)

	var decoded struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens            int `json:"prompt_tokens"`
			CompletionTokens        int `json:"completion_tokens"`
			TotalTokens             int `json:"total_tokens"`
			CompletionTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; payload=%s", err, payload)
	}
	if decoded.Choices[0].Message.Content != "answer" {
		t.Fatalf("content = %q, want answer", decoded.Choices[0].Message.Content)
	}
	if decoded.Choices[0].Message.ReasoningContent != "thinking" {
		t.Fatalf("reasoning_content = %q, want thinking", decoded.Choices[0].Message.ReasoningContent)
	}
	if decoded.Usage.PromptTokens != 11 || decoded.Usage.CompletionTokens != 7 || decoded.Usage.TotalTokens != 18 {
		t.Fatalf("usage = %+v, want 11/7/18", decoded.Usage)
	}
	if decoded.Usage.CompletionTokensDetails.ReasoningTokens == 0 {
		t.Fatal("reasoning_tokens = 0, want non-zero")
	}
}

func TestCursorConversationIDUsesExecutionSessionMetadata(t *testing.T) {
	req := cliproxyexecutor.Request{Payload: []byte(`{
		"model":"cursor-composer-2.5",
		"messages":[
			{"role":"system","content":"same system prompt"},
			{"role":"user","content":"hello"}
		]
	}`)}
	optsA := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.ExecutionSessionMetadataKey: "ws-session-a",
	}}
	optsB := cliproxyexecutor.Options{Metadata: map[string]any{
		cliproxyexecutor.ExecutionSessionMetadataKey: "ws-session-b",
	}}

	gotA := resolveCursorConversation("api-key", "same system prompt", req, optsA)
	gotB := resolveCursorConversation("api-key", "same system prompt", req, optsB)
	fallback := resolveCursorConversation("api-key", "same system prompt", req, cliproxyexecutor.Options{})

	if gotA.ConversationID == gotB.ConversationID {
		t.Fatalf("conversation IDs matched for different execution sessions: %q", gotA.ConversationID)
	}
	if gotA.ConversationID == fallback.ConversationID || gotB.ConversationID == fallback.ConversationID {
		t.Fatalf("execution sessions fell back to system prompt hash: a=%q b=%q fallback=%q", gotA.ConversationID, gotB.ConversationID, fallback.ConversationID)
	}
	if gotA.ExecutionSessionID != "ws-session-a" || gotB.ExecutionSessionID != "ws-session-b" {
		t.Fatalf("execution session tags = %q/%q, want ws-session-a/ws-session-b", gotA.ExecutionSessionID, gotB.ExecutionSessionID)
	}
}

func TestCursorConversationIDPrefersClaudeSessionMetadata(t *testing.T) {
	req := cliproxyexecutor.Request{Payload: []byte(`{
		"metadata":{"user_id":"{\"session_id\":\"claude-session\",\"device_id\":\"device\"}"},
		"prompt_cache_key":"cache-session",
		"messages":[{"role":"user","content":"hello"}]
	}`)}
	opts := cliproxyexecutor.Options{
		Headers: http.Header{
			"X-Session-Id": []string{"header-session"},
		},
		Metadata: map[string]any{
			cliproxyexecutor.ExecutionSessionMetadataKey: "execution-session",
		},
	}

	got := resolveCursorConversation("api-key", "system", req, opts)
	wantConversationID := deriveConversationId("api-key", "claude-session", "system")

	if got.ConversationID != wantConversationID {
		t.Fatalf("ConversationID = %q, want explicit Claude session %q", got.ConversationID, wantConversationID)
	}
	if got.SessionSource != "metadata.user_id.session_id" {
		t.Fatalf("SessionSource = %q, want metadata.user_id.session_id", got.SessionSource)
	}
	if got.ExecutionSessionID != "execution-session" {
		t.Fatalf("ExecutionSessionID = %q, want execution-session", got.ExecutionSessionID)
	}
}

func TestCursorConversationIDUsesStableSessionHeaders(t *testing.T) {
	req := cliproxyexecutor.Request{Payload: []byte(`{"messages":[{"role":"user","content":"hello"}]}`)}

	for _, headerName := range []string{"X-Session-ID", "Session_id"} {
		t.Run(headerName, func(t *testing.T) {
			headersA := make(http.Header)
			headersA.Set(headerName, "stable-session")
			headersB := make(http.Header)
			headersB.Set(headerName, "stable-session")
			headersC := make(http.Header)
			headersC.Set(headerName, "other-session")
			optsA := cliproxyexecutor.Options{Headers: headersA}
			optsB := cliproxyexecutor.Options{Headers: headersB}
			optsC := cliproxyexecutor.Options{Headers: headersC}

			gotA := resolveCursorConversation("api-key", "system", req, optsA)
			gotB := resolveCursorConversation("api-key", "system", req, optsB)
			gotC := resolveCursorConversation("api-key", "system", req, optsC)

			if gotA.ConversationID != gotB.ConversationID {
				t.Fatalf("same %s produced different conversation IDs: %q vs %q", headerName, gotA.ConversationID, gotB.ConversationID)
			}
			if gotA.ConversationID == gotC.ConversationID {
				t.Fatalf("different %s values produced same conversation ID: %q", headerName, gotA.ConversationID)
			}
		})
	}
}

func TestCursorCloseExecutionSessionRemovesTaggedSessionsAndCheckpoints(t *testing.T) {
	exec := &CursorExecutor{
		sessions:    make(map[string]*cursorSession),
		checkpoints: make(map[string]*savedCheckpoint),
	}
	canceled := 0
	exec.sessions["auth-a:conv-a"] = &cursorSession{
		conversationID:     "conv-a",
		executionSessionID: "execution-a",
		cancel: func() {
			canceled++
		},
	}
	exec.sessions["auth-b:conv-b"] = &cursorSession{
		conversationID:     "conv-b",
		executionSessionID: "execution-b",
		cancel: func() {
			canceled++
		},
	}
	exec.checkpoints["conv-a"] = &savedCheckpoint{executionSessionID: "execution-a"}
	exec.checkpoints["conv-b"] = &savedCheckpoint{executionSessionID: "execution-b"}

	exec.CloseExecutionSession("execution-a")

	if _, ok := exec.sessions["auth-a:conv-a"]; ok {
		t.Fatal("session tagged with execution-a was not removed")
	}
	if _, ok := exec.checkpoints["conv-a"]; ok {
		t.Fatal("checkpoint tagged with execution-a was not removed")
	}
	if _, ok := exec.sessions["auth-b:conv-b"]; !ok {
		t.Fatal("session tagged with execution-b was removed")
	}
	if _, ok := exec.checkpoints["conv-b"]; !ok {
		t.Fatal("checkpoint tagged with execution-b was removed")
	}
	if canceled != 1 {
		t.Fatalf("canceled = %d, want 1", canceled)
	}
}

func TestCursorCloseExecutionSessionBySessionKeyRemovesCheckpoint(t *testing.T) {
	exec := &CursorExecutor{
		sessions:    make(map[string]*cursorSession),
		checkpoints: make(map[string]*savedCheckpoint),
	}
	canceled := 0
	exec.sessions["auth-a:conv-a"] = &cursorSession{
		conversationID:     "conv-a",
		executionSessionID: "execution-a",
		cancel: func() {
			canceled++
		},
	}
	exec.sessions["auth-b:conv-b"] = &cursorSession{
		conversationID:     "conv-b",
		executionSessionID: "execution-b",
		cancel: func() {
			canceled++
		},
	}
	exec.checkpoints["conv-a"] = &savedCheckpoint{executionSessionID: "execution-a"}
	exec.checkpoints["conv-b"] = &savedCheckpoint{executionSessionID: "execution-b"}

	exec.CloseExecutionSession("auth-a:conv-a")

	if _, ok := exec.sessions["auth-a:conv-a"]; ok {
		t.Fatal("session keyed by auth-a:conv-a was not removed")
	}
	if _, ok := exec.checkpoints["conv-a"]; ok {
		t.Fatal("checkpoint for conv-a was not removed")
	}
	if _, ok := exec.sessions["auth-b:conv-b"]; !ok {
		t.Fatal("session keyed by auth-b:conv-b was removed")
	}
	if _, ok := exec.checkpoints["conv-b"]; !ok {
		t.Fatal("checkpoint for conv-b was removed")
	}
	if canceled != 1 {
		t.Fatalf("canceled = %d, want 1", canceled)
	}
}

func TestCursorPayloadCandidatesDeduplicateExactSliceReferences(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":"hello"}]}`)
	copyOfPayload := append([]byte(nil), payload...)
	req := cliproxyexecutor.Request{Payload: payload}
	opts := cliproxyexecutor.Options{OriginalRequest: payload}

	candidates := cursorPayloadCandidates(req, opts, payload, copyOfPayload)

	if got, want := len(candidates), 2; got != want {
		t.Fatalf("len(candidates) = %d, want %d", got, want)
	}
	if &candidates[0][0] != &payload[0] {
		t.Fatal("first candidate is not the original payload slice")
	}
	if &candidates[1][0] != &copyOfPayload[0] {
		t.Fatal("second candidate is not the independent payload copy")
	}
}

func TestCursorCanResumeToolSessionRejectsClosedStreams(t *testing.T) {
	session := &cursorSession{
		authID: "cursor-a.json",
		pending: []pendingMcpExec{{
			ExecMsgId:  7,
			ExecId:     "exec-read",
			ToolCallId: "call_read",
		}},
	}
	results := []toolResultInfo{{ToolCallId: "call_read", Content: "ok"}}

	if cursorCanResumeToolSession(session, "cursor-a.json", results, true) {
		t.Fatal("closed stream session should not be resumable")
	}
	if !cursorCanResumeToolSession(session, "cursor-a.json", results, false) {
		t.Fatal("open stream session with matching tool result should be resumable")
	}
}

func TestCursorShouldEmitInteractionToolCallRequiresDeclaredClientTool(t *testing.T) {
	msg := &cursorproto.DecodedServerMessage{
		Type:                cursorproto.ServerMsgExecMcpArgs,
		McpToolName:         "readLints",
		McpToolCallId:       "call_read_lints",
		InteractionToolCall: true,
		McpArgs:             map[string][]byte{"paths": []byte(`["AGENTS.md"]`)},
	}

	if cursorShouldEmitMcpExec(msg, []cursorproto.McpToolDef{{Name: cursorOpenAIToolAliasPrefix + "readLints"}}) != true {
		t.Fatal("declared interaction tool call was not emittable")
	}
	if cursorShouldEmitMcpExec(msg, []cursorproto.McpToolDef{{Name: cursorOpenAIToolAliasPrefix + "read"}}) != false {
		t.Fatal("undeclared interaction tool call was emittable")
	}
}

func TestCursorShouldNotEmitDeclaredInteractionToolCallWithMissingRequiredArgs(t *testing.T) {
	msg := &cursorproto.DecodedServerMessage{
		Type:                cursorproto.ServerMsgExecMcpArgs,
		McpToolName:         "grep",
		McpToolCallId:       "call_empty_grep",
		InteractionToolCall: true,
		McpArgs:             map[string][]byte{},
	}

	if cursorShouldEmitMcpExec(msg, []cursorproto.McpToolDef{{Name: cursorOpenAIToolAliasPrefix + "grep"}}) {
		t.Fatal("declared interaction grep with no pattern was emittable")
	}

	msg.McpArgs = map[string][]byte{"pattern": []byte(`"InteractionUpdate"`)}
	if !cursorShouldEmitMcpExec(msg, []cursorproto.McpToolDef{{Name: cursorOpenAIToolAliasPrefix + "grep"}}) {
		t.Fatal("declared interaction grep with pattern was not emittable")
	}
}

func TestCursorInteractionToolCollectorEmitsWhenArgsDeltaIsComplete(t *testing.T) {
	collector := newCursorInteractionToolCollector()
	started := &cursorproto.DecodedServerMessage{
		Type:                cursorproto.ServerMsgExecMcpArgs,
		McpToolName:         "grep",
		McpToolCallId:       "call_grep",
		InteractionToolCall: true,
	}
	partial := &cursorproto.DecodedServerMessage{
		Type:                     cursorproto.ServerMsgExecMcpArgs,
		McpToolName:              "grep",
		McpToolCallId:            "call_grep",
		InteractionToolCall:      true,
		InteractionArgsTextDelta: `{"pattern":"InteractionUpdate"}`,
	}
	completed := &cursorproto.DecodedServerMessage{
		Type:                         cursorproto.ServerMsgExecMcpArgs,
		McpToolName:                  "grep",
		McpToolCallId:                "call_grep",
		InteractionToolCall:          true,
		InteractionToolCallCompleted: true,
	}

	if got := collector.absorb(started); got != nil {
		t.Fatalf("started update emitted %#v, want nil", got)
	}
	got := collector.absorb(partial)
	if got == nil {
		t.Fatal("complete args delta did not emit collected tool call")
	}
	if got.McpToolName != "grep" || got.McpToolCallId != "call_grep" {
		t.Fatalf("collected msg = %#v, want grep/call_grep", got)
	}
	if string(got.McpArgs["pattern"]) != `"InteractionUpdate"` {
		t.Fatalf("pattern arg = %q, want JSON string", got.McpArgs["pattern"])
	}
	if got := collector.absorb(completed); got != nil {
		t.Fatalf("completed update emitted duplicate %#v, want nil", got)
	}
}

func TestCursorShouldRejectExecMcpArgsWithMismatchedKnownToolArgs(t *testing.T) {
	msg := &cursorproto.DecodedServerMessage{
		Type:          cursorproto.ServerMsgExecMcpArgs,
		McpToolName:   cursorOpenAIToolAliasPrefix + "grep",
		McpToolCallId: "call_bad_grep",
		ExecMsgId:     8,
		ExecId:        "exec-grep",
		McpArgs: map[string][]byte{
			"block_until_ms": []byte(`"120000"`),
			"command":        []byte(`"cd /repo && gitchamber kaitranntt/ccs"`),
			"description":    []byte(`"Fetch ccs source via gitchamber"`),
		},
	}

	if cursorShouldEmitMcpExec(msg, []cursorproto.McpToolDef{{Name: cursorOpenAIToolAliasPrefix + "grep"}}) {
		t.Fatal("exec grep with bash-shaped args was emittable")
	}
}

func TestCursorShouldNormalizeSingularWebFetchURLArg(t *testing.T) {
	msg := &cursorproto.DecodedServerMessage{
		Type:          cursorproto.ServerMsgExecMcpArgs,
		McpToolName:   cursorOpenAIToolAliasPrefix + "web_fetch",
		McpToolCallId: "call_web_fetch",
		ExecMsgId:     9,
		ExecId:        "exec-web-fetch",
		McpArgs: map[string][]byte{
			"url":       []byte(`"https://example.com/a"`),
			"objective": []byte(`"Read example"`),
		},
	}

	if !cursorShouldEmitMcpExec(msg, []cursorproto.McpToolDef{{Name: cursorOpenAIToolAliasPrefix + "web_fetch"}}) {
		t.Fatal("web_fetch with singular url was not normalized and emitted")
	}
	var urls []string
	if err := json.Unmarshal(msg.McpArgs["urls"], &urls); err != nil {
		t.Fatalf("urls arg is not JSON string array: %v raw=%q", err, msg.McpArgs["urls"])
	}
	if len(urls) != 1 || urls[0] != "https://example.com/a" {
		t.Fatalf("urls = %#v, want one normalized URL", urls)
	}
}

func TestCursorShouldEmitExecMcpArgsWithoutDeclaredClientTool(t *testing.T) {
	msg := &cursorproto.DecodedServerMessage{
		Type:          cursorproto.ServerMsgExecMcpArgs,
		McpToolName:   cursorOpenAIToolAliasPrefix + "read",
		McpToolCallId: "call_read",
		ExecMsgId:     7,
		ExecId:        "exec-read",
		McpArgs:       map[string][]byte{"path": []byte(`"README.md"`)},
	}

	if !cursorShouldEmitMcpExec(msg, nil) {
		t.Fatal("exec mcpArgs should remain emittable regardless of declared client tool list")
	}
}

func TestCursorCanResumeToolSessionRejectsInteractionToolCallsWithoutExecMetadata(t *testing.T) {
	session := &cursorSession{
		authID: "cursor-a.json",
		pending: []pendingMcpExec{{
			ToolCallId: "call_read_lints",
			ToolName:   "readLints",
			Args:       `{"paths":["AGENTS.md"]}`,
		}},
	}
	results := []toolResultInfo{{ToolCallId: "call_read_lints", Content: "[]"}}

	if cursorCanResumeToolSession(session, "cursor-a.json", results, false) {
		t.Fatal("interaction tool calls without exec metadata must cold-resume instead of sending exec results")
	}
}

func TestCursorResumeWithToolResultsRejectsMissingStream(t *testing.T) {
	exec := &CursorExecutor{}
	session := &cursorSession{
		toolResultCh: make(chan []toolResultInfo, 1),
		resumeOutCh:  make(chan cliproxyexecutor.StreamChunk, 1),
	}
	parsed := &parsedOpenAIRequest{ToolResults: []toolResultInfo{{ToolCallId: "call_read", Content: "ok"}}}

	if _, err := exec.resumeWithToolResults(context.Background(), session, parsed, sdktranslator.FromString(""), sdktranslator.FromString(""), cliproxyexecutor.Request{}, nil, nil, false); err == nil {
		t.Fatal("resumeWithToolResults() error = nil, want missing/dead stream error")
	}
}

func TestCursorExecDeduperAllowsDistinctInteractionToolCalls(t *testing.T) {
	deduper := newCursorExecDeduper()
	first := &cursorproto.DecodedServerMessage{
		Type:          cursorproto.ServerMsgExecMcpArgs,
		McpToolCallId: "call_read_lints",
	}
	second := &cursorproto.DecodedServerMessage{
		Type:          cursorproto.ServerMsgExecMcpArgs,
		McpToolCallId: "call_read_lints_again",
	}

	if !deduper.mark(first) {
		t.Fatal("first interaction tool call was treated as duplicate")
	}
	if !deduper.mark(second) {
		t.Fatal("second interaction tool call with a different call id was treated as duplicate")
	}
	if deduper.mark(first) {
		t.Fatal("same interaction tool call id was not treated as duplicate")
	}
}

func TestCursorToolResultsMatchPendingCalls(t *testing.T) {
	pending := []pendingMcpExec{
		{ToolCallId: "call_a"},
		{ToolCallId: "call_b"},
	}
	if cursorToolResultsMatchPending([]toolResultInfo{{ToolCallId: "call_b", Content: "ok"}}, pending) {
		t.Fatal("partial tool results must not resume a batched pending call")
	}
	if !cursorToolResultsMatchPending([]toolResultInfo{{ToolCallId: "call_a", Content: "ok-a"}, {ToolCallId: "call_b", Content: "ok-b"}}, pending) {
		t.Fatal("expected complete tool result batch to match all pending calls")
	}
	if cursorToolResultsMatchPending([]toolResultInfo{{ToolCallId: "call_missing", Content: "ok"}}, pending) {
		t.Fatal("unexpected match for unknown tool call id")
	}
}

func TestCursorMatchingToolResultsFiltersHistoricalResults(t *testing.T) {
	pending := []pendingMcpExec{{ToolCallId: "call_current"}}
	results := []toolResultInfo{
		{ToolCallId: "call_old", Content: "old"},
		{ToolCallId: "call_current", Content: "current"},
		{ToolCallId: "call_other", Content: "other"},
	}

	matched := cursorMatchingToolResults(results, pending)

	if got, want := len(matched), 1; got != want {
		t.Fatalf("matched results = %d, want %d: %#v", got, want, matched)
	}
	if matched[0].ToolCallId != "call_current" || matched[0].Content != "current" {
		t.Fatalf("matched result = %#v, want current result only", matched[0])
	}
}

func TestCursorStreamingThinkingDeltaUsesReasoningContent(t *testing.T) {
	delta := cursorStreamingThinkingDeltaJSON("thinking text")

	if strings.Contains(delta, "<think>") || strings.Contains(delta, "</think>") {
		t.Fatalf("thinking delta must not be encoded as visible tags: %s", delta)
	}
	if gjson.Get(delta, "content").Exists() {
		t.Fatalf("thinking delta content exists = %s, want reasoning_content only", delta)
	}
	if got := gjson.Get(delta, "reasoning_content").String(); got != "thinking text" {
		t.Fatalf("reasoning_content = %q, want thinking text (delta=%s)", got, delta)
	}
}

func TestCursorStreamingTextDeltaUsesContent(t *testing.T) {
	delta := cursorStreamingTextDeltaJSON("visible text")

	if got := gjson.Get(delta, "content").String(); got != "visible text" {
		t.Fatalf("content = %q, want visible text (delta=%s)", got, delta)
	}
	if gjson.Get(delta, "reasoning_content").Exists() {
		t.Fatalf("text delta reasoning_content exists = %s, want content only", delta)
	}
}

func TestCursorStreamingToolCallDeltaUsesResponseScopedIndex(t *testing.T) {
	delta := cursorStreamingToolCallDeltaJSON(0, pendingMcpExec{
		ToolCallId: "call_read",
		ToolName:   "read",
		Args:       `{"path":"file.go"}`,
	})

	call := gjson.Get(delta, "tool_calls.0")
	if got := call.Get("index").Int(); got != 0 {
		t.Fatalf("tool call index = %d, want response-scoped 0 (delta=%s)", got, delta)
	}
	if got := call.Get("id").String(); got != "call_read" {
		t.Fatalf("tool call id = %q, want call_read", got)
	}
	if got := call.Get("function.name").String(); got != "read" {
		t.Fatalf("tool call function name = %q, want read", got)
	}
	if got := call.Get("function.arguments").String(); got != `{"path":"file.go"}` {
		t.Fatalf("tool call arguments = %q, want JSON string", got)
	}
}

func TestCursorStreamingToolCallDeltasSupportBatchedToolCalls(t *testing.T) {
	deltas := cursorStreamingToolCallDeltasJSON([]pendingMcpExec{
		{ToolCallId: "call_read", ToolName: "read", Args: `{"path":"file.go"}`},
		{ToolCallId: "call_grep", ToolName: "grep", Args: `{"pattern":"func"}`},
	})

	if got, want := len(deltas), 2; got != want {
		t.Fatalf("tool call deltas = %d, want %d: %#v", got, want, deltas)
	}
	for i, delta := range deltas {
		call := gjson.Get(delta, "tool_calls.0")
		if got := int(call.Get("index").Int()); got != i {
			t.Fatalf("delta %d index = %d, want %d (%s)", i, got, i, delta)
		}
	}
	if got := gjson.Get(deltas[0], "tool_calls.0.function.name").String(); got != "read" {
		t.Fatalf("first tool name = %q, want read", got)
	}
	if got := gjson.Get(deltas[1], "tool_calls.0.function.name").String(); got != "grep" {
		t.Fatalf("second tool name = %q, want grep", got)
	}
}

func TestBuildRunRequestParamsSkipsToolsWithoutFunctionNames(t *testing.T) {
	parsed := parseOpenAIRequest([]byte(`{
		"model":"cursor-composer-2.5",
		"messages":[{"role":"user","content":"Use a tool."}],
		"tools":[
			{"type":"function","function":{"name":"","description":"missing name","parameters":{"type":"object"}}},
			{"type":"file_search"},
			{"type":"function","function":{"name":"get_weather","description":"Get weather.","parameters":{"type":"object"}}}
		]
	}`))

	params := buildRunRequestParams(parsed, "conv-1")

	if got, want := len(params.McpTools), 1; got != want {
		t.Fatalf("McpTools = %d, want %d: %#v", got, want, params.McpTools)
	}
	if got := params.McpTools[0].Name; got != "mcp__get_weather" {
		t.Fatalf("remaining tool name = %q, want mcp__get_weather", got)
	}
	if strings.Contains(params.UserText, "mcp__tool") {
		t.Fatalf("UserText contains fallback alias for malformed tool: %q", params.UserText)
	}
}

func TestBuildRunRequestParamsPrefixesAllOpenAIToolNames(t *testing.T) {
	parsed := parseOpenAIRequest([]byte(`{
		"model":"cursor-composer-2.5",
		"messages":[{"role":"user","content":"Use web_search."}],
		"tools":[
			{"type":"function","function":{"name":"web_search","description":"Search the web.","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"get_weather","description":"Get weather.","parameters":{"type":"object"}}},
			{"type":"function","function":{"name":"mcp__custom","description":"Already prefixed original.","parameters":{"type":"object"}}}
		]
	}`))

	params := buildRunRequestParams(parsed, "conv-1")

	if got := params.McpTools[0].Name; got != "mcp__web_search" {
		t.Fatalf("aliased tool name = %q, want mcp__web_search", got)
	}
	if got := cursorOpenAIToolNameForMcpTool("mcp__web_search"); got != "web_search" {
		t.Fatalf("mapped tool name = %q, want web_search", got)
	}
	if got := params.McpTools[1].Name; got != "mcp__get_weather" {
		t.Fatalf("non-native tool alias = %q, want mcp__get_weather", got)
	}
	if got := cursorOpenAIToolNameForMcpTool("mcp__get_weather"); got != "get_weather" {
		t.Fatalf("mapped non-native tool name = %q, want get_weather", got)
	}
	if got := params.McpTools[2].Name; got != "mcp__mcp__custom" {
		t.Fatalf("already-prefixed original alias = %q, want mcp__mcp__custom", got)
	}
	if got := cursorOpenAIToolNameForMcpTool("mcp__mcp__custom"); got != "mcp__custom" {
		t.Fatalf("mapped already-prefixed original = %q, want mcp__custom", got)
	}
	if params.McpTools[0].Description != "Search the web." {
		t.Fatalf("tool description = %q, want original description only", params.McpTools[0].Description)
	}
	if strings.Contains(params.SystemPrompt, "External OpenAI tools are exposed to Cursor") || strings.Contains(params.UserText, "External OpenAI tools are exposed to Cursor") {
		t.Fatalf("alias instructions must not be injected into prompts: system=%q user=%q", params.SystemPrompt, params.UserText)
	}
}

func TestCursorBuildNonStreamingToolCallCompletion(t *testing.T) {
	payload := cursorBuildNonStreamingToolCallCompletion("chatcmpl-test", 123, "cursor-composer-2.5", []pendingMcpExec{{
		ToolCallId: "call_weather",
		ToolName:   "get_weather",
		Args:       `{"city":"Paris"}`,
	}})

	var decoded struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Role      string  `json:"role"`
				Content   *string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v; payload=%s", err, payload)
	}
	choice := decoded.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", choice.FinishReason)
	}
	if choice.Message.Content != nil {
		t.Fatalf("content = %q, want null", *choice.Message.Content)
	}
	call := choice.Message.ToolCalls[0]
	if call.ID != "call_weather" || call.Function.Name != "get_weather" || call.Function.Arguments != `{"city":"Paris"}` {
		t.Fatalf("tool call = %+v, want weather call with JSON string args", call)
	}
}

func TestParseTranslatedResponsesDeveloperDoesNotCreateHistoricalTurn(t *testing.T) {
	responsesPayload := []byte(`{
		"model":"cursor-composer-2.5",
		"input":[
			{"type":"message","role":"developer","content":[{"type":"input_text","text":"You are running in /workspace."}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"Inspect the proxy transport paths."}]}
		]
	}`)

	chatPayload := responsesconverter.ConvertOpenAIResponsesRequestToOpenAIChatCompletions("cursor-composer-2.5", responsesPayload, true)
	parsed := parseOpenAIRequest(chatPayload)

	if parsed.UserText != "Inspect the proxy transport paths." {
		t.Fatalf("UserText = %q, want actual user prompt", parsed.UserText)
	}
	if len(parsed.Turns) != 0 {
		t.Fatalf("Turns = %d, want no historical turns for developer+user responses input", len(parsed.Turns))
	}
	if !strings.Contains(parsed.SystemPrompt, "You are running in /workspace.") {
		t.Fatalf("SystemPrompt = %q, want developer instruction preserved as system context", parsed.SystemPrompt)
	}
}

func TestCursorFlattenMessagesPrependsSystemForSingleUser(t *testing.T) {
	parsed := parseOpenAIRequest([]byte(`{
		"model":"cursor-composer-2.5",
		"messages":[
			{"role":"system","content":"Always answer in uppercase."},
			{"role":"user","content":"hello"}
		]
	}`))

	flattenConversationIntoUserText(parsed)

	if parsed.UserText != "Always answer in uppercase.\n\nhello" {
		t.Fatalf("UserText = %q, want system prompt prepended to single user text", parsed.UserText)
	}
	if len(parsed.Turns) != 0 || len(parsed.ToolResults) != 0 {
		t.Fatalf("Turns/ToolResults not cleared: turns=%d tools=%d", len(parsed.Turns), len(parsed.ToolResults))
	}
}

func TestParseOpenAIRequestOnlyTreatsTrailingToolMessagesAsPendingResults(t *testing.T) {
	parsed := parseOpenAIRequest([]byte(`{
		"model":"cursor-composer-2.5",
		"messages":[
			{"role":"user","content":"inspect"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_old","type":"function","function":{"name":"ls","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_old","content":"old files"},
			{"role":"assistant","content":"I found old files."},
			{"role":"user","content":"now continue"}
		]
	}`))

	if len(parsed.ToolResults) != 0 {
		t.Fatalf("historical tool results parsed as pending: %#v", parsed.ToolResults)
	}
	if parsed.UserText != "now continue" {
		t.Fatalf("UserText = %q, want latest user turn", parsed.UserText)
	}
}

func TestParseOpenAIRequestKeepsOnlyTrailingToolResultsForLiveResume(t *testing.T) {
	parsed := parseOpenAIRequest([]byte(`{
		"model":"cursor-composer-2.5",
		"messages":[
			{"role":"user","content":"inspect"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_old","type":"function","function":{"name":"ls","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_old","content":"old files"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_a","type":"function","function":{"name":"read","arguments":"{}"}},{"id":"call_b","type":"function","function":{"name":"grep","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_a","content":"read result"},
			{"role":"tool","tool_call_id":"call_b","content":"grep result"}
		]
	}`))

	if got, want := len(parsed.ToolResults), 2; got != want {
		t.Fatalf("pending tool results = %d, want %d: %#v", got, want, parsed.ToolResults)
	}
	if parsed.ToolResults[0].ToolCallId != "call_a" || parsed.ToolResults[0].Content != "read result" {
		t.Fatalf("first pending result = %#v, want call_a", parsed.ToolResults[0])
	}
	if parsed.ToolResults[1].ToolCallId != "call_b" || parsed.ToolResults[1].Content != "grep result" {
		t.Fatalf("second pending result = %#v, want call_b", parsed.ToolResults[1])
	}
	if parsed.UserText != "" {
		t.Fatalf("UserText = %q, want empty while trailing tool results await live resume", parsed.UserText)
	}
}

func TestCursorFlattenMessagesIncludesAssistantToolCallsAndToolResults(t *testing.T) {
	parsed := parseOpenAIRequest([]byte(`{
		"model":"cursor-composer-2.5",
		"messages":[
			{"role":"system","content":"Be concise."},
			{"role":"user","content":"what is the weather?"},
			{"role":"assistant","content":"Let me check.","tool_calls":[{"id":"call_weather","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},
			{"role":"tool","tool_call_id":"call_weather","content":"sunny, 22C"},
			{"role":"user","content":"summarize"}
		]
	}`))

	flattenConversationIntoUserText(parsed)

	for _, want := range []string{
		"Be concise.",
		"User: what is the weather?",
		"Assistant: Let me check.",
		`Assistant called tool get_weather (call_weather) with arguments: {"city":"Paris"}`,
		"Tool result (call_weather): sunny, 22C",
		"User: summarize",
	} {
		if !strings.Contains(parsed.UserText, want) {
			t.Fatalf("flattened text missing %q in:\n%s", want, parsed.UserText)
		}
	}
	if len(parsed.Turns) != 0 || len(parsed.ToolResults) != 0 {
		t.Fatalf("Turns/ToolResults not cleared: turns=%d tools=%d", len(parsed.Turns), len(parsed.ToolResults))
	}
}

func TestCursorFlattenMessagesKeepsSingleUserFastPath(t *testing.T) {
	parsed := parseOpenAIRequest([]byte(`{
		"model":"cursor-composer-2.5",
		"messages":[{"role":"user","content":"hello"}]
	}`))

	flattenConversationIntoUserText(parsed)

	if parsed.UserText != "hello" {
		t.Fatalf("UserText = %q, want original single user text", parsed.UserText)
	}
}

func TestCursorJSONErrorFromPayloadMapsResourceExhausted(t *testing.T) {
	err := cursorJSONErrorFromPayload([]byte(`{"error":{"code":"resource_exhausted","message":"rate limited"}}`))
	if err == nil {
		t.Fatal("cursorJSONErrorFromPayload() = nil, want error")
	}
	statusErr, ok := err.(interface{ StatusCode() int })
	if !ok {
		t.Fatalf("error type %T does not expose StatusCode", err)
	}
	if statusErr.StatusCode() != http.StatusTooManyRequests {
		t.Fatalf("StatusCode = %d, want %d", statusErr.StatusCode(), http.StatusTooManyRequests)
	}
}

func TestCursorExecDeduperSeparatesRequestContextAndMCPWithEmptyExecID(t *testing.T) {
	d := newCursorExecDeduper()
	requestContext := &cursorproto.DecodedServerMessage{Type: cursorproto.ServerMsgExecRequestCtx, ExecMsgId: 1}
	mcpArgs := &cursorproto.DecodedServerMessage{Type: cursorproto.ServerMsgExecMcpArgs, ExecMsgId: 1}

	if !d.mark(requestContext) {
		t.Fatal("first request_context mark = false, want true")
	}
	if d.mark(requestContext) {
		t.Fatal("duplicate request_context mark = true, want false")
	}
	if !d.mark(mcpArgs) {
		t.Fatal("mcp_args mark = false, want true; type must be part of dedupe key")
	}
}

func TestCursorShouldEndAfterKVOnlyAfterContentOutsideToolWait(t *testing.T) {
	if cursorShouldEndAfterKV(false, cursorproto.ServerMsgKvSetBlob, false) {
		t.Fatal("KV before content should not end stream")
	}
	if !cursorShouldEndAfterKV(true, cursorproto.ServerMsgKvSetBlob, false) {
		t.Fatal("KV set after content should end stream outside tool wait")
	}
	if cursorShouldEndAfterKV(true, cursorproto.ServerMsgKvGetBlob, false) {
		t.Fatal("KV get after content should not end stream")
	}
	if cursorShouldEndAfterKV(true, cursorproto.ServerMsgKvSetBlob, true) {
		t.Fatal("KV set during pending tool wait must not end the upstream stream")
	}
}

func TestCursorRemoveStoredSessionIfCurrent(t *testing.T) {
	exec := &CursorExecutor{sessions: make(map[string]*cursorSession)}
	current := &cursorSession{}
	other := &cursorSession{}
	exec.sessions["session"] = current

	if !exec.removeSessionIfCurrent("session", current) {
		t.Fatal("removeSessionIfCurrent() = false, want true for matching session")
	}
	if _, ok := exec.sessions["session"]; ok {
		t.Fatal("matching session was not removed")
	}

	exec.sessions["session"] = other
	if exec.removeSessionIfCurrent("session", current) {
		t.Fatal("removeSessionIfCurrent() = true, want false for stale pointer")
	}
	if got := exec.sessions["session"]; got != other {
		t.Fatal("non-matching session was removed")
	}
}

func TestCloseCursorSessionsClosesResumeOutput(t *testing.T) {
	ch := make(chan cliproxyexecutor.StreamChunk)
	closeCursorSessions([]*cursorSession{{resumeOutCh: ch}})

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("resumeOutCh is still open")
		}
	default:
		t.Fatal("resumeOutCh was not closed")
	}

	closeCursorSessions([]*cursorSession{{resumeOutCh: ch}})
}

func TestCursorToolFallbackInstructionPreventsRepeatingCompletedCalls(t *testing.T) {
	got := cursorAppendToolFallbackInstruction("User: inspect files")

	if !strings.Contains(got, "The tool results above are already completed") {
		t.Fatalf("fallback instruction missing completed-tool guidance: %q", got)
	}
	if !strings.Contains(got, "User: inspect files") {
		t.Fatalf("fallback instruction dropped original text: %q", got)
	}
}

func TestCursorResolveRequestedModelAcceptsPrefixedAndRawIDs(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantModel  string
		wantParams map[string]string
	}{
		{name: "prefixed composer", input: "cursor-composer-2.5", wantModel: "composer-2.5"},
		{name: "raw composer", input: "composer-2.5", wantModel: "composer-2.5"},
		{name: "prefixed default", input: "cursor-default", wantModel: "default"},
		{name: "auto maps to default", input: "auto", wantModel: "default"},
		{name: "prefixed auto maps to default", input: "cursor-auto", wantModel: "default"},
		{name: "composer fast parameter", input: "cursor-composer-2-fast", wantModel: "composer-2", wantParams: map[string]string{"fast": "true"}},
		{name: "raw cursor-small remains raw", input: "cursor-small", wantModel: "cursor-small"},
		{name: "namespaced cursor-small strips one prefix", input: "cursor-cursor-small", wantModel: "cursor-small"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := cursorResolveRequestedModel(tt.input)
			if got.ModelID != tt.wantModel {
				t.Fatalf("ModelID = %q, want %q", got.ModelID, tt.wantModel)
			}
			if len(got.Parameters) != len(tt.wantParams) {
				t.Fatalf("parameters = %#v, want %#v", got.Parameters, tt.wantParams)
			}
			for _, param := range got.Parameters {
				if tt.wantParams[param.ID] != param.Value {
					t.Fatalf("parameter %q = %q, want %q", param.ID, param.Value, tt.wantParams[param.ID])
				}
			}
		})
	}
}

func TestBuildRunRequestParamsNormalizesCursorModelForUpstream(t *testing.T) {
	parsed := &parsedOpenAIRequest{
		Model:        "cursor-gpt-5.4(high)",
		SystemPrompt: "system",
		UserText:     "hello",
		RawPayload:   []byte(`{"model":"cursor-gpt-5.4(high)","messages":[{"role":"user","content":"hello"}]}`),
	}

	params := buildRunRequestParams(parsed, "conv-1")
	if params.ModelId != "gpt-5.4-high" {
		t.Fatalf("ModelId = %q, want gpt-5.4-high", params.ModelId)
	}
}

func TestCursorNormalizeExecutionModelInOpenAIPayload(t *testing.T) {
	payload := []byte(`{"model":"cursor/composer-2.5","messages":[{"role":"user","content":"hello"}]}`)
	normalized := cursorNormalizeExecutionModelInOpenAIPayload(payload, "composer-2.5")
	parsed := parseOpenAIRequest(normalized)
	params := buildRunRequestParams(parsed, "conv-1")

	if parsed.Model != "composer-2.5" {
		t.Fatalf("parsed model = %q, want composer-2.5", parsed.Model)
	}
	if params.ModelId != "composer-2.5" {
		t.Fatalf("ModelId = %q, want composer-2.5", params.ModelId)
	}
}

func TestGetCursorFallbackModelsIncludePrefixedAndRawAliases(t *testing.T) {
	models := GetCursorFallbackModels()
	ids := make(map[string]bool, len(models))
	for _, model := range models {
		ids[model.ID] = true
	}
	for _, id := range []string{"cursor-composer-2.5", "composer-2.5", "cursor-gpt-5.4", "gpt-5.4", "cursor-default", "default"} {
		if !ids[id] {
			t.Fatalf("fallback models missing %q; got ids=%v", id, ids)
		}
	}
	if ids["small"] {
		t.Fatalf("fallback models should not expose small alias for raw cursor-small")
	}
}

func TestParseOpenAIRequestExtractsDataAndRemoteImageURLs(t *testing.T) {
	const redPixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAIAAACQd1PeAAAADElEQVR4nGP4z8AAAAMBAQDJ/pLvAAAAAElFTkSuQmCC"
	payload := []byte(`{
		"model":"cursor-composer-2.5",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"what color?"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,` + redPixelPNG + `"}},
			{"type":"image_url","image_url":{"url":"https://example.test/red.png"}}
		]}]
	}`)

	parsed := parseOpenAIRequest(payload)
	if parsed.UserText != "what color?" {
		t.Fatalf("UserText = %q, want text content", parsed.UserText)
	}
	if len(parsed.Images) != 2 {
		t.Fatalf("Images = %d, want 2", len(parsed.Images))
	}
	if parsed.Images[0].MimeType != "image/png" || len(parsed.Images[0].Data) == 0 {
		t.Fatalf("first image = %#v, want decoded data URL PNG", parsed.Images[0])
	}
	if parsed.Images[1].URL != "https://example.test/red.png" {
		t.Fatalf("second image URL = %q, want remote URL", parsed.Images[1].URL)
	}
}

func TestCursorExecutorResolveRemoteImageURL(t *testing.T) {
	imageBytes := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 'r', 'e', 'd'}
	imageServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/red.png" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png; charset=binary")
		_, _ = w.Write(imageBytes)
	}))
	defer imageServer.Close()

	parsed := &parsedOpenAIRequest{Images: []cursorproto.ImageData{{URL: imageServer.URL + "/red.png"}}}
	exec := NewCursorExecutor(nil)
	auth := &cliproxyauth.Auth{Metadata: map[string]any{"access_token": "token-x"}}

	if err := exec.resolveCursorRemoteImages(context.Background(), auth, parsed); err != nil {
		t.Fatalf("resolveCursorRemoteImages() error = %v", err)
	}
	if len(parsed.Images) != 1 {
		t.Fatalf("Images = %d, want 1", len(parsed.Images))
	}
	if parsed.Images[0].MimeType != "image/png" {
		t.Fatalf("MimeType = %q, want image/png", parsed.Images[0].MimeType)
	}
	if got := string(parsed.Images[0].Data); got != string(imageBytes) {
		t.Fatalf("image data = %q, want %q", got, string(imageBytes))
	}
	if parsed.Images[0].URL != "" {
		t.Fatalf("URL = %q, want cleared after fetch", parsed.Images[0].URL)
	}
}

func TestCursorExecutorRejectsRemoteNonImage(t *testing.T) {
	textServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("not an image"))
	}))
	defer textServer.Close()

	parsed := &parsedOpenAIRequest{Images: []cursorproto.ImageData{{URL: textServer.URL}}}
	exec := NewCursorExecutor(nil)

	if err := exec.resolveCursorRemoteImages(context.Background(), nil, parsed); err == nil {
		t.Fatal("resolveCursorRemoteImages() error = nil, want non-image content type error")
	}
}

func TestParseModelsResponsePrefixesRemoteIDsAndAddsRawAliases(t *testing.T) {
	models := cursorExpandModelAliases([]*registry.ModelInfo{
		{ID: cursorPublicModelID("composer-2.5"), Name: "composer-2.5", OwnedBy: "cursor", Type: cursorAuthType},
		{ID: cursorPublicModelID("cursor-small"), Name: "cursor-small", OwnedBy: "cursor", Type: cursorAuthType},
	})

	ids := make(map[string]bool, len(models))
	for _, model := range models {
		ids[model.ID] = true
	}
	for _, id := range []string{"cursor-composer-2.5", "composer-2.5", "cursor-small"} {
		if !ids[id] {
			t.Fatalf("expanded model aliases missing %q; got ids=%v", id, ids)
		}
	}
	for _, id := range []string{"small", "cursor-cursor-small", "cursor/composer-2.5", "cursor/cursor-small"} {
		if ids[id] {
			t.Fatalf("expanded model aliases unexpectedly contained %q; got ids=%v", id, ids)
		}
	}
	_ = fmt.Sprintf("%v", ids)
}
