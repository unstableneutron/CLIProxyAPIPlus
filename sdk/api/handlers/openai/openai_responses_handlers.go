// Package openai provides HTTP handlers for OpenAIResponses API endpoints.
// This package implements the OpenAIResponses-compatible API interface, including model listing
// and chat completion functionality. It supports both streaming and non-streaming responses,
// and manages a pool of clients to interact with backend services.
// The handlers translate OpenAIResponses API requests to the appropriate backend format and
// convert responses back to OpenAIResponses-compatible format.
package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/client/codex/optimize-multi-agent-v2"
	. "github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	responsesconverter "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/openai/openai/responses"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func writeResponsesSSEChunk(w io.Writer, chunk []byte) {
	if w == nil || len(chunk) == 0 {
		return
	}
	if _, err := w.Write(chunk); err != nil {
		return
	}
	if bytes.HasSuffix(chunk, []byte("\n\n")) || bytes.HasSuffix(chunk, []byte("\r\n\r\n")) {
		return
	}
	suffix := []byte("\n\n")
	if bytes.HasSuffix(chunk, []byte("\r\n")) {
		suffix = []byte("\r\n")
	} else if bytes.HasSuffix(chunk, []byte("\n")) {
		suffix = []byte("\n")
	}
	if _, err := w.Write(suffix); err != nil {
		return
	}
}

type responsesSSEFramer struct {
	pending              []byte
	outputItems          map[int][]byte
	outputOrder          []int
	unindexedOutputItems [][]byte
	onCompleted          func([]byte)
	lastEvent            string
	terminalEvent        string
	terminalError        *interfaces.ErrorMessage
	failureEvent         string
	dataFrames           int
	context              *gin.Context
}

const responsesLastSequenceKey = "openai-responses-last-sequence"

// recordSequenceFromFrame remembers sequence numbers only after the SSE framer
// has reassembled a complete frame. Raw transport chunks may split JSON tokens.
func (f *responsesSSEFramer) recordSequenceFromFrame(frame []byte) {
	if f == nil || f.context == nil || len(frame) == 0 {
		return
	}
	payload, ok := responsesSSEDataPayload(frame)
	if !ok || !json.Valid(payload) {
		return
	}
	recordResponsesSequencePayload(f.context, payload)
}

func recordResponsesSequencePayload(c *gin.Context, payload []byte) {
	if c == nil || !json.Valid(payload) {
		return
	}
	last := int64(-1)
	if current, ok := c.Get(responsesLastSequenceKey); ok {
		if value, okValue := current.(int64); okValue {
			last = value
		}
	}
	sequence := gjson.GetBytes(payload, "sequence_number")
	if sequence.Exists() && sequence.Type == gjson.Number && sequence.Int() > last {
		last = sequence.Int()
	}

	if last >= 0 {
		c.Set(responsesLastSequenceKey, last)
	}
}

// recordResponsesSequenceFromConvertedOutput accepts only complete converter
// outputs, unlike native transport chunks which must first pass through the framer.
func recordResponsesSequenceFromConvertedOutput(c *gin.Context, output []byte) {
	payload, ok := responsesSSEDataPayload(output)
	if !ok {
		payload = bytes.TrimSpace(output)
	}
	recordResponsesSequencePayload(c, payload)
}

func nextResponsesSequence(c *gin.Context) int {
	if c == nil {
		return 0
	}
	if current, ok := c.Get(responsesLastSequenceKey); ok {
		if value, okValue := current.(int64); okValue && value >= 0 {
			return int(value + 1)
		}
	}
	return 0
}

func (f *responsesSSEFramer) WriteChunk(w io.Writer, chunk []byte) {
	if len(chunk) == 0 || f.terminalEvent != "" {
		return
	}
	if responsesSSEStartsNewDataFrame(f.pending, chunk) {
		f.writeFrame(w, f.pending)
		f.pending = f.pending[:0]
		if f.terminalEvent != "" {
			return
		}
	}
	if responsesSSENeedsLineBreak(f.pending, chunk) {
		f.pending = append(f.pending, '\n')
	}
	f.pending = append(f.pending, chunk...)
	for {
		frameLen := responsesSSEFrameLen(f.pending)
		if frameLen == 0 {
			break
		}
		f.writeFrame(w, f.pending[:frameLen])
		copy(f.pending, f.pending[frameLen:])
		f.pending = f.pending[:len(f.pending)-frameLen]
		if f.terminalEvent != "" {
			f.pending = f.pending[:0]
			return
		}
	}
	if len(bytes.TrimSpace(f.pending)) == 0 {
		f.pending = f.pending[:0]
		return
	}
	if len(f.pending) == 0 || !responsesSSECanEmitWithoutDelimiter(f.pending) {
		return
	}
	f.writeFrame(w, f.pending)
	f.pending = f.pending[:0]
}

func (f *responsesSSEFramer) Flush(w io.Writer) {
	if len(f.pending) == 0 || f.terminalEvent != "" {
		return
	}
	if len(bytes.TrimSpace(f.pending)) == 0 {
		f.pending = f.pending[:0]
		return
	}
	if !responsesSSECanFlushWithoutDelimiter(f.pending) {
		f.pending = f.pending[:0]
		return
	}
	f.writeFrame(w, f.pending)
	f.pending = f.pending[:0]
}

func (f *responsesSSEFramer) writeFrame(w io.Writer, frame []byte) {
	f.recordSequenceFromFrame(frame)
	writeResponsesSSEChunk(w, f.repairFrame(frame))
}

func (f *responsesSSEFramer) repairFrame(frame []byte) []byte {
	payload, ok := responsesSSEDataPayload(frame)
	if !ok || len(payload) == 0 {
		return frame
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		f.dataFrames++
		return frame
	}
	if !json.Valid(payload) {
		return frame
	}
	f.dataFrames++

	payloadType := gjson.GetBytes(payload, "type").String()
	if responsesSSEErrorEvent(payloadType) || responsesSSEPayloadHasError(payload) {
		if payloadType != "" {
			f.lastEvent = sanitizeResponsesStreamEventName(payloadType)
		}
		return f.repairErrorPayload(payload)
	}
	streamEvent := responsesSSEEventName(frame)
	eventType := payloadType
	if responsesSSETerminalEvent(streamEvent) {
		eventType = streamEvent
	} else if eventType == "" {
		eventType = streamEvent
	}
	if eventType != "" {
		f.lastEvent = sanitizeResponsesStreamEventName(eventType)
	}
	if responsesSSEErrorEvent(eventType) {
		return f.repairErrorPayload(payload)
	}
	if responsesSSETerminalEvent(eventType) {
		f.terminalEvent = eventType
	}

	switch eventType {
	case "response.output_item.done":
		f.recordOutputItem(payload)
	case "response.completed":
		repaired := f.repairCompletedPayload(payload)
		if f.onCompleted != nil {
			f.onCompleted(repaired)
		}
		if !bytes.Equal(repaired, payload) {
			return responsesSSEFrameWithData(frame, repaired)
		}
	}
	return frame
}

