package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	cursorauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor"
	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/http2"
)

const (
	cursorAPIURL               = "https://api2.cursor.sh"
	cursorRunPath              = "/agent.v1.AgentService/Run"
	cursorModelsPath           = "/agent.v1.AgentService/GetUsableModels"
	defaultCursorClientVersion = "cli-2026.02.13-41ac335"
	cursorAuthType             = "cursor"
	cursorHeartbeatInterval    = 5 * time.Second
	cursorSessionTTL           = 30 * time.Minute
	cursorCheckpointTTL        = 30 * time.Minute
	cursorMaxImageBytes        = 20 << 20
)

// CursorExecutor handles requests to the Cursor API via Connect+Protobuf protocol.
type CursorExecutor struct {
	cfg         *config.Config
	mu          sync.Mutex
	sessions    map[string]*cursorSession
	checkpoints map[string]*savedCheckpoint // keyed by conversationId
}

// savedCheckpoint stores the server's conversation_checkpoint_update for reuse.
type savedCheckpoint struct {
	data               []byte            // raw ConversationStateStructure protobuf bytes
	blobStore          map[string][]byte // blobs referenced by the checkpoint
	authID             string            // auth that produced this checkpoint (checkpoint is auth-specific)
	executionSessionID string            // downstream execution session that owns this checkpoint, when known
	updatedAt          time.Time
}

type cursorSession struct {
	stream             *cursorproto.H2Stream
	blobStore          map[string][]byte
	mcpTools           []cursorproto.McpToolDef
	pending            []pendingMcpExec
	cancel             context.CancelFunc // cancels the session-scoped heartbeat (NOT tied to HTTP request)
	createdAt          time.Time
	authID             string // auth file ID that created this session (for multi-account isolation)
	conversationID     string
	executionSessionID string
	toolResultCh       chan []toolResultInfo                      // receives tool results from the next HTTP request
	resumeOutCh        chan cliproxyexecutor.StreamChunk          // output channel for resumed response
	switchOutput       func(ch chan cliproxyexecutor.StreamChunk) // callback to switch output channel
}

type pendingMcpExec struct {
	ExecMsgId  uint32
	ExecId     string
	ToolCallId string
	ToolName   string
	Args       string // JSON-encoded args
}

func cursorClientVersionHeader() string {
	if version := strings.TrimSpace(os.Getenv("CLIPROXY_CURSOR_CLIENT_VERSION")); version != "" {
		if strings.HasPrefix(version, "cli-") {
			return version
		}
		return "cli-" + version
	}
	return defaultCursorClientVersion
}

func cursorMatchingToolResults(results []toolResultInfo, pending []pendingMcpExec) []toolResultInfo {
	if len(results) == 0 || len(pending) == 0 {
		return nil
	}
	pendingIDs := make(map[string]struct{}, len(pending))
	for _, call := range pending {
		if id := strings.TrimSpace(call.ToolCallId); id != "" {
			pendingIDs[id] = struct{}{}
		}
	}
	if len(pendingIDs) == 0 {
		return nil
	}
	matched := make([]toolResultInfo, 0, len(pending))
	seen := make(map[string]struct{}, len(pending))
	for _, result := range results {
		if _, ok := pendingIDs[result.ToolCallId]; !ok {
			continue
		}
		if _, ok := seen[result.ToolCallId]; ok {
			continue
		}
		seen[result.ToolCallId] = struct{}{}
		matched = append(matched, result)
	}
	return matched
}

func cursorToolResultsMatchPending(results []toolResultInfo, pending []pendingMcpExec) bool {
	matched := cursorMatchingToolResults(results, pending)
	if len(matched) == 0 {
		return false
	}
	pendingIDs := make(map[string]struct{}, len(pending))
	for _, call := range pending {
		if id := strings.TrimSpace(call.ToolCallId); id != "" {
			pendingIDs[id] = struct{}{}
		}
	}
	return len(pendingIDs) > 0 && len(matched) == len(pendingIDs)
}

func cursorShouldEmitMcpExec(msg *cursorproto.DecodedServerMessage, mcpTools []cursorproto.McpToolDef) bool {
	if msg == nil || msg.Type != cursorproto.ServerMsgExecMcpArgs {
		return false
	}
	if !msg.InteractionToolCall {
		return true
	}
	toolName := strings.TrimSpace(msg.McpToolName)
	if toolName == "" {
		return false
	}
	declared := false
	for _, tool := range mcpTools {
		if cursorOpenAIToolNameForMcpTool(tool.Name) == toolName {
			declared = true
			break
		}
	}
	return declared && cursorInteractionToolCallHasRequiredArgs(toolName, msg.McpArgs)
}

func cursorInteractionToolCallHasRequiredArgs(toolName string, args map[string][]byte) bool {
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "grep":
		return cursorJSONArgString(args, "pattern") != ""
	case "read", "delete":
		return cursorJSONArgString(args, "path") != ""
	case "shell", "bash":
		return cursorJSONArgString(args, "command") != ""
	case "write":
		return cursorJSONArgString(args, "path") != "" && cursorJSONArgString(args, "fileText") != ""
	case "edit":
		return cursorJSONArgString(args, "path") != "" && (cursorJSONArgString(args, "patchContent") != "" || cursorJSONArgString(args, "oldText") != "" || cursorJSONArgString(args, "newText") != "" || cursorJSONArgString(args, "streamContent") != "")
	case "readlints":
		return len(cursorJSONArgStringSlice(args, "paths")) > 0
	case "mcp":
		return false
	default:
		return len(args) > 0
	}
}

func cursorJSONArgString(args map[string][]byte, key string) string {
	if len(args) == 0 {
		return ""
	}
	raw, ok := args[key]
	if !ok {
		return ""
	}
	var decoded string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return strings.TrimSpace(string(raw))
	}
	return strings.TrimSpace(decoded)
}

func cursorJSONArgStringSlice(args map[string][]byte, key string) []string {
	if len(args) == 0 {
		return nil
	}
	raw, ok := args[key]
	if !ok {
		return nil
	}
	var decoded []string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil
	}
	out := decoded[:0]
	for _, item := range decoded {
		if strings.TrimSpace(item) != "" {
			out = append(out, item)
		}
	}
	return out
}

func cursorCanResumeToolSession(session *cursorSession, authID string, results []toolResultInfo, streamDone bool) bool {
	if session == nil || streamDone {
		return false
	}
	if session.authID != authID {
		return false
	}
	if !cursorPendingExecsCanUseExecResult(session.pending) {
		return false
	}
	return cursorToolResultsMatchPending(results, session.pending)
}

func cursorPendingExecsCanUseExecResult(pending []pendingMcpExec) bool {
	if len(pending) == 0 {
		return false
	}
	for _, exec := range pending {
		if exec.ExecMsgId == 0 && strings.TrimSpace(exec.ExecId) == "" {
			return false
		}
	}
	return true
}

func cursorH2StreamDone(stream *cursorproto.H2Stream) bool {
	if stream == nil {
		return true
	}
	select {
	case <-stream.Done():
		return true
	default:
		return false
	}
}

func cursorStreamingTextDeltaJSON(text string) string {
	return fmt.Sprintf(`{"content":%s}`, jsonString(text))
}

func cursorStreamingToolCallDeltaJSON(index int, exec pendingMcpExec) string {
	return fmt.Sprintf(`{"tool_calls":[{"index":%d,"id":"%s","type":"function","function":{"name":"%s","arguments":%s}}]}`,
		index, exec.ToolCallId, exec.ToolName, jsonString(exec.Args))
}

func cursorStreamingToolCallDeltasJSON(execs []pendingMcpExec) []string {
	deltas := make([]string, 0, len(execs))
	for i, exec := range execs {
		deltas = append(deltas, cursorStreamingToolCallDeltaJSON(i, exec))
	}
	return deltas
}

func cursorStreamingThinkingDeltaJSON(text string) string {
	return fmt.Sprintf(`{"reasoning_content":%s}`, jsonString(text))
}

func cursorBuildNonStreamingTextCompletion(id string, created int64, model, content, reasoning string, inputTokens, outputTokens int64) []byte {
	if outputTokens == 0 {
		estimated := int64(len(content)+len(reasoning)) / 4
		if estimated == 0 && (content != "" || reasoning != "") {
			estimated = 1
		}
		outputTokens = estimated
	}
	type message struct {
		Role             string `json:"role"`
		Content          string `json:"content"`
		ReasoningContent string `json:"reasoning_content,omitempty"`
	}
	type choice struct {
		Index        int     `json:"index"`
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}
	usage := map[string]any{
		"prompt_tokens":     inputTokens,
		"completion_tokens": outputTokens,
		"total_tokens":      inputTokens + outputTokens,
	}
	if reasoning != "" {
		reasoningTokens := int64(len(reasoning)) / 4
		if reasoningTokens == 0 {
			reasoningTokens = 1
		}
		usage["completion_tokens_details"] = map[string]int64{"reasoning_tokens": reasoningTokens}
	}
	resp := struct {
		ID      string         `json:"id"`
		Object  string         `json:"object"`
		Created int64          `json:"created"`
		Model   string         `json:"model"`
		Choices []choice       `json:"choices"`
		Usage   map[string]any `json:"usage"`
	}{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []choice{{
			Index: 0,
			Message: message{
				Role:             "assistant",
				Content:          content,
				ReasoningContent: reasoning,
			},
			FinishReason: "stop",
		}},
		Usage: usage,
	}
	out, _ := json.Marshal(resp)
	return out
}

func cursorBuildNonStreamingToolCallCompletion(id string, created int64, model string, pending []pendingMcpExec) []byte {
	type toolFunction struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}
	type toolCall struct {
		ID       string       `json:"id"`
		Type     string       `json:"type"`
		Function toolFunction `json:"function"`
	}
	type message struct {
		Role      string     `json:"role"`
		Content   *string    `json:"content"`
		ToolCalls []toolCall `json:"tool_calls"`
	}
	type choice struct {
		Index        int     `json:"index"`
		Message      message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	}
	resp := struct {
		ID      string         `json:"id"`
		Object  string         `json:"object"`
		Created int64          `json:"created"`
		Model   string         `json:"model"`
		Choices []choice       `json:"choices"`
		Usage   map[string]int `json:"usage"`
	}{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Usage: map[string]int{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		},
	}
	calls := make([]toolCall, 0, len(pending))
	for _, call := range pending {
		arguments := strings.TrimSpace(call.Args)
		if arguments == "" {
			arguments = "{}"
		}
		calls = append(calls, toolCall{
			ID:   call.ToolCallId,
			Type: "function",
			Function: toolFunction{
				Name:      call.ToolName,
				Arguments: arguments,
			},
		})
	}
	resp.Choices = []choice{{
		Index: 0,
		Message: message{
			Role:      "assistant",
			Content:   nil,
			ToolCalls: calls,
		},
		FinishReason: "tool_calls",
	}}
	out, _ := json.Marshal(resp)
	return out
}

// NewCursorExecutor constructs a new executor instance.
func NewCursorExecutor(cfg *config.Config) *CursorExecutor {
	e := &CursorExecutor{
		cfg:         cfg,
		sessions:    make(map[string]*cursorSession),
		checkpoints: make(map[string]*savedCheckpoint),
	}
	go e.cleanupLoop()
	return e
}

// Identifier implements ProviderExecutor.
func (e *CursorExecutor) Identifier() string { return cursorAuthType }

// CloseExecutionSession implements ExecutionSessionCloser.
func (e *CursorExecutor) CloseExecutionSession(sessionID string) {
	if e == nil {
		return
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}

	var sessionsToClose []*cursorSession
	e.mu.Lock()
	if sessionID == cliproxyauth.CloseAllExecutionSessionsID {
		for k, s := range e.sessions {
			sessionsToClose = append(sessionsToClose, s)
			delete(e.sessions, k)
		}
		for k := range e.checkpoints {
			delete(e.checkpoints, k)
		}
	} else {
		conversationIDFromKey := cursorConversationIDFromSessionKey(sessionID)
		for k, s := range e.sessions {
			if s == nil {
				continue
			}
			if k == sessionID || s.conversationID == sessionID || s.executionSessionID == sessionID {
				sessionsToClose = append(sessionsToClose, s)
				if s.conversationID != "" {
					delete(e.checkpoints, s.conversationID)
				}
				delete(e.sessions, k)
			}
		}
		for k, cp := range e.checkpoints {
			if k == sessionID || k == conversationIDFromKey || (cp != nil && cp.executionSessionID == sessionID) {
				delete(e.checkpoints, k)
			}
		}
	}
	e.mu.Unlock()

	closeCursorSessions(sessionsToClose)
}

func cursorConversationIDFromSessionKey(sessionID string) string {
	idx := strings.LastIndexByte(sessionID, ':')
	if idx < 0 || idx == len(sessionID)-1 {
		return ""
	}
	return sessionID[idx+1:]
}

func closeCursorSessions(sessions []*cursorSession) {
	for _, s := range sessions {
		if s == nil {
			continue
		}
		if s.cancel != nil {
			s.cancel()
		}
		if s.stream != nil {
			s.stream.Close()
		}
		closeCursorStreamChunkChannel(s.resumeOutCh)
	}
}

func closeCursorStreamChunkChannel(ch chan cliproxyexecutor.StreamChunk) {
	if ch == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	close(ch)
}

func sendCursorStreamChunk(ch chan cliproxyexecutor.StreamChunk, chunk cliproxyexecutor.StreamChunk) {
	if ch == nil {
		return
	}
	defer func() {
		_ = recover()
	}()
	ch <- chunk
}

func (e *CursorExecutor) removeSessionIfCurrent(sessionKey string, session *cursorSession) bool {
	if e == nil || session == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sessions == nil || e.sessions[sessionKey] != session {
		return false
	}
	delete(e.sessions, sessionKey)
	return true
}

func (e *CursorExecutor) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		var sessionsToClose []*cursorSession
		e.mu.Lock()
		for k, s := range e.sessions {
			if s != nil && time.Since(s.createdAt) > cursorSessionTTL {
				sessionsToClose = append(sessionsToClose, s)
				delete(e.sessions, k)
			}
		}
		for k, cp := range e.checkpoints {
			if cp != nil && time.Since(cp.updatedAt) > cursorCheckpointTTL {
				delete(e.checkpoints, k)
			}
		}
		e.mu.Unlock()
		closeCursorSessions(sessionsToClose)
	}
}

