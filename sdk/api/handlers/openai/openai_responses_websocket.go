package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	requestlogging "github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai/responsesreplay"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var errResponsesWebsocketTurnCompacted = errors.New("responses websocket main turn superseded by compaction")

const (
	wsRequestTypeCreate  = "response.create"
	wsRequestTypeAppend  = "response.append"
	wsEventTypeError     = "error"
	wsEventTypeCompleted = "response.completed"
	wsEventTypeDone      = "response.done"
	wsDoneMarker         = "[DONE]"
	wsTurnStateHeader    = "x-codex-turn-state"
	wsTimelineBodyKey    = "WEBSOCKET_TIMELINE_OVERRIDE"
)

var responsesWebsocketUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

type websocketTimelineAppender interface {
	Append(eventType string, payload []byte, timestamp time.Time)
}

type responsesWebsocketStateSupport int

const (
	responsesWebsocketStateUnknown responsesWebsocketStateSupport = iota
	responsesWebsocketStateSupported
	responsesWebsocketStateUnsupported
)

const responsesStateAuthCapabilityKey = "responses_state"
const responsesStateAuthModelsKey = "responses_state_models"

type websocketTimelineLog struct {
	enabled bool
	source  *requestlogging.FileBodySource
	builder *strings.Builder

	currentPart       io.WriteCloser
	currentPartHasLog bool
}

func newWebsocketTimelineLog(enabled bool, source *requestlogging.FileBodySource) *websocketTimelineLog {
	if !enabled {
		return &websocketTimelineLog{}
	}
	if source == nil {
		return newInMemoryWebsocketTimelineLog()
	}
	return &websocketTimelineLog{
		enabled: true,
		source:  source,
	}
}

func newInMemoryWebsocketTimelineLog() *websocketTimelineLog {
	return &websocketTimelineLog{
		enabled: true,
		builder: &strings.Builder{},
	}
}

func websocketTimelineSourceFromContext(c *gin.Context) *requestlogging.FileBodySource {
	if c == nil {
		return nil
	}
	value, exists := c.Get(requestlogging.WebsocketTimelineSourceContextKey)
	if !exists {
		return nil
	}
	source, ok := value.(*requestlogging.FileBodySource)
	if !ok {
		return nil
	}
	return source
}

func (l *websocketTimelineLog) BeginRequest() {
	if l == nil || !l.enabled || l.source == nil {
		return
	}
	l.closeCurrentPart()
	part, errCreate := l.source.CreatePart("request")
	if errCreate != nil {
		log.WithError(errCreate).Warn("failed to create websocket request detail log")
		return
	}
	l.currentPart = part
	l.currentPartHasLog = false
}

func (l *websocketTimelineLog) Append(eventType string, payload []byte, timestamp time.Time) {
	if l == nil || !l.enabled {
		return
	}
	data := formatWebsocketTimelineEvent(eventType, payload, timestamp)
	if len(data) == 0 {
		return
	}
	if l.source != nil {
		if l.currentPart == nil {
			l.BeginRequest()
		}
		if l.currentPart == nil {
			return
		}
		if errWrite := writeWebsocketTimelinePart(l.currentPart, data, l.currentPartHasLog); errWrite != nil {
			log.WithError(errWrite).Warn("failed to write websocket request detail log")
			return
		}
		l.currentPartHasLog = true
		return
	}
	if l.builder != nil {
		writeWebsocketTimelineBuilder(l.builder, data)
	}
}

func (l *websocketTimelineLog) SetContext(c *gin.Context) {
	if l == nil || !l.enabled {
		return
	}
	l.closeCurrentPart()
	if l.source != nil {
		if l.source.HasPayload() {
			c.Set(requestlogging.WebsocketTimelineSourceContextKey, l.source)
			return
		}
		if errCleanup := l.source.Cleanup(); errCleanup != nil {
			log.WithError(errCleanup).Warn("failed to clean up empty websocket timeline log parts")
		}
	}
	if l.builder != nil {
		setWebsocketTimelineBody(c, l.builder.String())
	}
}

func (l *websocketTimelineLog) String() string {
	if l == nil || !l.enabled {
		return ""
	}
	l.closeCurrentPart()
	if l.source != nil {
		data, errRead := l.source.Bytes()
		if errRead != nil {
			return ""
		}
		return string(data)
	}
	if l.builder == nil {
		return ""
	}
	return l.builder.String()
}

func (l *websocketTimelineLog) closeCurrentPart() {
	if l == nil || l.currentPart == nil {
		return
	}
	if errClose := l.currentPart.Close(); errClose != nil {
		log.WithError(errClose).Warn("failed to close websocket request detail log")
	}
	l.currentPart = nil
	l.currentPartHasLog = false
}

func writeWebsocketTimelinePart(w io.Writer, data []byte, prependNewline bool) error {
	if w == nil || len(data) == 0 {
		return nil
	}
	if prependNewline {
		if _, errWrite := io.WriteString(w, "\n"); errWrite != nil {
			return errWrite
		}
	}
	_, errWrite := w.Write(data)
	return errWrite
}

func writeWebsocketTimelineBuilder(builder *strings.Builder, data []byte) {
	if builder == nil || len(data) == 0 {
		return
	}
	if builder.Len() > 0 {
		builder.WriteString("\n")
	}
	builder.Write(data)
}

