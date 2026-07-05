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
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"MiniMaxAI/MiniMax-M3","object":"model","created":1780357901,"owned_by":"command-code","name":"MiniMax M3","context_length":1000000},{"id":"stepfun/Step-3.5-Flash","object":"model","created":1780357901,"owned_by":"command-code","name":"Step 3.5 Flash","context_length":1000000},{"id":"xiaomi/mimo-v2.5-pro","object":"model","created":1780357901,"owned_by":"command-code","name":"MiMo V2.5 Pro","context_length":1000000},{"id":"deepseek/deepseek-v4-flash","object":"model","created":1780357901,"owned_by":"command-code","name":"DeepSeek V4 Flash","context_length":1000000}]}`))
	}))
	defer server.Close()

	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "commandcode-live-models-auth",
		Provider: "commandcode",
		Prefix:   "cc",
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

	ids := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != nil {
			ids[model.ID] = struct{}{}
		}
	}
	for _, id := range []string{
		"MiniMaxAI/MiniMax-M3",
		"cc/MiniMaxAI/MiniMax-M3",
		"stepfun/Step-3.5-Flash",
		"cc/stepfun/Step-3.5-Flash",
		"xiaomi/mimo-v2.5-pro",
		"cc/xiaomi/mimo-v2.5-pro",
		"deepseek/deepseek-v4-flash",
		"cc/deepseek/deepseek-v4-flash",
	} {
		if _, ok := ids[id]; !ok {
			t.Fatalf("expected CommandCode live catalog model %q, got %+v", id, models)
		}
	}
	for _, id := range []string{
		"minimax-m3",
		"cc/minimax-m3",
		"step-3.5-flash",
		"cc/step-3.5-flash",
		"mimo-v2.5-pro",
		"cc/mimo-v2.5-pro",
		"deepseek-v4-flash",
		"cc/deepseek-v4-flash",
		"ds4-flash",
		"cc/ds4-flash",
	} {
		if _, ok := ids[id]; ok {
			t.Fatalf("live CommandCode catalog should not auto-generate short alias %q in %+v", id, models)
		}
	}
}

func TestRegisterModelsForAuth_CommandCodeUsesExplicitConfigAliases(t *testing.T) {
	service := &Service{
		cfg: &config.Config{
			CommandCodeKey: []config.CommandCodeKey{{
				APIKey: "user_test",
				Models: []config.CommandCodeModel{
					{Name: "deepseek/deepseek-v4-pro"},
					{Name: "deepseek/deepseek-v4-pro", Alias: "ds4-pro"},
					{Name: "stepfun/Step-3.5-Flash", Alias: "step-3.5-flash"},
				},
			}},
		},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "commandcode-config-aliases-auth",
		Provider: "commandcode",
		Prefix:   "cc",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key": "user_test",
		},
	}
	defer GlobalModelRegistry().UnregisterClient(auth.ID)

	service.registerModelsForAuth(context.Background(), auth)
	models := GlobalModelRegistry().GetAvailableModelsByProvider("commandcode")
	ids := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != nil {
			ids[model.ID] = struct{}{}
		}
	}
	for _, id := range []string{
		"deepseek/deepseek-v4-pro",
		"cc/deepseek/deepseek-v4-pro",
		"ds4-pro",
		"cc/ds4-pro",
		"step-3.5-flash",
		"cc/step-3.5-flash",
	} {
		if _, ok := ids[id]; !ok {
			t.Fatalf("expected explicit CommandCode config model %q, got %+v", id, models)
		}
	}
}

func TestRegisterModelsForAuth_CommandCodeExcludesCanonicalCatalogModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/provider/v1/models" {
			t.Fatalf("path = %s, want /provider/v1/models", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"deepseek/deepseek-v4-flash","object":"model","created":1780357901,"owned_by":"command-code","name":"DeepSeek V4 Flash","context_length":1000000},{"id":"stepfun/Step-3.5-Flash","object":"model","created":1780357901,"owned_by":"command-code","name":"Step 3.5 Flash","context_length":1000000}]}`))
	}))
	defer server.Close()

	service := &Service{
		cfg: &config.Config{
			CommandCodeKey: []config.CommandCodeKey{{
				APIKey:         "user_test",
				BaseURL:        server.URL,
				ExcludedModels: []string{"deepseek/deepseek-v4-flash"},
			}},
		},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "commandcode-excluded-aliases-auth",
		Provider: "commandcode",
		Prefix:   "cc",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "user_test",
			"base_url": server.URL,
		},
	}
	defer GlobalModelRegistry().UnregisterClient(auth.ID)

	service.registerModelsForAuth(context.Background(), auth)
	models := GlobalModelRegistry().GetAvailableModelsByProvider("commandcode")
	ids := make(map[string]struct{}, len(models))
	for _, model := range models {
		if model != nil {
			ids[model.ID] = struct{}{}
		}
	}
	for _, id := range []string{
		"deepseek/deepseek-v4-flash",
		"cc/deepseek/deepseek-v4-flash",
	} {
		if _, ok := ids[id]; ok {
			t.Fatalf("excluded CommandCode model surfaced as %q in %+v", id, models)
		}
	}
	for _, id := range []string{"stepfun/Step-3.5-Flash", "cc/stepfun/Step-3.5-Flash"} {
		if _, ok := ids[id]; !ok {
			t.Fatalf("expected non-excluded CommandCode catalog model %q, got %+v", id, models)
		}
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