// findSessionByConversationLocked searches for a session matching the given
// conversationId regardless of authID. Used to find and clean up stale sessions
// from a previous auth after quota failover. Caller must hold e.mu.
func (e *CursorExecutor) findSessionByConversationLocked(convId string) string {
	suffix := ":" + convId
	for k := range e.sessions {
		if strings.HasSuffix(k, suffix) {
			return k
		}
	}
	return ""
}

// cursorStatusErr implements the StatusError and RetryAfter interfaces so the
// conductor can classify Cursor errors (e.g. 429 → quota cooldown).
type cursorStatusErr struct {
	code int
	msg  string
}

func (e cursorStatusErr) Error() string              { return e.msg }
func (e cursorStatusErr) StatusCode() int            { return e.code }
func (e cursorStatusErr) RetryAfter() *time.Duration { return nil } // no retry-after info from Cursor; conductor uses exponential backoff

// classifyCursorError maps Cursor Connect/H2 errors to HTTP status codes.
// Layer 1: precise match on ConnectError.Code (gRPC standard codes).
// Layer 2: fuzzy string match for H2 frame errors and unknown formats.
// Unclassified errors pass through unchanged.
func classifyCursorError(err error) error {
	if err == nil {
		return nil
	}

	// Layer 1: structured ConnectError from ParseConnectEndStream
	var ce *cursorproto.ConnectError
	if errors.As(err, &ce) {
		log.Infof("cursor: Connect error code=%q message=%q", ce.Code, ce.Message)
		switch ce.Code {
		case "resource_exhausted":
			return cursorStatusErr{code: 429, msg: err.Error()}
		case "unauthenticated":
			return cursorStatusErr{code: 401, msg: err.Error()}
		case "permission_denied":
			return cursorStatusErr{code: 403, msg: err.Error()}
		case "unavailable":
			return cursorStatusErr{code: 503, msg: err.Error()}
		case "internal":
			return cursorStatusErr{code: 500, msg: err.Error()}
		default:
			// Unknown Connect code — log for observation, treat as 502
			return cursorStatusErr{code: 502, msg: err.Error()}
		}
	}

	// Layer 2: fuzzy match for H2 errors and unstructured messages
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "rate limit") || strings.Contains(msg, "quota") ||
		strings.Contains(msg, "too many"):
		return cursorStatusErr{code: 429, msg: err.Error()}
	case strings.Contains(msg, "rst_stream") || strings.Contains(msg, "goaway"):
		return cursorStatusErr{code: 502, msg: err.Error()}
	}

	return err
}

// PrepareRequest implements ProviderExecutor (for HttpRequest support).
func (e *CursorExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	token := cursorAccessToken(auth)
	if token == "" {
		return fmt.Errorf("cursor: access token not found")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// HttpRequest injects credentials and executes the request.
func (e *CursorExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("cursor: request is nil")
	}
	if err := e.PrepareRequest(req, auth); err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

// CountTokens estimates token count locally using tiktoken.
func (e *CursorExecutor) CountTokens(_ context.Context, _ *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	defer func() {
		if err != nil {
			log.Warnf("cursor CountTokens error: %v", err)
		} else {
			log.Debugf("cursor CountTokens: model=%s result=%s", req.Model, string(resp.Payload))
		}
	}()
	model := gjson.GetBytes(req.Payload, "model").String()
	if model == "" {
		model = req.Model
	}

	enc, err := getTokenizer(model)
	if err != nil {
		// Fallback: return zero tokens rather than error (avoids 502)
		return cliproxyexecutor.Response{Payload: buildOpenAIUsageJSON(0)}, nil
	}

	// Detect format: Claude (/v1/messages) vs OpenAI (/v1/chat/completions)
	var count int64
	if gjson.GetBytes(req.Payload, "system").Exists() || opts.SourceFormat.String() == "claude" {
		count, _ = countClaudeChatTokens(enc, req.Payload)
	} else {
		count, _ = countOpenAIChatTokens(enc, req.Payload)
	}

	return cliproxyexecutor.Response{Payload: buildOpenAIUsageJSON(count)}, nil
}

// Refresh attempts to refresh the Cursor access token.
func (e *CursorExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	refreshToken := cursorRefreshToken(auth)
	if refreshToken == "" {
		return nil, fmt.Errorf("cursor: no refresh token available")
	}

	tokens, err := cursorauth.RefreshToken(ctx, refreshToken)
	if err != nil {
		return nil, err
	}

	expiresAt := cursorauth.GetTokenExpiry(tokens.AccessToken)

	newAuth := auth.Clone()
	newAuth.Metadata["access_token"] = tokens.AccessToken
	newAuth.Metadata["refresh_token"] = tokens.RefreshToken
	newAuth.Metadata["expires_at"] = expiresAt.Format(time.RFC3339)
	return newAuth, nil
}

// Execute handles non-streaming requests.
func (e *CursorExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	log.Debugf("cursor Execute: model=%s sourceFormat=%s payloadLen=%d", req.Model, opts.SourceFormat, len(req.Payload))
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("cursor Execute PANIC: %v", r)
			err = fmt.Errorf("cursor: internal panic: %v", r)
		}
		if err != nil {
			log.Warnf("cursor Execute error: %v", err)
		}
	}()
	accessToken := cursorAccessToken(auth)
	if accessToken == "" {
		return resp, fmt.Errorf("cursor: access token not found")
	}

	// Translate input to OpenAI format if needed (e.g. Claude /v1/messages format)
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	payload := req.Payload
	if from.String() != "" && from.String() != "openai" {
		payload = sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(payload), false)
	}
	payload = cursorNormalizeExecutionModelInOpenAIPayload(payload, req.Model)

	parsed := parseOpenAIRequest(payload)
	if err := e.resolveCursorRemoteImages(ctx, auth, parsed); err != nil {
		return resp, err
	}
	flattenConversationIntoUserText(parsed)
	conversation := resolveCursorConversation(apiKeyFromContext(ctx), parsed.SystemPrompt, req, opts, payload)
	conversationId := conversation.ConversationID
	params := buildRunRequestParams(parsed, conversationId)

	requestBytes := cursorproto.EncodeRunRequest(params)
	framedRequest := cursorproto.FrameConnectMessage(requestBytes, 0)

	stream, err := openCursorH2Stream(accessToken)
	if err != nil {
		return resp, err
	}
	defer stream.Close()

	// Send the request frame
	if err := stream.Write(framedRequest); err != nil {
		return resp, fmt.Errorf("cursor: failed to send request: %w", err)
	}

	// Start heartbeat
	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()
	go cursorH2Heartbeat(sessionCtx, stream)

	// Collect full text from streaming response, or capture tool calls if the
	// model pauses for MCP execution.
	var fullText strings.Builder
	var thinkingText strings.Builder
	var pendingToolCalls []pendingMcpExec
	usage := &cursorTokenUsage{}
	usage.setInputEstimate(len(payload))
	if streamErr := processH2SessionFrames(sessionCtx, stream, params.BlobStore, params.McpTools,
		func(text string, isThinking bool) {
			if isThinking {
				thinkingText.WriteString(text)
			} else {
				fullText.WriteString(text)
			}
		},
		func(execs []pendingMcpExec) {
			pendingToolCalls = append(pendingToolCalls, execs...)
		},
		nil,
		usage,
		nil, // onCheckpoint - non-streaming doesn't persist
	); streamErr != nil && fullText.Len() == 0 && len(pendingToolCalls) == 0 {
		return resp, classifyCursorError(fmt.Errorf("cursor: stream error: %w", streamErr))
	}

	id := "chatcmpl-" + uuid.New().String()[:28]
	created := time.Now().Unix()
	var openaiResp []byte
	if len(pendingToolCalls) > 0 {
		openaiResp = cursorBuildNonStreamingToolCallCompletion(id, created, parsed.Model, pendingToolCalls)
	} else {
		inputTokens, outputTokens := usage.get()
		openaiResp = cursorBuildNonStreamingTextCompletion(id, created, parsed.Model, fullText.String(), thinkingText.String(), inputTokens, outputTokens)
	}

	// Translate response back to source format if needed
	result := openaiResp
	if from.String() != "" && from.String() != "openai" {
		var param any
		result = sdktranslator.TranslateNonStream(ctx, to, from, req.Model, bytes.Clone(opts.OriginalRequest), payload, result, &param)
	}
	resp.Payload = result
	return resp, nil
}