// ResponsesWebsocket handles websocket requests for /v1/responses.
// It accepts `response.create` and `response.append` requests and streams
// response events back as JSON websocket text messages.
func (h *OpenAIResponsesAPIHandler) ResponsesWebsocket(c *gin.Context) {
	conn, err := responsesWebsocketUpgrader.Upgrade(c.Writer, c.Request, websocketUpgradeHeaders(c.Request))
	if err != nil {
		return
	}
	passthroughSessionID := uuid.NewString()
	downstreamSessionKey := websocketDownstreamSessionKey(c.Request)
	retainResponsesWebsocketToolCaches(downstreamSessionKey)
	clientIP := websocketClientAddress(c)
	log.Infof("responses websocket: client connected id=%s remote=%s", passthroughSessionID, clientIP)

	requestLogEnabled := h != nil && h.Cfg != nil && h.Cfg.RequestLog
	wsTimelineLog := newWebsocketTimelineLog(requestLogEnabled, websocketTimelineSourceFromContext(c))
	turnKey, turnMetadata, hasTurnKey := responsesTurnCoordinationKeyFromContext(c)
	coordinateMainTurn := hasTurnKey && turnMetadata.IsMainTurn()

	wsDone := make(chan struct{})
	defer close(wsDone)

	var downstreamCloseOnce sync.Once
	closeDownstream := func() {
		downstreamCloseOnce.Do(func() {
			if errClose := conn.Close(); errClose != nil {
				log.Warnf("responses websocket: close connection error: %v", errClose)
			}
		})
	}

	var upstreamDisconnectMu sync.Mutex
	forwardingUpstream := false
	closeAfterForward := false
	beginUpstreamForward := func() {
		upstreamDisconnectMu.Lock()
		forwardingUpstream = true
		upstreamDisconnectMu.Unlock()
	}
	clearPendingUpstreamDisconnectClose := func() {
		upstreamDisconnectMu.Lock()
		closeAfterForward = false
		upstreamDisconnectMu.Unlock()
	}
	endUpstreamForward := func() {
		upstreamDisconnectMu.Lock()
		shouldClose := closeAfterForward
		forwardingUpstream = false
		closeAfterForward = false
		upstreamDisconnectMu.Unlock()
		if shouldClose {
			closeDownstream()
		}
	}
	requestCloseForUpstreamDisconnect := func() {
		upstreamDisconnectMu.Lock()
		if forwardingUpstream {
			closeAfterForward = true
			upstreamDisconnectMu.Unlock()
			return
		}
		upstreamDisconnectMu.Unlock()
		closeDownstream()
	}

	if h != nil && h.AuthManager != nil {
		type upstreamDisconnectSubscriber interface {
			UpstreamDisconnectChan(sessionID string) <-chan error
		}
		for _, provider := range []string{"codex", "xai"} {
			exec, ok := h.AuthManager.Executor(provider)
			if !ok || exec == nil {
				continue
			}
			if subscriber, ok := exec.(upstreamDisconnectSubscriber); ok && subscriber != nil {
				disconnectCh := subscriber.UpstreamDisconnectChan(passthroughSessionID)
				if disconnectCh != nil {
					go func() {
						select {
						case <-wsDone:
							return
						case <-disconnectCh:
							requestCloseForUpstreamDisconnect()
						}
					}()
				}
			}
		}
	}

	var wsTerminateErr error
	defer func() {
		releaseResponsesWebsocketToolCaches(downstreamSessionKey)
		if wsTerminateErr != nil {
			appendWebsocketTimelineDisconnect(wsTimelineLog, wsTerminateErr, time.Now())
			// log.Infof("responses websocket: session closing id=%s reason=%v", passthroughSessionID, wsTerminateErr)
		} else {
			log.Infof("responses websocket: session closing id=%s", passthroughSessionID)
		}
		if h != nil && h.AuthManager != nil {
			h.AuthManager.CloseExecutionSession(passthroughSessionID)
			log.Infof("responses websocket: upstream execution session closed id=%s", passthroughSessionID)
		}
		wsTimelineLog.SetContext(c)
		closeDownstream()
	}()

	var lastRequest []byte
	lastResponseOutput := []byte("[]")
	lastResponseID := ""
	var lastResponsePendingToolCallIDs []string
	var previousCheckpoint responsesWebsocketCheckpoint
	pinnedAuthID := ""
	passthroughModelName := ""
	sessionAuthByID := func(authID string) (*coreauth.Auth, bool) {
		if h == nil || h.AuthManager == nil {
			return nil, false
		}
		if auth, ok := h.AuthManager.GetExecutionSessionAuthByID(passthroughSessionID, authID); ok {
			return auth, true
		}
		return h.AuthManager.GetByID(authID)
	}
	forceTranscriptReplayNextRequest := false
	responsesStateByRoute := make(map[string]responsesWebsocketStateSupport)
	responsesStateForRoute := func(authID string, modelName string) responsesWebsocketStateSupport {
		key := responsesWebsocketStateRouteKey(authID, modelName)
		if key == "" {
			return responsesWebsocketStateUnknown
		}
		return responsesStateByRoute[key]
	}
	markResponsesStateForRoute := func(authID string, modelName string, support responsesWebsocketStateSupport) {
		key := responsesWebsocketStateRouteKey(authID, modelName)
		if key == "" {
			return
		}
		responsesStateByRoute[key] = support
	}

	for {
		msgType, payload, errReadMessage := conn.ReadMessage()
		if errReadMessage != nil {
			wsTerminateErr = errReadMessage
			if websocket.IsCloseError(errReadMessage, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				log.Infof("responses websocket: client disconnected id=%s error=%v", passthroughSessionID, errReadMessage)
			} else {
				// log.Warnf("responses websocket: read message failed id=%s error=%v", passthroughSessionID, errReadMessage)
			}
			return
		}
		if msgType != websocket.TextMessage && msgType != websocket.BinaryMessage {
			continue
		}
		// log.Infof(
		// 	"responses websocket: downstream_in id=%s type=%d event=%s payload=%s",
		// 	passthroughSessionID,
		// 	msgType,
		// 	websocketPayloadEventType(payload),
		// 	websocketPayloadPreview(payload),
		// )
		wsTimelineLog.BeginRequest()
		wsTimelineLog.Append("request", payload, time.Now())

		updatedPayload, prefixErrMsg := handlers.ApplyForceModelPrefixHeader(c, payload)
		if prefixErrMsg != nil {
			h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), prefixErrMsg)
			markAPIResponseTimestamp(c)
			errorPayload, errWrite := writeResponsesWebsocketError(conn, wsTimelineLog, prefixErrMsg)
			log.Infof(
				"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				passthroughSessionID,
				websocket.TextMessage,
				websocketPayloadEventType(errorPayload),
				websocketPayloadPreview(errorPayload),
			)
			if errWrite != nil {
				log.Warnf(
					"responses websocket: downstream_out write failed id=%s event=%s error=%v",
					passthroughSessionID,
					websocketPayloadEventType(errorPayload),
					errWrite,
				)
				return
			}
			continue
		}
		payload = updatedPayload

		requestModelName := strings.TrimSpace(gjson.GetBytes(payload, "model").String())
		if requestModelName == "" {
			requestModelName = passthroughModelName
		}
		if requestModelName == "" {
			requestModelName = strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		}
		logicalPreviousResponseID := strings.TrimSpace(gjson.GetBytes(payload, "previous_response_id").String())
		hasPreviousResponseCandidate := responsesWebsocketCanProbeResponsesState(logicalPreviousResponseID, lastResponseID)
		useUpstreamWebsocketPassthrough := h.responsesWebsocketUsesUpstreamWebsocketPassthrough(requestModelName)
		allowIncrementalInputWithPreviousResponseID := false
		allowCompactionReplayBypass := false
		dynamicResponsesStateProbe := false
		if pinnedAuthID != "" {
			if pinnedAuth, ok := sessionAuthByID(pinnedAuthID); ok && pinnedAuth != nil {
				if !useUpstreamWebsocketPassthrough && h.responsesWebsocketAuthUsesUpstreamWebsocketPassthrough(pinnedAuth) {
					useUpstreamWebsocketPassthrough = true
				}
				if useUpstreamWebsocketPassthrough {
					allowIncrementalInputWithPreviousResponseID = responsesWebsocketAuthSupportsIncrementalInput(pinnedAuth)
				}
				allowCompactionReplayBypass = responsesWebsocketAuthSupportsCompactionReplay(pinnedAuth)
				if !useUpstreamWebsocketPassthrough &&
					hasPreviousResponseCandidate &&
					!allowIncrementalInputWithPreviousResponseID &&
					responsesWebsocketAuthCanProbeResponsesStateForModel(pinnedAuth, requestModelName) {
					allowIncrementalInputWithPreviousResponseID = responsesStateForRoute(pinnedAuthID, requestModelName) != responsesWebsocketStateUnsupported
					dynamicResponsesStateProbe = allowIncrementalInputWithPreviousResponseID
				}
			}
		} else {
			if useUpstreamWebsocketPassthrough {
				allowIncrementalInputWithPreviousResponseID = h.websocketUpstreamSupportsIncrementalInputForModel(requestModelName)
			}
			allowCompactionReplayBypass = h.websocketUpstreamSupportsCompactionReplayForModel(requestModelName)
			if !useUpstreamWebsocketPassthrough &&
				hasPreviousResponseCandidate &&
				!allowIncrementalInputWithPreviousResponseID &&
				h.responsesWebsocketCanProbeResponsesStateForModel(requestModelName) {
				allowIncrementalInputWithPreviousResponseID = true
				dynamicResponsesStateProbe = true
			}
		}
		if forceTranscriptReplayNextRequest {
			allowIncrementalInputWithPreviousResponseID = false
			dynamicResponsesStateProbe = false
		}

		var requestJSON []byte
		var updatedLastRequest []byte
		var fallbackRequestJSON []byte
		var errMsg *interfaces.ErrorMessage
		if useUpstreamWebsocketPassthrough {
			requestJSON, errMsg = normalizeResponsesWebsocketPassthroughRequest(payload, requestModelName)
			if errMsg == nil {
				stateRequestJSON, stateLastRequest, stateErrMsg := normalizeResponsesWebsocketRequestWithIncrementalState(
					payload,
					lastRequest,
					lastResponseOutput,
					lastResponseID,
					lastResponsePendingToolCallIDs,
					false,
					allowCompactionReplayBypass,
				)
				if stateErrMsg == nil && len(stateRequestJSON) > 0 {
					stateRequestJSON = repairResponsesWebsocketToolCalls(downstreamSessionKey, stateRequestJSON)
					stateRequestJSON = dedupeResponsesWebsocketInputItemsByID(stateRequestJSON)
					updatedLastRequest = bytes.Clone(stateRequestJSON)
					if len(stateLastRequest) > 0 && !bytes.Equal(stateLastRequest, stateRequestJSON) {
						updatedLastRequest = bytes.Clone(stateRequestJSON)
					}
					if gjson.GetBytes(requestJSON, "previous_response_id").Exists() {
						fallbackRequestJSON = bytes.Clone(stateRequestJSON)
					}
					if forceTranscriptReplayNextRequest {
						requestJSON = bytes.Clone(stateRequestJSON)
						fallbackRequestJSON = nil
					}
				}
			}
		} else {
			requestJSON, updatedLastRequest, errMsg = normalizeResponsesWebsocketRequestWithIncrementalState(
				payload,
				lastRequest,
				lastResponseOutput,
				lastResponseID,
				lastResponsePendingToolCallIDs,
				allowIncrementalInputWithPreviousResponseID,
				allowCompactionReplayBypass,
			)
			if errMsg == nil && allowIncrementalInputWithPreviousResponseID && gjson.GetBytes(requestJSON, "previous_response_id").Exists() {
				var fallbackErrMsg *interfaces.ErrorMessage
				fallbackRequestJSON, _, fallbackErrMsg = normalizeResponsesWebsocketRequestWithIncrementalState(
					payload,
					lastRequest,
					lastResponseOutput,
					lastResponseID,
					lastResponsePendingToolCallIDs,
					false,
					allowCompactionReplayBypass,
				)
				if fallbackErrMsg != nil {
					fallbackRequestJSON = nil
				}
			}
		}
		if errMsg != nil {
			h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
			markAPIResponseTimestamp(c)
			errorPayload, errWrite := writeResponsesWebsocketError(conn, wsTimelineLog, errMsg)
			log.Infof(
				"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				passthroughSessionID,
				websocket.TextMessage,
				websocketPayloadEventType(errorPayload),
				websocketPayloadPreview(errorPayload),
			)
			if errWrite != nil {
				log.Warnf(
					"responses websocket: downstream_out write failed id=%s event=%s error=%v",
					passthroughSessionID,
					websocketPayloadEventType(errorPayload),
					errWrite,
				)
				return
			}
			continue
		}
		logicalPreviousResponseID = responsesWebsocketEffectivePreviousResponseID(logicalPreviousResponseID, requestJSON)
		if logicalPreviousResponseID == "" {
			lastCompletedResponseID := strings.TrimSpace(lastResponseID)
			if lastCompletedResponseID != "" && !responsesWebsocketSyntheticPrewarmResponseID(lastCompletedResponseID) {
				logicalPreviousResponseID = lastCompletedResponseID
			}
		}
		if shouldHandleResponsesWebsocketPrewarmLocally(payload, lastRequest) {
			if updated, errDelete := sjson.DeleteBytes(requestJSON, "generate"); errDelete == nil {
				requestJSON = updated
			}
			if updated, errDelete := sjson.DeleteBytes(updatedLastRequest, "generate"); errDelete == nil {
				updatedLastRequest = updated
			}
			lastRequest = updatedLastRequest
			lastResponseOutput = []byte("[]")
			lastResponseID = ""
			lastResponsePendingToolCallIDs = nil
			forceTranscriptReplayNextRequest = true
			if errWrite := writeResponsesWebsocketSyntheticPrewarm(c, conn, requestJSON, wsTimelineLog, passthroughSessionID); errWrite != nil {
				wsTerminateErr = errWrite
				return
			}
			continue
		}

		previousLastRequest := bytes.Clone(lastRequest)
		previousLastResponseOutput := bytes.Clone(lastResponseOutput)
		previousLastResponseID := lastResponseID
		previousLastResponsePendingToolCallIDs := append([]string(nil), lastResponsePendingToolCallIDs...)
		previousCheckpointBeforeRequest := previousCheckpoint.Clone()
		forcedTranscriptReplay := forceTranscriptReplayNextRequest
		requestJSON = repairResponsesWebsocketToolCalls(downstreamSessionKey, requestJSON)
		requestJSON = dedupeResponsesWebsocketInputItemsByID(requestJSON)
		if len(fallbackRequestJSON) > 0 {
			fallbackRequestJSON = repairResponsesWebsocketToolCalls(downstreamSessionKey, fallbackRequestJSON)
			fallbackRequestJSON = dedupeResponsesWebsocketInputItemsByID(fallbackRequestJSON)
			updatedLastRequest = bytes.Clone(fallbackRequestJSON)
		} else if len(updatedLastRequest) == 0 {
			updatedLastRequest = bytes.Clone(requestJSON)
		} else if !useUpstreamWebsocketPassthrough {
			updatedLastRequest = bytes.Clone(requestJSON)
		}
		rollbackRequestJSON := responsesWebsocketRollbackRequest(fallbackRequestJSON, previousCheckpoint)
		replayRetryBaseJSON := requestJSON
		if len(fallbackRequestJSON) > 0 {
			replayRetryBaseJSON = fallbackRequestJSON
		}
		encryptedContentRetryRequestJSON := []byte(nil)
		if sanitized, changed := stripResponsesWebsocketReasoningEncryptedContent(replayRetryBaseJSON); changed {
			encryptedContentRetryRequestJSON = sanitized
		}
		providerIdentifierRetryRequestJSON := []byte(nil)
		if sanitized, changed := responsesreplay.Render(replayRetryBaseJSON, responsesreplay.AttemptWithoutProviderIdentifiers); changed {
			providerIdentifierRetryRequestJSON = sanitized
		}
		portableReplayRequestJSON := []byte(nil)
		if sanitized, changed := sanitizeResponsesWebsocketPortableReplay(replayRetryBaseJSON); changed {
			portableReplayRequestJSON = sanitized
		}

		if useUpstreamWebsocketPassthrough {
			if modelName := strings.TrimSpace(gjson.GetBytes(requestJSON, "model").String()); modelName != "" {
				passthroughModelName = modelName
			}
			if forcedTranscriptReplay {
				forceTranscriptReplayNextRequest = false
			}
		} else {
			lastRequest = updatedLastRequest
			if forcedTranscriptReplay {
				forceTranscriptReplayNextRequest = false
			}
		}
		if useUpstreamWebsocketPassthrough {
			lastRequest = updatedLastRequest
		}

		runUpstreamRequest := func(upstreamPayload []byte, interceptPreviousResponseNotFound bool, interceptInvalidEncryptedContent bool, interceptProviderItemNotFound bool) responsesWebsocketUpstreamAttempt {
			modelName := gjson.GetBytes(upstreamPayload, "model").String()
			cliCtx, cliCancel := h.GetContextWithCancel(h, c, context.Background())
			turnCanceledByCompaction := func() bool { return false }
			if coordinateMainTurn {
				finishMainTurn := func() {}
				cliCtx, finishMainTurn, turnCanceledByCompaction = h.beginResponsesMainTurn(cliCtx, turnKey)
				defer finishMainTurn()
			}
			cliCtx = cliproxyexecutor.WithDownstreamWebsocket(cliCtx)
			cliCtx = handlers.WithExecutionSessionID(cliCtx, passthroughSessionID)
			if !useUpstreamWebsocketPassthrough {
				stateMode := cliproxyexecutor.ResponsesStateModeReplay
				if gjson.GetBytes(upstreamPayload, "previous_response_id").Exists() {
					stateMode = cliproxyexecutor.ResponsesStateModeProbe
				}
				cliCtx = handlers.WithResponsesStateMode(cliCtx, stateMode)
			}
			selectedAuthID := strings.TrimSpace(pinnedAuthID)
			if pinnedAuthID != "" {
				cliCtx = handlers.WithPinnedAuthID(cliCtx, pinnedAuthID)
			} else {
				cliCtx = handlers.WithSelectedAuthIDCallback(cliCtx, func(authID string) {
					authID = strings.TrimSpace(authID)
					if authID == "" || h == nil || h.AuthManager == nil {
						return
					}
					selectedAuthID = authID
					selectedAuth, ok := sessionAuthByID(authID)
					if !ok || selectedAuth == nil {
						return
					}
					if websocketUpstreamSupportsIncrementalInput(selectedAuth.Attributes, selectedAuth.Metadata) {
						pinnedAuthID = authID
					}
				})
			}
			dataChan, _, errChan := h.ExecuteStreamWithAuthManager(cliCtx, h.HandlerType(), modelName, upstreamPayload, "")
			forwardResult, errForward := h.forwardResponsesWebsocketWithOptions(c, conn, cliCancel, dataChan, errChan, wsTimelineLog, passthroughSessionID, responsesWebsocketForwardOptions{
				interceptPreviousResponseNotFound: interceptPreviousResponseNotFound,
				interceptInvalidEncryptedContent:  interceptInvalidEncryptedContent,
				interceptProviderItemNotFound:     interceptProviderItemNotFound,
				logicalPreviousResponseID:         logicalPreviousResponseID,
				turnCanceledByCompaction:          turnCanceledByCompaction,
			})
			return responsesWebsocketUpstreamAttempt{
				forwardResult:  forwardResult,
				selectedAuthID: selectedAuthID,
				err:            errForward,
			}
		}

		beginUpstreamForward()
		usedResponsesState := !useUpstreamWebsocketPassthrough && gjson.GetBytes(requestJSON, "previous_response_id").Exists()
		usedDynamicResponsesState := usedResponsesState && dynamicResponsesStateProbe
		attempt := runUpstreamRequest(requestJSON, len(fallbackRequestJSON) > 0, len(encryptedContentRetryRequestJSON) > 0, len(portableReplayRequestJSON) > 0)
		forwardResult, errForward := attempt.forwardResult, attempt.err
		usedRollbackRetry := false
		usedFullReplayRetry := false
		stateProbeFailed := false
		if errForward == nil && forwardResult.interceptedForRetry && len(fallbackRequestJSON) > 0 && responsesWebsocketPreviousResponseNotFound(forwardResult.errorMessage) {
			if usedDynamicResponsesState &&
				strings.TrimSpace(attempt.selectedAuthID) != "" &&
				responsesStateForRoute(attempt.selectedAuthID, requestModelName) != responsesWebsocketStateSupported {
				stateProbeFailed = true
				markResponsesStateForRoute(attempt.selectedAuthID, requestModelName, responsesWebsocketStateUnsupported)
				pinnedAuthID = strings.TrimSpace(attempt.selectedAuthID)
			}
			if len(rollbackRequestJSON) > 0 && !stateProbeFailed {
				clearPendingUpstreamDisconnectClose()
				log.Infof("responses websocket: previous_response_id not found id=%s, retrying from previous checkpoint", passthroughSessionID)
				attempt = runUpstreamRequest(rollbackRequestJSON, true, len(encryptedContentRetryRequestJSON) > 0, len(portableReplayRequestJSON) > 0)
				forwardResult, errForward = attempt.forwardResult, attempt.err
				usedRollbackRetry = errForward == nil && !responsesWebsocketPreviousResponseNotFound(forwardResult.errorMessage)
			}
			if errForward == nil && forwardResult.interceptedForRetry && responsesWebsocketPreviousResponseNotFound(forwardResult.errorMessage) {
				clearPendingUpstreamDisconnectClose()
				log.Infof("responses websocket: previous_response_id not found id=%s, retrying with full transcript", passthroughSessionID)
				attempt = runUpstreamRequest(fallbackRequestJSON, false, len(encryptedContentRetryRequestJSON) > 0, len(portableReplayRequestJSON) > 0)
				forwardResult, errForward = attempt.forwardResult, attempt.err
				usedRollbackRetry = false
				usedFullReplayRetry = true
			}
		}
		usedEncryptedContentRetry := false
		if errForward == nil && forwardResult.interceptedForRetry && len(encryptedContentRetryRequestJSON) > 0 && responsesWebsocketInvalidEncryptedContent(forwardResult.errorMessage) {
			clearPendingUpstreamDisconnectClose()
			log.Infof("responses websocket: invalid encrypted content id=%s, retrying without reasoning encrypted_content", passthroughSessionID)
			attempt = runUpstreamRequest(encryptedContentRetryRequestJSON, len(fallbackRequestJSON) > 0, false, len(providerIdentifierRetryRequestJSON) > 0 || len(portableReplayRequestJSON) > 0)
			forwardResult, errForward = attempt.forwardResult, attempt.err
			usedEncryptedContentRetry = errForward == nil && forwardResult.errorMessage == nil
			if usedEncryptedContentRetry {
				lastRequest = bytes.Clone(encryptedContentRetryRequestJSON)
			}
		}
		if errForward == nil && forwardResult.interceptedForRetry && len(providerIdentifierRetryRequestJSON) > 0 && responsesWebsocketProviderItemNotFound(forwardResult.errorMessage) && !usedEncryptedContentRetry {
			clearPendingUpstreamDisconnectClose()
			log.Infof("responses websocket: provider item id not found id=%s, retrying without provider identifiers", passthroughSessionID)
			attempt = runUpstreamRequest(providerIdentifierRetryRequestJSON, false, len(encryptedContentRetryRequestJSON) > 0, len(portableReplayRequestJSON) > 0)
			forwardResult, errForward = attempt.forwardResult, attempt.err
			if errForward == nil && forwardResult.errorMessage == nil {
				lastRequest = bytes.Clone(providerIdentifierRetryRequestJSON)
			}
		}
		usedPortableReplayRetry := false
		if errForward == nil && forwardResult.interceptedForRetry && len(portableReplayRequestJSON) > 0 && (responsesWebsocketProviderItemNotFound(forwardResult.errorMessage) || responsesWebsocketInvalidEncryptedContent(forwardResult.errorMessage)) {
			clearPendingUpstreamDisconnectClose()
			log.Infof("responses websocket: provider state not portable id=%s, retrying with portable replay", passthroughSessionID)
			attempt = runUpstreamRequest(portableReplayRequestJSON, false, false, false)
			forwardResult, errForward = attempt.forwardResult, attempt.err
			usedPortableReplayRetry = errForward == nil && forwardResult.errorMessage == nil
			if usedPortableReplayRetry {
				lastRequest = bytes.Clone(portableReplayRequestJSON)
			}
		}
		endUpstreamForward()
		if errForward != nil {
			if errors.Is(errForward, errResponsesWebsocketTurnCompacted) {
				wsTerminateErr = errForward
				log.Infof("responses websocket: main turn compacted id=%s", passthroughSessionID)
				return
			}
			wsTerminateErr = errForward
			log.Warnf("responses websocket: forward failed id=%s error=%v", passthroughSessionID, errForward)
			return
		}
		if shouldReleaseResponsesWebsocketPinnedAuth(forwardResult.errorMessage) {
			pinnedAuthID = ""
			forceTranscriptReplayNextRequest = true
			lastRequest = previousLastRequest
			lastResponseOutput = previousLastResponseOutput
			lastResponseID = previousLastResponseID
			lastResponsePendingToolCallIDs = previousLastResponsePendingToolCallIDs
			previousCheckpoint = previousCheckpointBeforeRequest
			if useUpstreamWebsocketPassthrough {
				passthroughModelName = ""
			}
			continue
		}
		if strings.TrimSpace(attempt.selectedAuthID) != "" && forwardResult.errorMessage == nil && forwardResult.completedResponseID != "" {
			pinnedAuthID = strings.TrimSpace(attempt.selectedAuthID)
			if usedDynamicResponsesState && !stateProbeFailed {
				markResponsesStateForRoute(attempt.selectedAuthID, requestModelName, responsesWebsocketStateSupported)
			}
		}
		lastResponseOutput = forwardResult.completedOutput
		lastResponsePendingToolCallIDs = append([]string(nil), forwardResult.pendingToolCallIDs...)
		if forwardResult.completedResponseID != "" {
			lastResponseID = strings.TrimSpace(forwardResult.completedResponseID)
			switch {
			case usedFullReplayRetry, usedEncryptedContentRetry, usedPortableReplayRetry:
				previousCheckpoint = responsesWebsocketCheckpoint{}
			case usedRollbackRetry:
				previousCheckpoint = previousCheckpointBeforeRequest
			case previousLastResponseID != "":
				previousCheckpoint = responsesWebsocketCheckpoint{
					responseID: previousLastResponseID,
					request:    previousLastRequest,
					output:     previousLastResponseOutput,
				}
			default:
				previousCheckpoint = responsesWebsocketCheckpoint{}
			}
		} else {
			lastResponseID = ""
			previousCheckpoint = responsesWebsocketCheckpoint{}
		}
	}
}

