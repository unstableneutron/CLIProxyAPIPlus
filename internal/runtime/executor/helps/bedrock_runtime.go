package helps

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type BedrockStreamNormalizer struct {
	id              string
	model           string
	started         bool
	textBlockOpen   bool
	textBlockClosed bool
	textBlockIndex  int64
	thinkingOpen    bool
	thinkingClosed  bool
	thinkingIndex   int64
	thinkingSig     string
	toolBlockOpen   bool
	toolBlockClosed bool
	toolBlockIndex  int64
	finished        bool
	stopReason      string
	usage           bedrockUsage
}

type bedrockUsage struct {
	InputTokens               int64
	OutputTokens              int64
	TotalTokens               int64
	CacheReadInputTokens      int64
	CacheCreationInputTokens  int64
	CacheWriteInputTokens     int64
	ReasoningTokens           int64
	CacheReadInputTokensSet   bool
	CacheCreateInputTokensSet bool
	ReasoningTokensSet        bool
}

func NormalizeBedrockAPI(value string, stream bool) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "converse":
		if stream {
			return "converse-stream"
		}
		return "converse"
	case "converse-stream", "conversestream", "converse_stream":
		return "converse-stream"
	case "invoke", "invoke-model", "invokemodel":
		if stream {
			return "invoke-stream"
		}
		return "invoke"
	case "invoke-stream", "invoke-with-response-stream", "invokemodelwithresponsestream":
		return "invoke-stream"
	default:
		return strings.ToLower(strings.TrimSpace(value))
	}
}

func BedrockRuntimeURL(baseURL, model, api string) string {
	escapedModel := url.PathEscape(strings.TrimSpace(model))
	switch NormalizeBedrockAPI(api, strings.Contains(api, "stream")) {
	case "converse-stream":
		return strings.TrimSuffix(baseURL, "/") + "/model/" + escapedModel + "/converse-stream"
	case "invoke":
		return strings.TrimSuffix(baseURL, "/") + "/model/" + escapedModel + "/invoke"
	case "invoke-stream":
		return strings.TrimSuffix(baseURL, "/") + "/model/" + escapedModel + "/invoke-with-response-stream"
	default:
		return strings.TrimSuffix(baseURL, "/") + "/model/" + escapedModel + "/converse"
	}
}

func BedrockPayloadForAPI(api, model string, claudeBody []byte, stream bool) []byte {
	switch NormalizeBedrockAPI(api, stream) {
	case "invoke", "invoke-stream":
		return ClaudeMessagesToBedrockInvoke(model, claudeBody, stream)
	default:
		return ClaudeMessagesToBedrockConverse(claudeBody)
	}
}

func ClaudeMessagesToBedrockConverse(body []byte) []byte {
	root := gjson.ParseBytes(body)
	out := []byte(`{"messages":[]}`)
	if system := bedrockSystemBlocks(root.Get("system")); len(system) > 0 {
		out, _ = sjson.SetBytes(out, "system", system)
	}
	root.Get("messages").ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		if role == "" {
			role = "user"
		}
		bedrockMsg := []byte(`{"role":"","content":[]}`)
		bedrockMsg, _ = sjson.SetBytes(bedrockMsg, "role", role)
		for _, block := range bedrockContentBlocks(msg.Get("content")) {
			bedrockMsg, _ = sjson.SetRawBytes(bedrockMsg, "content.-1", block)
		}
		if gjson.GetBytes(bedrockMsg, "content").Array() != nil {
			out, _ = sjson.SetRawBytes(out, "messages.-1", bedrockMsg)
		}
		return true
	})
	inference := make(map[string]any)
	if v := root.Get("max_tokens"); v.Exists() {
		inference["maxTokens"] = v.Int()
	}
	if v := root.Get("temperature"); v.Exists() {
		inference["temperature"] = v.Float()
	}
	if v := root.Get("top_p"); v.Exists() {
		inference["topP"] = v.Float()
	}
	if v := root.Get("stop_sequences"); v.Exists() && v.IsArray() {
		stops := make([]string, 0, len(v.Array()))
		v.ForEach(func(_, stop gjson.Result) bool {
			if s := stop.String(); s != "" {
				stops = append(stops, s)
			}
			return true
		})
		if len(stops) > 0 {
			inference["stopSequences"] = stops
		}
	}
	if len(inference) > 0 {
		out, _ = sjson.SetBytes(out, "inferenceConfig", inference)
	}
	if toolConfig := bedrockToolConfig(root); toolConfig != nil {
		out, _ = sjson.SetBytes(out, "toolConfig", toolConfig)
	}
	if fields := bedrockAdditionalModelRequestFields(root); fields != nil {
		out, _ = sjson.SetBytes(out, "additionalModelRequestFields", fields)
	}
	return out
}

