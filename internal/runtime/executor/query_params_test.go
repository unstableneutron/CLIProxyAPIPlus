package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func TestCodexExecutorAppliesConfiguredQueryParams(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	defer server.Close()

	exec := NewCodexExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"api_key":              "sk-test",
		"base_url":             server.URL,
		"query:api-version":    "preview",
		"query:deployment":     "gpt-5.4-nomoderation",
		"query:existing_param": "override",
	}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4-nomoderation",
		Payload: []byte(`{"model":"gpt-5.4-nomoderation","input":[{"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`),
	}

	_, err := exec.Execute(context.Background(), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("codex")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	values, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatalf("parse query %q: %v", gotQuery, err)
	}
	if got := values.Get("api-version"); got != "preview" {
		t.Fatalf("api-version = %q, want preview", got)
	}
	if got := values.Get("deployment"); got != "gpt-5.4-nomoderation" {
		t.Fatalf("deployment = %q, want gpt-5.4-nomoderation", got)
	}
	if got := values.Get("existing_param"); got != "override" {
		t.Fatalf("existing_param = %q, want override", got)
	}
}

func TestOpenAICompatExecutorAppliesConfiguredQueryParams(t *testing.T) {
	var gotQuery string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	exec := NewOpenAICompatExecutor("openai-compatibility", &config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url":          server.URL + "/v1",
		"api_key":           "test",
		"query:api-version": "preview",
		"query:deployment":  "gpt-5.4-nomoderation",
		"query:existing":    "new",
	}}
	req := cliproxyexecutor.Request{
		Model:   "gpt-5.4-nomoderation",
		Payload: []byte(`{"model":"gpt-5.4-nomoderation","messages":[{"role":"user","content":"hi"}]}`),
	}

	_, err := exec.Execute(context.Background(), auth, req, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai")})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	values, err := url.ParseQuery(gotQuery)
	if err != nil {
		t.Fatalf("parse query %q: %v", gotQuery, err)
	}
	if got := values.Get("api-version"); got != "preview" {
		t.Fatalf("api-version = %q, want preview", got)
	}
	if got := values.Get("deployment"); got != "gpt-5.4-nomoderation" {
		t.Fatalf("deployment = %q, want gpt-5.4-nomoderation", got)
	}
	if got := values.Get("existing"); got != "new" {
		t.Fatalf("existing = %q, want new", got)
	}
}
