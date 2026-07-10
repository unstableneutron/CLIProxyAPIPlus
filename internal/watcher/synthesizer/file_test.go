package synthesizer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qoderauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/qoder"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestNewFileSynthesizer(t *testing.T) {
	synth := NewFileSynthesizer()
	if synth == nil {
		t.Fatal("expected non-nil synthesizer")
	}
}

func TestFileSynthesizer_Synthesize_NilContext(t *testing.T) {
	synth := NewFileSynthesizer()
	auths, err := synth.Synthesize(nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected empty auths, got %d", len(auths))
	}
}

func TestFileSynthesizer_Synthesize_EmptyAuthDir(t *testing.T) {
	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     "",
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

func TestFileSynthesizer_Synthesize_NonExistentDir(t *testing.T) {
	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     "/non/existent/path",
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

func TestFileSynthesizer_Synthesize_ValidAuthFile(t *testing.T) {
	tempDir := t.TempDir()

	// Create a valid auth file
	authData := map[string]any{
		"type":      "claude",
		"email":     "test@example.com",
		"proxy_url": "http://proxy.local",
		"prefix":    "test-prefix",
		"headers": map[string]string{
			" X-Test ": " value ",
			"X-Empty":  "  ",
		},
		"disable_cooling": true,
		"request_retry":   2,
	}
	data, _ := json.Marshal(authData)
	err := os.WriteFile(filepath.Join(tempDir, "claude-auth.json"), data, 0644)
	if err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
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
	if auths[0].Label != "test@example.com" {
		t.Errorf("expected label test@example.com, got %s", auths[0].Label)
	}
	if auths[0].Prefix != "test-prefix" {
		t.Errorf("expected prefix test-prefix, got %s", auths[0].Prefix)
	}
	if auths[0].ProxyURL != "http://proxy.local" {
		t.Errorf("expected proxy_url http://proxy.local, got %s", auths[0].ProxyURL)
	}
	if got := auths[0].Attributes["header:X-Test"]; got != "value" {
		t.Errorf("expected header:X-Test value, got %q", got)
	}
	if _, ok := auths[0].Attributes["header:X-Empty"]; ok {
		t.Errorf("expected header:X-Empty to be absent, got %q", auths[0].Attributes["header:X-Empty"])
	}
	if v, ok := auths[0].Metadata["disable_cooling"].(bool); !ok || !v {
		t.Errorf("expected disable_cooling true, got %v", auths[0].Metadata["disable_cooling"])
	}
	if v, ok := auths[0].Metadata["request_retry"].(float64); !ok || int(v) != 2 {
		t.Errorf("expected request_retry 2, got %v", auths[0].Metadata["request_retry"])
	}
	if auths[0].Status != coreauth.StatusActive {
		t.Errorf("expected status active, got %s", auths[0].Status)
	}
}

func TestFileSynthesizer_Synthesize_IgnoresGeminiProviderFile(t *testing.T) {
	tempDir := t.TempDir()

	authData := map[string]any{
		"type":  "gemini",
		"email": "gemini@example.com",
	}
	data, _ := json.Marshal(authData)
	err := os.WriteFile(filepath.Join(tempDir, "gemini-auth.json"), data, 0644)
	if err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected Gemini auth file to be ignored, got %d auths", len(auths))
	}
}

func TestSynthesizeAuthFileExpandsPluginMultiAuths(t *testing.T) {
	tempDir := t.TempDir()
	fullPath := filepath.Join(tempDir, "geminicli.json")
	raw := []byte(`{"type":"gemini-cli","excluded_models":["model-a"],"headers":{"X-Test":"value"}}`)

	ctx := &SynthesisContext{
		Config:  &config.Config{},
		AuthDir: tempDir,
		Now:     time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC),
		PluginAuthParser: multiAuthParserFunc(func(ctx context.Context, req pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
			if req.Provider != "gemini-cli" || req.Path != fullPath || req.FileName != "geminicli.json" {
				t.Fatalf("ParseAuths request = %#v, want file context", req)
			}
			return []*coreauth.Auth{
				{
					ID:       "geminicli.json",
					Provider: "gemini-cli",
					Metadata: map[string]any{
						"type": "gemini-cli",
						"headers": map[string]any{
							"X-Test": "value",
						},
					},
				},
				nil,
				{
					ID:       "geminicli-project-a.json",
					Provider: "gemini-cli",
					Metadata: map[string]any{
						"type":       "gemini-cli",
						"project_id": "project-a",
						"headers": map[string]any{
							"X-Test": "value",
						},
					},
				},
			}, true, nil
		}),
	}

	auths := SynthesizeAuthFile(ctx, fullPath, raw)
	if len(auths) != 2 {
		t.Fatalf("SynthesizeAuthFile() len = %d, want two plugin auths", len(auths))
	}
	if firstIndex, secondIndex := auths[0].EnsureIndex(), auths[1].EnsureIndex(); firstIndex == "" || firstIndex == secondIndex {
		t.Fatalf("auth indexes = %q/%q, want distinct non-empty indexes", firstIndex, secondIndex)
	}
	for _, auth := range auths {
		if !coreauth.IsPluginVirtualAuth(auth) {
			t.Fatalf("auth attributes = %#v, want plugin virtual marker", auth.Attributes)
		}
		if auth.Attributes[coreauth.AttributeVirtualSource] != fullPath {
			t.Fatalf("virtual_source = %q, want %q", auth.Attributes[coreauth.AttributeVirtualSource], fullPath)
		}
		if auth.Attributes["path"] != fullPath || auth.Attributes["source"] != fullPath {
			t.Fatalf("auth attributes = %#v, want source path", auth.Attributes)
		}
		if gotHeader := auth.Attributes["header:X-Test"]; gotHeader != "value" {
			t.Fatalf("header:X-Test = %q, want value", gotHeader)
		}
		if gotKind := auth.Attributes["auth_kind"]; gotKind != "oauth" {
			t.Fatalf("auth_kind = %q, want oauth", gotKind)
		}
	}
	if gotProject := auths[1].Metadata["project_id"]; gotProject != "project-a" {
		t.Fatalf("project_id = %#v, want project-a", gotProject)
	}
}

