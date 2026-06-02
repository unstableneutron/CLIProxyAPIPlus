package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

type forceModelPrefixCaptureExecutor struct {
	authID          string
	requestModel    string
	payloadModel    string
	requestedModel  string
	forwardedHeader string
	calls           int
}

func (e *forceModelPrefixCaptureExecutor) Identifier() string { return "codex" }

func (e *forceModelPrefixCaptureExecutor) Execute(_ context.Context, auth *coreauth.Auth, req coreexecutor.Request, opts coreexecutor.Options) (coreexecutor.Response, error) {
	e.calls++
	if auth != nil {
		e.authID = auth.ID
	}
	e.requestModel = req.Model
	e.payloadModel = gjson.GetBytes(req.Payload, "model").String()
	if opts.Metadata != nil {
		if requested, ok := opts.Metadata[coreexecutor.RequestedModelMetadataKey].(string); ok {
			e.requestedModel = requested
		}
	}
	if opts.Headers != nil {
		e.forwardedHeader = opts.Headers.Get(handlers.ForceModelPrefixHeader)
	}
	return coreexecutor.Response{Payload: []byte(`{"id":"resp-test","object":"chat.completion","choices":[]}`)}, nil
}

func (e *forceModelPrefixCaptureExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	return nil, errors.New("not implemented")
}

func (e *forceModelPrefixCaptureExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *forceModelPrefixCaptureExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *forceModelPrefixCaptureExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func newForceModelPrefixTestHandler(t *testing.T, exec *forceModelPrefixCaptureExecutor) *OpenAIAPIHandler {
	t.Helper()

	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(exec)

	auth := &coreauth.Auth{
		ID:       "force-prefix-productivity-auth",
		Provider: "codex",
		Prefix:   "productivity",
		Status:   coreauth.StatusActive,
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("manager.Register: %v", err)
	}

	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "productivity/gpt-5.5"}})
	manager.RefreshSchedulerEntry(auth.ID)
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})

	return NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager))
}

func performForceModelPrefixChatRequest(handler *OpenAIAPIHandler, headerValue string) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/v1/chat/completions", handler.ChatCompletions)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	if headerValue != "" {
		req.Header.Set("X-Force-Model-Prefix", headerValue)
	}
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	return resp
}

func TestChatCompletionsForceModelPrefixHeaderRoutesPrefixedAuth(t *testing.T) {
	exec := &forceModelPrefixCaptureExecutor{}
	handler := newForceModelPrefixTestHandler(t, exec)

	resp := performForceModelPrefixChatRequest(handler, "productivity/")

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusOK, resp.Body.String())
	}
	if exec.calls != 1 {
		t.Fatalf("executor calls = %d, want 1", exec.calls)
	}
	if exec.authID != "force-prefix-productivity-auth" {
		t.Fatalf("authID = %q, want force-prefix-productivity-auth", exec.authID)
	}
	if exec.requestModel != "gpt-5.5" {
		t.Fatalf("request model = %q, want gpt-5.5", exec.requestModel)
	}
	if exec.payloadModel != "productivity/gpt-5.5" {
		t.Fatalf("payload model = %q, want productivity/gpt-5.5", exec.payloadModel)
	}
	if exec.requestedModel != "productivity/gpt-5.5" {
		t.Fatalf("requested model metadata = %q, want productivity/gpt-5.5", exec.requestedModel)
	}
	if exec.forwardedHeader != "" {
		t.Fatalf("forwarded %s = %q, want empty", handlers.ForceModelPrefixHeader, exec.forwardedHeader)
	}
}

func TestChatCompletionsForceModelPrefixHeaderRejectsNestedPrefix(t *testing.T) {
	exec := &forceModelPrefixCaptureExecutor{}
	handler := newForceModelPrefixTestHandler(t, exec)

	resp := performForceModelPrefixChatRequest(handler, "productivity/team")

	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d: %s", resp.Code, http.StatusBadRequest, resp.Body.String())
	}
	if exec.calls != 0 {
		t.Fatalf("executor calls = %d, want 0", exec.calls)
	}
	if message := gjson.GetBytes(resp.Body.Bytes(), "error.message").String(); !strings.Contains(message, "X-Force-Model-Prefix") {
		t.Fatalf("error message = %q, want header name", message)
	}
}
