package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	qoderauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// QoderExecutor executes requests against the Qoder API with COSY authentication
type QoderExecutor struct {
	cfg *config.Config
}

// NewQoderExecutor creates a new Qoder executor
func NewQoderExecutor(cfg *config.Config) *QoderExecutor {
	return &QoderExecutor{
		cfg: cfg,
	}
}

// Identifier returns the provider identifier
func (e *QoderExecutor) Identifier() string {
	return "qoder"
}

// ExecuteStream executes a streaming request against Qoder API
func (e *QoderExecutor) ExecuteStream(ctx context.Context, authRecord *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	// Get token storage from auth record
	storage, ok := authRecord.Storage.(*qoderauth.QoderTokenStorage)
	if !ok {
		return nil, fmt.Errorf("invalid auth storage type for qoder: %T", authRecord.Storage)
	}

	// Check if token needs refresh
	bufferSeconds := int64(600) // 10 minutes
	authFilePath := ""
	if authRecord.Attributes != nil {
		authFilePath = strings.TrimSpace(authRecord.Attributes["path"])
	}
	if err := qoderauth.RefreshTokenIfNeeded(ctx, e.cfg, storage, bufferSeconds, authFilePath); err != nil {
		log.Warnf("Qoder token refresh failed: %v", err)
	}

	// Translate non-openai formats to chat completions before extracting messages
	payload := req.Payload
	if opts.SourceFormat != "" && opts.SourceFormat != sdktranslator.FormatOpenAI {
		payload = sdktranslator.TranslateRequest(opts.SourceFormat, sdktranslator.FormatOpenAI, req.Model, payload, false)
	}

	// Parse request to get model and messages
	var chatReq map[string]interface{}
	if err := json.Unmarshal(payload, &chatReq); err != nil {
		return nil, fmt.Errorf("failed to parse request: %w", err)
	}

	// Map model name
	model, _ := chatReq["model"].(string)
	qoderModel := model
	if mapped, ok := qoderauth.ModelMap[model]; ok {
		qoderModel = mapped
	}

	// Convert messages to prompt format and normalize tool history
	messagesRaw, _ := chatReq["messages"].([]interface{})
	toolsRaw := chatReq["tools"]
	normalized := normalizeQoderMessages(messagesRaw)
	useNormalized := hasToolHistory(messagesRaw)
	prompt := messagesToPromptGeneric(normalized, toolsRaw)

	requestID := uuid.New().String()
	sessionID := uuid.New().String()

	// Resolve the per-model server-side metadata (is_vl, is_reasoning,
	// max_input_tokens, ...). Failing here is a hard error — sending the
	// wrong block silently downgrades to a different model.
	modelConfig, err := buildQoderModelConfig(storage, qoderModel)
	if err != nil {
		return nil, err
	}

	// Build request body for Qoder API (agent router payload)
	reqBody := map[string]interface{}{
		"requestId":           requestID,
		"sessionId":           sessionID,
		"questionText":        prompt,
		"references":          []interface{}{},
		"mode":                "agent",
		"sessionType":         "qodercli",
		"chatTask":            "FREE_INPUT",
		"stream":              true,
		"source":              1,
		"isReply":             false,
		"taskDefinitionType":  "system",
		"codeLanguage":        "",
		"preferredLanguage":   "English",
		"closeTypewriter":     true,
		"pluginPayloadConfig": map[string]interface{}{},
		"chatContext": map[string]interface{}{
			"text":              prompt,
			"localeLang":        "English",
			"preferredLanguage": "English",
		},
		"extra": map[string]interface{}{
			"modelConfig": map[string]interface{}{
				"key": qoderModel,
			},
		},
		"request_id":       requestID,
		"request_set_id":   requestID,
		"chat_record_id":   requestID,
		"session_id":       sessionID,
		"agent_id":         "agent_common",
		"task_id":          "common",
		"chat_task":        "FREE_INPUT",
		"version":          "3",
		"aliyun_user_type": "personal_standard",
		"session_type":     "qodercli",
		"parameters": map[string]interface{}{
			"max_new_tokens": 16384,
			"max_tokens":     16384,
		},
		"model_config":     modelConfig,
		"messages": func() []interface{} {
			if useNormalized {
				return normalized
			}
			return messagesRaw
		}(),
	}
	if toolsRaw != nil {
		reqBody["tools"] = toolsRaw
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Build COSY auth headers
	headers, err := qoderauth.BuildAuthHeaders(
		bodyBytes,
		qoderauth.QoderChatURL,
		qoderauth.CosyCredentials{
			UserID:    storage.UserID,
			AuthToken: storage.Token,
			Name:      storage.Name,
			Email:     storage.Email,
			MachineID: storage.MachineID,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build COSY auth: %w", err)
	}

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, "POST", qoderauth.QoderChatURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	httpReq.Header.Set("Content-Type", "application/json")
	headers.Apply(httpReq)
	httpReq.Header.Set("Accept", "text/event-stream")

	// Send request
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, authRecord, 0)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}

	if httpResp.StatusCode != http.StatusOK {
		defer func() { _ = httpResp.Body.Close() }()
		body, _ := io.ReadAll(httpResp.Body)
		return nil, newQoderStatusError(httpResp.StatusCode, string(body))
	}

	// Create streaming channel
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() { _ = httpResp.Body.Close() }()

		var debugFile *os.File
		if debugPath := strings.TrimSpace(os.Getenv("QODER_DEBUG_SSE")); debugPath != "" {
			if f, err := os.OpenFile(debugPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
				debugFile = f
				defer func() { _ = f.Close() }()
			}
		}

		scanner := bufio.NewScanner(httpResp.Body)
		scanner.Buffer(nil, 52_428_800) // 50MB max line

		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			if debugFile != nil {
				_, _ = debugFile.Write(append([]byte("[raw] "), append(line, '\n')...))
			}

			// Skip non-data lines
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}

			data := bytes.TrimPrefix(line, []byte("data:"))
			data = bytes.TrimPrefix(data, []byte(" "))
			if bytes.Equal(data, []byte("[DONE]")) {
				emitDone(ctx, out, opts.SourceFormat, req.Model, opts.OriginalRequest, payload)
				return
			}
			if debugFile != nil {
				_, _ = debugFile.Write(append([]byte("[data] "), append(data, '\n')...))
			}

			// Parse Qoder response envelope
			var event map[string]interface{}
			if err := json.Unmarshal(data, &event); err != nil {
				continue
			}
			statusVal := 200
			if rawStatus, ok := event["statusCodeValue"]; ok {
				switch v := rawStatus.(type) {
				case float64:
					statusVal = int(v)
				case int:
					statusVal = v
				}
			}
			innerStr, _ := event["body"].(string)
			if statusVal != http.StatusOK {
				msg := innerStr
				if msg == "" {
					msg = fmt.Sprintf("upstream status %d", statusVal)
				}
				out <- cliproxyexecutor.StreamChunk{Err: newQoderStatusError(statusVal, msg)}
				return
			}
			if innerStr == "" {
				continue
			}
			if innerStr == "[DONE]" {
				emitDone(ctx, out, opts.SourceFormat, req.Model, opts.OriginalRequest, payload)
				return
			}
			var inner map[string]interface{}
			if err := json.Unmarshal([]byte(innerStr), &inner); err != nil {
				continue
			}
			chunkBytes, err := buildOpenAIChunk(inner, model)
			if err != nil {
				continue
			}
			// Reconstruct an OpenAI-compatible SSE line ("data: {chunk}").
			// Qoder's upstream nests OpenAI chunks inside a
			// {statusCodeValue, body} envelope so unlike kimi/openai-compat/
			// codebuddy we can't forward the raw upstream line — we have to
			// rebuild the SSE frame here. The format matches what those
			// other executors feed into TranslateStream so the translators'
			// "expects data: prefix" assumption holds.
			ssePayload := append([]byte("data: "), chunkBytes...)

			// Always run through TranslateStream. When source==target
			// (OpenAI client) it strips the "data:" prefix and returns
			// raw JSON; the OpenAI handler then re-adds the SSE framing.
			// For cross-format clients (Anthropic/Gemini) it emits the
			// format-specific stream events (message_start /
			// content_block_delta / ...) directly as fully framed bytes
			// because those handlers write chunks verbatim.
			to := sdktranslator.FormatOpenAI
			from := opts.SourceFormat
			if from == "" {
				from = to
			}
			var param any
			frames := sdktranslator.TranslateStream(ctx, to, from,
				req.Model, opts.OriginalRequest, payload, ssePayload, &param)
			for _, frame := range frames {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: frame}:
				case <-ctx.Done():
					return
				}
			}
		}
		// Scanner loop exited naturally (EOF). Emit a terminating
		// "data: [DONE]" / Anthropic message_stop frame so the client
		// closes the stream cleanly.
		emitDone(ctx, out, opts.SourceFormat, req.Model, opts.OriginalRequest, payload)
		// Check for scanner errors
		if err := scanner.Err(); err != nil {
			out <- cliproxyexecutor.StreamChunk{Err: fmt.Errorf("scanner error: %w", err)}
		}
	}()

	return &cliproxyexecutor.StreamResult{
		Headers: httpResp.Header.Clone(),
		Chunks:  out,
	}, nil
}

