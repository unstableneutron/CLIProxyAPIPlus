package cliproxy

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestEnsureExecutorsForAuth_BedrockBindsNativeExecutor(t *testing.T) {
	// Given
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "bedrock-auth-1",
		Provider: "bedrock",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key":  "test",
			"base_url": "https://bedrock.example",
		},
	}

	// When
	service.ensureExecutorsForAuth(auth)

	// Then
	resolved, ok := service.coreManager.Executor("bedrock")
	if !ok || resolved == nil {
		t.Fatal("expected bedrock executor after bind")
	}
	if _, isBedrock := resolved.(*executor.BedrockExecutor); !isBedrock {
		t.Fatalf("executor type = %T, want *executor.BedrockExecutor", resolved)
	}
	if _, isOpenAICompat := resolved.(*executor.OpenAICompatExecutor); isOpenAICompat {
		t.Fatal("bedrock must not bind the generic OpenAI-compatible executor")
	}
}

func TestRegisterModelsForAuth_BedrockUsesConfiguredModels(t *testing.T) {
	// Given
	service := &Service{
		cfg: &config.Config{
			Bedrock: []config.BedrockProvider{
				{
					Name:    "llm-fusion",
					BaseURL: "https://bedrock.example",
					Models: []config.BedrockModel{
						{Name: "global.anthropic.claude-sonnet-5", Alias: "sonnet-5"},
					},
				},
			},
		},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "bedrock-model-auth",
		Provider: "bedrock",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"base_url":     "https://bedrock.example",
			"bedrock_name": "llm-fusion",
		},
	}
	defer GlobalModelRegistry().UnregisterClient(auth.ID)

	// When
	service.registerModelsForAuth(context.Background(), auth)

	// Then
	models := GlobalModelRegistry().GetAvailableModelsByProvider("bedrock")
	var found *ModelInfo
	for _, model := range models {
		if model != nil && model.ID == "sonnet-5" {
			found = model
			break
		}
	}
	if found == nil {
		t.Fatalf("expected configured sonnet-5 model, got %+v", models)
	}
	if found.OwnedBy != "aws-bedrock" {
		t.Fatalf("OwnedBy = %q, want aws-bedrock", found.OwnedBy)
	}
	if found.Type != "bedrock" {
		t.Fatalf("Type = %q, want bedrock", found.Type)
	}
}
