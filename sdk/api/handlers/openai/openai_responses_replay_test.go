package openai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

type responsesReplayCaptureExecutor struct {
	mu        sync.Mutex
	payloads  [][]byte
	successes int
}

func (e *responsesReplayCaptureExecutor) Identifier() string { return "openai-compatibility" }

func (e *responsesReplayCaptureExecutor) Execute(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	if err := e.recordAndMaybeFail(req.Payload); err != nil {
		return coreexecutor.Response{}, err
	}

	successes := e.recordSuccess()
	return coreexecutor.Response{Payload: []byte(fmt.Sprintf(`{"id":"resp-rest-%d","object":"response","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`, successes))}, nil
}

func (e *responsesReplayCaptureExecutor) ExecuteStream(_ context.Context, _ *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	chunks := make(chan coreexecutor.StreamChunk, 1)
	if err := e.recordAndMaybeFail(req.Payload); err != nil {
		chunks <- coreexecutor.StreamChunk{Err: err}
		close(chunks)
		return &coreexecutor.StreamResult{Chunks: chunks}, nil
	}

	successes := e.recordSuccess()
	chunks <- coreexecutor.StreamChunk{Payload: []byte(fmt.Sprintf("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-stream-%d\",\"output\":[]}}\n\n", successes))}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func (e *responsesReplayCaptureExecutor) recordAndMaybeFail(payload []byte) error {
	e.mu.Lock()
	e.payloads = append(e.payloads, bytes.Clone(payload))
	e.mu.Unlock()

	if gjson.GetBytes(payload, "previous_response_id").Exists() || responsesWebsocketInputHasField(payload, "id") {
		return websocketPinnedFailoverStatusError{
			status: http.StatusBadRequest,
			msg:    `{"error":{"message":"input item not found","type":"invalid_request_error","code":"item_not_found","param":"input[0].id"}}`,
		}
	}
	if bytes.Contains(payload, []byte("reasoning.encrypted_content")) ||
		responsesWebsocketInputHasNonCompactionEncryptedContent(payload) {
		return websocketPinnedFailoverStatusError{
			status: http.StatusBadRequest,
			msg:    `{"error":{"message":"Encrypted content could not be verified","type":"invalid_request_error","code":"invalid_encrypted_content"}}`,
		}
	}
	return nil
}

func (e *responsesReplayCaptureExecutor) recordSuccess() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.successes++
	return e.successes
}

func (e *responsesReplayCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *responsesReplayCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *responsesReplayCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func (e *responsesReplayCaptureExecutor) Payloads() [][]byte {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([][]byte, len(e.payloads))
	for i := range e.payloads {
		out[i] = bytes.Clone(e.payloads[i])
	}
	return out
}

func TestResponsesRetriesProviderReplayStateForNonStreamingRequest(t *testing.T) {
	// Given
	gin.SetMode(gin.TestMode)
	modelName := "rest-replay-model"
	executor := &responsesReplayCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       "auth-rest-replay",
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: modelName}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", h.Responses)

	request := fmt.Sprintf(`{
		"model":%q,
		"previous_response_id":"resp_chatgpt",
		"include":["reasoning.encrypted_content","web_search_call.results"],
		"input":[
			{"type":"reasoning","id":"rs_1","encrypted_content":"rsn_chatgpt","summary":[{"type":"summary_text","text":"kept reasoning summary"}]},
			{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"kept assistant text"}]},
			{"type":"compaction","encrypted_content":"kept compacted transcript"},
			{"type":"message","id":"msg_2","role":"user","content":"next"}
		]
	}`, modelName)

	// When
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(request))
	router.ServeHTTP(recorder, req)

	// Then
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	payloads := executor.Payloads()
	if len(payloads) != 3 {
		t.Fatalf("upstream payload count = %d, want original plus two retries", len(payloads))
	}
	if !gjson.GetBytes(payloads[0], "previous_response_id").Exists() || !responsesWebsocketInputHasField(payloads[0], "id") {
		t.Fatalf("first payload missing provider state: %s", payloads[0])
	}
	if gjson.GetBytes(payloads[1], "previous_response_id").Exists() || responsesWebsocketInputHasField(payloads[1], "id") {
		t.Fatalf("identifier retry kept provider ids: %s", payloads[1])
	}
	if got := gjson.GetBytes(payloads[1], "input.0.encrypted_content").String(); got != "rsn_chatgpt" {
		t.Fatalf("identifier retry encrypted_content = %q, want preserved: %s", got, payloads[1])
	}
	if gjson.GetBytes(payloads[2], "input.0.encrypted_content").Exists() {
		t.Fatalf("portable retry kept reasoning encrypted content: %s", payloads[2])
	}
	if got := gjson.GetBytes(payloads[2], "input.2.encrypted_content").String(); got != "kept compacted transcript" {
		t.Fatalf("portable retry compaction encrypted_content = %q, want preserved: %s", got, payloads[2])
	}
	if got := gjson.GetBytes(payloads[2], "input.0.summary.0.text").String(); got != "kept reasoning summary" {
		t.Fatalf("portable retry reasoning summary = %q, want preserved: %s", got, payloads[2])
	}
}