// messagesToPromptGeneric converts generic messages to Qoder prompt format

const qoderToolCallInstructions = "[TOOL CALL INSTRUCTIONS]\nWhen you need to use a tool, output EXACTLY this on its own line and stop:\n\nCalled tool: tool_name({\"arg\": \"value\"})\n\nRules — no exceptions:\n- ONLY use the format above. No JSON-only blocks. No ```bash blocks.\n- If a tool is needed, call it IMMEDIATELY — do not describe what you are about to do, just do it.\n- Do NOT say \"I'll run...\", \"Let me check...\", \"Running now\", \"On it\" — output the Called tool line and stop.\n- To run a shell command: Called tool: exec({\"command\":\"your command here\"})\n- Do NOT invent or fabricate tool results. No results until the system returns them.\n- After receiving a tool result, call another tool or write your final answer.\n- Do NOT offer to perform tasks that require tools you do not have access to.\n- If no tool is needed, respond normally."

const qoderBehaviorInstructions = "[BEHAVIOR INSTRUCTIONS]\nPlan before a multi-step task:\n- If completing the task will require more than 2 tool calls, state your plan in one sentence before the first call.\n- Then execute — do not re-explain the plan on each step.\n\nNarrate progress between calls:\n- After every 2-3 tool calls, emit one short status line so the user can follow along (e.g. \"Found the file, now checking contents...\").\n- Keep it to one line — then immediately make the next tool call.\n\nPersist until the task is done:\n- Do NOT give up after one failed attempt. Try at least 2-5 different approaches before concluding something is impossible.\n- If a command fails, read the error message and fix it — wrong flags, wrong path, wrong syntax. Adjust and retry.\n- Only report failure after genuinely exhausting options. Describe what you tried and what each attempt returned.\n\nVerify before you state:\n- Do NOT state facts about emails, files, data, or system state from memory. If you can check it with a tool, check it first.\n- If you are unsure whether something exists or is true, run a tool to find out before answering.\n- Be honest about things you failed to do or are not sure about — do not make claims not supported by what the tools returned.\n\nRead the help before using an unfamiliar command:\n- If you are unsure what flags or arguments a CLI tool accepts, run it with --help first.\n- Example: Called tool: exec({\"command\":\"gog gmail --help\"})\n- The help output will tell you exactly what to do. Use it — do not guess."

