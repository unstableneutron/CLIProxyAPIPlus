package openai

import (
	"bytes"
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai/responsesreplay"
)

type responsesReplayExecution struct {
	ctx       context.Context
	modelName string
	payload   []byte
	alt       string
}

type responsesReplayPlannerState struct {
	attempt responsesreplay.Attempt
}

func (s *responsesReplayPlannerState) nextPayload(base []byte, errMsg *interfaces.ErrorMessage) ([]byte, bool) {
	nextPayload, nextAttempt, ok := nextResponsesReplayPayload(base, s.attempt, errMsg)
	if !ok {
		return nil, false
	}
	s.attempt = nextAttempt
	return nextPayload, true
}

func (h *OpenAIResponsesAPIHandler) executeResponsesWithReplayRetries(req responsesReplayExecution) ([]byte, http.Header, *interfaces.ErrorMessage) {
	replay := responsesReplayPlannerState{}
	payload := bytes.Clone(req.payload)
	for {
		resp, headers, errMsg := h.ExecuteWithAuthManager(req.ctx, h.HandlerType(), req.modelName, payload, req.alt)
		if errMsg == nil {
			return resp, headers, nil
		}

		nextPayload, ok := replay.nextPayload(req.payload, errMsg)
		if !ok {
			return resp, headers, errMsg
		}
		payload = nextPayload
	}
}

func nextResponsesReplayPayload(base []byte, attempt responsesreplay.Attempt, errMsg *interfaces.ErrorMessage) ([]byte, responsesreplay.Attempt, bool) {
	if errMsg == nil {
		return nil, attempt, false
	}
	status := responsesErrorStatus(errMsg)
	message := ""
	if errMsg.Error != nil {
		message = errMsg.Error.Error()
	}
	next, ok := responsesreplay.NextAttempt(attempt, responsesreplay.Classify(status, message))
	if !ok {
		return nil, attempt, false
	}
	nextPayload, changed := responsesreplay.Render(base, next)
	if !changed {
		return nil, attempt, false
	}
	return nextPayload, next, true
}
