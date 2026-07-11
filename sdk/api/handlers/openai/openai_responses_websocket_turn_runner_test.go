package openai

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
)

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
