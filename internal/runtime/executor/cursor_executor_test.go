package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
	responsesconverter "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/openai/openai/responses"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCursorRawProtoLogFrameUsesBase64AndHashes(t *testing.T) {
	payload := []byte{0x01, 0x02, 0x03, 0xff}
	frame := cursorproto.FrameConnectMessage(payload, cursorproto.ConnectCompressionFlag)

	got := cursorFormatRawProtoLogFrame("response", "stream-1", 7, cursorproto.ConnectCompressionFlag, frame, payload, "type=1")

	for _, want := range []string{
		"Direction: response",
		"Stream ID: stream-1",
		"Sequence: 7",
		"Flags: 0x01",
		"Frame Bytes: 9",
		"Payload Bytes: 4",
		"Decoded Message: type=1",
		"Frame-Base64: " + base64.StdEncoding.EncodeToString(frame),
		"Payload-Base64: " + base64.StdEncoding.EncodeToString(payload),
		"Frame-SHA256:",
		"Payload-SHA256:",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("raw proto log missing %q in:\n%s", want, got)
		}
	}
}

func TestCursorRawProtoLoggingUsesAPIRequestAndResponseSections(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ctx := context.WithValue(context.Background(), "gin", ginCtx)
	cfg := &config.Config{Debug: true}
	cfg.RequestLog = true

	payload := []byte{0x08, 0x01}
	frame := cursorproto.FrameConnectMessage(payload, 0)
	cursorRecordRunAPIRequest(ctx, cfg, &cliproxyauth.Auth{ID: "cursor.test.json", Provider: "cursor"}, map[string]string{
		":path":         cursorRunPath,
		"authorization": "Bearer local-secret",
		"content-type":  "application/connect+proto",
	}, payload, frame)
	cursorRecordRunAPIResponseMetadata(ctx, cfg)
	newCursorRawProtoLogger(ctx, cfg).appendResponseFrame("stream-1", 0, frame, payload, "type=1")

	requestValue, exists := ginCtx.Get("API_REQUEST")
	if !exists {
		t.Fatal("API_REQUEST was not recorded")
	}
	requestText := string(requestValue.([]byte))
	for _, want := range []string{
		"=== API REQUEST 1 ===",
		"Transport: h2-connect",
		"Frame-Base64: " + base64.StdEncoding.EncodeToString(frame),
		"Payload-Base64: " + base64.StdEncoding.EncodeToString(payload),
		"Authorization: Bearer loca...cret",
	} {
		if !strings.Contains(requestText, want) {
			t.Fatalf("API request log missing %q in:\n%s", want, requestText)
		}
	}

	responseValue, exists := ginCtx.Get("API_RESPONSE")
	if !exists {
		t.Fatal("API_RESPONSE was not recorded")
	}
	responseText := string(responseValue.([]byte))
	for _, want := range []string{
		"=== API RESPONSE 1 ===",
		"X-Cursor-Transport: h2-connect",
		"Direction: response",
		"Frame-Base64: " + base64.StdEncoding.EncodeToString(frame),
	} {
		if !strings.Contains(responseText, want) {
			t.Fatalf("API response log missing %q in:\n%s", want, responseText)
		}
	}
}

func TestCursorWaitForStreamStartReturnsOnBootstrapMarker(t *testing.T) {
	chunks := make(chan cliproxyexecutor.StreamChunk)
	streamErrCh := make(chan error)
	bootstrapStarted := make(chan struct{}, 1)
	payloadSent := make(chan struct{})
	bootstrapStarted <- struct{}{}

	result, err := cursorWaitForStreamStart(context.Background(), chunks, streamErrCh, bootstrapStarted, payloadSent, func() {})
	if err != nil {
		t.Fatalf("cursorWaitForStreamStart() error = %v", err)
	}
	if result == nil || result.Chunks != chunks {
		t.Fatal("cursorWaitForStreamStart() did not return the original stream chunks after bootstrap")
	}
}

func TestCursorWaitForStreamStartClosesStreamOnContextCancel(t *testing.T) {
	chunks := make(chan cliproxyexecutor.StreamChunk)
	streamErrCh := make(chan error)
	bootstrapStarted := make(chan struct{})
	payloadSent := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	closed := false
	result, err := cursorWaitForStreamStart(ctx, chunks, streamErrCh, bootstrapStarted, payloadSent, func() { closed = true })
	if result != nil {
		t.Fatalf("cursorWaitForStreamStart() result = %#v, want nil", result)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cursorWaitForStreamStart() error = %v, want context.Canceled", err)
	}
	if !closed {
		t.Fatal("cursorWaitForStreamStart() did not close the upstream stream on context cancellation")
	}
}

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