func websocketClientAddress(c *gin.Context) string {
	if c == nil || c.Request == nil {
		return ""
	}
	return strings.TrimSpace(c.ClientIP())
}

func websocketUpgradeHeaders(req *http.Request) http.Header {
	headers := http.Header{}
	if req == nil {
		return headers
	}

	// Keep the same sticky turn-state across reconnects when provided by the client.
	turnState := strings.TrimSpace(req.Header.Get(wsTurnStateHeader))
	if turnState != "" {
		headers.Set(wsTurnStateHeader, turnState)
	}
	return headers
}

func normalizeResponsesWebsocketRequest(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte) ([]byte, []byte, *interfaces.ErrorMessage) {
	return normalizeResponsesWebsocketRequestWithMode(rawJSON, lastRequest, lastResponseOutput, true, true)
}

func normalizeResponsesWebsocketRequestWithMode(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, allowIncrementalInputWithPreviousResponseID bool, allowCompactionReplayBypass bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	return normalizeResponsesWebsocketRequestWithLastResponseID(rawJSON, lastRequest, lastResponseOutput, "", allowIncrementalInputWithPreviousResponseID, allowCompactionReplayBypass)
}

func normalizeResponsesWebsocketRequestWithLastResponseID(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, lastResponseID string, allowIncrementalInputWithPreviousResponseID bool, allowCompactionReplayBypass bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	return normalizeResponsesWebsocketRequestWithIncrementalState(rawJSON, lastRequest, lastResponseOutput, lastResponseID, nil, allowIncrementalInputWithPreviousResponseID, allowCompactionReplayBypass)
}

func normalizeResponsesWebsocketRequestWithIncrementalState(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, lastResponseID string, lastResponsePendingToolCallIDs []string, allowIncrementalInputWithPreviousResponseID bool, allowCompactionReplayBypass bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	switch requestType {
	case wsRequestTypeCreate:
		// log.Infof("responses websocket: response.create request")
		if len(lastRequest) == 0 {
			return normalizeResponseCreateRequest(rawJSON)
		}
		return normalizeResponseSubsequentRequest(rawJSON, lastRequest, lastResponseOutput, lastResponseID, lastResponsePendingToolCallIDs, allowIncrementalInputWithPreviousResponseID, allowCompactionReplayBypass)
	case wsRequestTypeAppend:
		// log.Infof("responses websocket: response.append request")
		return normalizeResponseSubsequentRequest(rawJSON, lastRequest, lastResponseOutput, lastResponseID, lastResponsePendingToolCallIDs, allowIncrementalInputWithPreviousResponseID, allowCompactionReplayBypass)
	default:
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("unsupported websocket request type: %s", requestType),
		}
	}
}

func normalizeResponseCreateRequest(rawJSON []byte) ([]byte, []byte, *interfaces.ErrorMessage) {
	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	if !gjson.GetBytes(normalized, "input").Exists() {
		normalized, _ = sjson.SetRawBytes(normalized, "input", []byte("[]"))
	}

	modelName := strings.TrimSpace(gjson.GetBytes(normalized, "model").String())
	if modelName == "" {
		return nil, nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("missing model in response.create request"),
		}
	}
	normalized = finalizeResponsesWebsocketRequest(normalized, nil)
	normalized = stripUnsupportedResponsesWebsocketInputItemMetadata(normalized)
	return normalized, bytes.Clone(normalized), nil
}

func finalizeResponsesWebsocketRequest(normalized []byte, lastRequest []byte) []byte {
	normalized = normalizeCodexFastSpeedTierRequest(normalized)
	if gjson.GetBytes(normalized, "service_tier").Exists() {
		return normalized
	}
	if len(lastRequest) == 0 {
		return normalized
	}
	lastServiceTier := gjson.GetBytes(lastRequest, "service_tier")
	if !lastServiceTier.Exists() {
		return normalized
	}
	modelName := strings.TrimSpace(gjson.GetBytes(normalized, "model").String())
	lastModelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
	if modelName == "" || modelName != lastModelName {
		return normalized
	}
	updated, err := sjson.SetRawBytes(normalized, "service_tier", []byte(lastServiceTier.Raw))
	if err != nil {
		return normalized
	}
	return updated
}