func ClaudeMessagesToBedrockInvoke(_ string, body []byte, _ bool) []byte {
	out := bytes.Clone(body)
	out, _ = sjson.SetBytes(out, "anthropic_version", "bedrock-2023-05-31")
	return out
}

func BedrockResponseToClaudeSSE(model string, data []byte) []byte {
	return claudeMessageToSSE(BedrockResponseToClaudeMessage(model, data))
}

func BedrockResponseToClaudeMessage(model string, data []byte) []byte {
	root := gjson.ParseBytes(data)
	if root.Get("type").Exists() || root.Get("content").Exists() {
		return data
	}
	output := root.Get("output.message")
	messageID := fmt.Sprintf("msg_bedrock_%d", time.Now().UnixNano())
	content := make([]any, 0)
	output.Get("content").ForEach(func(_, part gjson.Result) bool {
		if text := part.Get("text").String(); text != "" {
			content = append(content, map[string]any{"type": "text", "text": text})
		} else if tool := part.Get("toolUse"); tool.Exists() {
			input := map[string]any{}
			if v := tool.Get("input"); v.Exists() {
				if parsed, ok := v.Value().(map[string]any); ok {
					input = parsed
				}
			}
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    tool.Get("toolUseId").String(),
				"name":  tool.Get("name").String(),
				"input": input,
			})
		} else if reasoning := part.Get("reasoningContent.reasoningText"); reasoning.Exists() {
			block := map[string]any{
				"type":     "thinking",
				"thinking": reasoning.Get("text").String(),
			}
			if signature := reasoning.Get("signature").String(); signature != "" {
				block["signature"] = signature
			}
			content = append(content, block)
		}
		return true
	})
	return mustJSON(map[string]any{
		"id":            messageID,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   mapBedrockStopReason(root.Get("stopReason").String()),
		"stop_sequence": nil,
		"usage":         claudeUsageFromBedrock(root.Get("usage")),
	})
}

func NewBedrockStreamNormalizer(model string) *BedrockStreamNormalizer {
	return &BedrockStreamNormalizer{
		id:    fmt.Sprintf("msg_bedrock_%d", time.Now().UnixNano()),
		model: model,
	}
}