// ExecuteStream handles streaming requests.
// It supports MCP tool call sessions: when Cursor returns an MCP tool call,
// the H2 stream is kept alive. When Claude Code returns the tool result in
// the next request, the result is sent back on the same stream (session resume).
// This mirrors the activeSessions/resumeWithToolResults pattern in cursor-fetch.ts.
func (e *CursorExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	log.Debugf("cursor ExecuteStream: model=%s sourceFormat=%s payloadLen=%d", req.Model, opts.SourceFormat, len(req.Payload))
	defer func() {
		if r := recover(); r != nil {
			log.Errorf("cursor ExecuteStream PANIC: %v", r)
			err = fmt.Errorf("cursor: internal panic: %v", r)
		}
		if err != nil {
			log.Warnf("cursor ExecuteStream error: %v", err)
		}
	}()
	accessToken := cursorAccessToken(auth)
	if accessToken == "" {
		return nil, fmt.Errorf("cursor: access token not found")
	}

	// Translate input to OpenAI format if needed
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	payload := req.Payload
	originalPayload := bytes.Clone(req.Payload)
	if len(opts.OriginalRequest) > 0 {
		originalPayload = bytes.Clone(opts.OriginalRequest)
	}
	if from.String() != "" && from.String() != "openai" {
		log.Debugf("cursor: translating request from %s to openai", from)
		payload = sdktranslator.TranslateRequest(from, to, req.Model, bytes.Clone(payload), true)
		log.Debugf("cursor: translated payload len=%d", len(payload))
	}
	payload = cursorNormalizeExecutionModelInOpenAIPayload(payload, req.Model)

	parsed := parseOpenAIRequest(payload)
	if err := e.resolveCursorRemoteImages(ctx, auth, parsed); err != nil {
		return nil, err
	}
	log.Debugf("cursor: parsed request: model=%s userText=%d chars, turns=%d, tools=%d, toolResults=%d",
		parsed.Model, len(parsed.UserText), len(parsed.Turns), len(parsed.Tools), len(parsed.ToolResults))

	conversation := resolveCursorConversation(apiKeyFromContext(ctx), parsed.SystemPrompt, req, opts, originalPayload, payload)
	conversationId := conversation.ConversationID
	authID := auth.ID // e.g. "cursor.json" or "cursor-account2.json"
	log.Debugf("cursor: conversationId=%s authID=%s sessionSource=%s", conversationId, authID, conversation.SessionSource)

	// Session key includes authID (H2 stream is auth-specific, not transferable).
	// Checkpoint key uses conversationId only — allows detecting auth migration.
	sessionKey := authID + ":" + conversationId
	checkpointKey := conversationId
	needsTranslate := from.String() != "" && from.String() != "openai"

	forceFlattenToolFallback := false

	// Check if we can resume an existing session with tool results
	if len(parsed.ToolResults) > 0 {
		forceFlattenToolFallback = true
		var staleSessionsToClose []*cursorSession
		e.mu.Lock()
		session, hasSession := e.sessions[sessionKey]
		if hasSession {
			delete(e.sessions, sessionKey)
		}
		// If no session found for current auth, check for stale sessions from
		// a different auth on the same conversation (quota failover scenario).
		// Clean them up since the H2 stream belongs to the old account.
		if !hasSession {
			if oldKey := e.findSessionByConversationLocked(conversationId); oldKey != "" {
				oldSession := e.sessions[oldKey]
				if oldSession != nil {
					log.Infof("cursor: cleaning up stale session from auth %s for conv=%s (auth migrated to %s)", oldSession.authID, conversationId, authID)
					staleSessionsToClose = append(staleSessionsToClose, oldSession)
				}
				delete(e.sessions, oldKey)
			}
		}
		e.mu.Unlock()
		closeCursorSessions(staleSessionsToClose)

		if hasSession && session != nil {
			streamDone := cursorH2StreamDone(session.stream)
			if !cursorCanResumeToolSession(session, authID, parsed.ToolResults, streamDone) {
				log.Warnf("cursor: session %s is not resumable (auth=%s streamDone=%t pending=%d results=%d); falling back to cold resume",
					sessionKey, session.authID, streamDone, len(session.pending), len(parsed.ToolResults))
				closeCursorSessions([]*cursorSession{session})
			} else {
				log.Debugf("cursor: resuming session %s with %d tool results", sessionKey, len(parsed.ToolResults))
				result, resumeErr := e.resumeWithToolResults(ctx, session, parsed, from, to, req, originalPayload, payload, needsTranslate)
				if resumeErr == nil {
					return result, nil
				}
				log.Warnf("cursor: failed to resume session %s: %v; falling back to cold resume", sessionKey, resumeErr)
				closeCursorSessions([]*cursorSession{session})
			}
		}
	}

	// Clean up any stale session for this key (or from a previous auth on same conversation)
	var oldSessionsToClose []*cursorSession
	e.mu.Lock()
	if old, ok := e.sessions[sessionKey]; ok {
		oldSessionsToClose = append(oldSessionsToClose, old)
		delete(e.sessions, sessionKey)
	} else if oldKey := e.findSessionByConversationLocked(conversationId); oldKey != "" {
		oldSessionsToClose = append(oldSessionsToClose, e.sessions[oldKey])
		delete(e.sessions, oldKey)
	}
	e.mu.Unlock()
	closeCursorSessions(oldSessionsToClose)

	// Look up saved checkpoint for this conversation (keyed by conversationId only).
	// Checkpoint is auth-specific: if auth changed (e.g. quota exhaustion failover),
	// the old checkpoint is useless on the new account — discard and flatten.
	e.mu.Lock()
	saved, hasCheckpoint := e.checkpoints[checkpointKey]
	e.mu.Unlock()

	var params *cursorproto.RunRequestParams
	if hasCheckpoint && saved.data != nil && saved.authID == authID && !forceFlattenToolFallback {
		// Same auth — use checkpoint normally. The server already has prior
		// conversation context, so keep only the current user message structured.
		params = buildRunRequestParams(parsed, conversationId)
		log.Debugf("cursor: using saved checkpoint (%d bytes) for conv=%s auth=%s", len(saved.data), checkpointKey, authID)
		params.RawCheckpoint = saved.data
		// Merge saved blobStore into params
		if params.BlobStore == nil {
			params.BlobStore = make(map[string][]byte)
		}
		for k, v := range saved.blobStore {
			if _, exists := params.BlobStore[k]; !exists {
				params.BlobStore[k] = v
			}
		}
	} else {
		if forceFlattenToolFallback {
			log.Debugf("cursor: live tool-result resume unavailable, flattening tool-result history into userText")
		} else if hasCheckpoint && saved.data != nil && saved.authID != authID {
			// Auth changed (quota failover) — checkpoint is not portable across accounts.
			// Discard and flatten conversation history into userText.
			log.Infof("cursor: auth migrated (%s → %s) for conv=%s, discarding checkpoint and flattening context", saved.authID, authID, checkpointKey)
			e.mu.Lock()
			delete(e.checkpoints, checkpointKey)
			e.mu.Unlock()
		} else {
			log.Debugf("cursor: no checkpoint, flattening full message history into userText")
		}
		flattenConversationIntoUserText(parsed)
		if forceFlattenToolFallback {
			parsed.UserText = cursorAppendToolFallbackInstruction(parsed.UserText)
		}
		params = buildRunRequestParams(parsed, conversationId)
	}
	requestBytes := cursorproto.EncodeRunRequest(params)
	framedRequest := cursorproto.FrameConnectMessage(requestBytes, 0)

	stream, err := openCursorH2Stream(accessToken)
	if err != nil {
		return nil, err
	}

	if err := stream.Write(framedRequest); err != nil {
		stream.Close()
		return nil, fmt.Errorf("cursor: failed to send request: %w", err)
	}

	// Use a session-scoped context for the heartbeat that is NOT tied to the HTTP request.
	// This ensures the heartbeat survives across request boundaries during MCP tool execution.
	// Mirrors the TS plugin's setInterval-based heartbeat that lives independently of HTTP responses.
	sessionCtx, sessionCancel := context.WithCancel(context.Background())
	go cursorH2Heartbeat(sessionCtx, stream)

	chunks := make(chan cliproxyexecutor.StreamChunk, 64)
	chatId := "chatcmpl-" + uuid.New().String()[:28]
	created := time.Now().Unix()

	var streamParam any

	// Tool result channel for inline mode. processH2SessionFrames blocks on it
	// when mcpArgs is received, while continuing to handle KV/heartbeat.
	toolResultCh := make(chan []toolResultInfo, 1)

	// Switchable output: initially writes to `chunks`. After mcpArgs, the
	// onMcpExec callback closes `chunks` (ending the first HTTP response),
	// then processH2SessionFrames blocks on toolResultCh. When results arrive,
	// it switches to `resumeOutCh` (created by resumeWithToolResults).
	var outMu sync.Mutex
	currentOut := chunks

	emitToOut := func(chunk cliproxyexecutor.StreamChunk) {
		outMu.Lock()
		out := currentOut
		outMu.Unlock()
		sendCursorStreamChunk(out, chunk)
	}

	// Wrap sendChunk/sendDone to use emitToOut
	sendChunkSwitchable := func(delta string, finishReason string) {
		fr := "null"
		if finishReason != "" {
			fr = finishReason
		}
		openaiJSON := fmt.Sprintf(`{"id":"%s","object":"chat.completion.chunk","created":%d,"model":"%s","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`,
			chatId, created, parsed.Model, delta, fr)
		sseLine := []byte("data: " + openaiJSON + "\n")

		if needsTranslate {
			translated := sdktranslator.TranslateStream(ctx, to, from, req.Model, originalPayload, payload, sseLine, &streamParam)
			for _, t := range translated {
				emitToOut(cliproxyexecutor.StreamChunk{Payload: bytes.Clone(t)})
			}
		} else {
			emitToOut(cliproxyexecutor.StreamChunk{Payload: []byte(openaiJSON)})
		}
	}

	sendDoneSwitchable := func() {
		if needsTranslate {
			done := sdktranslator.TranslateStream(ctx, to, from, req.Model, originalPayload, payload, []byte("data: [DONE]\n"), &streamParam)
			for _, d := range done {
				emitToOut(cliproxyexecutor.StreamChunk{Payload: bytes.Clone(d)})
			}
		} else {
			emitToOut(cliproxyexecutor.StreamChunk{Payload: []byte("[DONE]")})
		}
	}

	// Pre-response error detection for transparent failover:
	// If the stream fails before any chunk is emitted (e.g. quota exceeded),
	// ExecuteStream returns an error so the conductor retries with a different auth.
	streamErrCh := make(chan error, 1)
	firstChunkSent := make(chan struct{}, 1) // buffered: goroutine won't block signaling

	origEmitToOut := emitToOut
	emitToOut = func(chunk cliproxyexecutor.StreamChunk) {
		select {
		case firstChunkSent <- struct{}{}:
		default:
		}
		origEmitToOut(chunk)
	}

	go func() {
		var storedToolSession *cursorSession
		defer func() {
			if storedToolSession != nil && e.removeSessionIfCurrent(sessionKey, storedToolSession) {
				closeCursorStreamChunkChannel(storedToolSession.resumeOutCh)
			}
			outMu.Lock()
			out := currentOut
			currentOut = nil
			outMu.Unlock()
			closeCursorStreamChunkChannel(out)
			sessionCancel()
			stream.Close()
		}()

		toolCallIndex := 0
		usage := &cursorTokenUsage{}
		usage.setInputEstimate(len(payload))

		streamErr := processH2SessionFrames(sessionCtx, stream, params.BlobStore, params.McpTools,
			func(text string, isThinking bool) {
				if isThinking {
					sendChunkSwitchable(cursorStreamingThinkingDeltaJSON(text), "")
				} else {
					sendChunkSwitchable(cursorStreamingTextDeltaJSON(text), "")
				}
			},
			func(execs []pendingMcpExec) {
				if len(execs) == 0 {
					return
				}
				for _, exec := range execs {
					toolCallJSON := cursorStreamingToolCallDeltaJSON(toolCallIndex, exec)
					toolCallIndex++
					sendChunkSwitchable(toolCallJSON, "")
				}
				sendChunkSwitchable(`{}`, `"tool_calls"`)
				sendDoneSwitchable()

				// Close current output to end the current HTTP SSE response
				outMu.Lock()
				if currentOut != nil {
					closeCursorStreamChunkChannel(currentOut)
					currentOut = nil
				}
				outMu.Unlock()

				// Create new resume output channel, reuse the same toolResultCh
				resumeOut := make(chan cliproxyexecutor.StreamChunk, 64)
				log.Debugf("cursor: saving session %s for MCP tool resume (tools=%d first=%s)", sessionKey, len(execs), execs[0].ToolName)
				session := &cursorSession{
					stream:             stream,
					blobStore:          params.BlobStore,
					mcpTools:           params.McpTools,
					pending:            append([]pendingMcpExec(nil), execs...),
					cancel:             sessionCancel,
					createdAt:          time.Now(),
					authID:             authID,
					conversationID:     conversationId,
					executionSessionID: conversation.ExecutionSessionID,
					toolResultCh:       toolResultCh, // reuse same channel across rounds
					resumeOutCh:        resumeOut,
					switchOutput: func(ch chan cliproxyexecutor.StreamChunk) {
						outMu.Lock()
						currentOut = ch
						// Reset translator state so the new HTTP response gets
						// a fresh message_start, content_block_start, etc.
						streamParam = nil
						// New response needs its own message ID and fresh per-message
						// tool-call indexes. OpenAI streaming indexes are scoped to
						// one assistant response, not the whole Cursor H2 session.
						chatId = "chatcmpl-" + uuid.New().String()[:28]
						created = time.Now().Unix()
						toolCallIndex = 0
						outMu.Unlock()
					},
				}
				e.mu.Lock()
				e.sessions[sessionKey] = session
				e.mu.Unlock()
				storedToolSession = session

				// processH2SessionFrames will now block on toolResultCh (inline wait loop)
				// while continuing to handle KV messages
			},
			toolResultCh,
			usage,
			func(cpData []byte) {
				// Save checkpoint keyed by conversationId, tagged with authID for migration detection
				e.mu.Lock()
				e.checkpoints[checkpointKey] = &savedCheckpoint{
					data:               cpData,
					blobStore:          params.BlobStore,
					authID:             authID,
					executionSessionID: conversation.ExecutionSessionID,
					updatedAt:          time.Now(),
				}
				e.mu.Unlock()
				log.Debugf("cursor: saved checkpoint (%d bytes) for conv=%s auth=%s", len(cpData), checkpointKey, authID)
			},
		)

		// processH2SessionFrames returned — stream is done.
		// Check if error happened before any chunks were emitted.
		if streamErr != nil {
			select {
			case <-firstChunkSent:
				// Chunks were already sent to client — can't transparently retry.
				// Next request will failover via conductor's cooldown mechanism.
				log.Warnf("cursor: stream error after data sent (auth=%s conv=%s): %v", authID, conversationId, streamErr)
			default:
				// No data sent yet — propagate error for transparent conductor retry.
				log.Warnf("cursor: stream error before data sent (auth=%s conv=%s): %v — signaling retry", authID, conversationId, streamErr)
				streamErrCh <- streamErr
				outMu.Lock()
				if currentOut != nil {
					closeCursorStreamChunkChannel(currentOut)
					currentOut = nil
				}
				outMu.Unlock()
				return
			}
		}

		// Include token usage in the final stop chunk.
		inputTok, outputTok := usage.get()
		// Build the stop chunk with usage embedded in the choices array level.
		fr := `"stop"`
		openaiJSON := fmt.Sprintf(`{"id":"%s","object":"chat.completion.chunk","created":%d,"model":"%s","choices":[{"index":0,"delta":{},"finish_reason":%s}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
			chatId, created, parsed.Model, fr, inputTok, outputTok, inputTok+outputTok)
		sseLine := []byte("data: " + openaiJSON + "\n")
		if needsTranslate {
			translated := sdktranslator.TranslateStream(ctx, to, from, req.Model, originalPayload, payload, sseLine, &streamParam)
			for _, t := range translated {
				emitToOut(cliproxyexecutor.StreamChunk{Payload: bytes.Clone(t)})
			}
		} else {
			emitToOut(cliproxyexecutor.StreamChunk{Payload: []byte(openaiJSON)})
		}
		sendDoneSwitchable()

	}()

	// Wait for either the first chunk or a pre-response error.
	// If the stream fails before emitting any data (e.g. quota exceeded),
	// return an error so the conductor retries with a different auth.
	select {
	case streamErr := <-streamErrCh:
		return nil, classifyCursorError(fmt.Errorf("cursor: stream failed before response: %w", streamErr))
	case <-firstChunkSent:
		// Data started flowing — return stream to client
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}
}

// resumeWithToolResults injects tool results into the running processH2SessionFrames
// via the toolResultCh channel. The original goroutine from ExecuteStream is still alive,
// blocking on toolResultCh. Once we send the results, it sends the MCP result to Cursor
// and continues processing the response text — all in the same goroutine that has been
// handling KV messages the whole time.
func (e *CursorExecutor) resumeWithToolResults(
	ctx context.Context,
	session *cursorSession,
	parsed *parsedOpenAIRequest,
	from, to sdktranslator.Format,
	req cliproxyexecutor.Request,
	originalPayload, payload []byte,
	needsTranslate bool,
) (*cliproxyexecutor.StreamResult, error) {
	if session == nil {
		return nil, fmt.Errorf("cursor: missing session")
	}

	matchedToolResults := cursorMatchingToolResults(parsed.ToolResults, session.pending)
	log.Debugf("cursor: resumeWithToolResults: injecting %d matching tool results via channel (from %d total)", len(matchedToolResults), len(parsed.ToolResults))

	if len(matchedToolResults) == 0 {
		return nil, fmt.Errorf("cursor: no tool result matches pending calls")
	}

	if session.toolResultCh == nil {
		return nil, fmt.Errorf("cursor: session has no toolResultCh (stale session?)")
	}
	if session.resumeOutCh == nil {
		return nil, fmt.Errorf("cursor: session has no resumeOutCh")
	}
	if cursorH2StreamDone(session.stream) {
		return nil, fmt.Errorf("cursor: session stream is no longer active")
	}

	log.Debugf("cursor: resumeWithToolResults: switching output to resumeOutCh and injecting results")

	// Switch the output channel BEFORE injecting results, so that when
	// processH2SessionFrames unblocks and starts emitting text, it writes
	// to the resumeOutCh which the new HTTP handler is reading from.
	if session.switchOutput != nil {
		session.switchOutput(session.resumeOutCh)
	}

	// Inject tool results — this unblocks the waiting processH2SessionFrames.
	select {
	case session.toolResultCh <- matchedToolResults:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-session.stream.Done():
		return nil, fmt.Errorf("cursor: session stream closed before tool result injection")
	}

	// Return the resumeOutCh for the new HTTP handler to read from
	return &cliproxyexecutor.StreamResult{Chunks: session.resumeOutCh}, nil
}

// --- H2Stream helpers ---

func openCursorH2Stream(accessToken string) (*cursorproto.H2Stream, error) {
	requestID := uuid.New().String()
	traceParent := cursorTraceParent()
	headers := map[string]string{
		":path":                    cursorRunPath,
		"content-type":             "application/connect+proto",
		"connect-protocol-version": "1",
		"connect-accept-encoding":  "gzip",
		"te":                       "trailers",
		"authorization":            "Bearer " + accessToken,
		"backend-traceparent":      traceParent,
		"traceparent":              traceParent,
		"user-agent":               "connect-es/1.6.1",
		"x-ghost-mode":             "true",
		"x-cursor-client-version":  cursorClientVersionHeader(),
		"x-cursor-client-type":     "cli",
		"x-original-request-id":    requestID,
		"x-request-id":             requestID,
	}
	return cursorproto.DialH2Stream("api2.cursor.sh", headers)
}

func cursorTraceParent() string {
	traceID := strings.ReplaceAll(uuid.New().String(), "-", "")
	spanID := strings.ReplaceAll(uuid.New().String(), "-", "")[:16]
	return "00-" + traceID + "-" + spanID + "-01"
}

func cursorH2Heartbeat(ctx context.Context, stream *cursorproto.H2Stream) {
	ticker := time.NewTicker(cursorHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hb := cursorproto.EncodeHeartbeat()
			frame := cursorproto.FrameConnectMessage(hb, 0)
			if err := stream.Write(frame); err != nil {
				return
			}
		}
	}
}

// --- Response processing ---

// cursorTokenUsage tracks token counts from Cursor's TokenDeltaUpdate messages.
type cursorTokenUsage struct {
	mu             sync.Mutex
	outputTokens   int64
	inputTokensEst int64 // estimated from request payload size
}

func (u *cursorTokenUsage) addOutput(delta int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.outputTokens += delta
}

func (u *cursorTokenUsage) setInputEstimate(payloadBytes int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	// Rough estimate: ~4 bytes per token for mixed content
	u.inputTokensEst = int64(payloadBytes / 4)
	if u.inputTokensEst < 1 {
		u.inputTokensEst = 1
	}
}

func (u *cursorTokenUsage) get() (input, output int64) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.inputTokensEst, u.outputTokens
}

func cursorJSONErrorFromPayload(payload []byte) error {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || trimmed[0] != '{' || !bytes.Contains(trimmed, []byte(`"error"`)) {
		return nil
	}
	var envelope struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Details []struct {
				Debug struct {
					Details struct {
						Title  string `json:"title"`
						Detail string `json:"detail"`
					} `json:"details"`
				} `json:"debug"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &envelope); err != nil || envelope.Error == nil {
		return nil
	}
	message := envelope.Error.Message
	for _, detail := range envelope.Error.Details {
		if detail.Debug.Details.Title != "" {
			message = detail.Debug.Details.Title
			break
		}
		if detail.Debug.Details.Detail != "" {
			message = detail.Debug.Details.Detail
			break
		}
	}
	if message == "" {
		message = string(trimmed)
	}
	switch strings.ToLower(strings.TrimSpace(envelope.Error.Code)) {
	case "resource_exhausted":
		return cursorStatusErr{code: http.StatusTooManyRequests, msg: message}
	case "unauthenticated":
		return cursorStatusErr{code: http.StatusUnauthorized, msg: message}
	case "permission_denied":
		return cursorStatusErr{code: http.StatusForbidden, msg: message}
	case "unavailable":
		return cursorStatusErr{code: http.StatusServiceUnavailable, msg: message}
	case "internal":
		return cursorStatusErr{code: http.StatusInternalServerError, msg: message}
	default:
		return cursorStatusErr{code: http.StatusBadGateway, msg: message}
	}
}

type cursorInteractionToolCollector struct {
	calls   map[string]*cursorInteractionToolState
	emitted map[string]struct{}
}

type cursorInteractionToolState struct {
	toolName string
	args     map[string][]byte
	argsText strings.Builder
}

func newCursorInteractionToolCollector() *cursorInteractionToolCollector {
	return &cursorInteractionToolCollector{
		calls:   make(map[string]*cursorInteractionToolState),
		emitted: make(map[string]struct{}),
	}
}

func (c *cursorInteractionToolCollector) absorb(msg *cursorproto.DecodedServerMessage) *cursorproto.DecodedServerMessage {
	if msg == nil || !msg.InteractionToolCall {
		return msg
	}
	callID := strings.TrimSpace(msg.McpToolCallId)
	if callID == "" {
		return nil
	}
	if _, ok := c.emitted[callID]; ok {
		return nil
	}
	state := c.calls[callID]
	if state == nil {
		state = &cursorInteractionToolState{}
		c.calls[callID] = state
	}
	if strings.TrimSpace(msg.McpToolName) != "" {
		state.toolName = msg.McpToolName
	}
	if len(msg.McpArgs) > 0 {
		if state.args == nil {
			state.args = make(map[string][]byte, len(msg.McpArgs))
		}
		for key, value := range msg.McpArgs {
			state.args[key] = append([]byte(nil), value...)
		}
	}
	if msg.InteractionArgsTextDelta != "" {
		state.argsText.WriteString(msg.InteractionArgsTextDelta)
	}
	args := state.args
	if len(args) == 0 {
		args = cursorArgsMapFromJSONObject(state.argsText.String())
	}
	if state.toolName != "" && cursorInteractionToolCallHasRequiredArgs(state.toolName, args) {
		delete(c.calls, callID)
		c.emitted[callID] = struct{}{}
		return &cursorproto.DecodedServerMessage{
			Type:                cursorproto.ServerMsgExecMcpArgs,
			McpToolName:         state.toolName,
			McpToolCallId:       callID,
			McpArgs:             args,
			InteractionToolCall: true,
		}
	}
	if !msg.InteractionToolCallCompleted {
		return nil
	}
	delete(c.calls, callID)
	c.emitted[callID] = struct{}{}
	return &cursorproto.DecodedServerMessage{
		Type:                cursorproto.ServerMsgExecMcpArgs,
		McpToolName:         state.toolName,
		McpToolCallId:       callID,
		McpArgs:             args,
		InteractionToolCall: true,
	}
}

func cursorArgsMapFromJSONObject(text string) map[string][]byte {
	if strings.TrimSpace(text) == "" {
		return map[string][]byte{}
	}
	decoded := make(map[string]json.RawMessage)
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		return map[string][]byte{}
	}
	args := make(map[string][]byte, len(decoded))
	for key, value := range decoded {
		args[key] = append([]byte(nil), value...)
	}
	return args
}