func normalizeResponseSubsequentRequest(rawJSON []byte, lastRequest []byte, lastResponseOutput []byte, lastResponseID string, lastResponsePendingToolCallIDs []string, allowIncrementalInputWithPreviousResponseID bool, allowCompactionReplayBypass bool) ([]byte, []byte, *interfaces.ErrorMessage) {
	if len(lastRequest) == 0 {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request received before response.create"),
		}
	}

	nextInput := gjson.GetBytes(rawJSON, "input")
	if !nextInput.Exists() || !nextInput.IsArray() {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("websocket request requires array field: input"),
		}
	}

	// Compaction can cause clients to replace local websocket history with a new
	// compact transcript on the next `response.create`. When the input already
	// contains historical model output items, treating it as an incremental append
	// duplicates stale turn-state and can leave late orphaned function_call items.
	if shouldReplaceWebsocketTranscript(rawJSON, nextInput, allowCompactionReplayBypass) {
		normalized := normalizeResponseTranscriptReplacement(rawJSON, lastRequest)
		return normalized, bytes.Clone(normalized), nil
	}

	// Websocket v2 mode uses response.create with previous_response_id + incremental input.
	// Do not expand it into a full input transcript; upstream expects the incremental payload.
	if allowIncrementalInputWithPreviousResponseID {
		prev := strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String())
		if prev == "" {
			if !inputSatisfiesPendingToolCalls(nextInput, lastResponsePendingToolCallIDs) {
				normalized := normalizeResponseTranscriptReplacement(rawJSON, lastRequest)
				return normalized, bytes.Clone(normalized), nil
			}
			prev = strings.TrimSpace(lastResponseID)
		}
		if prev != "" {
			normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
			if errDelete != nil {
				normalized = bytes.Clone(rawJSON)
			}
			normalized, _ = sjson.SetBytes(normalized, "previous_response_id", prev)
			if !gjson.GetBytes(normalized, "model").Exists() {
				modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
				if modelName != "" {
					normalized, _ = sjson.SetBytes(normalized, "model", modelName)
				}
			}
			if !gjson.GetBytes(normalized, "instructions").Exists() {
				instructions := gjson.GetBytes(lastRequest, "instructions")
				if instructions.Exists() {
					normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
				}
			}
			normalized, _ = sjson.SetBytes(normalized, "stream", true)
			normalized = finalizeResponsesWebsocketRequest(normalized, lastRequest)
			normalized = stripUnsupportedResponsesWebsocketInputItemMetadata(normalized)
			return normalized, bytes.Clone(normalized), nil
		}
	}

	// When the client sends a compact replay for a downstream that can consume it
	// directly, the input already carries the canonical history. In that case,
	// skip merging with stale lastRequest/lastResponseOutput to avoid breaking
	// function_call / function_call_output pairings.
	// See: https://github.com/router-for-me/CLIProxyAPI/issues/2207
	var mergedInput string
	if allowCompactionReplayBypass && inputContainsFullTranscript(nextInput) {
		log.Infof("responses websocket: full transcript detected, skipping stale merge (input items=%d)", len(nextInput.Array()))
		mergedInput = nextInput.Raw
	} else {
		appendInputRaw := nextInput.Raw
		if inputContainsFullTranscript(nextInput) {
			appendInputRaw = inputWithoutCompactionItems(nextInput)
		}

		existingInput := gjson.GetBytes(lastRequest, "input")
		var errMerge error
		mergedInput, errMerge = mergeJSONArrayRaw(existingInput.Raw, normalizeJSONArrayRaw(lastResponseOutput))
		if errMerge != nil {
			return nil, lastRequest, &interfaces.ErrorMessage{
				StatusCode: http.StatusBadRequest,
				Error:      fmt.Errorf("invalid previous response output: %w", errMerge),
			}
		}

		mergedInput, errMerge = mergeJSONArrayRaw(mergedInput, appendInputRaw)
		if errMerge != nil {
			return nil, lastRequest, &interfaces.ErrorMessage{
				StatusCode: http.StatusBadRequest,
				Error:      fmt.Errorf("invalid request input: %w", errMerge),
			}
		}
	}
	dedupedInput, errDedupeFunctionCalls := dedupeFunctionCallsByCallID(mergedInput)
	if errDedupeFunctionCalls == nil {
		mergedInput = dedupedInput
	}
	dedupedInput, errDedupeItemIDs := dedupeInputItemsByID(mergedInput)
	if errDedupeItemIDs == nil {
		mergedInput = dedupedInput
	}

	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	var errSet error
	normalized, errSet = sjson.SetRawBytes(normalized, "input", []byte(mergedInput))
	if errSet != nil {
		return nil, lastRequest, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("failed to merge websocket input: %w", errSet),
		}
	}
	if !gjson.GetBytes(normalized, "model").Exists() {
		modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		if modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	normalized = finalizeResponsesWebsocketRequest(normalized, lastRequest)
	normalized = stripUnsupportedResponsesWebsocketInputItemMetadata(normalized)
	return normalized, bytes.Clone(normalized), nil
}

func shouldReplaceWebsocketTranscript(rawJSON []byte, nextInput gjson.Result, allowCompactionReplayBypass bool) bool {
	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	if requestType != wsRequestTypeCreate && requestType != wsRequestTypeAppend {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "previous_response_id").String()) != "" {
		return false
	}
	if !nextInput.Exists() || !nextInput.IsArray() {
		return false
	}

	if allowCompactionReplayBypass && inputContainsFullTranscript(nextInput) {
		return true
	}

	for _, item := range nextInput.Array() {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "function_call", "custom_tool_call", "tool_search_call":
			return true
		case "message":
			role := strings.TrimSpace(item.Get("role").String())
			if role == "assistant" {
				return true
			}
		}
	}

	return false
}

func inputSatisfiesPendingToolCalls(input gjson.Result, pendingCallIDs []string) bool {
	if len(pendingCallIDs) == 0 {
		return true
	}
	if !input.IsArray() {
		return false
	}
	outputs := make(map[string]struct{}, len(pendingCallIDs))
	for _, item := range input.Array() {
		switch strings.TrimSpace(item.Get("type").String()) {
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			callID := strings.TrimSpace(item.Get("call_id").String())
			if callID != "" {
				outputs[callID] = struct{}{}
			}
		}
	}
	for _, callID := range pendingCallIDs {
		callID = strings.TrimSpace(callID)
		if callID == "" {
			continue
		}
		if _, ok := outputs[callID]; !ok {
			return false
		}
	}
	return true
}

func normalizeResponseTranscriptReplacement(rawJSON []byte, lastRequest []byte) []byte {
	normalized, errDelete := sjson.DeleteBytes(rawJSON, "type")
	if errDelete != nil {
		normalized = bytes.Clone(rawJSON)
	}
	normalized, _ = sjson.DeleteBytes(normalized, "previous_response_id")
	if !gjson.GetBytes(normalized, "model").Exists() {
		modelName := strings.TrimSpace(gjson.GetBytes(lastRequest, "model").String())
		if modelName != "" {
			normalized, _ = sjson.SetBytes(normalized, "model", modelName)
		}
	}
	if !gjson.GetBytes(normalized, "instructions").Exists() {
		instructions := gjson.GetBytes(lastRequest, "instructions")
		if instructions.Exists() {
			normalized, _ = sjson.SetRawBytes(normalized, "instructions", []byte(instructions.Raw))
		}
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	normalized = stripUnsupportedResponsesWebsocketInputItemMetadata(normalized)
	return bytes.Clone(normalized)
}

func stripUnsupportedResponsesWebsocketInputItemMetadata(payload []byte) []byte {
	input := gjson.GetBytes(payload, "input")
	if !input.Exists() || !input.IsArray() {
		return payload
	}
	sanitized := payload
	for i, item := range input.Array() {
		if !item.Get("metadata").Exists() {
			continue
		}
		updated, errDelete := sjson.DeleteBytes(sanitized, fmt.Sprintf("input.%d.metadata", i))
		if errDelete != nil {
			continue
		}
		sanitized = updated
	}
	return sanitized
}

func dedupeFunctionCallsByCallID(rawArray string) (string, error) {
	rawArray = strings.TrimSpace(rawArray)
	if rawArray == "" {
		return "[]", nil
	}
	var items []json.RawMessage
	if errUnmarshal := json.Unmarshal([]byte(rawArray), &items); errUnmarshal != nil {
		return "", errUnmarshal
	}

	seenCallIDs := make(map[string]struct{}, len(items))
	filtered := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		if len(item) == 0 {
			continue
		}
		itemType := strings.TrimSpace(gjson.GetBytes(item, "type").String())
		if isResponsesToolCallType(itemType) {
			callID := strings.TrimSpace(gjson.GetBytes(item, "call_id").String())
			if callID != "" {
				if _, ok := seenCallIDs[callID]; ok {
					continue
				}
				seenCallIDs[callID] = struct{}{}
			}
		}
		filtered = append(filtered, item)
	}

	out, errMarshal := json.Marshal(filtered)
	if errMarshal != nil {
		return "", errMarshal
	}
	return string(out), nil
}

func dedupeResponsesWebsocketInputItemsByID(payload []byte) []byte {
	input := gjson.GetBytes(payload, "input")
	if !input.Exists() || !input.IsArray() {
		return payload
	}
	dedupedInput, errDedupe := dedupeInputItemsByID(input.Raw)
	if errDedupe != nil || dedupedInput == input.Raw {
		return payload
	}
	updated, errSet := sjson.SetRawBytes(payload, "input", []byte(dedupedInput))
	if errSet != nil {
		return payload
	}
	return updated
}

func dedupeInputItemsByID(rawArray string) (string, error) {
	rawArray = strings.TrimSpace(rawArray)
	if rawArray == "" {
		return "[]", nil
	}
	var items []json.RawMessage
	if errUnmarshal := json.Unmarshal([]byte(rawArray), &items); errUnmarshal != nil {
		return "", errUnmarshal
	}

	// Parse each item's type, id and call_id once; gjson is a scan-based
	// parser, so reusing this metadata avoids rescanning every item in each of
	// the loops below as the conversation history grows.
	type itemMetadata struct {
		itemType string
		id       string
		callID   string
	}
	meta := make([]itemMetadata, len(items))
	for i, item := range items {
		if len(item) == 0 {
			continue
		}
		res := gjson.GetManyBytes(item, "type", "id", "call_id")
		meta[i] = itemMetadata{
			itemType: strings.TrimSpace(res[0].String()),
			id:       strings.TrimSpace(res[1].String()),
			callID:   strings.TrimSpace(res[2].String()),
		}
	}

	// Collect the call_ids that are still referenced by tool-call output
	// items. When several input items share the same id, the one we keep must
	// preserve any call_id that has a matching output; otherwise the upstream
	// rejects the request with "No tool call found for function call output".
	referencedCallIDs := make(map[string]struct{}, len(items))
	for i := range items {
		switch meta[i].itemType {
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			if meta[i].callID != "" {
				referencedCallIDs[meta[i].callID] = struct{}{}
			}
		}
	}

	// For each id, choose the index to keep. The default is the last
	// occurrence (matching the original dedupe behavior), but we never replace
	// an item whose call_id still has a matching output with one that does not.
	// This keeps a single item per id while ensuring retained tool calls stay
	// paired with their outputs.
	keepIndexByID := make(map[string]int, len(items))
	keepReferencedByID := make(map[string]bool, len(items))
	for i := range items {
		itemID := meta[i].id
		if itemID == "" {
			continue
		}
		_, referenced := referencedCallIDs[meta[i].callID]
		referenced = referenced && meta[i].callID != ""
		if _, seen := keepIndexByID[itemID]; !seen {
			keepIndexByID[itemID] = i
			keepReferencedByID[itemID] = referenced
			continue
		}
		if referenced || !keepReferencedByID[itemID] {
			keepIndexByID[itemID] = i
			keepReferencedByID[itemID] = referenced
		}
	}

	filtered := make([]json.RawMessage, 0, len(items))
	for i, item := range items {
		if len(item) == 0 {
			continue
		}
		itemID := meta[i].id
		if itemID != "" {
			if keepIndexByID[itemID] != i {
				continue
			}
		}
		filtered = append(filtered, item)
	}

	out, errMarshal := json.Marshal(filtered)
	if errMarshal != nil {
		return "", errMarshal
	}
	return string(out), nil
}

func websocketUpstreamSupportsIncrementalInput(attributes map[string]string, metadata map[string]any) bool {
	if len(attributes) > 0 {
		if raw := strings.TrimSpace(attributes["websockets"]); raw != "" {
			parsed, errParse := strconv.ParseBool(raw)
			if errParse == nil {
				return parsed
			}
		}
	}
	if len(metadata) == 0 {
		return false
	}
	raw, ok := metadata["websockets"]
	if !ok || raw == nil {
		return false
	}
	switch value := raw.(type) {
	case bool:
		return value
	case string:
		parsed, errParse := strconv.ParseBool(strings.TrimSpace(value))
		if errParse == nil {
			return parsed
		}
	default:
	}
	return false
}

func (h *OpenAIResponsesAPIHandler) websocketUpstreamSupportsIncrementalInputForModel(modelName string) bool {
	auths, _ := h.responsesWebsocketAvailableAuthsForModel(modelName)
	for _, auth := range auths {
		if responsesWebsocketAuthSupportsIncrementalInput(auth) {
			return true
		}
	}
	return false
}

func (h *OpenAIResponsesAPIHandler) websocketUpstreamSupportsCompactionReplayForModel(modelName string) bool {
	auths, _ := h.responsesWebsocketAvailableAuthsForModel(modelName)
	if len(auths) == 0 {
		return false
	}
	for _, auth := range auths {
		if !responsesWebsocketAuthSupportsCompactionReplay(auth) {
			return false
		}
	}
	return true
}