func (n *BedrockStreamNormalizer) ConvertLine(line []byte) [][]byte {
	payload := bytes.TrimSpace(line)
	if n.finished {
		return nil
	}
	if bytes.HasPrefix(payload, []byte("data:")) {
		payload = bytes.TrimSpace(payload[len("data:"):])
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		return n.Finish()
	}
	if decoded, ok := decodeBedrockInvokeChunk(payload); ok {
		payload = decoded
	}
	root := gjson.ParseBytes(payload)
	var out [][]byte
	out = append(out, n.start()...)
	if toolStart := root.Get("contentBlockStart.start.toolUse"); toolStart.Exists() {
		index := root.Get("contentBlockStart.contentBlockIndex")
		out = append(out, n.openToolBlock(index.Int(), toolStart.Get("toolUseId").String(), toolStart.Get("name").String())...)
	}
	if reasoningStart := root.Get("contentBlockStart.start.reasoningContent.reasoningText"); reasoningStart.Exists() || root.Get("contentBlockStart.start.reasoningContent").Exists() {
		index := root.Get("contentBlockStart.contentBlockIndex")
		out = append(out, n.openThinkingBlock(index.Int(), reasoningStart.Get("signature").String())...)
	}
	delta := root.Get("contentBlockDelta.delta.text")
	index := root.Get("contentBlockDelta.contentBlockIndex")
	if !delta.Exists() {
		delta = root.Get("delta.text")
		index = root.Get("contentBlockIndex")
	}
	if delta.Exists() {
		out = append(out, n.openTextBlock(index.Int())...)
		out = append(out, claudeDataLine(mustJSON(map[string]any{
			"type":  "content_block_delta",
			"index": index.Int(),
			"delta": map[string]any{"type": "text_delta", "text": delta.String()},
		})))
	}
	if toolDelta := root.Get("contentBlockDelta.delta.toolUse.input"); toolDelta.Exists() {
		index := root.Get("contentBlockDelta.contentBlockIndex")
		out = append(out, n.openToolBlock(index.Int(), "", "")...)
		out = append(out, claudeDataLine(mustJSON(map[string]any{
			"type":  "content_block_delta",
			"index": index.Int(),
			"delta": map[string]any{"type": "input_json_delta", "partial_json": toolDelta.String()},
		})))
	}
	if reasoningDelta := root.Get("contentBlockDelta.delta.reasoningContent.text"); reasoningDelta.Exists() {
		index := root.Get("contentBlockDelta.contentBlockIndex")
		out = append(out, n.openThinkingBlock(index.Int(), "")...)
		out = append(out, claudeDataLine(mustJSON(map[string]any{
			"type":  "content_block_delta",
			"index": index.Int(),
			"delta": map[string]any{"type": "thinking_delta", "thinking": reasoningDelta.String()},
		})))
	}
	if stopIndex := root.Get("contentBlockStop.contentBlockIndex"); stopIndex.Exists() {
		out = append(out, n.closeBlock(stopIndex.Int())...)
	}
	reason := root.Get("messageStop.stopReason")
	if !reason.Exists() {
		reason = root.Get("stopReason")
	}
	if reason.Exists() {
		n.stopReason = mapBedrockStopReason(reason.String())
	}
	usage := root.Get("metadata.usage")
	if !usage.Exists() {
		usage = root.Get("usage")
	}
	if usage.Exists() {
		n.usage = bedrockUsageFromResult(usage)
	}
	if root.Get("type").String() != "" {
		return [][]byte{append([]byte("data: "), payload...)}
	}
	return out
}

func (n *BedrockStreamNormalizer) Finish() [][]byte {
	if n.finished {
		return nil
	}
	n.finished = true
	var out [][]byte
	out = append(out, n.start()...)
	if n.textBlockOpen && !n.textBlockClosed {
		out = append(out, n.closeBlock(n.textBlockIndex)...)
	}
	if n.thinkingOpen && !n.thinkingClosed {
		out = append(out, n.closeBlock(n.thinkingIndex)...)
	}
	if n.toolBlockOpen && !n.toolBlockClosed {
		out = append(out, n.closeBlock(n.toolBlockIndex)...)
	}
	if n.stopReason == "" {
		n.stopReason = "end_turn"
	}
	out = append(out, claudeDataLine(mustJSON(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": n.stopReason, "stop_sequence": nil},
		"usage": n.usage.claudeUsageMap(),
	})))
	out = append(out, claudeDataLine([]byte(`{"type":"message_stop"}`)))
	return out
}

func bedrockSystemBlocks(system gjson.Result) []map[string]string {
	var out []map[string]string
	if !system.Exists() {
		return out
	}
	if system.Type == gjson.String && system.String() != "" {
		return []map[string]string{{"text": system.String()}}
	}
	if system.IsArray() {
		system.ForEach(func(_, block gjson.Result) bool {
			if text := block.Get("text").String(); text != "" {
				out = append(out, map[string]string{"text": text})
			}
			return true
		})
	}
	return out
}