func responsesSSEPayloadErrorMessage(payload []byte) *interfaces.ErrorMessage {
	status := http.StatusBadGateway
	for _, path := range []string{"status", "status_code", "error.status", "error.status_code", "response.error.status", "response.error.status_code"} {
		candidate := int(gjson.GetBytes(payload, path).Int())
		if candidate >= http.StatusBadRequest && candidate <= 599 {
			status = candidate
			break
		}
	}
	return sanitizeResponsesStreamErrorMessage(&interfaces.ErrorMessage{StatusCode: status, Error: fmt.Errorf("%s", payload)})
}

func (f *responsesSSEFramer) repairErrorPayload(payload []byte) []byte {
	errMsg := responsesSSEPayloadErrorMessage(payload)
	status := errMsg.StatusCode
	f.terminalError = errMsg
	failureEvent := f.failureEvent
	if failureEvent != "response.failed" {
		failureEvent = "error"
	}
	f.terminalEvent = failureEvent
	errText := responsesStreamErrorText(errMsg, status)
	if failureEvent == "response.failed" {
		chunk := handlers.BuildOpenAIResponsesStreamFailedChunk(status, errText, nextResponsesSequence(f.context))
		return []byte(fmt.Sprintf("event: response.failed\ndata: %s\n\n", chunk))
	}
	chunk := handlers.BuildOpenAIResponsesStreamErrorChunk(status, errText, nextResponsesSequence(f.context))
	return []byte(fmt.Sprintf("event: error\ndata: %s\n\n", chunk))
}

func responsesSSEErrorEvent(eventType string) bool {
	switch eventType {
	case "response.failed", "response.error", "error":
		return true
	default:
		return false
	}
}

func responsesSSETerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed", "response.done", "response.error", "error":
		return true
	default:
		return false
	}
}

func responsesSSEPayloadHasError(payload []byte) bool {
	for _, path := range []string{"error", "response.error"} {
		result := gjson.GetBytes(payload, path)
		if result.Exists() && result.Type != gjson.Null {
			return true
		}
	}
	return gjson.GetBytes(payload, "code").Exists() && gjson.GetBytes(payload, "message").Exists()
}

func responsesSSEDataPayload(frame []byte) ([]byte, bool) {
	var payload []byte
	found := false
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		trimmed := bytes.TrimSpace(line)
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(trimmed[len("data:"):])
		if found {
			payload = append(payload, '\n')
		}
		payload = append(payload, data...)
		found = true
	}
	return payload, found
}

func responsesSSEFrameWithData(frame, payload []byte) []byte {
	var out bytes.Buffer
	for _, line := range bytes.Split(frame, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		out.Write(line)
		out.WriteByte('\n')
	}
	for _, line := range bytes.Split(payload, []byte("\n")) {
		out.WriteString("data: ")
		out.Write(line)
		out.WriteByte('\n')
	}
	out.WriteByte('\n')
	return out.Bytes()
}

func (f *responsesSSEFramer) recordOutputItem(payload []byte) {
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || !item.IsObject() || item.Get("type").String() == "" {
		return
	}

	if outputIndex := gjson.GetBytes(payload, "output_index"); outputIndex.Exists() {
		index := int(outputIndex.Int())
		if f.outputItems == nil {
			f.outputItems = make(map[int][]byte)
		}
		if _, exists := f.outputItems[index]; !exists {
			f.outputOrder = append(f.outputOrder, index)
		}
		f.outputItems[index] = append([]byte(nil), item.Raw...)
		return
	}

	f.unindexedOutputItems = append(f.unindexedOutputItems, append([]byte(nil), item.Raw...))
}

func (f *responsesSSEFramer) repairCompletedPayload(payload []byte) []byte {
	if len(f.outputOrder) == 0 && len(f.unindexedOutputItems) == 0 {
		return payload
	}
	output := gjson.GetBytes(payload, "response.output")
	if output.Exists() && (!output.IsArray() || len(output.Array()) > 0) {
		return payload
	}

	var outputJSON bytes.Buffer
	outputJSON.WriteByte('[')
	indexes := append([]int(nil), f.outputOrder...)
	sort.Ints(indexes)
	written := 0
	for _, index := range indexes {
		item, ok := f.outputItems[index]
		if !ok {
			continue
		}
		if written > 0 {
			outputJSON.WriteByte(',')
		}
		outputJSON.Write(item)
		written++
	}
	for _, item := range f.unindexedOutputItems {
		if written > 0 {
			outputJSON.WriteByte(',')
		}
		outputJSON.Write(item)
		written++
	}
	outputJSON.WriteByte(']')

	repaired, err := sjson.SetRawBytes(payload, "response.output", outputJSON.Bytes())
	if err != nil {
		return payload
	}
	return repaired
}

func responsesSSEFrameLen(chunk []byte) int {
	if len(chunk) == 0 {
		return 0
	}
	lf := bytes.Index(chunk, []byte("\n\n"))
	crlf := bytes.Index(chunk, []byte("\r\n\r\n"))
	switch {
	case lf < 0:
		if crlf < 0 {
			return 0
		}
		return crlf + 4
	case crlf < 0:
		return lf + 2
	case lf < crlf:
		return lf + 2
	default:
		return crlf + 4
	}
}

func responsesSSENeedsMoreData(chunk []byte) bool {
	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) == 0 {
		return false
	}
	return responsesSSEHasField(trimmed, []byte("event:")) && !responsesSSEHasField(trimmed, []byte("data:"))
}