func TestSynthesizeAuthFileAppliesSourceDisabledToPluginMultiAuths(t *testing.T) {
	tempDir := t.TempDir()
	fullPath := filepath.Join(tempDir, "geminicli.json")
	raw := []byte(`{"type":"gemini-cli","disabled":true}`)

	ctx := &SynthesisContext{
		Config:  &config.Config{},
		AuthDir: tempDir,
		Now:     time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC),
		PluginAuthParser: multiAuthParserFunc(func(context.Context, pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
			return []*coreauth.Auth{
				{ID: "geminicli.json", Provider: "gemini-cli", Metadata: map[string]any{"type": "gemini-cli"}},
				{ID: "geminicli-project-a.json", Provider: "gemini-cli", Metadata: map[string]any{"type": "gemini-cli", "project_id": "project-a"}},
			}, true, nil
		}),
	}

	auths := SynthesizeAuthFile(ctx, fullPath, raw)
	if len(auths) != 2 {
		t.Fatalf("SynthesizeAuthFile() len = %d, want two plugin auths", len(auths))
	}
	for _, auth := range auths {
		if !auth.Disabled || auth.Status != coreauth.StatusDisabled {
			t.Fatalf("auth %s disabled/status = %v/%s, want disabled", auth.ID, auth.Disabled, auth.Status)
		}
		if got, _ := auth.Metadata["disabled"].(bool); !got {
			t.Fatalf("auth %s metadata disabled = %#v, want true", auth.ID, auth.Metadata["disabled"])
		}
	}
}

