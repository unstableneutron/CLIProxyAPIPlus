package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/sjson"
)

const codexCompactMessageTokenBudget = 64000

const codexCompactUnsupportedStatefulError = `{"error":{"message":"Stateful compact requests are not supported for Codex OAuth. Resend the complete transcript in input and omit previous_response_id.","type":"invalid_request_error","code":"unsupported_stateful_compact"}}`

func (e *CodexExecutor) executeOAuthCompact(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	requestObject, err := codexCompactDecodeJSONObject(req.Payload)
	if err != nil {
		return cliproxyexecutor.Response{}, codexCompactInvalidRequestErr(err)
	}
	if _, exists := requestObject["previous_response_id"]; exists {
		return cliproxyexecutor.Response{}, newCodexStatusErr(http.StatusBadRequest, []byte(codexCompactUnsupportedStatefulError))
	}
	if err = codexCompactValidateRequest(requestObject); err != nil {
		return cliproxyexecutor.Response{}, codexCompactInvalidRequestErr(err)
	}
	retained := codexCompactRetainedUserMessages(requestObject["input"])
	input, err := codexCompactResponsesInput(requestObject["input"])
	if err != nil {
		return cliproxyexecutor.Response{}, codexCompactInvalidRequestErr(err)
	}
	input = append(input, map[string]any{"type": "compaction_trigger"})
	requestObject["input"] = input
	requestObject["stream"] = true
	requestObject["store"] = false
	requestObject["include"] = []any{"reasoning.encrypted_content"}
	delete(requestObject, "prompt_cache_options")
	delete(requestObject, "prompt_cache_retention")
	prepared, err := json.Marshal(requestObject)
	if err != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("codex compact: encode request: %w", err)
	}

	normalOpts := opts
	normalOpts.Alt = ""
	normalOpts.Stream = false
	normalOpts.SourceFormat = sdktranslator.FromString("openai-response")
	normalOpts.ResponseFormat = sdktranslator.FromString("openai-response")
	normalReq := req
	normalReq.Payload = prepared
	normalReq.Format = sdktranslator.FromString("openai-response")
	response, err := e.executeResponses(ctx, auth, normalReq, normalOpts, false)
	if err != nil {
		var incomplete codexIncompleteStreamError
		if errors.As(err, &incomplete) {
			return cliproxyexecutor.Response{}, codexCompactUpstreamProtocolErr()
		}
		return cliproxyexecutor.Response{}, err
	}
	envelope, err := buildCodexCompactEnvelope(response.Payload, retained)
	if err != nil {
		return cliproxyexecutor.Response{}, codexCompactUpstreamProtocolErr()
	}
	response.Payload = envelope
	if response.Headers == nil {
		response.Headers = make(http.Header)
	} else {
		response.Headers = response.Headers.Clone()
	}
	response.Headers.Set("Content-Type", "application/json")
	return response, nil
}

func codexCompactUpstreamProtocolErr() error {
	return newCodexStatusErr(http.StatusBadGateway, []byte(`{"error":{"message":"invalid compaction response from upstream","type":"upstream_protocol_error"}}`))
}