func responsesSSEHasField(chunk []byte, prefix []byte) bool {
	s := chunk
	for len(s) > 0 {
		line := s
		if i := bytes.IndexByte(s, '\n'); i >= 0 {
			line = s[:i]
			s = s[i+1:]
		} else {
			s = nil
		}
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func responsesSSECanEmitWithoutDelimiter(chunk []byte) bool {
	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) == 0 || responsesSSENeedsMoreData(trimmed) ||
		!responsesSSEHasField(trimmed, []byte("event:")) || !responsesSSEHasField(trimmed, []byte("data:")) {
		return false
	}
	return responsesSSEDataLinesValid(trimmed)
}

func responsesSSECanFlushWithoutDelimiter(chunk []byte) bool {
	trimmed := bytes.TrimSpace(chunk)
	return len(trimmed) > 0 && responsesSSEHasField(trimmed, []byte("data:")) && responsesSSEDataLinesValid(trimmed)
}

func responsesSSEStartsNewDataFrame(pending, chunk []byte) bool {
	trimmedPending := bytes.TrimSpace(pending)
	if len(trimmedPending) == 0 || responsesSSEHasField(trimmedPending, []byte("event:")) ||
		!responsesSSEHasField(trimmedPending, []byte("data:")) || !responsesSSEDataLinesValid(trimmedPending) {
		return false
	}
	trimmedChunk := bytes.TrimLeft(chunk, " \t\r\n")
	return bytes.HasPrefix(trimmedChunk, []byte("data:"))
}

func responsesSSEEventName(frame []byte) string {
	for _, line := range bytes.Split(frame, []byte("\n")) {
		trimmed := bytes.TrimSpace(bytes.TrimRight(line, "\r"))
		if bytes.HasPrefix(trimmed, []byte("event:")) {
			return strings.TrimSpace(string(trimmed[len("event:"):]))
		}
	}
	return ""
}

func responsesSSEDataLinesValid(chunk []byte) bool {
	payload, found := responsesSSEDataPayload(chunk)
	if !found {
		return true
	}
	payload = bytes.TrimSpace(payload)
	return len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || json.Valid(payload)
}

const (
	responsesStreamBootstrapGrace     = 200 * time.Millisecond
	responsesStreamBootstrapHeartbeat = 15 * time.Second
)

type responsesStreamBootstrapResult struct {
	data            <-chan []byte
	upstreamHeaders http.Header
	errs            <-chan *interfaces.ErrorMessage
}

type directResponsesStateEntry struct {
	request            []byte
	output             []byte
	pendingToolCallIDs []string
	createdAt          time.Time
}

type directResponsesStateCache struct {
	mu      sync.Mutex
	entries map[string]directResponsesStateEntry
	order   []string
}

const (
	directResponsesStateCacheMaxEntries = 512
	directResponsesStateCacheTTL        = time.Hour
)

func newDirectResponsesStateCache() *directResponsesStateCache {
	return &directResponsesStateCache{entries: make(map[string]directResponsesStateEntry)}
}

func (c *directResponsesStateCache) get(responseID string) (directResponsesStateEntry, bool) {
	responseID = strings.TrimSpace(responseID)
	if c == nil || responseID == "" {
		return directResponsesStateEntry{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneLocked(time.Now())
	entry, ok := c.entries[responseID]
	if !ok {
		return directResponsesStateEntry{}, false
	}
	entry.request = bytes.Clone(entry.request)
	entry.output = bytes.Clone(entry.output)
	entry.pendingToolCallIDs = append([]string(nil), entry.pendingToolCallIDs...)
	return entry, true
}

func (c *directResponsesStateCache) put(responseID string, entry directResponsesStateEntry) {
	responseID = strings.TrimSpace(responseID)
	if c == nil || responseID == "" || len(entry.request) == 0 {
		return
	}
	if len(entry.output) == 0 {
		entry.output = []byte("[]")
	}
	entry.request = bytes.Clone(entry.request)
	entry.output = bytes.Clone(entry.output)
	entry.pendingToolCallIDs = append([]string(nil), entry.pendingToolCallIDs...)
	if entry.createdAt.IsZero() {
		entry.createdAt = time.Now()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if _, exists := c.entries[responseID]; !exists {
		c.order = append(c.order, responseID)
	}
	c.entries[responseID] = entry
	c.pruneLocked(time.Now())
}

func (c *directResponsesStateCache) pruneLocked(now time.Time) {
	if c == nil {
		return
	}
	keep := c.order[:0]
	for _, id := range c.order {
		entry, ok := c.entries[id]
		if !ok {
			continue
		}
		if !entry.createdAt.IsZero() && now.Sub(entry.createdAt) > directResponsesStateCacheTTL {
			delete(c.entries, id)
			continue
		}
		keep = append(keep, id)
	}
	c.order = keep
	for len(c.order) > directResponsesStateCacheMaxEntries {
		oldest := c.order[0]
		copy(c.order, c.order[1:])
		c.order = c.order[:len(c.order)-1]
		delete(c.entries, oldest)
	}
}

func responsesSSENeedsLineBreak(pending, chunk []byte) bool {
	if len(pending) == 0 || len(chunk) == 0 {
		return false
	}
	if bytes.HasSuffix(pending, []byte("\n")) || bytes.HasSuffix(pending, []byte("\r")) {
		return false
	}
	if chunk[0] == '\n' || chunk[0] == '\r' {
		return false
	}
	trimmed := bytes.TrimLeft(chunk, " \t")
	if len(trimmed) == 0 {
		return false
	}
	for _, prefix := range [][]byte{[]byte("data:"), []byte("event:"), []byte("id:"), []byte("retry:"), []byte(":")} {
		if bytes.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

// OpenAIResponsesAPIHandler contains the handlers for OpenAIResponses API endpoints.
// It holds a pool of clients to interact with the backend service.
type OpenAIResponsesAPIHandler struct {
	*handlers.BaseAPIHandler
	directResponsesState     *directResponsesStateCache
	responsesTurnCoordinator *responsesTurnCoordinator
}

// NewOpenAIResponsesAPIHandler creates a new OpenAIResponses API handlers instance.
// It takes an BaseAPIHandler instance as input and returns an OpenAIResponsesAPIHandler.
//
// Parameters:
//   - apiHandlers: The base API handlers instance
//
// Returns:
//   - *OpenAIResponsesAPIHandler: A new OpenAIResponses API handlers instance
func NewOpenAIResponsesAPIHandler(apiHandlers *handlers.BaseAPIHandler) *OpenAIResponsesAPIHandler {
	return &OpenAIResponsesAPIHandler{
		BaseAPIHandler:           apiHandlers,
		directResponsesState:     newDirectResponsesStateCache(),
		responsesTurnCoordinator: newResponsesTurnCoordinator(),
	}
}

// HandlerType returns the identifier for this handler implementation.
func (h *OpenAIResponsesAPIHandler) HandlerType() string {
	return OpenaiResponse
}

// Models returns the OpenAIResponses-compatible model metadata supported by this handler.
func (h *OpenAIResponsesAPIHandler) Models() []map[string]any {
	// Get dynamic models from the global registry
	modelRegistry := registry.GetGlobalRegistry()
	return modelRegistry.GetAvailableModels("openai")
}

// OpenAIResponsesModels handles the /v1/models endpoint.
// It returns a list of available AI models with their capabilities
// and specifications in OpenAIResponses-compatible format.
func (h *OpenAIResponsesAPIHandler) OpenAIResponsesModels(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"object": "list",
		"data":   h.Models(),
	})
}

func (h *OpenAIResponsesAPIHandler) prepareCodexMultiAgentV2Tools(c *gin.Context, payload []byte) []byte {
	if h == nil || h.Cfg == nil {
		return payload
	}

	requestCtx := context.Background()
	if c != nil && c.Request != nil {
		requestCtx = c.Request.Context()
	}
	requestCtx = context.WithValue(requestCtx, "gin", c)

	var requestHeaders http.Header
	if c != nil && c.Request != nil {
		requestHeaders = c.Request.Header
	}
	homeEnabled := h.AuthManager != nil && h.AuthManager.HomeEnabled()
	updated, prepared := multiagentv2.PrepareCodexMultiAgentV2Tools(
		requestCtx,
		requestHeaders,
		payload,
		h.Cfg.CodexOptimizeMultiAgentV2,
		homeEnabled,
	)
	if prepared && c != nil {
		c.Set(multiagentv2.CodexMultiAgentV2ToolsPreparedContextKey, true)
	}
	return updated
}

// Responses handles the /v1/responses endpoint.
// It determines whether the request is for a streaming or non-streaming response
// and calls the appropriate handler based on the model provider.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
func (h *OpenAIResponsesAPIHandler) Responses(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	// If data retrieval fails, return a 400 Bad Request error.
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}
	rawJSON = normalizeCodexFastSpeedTierRequest(rawJSON)
	updatedJSON, errMsg := handlers.ApplyForceModelPrefixHeader(c, rawJSON)
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		return
	}
	rawJSON = updatedJSON

	rawJSON = h.prepareCodexMultiAgentV2Tools(c, rawJSON)

	// Check if the client requested a streaming response.
	streamResult := gjson.GetBytes(rawJSON, "stream")
	stream := streamResult.Type == gjson.True

	modelName := gjson.GetBytes(rawJSON, "model").String()
	if responsesTurnMetadataIsCompaction(c) {
		finishCompaction := h.beginResponsesTurnCompaction(c)
		defer finishCompaction()
	}
	if overrideEndpoint, ok := resolveEndpointOverride(modelName, openAIResponsesEndpoint); ok && overrideEndpoint == openAIChatEndpoint {
		chatJSON := responsesconverter.ConvertOpenAIResponsesRequestToOpenAIChatCompletions(modelName, rawJSON, stream)
		stream = gjson.GetBytes(chatJSON, "stream").Bool()
		if stream {
			h.handleStreamingResponseViaChat(c, rawJSON, chatJSON)
		} else {
			h.handleNonStreamingResponseViaChat(c, rawJSON, chatJSON)
		}
		return
	}

	if stream {
		h.handleStreamingResponse(c, rawJSON)
	} else {
		h.handleNonStreamingResponse(c, rawJSON)
	}

}

func (h *OpenAIResponsesAPIHandler) Compact(c *gin.Context) {
	rawJSON, err := handlers.ReadRequestBody(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: fmt.Sprintf("Invalid request: %v", err),
				Type:    "invalid_request_error",
			},
		})
		return
	}
	rawJSON = normalizeCodexFastSpeedTierRequest(rawJSON)
	updatedJSON, errMsg := handlers.ApplyForceModelPrefixHeader(c, rawJSON)
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		return
	}
	rawJSON = updatedJSON

	streamResult := gjson.GetBytes(rawJSON, "stream")
	if streamResult.Type == gjson.True {
		c.JSON(http.StatusBadRequest, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported for compact responses",
				Type:    "invalid_request_error",
			},
		})
		return
	}
	if streamResult.Exists() {
		if updated, err := sjson.DeleteBytes(rawJSON, "stream"); err == nil {
			rawJSON = updated
		}
	}
	finishCompaction := h.beginResponsesTurnCompaction(c)
	defer finishCompaction()
	rawJSON = sanitizeOpenAIResponsesCompactRequest(rawJSON)

	c.Header("Content-Type", "application/json")
	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.executeResponsesWithReplayRetries(responsesReplayExecution{
		ctx:       cliCtx,
		modelName: modelName,
		payload:   rawJSON,
		alt:       "responses/compact",
	})
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