func (h *OpenAIResponsesAPIHandler) responsesWebsocketResponsesStateUnsupportedForModel(modelName string) bool {
	auths, _ := h.responsesWebsocketAvailableAuthsForModel(modelName)
	seenHTTPRoute := false
	for _, auth := range auths {
		if auth == nil || responsesWebsocketAuthSupportsIncrementalInput(auth) {
			continue
		}
		seenHTTPRoute = true
		if responsesWebsocketAuthResponsesStateSupportForModel(auth, modelName) != responsesWebsocketStateUnsupported {
			return false
		}
	}
	return seenHTTPRoute
}

func (h *OpenAIResponsesAPIHandler) responsesWebsocketCanProbeResponsesStateForModel(modelName string) bool {
	auths, _ := h.responsesWebsocketAvailableAuthsForModel(modelName)
	for _, auth := range auths {
		if responsesWebsocketAuthCanProbeResponsesStateForModel(auth, modelName) {
			return true
		}
	}
	return false
}

func responsesWebsocketAuthCanProbeResponsesStateForModel(auth *coreauth.Auth, modelName string) bool {
	if auth == nil {
		return false
	}
	switch responsesWebsocketAuthResponsesStateSupportForModel(auth, modelName) {
	case responsesWebsocketStateSupported:
		return true
	case responsesWebsocketStateUnsupported:
		return false
	default:
		return !responsesWebsocketAuthSupportsIncrementalInput(auth)
	}
}

func (h *OpenAIResponsesAPIHandler) responsesWebsocketAvailableAuthsForModel(modelName string) ([]*coreauth.Auth, string) {
	if h == nil || h.AuthManager == nil {
		return nil, ""
	}
	resolvedModelName := responsesWebsocketResolvedModelName(modelName)
	providerSet, modelKey := responsesWebsocketProviderSetForModel(resolvedModelName)
	if len(providerSet) == 0 {
		return nil, modelKey
	}

	registryRef := registry.GetGlobalRegistry()
	now := time.Now()
	auths := h.AuthManager.List()
	available := make([]*coreauth.Auth, 0, len(auths))
	for _, auth := range auths {
		if !responsesWebsocketAuthMatchesModel(auth, providerSet, modelKey, registryRef, now) {
			continue
		}
		available = append(available, auth)
	}
	return available, modelKey
}

func (h *OpenAIResponsesAPIHandler) responsesWebsocketUsesCodexWebsocketPassthrough(modelName string) bool {
	return h.responsesWebsocketUsesUpstreamWebsocketPassthrough(modelName)
}

func (h *OpenAIResponsesAPIHandler) responsesWebsocketAuthUsesUpstreamWebsocketPassthrough(auth *coreauth.Auth) bool {
	if h == nil || h.AuthManager == nil || auth == nil {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(auth.Provider))
	if provider != "codex" && provider != "xai" {
		return false
	}
	if _, ok := h.AuthManager.Executor(provider); !ok {
		return false
	}
	return websocketUpstreamSupportsIncrementalInput(auth.Attributes, auth.Metadata)
}

func (h *OpenAIResponsesAPIHandler) responsesWebsocketUsesUpstreamWebsocketPassthrough(modelName string) bool {
	modelName = strings.TrimSpace(modelName)
	if h == nil || h.AuthManager == nil || modelName == "" {
		return false
	}
	auths, _ := h.responsesWebsocketAvailableAuthsForModel(modelName)
	if len(auths) == 0 {
		return false
	}
	provider := ""
	for _, auth := range auths {
		if auth == nil {
			return false
		}
		authProvider := strings.ToLower(strings.TrimSpace(auth.Provider))
		if authProvider != "codex" && authProvider != "xai" {
			return false
		}
		if provider == "" {
			provider = authProvider
			if _, ok := h.AuthManager.Executor(provider); !ok {
				return false
			}
		} else if authProvider != provider {
			return false
		}
		if !websocketUpstreamSupportsIncrementalInput(auth.Attributes, auth.Metadata) {
			return false
		}
	}
	return provider != ""
}

func responsesWebsocketAuthSupportsIncrementalInput(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	return websocketUpstreamSupportsIncrementalInput(auth.Attributes, auth.Metadata)
}

func responsesWebsocketAuthResponsesStateSupport(auth *coreauth.Auth) responsesWebsocketStateSupport {
	if auth == nil {
		return responsesWebsocketStateUnknown
	}
	if value, ok := parseResponsesWebsocketStateSupportValue(auth.Attributes[responsesStateAuthCapabilityKey]); ok {
		return value
	}
	if len(auth.Metadata) == 0 {
		return responsesWebsocketStateUnknown
	}
	raw, ok := auth.Metadata[responsesStateAuthCapabilityKey]
	if !ok || raw == nil {
		return responsesWebsocketStateUnknown
	}
	if value, ok := parseResponsesWebsocketStateSupport(raw); ok {
		return value
	}
	return responsesWebsocketStateUnknown
}

func responsesWebsocketAuthResponsesStateSupportForModel(auth *coreauth.Auth, modelName string) responsesWebsocketStateSupport {
	if auth == nil {
		return responsesWebsocketStateUnknown
	}
	if scoped, hasScope := responsesWebsocketAuthResponsesStateModelsMatch(auth, modelName); hasScope && !scoped {
		return responsesWebsocketStateUnknown
	}
	return responsesWebsocketAuthResponsesStateSupport(auth)
}

func responsesWebsocketAuthResponsesStateModelsMatch(auth *coreauth.Auth, modelName string) (bool, bool) {
	modelName = strings.TrimSpace(modelName)
	if auth == nil || modelName == "" {
		return false, false
	}
	if matched, hasScope := responsesWebsocketResponsesStateModelsValueMatches(auth.Attributes[responsesStateAuthModelsKey], modelName); hasScope {
		return matched, true
	}
	if len(auth.Metadata) == 0 {
		return false, false
	}
	raw, ok := auth.Metadata[responsesStateAuthModelsKey]
	if !ok || raw == nil {
		return false, false
	}
	return responsesWebsocketResponsesStateModelsRawMatches(raw, modelName)
}

func responsesWebsocketResponsesStateModelsRawMatches(raw any, modelName string) (bool, bool) {
	switch value := raw.(type) {
	case string:
		return responsesWebsocketResponsesStateModelsValueMatches(value, modelName)
	case []string:
		return responsesWebsocketResponsesStateModelListMatches(value, modelName), len(value) > 0
	case []any:
		values := make([]string, 0, len(value))
		for _, item := range value {
			if s, ok := item.(string); ok {
				values = append(values, s)
			}
		}
		return responsesWebsocketResponsesStateModelListMatches(values, modelName), len(values) > 0
	default:
		return false, false
	}
}

func responsesWebsocketResponsesStateModelsValueMatches(raw string, modelName string) (bool, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false, false
	}
	var values []string
	if err := json.Unmarshal([]byte(raw), &values); err == nil {
		return responsesWebsocketResponsesStateModelListMatches(values, modelName), len(values) > 0
	}
	parts := strings.Split(raw, ",")
	return responsesWebsocketResponsesStateModelListMatches(parts, modelName), len(parts) > 0
}

func responsesWebsocketResponsesStateModelListMatches(values []string, modelName string) bool {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		return false
	}
	parsedModelName := strings.TrimSpace(thinking.ParseSuffix(modelName).ModelName)
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if value == modelName || strings.EqualFold(value, modelName) {
			return true
		}
		if parsedModelName != "" && (value == parsedModelName || strings.EqualFold(value, parsedModelName)) {
			return true
		}
	}
	return false
}

func parseResponsesWebsocketStateSupport(raw any) (responsesWebsocketStateSupport, bool) {
	switch value := raw.(type) {
	case bool:
		if value {
			return responsesWebsocketStateSupported, true
		}
		return responsesWebsocketStateUnsupported, true
	case string:
		return parseResponsesWebsocketStateSupportValue(value)
	default:
		return responsesWebsocketStateUnknown, false
	}
}

func parseResponsesWebsocketStateSupportValue(raw string) (responsesWebsocketStateSupport, bool) {
	value := strings.TrimSpace(strings.ToLower(raw))
	switch value {
	case "true", "1", "yes", "y", "on", "supported", "stateful":
		return responsesWebsocketStateSupported, true
	case "false", "0", "no", "n", "off", "unsupported", "stateless":
		return responsesWebsocketStateUnsupported, true
	default:
		return responsesWebsocketStateUnknown, false
	}
}

func normalizeResponsesWebsocketPassthroughRequest(rawJSON []byte, modelName string) ([]byte, *interfaces.ErrorMessage) {
	if !json.Valid(rawJSON) {
		return nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("invalid websocket request JSON"),
		}
	}

	requestType := strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String())
	switch requestType {
	case wsRequestTypeCreate, wsRequestTypeAppend:
	default:
		return nil, &interfaces.ErrorMessage{
			StatusCode: http.StatusBadRequest,
			Error:      fmt.Errorf("unsupported websocket request type: %s", requestType),
		}
	}

	normalized := bytes.Clone(rawJSON)
	if strings.TrimSpace(gjson.GetBytes(normalized, "model").String()) == "" {
		modelName = strings.TrimSpace(modelName)
		if modelName == "" {
			return nil, &interfaces.ErrorMessage{
				StatusCode: http.StatusBadRequest,
				Error:      fmt.Errorf("missing model in response.create request"),
			}
		}
		normalized, _ = sjson.SetBytes(normalized, "model", modelName)
	}
	normalized, _ = sjson.SetBytes(normalized, "stream", true)
	normalized = stripUnsupportedResponsesWebsocketInputItemMetadata(normalized)
	return normalized, nil
}

func responsesWebsocketResolvedModelName(modelName string) string {
	initialSuffix := thinking.ParseSuffix(modelName)
	if initialSuffix.ModelName == "auto" {
		resolvedBase := util.ResolveAutoModel(initialSuffix.ModelName)
		if initialSuffix.HasSuffix {
			return fmt.Sprintf("%s(%s)", resolvedBase, initialSuffix.RawSuffix)
		}
		return resolvedBase
	}
	return util.ResolveAutoModel(modelName)
}

func responsesWebsocketProviderSetForModel(resolvedModelName string) (map[string]struct{}, string) {
	parsed := thinking.ParseSuffix(resolvedModelName)
	baseModel := strings.TrimSpace(parsed.ModelName)
	providers := util.GetProviderName(baseModel)
	if len(providers) == 0 && baseModel != resolvedModelName {
		providers = util.GetProviderName(resolvedModelName)
	}
	providerSet := make(map[string]struct{}, len(providers))
	for _, provider := range providers {
		providerKey := strings.TrimSpace(strings.ToLower(provider))
		if providerKey == "" {
			continue
		}
		providerSet[providerKey] = struct{}{}
	}
	modelKey := baseModel
	if modelKey == "" {
		modelKey = strings.TrimSpace(resolvedModelName)
	}
	return providerSet, modelKey
}

func responsesWebsocketAuthMatchesModel(auth *coreauth.Auth, providerSet map[string]struct{}, modelKey string, registryRef *registry.ModelRegistry, now time.Time) bool {
	if auth == nil {
		return false
	}
	providerKey := strings.TrimSpace(strings.ToLower(auth.Provider))
	if _, ok := providerSet[providerKey]; !ok {
		return false
	}
	if modelKey != "" && registryRef != nil && !registryRef.ClientSupportsModel(auth.ID, modelKey) {
		return false
	}
	return responsesWebsocketAuthAvailableForModel(auth, modelKey, now)
}

func responsesWebsocketAuthSupportsCompactionReplay(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Provider), "codex")
}

func responsesWebsocketAuthAvailableForModel(auth *coreauth.Auth, modelName string, now time.Time) bool {
	if auth == nil {
		return false
	}
	if auth.Disabled || auth.Status == coreauth.StatusDisabled {
		return false
	}
	if modelName != "" && len(auth.ModelStates) > 0 {
		state, ok := auth.ModelStates[modelName]
		if (!ok || state == nil) && modelName != "" {
			baseModel := strings.TrimSpace(thinking.ParseSuffix(modelName).ModelName)
			if baseModel != "" && baseModel != modelName {
				state, ok = auth.ModelStates[baseModel]
			}
		}
		if ok && state != nil {
			if state.Status == coreauth.StatusDisabled {
				return false
			}
			if state.Unavailable && !state.NextRetryAfter.IsZero() && state.NextRetryAfter.After(now) {
				return false
			}
			return true
		}
	}
	if auth.Unavailable && !auth.NextRetryAfter.IsZero() && auth.NextRetryAfter.After(now) {
		return false
	}
	return true
}

