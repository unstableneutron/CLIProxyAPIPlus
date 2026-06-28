package openai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type blockingBootstrapExecutor struct {
	started chan struct{}
	once    sync.Once
}

func (e *blockingBootstrapExecutor) Identifier() string { return "codex" }

func (e *blockingBootstrapExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *blockingBootstrapExecutor) ExecuteStream(ctx context.Context, _ *coreauth.Auth, _ coreexecutor.Request, _ coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.once.Do(func() { close(e.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (e *blockingBootstrapExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *blockingBootstrapExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *blockingBootstrapExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

type blockingFirstChunkExecutor struct {
	started chan struct{}
	once    sync.Once
	chunks  chan coreexecutor.StreamChunk
}

func (e *blockingFirstChunkExecutor) Identifier() string { return "codex" }

func (e *blockingFirstChunkExecutor) Execute(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *blockingFirstChunkExecutor) ExecuteStream(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (*coreexecutor.StreamResult, error) {
	e.once.Do(func() { close(e.started) })
	return &coreexecutor.StreamResult{Chunks: e.chunks}, nil
}

func (e *blockingFirstChunkExecutor) Refresh(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	return auth, nil
}

func (e *blockingFirstChunkExecutor) CountTokens(context.Context, *coreauth.Auth, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error) {
	return coreexecutor.Response{}, errors.New("not implemented")
}

func (e *blockingFirstChunkExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

func newBlockingBootstrapResponsesHandler(t *testing.T, cfg *sdkconfig.SDKConfig) (*OpenAIResponsesAPIHandler, *blockingBootstrapExecutor) {
	t.Helper()

	executor := &blockingBootstrapExecutor{started: make(chan struct{})}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerTestStreamAuth(t, manager)

	return NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(cfg, manager)), executor
}

func newBlockingFirstChunkResponsesHandler(t *testing.T, cfg *sdkconfig.SDKConfig) (*OpenAIResponsesAPIHandler, *blockingFirstChunkExecutor) {
	t.Helper()

	executor := &blockingFirstChunkExecutor{
		started: make(chan struct{}),
		chunks:  make(chan coreexecutor.StreamChunk),
	}
	manager := coreauth.NewManager(nil, nil, nil)
	manager.RegisterExecutor(executor)
	registerTestStreamAuth(t, manager)

	return NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(cfg, manager)), executor
}

func registerTestStreamAuth(t *testing.T, manager *coreauth.Manager) {
	t.Helper()

	auth := &coreauth.Auth{
		ID:       "auth-blocking-bootstrap",
		Provider: "codex",
		Status:   coreauth.StatusActive,
		Metadata: map[string]any{"email": "test@example.com"},
	}
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatalf("manager.Register: %v", err)
	}

	registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: "test-model"}})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(auth.ID)
	})
}

func TestForwardResponsesStreamTerminalErrorUsesResponsesErrorChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	base := handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil)
	h := NewOpenAIResponsesAPIHandler(base)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatalf("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage, 1)
	errs <- &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errors.New("unexpected EOF")}
	close(errs)

	h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, nil)
	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("expected responses error chunk, got: %q", body)
	}
	if strings.Contains(body, `"error":{`) {
		t.Fatalf("expected streaming error chunk (top-level type), got HTTP error body: %q", body)
	}
}

func TestHandleStreamingResponseCommitsSSEWhileBootstrapWaits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, executor := newBlockingBootstrapResponsesHandler(t, &sdkconfig.SDKConfig{})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	reqCtx, cancelReq := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(reqCtx)

	done := make(chan struct{})
	go func() {
		h.handleStreamingResponse(c, []byte(`{"model":"test-model","stream":true}`))
		close(done)
	}()

	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("upstream bootstrap did not start")
	}

	time.Sleep(500 * time.Millisecond)
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("expected SSE content type while bootstrap waits, got %q", got)
	}
	if body := recorder.Body.String(); !strings.Contains(body, ": stream-start") {
		t.Fatalf("expected bootstrap heartbeat before upstream first byte, got body %q", body)
	}

	select {
	case <-done:
		t.Fatal("handler returned before request cancellation")
	default:
	}

	cancelReq()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not exit after request cancellation")
	}
}