// handleNonStreamingResponse handles non-streaming chat completion responses
// for Gemini models. It selects a client from the pool, sends the request, and
// aggregates the response before sending it back to the client in OpenAIResponses format.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAIResponses-compatible request
func (h *OpenAIResponsesAPIHandler) handleNonStreamingResponse(c *gin.Context, rawJSON []byte) {
	c.Header("Content-Type", "application/json")

	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	resp, upstreamHeaders, errMsg := h.executeResponsesWithReplayRetries(responsesReplayExecution{
		ctx:       cliCtx,
		modelName: modelName,
		payload:   rawJSON,
	})
	stopKeepAlive()
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	_, _ = c.Writer.Write(resp)
	cliCancel()
}

func (h *OpenAIResponsesAPIHandler) handleNonStreamingResponseViaChat(c *gin.Context, originalResponsesJSON, chatJSON []byte) {
	c.Header("Content-Type", "application/json")

	modelName := gjson.GetBytes(chatJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, OpenAI, modelName, chatJSON, "")
	if errMsg != nil {
		h.WriteErrorResponse(c, errMsg)
		cliCancel(errMsg.Error)
		return
	}
	handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
	var param any
	converted := responsesconverter.ConvertOpenAIChatCompletionsResponseToOpenAIResponsesNonStream(cliCtx, modelName, originalResponsesJSON, originalResponsesJSON, resp, &param)
	if len(converted) == 0 {
		h.WriteErrorResponse(c, &interfaces.ErrorMessage{
			StatusCode: http.StatusInternalServerError,
			Error:      fmt.Errorf("failed to convert chat completion response to responses format"),
		})
		cliCancel(fmt.Errorf("response conversion failed"))
		return
	}
	_, _ = c.Writer.Write(converted)
	cliCancel()
}

