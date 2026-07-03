package synthesizer

import (
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"gopkg.in/yaml.v3"
)

func TestNewConfigSynthesizer(t *testing.T) {
	synth := NewConfigSynthesizer()
	if synth == nil {
		t.Fatal("expected non-nil synthesizer")
	}
}

func TestConfigSynthesizer_Synthesize_NilContext(t *testing.T) {
	synth := NewConfigSynthesizer()
	auths, err := synth.Synthesize(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestConfigSynthesizer_Synthesize_NilConfig(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config:      nil,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}
	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestConfigSynthesizer_GeminiKeys(t *testing.T) {
	tests := []struct {
		name       string
		geminiKeys []config.GeminiKey
		wantLen    int
		validate   func(*testing.T, []*coreauth.Auth)
	}{
		{
			name: "single gemini key",
			geminiKeys: []config.GeminiKey{
				{APIKey: "test-key-123", Prefix: "team-a"},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if auths[0].Provider != "gemini" {
					t.Errorf("expected provider gemini, got %s", auths[0].Provider)
				}
				if auths[0].Prefix != "team-a" {
					t.Errorf("expected prefix team-a, got %s", auths[0].Prefix)
				}
				if auths[0].Label != "gemini-apikey" {
					t.Errorf("expected label gemini-apikey, got %s", auths[0].Label)
				}
				if auths[0].Attributes["api_key"] != "test-key-123" {
					t.Errorf("expected api_key test-key-123, got %s", auths[0].Attributes["api_key"])
				}
				if auths[0].Metadata != nil {
					t.Errorf("expected metadata to be nil when disable_cooling not set, got %v", auths[0].Metadata)
				}
				if auths[0].Status != coreauth.StatusActive {
					t.Errorf("expected status active, got %s", auths[0].Status)
				}
			},
		},
		{
			name: "gemini key disable cooling",
			geminiKeys: []config.GeminiKey{
				{APIKey: "test-key-123", Prefix: "team-a", DisableCooling: true},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if v, ok := auths[0].Metadata["disable_cooling"].(bool); !ok || !v {
					t.Errorf("expected disable_cooling=true, got %v", auths[0].Metadata["disable_cooling"])
				}
			},
		},
		{
			name: "gemini key with base url and proxy",
			geminiKeys: []config.GeminiKey{
				{
					APIKey:   "api-key",
					BaseURL:  "https://custom.api.com",
					ProxyURL: "http://proxy.local:8080",
					Prefix:   "custom",
				},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if auths[0].Attributes["base_url"] != "https://custom.api.com" {
					t.Errorf("expected base_url https://custom.api.com, got %s", auths[0].Attributes["base_url"])
				}
				if auths[0].ProxyURL != "http://proxy.local:8080" {
					t.Errorf("expected proxy_url http://proxy.local:8080, got %s", auths[0].ProxyURL)
				}
			},
		},
		{
			name: "gemini key with headers",
			geminiKeys: []config.GeminiKey{
				{
					APIKey:  "api-key",
					Headers: map[string]string{"X-Custom": "value"},
				},
			},
			wantLen: 1,
			validate: func(t *testing.T, auths []*coreauth.Auth) {
				if auths[0].Attributes["header:X-Custom"] != "value" {
					t.Errorf("expected header:X-Custom=value, got %s", auths[0].Attributes["header:X-Custom"])
				}
			},
		},
		{
			name: "empty api key skipped",
			geminiKeys: []config.GeminiKey{
				{APIKey: ""},
				{APIKey: "  "},
				{APIKey: "valid-key"},
			},
			wantLen: 1,
		},
		{
			name: "multiple gemini keys",
			geminiKeys: []config.GeminiKey{
				{APIKey: "key-1", Prefix: "a"},
				{APIKey: "key-2", Prefix: "b"},
				{APIKey: "key-3", Prefix: "c"},
			},
			wantLen: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synth := NewConfigSynthesizer()
			ctx := &SynthesisContext{
				Config: &config.Config{
					GeminiKey: tt.geminiKeys,
				},
				Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
				IDGenerator: NewStableIDGenerator(),
			}

			auths, err := synth.Synthesize(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(auths) != tt.wantLen {
				t.Fatalf("expected %d auths, got %d", tt.wantLen, len(auths))
			}

			if tt.validate != nil && len(auths) > 0 {
				tt.validate(t, auths)
			}
		})
	}
}

func TestConfigSynthesizer_ClaudeKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{
					APIKey:                  "sk-ant-api-xxx",
					Prefix:                  "main",
					BaseURL:                 "https://api.anthropic.com",
					DisableCooling:          true,
					RebuildMidSystemMessage: true,
					Models: []config.ClaudeModel{
						{Name: "claude-3-opus"},
						{Name: "claude-3-sonnet"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "claude" {
		t.Errorf("expected provider claude, got %s", auths[0].Provider)
	}
	if auths[0].Label != "claude-apikey" {
		t.Errorf("expected label claude-apikey, got %s", auths[0].Label)
	}
	if auths[0].Prefix != "main" {
		t.Errorf("expected prefix main, got %s", auths[0].Prefix)
	}
	if auths[0].Attributes["api_key"] != "sk-ant-api-xxx" {
		t.Errorf("expected api_key sk-ant-api-xxx, got %s", auths[0].Attributes["api_key"])
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in attributes")
	}
	if got := auths[0].Attributes["rebuild_mid_system_message"]; got != "true" {
		t.Errorf("expected rebuild_mid_system_message=true, got %s", got)
	}
	if v, ok := auths[0].Metadata["disable_cooling"].(bool); !ok || !v {
		t.Errorf("expected disable_cooling=true, got %v", auths[0].Metadata["disable_cooling"])
	}
}

func TestConfigSynthesizer_ClaudeKeys_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			ClaudeKey: []config.ClaudeKey{
				{APIKey: ""},    // empty, should be skipped
				{APIKey: "   "}, // whitespace, should be skipped
				{APIKey: "valid-key", Headers: map[string]string{"X-Custom": "value"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth (empty keys skipped), got %d", len(auths))
	}
	if auths[0].Attributes["header:X-Custom"] != "value" {
		t.Errorf("expected header:X-Custom=value, got %s", auths[0].Attributes["header:X-Custom"])
	}
}

func TestConfigSynthesizer_ConfigAPIKeyLabelsFromYAML(t *testing.T) {
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(`
gemini-api-key:
  - api-key: gemini-key
    label: Gemini facade key
claude-api-key:
  - api-key: claude-key
    label: Claude facade key
codex-api-key:
  - api-key: codex-key
    base-url: https://codex.example.test/v1
    label: Codex facade key
commandcode-api-key:
  - api-key: commandcode-key
    label: CommandCode facade key
vertex-api-key:
  - api-key: vertex-key
    label: Vertex facade key
`), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}

	synth := NewConfigSynthesizer()
	auths, err := synth.Synthesize(&SynthesisContext{
		Config:      &cfg,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	labelsByProvider := make(map[string]string, len(auths))
	for _, auth := range auths {
		labelsByProvider[auth.Provider] = auth.Label
	}

	want := map[string]string{
		"gemini":      "Gemini facade key",
		"claude":      "Claude facade key",
		"codex":       "Codex facade key",
		"commandcode": "CommandCode facade key",
		"vertex":      "Vertex facade key",
	}
	for provider, wantLabel := range want {
		if got := labelsByProvider[provider]; got != wantLabel {
			t.Fatalf("%s label = %q, want %q", provider, got, wantLabel)
		}
	}
}

func TestConfigSynthesizer_OpenAICompatAPIKeyEntryLabelFromYAML(t *testing.T) {
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(`
openai-compatibility:
  - name: FacadeProvider
    base-url: https://compat.example.test/v1
    api-key-entries:
      - api-key: compat-key-a
        label: First facade key
      - api-key: compat-key-b
`), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}

	synth := NewConfigSynthesizer()
	auths, err := synth.Synthesize(&SynthesisContext{
		Config:      &cfg,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 2 {
		t.Fatalf("auth count = %d, want 2", len(auths))
	}
	if got := auths[0].Label; got != "First facade key" {
		t.Fatalf("first label = %q, want First facade key", got)
	}
	if got := auths[1].Label; got != "FacadeProvider" {
		t.Fatalf("second label = %q, want provider fallback", got)
	}
}

func TestConfigSynthesizer_OpenAICompatUsesAPIKeyEnv(t *testing.T) {
	t.Setenv("MANTLE_TEST_KEY", "mantle-token")
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:      "bedrock-mantle",
					BaseURL:   "https://bedrock-mantle.example/v1",
					APIKeyEnv: "MANTLE_TEST_KEY",
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("auth count = %d, want 1", len(auths))
	}
	if got := auths[0].Attributes["api_key"]; got != "mantle-token" {
		t.Fatalf("api_key = %q, want mantle-token", got)
	}
	if got := auths[0].Label; got != "MANTLE_TEST_KEY" {
		t.Fatalf("label = %q, want env var label", got)
	}
	if got := auths[0].Attributes["provider_key"]; got != "openai-compatible-bedrock-mantle" {
		t.Fatalf("provider_key = %q, want namespaced mantle provider", got)
	}
}

func TestConfigSynthesizer_CodexKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{
				{
					APIKey:         "codex-key-123",
					Prefix:         "dev",
					BaseURL:        "https://api.openai.com",
					ProxyURL:       "http://proxy.local",
					Websockets:     true,
					ResponsesState: "false",
					Models:         []config.CodexModel{{Name: "gpt-5.5-aws", Alias: "gpt-5.5"}},
					DisableCooling: true,
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "codex" {
		t.Errorf("expected provider codex, got %s", auths[0].Provider)
	}
	if auths[0].Label != "codex-apikey" {
		t.Errorf("expected label codex-apikey, got %s", auths[0].Label)
	}
	if auths[0].ProxyURL != "http://proxy.local" {
		t.Errorf("expected proxy_url http://proxy.local, got %s", auths[0].ProxyURL)
	}
	if auths[0].Attributes["websockets"] != "true" {
		t.Errorf("expected websockets=true, got %s", auths[0].Attributes["websockets"])
	}
	if auths[0].Attributes["responses_state"] != "false" {
		t.Errorf("expected responses_state=false, got %s", auths[0].Attributes["responses_state"])
	}
	modelsAttr := auths[0].Attributes["responses_state_models"]
	if !strings.Contains(modelsAttr, "gpt-5.5-aws") || !strings.Contains(modelsAttr, "dev/gpt-5.5-aws") {
		t.Errorf("expected responses_state_models to include route model names, got %s", modelsAttr)
	}
	if v, ok := auths[0].Metadata["disable_cooling"].(bool); !ok || !v {
		t.Errorf("expected disable_cooling=true, got %v", auths[0].Metadata["disable_cooling"])
	}
}

func TestConfigSynthesizer_CodexKeys_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CodexKey: []config.CodexKey{
				{APIKey: ""},   // empty, should be skipped
				{APIKey: "  "}, // whitespace, should be skipped
				{
					APIKey:      "valid-key",
					Headers:     map[string]string{"Authorization": "Bearer xyz"},
					QueryParams: map[string]string{"api-version": "preview"},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth (empty keys skipped), got %d", len(auths))
	}
	if auths[0].Attributes["header:Authorization"] != "Bearer xyz" {
		t.Errorf("expected header:Authorization=Bearer xyz, got %s", auths[0].Attributes["header:Authorization"])
	}
	if auths[0].Attributes["query:api-version"] != "preview" {
		t.Errorf("expected query:api-version=preview, got %s", auths[0].Attributes["query:api-version"])
	}
}

func TestConfigSynthesizer_OpenAICompat(t *testing.T) {
	tests := []struct {
		name    string
		compat  []config.OpenAICompatibility
		wantLen int
	}{
		{
			name: "with APIKeyEntries",
			compat: []config.OpenAICompatibility{
				{
					Name:           "CustomProvider",
					BaseURL:        "https://custom.api.com",
					DisableCooling: true,
					Headers:        map[string]string{"X-Custom": "value"},
					QueryParams:    map[string]string{"api-version": "preview"},
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "key-1"},
						{APIKey: "key-2"},
					},
				},
			},
			wantLen: 2,
		},
		{
			name: "empty APIKeyEntries included (legacy)",
			compat: []config.OpenAICompatibility{
				{
					Name:    "EmptyKeys",
					BaseURL: "https://empty.api.com",
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: ""},
						{APIKey: "   "},
					},
				},
			},
			wantLen: 2,
		},
		{
			name: "without APIKeyEntries (fallback)",
			compat: []config.OpenAICompatibility{
				{
					Name:    "NoKeyProvider",
					BaseURL: "https://no-key.api.com",
				},
			},
			wantLen: 1,
		},
		{
			name: "empty name defaults",
			compat: []config.OpenAICompatibility{
				{
					Name:    "",
					BaseURL: "https://default.api.com",
				},
			},
			wantLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synth := NewConfigSynthesizer()
			ctx := &SynthesisContext{
				Config: &config.Config{
					OpenAICompatibility: tt.compat,
				},
				Now:         time.Now(),
				IDGenerator: NewStableIDGenerator(),
			}

			auths, err := synth.Synthesize(ctx)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(auths) != tt.wantLen {
				t.Fatalf("expected %d auths, got %d", tt.wantLen, len(auths))
			}
			if tt.name == "with APIKeyEntries" {
				for i := range auths {
					if v, ok := auths[i].Metadata["disable_cooling"].(bool); !ok || !v {
						t.Fatalf("expected auth[%d].disable_cooling=true, got %v", i, auths[i].Metadata["disable_cooling"])
					}
					if got := auths[i].Attributes["header:X-Custom"]; got != "value" {
						t.Fatalf("auth[%d] header:X-Custom = %q, want value", i, got)
					}
					if got := auths[i].Attributes["query:api-version"]; got != "preview" {
						t.Fatalf("auth[%d] query:api-version = %q, want preview", i, got)
					}
				}
			}
		})
	}
}

func TestConfigSynthesizer_OpenAICompat_UsesNamespacedProviderKey(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "kimi",
					BaseURL: "https://kimi-compatible.example.com/v1",
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "test-key"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "openai-compatible-kimi" {
		t.Fatalf("provider = %q, want openai-compatible-kimi", auth.Provider)
	}
	if auth.Attributes["provider_key"] != "openai-compatible-kimi" {
		t.Fatalf("provider_key = %q, want openai-compatible-kimi", auth.Attributes["provider_key"])
	}
	if auth.Attributes["compat_name"] != "kimi" {
		t.Fatalf("compat_name = %q, want kimi", auth.Attributes["compat_name"])
	}
}

func TestConfigSynthesizer_VertexCompat(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{
					APIKey:  "vertex-key-123",
					BaseURL: "https://vertex.googleapis.com",
					Prefix:  "vertex-prod",
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	if auths[0].Provider != "vertex" {
		t.Errorf("expected provider vertex, got %s", auths[0].Provider)
	}
	if auths[0].Label != "vertex-apikey" {
		t.Errorf("expected label vertex-apikey, got %s", auths[0].Label)
	}
	if auths[0].Prefix != "vertex-prod" {
		t.Errorf("expected prefix vertex-prod, got %s", auths[0].Prefix)
	}
}

func TestConfigSynthesizer_VertexCompat_SkipsEmptyAndHeaders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{APIKey: "", BaseURL: "https://vertex.api"},   // empty key creates auth without api_key attr
				{APIKey: "  ", BaseURL: "https://vertex.api"}, // whitespace key creates auth without api_key attr
				{APIKey: "valid-key", BaseURL: "https://vertex.api", Headers: map[string]string{"X-Vertex": "test"}},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Vertex compat doesn't skip empty keys - it creates auths without api_key attribute
	if len(auths) != 3 {
		t.Fatalf("expected 3 auths, got %d", len(auths))
	}
	// First two should not have api_key attribute
	if _, ok := auths[0].Attributes["api_key"]; ok {
		t.Error("expected first auth to not have api_key attribute")
	}
	if _, ok := auths[1].Attributes["api_key"]; ok {
		t.Error("expected second auth to not have api_key attribute")
	}
	// Third should have headers
	if auths[2].Attributes["header:X-Vertex"] != "test" {
		t.Errorf("expected header:X-Vertex=test, got %s", auths[2].Attributes["header:X-Vertex"])
	}
}

func TestConfigSynthesizer_OpenAICompat_WithModelsHash(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "TestProvider",
					BaseURL: "https://test.api.com",
					Models: []config.OpenAICompatibilityModel{
						{Name: "model-a"},
						{Name: "model-b"},
					},
					APIKeyEntries: []config.OpenAICompatibilityAPIKey{
						{APIKey: "key-with-models"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in attributes")
	}
	if auths[0].Attributes["api_key"] != "key-with-models" {
		t.Errorf("expected api_key key-with-models, got %s", auths[0].Attributes["api_key"])
	}
}

func TestConfigSynthesizer_OpenAICompat_FallbackWithModels(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OpenAICompatibility: []config.OpenAICompatibility{
				{
					Name:    "NoKeyWithModels",
					BaseURL: "https://nokey.api.com",
					Models: []config.OpenAICompatibilityModel{
						{Name: "model-x"},
					},
					Headers: map[string]string{"X-API": "header-value"},
					// No APIKeyEntries - should use fallback path
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in fallback path")
	}
	if auths[0].Attributes["header:X-API"] != "header-value" {
		t.Errorf("expected header:X-API=header-value, got %s", auths[0].Attributes["header:X-API"])
	}
}

func TestConfigSynthesizer_VertexCompat_WithModels(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			VertexCompatAPIKey: []config.VertexCompatKey{
				{
					APIKey:  "vertex-key",
					BaseURL: "https://vertex.api",
					Models: []config.VertexCompatModel{
						{Name: "gemini-pro", Alias: "pro"},
						{Name: "gemini-ultra", Alias: "ultra"},
					},
				},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	if _, ok := auths[0].Attributes["models_hash"]; !ok {
		t.Error("expected models_hash in vertex auth with models")
	}
}

func TestConfigSynthesizer_IDStability(t *testing.T) {
	cfg := &config.Config{
		GeminiKey: []config.GeminiKey{
			{APIKey: "stable-key", Prefix: "test"},
		},
	}

	// Generate IDs twice with fresh generators
	synth1 := NewConfigSynthesizer()
	ctx1 := &SynthesisContext{
		Config:      cfg,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}
	auths1, _ := synth1.Synthesize(ctx1)

	synth2 := NewConfigSynthesizer()
	ctx2 := &SynthesisContext{
		Config:      cfg,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}
	auths2, _ := synth2.Synthesize(ctx2)

	if auths1[0].ID != auths2[0].ID {
		t.Errorf("same config should produce same ID: got %q and %q", auths1[0].ID, auths2[0].ID)
	}
}

func TestConfigSynthesizer_AllProviders(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			GeminiKey: []config.GeminiKey{
				{APIKey: "gemini-key"},
			},
			ClaudeKey: []config.ClaudeKey{
				{APIKey: "claude-key"},
			},
			CodexKey: []config.CodexKey{
				{APIKey: "codex-key"},
			},
			OpenAICompatibility: []config.OpenAICompatibility{
				{Name: "compat", BaseURL: "https://compat.api"},
			},
			VertexCompatAPIKey: []config.VertexCompatKey{
				{APIKey: "vertex-key", BaseURL: "https://vertex.api"},
			},
		},
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 5 {
		t.Fatalf("expected 5 auths, got %d", len(auths))
	}

	providers := make(map[string]bool)
	for _, a := range auths {
		providers[a.Provider] = true
	}

	expected := []string{"gemini", "claude", "codex", "openai-compatible-compat", "vertex"}
	for _, p := range expected {
		if !providers[p] {
			t.Errorf("expected provider %s not found", p)
		}
	}
}