func TestCursorShouldRejectSingularWebFetchURLArg(t *testing.T) {
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
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"urls":{"type":"array"},"objective":{"type":"string"}},
		"required":["urls"],
		"additionalProperties":false
	}`)

	emit, reason := cursorValidateMcpExecForEmission(msg, []cursorproto.McpToolDef{{Name: cursorOpenAIToolAliasPrefix + "web_fetch", InputSchema: schema}})
	if emit {
		t.Fatal("web_fetch with singular url was emittable")
	}
	for _, want := range []string{`missing required argument "urls"`, `argument "url" is not declared`} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason = %q, want to contain %q", reason, want)
		}
	}
	if _, exists := msg.McpArgs["urls"]; exists {
		t.Fatal("singular url was repaired into urls")
	}
}

func TestCursorShouldRejectFffGrepSingularPatternArg(t *testing.T) {
	msg := &cursorproto.DecodedServerMessage{
		Type:          cursorproto.ServerMsgExecMcpArgs,
		McpToolName:   cursorOpenAIToolAliasPrefix + "fff_grep",
		McpToolCallId: "call_fff_grep",
		ExecMsgId:     10,
		ExecId:        "exec-fff-grep",
		McpArgs: map[string][]byte{
			"literal": []byte(`true`),
			"pattern": []byte(`"[\"cursor:\", \"chatPath\", \"StreamChat\"]"`),
			"within":  []byte(`["/repo/open-sse/config/providers.js"]`),
		},
	}
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"literal":{"type":"boolean"},"patterns":{"type":"array"},"within":{"type":"array"}},
		"required":["patterns"],
		"additionalProperties":false
	}`)

	emit, reason := cursorValidateMcpExecForEmission(msg, []cursorproto.McpToolDef{{Name: cursorOpenAIToolAliasPrefix + "fff_grep", InputSchema: schema}})
	if emit {
		t.Fatal("fff_grep with singular pattern was emittable")
	}
	for _, want := range []string{`missing required argument "patterns"`, `argument "pattern" is not declared`} {
		if !strings.Contains(reason, want) {
			t.Fatalf("reason = %q, want to contain %q", reason, want)
		}
	}
	if _, exists := msg.McpArgs["patterns"]; exists {
		t.Fatal("singular pattern was repaired into patterns")
	}
}

func TestCursorShouldRejectReadWithFffGrepArgs(t *testing.T) {
	msg := &cursorproto.DecodedServerMessage{
		Type:          cursorproto.ServerMsgExecMcpArgs,
		McpToolName:   cursorOpenAIToolAliasPrefix + "read",
		McpToolCallId: "call_read_but_grep_args",
		ExecMsgId:     11,
		ExecId:        "exec-read-but-grep-args",
		McpArgs: map[string][]byte{
			"literal":  []byte(`true`),
			"patterns": []byte(`["cursor:","chatPath","StreamChat"]`),
			"within":   []byte(`["/repo/open-sse/config/providers.js"]`),
		},
	}

	if cursorShouldEmitMcpExec(msg, []cursorproto.McpToolDef{
		{Name: cursorOpenAIToolAliasPrefix + "read"},
		{Name: cursorOpenAIToolAliasPrefix + "fff_grep"},
	}) {
		t.Fatal("read with fff_grep-shaped args was emittable")
	}
	if got := cursorOpenAIToolNameForMcpTool(msg.McpToolName); got != "read" {
		t.Fatalf("tool name = %q, want read", got)
	}
}