type directResponsesStreamStateTracker struct {
	cache   *directResponsesStateCache
	request []byte
}

func (t directResponsesStreamStateTracker) Complete(payload []byte) {
	if t.cache == nil || len(t.request) == 0 || len(payload) == 0 || !json.Valid(payload) {
		return
	}
	response := gjson.GetBytes(payload, "response")
	if !response.Exists() || !response.IsObject() {
		return
	}
	responseID := strings.TrimSpace(response.Get("id").String())
	if responseID == "" {
		return
	}
	outputRaw := response.Get("output").Raw
	if strings.TrimSpace(outputRaw) == "" {
		outputRaw = "[]"
	}
	output := []byte(outputRaw)
	t.cache.put(responseID, directResponsesStateEntry{
		request:            t.request,
		output:             output,
		pendingToolCallIDs: responsesPendingToolCallIDsFromOutput(output),
		createdAt:          time.Now(),
	})
}

func (h *OpenAIResponsesAPIHandler) prepareDirectResponsesStreamState(modelName string, rawJSON []byte) ([]byte, directResponsesStreamStateTracker) {
	normalized := normalizeDirectResponsesStreamStateRequest(rawJSON)
	tracker := directResponsesStreamStateTracker{cache: h.directResponsesStateCache()}
	previousResponseID := strings.TrimSpace(gjson.GetBytes(normalized, "previous_response_id").String())
	if previousResponseID == "" {
		tracker.request = bytes.Clone(normalized)
		return normalized, tracker
	}

	entry, ok := tracker.cache.get(previousResponseID)
	if !ok {
		stripped, _ := sjson.DeleteBytes(normalized, "previous_response_id")
		tracker.request = bytes.Clone(stripped)
		return stripped, tracker
	}

	replay, _, errMsg := normalizeResponseSubsequentRequest(
		normalized,
		entry.request,
		entry.output,
		previousResponseID,
		entry.pendingToolCallIDs,
		false,
		false,
	)
	if errMsg != nil || len(replay) == 0 {
		stripped, _ := sjson.DeleteBytes(normalized, "previous_response_id")
		tracker.request = bytes.Clone(stripped)
		return stripped, tracker
	}
	replay = repairResponsesWebsocketToolCalls("", replay)
	replay = dedupeResponsesWebsocketInputItemsByID(replay)
	tracker.request = bytes.Clone(replay)
	return replay, tracker
}

func (h *OpenAIResponsesAPIHandler) directResponsesStateCache() *directResponsesStateCache {
	if h == nil {
		return nil
	}
	if h.directResponsesState == nil {
		h.directResponsesState = newDirectResponsesStateCache()
	}
	return h.directResponsesState
}

func normalizeDirectResponsesStreamStateRequest(rawJSON []byte) []byte {
	normalized := bytes.Clone(rawJSON)
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	if !gjson.GetBytes(normalized, "input").Exists() {
		normalized, _ = sjson.SetRawBytes(normalized, "input", []byte("[]"))
	}
	normalized = stripUnsupportedResponsesWebsocketInputItemMetadata(normalized)
	return normalized
}

// handleStreamingResponse handles streaming responses for Gemini models.
// It establishes a streaming connection with the backend service and forwards
// the response chunks to the client in real-time using Server-Sent Events.
//
// Parameters:
//   - c: The Gin context containing the HTTP request and response
//   - rawJSON: The raw JSON bytes of the OpenAIResponses-compatible request
func (h *OpenAIResponsesAPIHandler) handleStreamingResponse(c *gin.Context, rawJSON []byte) {
	// Get the http.Flusher interface to manually flush the response.
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	// New core execution path
	rawJSON = normalizeCodexFastSpeedTierRequest(rawJSON)
	modelName := gjson.GetBytes(rawJSON, "model").String()
	rawJSON, stateTracker := h.prepareDirectResponsesStreamState(modelName, rawJSON)
	replayPayload := bytes.Clone(rawJSON)
	replayState := responsesReplayPlannerState{}
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
	}
	failureEvent := "error"
	if isCodexResponsesClientRequest(c) {
		failureEvent = "response.failed"
	}
	framer := &responsesSSEFramer{onCompleted: stateTracker.Complete, failureEvent: failureEvent, context: c}
	bootstrapTimeout := handlers.StreamingBootstrapTimeout(h.Cfg)
	if isResponsesCompactionTriggerRequest(rawJSON) {
		bootstrapTimeout = 0
	}
	startResponsesStream := func(ctx context.Context) responsesStreamBootstrapResult {
		data, headers, errs := h.ExecuteStreamWithAuthManager(ctx, h.HandlerType(), modelName, replayPayload, "")
		return responsesStreamBootstrapResult{data: data, upstreamHeaders: headers, errs: errs}
	}
	retryResponsesStream := func(errMsg *interfaces.ErrorMessage) (responsesStreamBootstrapResult, bool) {
		nextPayload, ok := replayState.nextPayload(rawJSON, errMsg)
		if !ok {
			return responsesStreamBootstrapResult{}, false
		}
		replayPayload = nextPayload
		return startResponsesStream(cliCtx), true
	}

	h.handleResponsesStreamBootstrap(
		c,
		flusher,
		cliCtx,
		cliCancel,
		bootstrapTimeout,
		startResponsesStream,
		setSSEHeaders,
		func(data <-chan []byte, headers http.Header, errs <-chan *interfaces.ErrorMessage, committed bool, deadline time.Time) {
			h.forwardStreamAfterBootstrap(
				c,
				flusher,
				func(err error) { cliCancel(err) },
				data,
				headers,
				errs,
				setSSEHeaders,
				committed,
				deadline,
				retryResponsesStream,
				func(chunk []byte) {
					framer.WriteChunk(c.Writer, chunk)
					framer.Flush(c.Writer)
				},
				func(data <-chan []byte, errs <-chan *interfaces.ErrorMessage) {
					h.forwardResponsesStream(c, flusher, func(err error) { cliCancel(err) }, data, errs, framer)
				},
			)
		},
	)
}