type cursorExecDeduper struct {
	seen map[string]struct{}
}

func newCursorExecDeduper() *cursorExecDeduper {
	return &cursorExecDeduper{seen: make(map[string]struct{})}
}

func (d *cursorExecDeduper) mark(msg *cursorproto.DecodedServerMessage) bool {
	if d == nil || msg == nil || !cursorIsExecMessage(msg.Type) {
		return true
	}
	key := fmt.Sprintf("%d:%s:%d", msg.Type, msg.ExecId, msg.ExecMsgId)
	if msg.Type == cursorproto.ServerMsgExecMcpArgs && msg.ExecId == "" && msg.ExecMsgId == 0 && msg.McpToolCallId != "" {
		key = fmt.Sprintf("%d:tool:%s", msg.Type, msg.McpToolCallId)
	}
	if _, ok := d.seen[key]; ok {
		return false
	}
	d.seen[key] = struct{}{}
	return true
}

func cursorIsExecMessage(msgType cursorproto.ServerMessageType) bool {
	switch msgType {
	case cursorproto.ServerMsgExecRequestCtx,
		cursorproto.ServerMsgExecMcpArgs,
		cursorproto.ServerMsgExecShellArgs,
		cursorproto.ServerMsgExecReadArgs,
		cursorproto.ServerMsgExecWriteArgs,
		cursorproto.ServerMsgExecDeleteArgs,
		cursorproto.ServerMsgExecLsArgs,
		cursorproto.ServerMsgExecGrepArgs,
		cursorproto.ServerMsgExecFetchArgs,
		cursorproto.ServerMsgExecDiagnostics,
		cursorproto.ServerMsgExecShellStream,
		cursorproto.ServerMsgExecBgShellSpawn,
		cursorproto.ServerMsgExecWriteShellStdin,
		cursorproto.ServerMsgExecOther:
		return true
	default:
		return false
	}
}

func cursorShouldEndAfterKV(receivedContent bool, msgType cursorproto.ServerMessageType, waitingForTool bool) bool {
	return receivedContent && !waitingForTool && msgType == cursorproto.ServerMsgKvSetBlob
}

