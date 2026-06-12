package cliproxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestRegisterModelsForAuth_CommandCodeUsesLiveProviderCatalog(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/provider/v1/models" {
			t.Fatalf("path = %s, want /provider/v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"MiniMaxAI/MiniMax-M3","object":"model","created":1780357901,"owned_by":"command-code","name":"MiniMax M3","context_length":1000000}]}`))
	}))
	defer server.Close()

	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "commandcode-live-models-auth",
		Provider: "commandcode",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "user_test",
			"base_url": server.URL,
		},
	}
	defer GlobalModelRegistry().UnregisterClient(auth.ID)

	service.registerModelsForAuth(context.Background(), auth)
	models := GlobalModelRegistry().GetAvailableModelsByProvider("commandcode")
	var found *ModelInfo
	for _, model := range models {
		if model != nil && model.ID == "MiniMaxAI/MiniMax-M3" {
			found = model
			break
		}
	}
	if found == nil {
		t.Fatalf("expected live MiniMaxAI/MiniMax-M3 model, got %+v", models)
	}
	if found.DisplayName != "MiniMax M3 (CC)" {
		t.Fatalf("display name = %q", found.DisplayName)
	}
	if found.ContextLength != 1000000 {
		t.Fatalf("context length = %d", found.ContextLength)
	}
}

func TestEnsureExecutorsForAuth_CommandCodeBindsIndependentExecutor(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "commandcode-auth-1",
		Provider: "commandcode",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key": "user_test",
		},
	}

	service.ensureExecutorsForAuth(auth)
	resolved, ok := service.coreManager.Executor("commandcode")
	if !ok || resolved == nil {
		t.Fatal("expected commandcode executor after bind")
	}
	if _, isCommandCode := resolved.(*executor.CommandCodeExecutor); !isCommandCode {
		t.Fatalf("executor type = %T, want *executor.CommandCodeExecutor", resolved)
	}
	if _, isOpenAICompat := resolved.(*executor.OpenAICompatExecutor); isOpenAICompat {
		t.Fatal("commandcode must not bind the generic OpenAI-compatible executor")
	}
}
