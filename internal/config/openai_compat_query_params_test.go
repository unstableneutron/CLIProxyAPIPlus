package config

import "testing"

func TestSanitizeOpenAICompatibilityNormalizesQueryParams(t *testing.T) {
	cfg := &Config{OpenAICompatibility: []OpenAICompatibility{{
		BaseURL: " https://compat.example/v1 ",
		QueryParams: map[string]string{
			" api-version ": " preview ",
			"":              "ignored",
			"empty":         "   ",
		},
	}}}

	cfg.SanitizeOpenAICompatibility()

	if len(cfg.OpenAICompatibility) != 1 {
		t.Fatalf("provider count = %d, want 1", len(cfg.OpenAICompatibility))
	}
	params := cfg.OpenAICompatibility[0].QueryParams
	if len(params) != 1 {
		t.Fatalf("query params = %#v, want one normalized entry", params)
	}
	if got := params["api-version"]; got != "preview" {
		t.Fatalf("api-version = %q, want preview", got)
	}
}
