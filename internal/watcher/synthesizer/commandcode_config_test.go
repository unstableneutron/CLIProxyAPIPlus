package synthesizer

import (
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestConfigSynthesizer_CommandCodeKeys(t *testing.T) {
	synth := NewConfigSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			CommandCodeKey: []config.CommandCodeKey{
				{
					APIKey:         " user_test ",
					Prefix:         " team-a ",
					BaseURL:        " https://cc.example.test ",
					ProxyURL:       " direct ",
					Priority:       7,
					DisableCooling: true,
					Headers:        map[string]string{"X-Custom": " value "},
					Models: []config.CommandCodeModel{
						{Name: "deepseek/deepseek-v4-flash", Alias: "cc-flash"},
					},
				},
			},
		},
		Now:         time.Date(2026, 5, 5, 0, 0, 0, 0, time.UTC),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("Synthesize() error = %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "commandcode" {
		t.Fatalf("provider = %q", auth.Provider)
	}
	if auth.Label != "commandcode-apikey" {
		t.Fatalf("label = %q", auth.Label)
	}
	if auth.Prefix != "team-a" {
		t.Fatalf("prefix = %q", auth.Prefix)
	}
	if auth.ProxyURL != "direct" {
		t.Fatalf("proxy = %q", auth.ProxyURL)
	}
	if auth.Attributes["api_key"] != "user_test" {
		t.Fatalf("api_key attr = %q", auth.Attributes["api_key"])
	}
	if auth.Attributes["base_url"] != "https://cc.example.test" {
		t.Fatalf("base_url attr = %q", auth.Attributes["base_url"])
	}
	if auth.Attributes["priority"] != "7" {
		t.Fatalf("priority attr = %q", auth.Attributes["priority"])
	}
	if auth.Attributes["models_hash"] == "" {
		t.Fatal("expected models_hash attr")
	}
	if auth.Attributes["header:X-Custom"] != "value" {
		t.Fatalf("header attr = %q", auth.Attributes["header:X-Custom"])
	}
	if v, ok := auth.Metadata["disable_cooling"].(bool); !ok || !v {
		t.Fatalf("disable_cooling metadata = %#v", auth.Metadata)
	}
	if kind, value := auth.AccountInfo(); kind != "api_key" || value != "user_test" {
		t.Fatalf("AccountInfo() = (%q, %q)", kind, value)
	}
}

func TestSynthesizeAuthFile_CommandCodeExtractsAPIKey(t *testing.T) {
	ctx := &SynthesisContext{Config: &config.Config{}, AuthDir: "/auth", Now: time.Now(), IDGenerator: NewStableIDGenerator()}
	auths := SynthesizeAuthFile(ctx, "/auth/commandcode.json", []byte(`{"type":"commandcode","apiKey":"user_file","label":"Command Code"}`))
	if len(auths) != 1 {
		t.Fatalf("expected one auth, got %d", len(auths))
	}
	auth := auths[0]
	if auth.Provider != "commandcode" {
		t.Fatalf("provider = %q", auth.Provider)
	}
	if auth.Attributes["api_key"] != "user_file" {
		t.Fatalf("api_key attr = %q", auth.Attributes["api_key"])
	}
	if kind, value := auth.AccountInfo(); kind != "api_key" || value != "user_file" {
		t.Fatalf("AccountInfo() = (%q, %q)", kind, value)
	}
	if auth.Status != coreauth.StatusActive {
		t.Fatalf("status = %s", auth.Status)
	}
}