func codexCompactInvalidRequestErr(err error) error {
	payload, marshalErr := json.Marshal(map[string]any{"error": map[string]any{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
	if marshalErr != nil {
		payload = []byte(`{"error":{"message":"Invalid request","type":"invalid_request_error"}}`)
	}
	return newCodexStatusErr(http.StatusBadRequest, payload)
}

func codexCompactValidateRequest(request map[string]any) error {
	model, ok := request["model"].(string)
	if !ok || strings.TrimSpace(model) == "" {
		return fmt.Errorf("model must be a nonempty string")
	}
	if input, exists := request["input"]; exists && input != nil {
		if _, stringInput := input.(string); !stringInput {
			items, arrayInput := input.([]any)
			if !arrayInput {
				return fmt.Errorf("input must be a string or an array")
			}
			for index, item := range items {
				if _, objectInput := item.(map[string]any); !objectInput {
					return fmt.Errorf("input[%d] must be an object", index)
				}
			}
		}
	}
	if stream, exists := request["stream"]; exists {
		if _, ok := stream.(bool); !ok {
			return fmt.Errorf("stream must be a boolean")
		}
	}
	return nil
}

func codexCompactDecodeJSONObject(payload []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if value == nil {
		return nil, fmt.Errorf("expected JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("trailing JSON value")
		}
		return nil, fmt.Errorf("trailing JSON: %w", err)
	}
	return value, nil
}

func codexCompactCloneAnyMap(source map[string]any) map[string]any {
	clone := make(map[string]any, len(source)+1)
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

func codexCompactResponsesInput(raw any) ([]any, error) {
	if raw == nil {
		return nil, nil
	}
	if text, ok := raw.(string); ok {
		return []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}}, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("input must be null, a string, or an array")
	}
	result := make([]any, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if ok && object["type"] == "compaction_trigger" {
			continue
		}
		result = append(result, item)
	}
	return result, nil
}

func finalizeCodexCompactAdapterBody(body []byte) ([]byte, error) {
	request, err := codexCompactDecodeJSONObject(body)
	if err != nil {
		return nil, fmt.Errorf("codex compact: finalize adapter request: %w", err)
	}
	input, err := codexCompactResponsesInput(request["input"])
	if err != nil {
		return nil, fmt.Errorf("codex compact: finalize adapter input: %w", err)
	}
	request["input"] = append(input, map[string]any{"type": "compaction_trigger"})
	request["stream"] = true
	request["store"] = false
	request["include"] = []any{"reasoning.encrypted_content"}
	if tools, ok := request["tools"].([]any); ok {
		filtered := make([]any, 0, len(tools))
		for _, tool := range tools {
			object, objectOK := tool.(map[string]any)
			if objectOK && object["type"] == "image_generation" {
				continue
			}
			filtered = append(filtered, tool)
		}
		request["tools"] = filtered
	}
	finalized, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("codex compact: encode finalized adapter request: %w", err)
	}
	return finalized, nil
}

func patchCodexCompactCompletedOutput(eventData []byte, items [][]byte) []byte {
	var output bytes.Buffer
	output.WriteByte('[')
	for index, item := range items {
		if index > 0 {
			output.WriteByte(',')
		}
		output.Write(item)
	}
	output.WriteByte(']')
	patched, err := sjson.SetRawBytes(eventData, "response.output", output.Bytes())
	if err != nil {
		return eventData
	}
	return patched
}

func codexCompactRetainedUserMessages(raw any) []map[string]any {
	var messages []map[string]any
	if text, ok := raw.(string); ok {
		messages = append(messages, map[string]any{"type": "message", "role": "user", "status": "completed", "content": []any{map[string]any{"type": "input_text", "text": text}}})
	} else if items, ok := raw.([]any); ok {
		for _, item := range items {
			object, okObject := item.(map[string]any)
			if !okObject || object["role"] != "user" || (object["type"] != nil && object["type"] != "message") {
				continue
			}
			message := codexCompactCloneAnyMap(object)
			message["type"] = "message"
			message["role"] = "user"
			message["status"] = "completed"
			if text, stringContent := message["content"].(string); stringContent {
				message["content"] = []any{map[string]any{"type": "input_text", "text": text}}
			} else if _, exists := message["content"]; !exists {
				message["content"] = []any{}
			}
			messages = append(messages, message)
		}
	}
	budget := codexCompactMessageTokenBudget
	start := len(messages)
	for start > 0 {
		cost := codexCompactEstimatedTokens(messages[start-1])
		if cost > budget {
			if budget > 0 {
				messages[start-1] = codexCompactTruncateMessage(messages[start-1], budget)
				start--
			}
			break
		}
		budget -= cost
		start--
	}
	return messages[start:]
}

const codexCompactTruncationMarker = "\n…[truncated]…\n"

func codexCompactTruncateMessage(message map[string]any, tokenBudget int) map[string]any {
	clone := codexCompactCloneAnyMap(message)
	content, ok := message["content"].([]any)
	if !ok {
		return clone
	}
	clonedContent := make([]any, len(content))
	for index, item := range content {
		object, objectOK := item.(map[string]any)
		if !objectOK {
			clonedContent[index] = item
			continue
		}
		clonedObject := codexCompactCloneAnyMap(object)
		clonedContent[index] = clonedObject
	}
	remainingBytes := tokenBudget*4 + 3
	for index := len(clonedContent) - 1; index >= 0; index-- {
		object, okObject := clonedContent[index].(map[string]any)
		if !okObject {
			continue
		}
		text, okText := object["text"].(string)
		if !okText {
			continue
		}
		if len(text) <= remainingBytes {
			remainingBytes -= len(text)
			continue
		}
		object["text"] = codexCompactTruncateUTF8(text, remainingBytes)
		remainingBytes = 0
	}
	clone["content"] = clonedContent
	return clone
}

func codexCompactTruncateUTF8(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	if maxBytes <= 0 {
		return ""
	}
	marker := codexCompactTruncationMarker
	if maxBytes < len(marker) {
		marker = "…"
		if maxBytes < len(marker) {
			return ""
		}
	}
	available := maxBytes - len(marker)
	headBytes := available / 2
	tailBytes := available - headBytes
	head := text[:headBytes]
	for !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	tail := text[len(text)-tailBytes:]
	for !utf8.ValidString(tail) {
		tail = tail[1:]
	}
	return head + marker + tail
}

func codexCompactEstimatedTokens(message map[string]any) int {
	bytes := codexCompactTextBytes(message["content"])
	tokens := bytes / 4
	if tokens < 1 {
		return 1
	}
	return tokens
}

func codexCompactTextBytes(value any) int {
	switch typed := value.(type) {
	case string:
		return len([]byte(typed))
	case []any:
		total := 0
		for _, item := range typed {
			total += codexCompactTextBytes(item)
		}
		return total
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return len([]byte(text))
		}
	}
	return 0
}