func bedrockContentBlocks(content gjson.Result) [][]byte {
	var out [][]byte
	if !content.Exists() {
		return out
	}
	if content.Type == gjson.String && content.String() != "" {
		block := []byte(`{"text":""}`)
		block, _ = sjson.SetBytes(block, "text", content.String())
		return [][]byte{block}
	}
	if !content.IsArray() {
		return out
	}
	content.ForEach(func(_, part gjson.Result) bool {
		block := bedrockContentBlock(part)
		if len(block) > 0 {
			out = append(out, block)
		}
		return true
	})
	return out
}

func bedrockContentBlock(part gjson.Result) []byte {
	switch part.Get("type").String() {
	case "", "text":
		text := part.Get("text").String()
		if text == "" {
			return nil
		}
		block := []byte(`{"text":""}`)
		block, _ = sjson.SetBytes(block, "text", text)
		return block
	case "image":
		return bedrockImageContentBlock(part)
	case "document":
		return bedrockDocumentContentBlock(part)
	case "thinking":
		return bedrockReasoningContentBlock(part)
	case "tool_use":
		block := []byte(`{"toolUse":{"toolUseId":"","name":"","input":{}}}`)
		block, _ = sjson.SetBytes(block, "toolUse.toolUseId", part.Get("id").String())
		block, _ = sjson.SetBytes(block, "toolUse.name", part.Get("name").String())
		if input := part.Get("input"); input.Exists() {
			block, _ = sjson.SetRawBytes(block, "toolUse.input", []byte(input.Raw))
		}
		return block
	case "tool_result":
		block := []byte(`{"toolResult":{"toolUseId":"","content":[]}}`)
		block, _ = sjson.SetBytes(block, "toolResult.toolUseId", part.Get("tool_use_id").String())
		if part.Get("is_error").Bool() {
			block, _ = sjson.SetBytes(block, "toolResult.status", "error")
		}
		for _, item := range bedrockContentBlocks(part.Get("content")) {
			block, _ = sjson.SetRawBytes(block, "toolResult.content.-1", item)
		}
		return block
	default:
		return nil
	}
}

func bedrockImageContentBlock(part gjson.Result) []byte {
	source := part.Get("source")
	if source.Get("type").String() != "base64" {
		return nil
	}
	data := source.Get("data").String()
	if data == "" {
		return nil
	}
	format := bedrockMediaFormat(source.Get("media_type").String())
	if format == "" {
		return nil
	}
	block := []byte(`{"image":{"format":"","source":{"bytes":""}}}`)
	block, _ = sjson.SetBytes(block, "image.format", format)
	block, _ = sjson.SetBytes(block, "image.source.bytes", data)
	return block
}

func bedrockDocumentContentBlock(part gjson.Result) []byte {
	source := part.Get("source")
	if source.Get("type").String() != "base64" {
		return nil
	}
	data := source.Get("data").String()
	if data == "" {
		return nil
	}
	format := bedrockMediaFormat(source.Get("media_type").String())
	if format == "" {
		return nil
	}
	name := strings.TrimSpace(part.Get("title").String())
	if name == "" {
		name = strings.TrimSpace(part.Get("name").String())
	}
	if name == "" {
		name = "document " + format
	}
	name = bedrockDocumentName(name)
	block := []byte(`{"document":{"format":"","name":"","source":{"bytes":""}}}`)
	block, _ = sjson.SetBytes(block, "document.format", format)
	block, _ = sjson.SetBytes(block, "document.name", name)
	block, _ = sjson.SetBytes(block, "document.source.bytes", data)
	return block
}

var bedrockDocumentNameDisallowed = regexp.MustCompile(`[^A-Za-z0-9 \-\[\]\(\)]`)
var bedrockDocumentNameSpaces = regexp.MustCompile(`\s+`)

func bedrockDocumentName(name string) string {
	name = bedrockDocumentNameDisallowed.ReplaceAllString(name, " ")
	name = bedrockDocumentNameSpaces.ReplaceAllString(strings.TrimSpace(name), " ")
	if name == "" {
		return "document"
	}
	return name
}

