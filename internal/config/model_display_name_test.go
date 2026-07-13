package config

import (
	"encoding/json"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestModelDisplayNameConfigDecoding(t *testing.T) {
	const yamlConfig = `codex-api-key:
  - models:
      - name: codex-upstream
        alias: codex-alias
        display-name: Codex Name
claude-api-key:
  - models:
      - name: claude-upstream
        alias: claude-alias
        display-name: Claude Name
gemini-api-key:
  - models:
      - name: gemini-upstream
        alias: gemini-alias
        display-name: Gemini Name
vertex-api-key:
  - models:
      - name: vertex-upstream
        alias: vertex-alias
        display-name: Vertex Name
openai-compatibility:
  - models:
      - name: compat-upstream
        alias: compat-alias
        display-name: Compatibility Name
commandcode-api-key:
  - models:
      - name: commandcode-upstream
        alias: commandcode-alias
        display-name: CommandCode Name
bedrock:
  - models:
      - name: bedrock-upstream
        alias: bedrock-alias
        display-name: Bedrock Name
`
	const jsonConfig = `{"codex-api-key":[{"models":[{"name":"codex-upstream","alias":"codex-alias","display-name":"Codex Name"}]}],"claude-api-key":[{"models":[{"name":"claude-upstream","alias":"claude-alias","display-name":"Claude Name"}]}],"gemini-api-key":[{"models":[{"name":"gemini-upstream","alias":"gemini-alias","display-name":"Gemini Name"}]}],"vertex-api-key":[{"models":[{"name":"vertex-upstream","alias":"vertex-alias","display-name":"Vertex Name"}]}],"openai-compatibility":[{"models":[{"name":"compat-upstream","alias":"compat-alias","display-name":"Compatibility Name"}]}],"commandcode-api-key":[{"models":[{"name":"commandcode-upstream","alias":"commandcode-alias","display-name":"CommandCode Name"}]}],"bedrock":[{"models":[{"name":"bedrock-upstream","alias":"bedrock-alias","display-name":"Bedrock Name"}]}]}`

	for _, tt := range []struct {
		name   string
		decode func(*Config) error
	}{
		{
			name: "YAML",
			decode: func(cfg *Config) error {
				return yaml.Unmarshal([]byte(yamlConfig), cfg)
			},
		},
		{
			name: "JSON",
			decode: func(cfg *Config) error {
				return json.Unmarshal([]byte(jsonConfig), cfg)
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var cfg Config
			if errDecode := tt.decode(&cfg); errDecode != nil {
				t.Fatalf("decode config: %v", errDecode)
			}
			if got := cfg.CodexKey[0].Models[0].DisplayName; got != "Codex Name" {
				t.Fatalf("Codex display name = %q", got)
			}
			if got := cfg.ClaudeKey[0].Models[0].DisplayName; got != "Claude Name" {
				t.Fatalf("Claude display name = %q", got)
			}
			if got := cfg.GeminiKey[0].Models[0].DisplayName; got != "Gemini Name" {
				t.Fatalf("Gemini display name = %q", got)
			}
			if got := cfg.VertexCompatAPIKey[0].Models[0].DisplayName; got != "Vertex Name" {
				t.Fatalf("Vertex display name = %q", got)
			}
			if got := cfg.OpenAICompatibility[0].Models[0].DisplayName; got != "Compatibility Name" {
				t.Fatalf("OpenAI compatibility display name = %q", got)
			}
			if got := cfg.CommandCodeKey[0].Models[0].DisplayName; got != "CommandCode Name" {
				t.Fatalf("CommandCode display name = %q", got)
			}
			if got := cfg.Bedrock[0].Models[0].DisplayName; got != "Bedrock Name" {
				t.Fatalf("Bedrock display name = %q", got)
			}
		})
	}
}