func shouldHandleResponsesWebsocketPrewarmLocally(rawJSON []byte, lastRequest []byte) bool {
	if len(lastRequest) != 0 {
		return false
	}
	if strings.TrimSpace(gjson.GetBytes(rawJSON, "type").String()) != wsRequestTypeCreate {
		return false
	}
	generateResult := gjson.GetBytes(rawJSON, "generate")
	if !generateResult.Exists() || generateResult.Bool() {
		return false
	}
	input := gjson.GetBytes(rawJSON, "input")
	return !input.Exists() || (input.IsArray() && len(input.Array()) == 0)
}

func writeResponsesWebsocketSyntheticPrewarm(
	c *gin.Context,
	conn *websocket.Conn,
	requestJSON []byte,
	wsTimelineLog websocketTimelineAppender,
	sessionID string,
) error {
	payloads, errPayloads := syntheticResponsesWebsocketPrewarmPayloads(requestJSON)
	if errPayloads != nil {
		return errPayloads
	}
	for i := 0; i < len(payloads); i++ {
		markAPIResponseTimestamp(c)
		// log.Infof(
		// 	"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
		// 	sessionID,
		// 	websocket.TextMessage,
		// 	websocketPayloadEventType(payloads[i]),
		// 	websocketPayloadPreview(payloads[i]),
		// )
		if errWrite := writeResponsesWebsocketPayload(conn, wsTimelineLog, payloads[i], time.Now()); errWrite != nil {
			log.Warnf(
				"responses websocket: downstream_out write failed id=%s event=%s error=%v",
				sessionID,
				websocketPayloadEventType(payloads[i]),
				errWrite,
			)
			return errWrite
		}
	}
	return nil
}

func syntheticResponsesWebsocketPrewarmPayloads(requestJSON []byte) ([][]byte, error) {
	responseID := "resp_prewarm_" + uuid.NewString()
	createdAt := time.Now().Unix()
	modelName := strings.TrimSpace(gjson.GetBytes(requestJSON, "model").String())

	createdPayload := []byte(`{"type":"response.created","sequence_number":0,"response":{"id":"","object":"response","created_at":0,"status":"in_progress","background":false,"error":null,"output":[]}}`)
	var errSet error
	createdPayload, errSet = sjson.SetBytes(createdPayload, "response.id", responseID)
	if errSet != nil {
		return nil, errSet
	}
	createdPayload, errSet = sjson.SetBytes(createdPayload, "response.created_at", createdAt)
	if errSet != nil {
		return nil, errSet
	}
	if modelName != "" {
		createdPayload, errSet = sjson.SetBytes(createdPayload, "response.model", modelName)
		if errSet != nil {
			return nil, errSet
		}
	}

	completedPayload := []byte(`{"type":"response.completed","sequence_number":1,"response":{"id":"","object":"response","created_at":0,"status":"completed","background":false,"error":null,"output":[],"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}}`)
	completedPayload, errSet = sjson.SetBytes(completedPayload, "response.id", responseID)
	if errSet != nil {
		return nil, errSet
	}
	completedPayload, errSet = sjson.SetBytes(completedPayload, "response.created_at", createdAt)
	if errSet != nil {
		return nil, errSet
	}
	if modelName != "" {
		completedPayload, errSet = sjson.SetBytes(completedPayload, "response.model", modelName)
		if errSet != nil {
			return nil, errSet
		}
	}

	return [][]byte{createdPayload, completedPayload}, nil
}

func mergeJSONArrayRaw(existingRaw, appendRaw string) (string, error) {
	existingRaw = strings.TrimSpace(existingRaw)
	appendRaw = strings.TrimSpace(appendRaw)
	if existingRaw == "" {
		existingRaw = "[]"
	}
	if appendRaw == "" {
		appendRaw = "[]"
	}

	var existing []json.RawMessage
	if err := json.Unmarshal([]byte(existingRaw), &existing); err != nil {
		return "", err
	}
	var appendItems []json.RawMessage
	if err := json.Unmarshal([]byte(appendRaw), &appendItems); err != nil {
		return "", err
	}

	merged := append(existing, appendItems...)
	out, err := json.Marshal(merged)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// inputContainsFullTranscript returns true when the input array carries compact
// replay markers that indicate the client already sent the full conversation
// transcript. Merging that input with stale lastRequest/lastResponseOutput
// would duplicate or break function_call/function_call_output pairings, so the
// caller should use the input as-is.
//
// Assistant messages alone are not enough to classify the payload as a replay:
// incremental websocket requests may legitimately append assistant items.
func inputContainsFullTranscript(input gjson.Result) bool {
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		t := item.Get("type").String()
		if t == "compaction" || t == "compaction_summary" {
			return true
		}
	}
	return false
}

func inputWithoutCompactionItems(input gjson.Result) string {
	if !input.IsArray() {
		return normalizeJSONArrayRaw([]byte(input.Raw))
	}
	filtered := make([]string, 0, len(input.Array()))
	for _, item := range input.Array() {
		t := item.Get("type").String()
		if t == "compaction" || t == "compaction_summary" {
			continue
		}
		filtered = append(filtered, item.Raw)
	}
	return "[" + strings.Join(filtered, ",") + "]"
}

func normalizeJSONArrayRaw(raw []byte) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "[]"
	}
	result := gjson.Parse(trimmed)
	if result.Type == gjson.JSON && result.IsArray() {
		return trimmed
	}
	return "[]"
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesWebsocket(
	c *gin.Context,
	conn *websocket.Conn,
	cancel handlers.APIHandlerCancelFunc,
	data <-chan []byte,
	errs <-chan *interfaces.ErrorMessage,
	wsTimelineLog websocketTimelineAppender,
	sessionID string,
) ([]byte, string, []string, *interfaces.ErrorMessage, error) {
	result, err := h.forwardResponsesWebsocketWithOptions(c, conn, cancel, data, errs, wsTimelineLog, sessionID, responsesWebsocketForwardOptions{})
	return result.completedOutput, result.completedResponseID, result.pendingToolCallIDs, result.errorMessage, err
}

type responsesWebsocketForwardOptions struct {
	interceptPreviousResponseNotFound bool
	interceptInvalidEncryptedContent  bool
	interceptProviderItemNotFound     bool
	logicalPreviousResponseID         string
	turnCanceledByCompaction          func() bool
}

type responsesWebsocketForwardResult struct {
	completedOutput     []byte
	completedResponseID string
	pendingToolCallIDs  []string
	errorMessage        *interfaces.ErrorMessage
	interceptedForRetry bool
}

type responsesWebsocketUpstreamAttempt struct {
	forwardResult  responsesWebsocketForwardResult
	selectedAuthID string
	err            error
}

const responsesWebsocketStartupBufferMaxBytes = 1024 * 1024

type responsesWebsocketStartupBuffer struct {
	payloads [][]byte
	bytes    int
}