func bedrockReasoningContentBlock(part gjson.Result) []byte {
	thinking := part.Get("thinking").String()
	signature := part.Get("signature").String()
	if thinking == "" && signature == "" {
		return nil
	}
	block := []byte(`{"reasoningContent":{"reasoningText":{"text":""}}}`)
	block, _ = sjson.SetBytes(block, "reasoningContent.reasoningText.text", thinking)
	if signature != "" {
		block, _ = sjson.SetBytes(block, "reasoningContent.reasoningText.signature", signature)
	}
	return block
}

func bedrockMediaFormat(mediaType string) string {
	mediaType = strings.ToLower(strings.TrimSpace(strings.Split(mediaType, ";")[0]))
	switch mediaType {
	case "image/jpeg", "image/jpg":
		return "jpeg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	case "application/pdf":
		return "pdf"
	case "text/csv":
		return "csv"
	case "text/html":
		return "html"
	case "text/plain":
		return "txt"
	case "text/markdown":
		return "md"
	case "application/vnd.openxmlformats-officedocument.wordprocessingml.document":
		return "docx"
	case "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":
		return "xlsx"
	default:
		if slash := strings.LastIndex(mediaType, "/"); slash >= 0 && slash+1 < len(mediaType) {
			return strings.TrimPrefix(mediaType[slash+1:], "x-")
		}
		return ""
	}
}

func bedrockToolConfig(root gjson.Result) map[string]any {
	tools := root.Get("tools")
	if !tools.Exists() || !tools.IsArray() {
		return nil
	}
	out := map[string]any{"tools": []any{}}
	values := make([]any, 0, len(tools.Array()))
	tools.ForEach(func(_, tool gjson.Result) bool {
		if tool.Get("type").String() == "function" {
			tool = tool.Get("function")
		}
		name := tool.Get("name").String()
		if name == "" {
			return true
		}
		spec := map[string]any{
			"name":        name,
			"description": tool.Get("description").String(),
		}
		if schema := tool.Get("input_schema"); schema.Exists() {
			spec["inputSchema"] = map[string]any{"json": schema.Value()}
		} else if schema := tool.Get("parameters"); schema.Exists() {
			spec["inputSchema"] = map[string]any{"json": schema.Value()}
		} else if schema := tool.Get("parametersJsonSchema"); schema.Exists() {
			spec["inputSchema"] = map[string]any{"json": schema.Value()}
		}
		values = append(values, map[string]any{"toolSpec": spec})
		return true
	})
	if len(values) == 0 {
		return nil
	}
	out["tools"] = values
	if choice := root.Get("tool_choice"); choice.Exists() {
		switch choice.Get("type").String() {
		case "auto":
			out["toolChoice"] = map[string]any{"auto": map[string]any{}}
		case "any":
			out["toolChoice"] = map[string]any{"any": map[string]any{}}
		case "tool":
			out["toolChoice"] = map[string]any{"tool": map[string]any{"name": choice.Get("name").String()}}
		case "function":
			if name := choice.Get("function.name").String(); name != "" {
				out["toolChoice"] = map[string]any{"tool": map[string]any{"name": name}}
			}
		}
	} else if choice.Type == gjson.String {
		switch choice.String() {
		case "auto":
			out["toolChoice"] = map[string]any{"auto": map[string]any{}}
		case "required":
			out["toolChoice"] = map[string]any{"any": map[string]any{}}
		}
	}
	return out
}