func processH2SessionFrames(
	ctx context.Context,
	stream *cursorproto.H2Stream,
	blobStore map[string][]byte,
	mcpTools []cursorproto.McpToolDef,
	onText func(text string, isThinking bool),
	onMcpExec func(execs []pendingMcpExec),
	toolResultCh <-chan []toolResultInfo, // nil for no tool result injection; non-nil to wait for results
	tokenUsage *cursorTokenUsage, // tracks accumulated token usage (may be nil)
	onCheckpoint func(data []byte), // called when server sends conversation_checkpoint_update
) error {
	var buf bytes.Buffer
	rejectReason := "Tool not available in this environment. Use the MCP tools provided instead."
	execDeduper := newCursorExecDeduper()
	interactionTools := newCursorInteractionToolCollector()
	receivedContent := false
	log.Debugf("cursor: processH2SessionFrames started for streamID=%s, waiting for data...", stream.ID())
	for {
		select {
		case <-ctx.Done():
			log.Debugf("cursor: processH2SessionFrames exiting: context done")
			return ctx.Err()
		case data, ok := <-stream.Data():
			if !ok {
				log.Debugf("cursor: processH2SessionFrames[%s]: exiting: stream data channel closed", stream.ID())
				return stream.Err() // may be RST_STREAM, GOAWAY, or nil for clean close
			}
			// Log first 20 bytes of raw data for debugging
			previewLen := min(20, len(data))
			log.Debugf("cursor: processH2SessionFrames[%s]: received %d bytes from dataCh, first bytes: %x (%q)", stream.ID(), len(data), data[:previewLen], string(data[:previewLen]))
			buf.Write(data)
			log.Debugf("cursor: processH2SessionFrames[%s]: buf total=%d", stream.ID(), buf.Len())

			// Process all complete frames
			for {
				currentBuf := buf.Bytes()
				if len(currentBuf) == 0 {
					break
				}
				flags, payload, consumed, ok := cursorproto.ParseConnectFrame(currentBuf)
				if !ok {
					// Log detailed info about why parsing failed
					previewLen := min(20, len(currentBuf))
					log.Debugf("cursor: incomplete frame in buffer, waiting for more data (buf=%d bytes, first bytes: %x = %q)", len(currentBuf), currentBuf[:previewLen], string(currentBuf[:previewLen]))
					break
				}
				buf.Next(consumed)
				log.Debugf("cursor: parsed Connect frame flags=0x%02x payload=%d bytes consumed=%d", flags, len(payload), consumed)

				if flags&cursorproto.ConnectEndStreamFlag != 0 {
					if err := cursorproto.ParseConnectEndStream(payload); err != nil {
						log.Warnf("cursor: connect end stream error: %v", err)
						return err // propagate server-side errors (quota, rate limit, etc.)
					}
					continue
				}
				if jsonErr := cursorJSONErrorFromPayload(payload); jsonErr != nil {
					if receivedContent {
						log.Debugf("cursor: JSON error after content; ending stream cleanly: %v", jsonErr)
						return nil
					}
					return jsonErr
				}

				msg, err := cursorproto.DecodeAgentServerMessage(payload)
				if err != nil {
					log.Debugf("cursor: failed to decode server message: %v", err)
					continue
				}

				log.Debugf("cursor: decoded server message type=%d", msg.Type)
				if !execDeduper.mark(msg) {
					log.Debugf("cursor: skipping duplicate exec message type=%d execMsgId=%d execId=%q", msg.Type, msg.ExecMsgId, msg.ExecId)
					continue
				}
				switch msg.Type {
				case cursorproto.ServerMsgTextDelta:
					if msg.Text != "" {
						receivedContent = true
						if onText != nil {
							onText(msg.Text, false)
						}
					}
				case cursorproto.ServerMsgThinkingDelta:
					if msg.Text != "" {
						receivedContent = true
						if onText != nil {
							onText(msg.Text, true)
						}
					}
				case cursorproto.ServerMsgThinkingCompleted:
					// Handled by caller

				case cursorproto.ServerMsgTurnEnded:
					log.Debugf("cursor: TurnEnded received, stream will finish")
					return nil // clean completion

				case cursorproto.ServerMsgHeartbeat:
					// Server heartbeat, ignore silently
					continue

				case cursorproto.ServerMsgCheckpoint:
					if onCheckpoint != nil && len(msg.CheckpointData) > 0 {
						onCheckpoint(msg.CheckpointData)
					}
					continue

				case cursorproto.ServerMsgTokenDelta:
					if tokenUsage != nil && msg.TokenDelta > 0 {
						tokenUsage.addOutput(msg.TokenDelta)
					}
					continue

				case cursorproto.ServerMsgKvGetBlob:
					blobKey := cursorproto.BlobIdHex(msg.BlobId)
					data := blobStore[blobKey]
					resp := cursorproto.EncodeKvGetBlobResult(msg.KvId, data, msg.RequestMetadata)
					stream.Write(cursorproto.FrameConnectMessage(resp, 0))

				case cursorproto.ServerMsgKvSetBlob:
					blobKey := cursorproto.BlobIdHex(msg.BlobId)
					blobStore[blobKey] = append([]byte(nil), msg.BlobData...)
					resp := cursorproto.EncodeKvSetBlobResult(msg.KvId, msg.RequestMetadata)
					stream.Write(cursorproto.FrameConnectMessage(resp, 0))
					if cursorShouldEndAfterKV(receivedContent, msg.Type, false) {
						log.Debugf("cursor: KV set after content; treating as clean end of response")
						return nil
					}

				case cursorproto.ServerMsgExecRequestCtx:
					resp := cursorproto.EncodeExecRequestContextResult(msg.ExecMsgId, msg.ExecId, mcpTools)
					stream.Write(cursorproto.FrameConnectMessage(resp, 0))

				case cursorproto.ServerMsgExecMcpArgs:
					msg = interactionTools.absorb(msg)
					if msg == nil {
						continue
					}
					if !cursorShouldEmitMcpExec(msg, mcpTools) {
						log.Debugf("cursor: skipping interaction tool call not declared or not ready for client: toolName=%q toolCallId=%q", msg.McpToolName, msg.McpToolCallId)
						continue
					}
					if onMcpExec != nil {
						pendingExecs := []pendingMcpExec{cursorPendingMcpExecFromMessage(msg)}
						onMcpExec(pendingExecs)

						if toolResultCh == nil {
							return nil
						}

						for {
							// Inline mode: wait for the current tool result batch while
							// handling KV/heartbeat and queueing any additional MCP
							// requests Cursor pipelines before the current batch returns.
							log.Debugf("cursor: waiting for %d tool result(s) on channel (inline mode)...", len(pendingExecs))
							var toolResults []toolResultInfo
							var queuedExecs []pendingMcpExec
						waitLoop:
							for {
								select {
								case <-ctx.Done():
									return ctx.Err()
								case results, ok := <-toolResultCh:
									if !ok {
										return nil
									}
									toolResults = results
									break waitLoop
								case waitData, ok := <-stream.Data():
									if !ok {
										return stream.Err()
									}
									buf.Write(waitData)
									for {
										cb := buf.Bytes()
										if len(cb) == 0 {
											break
										}
										wf, wp, wc, wok := cursorproto.ParseConnectFrame(cb)
										if !wok {
											break
										}
										buf.Next(wc)
										if wf&cursorproto.ConnectEndStreamFlag != 0 {
											if err := cursorproto.ParseConnectEndStream(wp); err != nil {
												return err
											}
											continue
										}
										if jsonErr := cursorJSONErrorFromPayload(wp); jsonErr != nil {
											if receivedContent {
												return nil
											}
											return jsonErr
										}
										wmsg, werr := cursorproto.DecodeAgentServerMessage(wp)
										if werr != nil {
											continue
										}
										if !execDeduper.mark(wmsg) {
											continue
										}
										switch wmsg.Type {
										case cursorproto.ServerMsgKvGetBlob:
											blobKey := cursorproto.BlobIdHex(wmsg.BlobId)
											d := blobStore[blobKey]
											stream.Write(cursorproto.FrameConnectMessage(cursorproto.EncodeKvGetBlobResult(wmsg.KvId, d, wmsg.RequestMetadata), 0))
										case cursorproto.ServerMsgKvSetBlob:
											blobKey := cursorproto.BlobIdHex(wmsg.BlobId)
											blobStore[blobKey] = append([]byte(nil), wmsg.BlobData...)
											stream.Write(cursorproto.FrameConnectMessage(cursorproto.EncodeKvSetBlobResult(wmsg.KvId, wmsg.RequestMetadata), 0))
											if cursorShouldEndAfterKV(receivedContent, wmsg.Type, true) {
												return nil
											}
										case cursorproto.ServerMsgExecRequestCtx:
											stream.Write(cursorproto.FrameConnectMessage(cursorproto.EncodeExecRequestContextResult(wmsg.ExecMsgId, wmsg.ExecId, mcpTools), 0))
										case cursorproto.ServerMsgExecMcpArgs:
											wmsg = interactionTools.absorb(wmsg)
											if wmsg == nil {
												continue
											}
											if !cursorShouldEmitMcpExec(wmsg, mcpTools) {
												log.Debugf("cursor: skipping queued interaction tool call not declared or not ready for client: toolName=%q toolCallId=%q", wmsg.McpToolName, wmsg.McpToolCallId)
												continue
											}
											queued := cursorPendingMcpExecFromMessage(wmsg)
											log.Debugf("cursor: queued pipelined mcpArgs while waiting: execMsgId=%d execId=%q toolName=%s toolCallId=%s",
												queued.ExecMsgId, queued.ExecId, queued.ToolName, queued.ToolCallId)
											queuedExecs = append(queuedExecs, queued)
										case cursorproto.ServerMsgCheckpoint:
											if onCheckpoint != nil && len(wmsg.CheckpointData) > 0 {
												onCheckpoint(wmsg.CheckpointData)
											}
										case cursorproto.ServerMsgTurnEnded:
											return nil
										}
									}
								case <-stream.Done():
									return stream.Err()
								}
							}

							// Send MCP results for the current pending batch.
							for _, exec := range pendingExecs {
								sentResult := false
								for _, tr := range toolResults {
									if tr.ToolCallId == exec.ToolCallId {
										log.Debugf("cursor: sending inline MCP result for tool=%s", exec.ToolName)
										resultBytes := cursorproto.EncodeExecMcpResult(exec.ExecMsgId, exec.ExecId, tr.Content, false)
										stream.Write(cursorproto.FrameConnectMessage(resultBytes, 0))
										sentResult = true
										break
									}
								}
								if !sentResult {
									return fmt.Errorf("cursor: no tool result for pending call %s", exec.ToolCallId)
								}
							}
							// The tool_call response has ended and the injected result starts a
							// new assistant response on the same Cursor H2 stream. Scope
							// KV-after-content termination to that new response; otherwise text
							// emitted before the tool call can make post-result KV updates look
							// like the end of the resumed response and prematurely cut off
							// multi-tool chains.
							receivedContent = false
							if len(queuedExecs) == 0 {
								break
							}
							pendingExecs = queuedExecs
							onMcpExec(pendingExecs)
						}
						continue
					}

				case cursorproto.ServerMsgExecReadArgs:
					stream.Write(cursorproto.FrameConnectMessage(cursorproto.EncodeExecReadRejected(msg.ExecMsgId, msg.ExecId, msg.Path, rejectReason), 0))
				case cursorproto.ServerMsgExecWriteArgs:
					stream.Write(cursorproto.FrameConnectMessage(cursorproto.EncodeExecWriteRejected(msg.ExecMsgId, msg.ExecId, msg.Path, rejectReason), 0))
				case cursorproto.ServerMsgExecDeleteArgs:
					stream.Write(cursorproto.FrameConnectMessage(cursorproto.EncodeExecDeleteRejected(msg.ExecMsgId, msg.ExecId, msg.Path, rejectReason), 0))
				case cursorproto.ServerMsgExecLsArgs:
					stream.Write(cursorproto.FrameConnectMessage(cursorproto.EncodeExecLsRejected(msg.ExecMsgId, msg.ExecId, msg.Path, rejectReason), 0))
				case cursorproto.ServerMsgExecGrepArgs:
					stream.Write(cursorproto.FrameConnectMessage(cursorproto.EncodeExecGrepError(msg.ExecMsgId, msg.ExecId, rejectReason), 0))
				case cursorproto.ServerMsgExecShellArgs, cursorproto.ServerMsgExecShellStream:
					stream.Write(cursorproto.FrameConnectMessage(cursorproto.EncodeExecShellRejected(msg.ExecMsgId, msg.ExecId, msg.Command, msg.WorkingDirectory, rejectReason), 0))
				case cursorproto.ServerMsgExecBgShellSpawn:
					stream.Write(cursorproto.FrameConnectMessage(cursorproto.EncodeExecBackgroundShellSpawnRejected(msg.ExecMsgId, msg.ExecId, msg.Command, msg.WorkingDirectory, rejectReason), 0))
				case cursorproto.ServerMsgExecFetchArgs:
					stream.Write(cursorproto.FrameConnectMessage(cursorproto.EncodeExecFetchError(msg.ExecMsgId, msg.ExecId, msg.Url, rejectReason), 0))
				case cursorproto.ServerMsgExecDiagnostics:
					stream.Write(cursorproto.FrameConnectMessage(cursorproto.EncodeExecDiagnosticsResult(msg.ExecMsgId, msg.ExecId), 0))
				case cursorproto.ServerMsgExecWriteShellStdin:
					stream.Write(cursorproto.FrameConnectMessage(cursorproto.EncodeExecWriteShellStdinError(msg.ExecMsgId, msg.ExecId, rejectReason), 0))
				}
			}

		case <-stream.Done():
			log.Debugf("cursor: processH2SessionFrames exiting: stream done")
			return stream.Err()
		}
	}
}

// --- OpenAI request parsing ---

type parsedOpenAIRequest struct {
	Model        string
	RawPayload   []byte
	Messages     []gjson.Result
	Tools        []gjson.Result
	Stream       bool
	SystemPrompt string
	UserText     string
	Images       []cursorproto.ImageData
	Turns        []cursorproto.TurnData
	ToolResults  []toolResultInfo
}

type toolResultInfo struct {
	ToolCallId string
	Content    string
}

func parseOpenAIRequest(payload []byte) *parsedOpenAIRequest {
	p := &parsedOpenAIRequest{
		Model:      gjson.GetBytes(payload, "model").String(),
		RawPayload: bytes.Clone(payload),
		Stream:     gjson.GetBytes(payload, "stream").Bool(),
	}

	messages := gjson.GetBytes(payload, "messages").Array()
	p.Messages = messages

	// Extract system prompt
	var systemParts []string
	for _, msg := range messages {
		if msg.Get("role").String() == "system" {
			systemParts = append(systemParts, extractTextContent(msg.Get("content")))
		}
	}
	if len(systemParts) > 0 {
		p.SystemPrompt = strings.Join(systemParts, "\n")
	} else {
		p.SystemPrompt = "You are a helpful assistant."
	}

	// Extract turns and last user message. Tool messages are parsed separately
	// below: only trailing tool results are pending live-resume inputs. Older
	// tool results are historical context and should not disable checkpoint-based
	// multi-turn continuation after the assistant has already responded.
	var pendingUser string
	for _, msg := range messages {
		role := msg.Get("role").String()
		switch role {
		case "system", "tool":
			continue
		case "user":
			if pendingUser != "" {
				p.Turns = append(p.Turns, cursorproto.TurnData{UserText: pendingUser})
			}
			pendingUser = extractTextContent(msg.Get("content"))
			p.Images = extractImages(msg.Get("content"))
		case "assistant":
			assistantText := extractTextContent(msg.Get("content"))
			if pendingUser != "" {
				p.Turns = append(p.Turns, cursorproto.TurnData{
					UserText:      pendingUser,
					AssistantText: assistantText,
				})
				pendingUser = ""
			} else if len(p.Turns) > 0 && assistantText != "" {
				// Assistant message after tool results (no pending user) —
				// append to the last turn's assistant text to preserve context.
				last := &p.Turns[len(p.Turns)-1]
				if last.AssistantText != "" {
					last.AssistantText += "\n" + assistantText
				} else {
					last.AssistantText = assistantText
				}
			}
		}
	}

	// Only tool messages at the end of the request are pending results for the
	// previous assistant tool_call. Historical tool results are preserved by
	// flattenConversationIntoUserText when cold fallback is needed, but they
	// should not trigger live resume or force cold fallback on later user turns.
	p.ToolResults = cursorTrailingToolResults(messages)

	if pendingUser != "" {
		p.UserText = pendingUser
	} else if len(p.Turns) > 0 && len(p.ToolResults) == 0 {
		last := p.Turns[len(p.Turns)-1]
		p.Turns = p.Turns[:len(p.Turns)-1]
		p.UserText = last.UserText
	}

	// Extract tools
	p.Tools = gjson.GetBytes(payload, "tools").Array()

	return p
}

func cursorTrailingToolResults(messages []gjson.Result) []toolResultInfo {
	if len(messages) == 0 {
		return nil
	}
	var reversed []toolResultInfo
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Get("role").String() != "tool" {
			break
		}
		reversed = append(reversed, toolResultInfo{
			ToolCallId: msg.Get("tool_call_id").String(),
			Content:    extractTextContent(msg.Get("content")),
		})
	}
	if len(reversed) == 0 {
		return nil
	}
	results := make([]toolResultInfo, len(reversed))
	for i := range reversed {
		results[len(reversed)-1-i] = reversed[i]
	}
	return results
}

// flattenConversationIntoUserText flattens the full OpenAI-shaped message
// history into Cursor's single UserMessage text. Cursor reliably reads
// UserText but does not consistently honor client-synthesized protobuf turns
// or system-prompt blobs, so cold-start paths include system instructions,
// assistant tool calls, and tool results directly in text.
func flattenConversationIntoUserText(parsed *parsedOpenAIRequest) {
	if parsed == nil {
		return
	}
	if text := cursorFlattenMessages(parsed.Messages); text != "" {
		parsed.UserText = text
	} else if parsed.UserText == "" {
		parsed.UserText = "Continue from the conversation above."
	}
	parsed.Turns = nil
	parsed.ToolResults = nil
}

func cursorFlattenMessages(messages []gjson.Result) string {
	if len(messages) == 0 {
		return ""
	}

	systemTexts := make([]string, 0, 1)
	turns := make([]gjson.Result, 0, len(messages))
	for _, msg := range messages {
		if msg.Get("role").String() == "system" {
			if text := extractTextContent(msg.Get("content")); strings.TrimSpace(text) != "" {
				systemTexts = append(systemTexts, text)
			}
			continue
		}
		turns = append(turns, msg)
	}

	if len(turns) == 1 && turns[0].Get("role").String() == "user" && len(turns[0].Get("tool_calls").Array()) == 0 && !cursorContentHasToolResult(turns[0].Get("content")) {
		userText := extractTextContent(turns[0].Get("content"))
		if len(systemTexts) > 0 {
			if userText != "" {
				return strings.Join(systemTexts, "\n\n") + "\n\n" + userText
			}
			return strings.Join(systemTexts, "\n\n")
		}
		return userText
	}

	lines := make([]string, 0, len(turns)*2)
	for _, msg := range turns {
		role := msg.Get("role").String()
		switch role {
		case "user":
			text := extractTextContent(msg.Get("content"))
			if text != "" {
				lines = append(lines, "User: "+text)
			}
			lines = append(lines, cursorToolResultLinesFromContent(msg.Get("content"))...)
		case "assistant":
			text := extractTextContent(msg.Get("content"))
			if text != "" {
				lines = append(lines, "Assistant: "+text)
			}
			lines = append(lines, cursorAssistantToolCallLines(msg)...)
		case "tool":
			callID := msg.Get("tool_call_id").String()
			if callID == "" {
				callID = "(unknown)"
			}
			text := extractTextContent(msg.Get("content"))
			if len(text) > 8000 {
				text = text[:8000] + "\n... [truncated]"
			}
			lines = append(lines, fmt.Sprintf("Tool result (%s): %s", callID, text))
		default:
			text := extractTextContent(msg.Get("content"))
			if text != "" {
				lines = append(lines, role+": "+text)
			}
		}
	}

	body := strings.Join(lines, "\n\n")
	if len(systemTexts) > 0 {
		if body != "" {
			return strings.Join(systemTexts, "\n\n") + "\n\n" + body
		}
		return strings.Join(systemTexts, "\n\n")
	}
	return body
}

func cursorContentHasToolResult(content gjson.Result) bool {
	if !content.IsArray() {
		return false
	}
	for _, part := range content.Array() {
		if part.Get("type").String() == "tool_result" {
			return true
		}
	}
	return false
}