func (b *responsesWebsocketStartupBuffer) TryAppend(payload []byte) bool {
	if b == nil {
		return false
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 {
		return true
	}
	if b.bytes+len(payload) > responsesWebsocketStartupBufferMaxBytes {
		return false
	}
	b.payloads = append(b.payloads, bytes.Clone(payload))
	b.bytes += len(payload)
	return true
}

func (b *responsesWebsocketStartupBuffer) Drain() [][]byte {
	if b == nil || len(b.payloads) == 0 {
		return nil
	}
	payloads := b.payloads
	b.Reset()
	return payloads
}

func (b *responsesWebsocketStartupBuffer) Reset() {
	if b == nil {
		return
	}
	b.payloads = nil
	b.bytes = 0
}

func responsesWebsocketBufferableStartupPayload(eventType string) bool {
	// Keep this to bootstrap-only frames observed before model output.
	switch strings.TrimSpace(eventType) {
	case "codex.rate_limits", "response.created", "response.in_progress":
		return true
	default:
		return false
	}
}

func responsesWebsocketEffectivePreviousResponseID(rawPreviousResponseID string, requestJSON []byte) string {
	rawPreviousResponseID = strings.TrimSpace(rawPreviousResponseID)
	if rawPreviousResponseID != "" {
		return rawPreviousResponseID
	}
	return strings.TrimSpace(gjson.GetBytes(requestJSON, "previous_response_id").String())
}

func responsesWebsocketStateRouteKey(authID string, modelName string) string {
	authID = strings.TrimSpace(authID)
	modelName = strings.TrimSpace(modelName)
	if authID == "" || modelName == "" {
		return ""
	}
	return authID + "\n" + modelName
}

func responsesWebsocketCanProbeResponsesState(rawPreviousResponseID string, lastResponseID string) bool {
	rawPreviousResponseID = strings.TrimSpace(rawPreviousResponseID)
	if rawPreviousResponseID != "" {
		return !responsesWebsocketSyntheticPrewarmResponseID(rawPreviousResponseID)
	}
	lastResponseID = strings.TrimSpace(lastResponseID)
	return lastResponseID != "" && !responsesWebsocketSyntheticPrewarmResponseID(lastResponseID)
}

func responsesWebsocketSyntheticPrewarmResponseID(responseID string) bool {
	return strings.HasPrefix(strings.TrimSpace(responseID), "resp_prewarm_")
}

func responsesWebsocketPayloadWithLogicalPreviousResponseID(payload []byte, previousResponseID string) []byte {
	previousResponseID = strings.TrimSpace(previousResponseID)
	if previousResponseID == "" || len(payload) == 0 {
		return payload
	}
	response := gjson.GetBytes(payload, "response")
	responseRaw := strings.TrimSpace(response.Raw)
	if response.Type != gjson.JSON || !strings.HasPrefix(responseRaw, "{") {
		return payload
	}
	if current := strings.TrimSpace(gjson.GetBytes(payload, "response.previous_response_id").String()); current != "" {
		return payload
	}
	updated, errSet := sjson.SetBytes(payload, "response.previous_response_id", previousResponseID)
	if errSet != nil {
		return payload
	}
	return updated
}

func (h *OpenAIResponsesAPIHandler) forwardResponsesWebsocketWithOptions(
	c *gin.Context,
	conn *websocket.Conn,
	cancel handlers.APIHandlerCancelFunc,
	data <-chan []byte,
	errs <-chan *interfaces.ErrorMessage,
	wsTimelineLog websocketTimelineAppender,
	sessionID string,
	options responsesWebsocketForwardOptions,
) (responsesWebsocketForwardResult, error) {
	completed := false
	completedOutput := []byte("[]")
	completedResponseID := ""
	pendingToolCallIDs := make(map[string]struct{})
	downstreamCommitted := false
	startupBuffer := responsesWebsocketStartupBuffer{}
	bufferStartupPayloads := options.interceptPreviousResponseNotFound || options.interceptInvalidEncryptedContent || options.interceptProviderItemNotFound
	downstreamSessionKey := ""
	if c != nil && c.Request != nil {
		downstreamSessionKey = websocketDownstreamSessionKey(c.Request)
	}
	logAPIResponseError := func(errMsg *interfaces.ErrorMessage) {
		if h != nil {
			h.LoggingAPIResponseError(context.WithValue(context.Background(), "gin", c), errMsg)
		}
	}
	result := func(errMsg *interfaces.ErrorMessage, interceptedForRetry bool) responsesWebsocketForwardResult {
		return responsesWebsocketForwardResult{
			completedOutput:     completedOutput,
			completedResponseID: completedResponseID,
			pendingToolCallIDs:  sortedStringSet(pendingToolCallIDs),
			errorMessage:        errMsg,
			interceptedForRetry: interceptedForRetry,
		}
	}
	turnCanceledByCompaction := func() bool {
		return options.turnCanceledByCompaction != nil && options.turnCanceledByCompaction()
	}
	closeForTurnCompaction := func() (responsesWebsocketForwardResult, error) {
		startupBuffer.Reset()
		if conn != nil {
			deadline := time.Now().Add(time.Second)
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "compacted"), deadline)
		}
		cancel(context.Canceled)
		return result(nil, false), errResponsesWebsocketTurnCompacted
	}
	flushStartupBuffer := func() (bool, error) {
		payloads := startupBuffer.Drain()
		if len(payloads) == 0 {
			return false, nil
		}
		for _, payload := range payloads {
			markAPIResponseTimestamp(c)
			if errWrite := writeResponsesWebsocketPayload(conn, wsTimelineLog, payload, time.Now()); errWrite != nil {
				return true, errWrite
			}
		}
		return true, nil
	}
	handleStreamError := func(errMsg *interfaces.ErrorMessage) (responsesWebsocketForwardResult, error) {
		if errMsg != nil && turnCanceledByCompaction() {
			return closeForTurnCompaction()
		}
		if errMsg != nil && !downstreamCommitted {
			if options.interceptPreviousResponseNotFound && responsesWebsocketPreviousResponseNotFound(errMsg) {
				cancel(errMsg.Error)
				startupBuffer.Reset()
				return result(errMsg, true), nil
			}
			if options.interceptInvalidEncryptedContent && responsesWebsocketInvalidEncryptedContent(errMsg) {
				cancel(errMsg.Error)
				startupBuffer.Reset()
				return result(errMsg, true), nil
			}
			if options.interceptProviderItemNotFound && responsesWebsocketProviderItemNotFound(errMsg) {
				cancel(errMsg.Error)
				startupBuffer.Reset()
				return result(errMsg, true), nil
			}
		}
		if errMsg != nil && !downstreamCommitted {
			flushed, errFlush := flushStartupBuffer()
			if errFlush != nil {
				cancel(errFlush)
				return result(errMsg, false), errFlush
			}
			if flushed {
				downstreamCommitted = true
			}
		}
		if errMsg != nil {
			logAPIResponseError(errMsg)
			markAPIResponseTimestamp(c)
			errorPayload, errWrite := writeResponsesWebsocketError(conn, wsTimelineLog, errMsg)
			log.Infof(
				"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				sessionID,
				websocket.TextMessage,
				websocketPayloadEventType(errorPayload),
				websocketPayloadPreview(errorPayload),
			)
			if errWrite != nil {
				// log.Warnf(
				// 	"responses websocket: downstream_out write failed id=%s event=%s error=%v",
				// 	sessionID,
				// 	websocketPayloadEventType(errorPayload),
				// 	errWrite,
				// )
				cancel(errMsg.Error)
				return result(errMsg, false), errWrite
			}
			downstreamCommitted = true
		}
		if errMsg != nil {
			cancel(errMsg.Error)
		} else {
			cancel(nil)
		}
		return result(errMsg, false), nil
	}

	for {
		select {
		case <-c.Request.Context().Done():
			cancel(c.Request.Context().Err())
			startupBuffer.Reset()
			return result(nil, false), c.Request.Context().Err()
		case errMsg, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			return handleStreamError(errMsg)
		case chunk, ok := <-data:
			if !ok {
				if !completed {
					if turnCanceledByCompaction() {
						return closeForTurnCompaction()
					}
					if errs != nil {
						select {
						case errMsg, ok := <-errs:
							if ok {
								return handleStreamError(errMsg)
							}
							errs = nil
						case <-c.Request.Context().Done():
							cancel(c.Request.Context().Err())
							startupBuffer.Reset()
							return result(nil, false), c.Request.Context().Err()
						}
					}
					errMsg := &interfaces.ErrorMessage{
						StatusCode: http.StatusRequestTimeout,
						Error:      fmt.Errorf("stream closed before response.completed"),
					}
					logAPIResponseError(errMsg)
					if !downstreamCommitted {
						flushed, errFlush := flushStartupBuffer()
						if errFlush != nil {
							log.Warnf(
								"responses websocket: downstream_out write failed id=%s event=startup_buffer error=%v",
								sessionID,
								errFlush,
							)
							cancel(errFlush)
							return result(errMsg, false), errFlush
						}
						if flushed {
							downstreamCommitted = true
						}
					}
					markAPIResponseTimestamp(c)
					errorPayload, errWrite := writeResponsesWebsocketError(conn, wsTimelineLog, errMsg)
					log.Infof(
						"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
						sessionID,
						websocket.TextMessage,
						websocketPayloadEventType(errorPayload),
						websocketPayloadPreview(errorPayload),
					)
					if errWrite != nil {
						log.Warnf(
							"responses websocket: downstream_out write failed id=%s event=%s error=%v",
							sessionID,
							websocketPayloadEventType(errorPayload),
							errWrite,
						)
						cancel(errMsg.Error)
						return result(errMsg, false), errWrite
					}
					cancel(errMsg.Error)
					return result(errMsg, false), nil
				}
				cancel(nil)
				return result(nil, false), nil
			}

			payloads := websocketJSONPayloadsFromChunk(chunk)
			for i := range payloads {
				payloads[i] = responsesWebsocketPayloadWithLogicalPreviousResponseID(payloads[i], options.logicalPreviousResponseID)
				recordResponsesWebsocketToolCallsFromPayload(downstreamSessionKey, payloads[i])
				recordPendingToolCallIDsFromPayload(pendingToolCallIDs, payloads[i])
				eventType := gjson.GetBytes(payloads[i], "type").String()
				var payloadErrMsg *interfaces.ErrorMessage
				if eventType == wsEventTypeError {
					payloadErrMsg = responsesWebsocketErrorMessageFromPayload(payloads[i])
					logAPIResponseError(payloadErrMsg)
				} else if isResponsesWebsocketCompletionEvent(eventType) {
					completed = true
					completedOutput = responseCompletedOutputFromPayload(payloads[i])
					completedResponseID = responseCompletedIDFromPayload(payloads[i])
				} else if isResponsesWebsocketFailureTerminalEvent(eventType) {
					completed = true
				}
				if !downstreamCommitted {
					if options.interceptPreviousResponseNotFound && responsesWebsocketPayloadPreviousResponseNotFound(payloads[i]) {
						retryErrMsg := responsesWebsocketRetryErrorMessageFromPayload(payloads[i])
						cancel(retryErrMsg.Error)
						startupBuffer.Reset()
						return result(retryErrMsg, true), nil
					}
					if options.interceptInvalidEncryptedContent && responsesWebsocketPayloadInvalidEncryptedContent(payloads[i]) {
						retryErrMsg := responsesWebsocketRetryErrorMessageFromPayload(payloads[i])
						cancel(retryErrMsg.Error)
						startupBuffer.Reset()
						return result(retryErrMsg, true), nil
					}
					if options.interceptProviderItemNotFound && responsesWebsocketPayloadProviderItemNotFound(payloads[i]) {
						retryErrMsg := responsesWebsocketRetryErrorMessageFromPayload(payloads[i])
						cancel(retryErrMsg.Error)
						startupBuffer.Reset()
						return result(retryErrMsg, true), nil
					}
					if bufferStartupPayloads && responsesWebsocketBufferableStartupPayload(eventType) {
						if startupBuffer.TryAppend(payloads[i]) {
							continue
						}
						flushed, errFlush := flushStartupBuffer()
						if errFlush != nil {
							log.Warnf(
								"responses websocket: downstream_out write failed id=%s event=startup_buffer error=%v",
								sessionID,
								errFlush,
							)
							cancel(errFlush)
							return result(nil, false), errFlush
						}
						if flushed {
							downstreamCommitted = true
						}
						markAPIResponseTimestamp(c)
						if errWrite := writeResponsesWebsocketPayload(conn, wsTimelineLog, payloads[i], time.Now()); errWrite != nil {
							log.Warnf(
								"responses websocket: downstream_out write failed id=%s event=%s error=%v",
								sessionID,
								websocketPayloadEventType(payloads[i]),
								errWrite,
							)
							cancel(errWrite)
							return result(nil, false), errWrite
						}
						downstreamCommitted = true
						continue
					}
					flushed, errFlush := flushStartupBuffer()
					if errFlush != nil {
						log.Warnf(
							"responses websocket: downstream_out write failed id=%s event=startup_buffer error=%v",
							sessionID,
							errFlush,
						)
						cancel(errFlush)
						return result(nil, false), errFlush
					}
					if flushed {
						downstreamCommitted = true
					}
				}
				markAPIResponseTimestamp(c)
				// log.Infof(
				// 	"responses websocket: downstream_out id=%s type=%d event=%s payload=%s",
				// 	sessionID,
				// 	websocket.TextMessage,
				// 	websocketPayloadEventType(payloads[i]),
				// 	websocketPayloadPreview(payloads[i]),
				// )
				if errWrite := writeResponsesWebsocketPayload(conn, wsTimelineLog, payloads[i], time.Now()); errWrite != nil {
					log.Warnf(
						"responses websocket: downstream_out write failed id=%s event=%s error=%v",
						sessionID,
						websocketPayloadEventType(payloads[i]),
						errWrite,
					)
					cancel(errWrite)
					return result(nil, false), errWrite
				}
				downstreamCommitted = true
				if payloadErrMsg != nil {
					cancel(payloadErrMsg.Error)
					return result(payloadErrMsg, false), nil
				}
				if isResponsesWebsocketTerminalEvent(eventType) {
					cancel(nil)
					return result(nil, false), nil
				}
			}
		}
	}
}

func responsesErrorStatus(errMsg *interfaces.ErrorMessage) int {
	if errMsg == nil {
		return 0
	}
	if errMsg.StatusCode > 0 {
		return errMsg.StatusCode
	}
	if errMsg.Error != nil {
		if se, ok := errMsg.Error.(interface{ StatusCode() int }); ok && se != nil {
			return se.StatusCode()
		}
	}
	return 0
}

func responsesWebsocketPreviousResponseNotFound(errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil || errMsg.Error == nil {
		return false
	}
	status := responsesErrorStatus(errMsg)
	if status != 0 && status != http.StatusBadRequest && status != http.StatusNotFound && status < http.StatusInternalServerError {
		return false
	}
	return responsesWebsocketPayloadPreviousResponseNotFound([]byte(strings.TrimSpace(errMsg.Error.Error())))
}

func responsesWebsocketPayloadPreviousResponseNotFound(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	if gjson.ValidBytes(payload) {
		for _, path := range []string{"error.code", "body.error.code", "code", "response.error.code"} {
			if strings.TrimSpace(gjson.GetBytes(payload, path).String()) == "previous_response_not_found" {
				return true
			}
		}
	}
	lower := strings.ToLower(strings.TrimSpace(string(payload)))
	return strings.Contains(lower, "previous_response_not_found") ||
		(strings.Contains(lower, "previous response") && strings.Contains(lower, "not found")) ||
		(strings.Contains(lower, "previous_response_id") && strings.Contains(lower, "unsupported")) ||
		(strings.Contains(lower, "previous_response_id") && strings.Contains(lower, "not supported")) ||
		(strings.Contains(lower, "previous response") && strings.Contains(lower, "unsupported")) ||
		(strings.Contains(lower, "previous response") && strings.Contains(lower, "not supported")) ||
		strings.Contains(lower, "invalid previous response") ||
		(strings.Contains(lower, "unknown response") && strings.Contains(lower, "id"))
}

func responsesWebsocketInvalidEncryptedContent(errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil || errMsg.Error == nil {
		return false
	}
	status := responsesErrorStatus(errMsg)
	if status != 0 && status != http.StatusBadRequest && status < http.StatusInternalServerError {
		return false
	}
	return responsesreplay.Classify(status, errMsg.Error.Error()) == responsesreplay.ErrorInvalidEncryptedContent
}

func responsesWebsocketPayloadInvalidEncryptedContent(payload []byte) bool {
	return responsesreplay.Classify(http.StatusBadRequest, string(payload)) == responsesreplay.ErrorInvalidEncryptedContent
}

func responsesWebsocketInvalidEncryptedContentMessage(message string) bool {
	return responsesreplay.Classify(http.StatusBadRequest, message) == responsesreplay.ErrorInvalidEncryptedContent
}

func responsesWebsocketProviderItemNotFound(errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil || errMsg.Error == nil {
		return false
	}
	status := responsesErrorStatus(errMsg)
	if status != 0 && status != http.StatusBadRequest && status != http.StatusNotFound && status < http.StatusInternalServerError {
		return false
	}
	return responsesreplay.Classify(status, errMsg.Error.Error()) == responsesreplay.ErrorProviderStateNotFound
}

func responsesWebsocketPayloadProviderItemNotFound(payload []byte) bool {
	return responsesreplay.Classify(http.StatusBadRequest, string(payload)) == responsesreplay.ErrorProviderStateNotFound
}

func responsesWebsocketProviderItemNotFoundMessage(message string) bool {
	return responsesreplay.Classify(http.StatusBadRequest, message) == responsesreplay.ErrorProviderStateNotFound
}

func responsesWebsocketRetryErrorMessageFromPayload(payload []byte) *interfaces.ErrorMessage {
	status := int(gjson.GetBytes(payload, "status").Int())
	if status <= 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	if status <= 0 {
		status = http.StatusBadRequest
	}
	return &interfaces.ErrorMessage{StatusCode: status, Error: fmt.Errorf("%s", strings.TrimSpace(string(payload)))}
}

func stripResponsesWebsocketReasoningEncryptedContent(payload []byte) ([]byte, bool) {
	return responsesreplay.Render(payload, responsesreplay.AttemptWithoutReasoningEncryptedContent)
}

func sanitizeResponsesWebsocketPortableReplay(payload []byte) ([]byte, bool) {
	return responsesreplay.Render(payload, responsesreplay.AttemptPortableTranscript)
}

