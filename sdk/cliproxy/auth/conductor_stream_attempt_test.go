package auth

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type streamAttemptCancellationExecutor struct {
	mu                        sync.Mutex
	calls                     int
	firstCtx                  context.Context
	secondStartedBeforeCancel bool
}

func (*streamAttemptCancellationExecutor) Identifier() string { return "codex" }

func (*streamAttemptCancellationExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (e *streamAttemptCancellationExecutor) ExecuteStream(ctx context.Context, _ *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.mu.Lock()
	e.calls++
	call := e.calls
	if call == 1 {
		e.firstCtx = ctx
	}
	firstCtx := e.firstCtx
	e.mu.Unlock()

	chunks := make(chan cliproxyexecutor.StreamChunk, 1)
	if call == 1 {
		chunks <- cliproxyexecutor.StreamChunk{Err: &streamAttemptStatusError{status: http.StatusServiceUnavailable}}
		go func() {
			<-ctx.Done()
			close(chunks)
		}()
		return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
	}

	if firstCtx == nil || firstCtx.Err() == nil {
		e.mu.Lock()
		e.secondStartedBeforeCancel = true
		e.mu.Unlock()
	}
	chunks <- cliproxyexecutor.StreamChunk{Payload: []byte(`data: {"type":"response.output_text.delta","delta":"ok"}`)}
	close(chunks)
	return &cliproxyexecutor.StreamResult{Chunks: chunks}, nil
}

func (*streamAttemptCancellationExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (*streamAttemptCancellationExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, errors.New("not implemented")
}

func (*streamAttemptCancellationExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, errors.New("not implemented")
}

type streamAttemptStatusError struct {
	status int
}

func (e *streamAttemptStatusError) Error() string   { return "stream attempt failed" }
func (e *streamAttemptStatusError) StatusCode() int { return e.status }

func TestExecuteStreamCancelsRejectedAttemptBeforeAuthFailover(t *testing.T) {
	executor := &streamAttemptCancellationExecutor{}
	manager := NewManager(nil, &RoundRobinSelector{}, nil)
	manager.SetRetryConfig(0, 0, 0)
	manager.RegisterExecutor(executor)
	model := "stream-attempt-cancel-" + uuid.NewString()
	for index := 0; index < 2; index++ {
		auth := &Auth{ID: "stream-attempt-auth-" + uuid.NewString(), Provider: executor.Identifier(), Status: StatusActive}
		if _, errRegister := manager.Register(context.Background(), auth); errRegister != nil {
			t.Fatalf("register auth %d: %v", index, errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, auth.Provider, []*registry.ModelInfo{{ID: model}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	}

	stream, errStream := manager.ExecuteStream(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{Model: model}, cliproxyexecutor.Options{Stream: true})
	if errStream != nil {
		t.Fatalf("ExecuteStream() error = %v", errStream)
	}
	for range stream.Chunks {
	}

	executor.mu.Lock()
	defer executor.mu.Unlock()
	if executor.calls != 2 {
		t.Fatalf("provider attempts = %d, want 2", executor.calls)
	}
	if executor.secondStartedBeforeCancel {
		t.Fatal("second auth started before rejected provider attempt was canceled")
	}
}
