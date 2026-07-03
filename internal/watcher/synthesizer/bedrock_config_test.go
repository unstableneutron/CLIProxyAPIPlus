package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestConfigSynthesizer_BedrockKeys_useEnvKeyAndCustomHeaders_whenConfigured(t *testing.T) {
	// Given
	t.Setenv("BEDROCK_RUNTIME_TEST_KEY", "env-bedrock-token")
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			Bedrock: []config.BedrockProvider{
				{
					Name:    "llm-fusion",
					Prefix:  "bedrock",
					BaseURL: "https://llm-fusion-hub.example/api/v2/proxy/aws/bedrock",
					Auth: config.BedrockAuth{
						Type:      "raw",
						APIKeyEnv: "BEDROCK_RUNTIME_TEST_KEY",
						Headers: map[string]string{
							"X-Auth-Scope": "bedrock-smoke",
						},
					},
					Headers: map[string]string{
						"X-Team": "proxy",
					},
					QueryParams: map[string]string{
						"trace": "1",
					},
					Models: []config.BedrockModel{
						{
							Name:      "global.anthropic.claude-sonnet-5",
							Alias:     "sonnet-5",
							API:       "converse",
							StreamAPI: "converse-stream",
						},
					},
				},
			},
		},
		Now:         time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}

	// When
	auths, err := synth.Synthesize(ctx)

	// Then
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "bedrock" {
		t.Fatalf("provider = %q, want bedrock", auth.Provider)
	}
	if auth.Prefix != "bedrock" {
		t.Fatalf("prefix = %q, want bedrock", auth.Prefix)
	}
	wantAttrs := map[string]string{
		"api_key":             "env-bedrock-token",
		"auth_type":           "raw",
		"base_url":            "https://llm-fusion-hub.example/api/v2/proxy/aws/bedrock",
		"bedrock_name":        "llm-fusion",
		"bedrock_model_map":   `{"global.anthropic.claude-sonnet-5":"global.anthropic.claude-sonnet-5","sonnet-5":"global.anthropic.claude-sonnet-5"}`,
		"bedrock_api_map":     `{"global.anthropic.claude-sonnet-5":"converse","sonnet-5":"converse"}`,
		"bedrock_stream_map":  `{"global.anthropic.claude-sonnet-5":"converse-stream","sonnet-5":"converse-stream"}`,
		"header:X-Auth-Scope": "bedrock-smoke",
		"header:X-Team":       "proxy",
		"query:trace":         "1",
	}
	for key, want := range wantAttrs {
		if got := auth.Attributes[key]; got != want {
			t.Fatalf("attribute %s = %q, want %q", key, got, want)
		}
	}
	if auth.Status != coreauth.StatusActive {
		t.Fatalf("status = %s, want active", auth.Status)
	}
}

func TestConfigSynthesizer_BedrockKeys_defaultStreamAPITracksInvokeAPI(t *testing.T) {
	// Given
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			Bedrock: []config.BedrockProvider{
				{
					Name:    "llm-fusion",
					BaseURL: "https://llm-fusion-hub.example/api/v2/proxy/aws/bedrock",
					APIKey:  "token",
					Models: []config.BedrockModel{
						{
							Name:  "anthropic.claude-3-5-sonnet-20241022-v2:0",
							Alias: "sonnet-legacy",
							API:   "invoke",
						},
					},
				},
			},
		},
		Now:         time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}

	// When
	auths, err := synth.Synthesize(ctx)

	// Then
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	if got := auths[0].Attributes["bedrock_api_map"]; got != `{"anthropic.claude-3-5-sonnet-20241022-v2:0":"invoke","sonnet-legacy":"invoke"}` {
		t.Fatalf("bedrock_api_map = %q, want invoke map", got)
	}
	if got := auths[0].Attributes["bedrock_stream_map"]; got != `{"anthropic.claude-3-5-sonnet-20241022-v2:0":"invoke-stream","sonnet-legacy":"invoke-stream"}` {
		t.Fatalf("bedrock_stream_map = %q, want invoke-stream map", got)
	}
}
