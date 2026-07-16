package openai

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai/responsesreplay"
	"github.com/tidwall/gjson"
)

func TestResponsesWebsocketTurnRunnerReplaysXAIMissingPreviousResponseOnSameAuth(t *testing.T) {
	const responseID = "25a6b917-9417-9fa4-a21a-1e097d64a96b-xai-13"
	var mu sync.Mutex
	var payloads [][]byte
	var preferredAuthIDs []string
	var excludedAuthIDs [][]string
	runner := responsesWebsocketTurnRunner{
		execute: func(_ context.Context, payload []byte, preferredAuthID string, excluded []string, selected func(string)) (<-chan []byte, <-chan *interfaces.ErrorMessage) {
			mu.Lock()
			payloads = append(payloads, append([]byte(nil), payload...))
			preferredAuthIDs = append(preferredAuthIDs, preferredAuthID)
			excludedAuthIDs = append(excludedAuthIDs, append([]string(nil), excluded...))
			attempt := len(payloads)
			mu.Unlock()

			selected("auth-a")
			data := make(chan []byte, 2)
			errs := make(chan *interfaces.ErrorMessage, 1)
			if attempt == 1 {
				data <- []byte(`{"type":"response.created","response":{"id":"must-not-leak"}}`)
				errs <- &interfaces.ErrorMessage{
					StatusCode: http.StatusInternalServerError,
					Error:      errors.New(`{"type":"error","status":500,"error":{"message":"gRPC error: Response with id=` + responseID + ` not found","type":"api_error"}}`),
				}
			} else {
				data <- []byte(`{"type":"response.created","response":{"id":"accepted"}}`)
				data <- []byte(`{"type":"response.completed","response":{"id":"accepted","output":[]}}`)
			}
			close(data)
			close(errs)
			return data, errs
		},
	}
	stream := runner.Start(context.Background(), responsesWebsocketTurnInput{
		NativePayload: []byte(`{"model":"grok-4.3","previous_response_id":"` + responseID + `","input":[{"type":"message","id":"msg-new"}]}`),
		ReplayPayload: []byte(`{"model":"grok-4.3","input":[{"type":"message","id":"msg-history"},{"type":"message","id":"msg-new"}]}`),
	})

	var downstream [][]byte
	for payload := range stream.Data {
		downstream = append(downstream, payload)
	}
	for errMsg := range stream.Errors {
		t.Fatalf("unexpected downstream error: %+v", errMsg)
	}
	outcome := <-stream.outcome

	if len(payloads) != 2 {
		t.Fatalf("attempts = %d, want 2", len(payloads))
	}
	if preferredAuthIDs[1] != "auth-a" {
		t.Fatalf("second preferred auth = %q, want auth-a", preferredAuthIDs[1])
	}
	if len(excludedAuthIDs[1]) != 0 {
		t.Fatalf("second excluded auths = %v, want none", excludedAuthIDs[1])
	}
	if gjson.GetBytes(payloads[1], "previous_response_id").Exists() {
		t.Fatalf("second attempt kept previous_response_id: %s", payloads[1])
	}
	if got := gjson.GetBytes(payloads[1], "input.#").Int(); got != 2 {
		t.Fatalf("second attempt input count = %d, want full replay with 2 items: %s", got, payloads[1])
	}
	if len(downstream) != 2 || gjson.GetBytes(downstream[0], "response.id").String() != "accepted" {
		t.Fatalf("downstream events = %q, want only accepted attempt", downstream)
	}
	if !outcome.Completed || outcome.Attempts != 2 || outcome.SelectedAuthID != "auth-a" {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestResponsesWebsocketTurnRunnerStagesProviderIDCleanupAfterXAIMissingPreviousResponse(t *testing.T) {
	const responseID = "resp-stale"
	var payloads [][]byte
	runner := responsesWebsocketTurnRunner{
		execute: func(_ context.Context, payload []byte, preferredAuthID string, excluded []string, selected func(string)) (<-chan []byte, <-chan *interfaces.ErrorMessage) {
			payloads = append(payloads, append([]byte(nil), payload...))
			attempt := len(payloads)
			if attempt > 1 && preferredAuthID != "auth-a" {
				t.Fatalf("attempt %d preferred auth = %q, want auth-a", attempt, preferredAuthID)
			}
			if len(excluded) != 0 {
				t.Fatalf("attempt %d excluded auths = %v, want none", attempt, excluded)
			}
			selected("auth-a")
			data := make(chan []byte, 2)
			errs := make(chan *interfaces.ErrorMessage, 1)
			switch attempt {
			case 1:
				data <- []byte(`{"type":"response.created","response":{"id":"first-leak"}}`)
				errs <- &interfaces.ErrorMessage{StatusCode: 500, Error: errors.New(`{"type":"error","status":500,"error":{"message":"gRPC error: Response with id=resp-stale not found","type":"api_error"}}`)}
			case 2:
				data <- []byte(`{"type":"response.created","response":{"id":"second-leak"}}`)
				errs <- &interfaces.ErrorMessage{StatusCode: 400, Error: errors.New(`{"type":"error","status":400,"error":{"message":"input item not found","type":"invalid_request_error","code":"item_not_found","param":"input.1.id"}}`)}
			default:
				data <- []byte(`{"type":"response.created","response":{"id":"accepted"}}`)
				data <- []byte(`{"type":"response.completed","response":{"id":"accepted","output":[]}}`)
			}
			close(data)
			close(errs)
			return data, errs
		},
	}
	replay := []byte(`{"model":"grok-4.3","input":[{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup","arguments":"{}"},{"type":"function_call_output","id":"fco-1","call_id":"call-1","output":"ok"}]}`)
	stream := runner.Start(context.Background(), responsesWebsocketTurnInput{
		NativePayload: []byte(`{"model":"grok-4.3","previous_response_id":"resp-stale","input":[{"type":"function_call_output","id":"fco-1","call_id":"call-1","output":"ok"}]}`),
		ReplayPayload: replay,
	})
	for range stream.Data {
	}
	for errMsg := range stream.Errors {
		t.Fatalf("unexpected downstream error: %+v", errMsg)
	}
	outcome := <-stream.outcome

	if len(payloads) != 3 {
		t.Fatalf("attempts = %d, want 3", len(payloads))
	}
	if !gjson.GetBytes(payloads[1], "input.0.id").Exists() || !gjson.GetBytes(payloads[1], "input.1.id").Exists() {
		t.Fatalf("first replay stripped provider item IDs too early: %s", payloads[1])
	}
	if gjson.GetBytes(payloads[2], "input.0.id").Exists() || gjson.GetBytes(payloads[2], "input.1.id").Exists() {
		t.Fatalf("staged item repair kept provider item IDs: %s", payloads[2])
	}
	if got := gjson.GetBytes(payloads[2], "input.0.call_id").String(); got != "call-1" {
		t.Fatalf("function_call call_id = %q, want call-1: %s", got, payloads[2])
	}
	if got := gjson.GetBytes(payloads[2], "input.1.call_id").String(); got != "call-1" {
		t.Fatalf("function_call_output call_id = %q, want call-1: %s", got, payloads[2])
	}
	if !outcome.Completed || outcome.Attempts != 3 {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestResponsesWebsocketTurnRunnerPropagatesFailureAfterStateRepairUnchanged(t *testing.T) {
	const responseID = "resp-stale"
	finalErr := &interfaces.ErrorMessage{StatusCode: http.StatusBadGateway, Error: errors.New("xAI replay failed")}
	attempts := 0
	runner := responsesWebsocketTurnRunner{
		execute: func(_ context.Context, _ []byte, preferredAuthID string, excluded []string, selected func(string)) (<-chan []byte, <-chan *interfaces.ErrorMessage) {
			attempts++
			if attempts == 2 && preferredAuthID != "auth-a" {
				t.Fatalf("repair preferred auth = %q, want auth-a", preferredAuthID)
			}
			if len(excluded) != 0 {
				t.Fatalf("attempt %d excluded auths = %v, want none", attempts, excluded)
			}
			selected("auth-a")
			data := make(chan []byte)
			errs := make(chan *interfaces.ErrorMessage, 1)
			if attempts == 1 {
				errs <- &interfaces.ErrorMessage{StatusCode: 500, Error: errors.New(`{"type":"error","status":500,"error":{"message":"gRPC error: Response with id=resp-stale not found","type":"api_error"}}`)}
			} else {
				errs <- finalErr
			}
			close(data)
			close(errs)
			return data, errs
		},
	}
	stream := runner.Start(context.Background(), responsesWebsocketTurnInput{
		NativePayload: []byte(`{"previous_response_id":"` + responseID + `","input":[]}`),
		ReplayPayload: []byte(`{"input":[]}`),
	})
	for range stream.Data {
	}
	var gotErr *interfaces.ErrorMessage
	for errMsg := range stream.Errors {
		gotErr = errMsg
	}
	outcome := <-stream.outcome

	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if gotErr != finalErr {
		t.Fatalf("downstream error = %p, want unchanged replay error %p", gotErr, finalErr)
	}
	if outcome.Failure != responsesreplay.FailureAuthOrRoute || outcome.Completed {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestResponsesWebsocketTurnRunnerDiscardsStartupFramesBeforeFailover(t *testing.T) {
	var mu sync.Mutex
	var payloads [][]byte
	runner := responsesWebsocketTurnRunner{
		execute: func(_ context.Context, payload []byte, _ string, _ []string, selected func(string)) (<-chan []byte, <-chan *interfaces.ErrorMessage) {
			mu.Lock()
			payloads = append(payloads, append([]byte(nil), payload...))
			attempt := len(payloads)
			mu.Unlock()
			selected("auth-a")
			data := make(chan []byte, 2)
			errs := make(chan *interfaces.ErrorMessage, 1)
			if attempt == 1 {
				data <- []byte(`{"type":"response.created","response":{"id":"leaked"}}`)
				data <- []byte(`{"type":"error","error":{"code":"invalid_encrypted_content","message":"encrypted content could not be verified"}}`)
			} else {
				data <- []byte(`{"type":"response.created","response":{"id":"accepted"}}`)
				data <- []byte(`{"type":"response.completed","response":{"id":"accepted","output":[]}}`)
			}
			close(data)
			close(errs)
			return data, errs
		},
	}
	stream := runner.Start(context.Background(), responsesWebsocketTurnInput{
		NativePayload: []byte(`{"previous_response_id":"resp-a","input":[{"type":"reasoning","encrypted_content":"opaque"}]}`),
		ReplayPayload: []byte(`{"input":[{"type":"reasoning","encrypted_content":"opaque"}]}`),
	})
	var got [][]byte
	for payload := range stream.Data {
		got = append(got, payload)
	}
	if len(got) != 2 || string(got[0]) != `{"type":"response.created","response":{"id":"accepted"}}` {
		t.Fatalf("accepted stream = %q, want only second-attempt events", got)
	}
	if len(payloads) != 2 {
		t.Fatalf("attempts = %d, want 2", len(payloads))
	}
	if string(payloads[1]) == string(payloads[0]) {
		t.Fatal("state recovery retried identical payload")
	}
	outcome := <-stream.outcome
	if !outcome.Completed || outcome.Attempts != 2 {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestResponsesWebsocketTurnRunnerNeverRetriesAfterSemanticCommit(t *testing.T) {
	attempts := 0
	runner := responsesWebsocketTurnRunner{
		execute: func(_ context.Context, _ []byte, _ string, _ []string, selected func(string)) (<-chan []byte, <-chan *interfaces.ErrorMessage) {
			attempts++
			selected("auth-a")
			data := make(chan []byte, 2)
			errs := make(chan *interfaces.ErrorMessage, 1)
			data <- []byte(`{"type":"response.output_text.delta","delta":"hello"}`)
			errs <- &interfaces.ErrorMessage{StatusCode: 503, Error: errors.New("provider unavailable")}
			close(data)
			close(errs)
			return data, errs
		},
	}
	stream := runner.Start(context.Background(), responsesWebsocketTurnInput{
		NativePayload: []byte(`{"input":[]}`),
		ReplayPayload: []byte(`{"input":[]}`),
	})
	for range stream.Data {
	}
	for range stream.Errors {
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 after semantic commitment", attempts)
	}
	if outcome := <-stream.outcome; !outcome.Committed || outcome.Completed {
		t.Fatalf("outcome = %+v", outcome)
	}
}

func TestResponsesWebsocketStartupBufferHasStrictOneMiBBoundary(t *testing.T) {
	buffer := responsesWebsocketStartupBuffer{}
	if !buffer.append(make([]byte, responsesWebsocketStartupBufferLimit)) {
		t.Fatal("exactly one MiB should remain speculative")
	}
	if buffer.append([]byte{1}) {
		t.Fatal("payload exceeding one MiB should force commitment")
	}
	if buffer.bytes != responsesWebsocketStartupBufferLimit || len(buffer.payloads) != 1 {
		t.Fatalf("failed append mutated buffer: bytes=%d payloads=%d", buffer.bytes, len(buffer.payloads))
	}
}