func cursorToolResultLinesFromContent(content gjson.Result) []string {
	if !content.IsArray() {
		return nil
	}
	out := make([]string, 0)
	for _, part := range content.Array() {
		if part.Get("type").String() != "tool_result" {
			continue
		}
		callID := part.Get("tool_use_id").String()
		if callID == "" {
			callID = "(unknown)"
		}
		text := extractTextContent(part.Get("content"))
		if text == "" {
			text = part.Get("content").String()
		}
		out = append(out, fmt.Sprintf("Tool result (%s): %s", callID, text))
	}
	return out
}

func cursorAssistantToolCallLines(msg gjson.Result) []string {
	out := make([]string, 0)
	for _, tc := range msg.Get("tool_calls").Array() {
		id := tc.Get("id").String()
		if id == "" {
			id = "(unknown)"
		}
		name := tc.Get("function.name").String()
		if name == "" {
			name = "tool"
		}
		args := tc.Get("function.arguments").String()
		out = append(out, fmt.Sprintf("Assistant called tool %s (%s) with arguments: %s", name, id, args))
	}
	content := msg.Get("content")
	if content.IsArray() {
		for _, part := range content.Array() {
			if part.Get("type").String() != "tool_use" {
				continue
			}
			id := part.Get("id").String()
			if id == "" {
				id = "(unknown)"
			}
			name := part.Get("name").String()
			if name == "" {
				name = "tool"
			}
			args := part.Get("input").Raw
			if args == "" {
				args = "{}"
			}
			out = append(out, fmt.Sprintf("Assistant called tool %s (%s) with arguments: %s", name, id, args))
		}
	}
	return out
}

func extractTextContent(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if content.IsArray() {
		var parts []string
		for _, part := range content.Array() {
			if part.Get("type").String() == "text" {
				parts = append(parts, part.Get("text").String())
			}
		}
		return strings.Join(parts, "")
	}
	return content.String()
}

func extractImages(content gjson.Result) []cursorproto.ImageData {
	if !content.IsArray() {
		return nil
	}
	images := make([]cursorproto.ImageData, 0)
	for _, part := range content.Array() {
		if part.Get("type").String() != "image_url" {
			continue
		}
		rawURL := strings.TrimSpace(part.Get("image_url.url").String())
		if rawURL == "" {
			continue
		}
		if img := parseDataURL(rawURL); img != nil {
			images = append(images, *img)
			continue
		}
		images = append(images, cursorproto.ImageData{URL: rawURL})
	}
	return images
}

func parseDataURL(rawURL string) *cursorproto.ImageData {
	if !strings.HasPrefix(rawURL, "data:") {
		return nil
	}
	metadataAndData := strings.SplitN(strings.TrimPrefix(rawURL, "data:"), ",", 2)
	if len(metadataAndData) != 2 {
		return nil
	}
	metadata := metadataAndData[0]
	if !strings.Contains(metadata, ";base64") {
		return nil
	}
	mimeType := strings.TrimSpace(strings.SplitN(metadata, ";", 2)[0])
	if !strings.HasPrefix(mimeType, "image/") {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(metadataAndData[1])
	if err != nil {
		data, err = base64.RawStdEncoding.DecodeString(metadataAndData[1])
		if err != nil {
			return nil
		}
	}
	if len(data) > cursorMaxImageBytes {
		return nil
	}
	return &cursorproto.ImageData{MimeType: mimeType, Data: data}
}

func (e *CursorExecutor) resolveCursorRemoteImages(ctx context.Context, auth *cliproxyauth.Auth, parsed *parsedOpenAIRequest) error {
	if parsed == nil || len(parsed.Images) == 0 {
		return nil
	}
	client := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	for i := range parsed.Images {
		if len(parsed.Images[i].Data) > 0 || strings.TrimSpace(parsed.Images[i].URL) == "" {
			continue
		}
		image, err := cursorFetchRemoteImage(ctx, client, parsed.Images[i].URL)
		if err != nil {
			return err
		}
		parsed.Images[i] = image
	}
	return nil
}

func cursorFetchRemoteImage(ctx context.Context, client *http.Client, rawURL string) (cursorproto.ImageData, error) {
	parsedURL, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsedURL == nil || parsedURL.Host == "" {
		return cursorproto.ImageData{}, cursorStatusErr{code: http.StatusBadRequest, msg: "cursor: invalid image_url"}
	}
	if parsedURL.Scheme != "http" && parsedURL.Scheme != "https" {
		return cursorproto.ImageData{}, cursorStatusErr{code: http.StatusBadRequest, msg: "cursor: image_url must use http or https"}
	}
	if client == nil {
		client = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsedURL.String(), nil)
	if err != nil {
		return cursorproto.ImageData{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return cursorproto.ImageData{}, err
	}
	defer func() {
		if closeErr := resp.Body.Close(); closeErr != nil {
			log.Errorf("cursor: failed to close image_url response body: %v", closeErr)
		}
	}()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return cursorproto.ImageData{}, cursorStatusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("cursor: fetch image_url failed with status %d", resp.StatusCode)}
	}
	mimeType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0])
	if !strings.HasPrefix(mimeType, "image/") {
		return cursorproto.ImageData{}, cursorStatusErr{code: http.StatusBadRequest, msg: "cursor: image_url did not return an image content type"}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, cursorMaxImageBytes+1))
	if err != nil {
		return cursorproto.ImageData{}, err
	}
	if len(data) > cursorMaxImageBytes {
		return cursorproto.ImageData{}, cursorStatusErr{code: http.StatusRequestEntityTooLarge, msg: "cursor: image_url response is too large"}
	}
	return cursorproto.ImageData{MimeType: mimeType, Data: data}, nil
}

func buildRunRequestParams(parsed *parsedOpenAIRequest, conversationId string) *cursorproto.RunRequestParams {
	modelTarget := resolveCursorModelTarget(parsed.Model, parsed.RawPayload)
	requestedModel := cursorResolveRequestedModel(modelTarget)
	params := &cursorproto.RunRequestParams{
		ModelId:         requestedModel.ModelID,
		ModelParameters: requestedModel.Parameters,
		SystemPrompt:    parsed.SystemPrompt,
		UserText:        parsed.UserText,
		MessageId:       uuid.New().String(),
		ConversationId:  conversationId,
		Images:          parsed.Images,
		Turns:           parsed.Turns,
		BlobStore:       make(map[string][]byte),
	}

	// Convert OpenAI tools to Cursor MCP tool definitions. Prefix every
	// Cursor-facing MCP name so neither current nor future Cursor-native tool
	// names (web_search/read/grep/etc.) can steal the model's tool selection.
	// The original OpenAI name is restored by stripping exactly one prefix
	// before emitting tool_calls.
	for _, tool := range parsed.Tools {
		if tool.Get("type").String() != "function" {
			continue
		}
		fn := tool.Get("function")
		originalName := strings.TrimSpace(fn.Get("name").String())
		if originalName == "" {
			continue
		}
		params.McpTools = append(params.McpTools, cursorproto.McpToolDef{
			Name:        cursorOpenAIToolAliasPrefix + originalName,
			Description: strings.TrimSpace(fn.Get("description").String()),
			InputSchema: json.RawMessage(fn.Get("parameters").Raw),
		})
	}

	return params
}

// --- Helpers ---

func cursorAccessToken(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if v, ok := auth.Metadata["access_token"].(string); ok {
		return v
	}
	return ""
}

func cursorRefreshToken(auth *cliproxyauth.Auth) string {
	if auth == nil || auth.Metadata == nil {
		return ""
	}
	if v, ok := auth.Metadata["refresh_token"].(string); ok {
		return v
	}
	return ""
}

func applyCursorHeaders(req *http.Request, accessToken string) {
	requestID := uuid.New().String()
	traceParent := cursorTraceParent()
	req.Header.Set("Content-Type", "application/connect+proto")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Connect-Accept-Encoding", "gzip")
	req.Header.Set("Te", "trailers")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Backend-Traceparent", traceParent)
	req.Header.Set("Traceparent", traceParent)
	req.Header.Set("User-Agent", "connect-es/1.6.1")
	req.Header.Set("X-Ghost-Mode", "true")
	req.Header.Set("X-Cursor-Client-Version", cursorClientVersionHeader())
	req.Header.Set("X-Cursor-Client-Type", "cli")
	req.Header.Set("X-Original-Request-Id", requestID)
	req.Header.Set("X-Request-Id", requestID)
}

func newH2Client() *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			TLSClientConfig: &tls.Config{},
		},
	}
}

// extractCCH extracts the cch value from the system prompt's billing header.
func extractCCH(systemPrompt string) string {
	idx := strings.Index(systemPrompt, "cch=")
	if idx < 0 {
		return ""
	}
	rest := systemPrompt[idx+4:]
	end := strings.IndexAny(rest, "; \n")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

// extractClaudeCodeSessionId extracts session_id from Claude Code's metadata.user_id JSON.
// Format: {"metadata":{"user_id":"{\"session_id\":\"xxx\",\"device_id\":\"yyy\"}"}}
func extractClaudeCodeSessionId(payload []byte) string {
	userIDStr := strings.TrimSpace(gjson.GetBytes(payload, "metadata.user_id").String())
	if userIDStr == "" {
		return ""
	}
	// user_id is a JSON string that needs to be parsed again.
	if strings.HasPrefix(userIDStr, "{") {
		return strings.TrimSpace(gjson.Get(userIDStr, "session_id").String())
	}
	// Older Claude-style values may end with _session_{uuid}.
	const marker = "_session_"
	if idx := strings.LastIndex(userIDStr, marker); idx >= 0 {
		return strings.TrimSpace(userIDStr[idx+len(marker):])
	}
	return ""
}

type cursorConversationResolution struct {
	ConversationID     string
	SessionID          string
	SessionSource      string
	ExecutionSessionID string
}

func resolveCursorConversation(apiKey, systemPrompt string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, payloads ...[]byte) cursorConversationResolution {
	executionSessionID := cursorExecutionSessionID(opts.Metadata)
	sessionID, source := cursorSessionIDFromRequest(req, opts, executionSessionID, payloads...)
	return cursorConversationResolution{
		ConversationID:     deriveConversationId(apiKey, sessionID, systemPrompt),
		SessionID:          sessionID,
		SessionSource:      source,
		ExecutionSessionID: executionSessionID,
	}
}

func cursorExecutionSessionID(metadata map[string]any) string {
	if len(metadata) == 0 {
		return ""
	}
	raw, ok := metadata[cliproxyexecutor.ExecutionSessionMetadataKey]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	case fmt.Stringer:
		return strings.TrimSpace(v.String())
	default:
		return ""
	}
}

func cursorSessionIDFromRequest(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, executionSessionID string, payloads ...[]byte) (string, string) {
	candidates := cursorPayloadCandidates(req, opts, payloads...)

	for _, payload := range candidates {
		if sid := extractClaudeCodeSessionId(payload); sid != "" {
			return sid, "metadata.user_id.session_id"
		}
	}
	if executionSessionID != "" {
		return "execution:" + executionSessionID, cliproxyexecutor.ExecutionSessionMetadataKey
	}
	if sid := firstCursorPayloadString(candidates, "prompt_cache_key"); sid != "" {
		return "prompt_cache:" + sid, "prompt_cache_key"
	}
	if sid := cursorHeaderValue(opts.Headers, "X-Session-ID"); sid != "" {
		return "header:" + sid, "X-Session-ID"
	}
	if sid := cursorHeaderValue(opts.Headers, "Session_id"); sid != "" {
		return "codex:" + sid, "Session_id"
	}
	if sid := cursorHeaderValue(opts.Headers, "X-Amp-Thread-Id"); sid != "" {
		return "amp:" + sid, "X-Amp-Thread-Id"
	}
	if sid := cursorHeaderValue(opts.Headers, "X-Client-Request-Id"); sid != "" {
		return "clientreq:" + sid, "X-Client-Request-Id"
	}
	if sid := firstCursorPayloadString(candidates, "conversation_id"); sid != "" {
		return "conv:" + sid, "conversation_id"
	}
	if sid := firstCursorPayloadString(candidates, "session_id"); sid != "" {
		return "body_session:" + sid, "session_id"
	}
	return "", "system_prompt_hash"
}

func cursorPayloadCandidates(req cliproxyexecutor.Request, opts cliproxyexecutor.Options, payloads ...[]byte) [][]byte {
	candidates := make([][]byte, 0, 2+len(payloads))
	add := func(payload []byte) {
		if len(payload) == 0 {
			return
		}
		for _, candidate := range candidates {
			if len(candidate) == len(payload) && &candidate[0] == &payload[0] {
				return
			}
		}
		candidates = append(candidates, payload)
	}
	add(req.Payload)
	add(opts.OriginalRequest)
	for _, payload := range payloads {
		add(payload)
	}
	return candidates
}

func firstCursorPayloadString(payloads [][]byte, path string) string {
	for _, payload := range payloads {
		if value := strings.TrimSpace(gjson.GetBytes(payload, path).String()); value != "" {
			return value
		}
	}
	return ""
}

func cursorHeaderValue(headers http.Header, key string) string {
	if headers == nil {
		return ""
	}
	return strings.TrimSpace(headers.Get(key))
}

// deriveConversationId generates a deterministic conversation_id.
// Priority: session_id (stable across resume) > system prompt hash (fallback).
func deriveConversationId(apiKey, sessionId, systemPrompt string) string {
	var input string
	if sessionId != "" {
		// Best: use Claude Code's session_id — stable even across resume
		input = "cursor-conv:" + apiKey + ":" + sessionId
	} else {
		// Fallback: use system prompt content minus volatile cch
		stable := systemPrompt
		if idx := strings.Index(stable, "cch="); idx >= 0 {
			end := strings.IndexAny(stable[idx:], "; \n")
			if end > 0 {
				stable = stable[:idx] + stable[idx+end:]
			}
		}
		if len(stable) > 500 {
			stable = stable[:500]
		}
		input = "cursor-conv:" + apiKey + ":" + stable
	}
	h := sha256.Sum256([]byte(input))
	s := hex.EncodeToString(h[:16])
	return fmt.Sprintf("%s-%s-%s-%s-%s", s[:8], s[8:12], s[12:16], s[16:20], s[20:32])
}