func buildCodexCompactEnvelope(payload []byte, messages []map[string]any) ([]byte, error) {
	response, err := codexCompactDecodeJSONObject(payload)
	if err != nil {
		return nil, err
	}
	id, _ := response["id"].(string)
	created, hasCreated := response["created_at"].(json.Number)
	usage, hasUsage := response["usage"].(map[string]any)
	if status, exists := response["status"]; !exists || status != "completed" {
		return nil, fmt.Errorf("terminal response is not completed")
	}
	if id == "" {
		return nil, fmt.Errorf("missing response id")
	}
	if !hasCreated {
		return nil, fmt.Errorf("missing or invalid created_at")
	}
	if _, err := strconv.ParseInt(string(created), 10, 64); err != nil {
		return nil, fmt.Errorf("created_at must be an integer")
	}
	if !hasUsage {
		return nil, fmt.Errorf("missing or invalid usage")
	}
	for _, field := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		if !codexCompactNonnegativeInteger(usage[field]) {
			return nil, fmt.Errorf("usage.%s must be a nonnegative integer", field)
		}
	}
	if err := codexCompactNormalizeUsage(usage); err != nil {
		return nil, err
	}
	items, _ := response["output"].([]any)
	var compaction map[string]any
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok || object["type"] != "compaction" {
			continue
		}
		if compaction != nil {
			return nil, fmt.Errorf("terminal response has multiple compaction items")
		}
		compaction = object
	}
	if compaction == nil {
		return nil, fmt.Errorf("terminal response has no compaction item")
	}
	encrypted, _ := compaction["encrypted_content"].(string)
	if encrypted == "" {
		return nil, fmt.Errorf("compaction item has no encrypted content")
	}
	if itemID, _ := compaction["id"].(string); itemID == "" {
		compaction["id"] = "cmp_" + strings.TrimPrefix(id, "resp_")
	}
	output := make([]any, 0, len(messages)+1)
	for index, message := range messages {
		if messageID, _ := message["id"].(string); messageID == "" {
			message["id"] = fmt.Sprintf("msg_%s_%d", strings.TrimPrefix(id, "resp_"), index+1)
		}
		output = append(output, message)
	}
	output = append(output, compaction)
	return json.Marshal(map[string]any{"id": id, "created_at": created, "object": "response.compaction", "output": output, "usage": usage})
}

func codexCompactNormalizeUsage(usage map[string]any) error {
	input, ok := usage["input_tokens_details"].(map[string]any)
	if !ok && usage["input_tokens_details"] != nil {
		return fmt.Errorf("usage.input_tokens_details must be an object")
	}
	if input == nil {
		input = map[string]any{}
		usage["input_tokens_details"] = input
	}
	for field, value := range input {
		if !codexCompactNonnegativeInteger(value) {
			return fmt.Errorf("usage.input_tokens_details.%s must be a nonnegative integer", field)
		}
	}
	for _, field := range []string{"cached_tokens", "cache_write_tokens"} {
		if _, exists := input[field]; !exists {
			input[field] = json.Number("0")
		}
	}
	output, ok := usage["output_tokens_details"].(map[string]any)
	if !ok && usage["output_tokens_details"] != nil {
		return fmt.Errorf("usage.output_tokens_details must be an object")
	}
	if output == nil {
		output = map[string]any{}
		usage["output_tokens_details"] = output
	}
	for field, value := range output {
		if !codexCompactNonnegativeInteger(value) {
			return fmt.Errorf("usage.output_tokens_details.%s must be a nonnegative integer", field)
		}
	}
	if _, exists := output["reasoning_tokens"]; !exists {
		output["reasoning_tokens"] = json.Number("0")
	}
	return nil
}

func codexCompactNonnegativeInteger(value any) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	parsed, err := strconv.ParseInt(string(number), 10, 64)
	return err == nil && parsed >= 0
}

func codexExecutorPreservePreviousResponseID(opts cliproxyexecutor.Options) bool {
	if len(opts.Metadata) == 0 {
		return false
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ResponsesStateModeMetadataKey]
	if !ok || raw == nil {
		return false
	}
	mode := ""
	switch v := raw.(type) {
	case string:
		mode = strings.TrimSpace(v)
	case []byte:
		mode = strings.TrimSpace(string(v))
	default:
		return false
	}
	switch strings.ToLower(mode) {
	case cliproxyexecutor.ResponsesStateModeProbe, cliproxyexecutor.ResponsesStateModeStateful:
		return true
	default:
		return false
	}
}
