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
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
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

func TestCursorToolResultsMatchPendingCalls(t *testing.T) {
	pending := []pendingMcpExec{
		{ToolCallId: "call_a"},
		{ToolCallId: "call_b"},
	}
	if !cursorToolResultsMatchPending([]toolResultInfo{{ToolCallId: "call_b", Content: "ok"}}, pending) {
		t.Fatal("expected tool result to match one pending call")
	}
	if cursorToolResultsMatchPending([]toolResultInfo{{ToolCallId: "call_missing", Content: "ok"}}, pending) {
		t.Fatal("unexpected match for unknown tool call id")
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

func TestCursorShouldEndAfterKVOnlyAfterContent(t *testing.T) {
	if cursorShouldEndAfterKV(false, cursorproto.ServerMsgKvSetBlob) {
		t.Fatal("KV before content should not end stream")
	}
	if !cursorShouldEndAfterKV(true, cursorproto.ServerMsgKvSetBlob) {
		t.Fatal("KV set after content should end stream")
	}
	if cursorShouldEndAfterKV(true, cursorproto.ServerMsgKvGetBlob) {
		t.Fatal("KV get after content should not end stream")
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
	for _, id := range []string{"small", "cursor-cursor-small"} {
		if ids[id] {
			t.Fatalf("expanded model aliases unexpectedly contained %q; got ids=%v", id, ids)
		}
	}
	_ = fmt.Sprintf("%v", ids)
}