func TestSynthesizeAuthFilePluginHandledEmptySuppressesBuiltin(t *testing.T) {
	tempDir := t.TempDir()
	fullPath := filepath.Join(tempDir, "codex.json")
	raw := []byte(`{"type":"codex","access_token":"token"}`)

	ctx := &SynthesisContext{
		Config:  &config.Config{},
		AuthDir: tempDir,
		Now:     time.Date(2026, 6, 21, 0, 0, 0, 0, time.UTC),
		PluginAuthParser: multiAuthParserFunc(func(context.Context, pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
			return nil, true, nil
		}),
	}

	auths := SynthesizeAuthFile(ctx, fullPath, raw)
	if len(auths) != 0 {
		t.Fatalf("SynthesizeAuthFile() len = %d, want plugin-handled empty result", len(auths))
	}
}

type multiAuthParserFunc func(context.Context, pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error)

func (f multiAuthParserFunc) ParseAuth(context.Context, pluginapi.AuthParseRequest) (*coreauth.Auth, bool, error) {
	return nil, false, nil
}

func (f multiAuthParserFunc) ParseAuths(ctx context.Context, req pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
	return f(ctx, req)
}

func TestFileSynthesizer_Synthesize_SkipsInvalidFiles(t *testing.T) {
	tempDir := t.TempDir()

	// Create various invalid files
	_ = os.WriteFile(filepath.Join(tempDir, "not-json.txt"), []byte("text content"), 0644)
	_ = os.WriteFile(filepath.Join(tempDir, "invalid.json"), []byte("not valid json"), 0644)
	_ = os.WriteFile(filepath.Join(tempDir, "empty.json"), []byte(""), 0644)
	_ = os.WriteFile(filepath.Join(tempDir, "no-type.json"), []byte(`{"email": "test@example.com"}`), 0644)

	// Create one valid file
	validData, _ := json.Marshal(map[string]any{"type": "claude", "email": "valid@example.com"})
	_ = os.WriteFile(filepath.Join(tempDir, "valid.json"), validData, 0644)

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 1 {
		t.Fatalf("only valid auth file should be processed, got %d", len(auths))
	}
	if auths[0].Label != "valid@example.com" {
		t.Errorf("expected label valid@example.com, got %s", auths[0].Label)
	}
}

func TestFileSynthesizer_Synthesize_SkipsDirectories(t *testing.T) {
	tempDir := t.TempDir()

	// Create a subdirectory with a json file inside
	subDir := filepath.Join(tempDir, "subdir.json")
	err := os.Mkdir(subDir, 0755)
	if err != nil {
		t.Fatalf("failed to create subdir: %v", err)
	}

	// Create a valid file in root
	validData, _ := json.Marshal(map[string]any{"type": "claude"})
	_ = os.WriteFile(filepath.Join(tempDir, "valid.json"), validData, 0644)

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
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
}

func TestFileSynthesizer_Synthesize_RelativeID(t *testing.T) {
	tempDir := t.TempDir()

	authData := map[string]any{"type": "claude"}
	data, _ := json.Marshal(authData)
	err := os.WriteFile(filepath.Join(tempDir, "my-auth.json"), data, 0644)
	if err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
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

	// ID should be relative path
	if auths[0].ID != "my-auth.json" {
		t.Errorf("expected ID my-auth.json, got %s", auths[0].ID)
	}
}

func TestFileSynthesizer_Synthesize_PrefixValidation(t *testing.T) {
	tests := []struct {
		name       string
		prefix     string
		wantPrefix string
	}{
		{"valid prefix", "myprefix", "myprefix"},
		{"prefix with slashes trimmed", "/myprefix/", "myprefix"},
		{"prefix with spaces trimmed", "  myprefix  ", "myprefix"},
		{"prefix with internal slash rejected", "my/prefix", ""},
		{"empty prefix", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			authData := map[string]any{
				"type":   "claude",
				"prefix": tt.prefix,
			}
			data, _ := json.Marshal(authData)
			_ = os.WriteFile(filepath.Join(tempDir, "auth.json"), data, 0644)

			synth := NewFileSynthesizer()
			ctx := &SynthesisContext{
				Config:      &config.Config{},
				AuthDir:     tempDir,
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
			if auths[0].Prefix != tt.wantPrefix {
				t.Errorf("expected prefix %q, got %q", tt.wantPrefix, auths[0].Prefix)
			}
		})
	}
}

