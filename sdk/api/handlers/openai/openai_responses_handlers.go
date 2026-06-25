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
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	. "github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	responsesconverter "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/openai/openai/responses"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
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
}

func (f *responsesSSEFramer) WriteChunk(w io.Writer, chunk []byte) {
	if len(chunk) == 0 {
		return
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
	if len(f.pending) == 0 {
		return
	}
	if len(bytes.TrimSpace(f.pending)) == 0 {
		f.pending = f.pending[:0]
		return
	}
	if !responsesSSECanEmitWithoutDelimiter(f.pending) {
		f.pending = f.pending[:0]
		return
	}
	f.writeFrame(w, f.pending)
	f.pending = f.pending[:0]
}

func (f *responsesSSEFramer) writeFrame(w io.Writer, frame []byte) {
	writeResponsesSSEChunk(w, f.repairFrame(frame))
}

func (f *responsesSSEFramer) repairFrame(frame []byte) []byte {
	payload, ok := responsesSSEDataPayload(frame)
	if !ok || len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) || !json.Valid(payload) {
		return frame
	}

	switch gjson.GetBytes(payload, "type").String() {
	case "response.output_item.done":
		f.recordOutputItem(payload)
	case "response.completed":
		repaired := f.repairCompletedPayload(payload)
		if !bytes.Equal(repaired, payload) {
			return responsesSSEFrameWithData(frame, repaired)
		}
	}
	return frame
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
	if len(trimmed) == 0 || responsesSSENeedsMoreData(trimmed) || !responsesSSEHasField(trimmed, []byte("data:")) {
		return false
	}
	return responsesSSEDataLinesValid(trimmed)
}

func responsesSSEDataLinesValid(chunk []byte) bool {
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
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if len(data) == 0 || bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if !json.Valid(data) {
			return false
		}
	}
	return true
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
		BaseAPIHandler: apiHandlers,
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

	// Check if the client requested a streaming response.
	streamResult := gjson.GetBytes(rawJSON, "stream")
	stream := streamResult.Type == gjson.True

	modelName := gjson.GetBytes(rawJSON, "model").String()
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

	c.Header("Content-Type", "application/json")
	modelName := gjson.GetBytes(rawJSON, "model").String()
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)
	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "responses/compact")
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
	cliCtx = h.withDirectResponsesStateMode(cliCtx, modelName, rawJSON)
	stopKeepAlive := h.StartNonStreamingKeepAlive(c, cliCtx)

	resp, upstreamHeaders, errMsg := h.ExecuteWithAuthManager(cliCtx, h.HandlerType(), modelName, rawJSON, "")
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
	cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
	cliCtx = h.withDirectResponsesStateMode(cliCtx, modelName, rawJSON)

	setSSEHeaders := func() {
		c.Header("Content-Type", "text/event-stream")
		c.Header("Cache-Control", "no-cache")
		c.Header("Connection", "keep-alive")
		c.Header("Access-Control-Allow-Origin", "*")
	}
	framer := &responsesSSEFramer{}

	h.handleResponsesStreamBootstrap(
		c,
		flusher,
		cliCtx,
		cliCancel,
		func(ctx context.Context) responsesStreamBootstrapResult {
			data, headers, errs := h.ExecuteStreamWithAuthManager(ctx, h.HandlerType(), modelName, rawJSON, "")
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
				func(chunk []byte) { framer.WriteChunk(c.Writer, chunk) },
				func(data <-chan []byte, errs <-chan *interfaces.ErrorMessage) {
					h.forwardResponsesStream(c, flusher, func(err error) { cliCancel(err) }, data, errs, framer)
				},
			)
		},
	)
}

func (h *OpenAIResponsesAPIHandler) withDirectResponsesStateMode(ctx context.Context, modelName string, rawJSON []byte) context.Context {
	if !gjson.GetBytes(rawJSON, "previous_response_id").Exists() {
		return ctx
	}
	if h.responsesWebsocketResponsesStateUnsupportedForModel(modelName) {
		return ctx
	}
	return handlers.WithResponsesStateMode(ctx, cliproxyexecutor.ResponsesStateModeProbe)
}

func (h *OpenAIResponsesAPIHandler) handleResponsesStreamBootstrap(
	c *gin.Context,
	flusher http.Flusher,
	cliCtx context.Context,
	cliCancel func(...interface{}),
	start func(context.Context) responsesStreamBootstrapResult,
	setSSEHeaders func(),
	forward func(data <-chan []byte, headers http.Header, errs <-chan *interfaces.ErrorMessage, committed bool, deadline time.Time),
) {
	bootstrapCtx, bootstrapCancel := context.WithCancel(cliCtx)
	defer bootstrapCancel()

	bootstrapTimeout := handlers.StreamingBootstrapTimeout(h.Cfg)
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
			if committed {
				writeResponsesStreamErrorMessage(c.Writer, errMsg)
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
				h.writeResponsesBootstrapClosedError(c, flusher, cancel, committed)
				return
			}
			if !committed {
				committed = true
				graceC = nil
				setSSEHeaders()
				handlers.WriteUpstreamHeaders(c.Writer.Header(), upstreamHeaders)
			}
			if writeFirstChunk != nil {
				writeFirstChunk(chunk)
			}
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
	if committed {
		writeResponsesStreamError(c.Writer, http.StatusBadGateway, err.Error())
		flusher.Flush()
	} else {
		h.WriteErrorResponse(c, &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: err})
	}
	cancel(err)
}

func writeResponsesStreamErrorMessage(w io.Writer, errMsg *interfaces.ErrorMessage) {
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
				_, _ = c.Writer.Write(out)
				_, _ = c.Writer.Write([]byte("\n"))
			}
		},
		WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
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
			body := handlers.BuildErrorResponseBody(status, errText)
			_, _ = fmt.Fprintf(c.Writer, "\nevent: error\ndata: %s\n\n", string(body))
		},
		WriteDone: func() {
			_, _ = c.Writer.Write([]byte("\n"))
		},
	})
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesStream(c *gin.Context, flusher http.Flusher, cancel func(error), data <-chan []byte, errs <-chan *interfaces.ErrorMessage, framer *responsesSSEFramer) {
	if framer == nil {
		framer = &responsesSSEFramer{}
	}
	h.ForwardStream(c, flusher, cancel, data, errs, handlers.StreamForwardOptions{
		WriteChunk: func(chunk []byte) {
			framer.WriteChunk(c.Writer, chunk)
		},
		WriteTerminalError: func(errMsg *interfaces.ErrorMessage) {
			framer.Flush(c.Writer)
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
			chunk := handlers.BuildOpenAIResponsesStreamErrorChunk(status, errText, 0)
			_, _ = fmt.Fprintf(c.Writer, "\nevent: error\ndata: %s\n\n", string(chunk))
		},
		WriteDone: func() {
			framer.Flush(c.Writer)
			_, _ = c.Writer.Write([]byte("\n"))
		},
	})
}
