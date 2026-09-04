package auth

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type retryableRateLimitError struct {
	status     int
	retryAfter time.Duration
}

func (e *retryableRateLimitError) Error() string { return "rate limited" }

func (e *retryableRateLimitError) StatusCode() int { return e.status }

func (e *retryableRateLimitError) RetryAfter() *time.Duration { return &e.retryAfter }

type rateLimitedExecutor struct {
	calls atomic.Int32
	err   error
}

func (e *rateLimitedExecutor) Identifier() string { return "gemini" }

func (e *rateLimitedExecutor) Execute(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.calls.Add(1)
	return cliproxyexecutor.Response{}, e.err
}

func (e *rateLimitedExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.calls.Add(1)
	return nil, e.err
}

func (e *rateLimitedExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.calls.Add(1)
	return cliproxyexecutor.Response{}, e.err
}

func (e *rateLimitedExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) { return a, nil }

func (e *rateLimitedExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

type idRecordingRateLimitedExecutor struct {
	mu         sync.Mutex
	identifier string
	calls      map[string]int
	err        error
}

func (e *idRecordingRateLimitedExecutor) Identifier() string {
	if e.identifier != "" {
		return e.identifier
	}
	return "gemini"
}

func (e *idRecordingRateLimitedExecutor) record(id string) {
	e.mu.Lock()
	e.calls[id]++
	e.mu.Unlock()
}

func (e *idRecordingRateLimitedExecutor) Execute(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.record(a.ID)
	return cliproxyexecutor.Response{}, e.err
}

func (e *idRecordingRateLimitedExecutor) ExecuteStream(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	e.record(a.ID)
	return nil, e.err
}

func (e *idRecordingRateLimitedExecutor) CountTokens(_ context.Context, a *Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.record(a.ID)
	return cliproxyexecutor.Response{}, e.err
}

func (e *idRecordingRateLimitedExecutor) Refresh(_ context.Context, a *Auth) (*Auth, error) {
	return a, nil
}

func (e *idRecordingRateLimitedExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *idRecordingRateLimitedExecutor) count(id string) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls[id]
}

// The end-to-end cooldown retry path now enforces minQuotaCooldownFloor before
// resetting exclusions. Test the reset operation directly so this regression
// stays fast while the floor is covered by conductor_subsecond_cooldown_test.go.
func TestCooldownRetryResetsExclusions(t *testing.T) {
	withQuotaCooldownEnabled(t)

	manager := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-rotation", Provider: "gemini", Status: StatusActive}
	if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
		t.Fatalf("register auth: %v", errRegister)
	}

	got := manager.resetRecoveredExclusions(map[string]struct{}{auth.ID: {}}, nil)

	if _, ok := got[auth.ID]; ok {
		t.Fatalf("rotation-added exclusion %q was not reset", auth.ID)
	}
}

func TestCooldownRetryPreservesCallerExclusions(t *testing.T) {
	withQuotaCooldownEnabled(t)

	manager := NewManager(nil, nil, nil)
	got := manager.resetRecoveredExclusions(
		map[string]struct{}{"auth-caller": {}},
		map[string]struct{}{"auth-caller": {}},
	)
	if _, ok := got["auth-caller"]; !ok {
		t.Fatal("caller-provided exclusion was not preserved")
	}
}

func TestCooldownRetryPreservesConfigDisabledCoolingExclusions(t *testing.T) {
	withQuotaCooldownEnabled(t)

	t.Run("global config", func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		manager.SetConfigSnapshot(&internalconfig.Config{DisableCooling: true})
		auth := &Auth{ID: "auth-global-disabled", Provider: "gemini", Status: StatusActive}
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
			t.Fatalf("register auth: %v", errRegister)
		}

		got := manager.resetRecoveredExclusions(map[string]struct{}{auth.ID: {}}, nil)
		if _, ok := got[auth.ID]; !ok {
			t.Fatal("global config-disabled cooling exclusion was reset")
		}
	})

	t.Run("provider compatibility config", func(t *testing.T) {
		disableCooling := true
		manager := NewManager(nil, nil, nil)
		manager.SetConfigSnapshot(&internalconfig.Config{
			OpenAICompatibility: []internalconfig.OpenAICompatibility{{
				Name:           "custom-openai",
				DisableCooling: &disableCooling,
			}},
		})
		auth := &Auth{
			ID:       "auth-compat-disabled",
			Provider: "openai-compatibility",
			Status:   StatusActive,
			Attributes: map[string]string{
				"provider_key": "custom-openai",
			},
		}
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
			t.Fatalf("register auth: %v", errRegister)
		}

		got := manager.resetRecoveredExclusions(map[string]struct{}{auth.ID: {}}, nil)
		if _, ok := got[auth.ID]; !ok {
			t.Fatal("provider config-disabled cooling exclusion was reset")
		}
	})

	t.Run("cooling enabled control", func(t *testing.T) {
		manager := NewManager(nil, nil, nil)
		manager.SetConfigSnapshot(&internalconfig.Config{DisableCooling: false})
		auth := &Auth{ID: "auth-cooling-enabled", Provider: "gemini", Status: StatusActive}
		if _, errRegister := manager.Register(WithSkipPersist(context.Background()), auth); errRegister != nil {
			t.Fatalf("register auth: %v", errRegister)
		}

		got := manager.resetRecoveredExclusions(map[string]struct{}{auth.ID: {}}, nil)
		if _, ok := got[auth.ID]; ok {
			t.Fatal("cooling-enabled rotation exclusion was not reset")
		}
	})
}