func bedrockAdditionalModelRequestFields(root gjson.Result) map[string]any {
	fields := make(map[string]any)
	if format := root.Get("response_format"); format.Exists() {
		fields["response_format"] = format.Value()
	} else if format := root.Get("text.format"); format.Exists() {
		fields["response_format"] = format.Value()
	}
	if outputConfig := root.Get("output_config"); outputConfig.Exists() {
		fields["output_config"] = outputConfig.Value()
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func claudeMessageToSSE(data []byte) []byte {
	root := gjson.ParseBytes(data)
	messageID := root.Get("id").String()
	if messageID == "" {
		messageID = fmt.Sprintf("msg_bedrock_%d", time.Now().UnixNano())
	}
	model := root.Get("model").String()
	var lines [][]byte
	lines = append(lines, claudeDataLine(mustJSON(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         root.Get("usage").Value(),
		},
	})))
	index := 0
	root.Get("content").ForEach(func(_, part gjson.Result) bool {
		for _, event := range bedrockContentPartToClaudeEvents(index, part) {
			lines = append(lines, claudeDataLine(event))
		}
		index++
		return true
	})
	lines = append(lines, claudeDataLine(mustJSON(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": mapBedrockStopReason(root.Get("stop_reason").String()), "stop_sequence": nil},
		"usage": root.Get("usage").Value(),
	})))
	lines = append(lines, claudeDataLine([]byte(`{"type":"message_stop"}`)))
	return bytes.Join(lines, []byte("\n"))
}

func bedrockContentPartToClaudeEvents(index int, part gjson.Result) [][]byte {
	if text := part.Get("text").String(); text != "" {
		return [][]byte{
			mustJSON(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "text", "text": ""}}),
			mustJSON(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": text}}),
			mustJSON(map[string]any{"type": "content_block_stop", "index": index}),
		}
	}
	if part.Get("type").String() == "text" {
		return [][]byte{
			mustJSON(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "text", "text": ""}}),
			mustJSON(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": part.Get("text").String()}}),
			mustJSON(map[string]any{"type": "content_block_stop", "index": index}),
		}
	}
	if tool := part.Get("toolUse"); tool.Exists() {
		input := "{}"
		if v := tool.Get("input"); v.Exists() {
			input = v.Raw
		}
		return [][]byte{
			mustJSON(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": tool.Get("toolUseId").String(), "name": tool.Get("name").String(), "input": map[string]any{}}}),
			mustJSON(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": input}}),
			mustJSON(map[string]any{"type": "content_block_stop", "index": index}),
		}
	}
	if part.Get("type").String() == "tool_use" {
		input := "{}"
		if v := part.Get("input"); v.Exists() {
			input = v.Raw
		}
		return [][]byte{
			mustJSON(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "tool_use", "id": part.Get("id").String(), "name": part.Get("name").String(), "input": map[string]any{}}}),
			mustJSON(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "input_json_delta", "partial_json": input}}),
			mustJSON(map[string]any{"type": "content_block_stop", "index": index}),
		}
	}
	return nil
}

func claudeUsageFromBedrock(usage gjson.Result) map[string]any {
	return bedrockUsageFromResult(usage).claudeUsageMap()
}

func bedrockUsageFromResult(usage gjson.Result) bedrockUsage {
	out := bedrockUsage{
		InputTokens:  usage.Get("inputTokens").Int(),
		OutputTokens: usage.Get("outputTokens").Int(),
		TotalTokens:  usage.Get("totalTokens").Int(),
	}
	if v := usage.Get("cacheReadInputTokens"); v.Exists() {
		out.CacheReadInputTokens = v.Int()
		out.CacheReadInputTokensSet = true
	}
	if v := usage.Get("cacheWriteInputTokens"); v.Exists() {
		out.CacheWriteInputTokens = v.Int()
		out.CacheCreationInputTokens = v.Int()
		out.CacheCreateInputTokensSet = true
	}
	if v := usage.Get("cacheCreationInputTokens"); v.Exists() {
		out.CacheCreationInputTokens = v.Int()
		out.CacheCreateInputTokensSet = true
	}
	if v := usage.Get("reasoningTokens"); v.Exists() {
		out.ReasoningTokens = v.Int()
		out.ReasoningTokensSet = true
	}
	if v := usage.Get("thinkingTokens"); v.Exists() {
		out.ReasoningTokens = v.Int()
		out.ReasoningTokensSet = true
	}
	return out
}

