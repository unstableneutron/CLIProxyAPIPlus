package config

import "testing"

func TestParseConfigBytes_CodexResponsesStateBoolean(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`
codex-api-key:
  - api-key: test-key
    base-url: https://example.test/v1
    models:
      - name: gpt-5.5-aws
    responses-state: false
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if len(cfg.CodexKey) != 1 {
		t.Fatalf("len(CodexKey) = %d, want 1", len(cfg.CodexKey))
	}
	if got := string(cfg.CodexKey[0].ResponsesState); got != "false" {
		t.Fatalf("ResponsesState = %q, want false", got)
	}
}
