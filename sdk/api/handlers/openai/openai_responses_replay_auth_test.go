package openai

import (
	"bytes"
	"context"
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

type responsesReplayAuthCaptureExecutor struct {
	compactCaptureExecutor
	mu       sync.Mutex
	authIDs  []string
	payloads [][]byte
}

func (e *responsesReplayAuthCaptureExecutor) Execute(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (coreexecutor.Response, error) {
	e.mu.Lock()
	e.authIDs = append(e.authIDs, auth.ID)
	e.payloads = append(e.payloads, bytes.Clone(req.Payload))
	attempt := len(e.authIDs)
	e.mu.Unlock()
	if attempt == 1 {
		return coreexecutor.Response{}, responsesReplayInvalidEncryptedContentError{}
	}
	return coreexecutor.Response{Payload: []byte(`{"id":"resp-replayed","status":"completed","output":[]}`)}, nil
}

type responsesReplayInvalidEncryptedContentError struct{}

func (responsesReplayInvalidEncryptedContentError) Error() string {
	return `{"error":{"type":"invalid_request_error","code":"invalid_encrypted_content","message":"encrypted content could not be verified"}}`
}

func (responsesReplayInvalidEncryptedContentError) StatusCode() int { return http.StatusBadRequest }

type responsesReplayStreamCancellationExecutor struct {
	compactCaptureExecutor
	mu                        sync.Mutex
	authIDs                   []string
	payloads                  [][]byte
	firstCtx                  context.Context
	secondStartedBeforeCancel bool
}

func (e *responsesReplayStreamCancellationExecutor) ExecuteStream(ctx context.Context, auth *coreauth.Auth, req coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.mu.Lock()
	e.authIDs = append(e.authIDs, auth.ID)
	e.payloads = append(e.payloads, bytes.Clone(req.Payload))
	attempt := len(e.authIDs)
	if attempt == 1 {
		e.firstCtx = ctx
	}
	firstCtx := e.firstCtx
	e.mu.Unlock()

	if attempt == 1 {
		chunks := make(chan coreexecutor.StreamChunk, 2)
		chunks <- coreexecutor.StreamChunk{Bootstrap: true}
		chunks <- coreexecutor.StreamChunk{Err: responsesReplayInvalidEncryptedContentError{}}
		go func() {
			<-ctx.Done()
			close(chunks)
		}()
		return &coreexecutor.StreamResult{Chunks: chunks}, nil
	}

	if firstCtx == nil || firstCtx.Err() == nil {
		e.mu.Lock()
		e.secondStartedBeforeCancel = true
		e.mu.Unlock()
	}
	chunks := make(chan coreexecutor.StreamChunk, 2)
	chunks <- coreexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n")}
	chunks <- coreexecutor.StreamChunk{Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-replayed\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")}
	close(chunks)
	return &coreexecutor.StreamResult{Chunks: chunks}, nil
}

func TestResponsesReplayRetryStaysOnOwningAuth(t *testing.T) {
	executor := &responsesReplayAuthCaptureExecutor{}
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	for _, authID := range []string{"responses-replay-auth-a", "responses-replay-auth-b"} {
		auth := &coreauth.Auth{ID: authID, Provider: executor.Identifier(), Status: coreauth.StatusActive}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", authID, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "responses-replay-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}

	handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	payload := []byte(`{"model":"responses-replay-model","input":[{"type":"reasoning","encrypted_content":"account-a-state"}]}`)
	resp, _, errMsg := handler.executeResponsesWithReplayRetries(responsesReplayExecution{
		ctx:       context.Background(),
		modelName: "responses-replay-model",
		payload:   payload,
	})
	if errMsg != nil {
		t.Fatalf("replay execution failed: %v", errMsg.Error)
	}
	if gjson.GetBytes(resp, "id").String() != "resp-replayed" {
		t.Fatalf("response = %s, want replay success", resp)
	}

	executor.mu.Lock()
	authIDs := append([]string(nil), executor.authIDs...)
	payloads := append([][]byte(nil), executor.payloads...)
	executor.mu.Unlock()
	if len(authIDs) != 2 {
		t.Fatalf("attempt count = %d, want 2", len(authIDs))
	}
	if authIDs[0] == "" || authIDs[1] != authIDs[0] {
		t.Fatalf("replay migrated across auths: %v", authIDs)
	}
	if gjson.GetBytes(payloads[1], "input.0.encrypted_content").Exists() {
		t.Fatalf("replay retry retained rejected encrypted content: %s", payloads[1])
	}
}

func TestResponsesReplayAuthOwnerSealsFinalInitialSelection(t *testing.T) {
	owner := &responsesReplayAuthOwner{}
	owner.observe("auth-a")
	owner.observe("auth-b")
	owner.seal()

	var wg sync.WaitGroup
	for index := 0; index < 32; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			owner.observe("late-auth")
		}()
	}
	wg.Wait()

	if got := owner.id(); got != "auth-b" {
		t.Fatalf("sealed replay owner = %q, want final initial selection auth-b", got)
	}
}

func TestResponsesStreamReplayCancelsRejectedAttemptBeforeRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	executor := &responsesReplayStreamCancellationExecutor{}
	manager := coreauth.NewManager(nil, &coreauth.RoundRobinSelector{}, nil)
	manager.RegisterExecutor(executor)
	for _, authID := range []string{"responses-stream-replay-auth-a", "responses-stream-replay-auth-b"} {
		auth := &coreauth.Auth{ID: authID, Provider: executor.Identifier(), Status: coreauth.StatusActive}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register %s: %v", authID, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "responses-stream-replay-model"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}
	handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
	router := gin.New()
	router.POST("/v1/responses", handler.Responses)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"responses-stream-replay-model","stream":true,"input":[{"type":"reasoning","encrypted_content":"account-a-state"}]}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "resp-replayed") {
		t.Fatalf("status/body = %d/%q, want replayed stream", recorder.Code, recorder.Body.String())
	}
	executor.mu.Lock()
	defer executor.mu.Unlock()
	if len(executor.authIDs) != 2 || executor.authIDs[0] != executor.authIDs[1] {
		t.Fatalf("stream replay auths = %v, want two attempts on one auth", executor.authIDs)
	}
	if executor.secondStartedBeforeCancel {
		t.Fatal("stream replay started before rejected provider attempt was canceled")
	}
	if gjson.GetBytes(executor.payloads[1], "input.0.encrypted_content").Exists() {
		t.Fatalf("stream replay retained rejected encrypted content: %s", executor.payloads[1])
	}
}
