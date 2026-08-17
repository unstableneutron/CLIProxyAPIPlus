package openai

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai/responsesreplay"
)

func responsesErrorStatus(errMsg *interfaces.ErrorMessage) int {
	if errMsg == nil {
		return 0
	}
	if errMsg.StatusCode > 0 {
		return errMsg.StatusCode
	}
	if statusErr, ok := errMsg.Error.(interface{ StatusCode() int }); ok && statusErr != nil {
		return statusErr.StatusCode()
	}
	return 0
}

type responsesReplayExecution struct {
	ctx       context.Context
	modelName string
	payload   []byte
	alt       string
	owner     *responsesReplayAuthOwner
}

type responsesReplayPlannerState struct {
	attempt responsesreplay.Attempt
}

type responsesReplayAuthOwner struct {
	mu     sync.RWMutex
	authID string
	sealed bool
}

func (o *responsesReplayAuthOwner) observe(authID string) {
	if o == nil || strings.TrimSpace(authID) == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sealed {
		return
	}
	o.authID = strings.TrimSpace(authID)
}

func (o *responsesReplayAuthOwner) seal() {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.sealed = true
	o.mu.Unlock()
}

func (o *responsesReplayAuthOwner) pin(ctx context.Context) context.Context {
	if o == nil {
		return ctx
	}
	o.mu.RLock()
	authID := o.authID
	o.mu.RUnlock()
	return handlers.WithPinnedAuthID(ctx, authID)
}

func (o *responsesReplayAuthOwner) id() string {
	if o == nil {
		return ""
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.authID
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
	owner := req.owner
	if owner == nil {
		owner = &responsesReplayAuthOwner{}
	}
	executionCtx := handlers.WithAdditionalSelectedAuthIDCallback(req.ctx, owner.observe)
	payload := bytes.Clone(req.payload)
	for {
		attemptCtx := executionCtx
		if replay.attempt != responsesreplay.AttemptOriginal {
			attemptCtx = owner.pin(attemptCtx)
		}
		resp, headers, errMsg := h.ExecuteWithAuthManager(attemptCtx, h.HandlerType(), req.modelName, payload, req.alt)
		owner.seal()
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