func TestFileSynthesizer_Synthesize_PriorityParsing(t *testing.T) {
	tests := []struct {
		name     string
		priority any
		want     string
		hasValue bool
	}{
		{
			name:     "string with spaces",
			priority: " 10 ",
			want:     "10",
			hasValue: true,
		},
		{
			name:     "number",
			priority: 8,
			want:     "8",
			hasValue: true,
		},
		{
			name:     "invalid string",
			priority: "1x",
			hasValue: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			authData := map[string]any{
				"type":     "claude",
				"priority": tt.priority,
			}
			data, _ := json.Marshal(authData)
			errWriteFile := os.WriteFile(filepath.Join(tempDir, "auth.json"), data, 0644)
			if errWriteFile != nil {
				t.Fatalf("failed to write auth file: %v", errWriteFile)
			}

			synth := NewFileSynthesizer()
			ctx := &SynthesisContext{
				Config:      &config.Config{},
				AuthDir:     tempDir,
				Now:         time.Now(),
				IDGenerator: NewStableIDGenerator(),
			}

			auths, errSynthesize := synth.Synthesize(ctx)
			if errSynthesize != nil {
				t.Fatalf("unexpected error: %v", errSynthesize)
			}
			if len(auths) != 1 {
				t.Fatalf("expected 1 auth, got %d", len(auths))
			}

			value, ok := auths[0].Attributes["priority"]
			if tt.hasValue {
				if !ok {
					t.Fatal("expected priority attribute to be set")
				}
				if value != tt.want {
					t.Fatalf("expected priority %q, got %q", tt.want, value)
				}
				return
			}
			if ok {
				t.Fatalf("expected priority attribute to be absent, got %q", value)
			}
		})
	}
}