func deriveSessionKey(clientKey string, model string, messages []gjson.Result) string {
	var firstUserContent string
	var systemContent string
	for _, msg := range messages {
		role := msg.Get("role").String()
		if role == "user" && firstUserContent == "" {
			firstUserContent = extractTextContent(msg.Get("content"))
		} else if role == "system" && systemContent == "" {
			// System prompt differs per Claude Code session (contains cwd, session_id, etc.)
			content := extractTextContent(msg.Get("content"))
			if len(content) > 200 {
				systemContent = content[:200]
			} else {
				systemContent = content
			}
		}
	}
	// Include client API key + system prompt hash to prevent session collisions:
	// - Different users have different API keys
	// - Different Claude Code sessions have different system prompts (cwd, tools, etc.)
	input := clientKey + ":" + model + ":" + systemContent + ":" + firstUserContent
	if len(input) > 500 {
		input = input[:500]
	}
	h := sha256.Sum256([]byte(input))
	return hex.EncodeToString(h[:])[:16]
}

func sseChunk(id string, created int64, model string, delta string, finishReason string) cliproxyexecutor.StreamChunk {
	fr := "null"
	if finishReason != "" {
		fr = finishReason
	}
	// Note: the framework's WriteChunk adds "data: " prefix and "\n\n" suffix,
	// so we only output the raw JSON here.
	data := fmt.Sprintf(`{"id":"%s","object":"chat.completion.chunk","created":%d,"model":"%s","choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`,
		id, created, model, delta, fr)
	return cliproxyexecutor.StreamChunk{
		Payload: []byte(data),
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func cursorPendingMcpExecFromMessage(msg *cursorproto.DecodedServerMessage) pendingMcpExec {
	decodedArgs := decodeMcpArgsToJSON(msg.McpArgs)
	toolCallId := msg.McpToolCallId
	if toolCallId == "" {
		toolCallId = uuid.New().String()
	}
	toolName := cursorOpenAIToolNameForMcpTool(msg.McpToolName)
	log.Debugf("cursor: received mcpArgs from server: execMsgId=%d execId=%q toolName=%s cursorToolName=%s toolCallId=%s",
		msg.ExecMsgId, msg.ExecId, toolName, msg.McpToolName, toolCallId)
	return pendingMcpExec{
		ExecMsgId:  msg.ExecMsgId,
		ExecId:     msg.ExecId,
		ToolCallId: toolCallId,
		ToolName:   toolName,
		Args:       decodedArgs,
	}
}

func decodeMcpArgsToJSON(args map[string][]byte) string {
	if len(args) == 0 {
		return "{}"
	}
	result := make(map[string]interface{})
	for k, v := range args {
		// Try protobuf Value decoding first (matches TS: toJson(ValueSchema, fromBinary(ValueSchema, value)))
		if decoded, err := cursorproto.ProtobufValueBytesToJSON(v); err == nil {
			result[k] = decoded
		} else {
			// Fallback: try raw JSON
			var jsonVal interface{}
			if err := json.Unmarshal(v, &jsonVal); err == nil {
				result[k] = jsonVal
			} else {
				result[k] = string(v)
			}
		}
	}
	b, _ := json.Marshal(result)
	return string(b)
}

const cursorOpenAIToolAliasPrefix = "mcp__"

func cursorOpenAIToolNameForMcpTool(cursorName string) string {
	return strings.TrimPrefix(cursorName, cursorOpenAIToolAliasPrefix)
}

const cursorToolFallbackInstruction = "The tool results above are already completed. Continue from those results; do not repeat tool calls that already have results unless the user explicitly asks to rerun them."

func cursorAppendToolFallbackInstruction(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return cursorToolFallbackInstruction
	}
	return text + "\n\n" + cursorToolFallbackInstruction
}

type cursorRequestedModel struct {
	ModelID    string
	Parameters []cursorproto.ModelParameter
}

type cursorEffortBucket string

const (
	cursorEffortNone   cursorEffortBucket = "none"
	cursorEffortLow    cursorEffortBucket = "low"
	cursorEffortMedium cursorEffortBucket = "medium"
	cursorEffortHigh   cursorEffortBucket = "high"
	cursorEffortXHigh  cursorEffortBucket = "xhigh"
)

type cursorCanonicalFamily struct {
	ID      string
	Default string
	None    string
	Low     string
	Medium  string
	High    string
	XHigh   string
}

var cursorCanonicalFamilies = []cursorCanonicalFamily{
	{ID: "cursor-claude-4-sonnet", Default: "cursor-claude-4-sonnet", None: "cursor-claude-4-sonnet", Low: "cursor-claude-4-sonnet-thinking", Medium: "cursor-claude-4-sonnet-thinking", High: "cursor-claude-4-sonnet-thinking", XHigh: "cursor-claude-4-sonnet-thinking"},
	{ID: "cursor-claude-4-sonnet-1m", Default: "cursor-claude-4-sonnet-1m", None: "cursor-claude-4-sonnet-1m", Low: "cursor-claude-4-sonnet-1m-thinking", Medium: "cursor-claude-4-sonnet-1m-thinking", High: "cursor-claude-4-sonnet-1m-thinking", XHigh: "cursor-claude-4-sonnet-1m-thinking"},
	{ID: "cursor-claude-4.5-sonnet", Default: "cursor-claude-4.5-sonnet", None: "cursor-claude-4.5-sonnet", Low: "cursor-claude-4.5-sonnet-thinking", Medium: "cursor-claude-4.5-sonnet-thinking", High: "cursor-claude-4.5-sonnet-thinking", XHigh: "cursor-claude-4.5-sonnet-thinking"},
	{ID: "cursor-claude-4.6-opus-max", Default: "cursor-claude-4.6-opus-max", None: "cursor-claude-4.6-opus-max", Low: "cursor-claude-4.6-opus-max-thinking", Medium: "cursor-claude-4.6-opus-max-thinking", High: "cursor-claude-4.6-opus-max-thinking", XHigh: "cursor-claude-4.6-opus-max-thinking"},
	{ID: "cursor-grok-4-20", Default: "cursor-grok-4-20", None: "cursor-grok-4-20", Low: "cursor-grok-4-20-thinking", Medium: "cursor-grok-4-20-thinking", High: "cursor-grok-4-20-thinking", XHigh: "cursor-grok-4-20-thinking"},
	{ID: "cursor-claude-4.5-opus", Default: "cursor-claude-4.5-opus-high", None: "cursor-claude-4.5-opus-high", Low: "cursor-claude-4.5-opus-high-thinking", Medium: "cursor-claude-4.5-opus-high-thinking", High: "cursor-claude-4.5-opus-high-thinking", XHigh: "cursor-claude-4.5-opus-high-thinking"},
	{ID: "cursor-claude-4.6-opus", Default: "cursor-claude-4.6-opus-high", None: "cursor-claude-4.6-opus-high", Low: "cursor-claude-4.6-opus-high-thinking", Medium: "cursor-claude-4.6-opus-high-thinking", High: "cursor-claude-4.6-opus-high-thinking", XHigh: "cursor-claude-4.6-opus-high-thinking"},
	{ID: "cursor-claude-4.6-sonnet", Default: "cursor-claude-4.6-sonnet-medium", None: "cursor-claude-4.6-sonnet-medium", Low: "cursor-claude-4.6-sonnet-medium-thinking", Medium: "cursor-claude-4.6-sonnet-medium-thinking", High: "cursor-claude-4.6-sonnet-medium-thinking", XHigh: "cursor-claude-4.6-sonnet-medium-thinking"},
	{ID: "cursor-gpt-5.1", Default: "cursor-gpt-5.1", None: "cursor-gpt-5.1-low", Low: "cursor-gpt-5.1-low", Medium: "cursor-gpt-5.1", High: "cursor-gpt-5.1-high", XHigh: "cursor-gpt-5.1-high"},
	{ID: "cursor-gpt-5.1-codex-mini", Default: "cursor-gpt-5.1-codex-mini", None: "cursor-gpt-5.1-codex-mini-low", Low: "cursor-gpt-5.1-codex-mini-low", Medium: "cursor-gpt-5.1-codex-mini", High: "cursor-gpt-5.1-codex-mini-high", XHigh: "cursor-gpt-5.1-codex-mini-high"},
	{ID: "cursor-gpt-5.1-codex-max", Default: "cursor-gpt-5.1-codex-max-medium", None: "cursor-gpt-5.1-codex-max-low", Low: "cursor-gpt-5.1-codex-max-low", Medium: "cursor-gpt-5.1-codex-max-medium", High: "cursor-gpt-5.1-codex-max-high", XHigh: "cursor-gpt-5.1-codex-max-xhigh"},
	{ID: "cursor-gpt-5.2", Default: "cursor-gpt-5.2", None: "cursor-gpt-5.2-low", Low: "cursor-gpt-5.2-low", Medium: "cursor-gpt-5.2", High: "cursor-gpt-5.2-high", XHigh: "cursor-gpt-5.2-xhigh"},
	{ID: "cursor-gpt-5.2-codex", Default: "cursor-gpt-5.2-codex", None: "cursor-gpt-5.2-codex-low", Low: "cursor-gpt-5.2-codex-low", Medium: "cursor-gpt-5.2-codex", High: "cursor-gpt-5.2-codex-high", XHigh: "cursor-gpt-5.2-codex-xhigh"},
	{ID: "cursor-gpt-5.3-codex", Default: "cursor-gpt-5.3-codex", None: "cursor-gpt-5.3-codex-low", Low: "cursor-gpt-5.3-codex-low", Medium: "cursor-gpt-5.3-codex", High: "cursor-gpt-5.3-codex-high", XHigh: "cursor-gpt-5.3-codex-xhigh"},
	{ID: "cursor-gpt-5.3-codex-spark-preview", Default: "cursor-gpt-5.3-codex-spark-preview", None: "cursor-gpt-5.3-codex-spark-preview-low", Low: "cursor-gpt-5.3-codex-spark-preview-low", Medium: "cursor-gpt-5.3-codex-spark-preview", High: "cursor-gpt-5.3-codex-spark-preview-high", XHigh: "cursor-gpt-5.3-codex-spark-preview-xhigh"},
	{ID: "cursor-gpt-5.4", Default: "cursor-gpt-5.4-medium", None: "cursor-gpt-5.4-low", Low: "cursor-gpt-5.4-low", Medium: "cursor-gpt-5.4-medium", High: "cursor-gpt-5.4-high", XHigh: "cursor-gpt-5.4-xhigh"},
	{ID: "cursor-gpt-5.4-mini", Default: "cursor-gpt-5.4-mini-medium", None: "cursor-gpt-5.4-mini-none", Low: "cursor-gpt-5.4-mini-low", Medium: "cursor-gpt-5.4-mini-medium", High: "cursor-gpt-5.4-mini-high", XHigh: "cursor-gpt-5.4-mini-xhigh"},
	{ID: "cursor-gpt-5.4-nano", Default: "cursor-gpt-5.4-nano-medium", None: "cursor-gpt-5.4-nano-none", Low: "cursor-gpt-5.4-nano-low", Medium: "cursor-gpt-5.4-nano-medium", High: "cursor-gpt-5.4-nano-high", XHigh: "cursor-gpt-5.4-nano-xhigh"},
	{ID: "cursor-composer-1.5", Default: "cursor-composer-1.5", None: "cursor-composer-1.5", Low: "cursor-composer-1.5", Medium: "cursor-composer-1.5", High: "cursor-composer-1.5", XHigh: "cursor-composer-1.5"},
	{ID: "cursor-composer-2", Default: "cursor-composer-2", None: "cursor-composer-2", Low: "cursor-composer-2", Medium: "cursor-composer-2", High: "cursor-composer-2", XHigh: "cursor-composer-2"},
	{ID: "cursor-composer-2.5", Default: "cursor-composer-2.5", None: "cursor-composer-2.5", Low: "cursor-composer-2.5", Medium: "cursor-composer-2.5", High: "cursor-composer-2.5", XHigh: "cursor-composer-2.5"},
	{ID: "cursor-default", Default: "cursor-default", None: "cursor-default", Low: "cursor-default", Medium: "cursor-default", High: "cursor-default", XHigh: "cursor-default"},
	{ID: "cursor-gemini-3-flash", Default: "cursor-gemini-3-flash", None: "cursor-gemini-3-flash", Low: "cursor-gemini-3-flash", Medium: "cursor-gemini-3-flash", High: "cursor-gemini-3-flash", XHigh: "cursor-gemini-3-flash"},
	{ID: "cursor-gemini-3-pro", Default: "cursor-gemini-3-pro", None: "cursor-gemini-3-pro", Low: "cursor-gemini-3-pro", Medium: "cursor-gemini-3-pro", High: "cursor-gemini-3-pro", XHigh: "cursor-gemini-3-pro"},
	{ID: "cursor-gemini-3.1-pro", Default: "cursor-gemini-3.1-pro", None: "cursor-gemini-3.1-pro", Low: "cursor-gemini-3.1-pro", Medium: "cursor-gemini-3.1-pro", High: "cursor-gemini-3.1-pro", XHigh: "cursor-gemini-3.1-pro"},
	{ID: "cursor-gpt-5-mini", Default: "cursor-gpt-5-mini", None: "cursor-gpt-5-mini", Low: "cursor-gpt-5-mini", Medium: "cursor-gpt-5-mini", High: "cursor-gpt-5-mini", XHigh: "cursor-gpt-5-mini"},
	{ID: "cursor-kimi-k2.5", Default: "cursor-kimi-k2.5", None: "cursor-kimi-k2.5", Low: "cursor-kimi-k2.5", Medium: "cursor-kimi-k2.5", High: "cursor-kimi-k2.5", XHigh: "cursor-kimi-k2.5"},
}

var cursorCanonicalFamiliesByID = func() map[string]cursorCanonicalFamily {
	out := make(map[string]cursorCanonicalFamily, len(cursorCanonicalFamilies)*2)
	for _, family := range cursorCanonicalFamilies {
		out[family.ID] = family
		out[strings.TrimPrefix(family.ID, "cursor-")] = family
	}
	return out
}()

var cursorUpstreamIDsWithCursorPrefix = map[string]struct{}{
	"cursor-small": {},
}

func cursorNormalizeExecutionModelInOpenAIPayload(payload []byte, model string) []byte {
	model = strings.TrimSpace(model)
	if model == "" || len(payload) == 0 {
		return payload
	}
	updated, err := sjson.SetBytes(payload, "model", model)
	if err != nil {
		return payload
	}
	return updated
}

func cursorResolveRequestedModel(model string) cursorRequestedModel {
	modelID := cursorUpstreamModelID(model)
	if modelID == "auto" {
		modelID = "default"
	}
	if strings.HasPrefix(modelID, "composer-") && strings.HasSuffix(modelID, "-fast") {
		return cursorRequestedModel{
			ModelID: strings.TrimSuffix(modelID, "-fast"),
			Parameters: []cursorproto.ModelParameter{{
				ID:    "fast",
				Value: "true",
			}},
		}
	}
	return cursorRequestedModel{ModelID: modelID}
}

func cursorUpstreamModelID(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return model
	}
	if strings.HasPrefix(model, "cursor-cursor-") {
		return strings.TrimPrefix(model, "cursor-")
	}
	if strings.HasPrefix(model, "cursor-") {
		if _, keep := cursorUpstreamIDsWithCursorPrefix[model]; !keep {
			return strings.TrimPrefix(model, "cursor-")
		}
	}
	return model
}