func (h *OpenAIResponsesAPIHandler) handleResponsesStreamBootstrap(
	c *gin.Context,
	flusher http.Flusher,
	cliCtx context.Context,
	cliCancel func(...interface{}),
	bootstrapTimeout time.Duration,
	start func(context.Context) responsesStreamBootstrapResult,
	setSSEHeaders func(),
	forward func(data <-chan []byte, headers http.Header, errs <-chan *interfaces.ErrorMessage, committed bool, deadline time.Time),
) {
	bootstrapCtx, bootstrapCancel := context.WithCancel(cliCtx)
	defer bootstrapCancel()

	bootstrapDeadline := time.Time{}
	var timeoutC <-chan time.Time
	var timeoutTimer *time.Timer
	if bootstrapTimeout > 0 {
		bootstrapDeadline = time.Now().Add(bootstrapTimeout)
		timeoutTimer = time.NewTimer(bootstrapTimeout)
		timeoutC = timeoutTimer.C
		defer timeoutTimer.Stop()
	}

	bootstrapCh := make(chan responsesStreamBootstrapResult, 1)
	go func() {
		result := start(bootstrapCtx)
		select {
		case bootstrapCh <- result:
		case <-bootstrapCtx.Done():
		}
	}()

	graceTimer := time.NewTimer(responsesStreamBootstrapGrace)
	defer graceTimer.Stop()

	select {
	case <-c.Request.Context().Done():
		cliCancel(c.Request.Context().Err())
		return
	case result := <-bootstrapCh:
		forward(result.data, result.upstreamHeaders, result.errs, false, bootstrapDeadline)
		return
	case <-timeoutC:
		setSSEHeaders()
		_, _ = c.Writer.Write([]byte(": stream-start\n\n"))
		msg := fmt.Errorf("upstream stream bootstrap timed out after %s", bootstrapTimeout)
		writeResponsesStreamError(c.Writer, http.StatusGatewayTimeout, msg.Error())
		flusher.Flush()
		bootstrapCancel()
		cliCancel(msg)
		return
	case <-graceTimer.C:
	}

	setSSEHeaders()
	_, _ = c.Writer.Write([]byte(": stream-start\n\n"))
	flusher.Flush()

	heartbeat := time.NewTicker(responsesStreamBootstrapHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-c.Request.Context().Done():
			cliCancel(c.Request.Context().Err())
			return
		case result := <-bootstrapCh:
			forward(result.data, result.upstreamHeaders, result.errs, true, bootstrapDeadline)
			return
		case <-timeoutC:
			msg := fmt.Errorf("upstream stream bootstrap timed out after %s", bootstrapTimeout)
			writeResponsesStreamError(c.Writer, http.StatusGatewayTimeout, msg.Error())
			flusher.Flush()
			bootstrapCancel()
			cliCancel(msg)
			return
		case <-heartbeat.C:
			_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
			flusher.Flush()
		}
	}
}

func (h *OpenAIResponsesAPIHandler) forwardStreamAfterBootstrap(
	c *gin.Context,
	flusher http.Flusher,
	cancel func(error),
	data <-chan []byte,
	upstreamHeaders http.Header,
	errs <-chan *interfaces.ErrorMessage,
	setSSEHeaders func(),
	committed bool,
	bootstrapDeadline time.Time,
	retryBeforeCommit func(*interfaces.ErrorMessage) (responsesStreamBootstrapResult, bool),
	writeFirstChunk func([]byte),
	forwardRest func(<-chan []byte, <-chan *interfaces.ErrorMessage),
) {
	var graceC <-chan time.Time
	var graceTimer *time.Timer
	if !committed {
		graceTimer = time.NewTimer(responsesStreamBootstrapGrace)
		graceC = graceTimer.C
		defer graceTimer.Stop()
	}

	var timeoutC <-chan time.Time
	var timeoutTimer *time.Timer
	if !bootstrapDeadline.IsZero() {
		timeoutTimer = time.NewTimer(time.Until(bootstrapDeadline))
		timeoutC = timeoutTimer.C
		defer timeoutTimer.Stop()
	}

	var heartbeat *time.Ticker
	var heartbeatC <-chan time.Time
	if committed {
		heartbeat = time.NewTicker(responsesStreamBootstrapHeartbeat)
		heartbeatC = heartbeat.C
	}
	defer func() {
		if heartbeat != nil {
			heartbeat.Stop()
		}
	}()

	commitStream := func() {
		if committed {
			return
		}
		committed = true
		graceC = nil
		setSSEHeaders()
		_, _ = c.Writer.Write([]byte(": stream-start\n\n"))
		flusher.Flush()
		heartbeat = time.NewTicker(responsesStreamBootstrapHeartbeat)
		heartbeatC = heartbeat.C
	}

	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			return
		case <-graceC:
			commitStream()
		case <-timeoutC:
			commitStream()
			msg := fmt.Errorf("upstream stream bootstrap timed out before first chunk")
			writeResponsesStreamError(c.Writer, http.StatusGatewayTimeout, msg.Error())
			flusher.Flush()
			cancel(msg)
			return
		case <-heartbeatC:
			_, _ = c.Writer.Write([]byte(": keep-alive\n\n"))
			flusher.Flush()
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				if data == nil {
					h.writeResponsesBootstrapClosedError(c, flusher, cancel, committed)
					return
				}
				continue
			}
			if !committed && retryBeforeCommit != nil {
				if retryResult, retried := retryBeforeCommit(errMsg); retried {
					data = retryResult.data
					upstreamHeaders = retryResult.upstreamHeaders
					errs = retryResult.errs
					continue
				}
			}
			errMsg = sanitizeResponsesInitialErrorMessage(errMsg)
			if committed {
				h.logResponsesStreamError(c, nil, errMsg)
				writeResponsesStreamErrorMessage(c, c.Writer, errMsg)
				flusher.Flush()
			} else {
				h.WriteErrorResponse(c, errMsg)
			}
			if errMsg != nil {
				cancel(errMsg.Error)
			} else {
				cancel(nil)
			}
			return
		case chunk, ok := <-data:
			if !ok {
				data = nil
				if errs == nil {
					h.writeResponsesBootstrapClosedError(c, flusher, cancel, committed)
					return
				}
				continue
			}
			if !committed {
				setSSEHeaders()
				handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			}
			sizeBefore := c.Writer.Size()
			if writeFirstChunk != nil {
				writeFirstChunk(chunk)
			}
			if !committed && c.Writer.Size() == sizeBefore {
				continue
			}
			committed = true
			graceC = nil
			flusher.Flush()
			if forwardRest != nil {
				forwardRest(data, errs)
			}
			return
		}
	}
}
func (h *OpenAIResponsesAPIHandler) writeResponsesBootstrapClosedError(c *gin.Context, flusher http.Flusher, cancel func(error), committed bool) {
	err := fmt.Errorf("upstream stream closed before first chunk")
	errMsg := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err}
	if committed {
		h.logResponsesStreamError(c, nil, errMsg)
		writeResponsesStreamErrorMessage(c, c.Writer, errMsg)
		flusher.Flush()
	} else {
		h.WriteErrorResponse(c, errMsg)
	}
	cancel(err)
}