func messagesToPromptGeneric(messages []interface{}, tools interface{}) string {
	parts := make([]string, 0, len(messages)+2)
	if tools != nil {
		parts = append(parts, qoderToolCallInstructions)
		parts = append(parts, qoderBehaviorInstructions)
	}

	for _, msg := range messages {
		msgMap, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msgMap["role"].(string)
		content := extractContentGeneric(msgMap["content"])

		switch role {
		case "system":
			parts = append(parts, "[System Instructions]\n"+content)
		case "assistant":
			parts = append(parts, "[Previous Assistant Response]\n"+content)
		case "user":
			parts = append(parts, content)
		case "tool":
			name, _ := msgMap["name"].(string)
			if name == "" {
				name = "tool"
			}
			parts = append(parts, fmt.Sprintf("[Tool Result for %s]\n%s", name, content))
		}
	}

	return strings.Join(parts, "\n\n")
}

// extractContentGeneric extracts text content from message content field
func extractContentGeneric(content interface{}) string {
	switch v := content.(type) {
	case string:
		return v
	case []interface{}:
		var parts []string
		for _, item := range v {
			if itemMap, ok := item.(map[string]interface{}); ok {
				if itemMap["type"] == "text" {
					if text, ok := itemMap["text"].(string); ok {
						parts = append(parts, text)
					}
					continue
				}
				if text, ok := itemMap["text"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		return fmt.Sprintf("%v", content)
	}
}

func normalizeQoderMessages(messages []interface{}) []interface{} {
	if len(messages) == 0 {
		return nil
	}
	out := make([]interface{}, 0, len(messages))
	for _, msg := range messages {
		msgMap, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msgMap["role"].(string)
		switch role {
		case "tool":
			name, _ := msgMap["name"].(string)
			if name == "" {
				name = "tool"
			}
			content := extractContentGeneric(msgMap["content"])
			out = append(out, map[string]interface{}{
				"role":    "user",
				"content": fmt.Sprintf("[Tool Result for %s]\n%s", name, content),
			})
		case "assistant":
			if toolCalls, ok := msgMap["tool_calls"].([]interface{}); ok && len(toolCalls) > 0 {
				parts := make([]string, 0, len(toolCalls))
				for _, call := range toolCalls {
					callMap, ok := call.(map[string]interface{})
					if !ok {
						continue
					}
					fn, _ := callMap["function"].(map[string]interface{})
					name, _ := fn["name"].(string)
					args, _ := fn["arguments"].(string)
					if name == "" {
						name = "?"
					}
					if args == "" {
						args = "{}"
					}
					parts = append(parts, fmt.Sprintf("Called tool: %s(%s)", name, args))
				}
				content := extractContentGeneric(msgMap["content"])
				text := strings.Join(parts, "\n")
				if content != "" {
					text = content + "\n" + text
				}
				out = append(out, map[string]interface{}{
					"role":    "assistant",
					"content": text,
				})
				continue
			}
			out = append(out, msgMap)
		default:
			out = append(out, msgMap)
		}
	}
	return out
}

func hasToolHistory(messages []interface{}) bool {
	for _, msg := range messages {
		msgMap, ok := msg.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msgMap["role"].(string)
		if role == "tool" {
			return true
		}
		if role == "assistant" {
			if toolCalls, ok := msgMap["tool_calls"].([]interface{}); ok && len(toolCalls) > 0 {
				return true
			}
		}
	}
	return false
}

func buildOpenAIChunk(inner map[string]interface{}, model string) ([]byte, error) {
	if inner == nil {
		return nil, fmt.Errorf("empty inner payload")
	}
	if _, ok := inner["model"]; !ok || inner["model"] == "" {
		inner["model"] = model
	}
	if choices, ok := inner["choices"].([]interface{}); ok {
		if len(choices) == 0 {
			if inner["finish_reason"] != nil || inner["stop"] != nil {
				inner["choices"] = []map[string]interface{}{{
					"index":         0,
					"delta":         map[string]interface{}{},
					"finish_reason": "stop",
				}}
			}
		}
	}
	return json.Marshal(inner)
}

// emitDone publishes the terminating SSE frame(s) for the stream. The
// upstream "[DONE]" sentinel is fed through TranslateStream so the
// client's SourceFormat dictates the actual wire bytes — "data: [DONE]\n\n"
// for OpenAI, "event: message_stop\ndata: {...}\n\n" for Anthropic, and
// the equivalent format-specific terminators for Gemini etc. This mirrors
// the pattern used by kimi_executor.
func emitDone(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk,
	sourceFormat sdktranslator.Format, reqModel string, originalReq, body []byte) {
	to := sdktranslator.FormatOpenAI
	from := sourceFormat
	if from == "" {
		from = to
	}
	var param any
	frames := sdktranslator.TranslateStream(ctx, to, from,
		reqModel, originalReq, body, []byte("[DONE]"), &param)
	for _, frame := range frames {
		select {
		case out <- cliproxyexecutor.StreamChunk{Payload: frame}:
		case <-ctx.Done():
			return
		}
	}
}

// qoderStatusError implements StatusError for Qoder API errors
type qoderStatusError struct {
	status  int
	message string
}

func newQoderStatusError(status int, message string) *qoderStatusError {
	return &qoderStatusError{status: status, message: message}
}

func (e *qoderStatusError) Error() string {
	return fmt.Sprintf("Qoder API error %d: %s", e.status, e.message)
}

func (e *qoderStatusError) StatusCode() int {
	return e.status
}

// CountTokens estimates token count for the request (placeholder implementation)
func (e *QoderExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	// Translate non-openai formats before extracting messages
	payload := req.Payload
	if opts.SourceFormat != "" && opts.SourceFormat != sdktranslator.FormatOpenAI {
		payload = sdktranslator.TranslateRequest(opts.SourceFormat, sdktranslator.FormatOpenAI, req.Model, payload, false)
	}

	// Simple estimation: 1 token ≈ 4 characters
	var chatReq map[string]interface{}
	if err := json.Unmarshal(payload, &chatReq); err != nil {
		return cliproxyexecutor.Response{}, err
	}

	messagesRaw, _ := chatReq["messages"].([]interface{})
	totalChars := 0
	for _, msg := range messagesRaw {
		if msgMap, ok := msg.(map[string]interface{}); ok {
			content := extractContentGeneric(msgMap["content"])
			totalChars += len(content)
		}
	}

	estimatedTokens := totalChars / 4
	if estimatedTokens < 1 {
		estimatedTokens = 1
	}

	response := map[string]interface{}{
		"usage": map[string]int{
			"prompt_tokens":     estimatedTokens,
			"completion_tokens": 0,
			"total_tokens":      estimatedTokens,
		},
	}

	responseBytes, _ := json.Marshal(response)
	return cliproxyexecutor.Response{
		Payload: responseBytes,
	}, nil
}

// Execute executes a non-streaming request against Qoder API
func (e *QoderExecutor) Execute(ctx context.Context, authRecord *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	// Force ExecuteStream to emit raw OpenAI chunks (no cross-format
	// translation) so we can accumulate content from choices[0].delta.
	// We'll run TranslateNonStream ourselves at the end.
	internalOpts := opts
	internalOpts.SourceFormat = sdktranslator.FormatOpenAI
	streamResult, err := e.ExecuteStream(ctx, authRecord, req, internalOpts)
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	// Accumulate all chunks
	var content strings.Builder
	var finishReason string
	type pendingToolCall struct {
		ID        string
		Name      string
		Arguments string
	}
	pendingToolCalls := make(map[int]*pendingToolCall)

	for chunk := range streamResult.Chunks {
		if chunk.Err != nil {
			return cliproxyexecutor.Response{}, chunk.Err
		}

		// ExecuteStream was called with SourceFormat=FormatOpenAI so
		// TranslateStream strips the "data:" prefix and returns raw JSON.
		// Skip empty or [DONE] payloads.
		raw := chunk.Payload
		if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("[DONE]")) {
			continue
		}

		var oiChunk map[string]interface{}
		if err := json.Unmarshal(raw, &oiChunk); err == nil {
			if choices, ok := oiChunk["choices"].([]interface{}); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]interface{}); ok {
					if delta, ok := choice["delta"].(map[string]interface{}); ok {
						if toolCalls, ok := delta["tool_calls"].([]interface{}); ok {
							for _, call := range toolCalls {
								callMap, ok := call.(map[string]interface{})
								if !ok {
									continue
								}
								idx := 0
								if rawIdx, ok := callMap["index"].(float64); ok {
									idx = int(rawIdx)
								}
								entry := pendingToolCalls[idx]
								if entry == nil {
									entry = &pendingToolCall{}
									pendingToolCalls[idx] = entry
								}
								if id, ok := callMap["id"].(string); ok && id != "" {
									entry.ID = id
								}
								if fn, ok := callMap["function"].(map[string]interface{}); ok {
									if name, ok := fn["name"].(string); ok && name != "" {
										entry.Name = name
									}
									if args, ok := fn["arguments"].(string); ok && args != "" {
										entry.Arguments += args
									}
								}
							}
						}
						if contentStr, ok := delta["content"].(string); ok {
							content.WriteString(contentStr)
						}
					}
					if fr, ok := choice["finish_reason"].(string); ok && fr != "" {
						finishReason = fr
					}
				}
			}
		}
	}

	var toolCalls []map[string]interface{}
	if finishReason == "tool_calls" && len(pendingToolCalls) > 0 {
		for i := 0; i < len(pendingToolCalls); i++ {
			entry, ok := pendingToolCalls[i]
			if !ok || entry == nil {
				continue
			}
			id := entry.ID
			if id == "" {
				id = fmt.Sprintf("call_%d", time.Now().UnixNano())
			}
			args := entry.Arguments
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			toolCalls = append(toolCalls, map[string]interface{}{
				"id":   id,
				"type": "function",
				"function": map[string]interface{}{
					"name":      entry.Name,
					"arguments": args,
				},
			})
		}
	}

	// Build final response
	message := map[string]interface{}{
		"role":    "assistant",
		"content": content.String(),
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	response := map[string]interface{}{
		"id":      fmt.Sprintf("qoder-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   req.Model,
		"choices": []map[string]interface{}{
			{
				"index":         0,
				"message":       message,
				"finish_reason": finishReason,
			},
		},
	}

	responseBytes, _ := json.Marshal(response)

	// Translate the Qoder OpenAI-format response back to the client's expected
	// SourceFormat (mirrors the TranslateNonStream flow used by every other executor).
	var param any
	requestPayload := req.Payload
	if opts.SourceFormat != "" && opts.SourceFormat != sdktranslator.FormatOpenAI {
		requestPayload = sdktranslator.TranslateRequest(opts.SourceFormat, sdktranslator.FormatOpenAI, req.Model, req.Payload, false)
	}
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatOpenAI, opts.SourceFormat, req.Model, opts.OriginalRequest, requestPayload, responseBytes, &param)
	responseBytes = out

	return cliproxyexecutor.Response{
		Payload: responseBytes,
		Headers: streamResult.Headers,
	}, nil
}