func cursorPublicModelID(id string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		return id
	}
	if strings.HasPrefix(id, "cursor-") {
		return id
	}
	return "cursor-" + id
}

func resolveCursorModelTarget(model string, payload []byte) string {
	parsed := thinking.ParseSuffix(strings.TrimSpace(model))
	if family, ok := cursorCanonicalFamiliesByID[parsed.ModelName]; ok {
		if parsed.HasSuffix {
			if bucket, ok := cursorEffortBucketFromRawValue(parsed.RawSuffix); ok {
				return family.targetFor(bucket)
			}
		}
		if bucket, ok := cursorEffortBucketFromRequest(payload); ok {
			return family.targetFor(bucket)
		}
		return family.Default
	}
	return cursorPublicModelID(parsed.ModelName)
}

func (f cursorCanonicalFamily) targetFor(bucket cursorEffortBucket) string {
	switch bucket {
	case cursorEffortNone:
		if f.None != "" {
			return f.None
		}
	case cursorEffortLow:
		if f.Low != "" {
			return f.Low
		}
	case cursorEffortMedium:
		if f.Medium != "" {
			return f.Medium
		}
	case cursorEffortHigh:
		if f.High != "" {
			return f.High
		}
	case cursorEffortXHigh:
		if f.XHigh != "" {
			return f.XHigh
		}
	}
	return f.Default
}

func cursorEffortBucketFromRequest(body []byte) (cursorEffortBucket, bool) {
	for _, path := range []string{"reasoning_effort", "reasoning.effort", "thinking.effort", "thinking.level", "thinkingBudget", "thinking.budget", "generationConfig.thinkingConfig.thinkingBudget"} {
		value := gjson.GetBytes(body, path)
		if !value.Exists() {
			continue
		}
		if value.Type == gjson.Number {
			return cursorEffortBucketFromBudget(int(value.Int())), true
		}
		if bucket, ok := cursorEffortBucketFromRawValue(value.String()); ok {
			return bucket, true
		}
	}
	return "", false
}

func cursorEffortBucketFromRawValue(raw string) (cursorEffortBucket, bool) {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "":
		return "", false
	case "none":
		return cursorEffortNone, true
	case "minimal", "low", "small":
		return cursorEffortLow, true
	case "auto", "default", "medium", "mid":
		return cursorEffortMedium, true
	case "high":
		return cursorEffortHigh, true
	case "xhigh", "max", "large":
		return cursorEffortXHigh, true
	}
	budget, err := strconv.Atoi(value)
	if err != nil {
		return "", false
	}
	return cursorEffortBucketFromBudget(budget), true
}

func cursorEffortBucketFromBudget(budget int) cursorEffortBucket {
	switch {
	case budget == 0:
		return cursorEffortNone
	case budget < 0:
		return cursorEffortMedium
	case budget <= 1024:
		return cursorEffortLow
	case budget <= 8192:
		return cursorEffortMedium
	case budget <= 32768:
		return cursorEffortHigh
	default:
		return cursorEffortXHigh
	}
}

func cursorAddCanonicalFamilyAliases(models []*registry.ModelInfo) []*registry.ModelInfo {
	if len(models) == 0 {
		return models
	}
	byID := make(map[string]*registry.ModelInfo, len(models)+len(cursorCanonicalFamilies))
	out := make([]*registry.ModelInfo, 0, len(models)+len(cursorCanonicalFamilies))
	for _, model := range models {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		byID[model.ID] = model
		out = append(out, model)
	}
	for _, family := range cursorCanonicalFamilies {
		if family.ID == family.Default {
			continue
		}
		if _, exists := byID[family.ID]; exists {
			continue
		}
		source := byID[family.Default]
		if source == nil {
			continue
		}
		clone := *source
		clone.ID = family.ID
		clone.DisplayName = strings.TrimPrefix(family.ID, "cursor-")
		out = append(out, &clone)
		byID[family.ID] = &clone
	}
	return out
}

func cursorExpandModelAliases(models []*registry.ModelInfo) []*registry.ModelInfo {
	models = cursorAddCanonicalFamilyAliases(models)
	byID := make(map[string]*registry.ModelInfo, len(models)*2)
	out := make([]*registry.ModelInfo, 0, len(models)*2)
	for _, model := range models {
		if model == nil || strings.TrimSpace(model.ID) == "" {
			continue
		}
		if _, exists := byID[model.ID]; !exists {
			byID[model.ID] = model
			out = append(out, model)
		}
		if _, keep := cursorUpstreamIDsWithCursorPrefix[model.ID]; keep {
			continue
		}
		if strings.HasPrefix(model.ID, "cursor-") {
			rawID := strings.TrimPrefix(model.ID, "cursor-")
			if rawID != "" {
				if _, exists := byID[rawID]; !exists {
					clone := *model
					clone.ID = rawID
					clone.Name = rawID
					out = append(out, &clone)
					byID[rawID] = &clone
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// --- Model Discovery ---

// FetchCursorModels retrieves available models from Cursor's API.
func FetchCursorModels(ctx context.Context, auth *cliproxyauth.Auth, cfg *config.Config) []*registry.ModelInfo {
	accessToken := cursorAccessToken(auth)
	if accessToken == "" {
		return GetCursorFallbackModels()
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	// GetUsableModels is a unary RPC call (not streaming)
	// Send an empty protobuf request
	emptyReq := make([]byte, 0)

	h2Req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		cursorAPIURL+cursorModelsPath, bytes.NewReader(emptyReq))
	if err != nil {
		log.Debugf("cursor: failed to create models request: %v", err)
		return GetCursorFallbackModels()
	}

	h2Req.Header.Set("Content-Type", "application/proto")
	h2Req.Header.Set("Te", "trailers")
	h2Req.Header.Set("Authorization", "Bearer "+accessToken)
	h2Req.Header.Set("X-Ghost-Mode", "true")
	h2Req.Header.Set("X-Cursor-Client-Version", cursorClientVersionHeader())
	h2Req.Header.Set("X-Cursor-Client-Type", "cli")

	client := newH2Client()
	resp, err := client.Do(h2Req)
	if err != nil {
		log.Debugf("cursor: models request failed: %v", err)
		return GetCursorFallbackModels()
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Debugf("cursor: models request returned status %d", resp.StatusCode)
		return GetCursorFallbackModels()
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return GetCursorFallbackModels()
	}

	models := parseModelsResponse(body)
	if len(models) == 0 {
		return GetCursorFallbackModels()
	}
	return cursorExpandModelAliases(models)
}

func parseModelsResponse(data []byte) []*registry.ModelInfo {
	// Try stripping Connect framing first
	if len(data) >= cursorproto.ConnectFrameHeaderSize {
		_, payload, _, ok := cursorproto.ParseConnectFrame(data)
		if ok {
			data = payload
		}
	}

	// The response is a GetUsableModelsResponse protobuf.
	// We need to decode it manually - it contains a repeated "models" field.
	// Based on the TS code, the response has a `models` field (repeated) containing
	// model objects with modelId, displayName, thinkingDetails, etc.

	// For now, we'll try a simple decode approach
	var models []*registry.ModelInfo
	// Field 1 is likely "models" (repeated submessage)
	for len(data) > 0 {
		num, typ, n := consumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]

		if typ == 2 { // BytesType (submessage)
			val, n := consumeBytes(data)
			if n < 0 {
				break
			}
			data = data[n:]

			if num == 1 { // models field
				if m := parseModelEntry(val); m != nil {
					models = append(models, m)
				}
			}
		} else {
			n := consumeFieldValue(num, typ, data)
			if n < 0 {
				break
			}
			data = data[n:]
		}
	}

	return models
}

func parseModelEntry(data []byte) *registry.ModelInfo {
	var modelId, displayName string
	var hasThinking bool

	for len(data) > 0 {
		num, typ, n := consumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]

		switch typ {
		case 2: // BytesType
			val, n := consumeBytes(data)
			if n < 0 {
				return nil
			}
			data = data[n:]
			switch num {
			case 1: // modelId
				modelId = string(val)
			case 2: // thinkingDetails
				hasThinking = true
			case 3: // displayModelId (use as fallback)
				if displayName == "" {
					displayName = string(val)
				}
			case 4: // displayName
				displayName = string(val)
			case 5: // displayNameShort
				if displayName == "" {
					displayName = string(val)
				}
			}
		case 0: // VarintType
			_, n := consumeVarint(data)
			if n < 0 {
				return nil
			}
			data = data[n:]
		default:
			n := consumeFieldValue(num, typ, data)
			if n < 0 {
				return nil
			}
			data = data[n:]
		}
	}

	if modelId == "" {
		return nil
	}
	if displayName == "" {
		displayName = modelId
	}

	info := &registry.ModelInfo{
		ID:                  cursorPublicModelID(modelId),
		Object:              "model",
		Created:             time.Now().Unix(),
		OwnedBy:             "cursor",
		Type:                cursorAuthType,
		DisplayName:         displayName,
		Name:                modelId,
		ContextLength:       200000,
		MaxCompletionTokens: 64000,
	}
	if hasThinking {
		info.Thinking = &registry.ThinkingSupport{
			Max:            50000,
			DynamicAllowed: true,
		}
	}
	return info
}

// GetCursorFallbackModels returns hardcoded fallback models.
func GetCursorFallbackModels() []*registry.ModelInfo {
	now := time.Now().Unix()
	models := []*registry.ModelInfo{
		cursorFallbackModel(now, "cursor-default", "Default", 200000, 64000, true),
		cursorFallbackModel(now, "cursor-composer-1", "Composer 1", 200000, 64000, true),
		cursorFallbackModel(now, "cursor-composer-1.5", "Composer 1.5", 200000, 64000, true),
		cursorFallbackModel(now, "cursor-composer-2", "Composer 2", 200000, 64000, true),
		cursorFallbackModel(now, "cursor-composer-2.5", "Composer 2.5", 200000, 64000, true),
		cursorFallbackModel(now, "cursor-claude-4.6-opus-high", "Claude 4.6 Opus", 200000, 128000, true),
		cursorFallbackModel(now, "cursor-claude-4.6-sonnet-medium", "Claude 4.6 Sonnet", 200000, 64000, true),
		cursorFallbackModel(now, "cursor-claude-4.5-sonnet", "Claude 4.5 Sonnet", 200000, 64000, true),
		cursorFallbackModel(now, "cursor-gpt-5.4-medium", "GPT-5.4", 272000, 128000, true),
		cursorFallbackModel(now, "cursor-gpt-5.2", "GPT-5.2", 400000, 128000, true),
		cursorFallbackModel(now, "cursor-gpt-5.2-codex", "GPT-5.2 Codex", 400000, 128000, true),
		cursorFallbackModel(now, "cursor-gpt-5.3-codex", "GPT-5.3 Codex", 400000, 128000, true),
		cursorFallbackModel(now, "cursor-gpt-5.3-codex-spark-preview", "GPT-5.3 Codex Spark", 128000, 128000, true),
		cursorFallbackModel(now, "cursor-gemini-3.1-pro", "Gemini 3.1 Pro", 1000000, 64000, true),
		cursorFallbackModel(now, "cursor-grok-code-fast-1", "Grok Code Fast 1", 128000, 64000, false),
		cursorFallbackModel(now, "cursor-small", "Cursor Small", 200000, 64000, false),
	}
	return cursorExpandModelAliases(models)
}

func cursorFallbackModel(now int64, id, displayName string, contextLength, maxTokens int, thinkingSupport bool) *registry.ModelInfo {
	model := &registry.ModelInfo{
		ID:                  id,
		Object:              "model",
		Created:             now,
		OwnedBy:             "cursor",
		Type:                cursorAuthType,
		DisplayName:         displayName,
		Name:                cursorUpstreamModelID(id),
		ContextLength:       contextLength,
		MaxCompletionTokens: maxTokens,
	}
	if thinkingSupport {
		model.Thinking = &registry.ThinkingSupport{Max: 50000, DynamicAllowed: true}
	}
	return model
}

// Low-level protowire helpers (avoid importing protowire in executor)
func consumeTag(b []byte) (num int, typ int, n int) {
	v, n := consumeVarint(b)
	if n < 0 {
		return 0, 0, -1
	}
	return int(v >> 3), int(v & 7), n
}

func consumeVarint(b []byte) (uint64, int) {
	var val uint64
	for i := 0; i < len(b) && i < 10; i++ {
		val |= uint64(b[i]&0x7f) << (7 * i)
		if b[i]&0x80 == 0 {
			return val, i + 1
		}
	}
	return 0, -1
}

func consumeBytes(b []byte) ([]byte, int) {
	length, n := consumeVarint(b)
	if n < 0 || int(length) > len(b)-n {
		return nil, -1
	}
	return b[n : n+int(length)], n + int(length)
}

func consumeFieldValue(num, typ int, b []byte) int {
	switch typ {
	case 0: // Varint
		_, n := consumeVarint(b)
		return n
	case 1: // 64-bit
		if len(b) < 8 {
			return -1
		}
		return 8
	case 2: // Length-delimited
		_, n := consumeBytes(b)
		return n
	case 5: // 32-bit
		if len(b) < 4 {
			return -1
		}
		return 4
	default:
		return -1
	}
}
