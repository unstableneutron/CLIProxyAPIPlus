package helps

import (
	"bytes"
	"encoding/base64"
	"testing"

	"github.com/tidwall/gjson"
)

func TestClaudeMessagesToBedrockConverse_mapsImageDocumentAndThinkingBlocks(t *testing.T) {
	t.Parallel()

	imageData := base64.StdEncoding.EncodeToString([]byte("image-bytes"))
	docData := base64.StdEncoding.EncodeToString([]byte("document bytes"))
	body := []byte(`{
		"messages":[{
			"role":"user",
			"content":[
				{"type":"text","text":"inspect these"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + imageData + `"}},
				{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"` + docData + `"},"title":"report.pdf"}
			]
		},{
			"role":"assistant",
			"content":[
				{"type":"thinking","thinking":"internal chain","signature":"sig_123"},
				{"type":"text","text":"visible"}
			]
		}]
	}`)

	out := ClaudeMessagesToBedrockConverse(body)

	if got := gjson.GetBytes(out, "messages.0.content.1.image.format").String(); got != "png" {
		t.Fatalf("image format = %q, want png; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.0.content.1.image.source.bytes").String(); got != imageData {
		t.Fatalf("image bytes = %q, want base64 image data; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.0.content.2.document.format").String(); got != "pdf" {
		t.Fatalf("document format = %q, want pdf; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.0.content.2.document.name").String(); got != "report pdf" {
		t.Fatalf("document name = %q, want report pdf; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.0.content.2.document.source.bytes").String(); got != docData {
		t.Fatalf("document bytes = %q, want base64 document data; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.1.content.0.reasoningContent.reasoningText.text").String(); got != "internal chain" {
		t.Fatalf("reasoning text = %q, want internal chain; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "messages.1.content.0.reasoningContent.reasoningText.signature").String(); got != "sig_123" {
		t.Fatalf("reasoning signature = %q, want sig_123; output=%s", got, out)
	}
}

func TestClaudeMessagesToBedrockConverse_mapsStructuredOutputConfig(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"messages":[{"role":"user","content":"return json"}],
		"response_format":{"type":"json_schema","json_schema":{"name":"answer","schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}}},
		"text":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"fallback":{"type":"boolean"}}}}}
	}`)

	out := ClaudeMessagesToBedrockConverse(body)

	if got := gjson.GetBytes(out, "additionalModelRequestFields.response_format.type").String(); got != "json_schema" {
		t.Fatalf("response_format type = %q, want json_schema; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "additionalModelRequestFields.response_format.json_schema.schema.properties.answer.type").String(); got != "string" {
		t.Fatalf("json schema answer type = %q, want string; output=%s", got, out)
	}
}

func TestClaudeMessagesToBedrockInvokeUsesAnthropicBedrockBody(t *testing.T) {
	t.Parallel()

	body := []byte(`{"messages":[{"role":"user","content":"hi"}],"max_tokens":8}`)

	out := ClaudeMessagesToBedrockInvoke("anthropic.claude-sonnet-4", body, true)

	if got := gjson.GetBytes(out, "anthropic_version").String(); got != "bedrock-2023-05-31" {
		t.Fatalf("anthropic_version = %q, want bedrock-2023-05-31; output=%s", got, out)
	}
	if gjson.GetBytes(out, "model").Exists() {
		t.Fatalf("invoke payload should not include model; output=%s", out)
	}
	if gjson.GetBytes(out, "stream").Exists() {
		t.Fatalf("invoke payload should not include stream; output=%s", out)
	}
}