func writeResponsesStreamErrorMessage(c *gin.Context, w io.Writer, errMsg *interfaces.ErrorMessage) {
	if errMsg == nil {
		return
	}
	status := http.StatusInternalServerError
	if errMsg.StatusCode > 0 {
		status = errMsg.StatusCode
	}
	errText := http.StatusText(status)
	if errMsg.Error != nil && errMsg.Error.Error() != "" {
		errText = errMsg.Error.Error()
	}
	if isCodexResponsesClientRequest(c) {
		chunk := handlers.BuildOpenAIResponsesStreamFailedChunk(status, errText, 0)
		_, _ = fmt.Fprintf(w, "\nevent: response.failed\ndata: %s\n\n", string(chunk))
		return
	}
	writeResponsesStreamError(w, status, errText)
}

func writeResponsesStreamError(w io.Writer, status int, errText string) {
	chunk := handlers.BuildOpenAIResponsesStreamErrorChunk(status, errText, 0)
	_, _ = fmt.Fprintf(w, "\nevent: error\ndata: %s\n\n", string(chunk))
}

func (h *OpenAIResponsesAPIHandler) handleStreamingResponseViaChat(c *gin.Context, originalResponsesJSON, chatJSON []byte) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, handlers.ErrorResponse{
			Error: handlers.ErrorDetail{
				Message: "Streaming not supported",
				Type:    "server_error",
			},
		})
		return
	}

	modelName := gjson.GetBytes(chatJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	var param any

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
	}

	h.handleResponsesStreamBootstrap(
		c,
		flusher,
		cliCtx,
		cliCancel,
		handlers.StreamingBootstrapTimeout(h.Cfg),
		func(ctx context.Context) responsesStreamBootstrapResult {
			data, headers, errs := h.ExecuteStreamWithAuthManager(ctx, OpenAI, modelName, chatJSON, "")
			return responsesStreamBootstrapResult{data: data, upstreamHeaders: headers, errs: errs}
		},
		setSSEHeaders,
		func(data <-chan []byte, headers http.Header, errs <-chan *interfaces.ErrorMessage, committed bool, deadline time.Time) {
			h.forwardStreamAfterBootstrap(
				c,
				flusher,
				func(err error) { cliCancel(err) },
				data,
				headers,
				errs,
				setSSEHeaders,
				committed,
				deadline,
				nil,
				func(chunk []byte) {
					writeChatAsResponsesChunk(c, cliCtx, modelName, originalResponsesJSON, chunk, &param)
				},
				func(data <-chan []byte, errs <-chan *interfaces.ErrorMessage) {
					h.forwardChatAsResponsesStream(c, flusher, func(err error) { cliCancel(err) }, data, errs, cliCtx, modelName, originalResponsesJSON, &param)
				},
			)
		},
	)
}

func writeChatAsResponsesChunk(c *gin.Context, ctx context.Context, modelName string, originalResponsesJSON, chunk []byte, param *any) {
	outputs := responsesconverter.ConvertOpenAIChatCompletionsResponseToOpenAIResponses(ctx, modelName, originalResponsesJSON, originalResponsesJSON, chunk, param)
	for _, out := range outputs {
		if len(out) == 0 {
			continue
		}
		if bytes.HasPrefix(out, []byte("event:")) {
			_, _ = c.Writer.Write([]byte("\n"))
		}
		recordResponsesSequenceFromConvertedOutput(c, out)
		_, _ = c.Writer.Write(out)
		_, _ = c.Writer.Write([]byte("\n"))
	}
}

func (h *OpenAIResponsesAPIHandler) forwardChatAsResponsesStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, ctx context.Context, modelName string, originalResponsesJSON []byte, param *any) {
	h.ForwardStream(c, flusher, cancel, data, errs, handlers.StreamForwardOptions{
		WriteChunk: func(chunk []byte) {
			outputs := responsesconverter.ConvertOpenAIChatCompletionsResponseToOpenAIResponses(ctx, modelName, originalResponsesJSON, originalResponsesJSON, chunk, param)
			for _, out := range outputs {
				if len(out) == 0 {
					continue
				}
				if bytes.HasPrefix(out, []byte("event:")) {
					_, _ = c.Writer.Write([]byte("\n"))
				}
				recordResponsesSequenceFromConvertedOutput(c, out)
				_, _ = c.Writer.Write(out)
				_, _ = c.Writer.Write([]byte("\n"))
			}
		},
		WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
			writeResponsesTerminalError(c, errMsg)
		},
		WriteDone: func() {
			_, _ = c.Writer.Write([]byte("\n"))
		},
	})
}

// isCodexResponsesClientRequest limits the alternate terminal event to official Codex clients.
func isCodexResponsesClientRequest(c *gin.Context) bool {
	if c == nil || c.Request == nil {
		return false
	}
	if multiagentv2.IsCodexClientUserAgent(c.GetHeader("User-Agent")) {
		return true
	}

	switch originator := strings.ToLower(strings.TrimSpace(c.GetHeader("Originator"))); originator {
	case "codex desktop", "codex-tui", "codex_cli_rs":
		return true
	default:
		return strings.HasPrefix(originator, "codex desktop/") || strings.HasPrefix(originator, "codex-tui/") || strings.HasPrefix(originator, "codex_cli_rs/")
	}
}

const (
	responsesStreamErrorMessageLimit = 2048
	responsesStreamErrorFieldLimit   = 256
)

var (
	responsesStreamSensitiveValuePattern = regexp.MustCompile(`(?i)((?:"?(?:api[_-]?key|access[_-]?token|token|authorization|secret)"?)\s*[=:]\s*"?)([^\s"&,;}]+)`)
	responsesStreamBearerPattern         = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
)