func TestFileSynthesizer_Synthesize_OAuthExcludedModelsMerged(t *testing.T) {
	tempDir := t.TempDir()
	authData := map[string]any{
		"type":            "claude",
		"excluded_models": []string{"custom-model", "MODEL-B"},
	}
	data, _ := json.Marshal(authData)
	errWriteFile := os.WriteFile(filepath.Join(tempDir, "auth.json"), data, 0644)
	if errWriteFile != nil {
		t.Fatalf("failed to write auth file: %v", errWriteFile)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config: &config.Config{
			OAuthExcludedModels: map[string][]string{
				"claude": {"shared", "model-b"},
			},
		},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, errSynthesize := synth.Synthesize(ctx)
	if errSynthesize != nil {
		t.Fatalf("unexpected error: %v", errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	got := auths[0].Attributes["excluded_models"]
	want := "custom-model,model-b,shared"
	if got != want {
		t.Fatalf("expected excluded_models %q, got %q", want, got)
	}
}

func TestFileSynthesizer_Synthesize_OAuthModelAliases(t *testing.T) {
	tempDir := t.TempDir()
	authData := map[string]any{
		"type":  "codex",
		"email": "codex@example.com",
		"model-aliases": []map[string]any{
			{"name": " gpt-5.3-codex-spark ", "alias": " gpt-5.5 "},
			{"name": "gpt-5.3-codex-spark", "alias": "gpt-5.4", "fork": true},
			{"name": "gpt-5.3-codex-spark", "alias": "gpt-5.5"},
			{"name": "", "alias": "ignored"},
		},
	}
	data, _ := json.Marshal(authData)
	errWriteFile := os.WriteFile(filepath.Join(tempDir, "codex-auth.json"), data, 0644)
	if errWriteFile != nil {
		t.Fatalf("failed to write auth file: %v", errWriteFile)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, errSynthesize := synth.Synthesize(ctx)
	if errSynthesize != nil {
		t.Fatalf("unexpected error: %v", errSynthesize)
	}
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	got := auths[0].Attributes["model_aliases"]
	want := `[{"name":"gpt-5.3-codex-spark","alias":"gpt-5.5"},{"name":"gpt-5.3-codex-spark","alias":"gpt-5.4","fork":true}]`
	if got != want {
		t.Fatalf("expected model_aliases %q, got %q", want, got)
	}
}

func TestFileSynthesizer_Synthesize_IgnoresGeminiOAuthFile(t *testing.T) {
	tempDir := t.TempDir()

	authData := map[string]any{
		"type":       "gemini",
		"email":      "multi@example.com",
		"project_id": "project-a, project-b, project-c",
		"priority":   " 10 ",
	}
	data, _ := json.Marshal(authData)
	err := os.WriteFile(filepath.Join(tempDir, "gemini-multi.json"), data, 0644)
	if err != nil {
		t.Fatalf("failed to write auth file: %v", err)
	}

	synth := NewFileSynthesizer()
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}

	auths, err := synth.Synthesize(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(auths) != 0 {
		t.Fatalf("expected Gemini auth file to be ignored, got %d auths", len(auths))
	}
}

func TestFileSynthesizer_Synthesize_NoteParsing(t *testing.T) {
	tests := []struct {
		name     string
		note     any
		want     string
		hasValue bool
	}{
		{
			name:     "valid string note",
			note:     "hello world",
			want:     "hello world",
			hasValue: true,
		},
		{
			name:     "string note with whitespace",
			note:     "  trimmed note  ",
			want:     "trimmed note",
			hasValue: true,
		},
		{
			name:     "empty string note",
			note:     "",
			hasValue: false,
		},
		{
			name:     "whitespace only note",
			note:     "   ",
			hasValue: false,
		},
		{
			name:     "non-string note ignored",
			note:     12345,
			hasValue: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tempDir := t.TempDir()
			authData := map[string]any{
				"type": "claude",
				"note": tt.note,
			}
			data, _ := json.Marshal(authData)
			errWriteFile := os.WriteFile(filepath.Join(tempDir, "auth.json"), data, 0644)
			if errWriteFile != nil {
				t.Fatalf("failed to write auth file: %v", errWriteFile)
			}

			synth := NewFileSynthesizer()
			ctx := &SynthesisContext{
				Config:      &config.Config{},
				AuthDir:     tempDir,
				Now:         time.Now(),
				IDGenerator: NewStableIDGenerator(),
			}

			auths, errSynthesize := synth.Synthesize(ctx)
			if errSynthesize != nil {
				t.Fatalf("unexpected error: %v", errSynthesize)
			}
			if len(auths) != 1 {
				t.Fatalf("expected 1 auth, got %d", len(auths))
			}

			value, ok := auths[0].Attributes["note"]
			if tt.hasValue {
				if !ok {
					t.Fatal("expected note attribute to be set")
				}
				if value != tt.want {
					t.Fatalf("expected note %q, got %q", tt.want, value)
				}
				return
			}
			if ok {
				t.Fatalf("expected note attribute to be absent, got %q", value)
			}
		})
	}
}
func TestSynthesizeAuthFileExpandsTrustedGeminiCLIPluginAuth(t *testing.T) {
	tempDir := t.TempDir()

	authData := map[string]any{
		"type":       "gemini-cli",
		"email":      "multi@example.com",
		"project_id": "project-a, project-b",
		"priority":   5,
		"note":       "production keys",
	}
	data, _ := json.Marshal(authData)
	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tempDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
		PluginAuthParser: multiAuthParserFunc(func(context.Context, pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
			return []*coreauth.Auth{{
				ID:       "gemini-cli.json",
				Provider: "gemini-cli",
				Label:    "multi@example.com",
			}}, true, nil
		}),
	}

	auths := SynthesizeAuthFile(ctx, filepath.Join(tempDir, "gemini-cli.json"), data)
	if len(auths) != 3 {
		t.Fatalf("expected 3 auths (1 primary + 2 virtuals), got %d", len(auths))
	}

	primary := auths[0]
	if gotNote := primary.Attributes["note"]; gotNote != "production keys" {
		t.Errorf("expected primary note %q, got %q", "production keys", gotNote)
	}

	// Verify virtuals inherit note
	for i := 1; i < len(auths); i++ {
		v := auths[i]
		if gotNote := v.Attributes["note"]; gotNote != "production keys" {
			t.Errorf("expected virtual %d note %q, got %q", i, "production keys", gotNote)
		}
		if gotPriority := v.Attributes["priority"]; gotPriority != "5" {
			t.Errorf("expected virtual %d priority %q, got %q", i, "5", gotPriority)
		}
	}
}

// TestSynthesizeFileAuths_QoderPreservesModelConfigs verifies that hot-reloading
// a qoder auth file does not drop the cached model_configs map written by
// QoderTokenStorage.SaveTokenToFile. Without it, buildQoderModelConfig fails
// with "model config cache is empty" until /algo/api/v2/model/list returns,
// even when the disk copy already has the answer.
func TestSynthesizeFileAuths_QoderPreservesModelConfigs(t *testing.T) {
	tmpDir := t.TempDir()
	authPath := filepath.Join(tmpDir, "qoder-test@example.com.json")

	storage := &qoderauth.QoderTokenStorage{
		Token:        "tok",
		RefreshToken: "rtok",
		UserID:       "u-123",
		Name:         "Test",
		Email:        "test@example.com",
		ExpireTime:   1234567890,
		Type:         "qoder",
		MachineID:    "m-1",
		ModelConfigs: map[string]json.RawMessage{
			"dfmodel": json.RawMessage(`{"key":"dfmodel","format":"openai","is_vl":true,"max_input_tokens":131072}`),
		},
	}
	if err := storage.SaveTokenToFile(authPath); err != nil {
		t.Fatalf("SaveTokenToFile: %v", err)
	}

	data, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tmpDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}
	auths := SynthesizeAuthFile(ctx, authPath, data)
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}

	a := auths[0]
	if a.Provider != "qoder" {
		t.Fatalf("expected provider qoder, got %q", a.Provider)
	}
	storedAny := a.Storage
	stored, ok := storedAny.(*qoderauth.QoderTokenStorage)
	if !ok {
		t.Fatalf("expected *QoderTokenStorage, got %T", storedAny)
	}
	if stored.Email != "test@example.com" {
		t.Errorf("expected email preserved, got %q", stored.Email)
	}
	raw, ok := stored.GetModelConfig("dfmodel")
	if !ok {
		t.Fatalf("expected cached model_configs entry to survive reload, keys=%v", stored.ModelConfigKeys())
	}
	if !strings.Contains(string(raw), `"is_vl":true`) {
		t.Errorf("expected raw model_config to contain is_vl, got %s", string(raw))
	}
}

