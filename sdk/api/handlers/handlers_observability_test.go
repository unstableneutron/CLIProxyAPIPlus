package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestHeadersFromContextDropsTraceContextHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Request.Header.Set("Traceparent", validHandlerTraceparentForTest)
	c.Request.Header.Set("Tracestate", "vendor=value")
	c.Request.Header.Set("X-Amp-Thread-Id", "thread-1")

	ctx := context.WithValue(context.Background(), "gin", c)
	headers := headersFromContext(ctx)

	if got := headers.Get("Traceparent"); got != "" {
		t.Fatalf("Traceparent should not be copied into executor headers: %q", got)
	}
	if got := headers.Get("Tracestate"); got != "" {
		t.Fatalf("Tracestate should not be copied into executor headers: %q", got)
	}
	if got := headers.Get("X-Amp-Thread-Id"); got != "thread-1" {
		t.Fatalf("session affinity header = %q", got)
	}
}

func TestGetContextWithCancelSelectedAuthCallbackSetsUpstreamPrefix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{ID: "auth-one", Provider: "codex", FileName: "auth-one.json"}
	registered, err := manager.Register(context.Background(), auth)
	if err != nil {
		t.Fatalf("register auth: %v", err)
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	reqCtx := logging.WithProxyStatusHolder(context.Background())
	req = req.WithContext(reqCtx)
	c.Request = req

	handler := NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, manager)
	cliCtx, cancel := handler.GetContextWithCancel(nil, c, context.Background())
	defer cancel()

	metadata := requestExecutionMetadata(cliCtx)
	callback, ok := metadata[coreexecutor.SelectedAuthCallbackMetadataKey].(func(string))
	if !ok || callback == nil {
		t.Fatal("selected auth callback missing")
	}
	callback(registered.ID)

	status := logging.GetProxyStatus(cliCtx)
	want := logging.ShortSlotIdentifier(registered.Index)
	if status.Slot != want {
		t.Fatalf("slot = %q, want %q", status.Slot, want)
	}
}

const validHandlerTraceparentForTest = "00-1234567890abcdef1234567890abcdef-abcdef1234567890-01"