// Refresh is a no-op for Qoder.
//
// Qoder's device-flow token (the "dt-..." string) is already long-lived
// (~30 days for the access token, ~360 days for the refresh token per
// the deviceToken/poll response). The upstream does not expose the
// classic OAuth refresh dance — every endpoint we've observed (cubk1's
// qoder2api, Veria, the official @qoder-ai/qodercli) either skips
// refresh entirely or routes through a different /jobToken exchange
// flow that requires personalToken (we don't have one).
//
// Hitting /algo/api/v3/user/refresh_token with our device token returns
// 403 "Forbidden" / errorCode=Forbidden — the endpoint is not for our
// flow. Mark the auth refreshed-now and keep going; if a real expiry
// happens the user re-runs --qoder-login.
func (e *QoderExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if auth == nil {
		return nil, fmt.Errorf("qoder executor: auth is nil")
	}
	return auth, nil
}

// HttpRequest injects Qoder COSY authentication into the HTTP request and executes it
func (e *QoderExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	storage, ok := auth.Storage.(*qoderauth.QoderTokenStorage)
	if !ok {
		return nil, fmt.Errorf("invalid auth storage type for qoder")
	}

	// Read request body for COSY signing
	bodyBytes, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	req.Body = io.NopCloser(bytes.NewReader(bodyBytes))

	// Build COSY auth headers
	headers, err := qoderauth.BuildAuthHeaders(
		bodyBytes,
		req.URL.String(),
		qoderauth.CosyCredentials{
			UserID:    storage.UserID,
			AuthToken: storage.Token,
			Name:      storage.Name,
			Email:     storage.Email,
			MachineID: storage.MachineID,
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to build COSY auth: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	headers.Apply(req)

	// Execute request
	req = req.WithContext(ctx)
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(req)
}

// buildQoderModelConfig returns the model_config block for a chat request,
// pulled from the cache populated by FetchQoderModels (which mirrors what
// /algo/api/v2/model/list publishes — per-model is_vl / is_reasoning /
// max_input_tokens / price_factor / strategies / ...). Returns an error
// when the cache has no entry for modelKey: that means we either never
// successfully fetched the model list for this auth, or the user asked
// for a model the server doesn't expose. Either way we should fail loudly
// rather than guess and silently get downgraded to a different model.
func buildQoderModelConfig(storage *qoderauth.QoderTokenStorage, modelKey string) (map[string]interface{}, error) {
	if storage == nil || len(storage.ModelConfigs) == 0 {
		return nil, fmt.Errorf("qoder: model config cache is empty (model list not fetched yet); restart the service or check /algo/api/v2/model/list connectivity")
	}
	raw, ok := storage.ModelConfigs[modelKey]
	if !ok || len(raw) == 0 {
		return nil, fmt.Errorf("qoder: no model_config cached for %q; known models: %s", modelKey, sortedKeys(storage.ModelConfigs))
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, fmt.Errorf("qoder: cached model_config for %q is invalid JSON: %w", modelKey, err)
	}
	if cfg == nil {
		return nil, fmt.Errorf("qoder: cached model_config for %q decoded to nil", modelKey)
	}
	// The cache stores the model description; ensure the key matches what
	// we're sending (handles model alias rewrites in caller).
	cfg["key"] = modelKey
	return cfg, nil
}

// sortedKeys returns the keys of m in stable order, suitable for error messages.
func sortedKeys(m map[string]json.RawMessage) string {
	if len(m) == 0 {
		return "<none>"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// FetchQoderModels retrieves the live model list from Qoder's
// /algo/api/v2/model/list endpoint and converts it into ModelInfo entries.
// Falls back to the static registry if the auth lacks credentials, the request
// fails, or the response is malformed. Mirrors the FetchKiloModels /
// FetchCursorModels pattern used by other dynamic providers.
func FetchQoderModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*registry.ModelInfo {
	storage, ok := auth.Storage.(*qoderauth.QoderTokenStorage)
	if !ok || storage == nil || storage.Token == "" {
		log.Debug("qoder: no token, returning static models")
		return registry.GetQoderModels()
	}

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	headers, err := qoderauth.BuildAuthHeaders(nil, qoderauth.QoderModelListURL, qoderauth.CosyCredentials{
		UserID:    storage.UserID,
		AuthToken: storage.Token,
		Name:      storage.Name,
		Email:     storage.Email,
		MachineID: storage.MachineID,
	})
	if err != nil {
		log.Warnf("qoder: build cosy headers for model list: %v", err)
		return registry.GetQoderModels()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, qoderauth.QoderModelListURL, nil)
	if err != nil {
		log.Warnf("qoder: build model list request: %v", err)
		return registry.GetQoderModels()
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	headers.Apply(req)

	httpClient := helps.NewProxyAwareHTTPClient(ctx, cfg, auth, 0)
	resp, err := httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			log.Warnf("qoder: model list fetch canceled: %v", err)
		} else {
			log.Warnf("qoder: model list fetch failed: %v", err)
		}
		return registry.GetQoderModels()
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Warnf("qoder: read model list response: %v", err)
		return registry.GetQoderModels()
	}
	if resp.StatusCode != http.StatusOK {
		log.Warnf("qoder: model list returned %d: %s", resp.StatusCode, truncate(string(body), 300))
		return registry.GetQoderModels()
	}

	chat := gjson.GetBytes(body, "chat")
	if !chat.Exists() || !chat.IsArray() {
		log.Warnf("qoder: model list response missing 'chat' array")
		return registry.GetQoderModels()
	}

	now := time.Now().Unix()
	models := make([]*registry.ModelInfo, 0, 16)
	configs := make(map[string]json.RawMessage, 16)
	chat.ForEach(func(_, entry gjson.Result) bool {
		key := entry.Get("key").String()
		if key == "" {
			return true
		}
		if !entry.Get("enable").Bool() {
			return true
		}
		display := entry.Get("display_name").String()
		if display == "" {
			display = key
		}
		ctxLen := int(entry.Get("max_input_tokens").Int())
		isReasoning := entry.Get("is_reasoning").Bool()
		isVL := entry.Get("is_vl").Bool()

		// Cache the raw upstream JSON for this model so ExecuteStream can
		// forward the exact model_config the server published (per-model
		// is_vl / is_reasoning / max_input_tokens / price_factor / ...).
		configs[key] = json.RawMessage(entry.Raw)

		mi := &registry.ModelInfo{
			ID:            key,
			Object:        "model",
			Created:       now,
			OwnedBy:       "qoder",
			Type:          "qoder",
			DisplayName:   display,
			Description:   fmt.Sprintf("%s via Qoder", display),
			ContextLength: ctxLen,
		}
		if isVL {
			mi.SupportedInputModalities = []string{"TEXT", "IMAGE"}
		}
		if isReasoning {
			mi.Thinking = &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}
		}
		models = append(models, mi)
		return true
	})

	if len(models) == 0 {
		log.Warn("qoder: model list returned no enabled models, falling back to static")
		return registry.GetQoderModels()
	}

	// Persist the cached configs onto the auth's storage so subsequent
	// ExecuteStream calls can read them. We don't write the file here —
	// the framework's persist hook will pick this up on the next save.
	if storage, ok := auth.Storage.(*qoderauth.QoderTokenStorage); ok && storage != nil {
		storage.ModelConfigs = configs
	}

	log.Infof("qoder: fetched %d models from /algo/api/v2/model/list", len(models))
	return models
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