// TestSynthesizeFileAuths_QoderHandlesMissingModelConfigs ensures that auth
// files written by older builds (no model_configs key) still synthesize
// without error — the cache simply starts empty and FetchQoderModels will
// repopulate it.
func TestSynthesizeFileAuths_QoderHandlesMissingModelConfigs(t *testing.T) {
	tmpDir := t.TempDir()
	authPath := filepath.Join(tmpDir, "qoder-old@example.com.json")
	payload := map[string]any{
		"type":          "qoder",
		"email":         "old@example.com",
		"token":         "tok",
		"refresh_token": "rtok",
		"user_id":       "u-1",
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(authPath, data, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	ctx := &SynthesisContext{
		Config:      &config.Config{},
		AuthDir:     tmpDir,
		Now:         time.Now(),
		IDGenerator: NewStableIDGenerator(),
	}
	auths := SynthesizeAuthFile(ctx, authPath, data)
	if len(auths) != 1 {
		t.Fatalf("expected 1 auth, got %d", len(auths))
	}
	stored, ok := auths[0].Storage.(*qoderauth.QoderTokenStorage)
	if !ok {
		t.Fatalf("expected *QoderTokenStorage, got %T", auths[0].Storage)
	}
	if stored.Email != "old@example.com" {
		t.Errorf("expected email preserved, got %q", stored.Email)
	}
	if len(stored.ModelConfigKeys()) != 0 {
		t.Errorf("expected empty cache, got %v", stored.ModelConfigKeys())
	}
}