func truncateResponsesStreamErrorText(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

func redactResponsesStreamErrorText(text string) string {
	text = responsesStreamSensitiveValuePattern.ReplaceAllString(text, `${1}[REDACTED]`)
	return responsesStreamBearerPattern.ReplaceAllString(text, "Bearer [REDACTED]")
}

func sanitizeResponsesStreamEventName(eventName string) string {
	return truncateResponsesStreamErrorText(redactResponsesStreamErrorText(strings.TrimSpace(eventName)), responsesStreamErrorFieldLimit)
}

func responsesStreamErrorText(errMsg *interfaces.ErrorMessage, status int) string {
	text := http.StatusText(status)
	if errMsg != nil && errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
		text = strings.TrimSpace(errMsg.Error.Error())
	}
	if !json.Valid([]byte(text)) {
		return truncateResponsesStreamErrorText(redactResponsesStreamErrorText(text), responsesStreamErrorMessageLimit)
	}

	root := gjson.Parse(text)
	errorNode := root.Get("error")
	if !errorNode.Exists() || !errorNode.IsObject() {
		errorNode = root.Get("response.error")
	}
	if errorNode.Exists() && errorNode.IsObject() {
		safe := []byte(`{"error":{}}`)
		copied := false
		for _, field := range []string{"type", "code", "message", "param"} {
			value := errorNode.Get(field)
			if !value.Exists() || value.Type == gjson.Null {
				continue
			}
			limit := responsesStreamErrorFieldLimit
			if field == "message" {
				limit = responsesStreamErrorMessageLimit
			}
			safe, _ = sjson.SetBytes(safe, "error."+field, truncateResponsesStreamErrorText(redactResponsesStreamErrorText(value.String()), limit))
			copied = true
		}
		if copied {
			return string(safe)
		}
	}

	safe := []byte(`{"type":"error"}`)
	copied := false
	for _, field := range []string{"code", "message", "param"} {
		value := root.Get(field)
		if !value.Exists() || value.Type == gjson.Null {
			continue
		}
		limit := responsesStreamErrorFieldLimit
		if field == "message" {
			limit = responsesStreamErrorMessageLimit
		}
		safe, _ = sjson.SetBytes(safe, field, truncateResponsesStreamErrorText(redactResponsesStreamErrorText(value.String()), limit))
		copied = true
	}
	if copied {
		return string(safe)
	}
	return http.StatusText(status)
}

type responsesStreamSanitizedError struct {
	message string
	cause   error
}

func (e *responsesStreamSanitizedError) Error() string { return e.message }
func (e *responsesStreamSanitizedError) Unwrap() error { return e.cause }

func sanitizeResponsesInitialErrorMessage(errMsg *interfaces.ErrorMessage) *interfaces.ErrorMessage {
	if errMsg != nil && errMsg.DirectResponse {
		return errMsg
	}
	return sanitizeResponsesStreamErrorMessage(errMsg)
}

func sanitizeResponsesStreamErrorMessage(errMsg *interfaces.ErrorMessage) *interfaces.ErrorMessage {
	if errMsg == nil {
		return nil
	}
	status := errMsg.StatusCode
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusInternalServerError
	}
	safe := *errMsg
	safe.StatusCode = status
	safe.Error = &responsesStreamSanitizedError{message: responsesStreamErrorText(errMsg, status), cause: errMsg.Error}
	safe.DirectResponse = false
	safe.Body = nil
	return &safe
}

func (h *OpenAIResponsesAPIHandler) logResponsesStreamError(c *gin.Context, framer *responsesSSEFramer, errMsg *interfaces.ErrorMessage) {
	if errMsg == nil {
		return
	}
	status := errMsg.StatusCode
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusInternalServerError
	}
	lastEvent := "none"
	if framer != nil && framer.lastEvent != "" {
		lastEvent = framer.lastEvent
	}
	errText := responsesStreamErrorText(errMsg, status)
	h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), &interfaces.ErrorMessage{
		StatusCode: status,
		Error:      fmt.Errorf("responses stream terminated after %s: %s", lastEvent, errText),
	})
}

func writeResponsesTerminalError(c *gin.Context, errMsg *interfaces.ErrorMessage) {
	errMsg = sanitizeResponsesStreamErrorMessage(errMsg)
	if errMsg == nil {
		return
	}
	status := http.StatusInternalServerError
	if errMsg.StatusCode > 0 {
		status = errMsg.StatusCode
	}
	errText := http.StatusText(status)
	if errMsg.Error != nil && errMsg.Error.Error() != "" {
		errText = errMsg.Error.Error()
	}
	if isCodexResponsesClientRequest(c) {
		chunk := handlers.BuildOpenAIResponsesStreamFailedChunk(status, errText, nextResponsesSequence(c))
		_, _ = fmt.Fprintf(c.Writer, "\nevent: response.failed\ndata: %s\n\n", string(chunk))
		return
	}
	chunk := handlers.BuildOpenAIResponsesStreamErrorChunk(status, errText, nextResponsesSequence(c))
	_, _ = fmt.Fprintf(c.Writer, "\nevent: error\ndata: %s\n\n", string(chunk))
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, framer *responsesSSEFramer) {
	if framer == nil {
		framer = &responsesSSEFramer{context: c}
	} else if framer.context == nil {
		framer.context = c
	}
	if isCodexResponsesClientRequest(c) {
		framer.failureEvent = "response.failed"
	} else {
		framer.failureEvent = "error"
	}
	writeTerminalError := func(errMsg *interfaces.ErrorMessage) {
		framer.Flush(c.Writer)
		if errMsg == nil {
			return
		}
		h.logResponsesStreamError(c, framer, errMsg)
		if framer.terminalEvent != "" {
			return
		}
		writeResponsesTerminalError(c, errMsg)
	}

	h.ForwardStream(c, flusher, cancel, data, errs, handlers.StreamForwardOptions{
		NormalizeTerminalError: sanitizeResponsesStreamErrorMessage,
		WriteChunk: func(chunk []byte) {
			framer.WriteChunk(c.Writer, chunk)
		},
		ChunkError: func() *interfaces.ErrorMessage {
			if framer.terminalError != nil {
				h.logResponsesStreamError(c, framer, framer.terminalError)
			}
			return framer.terminalError
		},
		WriteTerminalError: writeTerminalError,
		CloseError: func() *interfaces.ErrorMessage {
			framer.Flush(c.Writer)
			if framer.terminalError != nil {
				return framer.terminalError
			}
			if framer.terminalEvent != "" {
				return nil
			}
			lastEvent := framer.lastEvent
			if lastEvent == "" {
				lastEvent = "none"
			}
			return &interfaces.ErrorMessage{
				StatusCode: http.StatusBadGateway,
				Error:      fmt.Errorf("upstream stream closed before a terminal event (last event: %s)", lastEvent),
			}
		},
		WriteDone: func() {
			framer.Flush(c.Writer)
			_, _ = c.Writer.Write([]byte("\n"))
		},
	})
}
