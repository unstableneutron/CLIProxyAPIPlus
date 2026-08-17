package openai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/tidwall/gjson"
)

func TestResponsesWebsocketTurnRunnerTreatsResponseIncompleteAsTerminal(t *testing.T) {
	runner := responsesWebsocketTurnRunner{
		execute: func(_ context.Context, _ []byte, _ string, _ []string, selected func(string)) (<-chan []byte, <-chan *interfaces.ErrorMessage) {
			selected("auth-a")
			data := make(chan []byte, 1)
			errs := make(chan *interfaces.ErrorMessage)
			data <- []byte(`{"type":"response.incomplete","response":{"id":"resp-1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}}`)
			close(data)
			close(errs)
			return data, errs
		},
	}
	stream := runner.Start(context.Background(), responsesWebsocketTurnInput{NativePayload: []byte(`{"input":[]}`)})

	var events [][]byte
	for payload := range stream.Data {
		events = append(events, payload)
	}
	for errMsg := range stream.Errors {
		t.Fatalf("unexpected turn-runner error: %+v", errMsg)
	}
	outcome := <-stream.outcome
	if len(events) != 1 || gjson.GetBytes(events[0], "type").String() != "response.incomplete" {
		t.Fatalf("downstream events = %q, want one response.incomplete", events)
	}
	if !outcome.Completed || outcome.Attempts != 1 || outcome.SelectedAuthID != "auth-a" {
		t.Fatalf("outcome = %+v, want completed terminal attempt", outcome)
	}
}

func TestForwardResponsesWebsocketTreatsResponseIncompleteAsTerminal(t *testing.T) {
	gin.SetMode(gin.TestMode)

	serverErrCh := make(chan error, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, errUpgrade := responsesWebsocketUpgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			serverErrCh <- errUpgrade
			return
		}
		defer func() { _ = conn.Close() }()

		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = r
		data := make(chan []byte, 1)
		errs := make(chan *interfaces.ErrorMessage)
		data <- []byte(`{"type":"response.incomplete","response":{"id":"resp-1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}}`)
		close(data)
		close(errs)

		_, responseID, _, errMsg, errForward := (*OpenAIResponsesAPIHandler)(nil).forwardResponsesWebsocket(
			ctx,
			newResponsesWebsocketWriter(conn),
			func(...interface{}) {},
			data,
			errs,
			newInMemoryWebsocketTimelineLog(),
			"session-1",
		)
		if errForward != nil {
			serverErrCh <- errForward
			return
		}
		if errMsg != nil {
			serverErrCh <- fmt.Errorf("unexpected websocket error: %v", errMsg.Error)
			return
		}
		if responseID != "resp-1" {
			serverErrCh <- fmt.Errorf("response ID = %q, want resp-1", responseID)
			return
		}
		serverErrCh <- nil
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, _, errDial := websocket.DefaultDialer.Dial(wsURL, nil)
	if errDial != nil {
		t.Fatalf("dial websocket: %v", errDial)
	}
	defer func() { _ = conn.Close() }()

	_, payload, errRead := conn.ReadMessage()
	if errRead != nil {
		t.Fatalf("read websocket response: %v", errRead)
	}
	if got := gjson.GetBytes(payload, "type").String(); got != "response.incomplete" {
		t.Fatalf("response type = %q, want response.incomplete", got)
	}
	if got := gjson.GetBytes(payload, "response.incomplete_details.reason").String(); got != "max_output_tokens" {
		t.Fatalf("incomplete reason = %q, want max_output_tokens", got)
	}
	if errServer := <-serverErrCh; errServer != nil && !errors.Is(errServer, websocket.ErrCloseSent) {
		t.Fatalf("server error: %v", errServer)
	}
}