func stripResponsesWebsocketReasoningEncryptedInclude(payload []byte) ([]byte, bool) {
	include := gjson.GetBytes(payload, "include")
	if !include.Exists() || !include.IsArray() {
		return payload, false
	}
	kept := make([]string, 0, len(include.Array()))
	changed := false
	for _, item := range include.Array() {
		if strings.TrimSpace(item.String()) == "reasoning.encrypted_content" {
			changed = true
			continue
		}
		kept = append(kept, item.Raw)
	}
	if !changed {
		return payload, false
	}
	if len(kept) == 0 {
		updated, err := sjson.DeleteBytes(payload, "include")
		return updated, err == nil
	}
	updated, err := sjson.SetRawBytes(payload, "include", []byte("["+strings.Join(kept, ",")+"]"))
	return updated, err == nil
}

type responsesWebsocketCheckpoint struct {
	responseID string
	request    []byte
	output     []byte
}

func (c responsesWebsocketCheckpoint) Clone() responsesWebsocketCheckpoint {
	return responsesWebsocketCheckpoint{
		responseID: c.responseID,
		request:    bytes.Clone(c.request),
		output:     bytes.Clone(c.output),
	}
}

func responsesWebsocketRollbackRequest(fullReplay []byte, checkpoint responsesWebsocketCheckpoint) []byte {
	if len(fullReplay) == 0 || checkpoint.responseID == "" || len(checkpoint.request) == 0 {
		return nil
	}
	fullInput := gjson.GetBytes(fullReplay, "input")
	checkpointInput := gjson.GetBytes(checkpoint.request, "input")
	if !fullInput.IsArray() || !checkpointInput.IsArray() {
		return nil
	}

	baselineRaw, errMerge := mergeJSONArrayRaw(checkpointInput.Raw, normalizeJSONArrayRaw(checkpoint.output))
	if errMerge != nil {
		return nil
	}
	baseline := gjson.Parse(baselineRaw)
	if !baseline.IsArray() {
		return nil
	}

	fullItems := fullInput.Array()
	baselineItems := baseline.Array()
	if len(fullItems) <= len(baselineItems) {
		return nil
	}
	for i := range baselineItems {
		if !responsesWebsocketJSONEqual(fullItems[i].Raw, baselineItems[i].Raw) {
			return nil
		}
	}

	tail := make([]string, 0, len(fullItems)-len(baselineItems))
	for _, item := range fullItems[len(baselineItems):] {
		tail = append(tail, item.Raw)
	}
	rollback := bytes.Clone(fullReplay)
	rollback, _ = sjson.SetBytes(rollback, "previous_response_id", checkpoint.responseID)
	rollback, errSet := sjson.SetRawBytes(rollback, "input", []byte("["+strings.Join(tail, ",")+"]"))
	if errSet != nil {
		return nil
	}
	return rollback
}

func responsesWebsocketJSONEqual(left string, right string) bool {
	var leftValue any
	var rightValue any
	if err := json.Unmarshal([]byte(left), &leftValue); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(right), &rightValue); err != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func isResponsesWebsocketTerminalEvent(eventType string) bool {
	return isResponsesWebsocketCompletionEvent(eventType) || eventType == wsEventTypeError || isResponsesWebsocketFailureTerminalEvent(eventType)
}

func isResponsesWebsocketFailureTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.failed", "response.incomplete", "response.cancelled":
		return true
	default:
		return false
	}
}

func shouldReleaseResponsesWebsocketPinnedAuth(errMsg *interfaces.ErrorMessage) bool {
	if errMsg == nil {
		return false
	}
	status := responsesErrorStatus(errMsg)
	switch status {
	case http.StatusUnauthorized,
		http.StatusPaymentRequired,
		http.StatusForbidden,
		http.StatusTooManyRequests,
		http.StatusRequestTimeout,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
	}
	if errMsg.Error != nil {
		msg := strings.ToLower(errMsg.Error.Error())
		switch {
		case strings.Contains(msg, "stream closed before response.completed"),
			strings.Contains(msg, "previous_response_not_found"),
			strings.Contains(msg, "ws_failed"),
			strings.Contains(msg, "upstream stream closed before first payload"),
			strings.Contains(msg, "empty_stream"):
			return true
		}
	}
	return false
}

func responseCompletedOutputFromPayload(payload []byte) []byte {
	output := gjson.GetBytes(payload, "response.output")
	if output.Exists() && output.IsArray() {
		return bytes.Clone([]byte(output.Raw))
	}
	return []byte("[]")
}

func responseCompletedIDFromPayload(payload []byte) string {
	return strings.TrimSpace(gjson.GetBytes(payload, "response.id").String())
}

func recordPendingToolCallIDsFromPayload(pending map[string]struct{}, payload []byte) {
	if pending == nil || len(payload) == 0 {
		return
	}
	updatePendingToolCallIDsFromItem(pending, gjson.GetBytes(payload, "item"))
	output := gjson.GetBytes(payload, "response.output")
	if output.IsArray() {
		for _, item := range output.Array() {
			updatePendingToolCallIDsFromItem(pending, item)
		}
	}
}

func updatePendingToolCallIDsFromItem(pending map[string]struct{}, item gjson.Result) {
	if pending == nil || !item.Exists() {
		return
	}
	switch strings.TrimSpace(item.Get("type").String()) {
	case "function_call", "custom_tool_call", "tool_search_call":
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID != "" {
			pending[callID] = struct{}{}
		}
	case "function_call_output", "custom_tool_call_output", "tool_search_output":
		callID := strings.TrimSpace(item.Get("call_id").String())
		if callID != "" {
			delete(pending, callID)
		}
	}
}

func sortedStringSet(values map[string]struct{}) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func websocketJSONPayloadsFromChunk(chunk []byte) [][]byte {
	payloads := make([][]byte, 0, 2)
	lines := bytes.Split(chunk, []byte("\n"))
	for i := range lines {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || bytes.HasPrefix(line, []byte("event:")) {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(line[len("data:"):])
		}
		if len(line) == 0 || bytes.Equal(line, []byte(wsDoneMarker)) {
			continue
		}
		if json.Valid(line) {
			payloads = append(payloads, bytes.Clone(line))
		}
	}

	if len(payloads) > 0 {
		return payloads
	}

	trimmed := bytes.TrimSpace(chunk)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		trimmed = bytes.TrimSpace(trimmed[len("data:"):])
	}
	if len(trimmed) > 0 && !bytes.Equal(trimmed, []byte(wsDoneMarker)) && json.Valid(trimmed) {
		payloads = append(payloads, bytes.Clone(trimmed))
	}
	return payloads
}

func writeResponsesWebsocketError(conn *websocket.Conn, wsTimelineLog websocketTimelineAppender, errMsg *interfaces.ErrorMessage) ([]byte, error) {
	status := http.StatusInternalServerError
	errText := http.StatusText(status)
	if errMsg != nil {
		if errMsg.StatusCode > 0 {
			status = errMsg.StatusCode
			errText = http.StatusText(status)
		}
		if errMsg.Error != nil && strings.TrimSpace(errMsg.Error.Error()) != "" {
			errText = errMsg.Error.Error()
		}
	}

	body := handlers.BuildErrorResponseBody(status, errText)
	payload := []byte(`{}`)
	var errSet error
	payload, errSet = sjson.SetBytes(payload, "type", wsEventTypeError)
	if errSet != nil {
		return nil, errSet
	}
	payload, errSet = sjson.SetBytes(payload, "status", status)
	if errSet != nil {
		return nil, errSet
	}

	if errMsg != nil && errMsg.Addon != nil {
		headers := []byte(`{}`)
		hasHeaders := false
		for key, values := range errMsg.Addon {
			if len(values) == 0 {
				continue
			}
			headerPath := strings.ReplaceAll(strings.ReplaceAll(key, `\\`, `\\\\`), ".", `\\.`)
			headers, errSet = sjson.SetBytes(headers, headerPath, values[0])
			if errSet != nil {
				return nil, errSet
			}
			hasHeaders = true
		}
		if hasHeaders {
			payload, errSet = sjson.SetRawBytes(payload, "headers", headers)
			if errSet != nil {
				return nil, errSet
			}
		}
	}

	if len(body) > 0 && json.Valid(body) {
		errorNode := gjson.GetBytes(body, "error")
		if errorNode.Exists() {
			payload, errSet = sjson.SetRawBytes(payload, "error", []byte(errorNode.Raw))
		} else {
			payload, errSet = sjson.SetRawBytes(payload, "error", body)
		}
		if errSet != nil {
			return nil, errSet
		}
	}

	if !gjson.GetBytes(payload, "error").Exists() {
		payload, errSet = sjson.SetBytes(payload, "error.type", "server_error")
		if errSet != nil {
			return nil, errSet
		}
		payload, errSet = sjson.SetBytes(payload, "error.message", errText)
		if errSet != nil {
			return nil, errSet
		}
	}

	return payload, writeResponsesWebsocketPayload(conn, wsTimelineLog, payload, time.Now())
}

func appendWebsocketEvent(builder *strings.Builder, eventType string, payload []byte) {
	if builder == nil {
		return
	}
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return
	}
	if builder.Len() > 0 {
		builder.WriteString("\n")
	}
	builder.WriteString("websocket.")
	builder.WriteString(eventType)
	builder.WriteString("\n")
	builder.Write(trimmedPayload)
	builder.WriteString("\n")
}

func websocketPayloadEventType(payload []byte) string {
	eventType := strings.TrimSpace(gjson.GetBytes(payload, "type").String())
	if eventType == "" {
		return "-"
	}
	return eventType
}

func isResponsesWebsocketCompletionEvent(eventType string) bool {
	return eventType == wsEventTypeCompleted || eventType == wsEventTypeDone
}

func responsesWebsocketErrorMessageFromPayload(payload []byte) *interfaces.ErrorMessage {
	status := int(gjson.GetBytes(payload, "status").Int())
	if status <= 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}
	if status <= 0 {
		status = http.StatusInternalServerError
	}

	errText := strings.TrimSpace(gjson.GetBytes(payload, "error.message").String())
	if errText == "" {
		errText = strings.TrimSpace(gjson.GetBytes(payload, "message").String())
	}
	if errText == "" {
		errText = strings.TrimSpace(string(payload))
	}
	if errText == "" {
		errText = http.StatusText(status)
	}
	return &interfaces.ErrorMessage{StatusCode: status, Error: fmt.Errorf("%s", errText)}
}

func websocketPayloadPreview(payload []byte) string {
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return "<empty>"
	}
	previewText := strings.ReplaceAll(string(trimmedPayload), "\n", "\\n")
	previewText = strings.ReplaceAll(previewText, "\r", "\\r")
	return previewText
}

func setWebsocketTimelineBody(c *gin.Context, body string) {
	setWebsocketBody(c, wsTimelineBodyKey, body)
}

func setWebsocketBody(c *gin.Context, key string, body string) {
	if c == nil {
		return
	}
	trimmedBody := strings.TrimSpace(body)
	if trimmedBody == "" {
		return
	}
	c.Set(key, []byte(trimmedBody))
}

func writeResponsesWebsocketPayload(conn *websocket.Conn, wsTimelineLog websocketTimelineAppender, payload []byte, timestamp time.Time) error {
	if wsTimelineLog != nil {
		wsTimelineLog.Append("response", payload, timestamp)
	}
	return conn.WriteMessage(websocket.TextMessage, payload)
}

func appendWebsocketTimelineDisconnect(timeline websocketTimelineAppender, err error, timestamp time.Time) {
	if err == nil {
		return
	}
	if timeline != nil {
		timeline.Append("disconnect", []byte(err.Error()), timestamp)
	}
}

func appendWebsocketTimelineEvent(builder *strings.Builder, eventType string, payload []byte, timestamp time.Time) {
	if builder == nil {
		return
	}
	writeWebsocketTimelineBuilder(builder, formatWebsocketTimelineEvent(eventType, payload, timestamp))
}

func formatWebsocketTimelineEvent(eventType string, payload []byte, timestamp time.Time) []byte {
	trimmedPayload := bytes.TrimSpace(payload)
	if len(trimmedPayload) == 0 {
		return nil
	}
	var builder strings.Builder
	builder.WriteString("Timestamp: ")
	builder.WriteString(timestamp.Format(time.RFC3339Nano))
	builder.WriteString("\n")
	builder.WriteString("Event: websocket.")
	builder.WriteString(eventType)
	builder.WriteString("\n")
	builder.Write(trimmedPayload)
	builder.WriteString("\n")
	return []byte(builder.String())
}

func markAPIResponseTimestamp(c *gin.Context) {
	if c == nil {
		return
	}
	if _, exists := c.Get("API_RESPONSE_TIMESTAMP"); exists {
		return
	}
	c.Set("API_RESPONSE_TIMESTAMP", time.Now())
}