func (u bedrockUsage) claudeUsageMap() map[string]any {
	out := map[string]any{
		"input_tokens":  u.InputTokens,
		"output_tokens": u.OutputTokens,
	}
	if u.CacheReadInputTokensSet {
		out["cache_read_input_tokens"] = u.CacheReadInputTokens
	}
	if u.CacheCreateInputTokensSet {
		out["cache_creation_input_tokens"] = u.CacheCreationInputTokens
	}
	if u.ReasoningTokensSet {
		out["thinking_tokens"] = u.ReasoningTokens
	}
	return out
}

func mapBedrockStopReason(reason string) string {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "tool_use":
		return "tool_use"
	case "max_tokens":
		return "max_tokens"
	case "stop_sequence":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

func (n *BedrockStreamNormalizer) start() [][]byte {
	if n.started {
		return nil
	}
	n.started = true
	return [][]byte{claudeDataLine(mustJSON(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            n.id,
			"type":          "message",
			"role":          "assistant",
			"model":         n.model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	}))}
}

func (n *BedrockStreamNormalizer) openTextBlock(index int64) [][]byte {
	if n.textBlockOpen && !n.textBlockClosed && n.textBlockIndex == index {
		return nil
	}
	n.textBlockOpen = true
	n.textBlockClosed = false
	n.textBlockIndex = index
	return [][]byte{claudeDataLine(mustJSON(map[string]any{
		"type":          "content_block_start",
		"index":         index,
		"content_block": map[string]any{"type": "text", "text": ""},
	}))}
}

func (n *BedrockStreamNormalizer) openThinkingBlock(index int64, signature string) [][]byte {
	if n.thinkingOpen && !n.thinkingClosed && n.thinkingIndex == index {
		return nil
	}
	n.thinkingOpen = true
	n.thinkingClosed = false
	n.thinkingIndex = index
	if signature != "" {
		n.thinkingSig = signature
	}
	contentBlock := map[string]any{"type": "thinking", "thinking": ""}
	if n.thinkingSig != "" {
		contentBlock["signature"] = n.thinkingSig
	}
	return [][]byte{claudeDataLine(mustJSON(map[string]any{
		"type":          "content_block_start",
		"index":         index,
		"content_block": contentBlock,
	}))}
}

func (n *BedrockStreamNormalizer) openToolBlock(index int64, id, name string) [][]byte {
	if n.toolBlockOpen && !n.toolBlockClosed && n.toolBlockIndex == index {
		return nil
	}
	n.toolBlockOpen = true
	n.toolBlockClosed = false
	n.toolBlockIndex = index
	contentBlock := map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}}
	return [][]byte{claudeDataLine(mustJSON(map[string]any{
		"type":          "content_block_start",
		"index":         index,
		"content_block": contentBlock,
	}))}
}

func (n *BedrockStreamNormalizer) closeBlock(index int64) [][]byte {
	if n.textBlockOpen && !n.textBlockClosed && n.textBlockIndex == index {
		n.textBlockClosed = true
		return [][]byte{claudeDataLine(mustJSON(map[string]any{"type": "content_block_stop", "index": index}))}
	}
	if n.thinkingOpen && !n.thinkingClosed && n.thinkingIndex == index {
		n.thinkingClosed = true
		return [][]byte{claudeDataLine(mustJSON(map[string]any{"type": "content_block_stop", "index": index}))}
	}
	if n.toolBlockOpen && !n.toolBlockClosed && n.toolBlockIndex == index {
		n.toolBlockClosed = true
		return [][]byte{claudeDataLine(mustJSON(map[string]any{"type": "content_block_stop", "index": index}))}
	}
	return nil
}

func decodeBedrockInvokeChunk(payload []byte) ([]byte, bool) {
	root := gjson.ParseBytes(payload)
	bytesField := root.Get("chunk.bytes")
	if !bytesField.Exists() {
		return nil, false
	}
	decoded, err := base64.StdEncoding.DecodeString(bytesField.String())
	if err != nil || len(decoded) == 0 {
		return nil, false
	}
	return decoded, true
}

func claudeDataLine(payload []byte) []byte {
	line := make([]byte, 0, len(payload)+6)
	line = append(line, "data: "...)
	line = append(line, payload...)
	return line
}

func mustJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return []byte(`{}`)
	}
	return data
}