func TestCursorShouldRejectMcpArgsWithUndeclaredSchemaProperties(t *testing.T) {
	msg := &cursorproto.DecodedServerMessage{
		Type:          cursorproto.ServerMsgExecMcpArgs,
		McpToolName:   cursorOpenAIToolAliasPrefix + "fff_grep",
		McpToolCallId: "call_bad_fff_grep",
		ExecMsgId:     12,
		ExecId:        "exec-bad-fff-grep",
		McpArgs: map[string][]byte{
			"literal":    []byte(`true`),
			"patterns":   []byte(`["cursor:"]`),
			"within":     []byte(`["/repo/open-sse/config/providers.js"]`),
			"unexpected": []byte(`"nope"`),
		},
	}
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"literal":{"type":"boolean"},"patterns":{"type":"array"},"within":{"type":"array"}},
		"required":["patterns"],
		"additionalProperties":false
	}`)

	if cursorShouldEmitMcpExec(msg, []cursorproto.McpToolDef{{Name: cursorOpenAIToolAliasPrefix + "fff_grep", InputSchema: schema}}) {
		t.Fatal("fff_grep with undeclared schema property was emittable")
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

func TestCursorExecDeduperDoesNotHideInteractionToolStartedAfterPartial(t *testing.T) {
	deduper := newCursorExecDeduper()
	collector := newCursorInteractionToolCollector()
	partial := &cursorproto.DecodedServerMessage{
		Type:                     cursorproto.ServerMsgExecMcpArgs,
		McpToolName:              "grep",
		McpToolCallId:            "call_grep",
		InteractionToolCall:      true,
		InteractionArgsTextDelta: `{"pattern":`,
	}
	started := &cursorproto.DecodedServerMessage{
		Type:                cursorproto.ServerMsgExecMcpArgs,
		McpToolName:         "grep",
		McpToolCallId:       "call_grep",
		McpArgs:             map[string][]byte{"pattern": []byte(`"InteractionUpdate"`)},
		InteractionToolCall: true,
	}

	if !deduper.mark(partial) {
		t.Fatal("partial interaction tool update was treated as duplicate")
	}
	if got := collector.absorb(partial); got != nil {
		t.Fatalf("incomplete partial update emitted %#v, want nil", got)
	}
	if !deduper.mark(started) {
		t.Fatal("tool started update was hidden as a duplicate after an incomplete partial update")
	}
	got := collector.absorb(started)
	if got == nil {
		t.Fatal("complete started update did not emit collected tool call")
	}
	if got.McpToolName != "grep" || got.McpToolCallId != "call_grep" {
		t.Fatalf("collected msg = %#v, want grep/call_grep", got)
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

func TestCursorStreamingToolCallDeltasSplitLargeArguments(t *testing.T) {
	longValue := strings.Repeat("x", cursorToolCallArgumentChunkSize*2+17)
	args := `{"command":"` + longValue + `"}`
	deltas := cursorStreamingToolCallDeltasJSON([]pendingMcpExec{
		{ToolCallId: "call_bash", ToolName: "bash", Args: args},
	})

	if len(deltas) < 3 {
		t.Fatalf("tool call deltas = %d, want split start + argument chunks", len(deltas))
	}
	first := gjson.Get(deltas[0], "tool_calls.0")
	if got := first.Get("id").String(); got != "call_bash" {
		t.Fatalf("first delta call id = %q, want call_bash", got)
	}
	if got := first.Get("function.name").String(); got != "bash" {
		t.Fatalf("first delta function name = %q, want bash", got)
	}
	var reconstructed strings.Builder
	for i, delta := range deltas {
		call := gjson.Get(delta, "tool_calls.0")
		if got := int(call.Get("index").Int()); got != 0 {
			t.Fatalf("delta %d index = %d, want 0 (%s)", i, got, delta)
		}
		chunk := call.Get("function.arguments").String()
		if chunk == "" {
			t.Fatalf("delta %d has empty arguments chunk: %s", i, delta)
		}
		if len(chunk) > cursorToolCallArgumentChunkSize+len(`{"command":"`) {
			t.Fatalf("delta %d arguments chunk len = %d, want bounded chunk", i, len(chunk))
		}
		reconstructed.WriteString(chunk)
	}
	if got := reconstructed.String(); got != args {
		t.Fatalf("reconstructed args len = %d, want %d", len(got), len(args))
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

type fakeCursorStream struct {
	data chan []byte
	done chan struct{}
	mu   sync.Mutex
	err  error
	once sync.Once
	dead chan struct{}
}

func newFakeCursorStream() *fakeCursorStream {
	return &fakeCursorStream{data: make(chan []byte), done: make(chan struct{}), dead: make(chan struct{})}
}
func (s *fakeCursorStream) ID() string            { return "test-stream" }
func (s *fakeCursorStream) Write([]byte) error    { return nil }
func (s *fakeCursorStream) Data() <-chan []byte   { return s.data }
func (s *fakeCursorStream) Done() <-chan struct{} { return s.done }
func (s *fakeCursorStream) Err() error            { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *fakeCursorStream) Close()                { s.once.Do(func() { close(s.dead) }) }

func newCursorExecutorHarness(process cursorFrameProcessor) *CursorExecutor {
	e := NewCursorExecutor(nil)
	e.openStream = func(string) (cursorStream, error) { return newFakeCursorStream(), nil }
	e.processFrames = process
	return e
}

func cursorTestAuth() *cliproxyauth.Auth {
	return &cliproxyauth.Auth{ID: "cursor-test", Metadata: map[string]any{"access_token": "test-token"}}
}

func cursorTestRequest(stream bool) cliproxyexecutor.Request {
	payload := `{"model":"cursor-test-model","messages":[{"role":"user","content":"hello"}]}`
	if stream {
		payload = `{"model":"cursor-test-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	}
	return cliproxyexecutor.Request{Model: "cursor-test-model", Payload: []byte(payload)}
}

func collectCursorStream(t *testing.T, result *cliproxyexecutor.StreamResult) []cliproxyexecutor.StreamChunk {
	t.Helper()
	done := make(chan []cliproxyexecutor.StreamChunk, 1)
	go func() {
		var chunks []cliproxyexecutor.StreamChunk
		for chunk := range result.Chunks {
			chunks = append(chunks, chunk)
		}
		done <- chunks
	}()
	select {
	case chunks := <-done:
		return chunks
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Cursor stream to close")
		return nil
	}
}

func cursorStreamPayload(chunks []cliproxyexecutor.StreamChunk) string {
	var payload strings.Builder
	for _, chunk := range chunks {
		payload.Write(chunk.Payload)
		payload.WriteByte('\n')
	}
	return payload.String()
}

func TestCursorExecuteReturnsReasoningAndUsage(t *testing.T) {
	e := newCursorExecutorHarness(func(_ context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, onText func(string, bool), _ func(pendingMcpExec), _ <-chan []toolResultInfo, usage *cursorTokenUsage, _ func([]byte)) error {
		onText("plan", true)
		onText("answer", false)
		usage.addOutput(7)
		return nil
	})

	resp, err := e.Execute(context.Background(), cursorTestAuth(), cursorTestRequest(false), cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.reasoning_content").String(); got != "plan" {
		t.Fatalf("reasoning_content = %q", got)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.message.content").String(); got != "answer" {
		t.Fatalf("content = %q", got)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.completion_tokens").Int(); got != 7 {
		t.Fatalf("completion_tokens = %d", got)
	}
	promptTokens := gjson.GetBytes(resp.Payload, "usage.prompt_tokens").Int()
	if promptTokens < 1 {
		t.Fatalf("prompt_tokens = %d, want positive estimate", promptTokens)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.total_tokens").Int(); got != promptTokens+7 {
		t.Fatalf("total_tokens = %d, want %d", got, promptTokens+7)
	}
}

func TestCursorExecuteReasoningOnlyErrorIsFailure(t *testing.T) {
	boom := errors.New("upstream reset")
	e := newCursorExecutorHarness(func(_ context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, onText func(string, bool), _ func(pendingMcpExec), _ <-chan []toolResultInfo, _ *cursorTokenUsage, _ func([]byte)) error {
		onText("partial thought", true)
		return boom
	})
	if _, err := e.Execute(context.Background(), cursorTestAuth(), cursorTestRequest(false), cliproxyexecutor.Options{}); !errors.Is(err, boom) {
		t.Fatalf("Execute() error = %v, want wrapped %v", err, boom)
	}
}

func TestCursorExecuteStreamPostChunkErrorIsTerminalError(t *testing.T) {
	boom := errors.New("upstream reset")
	e := newCursorExecutorHarness(func(_ context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, onText func(string, bool), _ func(pendingMcpExec), _ <-chan []toolResultInfo, _ *cursorTokenUsage, _ func([]byte)) error {
		onText("partial", false)
		return boom
	})
	result, err := e.ExecuteStream(context.Background(), cursorTestAuth(), cursorTestRequest(true), cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	var sawPayload, sawError, sawStop bool
	for chunk := range result.Chunks {
		if len(chunk.Payload) > 0 {
			sawPayload = true
			if gjson.GetBytes(chunk.Payload, "choices.0.finish_reason").String() == "stop" {
				sawStop = true
			}
		}
		if chunk.Err != nil {
			sawError = true
		}
	}
	if !sawPayload || !sawError || sawStop {
		t.Fatalf("payload=%v error=%v stop=%v", sawPayload, sawError, sawStop)
	}
}

func TestCursorExecuteStreamClaudeThinkingToolBoundaryAndResume(t *testing.T) {
	rawToolCallID := "call-a b\n\t"
	clientToolCallID := normalizeToolCallID(rawToolCallID)
	processorResult := make(chan error, 1)
	e := newCursorExecutorHarness(func(ctx context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, onText func(string, bool), onMcpExec func(pendingMcpExec), toolResultCh <-chan []toolResultInfo, usage *cursorTokenUsage, _ func([]byte)) error {
		onText("plan", true)
		onText("answer", false)
		onMcpExec(pendingMcpExec{
			ExecMsgId:  1,
			ExecId:     "exec-1",
			ToolCallId: clientToolCallID,
			ToolName:   "read",
			Args:       `{"path":"README.md"}`,
		})
		select {
		case results := <-toolResultCh:
			if len(results) != 1 || results[0].ToolCallId != clientToolCallID || results[0].Content != "file contents" {
				err := errors.New("resumed tool result did not preserve the emitted ID and content")
				processorResult <- err
				return err
			}
		case <-ctx.Done():
			processorResult <- ctx.Err()
			return ctx.Err()
		}
		onText("after tool", false)
		usage.addOutput(9)
		processorResult <- nil
		return nil
	})

	firstPayload := []byte(`{"model":"cursor-test-model","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), OriginalRequest: firstPayload}
	first, err := e.ExecuteStream(context.Background(), cursorTestAuth(), cliproxyexecutor.Request{Model: "cursor-test-model", Payload: firstPayload}, opts)
	if err != nil {
		t.Fatalf("first ExecuteStream() error = %v", err)
	}
	firstBody := cursorStreamPayload(collectCursorStream(t, first))
	thinkingAt := strings.Index(firstBody, `"type":"thinking"`)
	thinkingTextAt := strings.Index(firstBody, `"thinking":"plan"`)
	answerAt := strings.Index(firstBody, `"text":"answer"`)
	toolAt := strings.Index(firstBody, `"type":"tool_use"`)
	toolIDAt := strings.Index(firstBody, `"id":"`+clientToolCallID+`"`)
	toolStopAt := strings.Index(firstBody, `"stop_reason":"tool_use"`)
	if thinkingAt < 0 || thinkingTextAt < thinkingAt || answerAt < thinkingTextAt || toolAt < answerAt || toolIDAt < toolAt || toolStopAt < toolIDAt {
		t.Fatalf("Claude thinking/text/tool boundary order invalid:\n%s", firstBody)
	}
	e.mu.Lock()
	publishedSessions := len(e.sessions)
	var publishedPending []pendingMcpExec
	for _, session := range e.sessions {
		publishedPending = append(publishedPending, session.pending...)
	}
	e.mu.Unlock()
	if publishedSessions != 1 || len(publishedPending) != 1 || publishedPending[0].ToolCallId != clientToolCallID {
		t.Fatalf("tool boundary closed before resumable session publication: sessions=%d pending=%#v", publishedSessions, publishedPending)
	}

	secondPayload := []byte(`{"model":"cursor-test-model","max_tokens":128,"stream":true,"messages":[{"role":"user","content":"hello"},{"role":"assistant","content":[{"type":"tool_use","id":"` + clientToolCallID + `","name":"read","input":{"path":"README.md"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + clientToolCallID + `","content":"file contents"}]}]}`)
	second, err := e.ExecuteStream(context.Background(), cursorTestAuth(), cliproxyexecutor.Request{Model: "cursor-test-model", Payload: secondPayload}, cliproxyexecutor.Options{
		SourceFormat:    sdktranslator.FromString("claude"),
		OriginalRequest: secondPayload,
	})
	if err != nil {
		t.Fatalf("resumed ExecuteStream() error = %v", err)
	}
	secondBody := cursorStreamPayload(collectCursorStream(t, second))
	if strings.Contains(secondBody, `"type":"tool_use"`) || !strings.Contains(secondBody, `"text":"after tool"`) || !strings.Contains(secondBody, `"stop_reason":"end_turn"`) {
		t.Fatalf("resumed Claude stream has invalid boundary/order:\n%s", secondBody)
	}
	if err := <-processorResult; err != nil {
		t.Fatal(err)
	}
}

func TestNormalizeToolCallID(t *testing.T) {
	input := "call-cce860e6-ab07-414d-812c-785db35b17ca-4\nfc_d2335004-a95f-93b4-977b-e9eee6316be7_0"
	want := "cursor_call_Y2FsbC1jY2U4NjBlNi1hYjA3LTQxNGQtODEyYy03ODVkYjM1YjE3Y2EtNApmY19kMjMzNTAwNC1hOTVmLTkzYjQtOTc3Yi1lOWVlZTYzMTZiZTdfMA"
	if got := normalizeToolCallID(input); got != want {
		t.Fatalf("normalizeToolCallID() = %q, want %q", got, want)
	}
}

func TestNormalizeToolCallIDCollisionsRemainDistinct(t *testing.T) {
	ids := []string{
		"call-a b",
		"call-a_u0020_b",
		"call-a%20b",
		"call-a\\u0020b",
		"call-ab",
		"call-a\nb",
		"call-a\tb",
		"call-a\x00b",
	}
	seen := map[string]string{}
	for _, id := range ids {
		normalized := normalizeToolCallID(id)
		if prior, ok := seen[normalized]; ok && prior != id {
			t.Fatalf("IDs %q and %q collide as %q", prior, id, normalized)
		}
		seen[normalized] = id
		encoded := strings.TrimPrefix(normalized, "cursor_call_")
		decoded, err := base64.RawURLEncoding.DecodeString(encoded)
		if !strings.HasPrefix(normalized, "cursor_call_") || err != nil || string(decoded) != id {
			t.Fatalf("normalized ID %q does not reversibly encode %q: decoded=%q err=%v", normalized, id, decoded, err)
		}
	}
}

func TestParseOpenAIToolCallIDRoundTrip(t *testing.T) {
	id := "call-a b\n"
	parsed := parseOpenAIRequest([]byte(`{"model":"m","messages":[{"role":"tool","tool_call_id":"call-a b\n","content":"ok"}]}`))
	if len(parsed.ToolResults) != 1 {
		t.Fatalf("tool results = %d", len(parsed.ToolResults))
	}
	if got, want := parsed.ToolResults[0].ToolCallId, id; got != want {
		t.Fatalf("tool_call_id = %q, want %q", got, want)
	}
}

func TestParseOpenAIToolCallIDDoesNotDoubleNormalize(t *testing.T) {
	rawID := "call-a b\n\t"
	clientID := normalizeToolCallID(rawID)
	payload := []byte(`{"model":"m","messages":[{"role":"tool","tool_call_id":` + jsonString(clientID) + `,"content":"ok"}]}`)
	parsed := parseOpenAIRequest(payload)
	if len(parsed.ToolResults) != 1 {
		t.Fatalf("tool results = %d", len(parsed.ToolResults))
	}
	if got := parsed.ToolResults[0].ToolCallId; got != clientID {
		t.Fatalf("tool_call_id = %q, want exact client ID %q", got, clientID)
	}
}

func TestCursorStreamCoalescerBatchesAdjacentDeltas(t *testing.T) {
	type emittedDelta struct {
		text       string
		isThinking bool
	}
	emitted := make(chan emittedDelta, 4)
	coalescer := newCursorStreamCoalescer(context.Background(), time.Hour, func(text string, isThinking bool) { emitted <- emittedDelta{text, isThinking} })
	coalescer.push("first", true)
	coalescer.push(" second", true)
	coalescer.push(" third", true)
	coalescer.push("answer", false)
	coalescer.close()
	close(emitted)
	var got []emittedDelta
	for delta := range emitted {
		got = append(got, delta)
	}
	want := []emittedDelta{{"first", true}, {" second third", true}, {"answer", false}}
	if len(got) != len(want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("delta %d = %#v, want %#v", i, got[i], want[i])
		}
	}
}

func TestCursorStreamCoalescerCancellationDoesNotBlock(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	blocked := make(chan struct{})
	coalescer := newCursorStreamCoalescer(ctx, time.Hour, func(string, bool) { <-blocked })
	done := make(chan struct{})
	go func() { coalescer.push("first", false); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("push blocked after cancellation")
	}
	close(blocked)
}

func TestCursorStreamCoalescerFlushesOnCadence(t *testing.T) {
	type emittedDelta struct {
		text string
	}
	emitted := make(chan emittedDelta, 2)
	coalescer := newCursorStreamCoalescer(context.Background(), 5*time.Millisecond, func(text string, _ bool) {
		emitted <- emittedDelta{text: text}
	})
	coalescer.push("first", false)
	if got := (<-emitted).text; got != "first" {
		t.Fatalf("first delta = %q", got)
	}
	coalescer.push("batched", false)
	select {
	case got := <-emitted:
		if got.text != "batched" {
			t.Fatalf("cadence delta = %q", got.text)
		}
	case <-time.After(time.Second):
		t.Fatal("pending delta did not flush on cadence")
	}
	coalescer.close()
}

func TestCursorExecuteStreamCancellationBeforeFirstChunkReturns(t *testing.T) {
	processorExited := make(chan struct{})
	e := newCursorExecutorHarness(func(ctx context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, _ func(string, bool), _ func(pendingMcpExec), _ <-chan []toolResultInfo, _ *cursorTokenUsage, _ func([]byte)) error {
		<-ctx.Done()
		close(processorExited)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := e.ExecuteStream(ctx, cursorTestAuth(), cursorTestRequest(true), cliproxyexecutor.Options{})
	if !errors.Is(err, context.Canceled) || result != nil {
		t.Fatalf("ExecuteStream() = %#v, %v; want nil, context.Canceled", result, err)
	}
	select {
	case <-processorExited:
	case <-time.After(time.Second):
		t.Fatal("frame processor remained detached after request cancellation")
	}
}

func TestCursorResumeCancellationRestoresSession(t *testing.T) {
	e := NewCursorExecutor(nil)
	sessionKey := "cursor-test:conversation"
	session := &cursorSession{
		toolResultCh: make(chan []toolResultInfo, 1),
		resumeOutCh:  make(chan cliproxyexecutor.StreamChunk, 1),
		cancel:       func() {},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := e.resumeWithToolResults(
		ctx,
		sessionKey,
		session,
		&parsedOpenAIRequest{ToolResults: []toolResultInfo{{ToolCallId: "call-1", Content: "ok"}}},
		sdktranslator.FromString("openai"),
		sdktranslator.FromString("openai"),
		cursorTestRequest(true),
		nil,
		nil,
		false,
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resumeWithToolResults() error = %v, want context.Canceled", err)
	}
	e.mu.Lock()
	restored := e.sessions[sessionKey]
	e.mu.Unlock()
	if restored != session {
		t.Fatal("canceled resume did not restore owned session")
	}
	select {
	case results := <-session.toolResultCh:
		t.Fatalf("canceled resume injected tool results: %#v", results)
	default:
	}
}

func TestCursorResumeInvalidSessionIsDiscarded(t *testing.T) {
	e := NewCursorExecutor(nil)
	sessionKey := "cursor-test:invalid-session"
	stream := newFakeCursorStream()
	canceled := false
	session := &cursorSession{
		stream:      stream,
		resumeOutCh: make(chan cliproxyexecutor.StreamChunk, 1),
		cancel:      func() { canceled = true },
	}
	_, err := e.resumeWithToolResults(
		context.Background(),
		sessionKey,
		session,
		&parsedOpenAIRequest{ToolResults: []toolResultInfo{{ToolCallId: "call-1", Content: "ok"}}},
		sdktranslator.FromString("openai"),
		sdktranslator.FromString("openai"),
		cursorTestRequest(true),
		nil,
		nil,
		false,
	)
	if err == nil || !strings.Contains(err.Error(), "no toolResultCh") {
		t.Fatalf("resumeWithToolResults() error = %v", err)
	}
	e.mu.Lock()
	restored := e.sessions[sessionKey]
	e.mu.Unlock()
	if restored != nil || !canceled {
		t.Fatalf("invalid session was retained: restored=%v canceled=%v", restored != nil, canceled)
	}
	select {
	case <-stream.dead:
	default:
		t.Fatal("invalid session stream was not closed")
	}
}

func TestCursorResumeRejectsUnmatchedToolResultAndRestoresSession(t *testing.T) {
	e := NewCursorExecutor(nil)
	sessionKey := "cursor-test:pending-session"
	switched := false
	session := &cursorSession{
		pending:      []pendingMcpExec{{ToolCallId: "call-good"}},
		toolResultCh: make(chan []toolResultInfo, 1),
		resumeOutCh:  make(chan cliproxyexecutor.StreamChunk, 1),
		cancel:       func() {},
		switchOutput: func(chan cliproxyexecutor.StreamChunk, context.Context) { switched = true },
	}
	_, err := e.resumeWithToolResults(
		context.Background(),
		sessionKey,
		session,
		&parsedOpenAIRequest{ToolResults: []toolResultInfo{{ToolCallId: "call-wrong", Content: "ok"}}},
		sdktranslator.FromString("openai"),
		sdktranslator.FromString("openai"),
		cursorTestRequest(true),
		nil,
		nil,
		false,
	)
	if err == nil || !strings.Contains(err.Error(), "do not match") {
		t.Fatalf("resumeWithToolResults() error = %v", err)
	}
	e.mu.Lock()
	restored := e.sessions[sessionKey]
	e.mu.Unlock()
	if restored != session || switched {
		t.Fatalf("unmatched result lost session ownership: restored=%v switched=%v", restored == session, switched)
	}
	select {
	case results := <-session.toolResultCh:
		t.Fatalf("unmatched result was injected: %#v", results)
	default:
	}
}

func TestCursorToolBoundaryImmediateResumeUsesPublishedSession(t *testing.T) {
	clientID := normalizeToolCallID("call immediate")
	e := newCursorExecutorHarness(func(ctx context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, _ func(string, bool), onMcpExec func(pendingMcpExec), toolResultCh <-chan []toolResultInfo, _ *cursorTokenUsage, _ func([]byte)) error {
		onMcpExec(pendingMcpExec{ToolCallId: clientID, ToolName: "read", Args: `{}`})
		select {
		case results := <-toolResultCh:
			if len(results) != 1 || results[0].ToolCallId != clientID {
				return errors.New("immediate resume delivered wrong tool result")
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})
	var openMu sync.Mutex
	openCount := 0
	e.openStream = func(string) (cursorStream, error) {
		openMu.Lock()
		openCount++
		openMu.Unlock()
		return newFakeCursorStream(), nil
	}
	first, err := e.ExecuteStream(context.Background(), cursorTestAuth(), cursorTestRequest(true), cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	secondPayload := []byte(`{"model":"cursor-test-model","stream":true,"messages":[{"role":"user","content":"hello"},{"role":"assistant","tool_calls":[{"id":"` + clientID + `","type":"function","function":{"name":"read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"` + clientID + `","content":"ok"}]}`)
	type outcome struct {
		body string
		err  error
	}
	resumed := make(chan outcome, 1)
	go func() {
		for range first.Chunks {
		}
		second, errResume := e.ExecuteStream(context.Background(), cursorTestAuth(), cliproxyexecutor.Request{Model: "cursor-test-model", Payload: secondPayload}, cliproxyexecutor.Options{})
		if errResume != nil {
			resumed <- outcome{err: errResume}
			return
		}
		var chunks []cliproxyexecutor.StreamChunk
		for chunk := range second.Chunks {
			chunks = append(chunks, chunk)
		}
		resumed <- outcome{body: cursorStreamPayload(chunks)}
	}()
	select {
	case got := <-resumed:
		if got.err != nil {
			t.Fatal(got.err)
		}
		openMu.Lock()
		gotOpenCount := openCount
		openMu.Unlock()
		if gotOpenCount != 1 {
			t.Fatalf("immediate resume cold-started a second Cursor stream: opens=%d body=%s", gotOpenCount, got.body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("immediate close-triggered resume hung")
	}
}

func TestCursorExecuteStreamConcurrentCancelAndFirstEmit(t *testing.T) {
	for i := 0; i < 50; i++ {
		gate := make(chan struct{})
		e := newCursorExecutorHarness(func(_ context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, onText func(string, bool), _ func(pendingMcpExec), _ <-chan []toolResultInfo, _ *cursorTokenUsage, _ func([]byte)) error {
			<-gate
			onText("first", false)
			return nil
		})
		ctx, cancel := context.WithCancel(context.Background())
		type outcome struct {
			result *cliproxyexecutor.StreamResult
			err    error
		}
		finished := make(chan outcome, 1)
		go func() {
			result, err := e.ExecuteStream(ctx, cursorTestAuth(), cursorTestRequest(true), cliproxyexecutor.Options{})
			finished <- outcome{result: result, err: err}
		}()
		close(gate)
		cancel()
		select {
		case got := <-finished:
			if got.err != nil && !errors.Is(got.err, context.Canceled) {
				t.Fatalf("iteration %d: ExecuteStream() error = %v", i, got.err)
			}
			if got.result != nil {
				collectCursorStream(t, got.result)
			}
		case <-time.After(time.Second):
			t.Fatalf("iteration %d: cancel/first-emission race hung", i)
		}
	}
}

func TestCursorExecuteStreamBackpressureCancellationUnblocks(t *testing.T) {
	processorExited := make(chan struct{})
	e := newCursorExecutorHarness(func(_ context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, onText func(string, bool), _ func(pendingMcpExec), _ <-chan []toolResultInfo, _ *cursorTokenUsage, _ func([]byte)) error {
		defer close(processorExited)
		for i := 0; i < 256; i++ {
			onText("x", i%2 == 0)
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	result, err := e.ExecuteStream(ctx, cursorTestAuth(), cursorTestRequest(true), cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-processorExited:
	case <-time.After(time.Second):
		t.Fatal("frame processor blocked behind a full output buffer")
	}
	collectCursorStream(t, result)
}

func TestCursorOpenAIExecutorEmitsNoDoneChunk(t *testing.T) {
	e := newCursorExecutorHarness(func(_ context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, onText func(string, bool), _ func(pendingMcpExec), _ <-chan []toolResultInfo, _ *cursorTokenUsage, _ func([]byte)) error {
		onText("ok", false)
		return nil
	})
	result, err := e.ExecuteStream(context.Background(), cursorTestAuth(), cursorTestRequest(true), cliproxyexecutor.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var payload strings.Builder
	for chunk := range result.Chunks {
		payload.Write(chunk.Payload)
	}
	if strings.Contains(payload.String(), "[DONE]") {
		t.Fatalf("executor emitted DONE: %s", payload.String())
	}
}

// Alias keeps test processor signatures readable without weakening production types.
type anyMCPTools = []cursorproto.McpToolDef