func TestHandleStreamingResponseCommitsSSEWhileFirstChunkWaits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, executor := newBlockingFirstChunkResponsesHandler(t, &sdkconfig.SDKConfig{})
	defer close(executor.chunks)

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	reqCtx, cancelReq := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(reqCtx)

	done := make(chan struct{})
	go func() {
		h.handleStreamingResponse(c, []byte(`{"model":"test-model","stream":true}`))
		close(done)
	}()

	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("upstream stream did not start")
	}

	time.Sleep(500 * time.Millisecond)
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("expected SSE content type while first chunk waits, got %q", got)
	}
	if body := recorder.Body.String(); !strings.Contains(body, ": stream-start") {
		t.Fatalf("expected bootstrap heartbeat before upstream first chunk, got body %q", body)
	}

	select {
	case <-done:
		t.Fatal("handler returned before request cancellation")
	default:
	}

	cancelReq()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not exit after request cancellation")
	}
}

func TestResponsesStreamBootstrapCommitsWhenReturnedStreamWaitsForFirstChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	reqCtx, cancelReq := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(reqCtx)
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Fatalf("expected gin writer to implement http.Flusher")
	}

	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage)
	done := make(chan struct{})
	go func() {
		h.handleResponsesStreamBootstrap(
			c,
			flusher,
			context.Background(),
			func(...interface{}) {},
			func(context.Context) responsesStreamBootstrapResult {
				return responsesStreamBootstrapResult{data: data, errs: errs}
			},
			func() {
				c.Header("Content-Type", "text/event-stream")
			},
			func(data <-chan []byte, headers http.Header, errs <-chan *interfaces.ErrorMessage, committed bool, deadline time.Time) {
				framer := &responsesSSEFramer{}
				h.forwardStreamAfterBootstrap(
					c,
					flusher,
					func(error) {},
					data,
					headers,
					errs,
					func() { c.Header("Content-Type", "text/event-stream") },
					committed,
					deadline,
					nil,
					func(chunk []byte) { framer.WriteChunk(c.Writer, chunk) },
					func(data <-chan []byte, errs <-chan *interfaces.ErrorMessage) {
						h.forwardResponsesStream(c, flusher, func(error) {}, data, errs, framer)
					},
				)
			},
		)
		close(done)
	}()

	time.Sleep(500 * time.Millisecond)
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("expected SSE content type while returned stream waits, got %q", got)
	}
	if body := recorder.Body.String(); !strings.Contains(body, ": stream-start") {
		t.Fatalf("expected bootstrap heartbeat before returned stream first chunk, got body %q", body)
	}

	cancelReq()
	close(data)
	close(errs)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not exit after request cancellation")
	}
}

func TestHandleStreamingResponseViaChatCommitsSSEWhileBootstrapWaits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, executor := newBlockingBootstrapResponsesHandler(t, &sdkconfig.SDKConfig{})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	reqCtx, cancelReq := context.WithCancel(context.Background())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(reqCtx)

	done := make(chan struct{})
	go func() {
		h.handleStreamingResponseViaChat(
			c,
			[]byte(`{"model":"test-model","stream":true}`),
			[]byte(`{"model":"test-model","stream":true}`),
		)
		close(done)
	}()

	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("upstream bootstrap did not start")
	}

	time.Sleep(500 * time.Millisecond)
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("expected SSE content type while bootstrap waits, got %q", got)
	}
	if body := recorder.Body.String(); !strings.Contains(body, ": stream-start") {
		t.Fatalf("expected bootstrap heartbeat before upstream first byte, got body %q", body)
	}

	cancelReq()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not exit after request cancellation")
	}
}

func TestHandleStreamingResponseBootstrapTimeoutWritesResponsesErrorChunk(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, executor := newBlockingBootstrapResponsesHandler(t, &sdkconfig.SDKConfig{
		Streaming: sdkconfig.StreamingConfig{BootstrapTimeoutSeconds: 1},
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	done := make(chan struct{})
	go func() {
		h.handleStreamingResponse(c, []byte(`{"model":"test-model","stream":true}`))
		close(done)
	}()

	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("upstream bootstrap did not start")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after bootstrap timeout")
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `"type":"error"`) {
		t.Fatalf("expected Responses stream error chunk, got body %q", body)
	}
	if !strings.Contains(body, "upstream stream bootstrap timed out") {
		t.Fatalf("expected bootstrap timeout message, got body %q", body)
	}
}