func TestClaudeMessagesToBedrockConverse_mapsOpenAIFunctionTools(t *testing.T) {
	t.Parallel()

	body := []byte(`{
		"messages":[{"role":"user","content":"call a tool"}],
		"tools":[{"type":"function","function":{"name":"lookup_weather","description":"Lookup weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}],
		"tool_choice":{"type":"function","function":{"name":"lookup_weather"}}
	}`)

	out := ClaudeMessagesToBedrockConverse(body)

	if got := gjson.GetBytes(out, "toolConfig.tools.0.toolSpec.name").String(); got != "lookup_weather" {
		t.Fatalf("tool name = %q, want lookup_weather; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "toolConfig.tools.0.toolSpec.inputSchema.json.properties.city.type").String(); got != "string" {
		t.Fatalf("tool schema city type = %q, want string; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "toolConfig.toolChoice.tool.name").String(); got != "lookup_weather" {
		t.Fatalf("tool choice = %q, want lookup_weather; output=%s", got, out)
	}
}

func TestClaudeMessagesToBedrockConverse_sanitizesDocumentName(t *testing.T) {
	t.Parallel()

	docData := base64.StdEncoding.EncodeToString([]byte("document bytes"))
	body := []byte(`{
		"messages":[{
			"role":"user",
			"content":[
				{"type":"document","source":{"type":"base64","media_type":"text/plain","data":"` + docData + `"},"name":"marker.txt"}
			]
		}]
	}`)

	out := ClaudeMessagesToBedrockConverse(body)

	if got := gjson.GetBytes(out, "messages.0.content.0.document.name").String(); got != "marker txt" {
		t.Fatalf("document name = %q, want marker txt; output=%s", got, out)
	}
}

func TestBedrockResponseToClaudeMessage_mapsReasoningAndUsageDetails(t *testing.T) {
	t.Parallel()

	data := []byte(`{
		"output":{"message":{"role":"assistant","content":[
			{"reasoningContent":{"reasoningText":{"text":"hidden","signature":"sig_456"}}},
			{"text":"visible"}
		]}},
		"stopReason":"end_turn",
		"usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15,"cacheReadInputTokens":3,"cacheWriteInputTokens":2,"reasoningTokens":4}
	}`)

	out := BedrockResponseToClaudeMessage("model", data)

	if got := gjson.GetBytes(out, "content.0.type").String(); got != "thinking" {
		t.Fatalf("content.0.type = %q, want thinking; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "content.0.thinking").String(); got != "hidden" {
		t.Fatalf("thinking = %q, want hidden; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "content.0.signature").String(); got != "sig_456" {
		t.Fatalf("signature = %q, want sig_456; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "usage.cache_read_input_tokens").Int(); got != 3 {
		t.Fatalf("cache_read_input_tokens = %d, want 3; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "usage.cache_creation_input_tokens").Int(); got != 2 {
		t.Fatalf("cache_creation_input_tokens = %d, want 2; output=%s", got, out)
	}
	if got := gjson.GetBytes(out, "usage.thinking_tokens").Int(); got != 4 {
		t.Fatalf("thinking_tokens = %d, want 4; output=%s", got, out)
	}
}

func TestBedrockResponseToClaudeSSE_mapsNormalizedToolUse(t *testing.T) {
	t.Parallel()

	data := []byte(`{
		"output":{"message":{"role":"assistant","content":[
			{"toolUse":{"toolUseId":"tool_123","name":"lookup_weather","input":{"city":"Paris"}}}
		]}},
		"stopReason":"tool_use",
		"usage":{"inputTokens":10,"outputTokens":5,"totalTokens":15}
	}`)

	out := BedrockResponseToClaudeSSE("model", data)

	if !bytes.Contains(out, []byte(`"type":"tool_use"`)) {
		t.Fatalf("SSE missing tool_use block: %s", out)
	}
	if !bytes.Contains(out, []byte(`"name":"lookup_weather"`)) {
		t.Fatalf("SSE missing tool name: %s", out)
	}
	if !bytes.Contains(out, []byte(`"partial_json":"{\"city\":\"Paris\"}"`)) {
		t.Fatalf("SSE missing tool input delta: %s", out)
	}
}

func TestBedrockStreamNormalizerTranslatesReasoningDeltasAndUsageDetails(t *testing.T) {
	t.Parallel()

	normalizer := NewBedrockStreamNormalizer("model")
	var joined []byte
	for _, payload := range [][]byte{
		[]byte(`{"contentBlockStart":{"contentBlockIndex":0,"start":{"reasoningContent":{"reasoningText":{"signature":"sig_stream"}}}}}`),
		[]byte(`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"reasoningContent":{"text":"think "}}}}`),
		[]byte(`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"reasoningContent":{"text":"more"}}}}`),
		[]byte(`{"contentBlockStop":{"contentBlockIndex":0}}`),
		[]byte(`{"metadata":{"usage":{"inputTokens":11,"outputTokens":6,"cacheReadInputTokens":2,"cacheWriteInputTokens":1,"reasoningTokens":5}}}`),
	} {
		for _, line := range normalizer.ConvertLine(payload) {
			joined = append(joined, line...)
			joined = append(joined, '\n')
		}
	}
	for _, line := range normalizer.Finish() {
		joined = append(joined, line...)
		joined = append(joined, '\n')
	}

	if !bytes.Contains(joined, []byte(`"type":"thinking"`)) {
		t.Fatalf("stream missing thinking block: %s", joined)
	}
	if !bytes.Contains(joined, []byte(`"thinking":"think "`)) || !bytes.Contains(joined, []byte(`"thinking":"more"`)) {
		t.Fatalf("stream missing thinking deltas: %s", joined)
	}
	if !bytes.Contains(joined, []byte(`"cache_read_input_tokens":2`)) {
		t.Fatalf("stream missing cache read usage: %s", joined)
	}
	if !bytes.Contains(joined, []byte(`"thinking_tokens":5`)) {
		t.Fatalf("stream missing thinking token usage: %s", joined)
	}
}

func TestBedrockStreamNormalizerOpensIndependentBlockKindsAfterClose(t *testing.T) {
	t.Parallel()

	normalizer := NewBedrockStreamNormalizer("model")
	var joined []byte
	for _, payload := range [][]byte{
		[]byte(`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"text":"first"}}}`),
		[]byte(`{"contentBlockStop":{"contentBlockIndex":0}}`),
		[]byte(`{"contentBlockStart":{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"toolu_1","name":"lookup"}}}}`),
		[]byte(`{"contentBlockDelta":{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"city\":\"sf\"}"}}}}`),
		[]byte(`{"contentBlockStop":{"contentBlockIndex":1}}`),
		[]byte(`{"contentBlockDelta":{"contentBlockIndex":2,"delta":{"text":"second"}}}`),
		[]byte(`{"contentBlockStop":{"contentBlockIndex":2}}`),
	} {
		for _, line := range normalizer.ConvertLine(payload) {
			joined = append(joined, line...)
			joined = append(joined, '\n')
		}
	}
	for _, line := range normalizer.Finish() {
		joined = append(joined, line...)
		joined = append(joined, '\n')
	}

	if bytes.Count(joined, []byte(`"type":"content_block_start"`)) != 3 {
		t.Fatalf("stream did not open three independent blocks: %s", joined)
	}
	if !bytes.Contains(joined, []byte(`"type":"tool_use"`)) {
		t.Fatalf("stream missing tool block: %s", joined)
	}
	if !bytes.Contains(joined, []byte(`"text":"second"`)) {
		t.Fatalf("stream missing second text block: %s", joined)
	}
}
