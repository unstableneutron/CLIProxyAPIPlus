package openai

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/tidwall/gjson"
)

type forwardResponsesWebsocketResult struct {
	errMsg *interfaces.ErrorMessage
	err    error
}

func runForwardResponsesWebsocketErrorTest(
	t *testing.T,
	data <-chan []byte,
	errs <-chan *interfaces.ErrorMessage,
	onForwardStart func(),
) ([]byte, forwardResponsesWebsocketResult) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	resultCh := make(chan forwardResponsesWebsocketResult, 1)
	handler := NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&sdkconfig.SDKConfig{}, nil))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			resultCh <- forwardResponsesWebsocketResult{err: errUpgrade}
			return
		}
		defer func() { _ = conn.Close() }()
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = r
		if onForwardStart != nil {
			onForwardStart()
		}
		_, _, _, errMsg, errForward := handler.forwardResponsesWebsocket(
			ctx,
			newResponsesWebsocketWriter(conn),
			func(...interface{}) {},
			data,
			errs,
			newInMemoryWebsocketTimelineLog(),
			"session-error-order",
		)
		resultCh <- forwardResponsesWebsocketResult{errMsg: errMsg, err: errForward}
	}))
	defer server.Close()

	conn, _, errDial := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if errDial != nil {
		t.Fatalf("dial websocket: %v", errDial)
	}
	defer func() { _ = conn.Close() }()
	_, payload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read websocket: %v", errRead)
	}
	result := <-resultCh
	return payload, result
}

func TestForwardResponsesWebsocketWaitsForErrorAfterDataCloses(t *testing.T) {
	data := make(chan []byte)
	close(data)
	errs := make(chan *interfaces.ErrorMessage)
	const exactError = `{"type":"error","status":500,"error":{"message":"gRPC error: Response with id=resp-stale not found","type":"api_error"}}`
	upstreamErr := &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: errors.New(exactError)}

	payload, result := runForwardResponsesWebsocketErrorTest(t, data, errs, func() {
		go func() {
			time.Sleep(20 * time.Millisecond)
			errs <- upstreamErr
			close(errs)
		}()
	})

	if result.err != nil {
		t.Fatalf("forward error: %v", result.err)
	}
	if result.errMsg != upstreamErr {
		t.Fatalf("forwarded error pointer = %p, want unchanged upstream error %p", result.errMsg, upstreamErr)
	}
	if got := int(gjson.GetBytes(payload, "status").Int()); got != http.StatusInternalServerError {
		t.Fatalf("downstream status = %d, want 500: %s", got, payload)
	}
	if got := gjson.GetBytes(payload, "error.message").String(); got != "gRPC error: Response with id=resp-stale not found" {
		t.Fatalf("downstream error message = %q, want exact upstream message", got)
	}
	if got := gjson.GetBytes(payload, "error.type").String(); got != "api_error" {
		t.Fatalf("downstream error type = %q, want api_error: %s", got, payload)
	}
}

func TestForwardResponsesWebsocketSynthesizes408AfterBothChannelsClose(t *testing.T) {
	data := make(chan []byte)
	errs := make(chan *interfaces.ErrorMessage)
	close(data)
	close(errs)

	payload, result := runForwardResponsesWebsocketErrorTest(t, data, errs, nil)
	if result.err != nil {
		t.Fatalf("forward error: %v", result.err)
	}
	if result.errMsg == nil || result.errMsg.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("forwarded error = %+v, want 408", result.errMsg)
	}
	if got := int(gjson.GetBytes(payload, "status").Int()); got != http.StatusRequestTimeout {
		t.Fatalf("downstream status = %d, want 408: %s", got, payload)
	}
	if got := gjson.GetBytes(payload, "error.message").String(); got != "stream closed before response.completed" {
		t.Fatalf("downstream error message = %q", got)
	}
}