func TestResponsesRetriesProviderReplayStateForStreamingRequest(t *testing.T) {
	// Given
	gin.SetMode(gin.TestMode)
	modelName := "rest-replay-stream-model"
	executor := &responsesReplayCaptureExecutor{}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	auth := &coreauth.Auth{
		ID:       "auth-rest-replay-stream",
		Provider: executor.Identifier(),
		Status:   coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("Register auth: %v", err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: modelName}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	h := NewOpenAIResponsesAPIHandler(base)
	router := gin.New()
	router.POST("/v1/responses", h.Responses)

	request := fmt.Sprintf(`{
		"model":%q,
		"stream":true,
		"previous_response_id":"resp_chatgpt",
		"include":["reasoning.encrypted_content","web_search_call.results"],
		"input":[
			{"type":"reasoning","id":"rs_1","encrypted_content":"rsn_chatgpt","summary":[{"type":"summary_text","text":"kept reasoning summary"}]},
			{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"kept assistant text"}]},
			{"type":"compaction","encrypted_content":"kept compacted transcript"},
			{"type":"message","id":"msg_2","role":"user","content":"next"}
		]
	}`, modelName)

	// When
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(request))
	router.ServeHTTP(recorder, req)

	// Then
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"response.completed"`) {
		t.Fatalf("stream body missing completed event: %s", recorder.Body.String())
	}
	payloads := executor.Payloads()
	if len(payloads) != 3 {
		t.Fatalf("upstream payload count = %d, want original plus two retries", len(payloads))
	}
	if !responsesWebsocketInputHasField(payloads[0], "id") {
		t.Fatalf("first streaming payload missing provider item ids: %s", payloads[0])
	}
	if gjson.GetBytes(payloads[1], "previous_response_id").Exists() || responsesWebsocketInputHasField(payloads[1], "id") {
		t.Fatalf("identifier retry kept provider ids: %s", payloads[1])
	}
	if got := gjson.GetBytes(payloads[1], "input.0.encrypted_content").String(); got != "rsn_chatgpt" {
		t.Fatalf("identifier retry encrypted_content = %q, want preserved: %s", got, payloads[1])
	}
	if gjson.GetBytes(payloads[2], "input.0.encrypted_content").Exists() {
		t.Fatalf("portable retry kept reasoning encrypted content: %s", payloads[2])
	}
	if got := gjson.GetBytes(payloads[2], "input.2.encrypted_content").String(); got != "kept compacted transcript" {
		t.Fatalf("portable retry compaction encrypted_content = %q, want preserved: %s", got, payloads[2])
	}
}
