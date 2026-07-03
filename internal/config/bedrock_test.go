package config

import "testing"

func TestSanitizeBedrockProviders_dropsUnsupportedAuthType(t *testing.T) {
	cfg := &Config{
		Bedrock: []BedrockProvider{
			{
				BaseURL: " https://bedrock.example ",
				Auth: BedrockAuth{
					Type: "sigv4",
				},
				Models: []BedrockModel{
					{
						Name:  "anthropic.claude-3-5-sonnet-20241022-v2:0",
						Alias: "sonnet-legacy",
						API:   "invoke",
					},
				},
			},
		},
	}

	cfg.SanitizeBedrockProviders()

	if len(cfg.Bedrock) != 0 {
		t.Fatalf("bedrock providers = %d, want unsupported auth provider dropped", len(cfg.Bedrock))
	}
}

func TestSanitizeBedrockProviders_defaultsInvokeStreamAPI(t *testing.T) {
	cfg := &Config{
		Bedrock: []BedrockProvider{
			{
				BaseURL: " https://bedrock.example ",
				Auth: BedrockAuth{
					Type: "raw",
				},
				Models: []BedrockModel{
					{
						Name:  "anthropic.claude-3-5-sonnet-20241022-v2:0",
						Alias: "sonnet-legacy",
						API:   "invoke",
					},
				},
			},
		},
	}

	cfg.SanitizeBedrockProviders()

	if len(cfg.Bedrock) != 1 {
		t.Fatalf("bedrock providers = %d, want 1", len(cfg.Bedrock))
	}
	if got := cfg.Bedrock[0].Models[0].API; got != "invoke" {
		t.Fatalf("api = %q, want invoke", got)
	}
	if got := cfg.Bedrock[0].Models[0].StreamAPI; got != "invoke-stream" {
		t.Fatalf("stream api = %q, want invoke-stream", got)
	}
}
