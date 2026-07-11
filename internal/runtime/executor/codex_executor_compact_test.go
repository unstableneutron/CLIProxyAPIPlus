package executor

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorCompactAddsDefaultInstructions(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{
			name:    "missing instructions",
			payload: `{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"history"},{"type":"compaction_trigger"}]}`,
		},
		{
			name:    "null instructions",
			payload: `{"model":"gpt-5.4","instructions":null,"input":[{"type":"message","role":"user","content":"history"},{"type":"compaction_trigger"}]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				body, _ := io.ReadAll(r.Body)
				gotBody = body
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
			}))
			defer server.Close()

			executor := NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url": server.URL,
				"api_key":  "test",
			}}

			resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "gpt-5.4",
				Payload: []byte(tc.payload),
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("openai-response"),
				Alt:          "responses/compact",
				Stream:       false,
			})
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if gotPath != "/responses/compact" {
				t.Fatalf("path = %q, want %q", gotPath, "/responses/compact")
			}
			if instructions := gjson.GetBytes(gotBody, "instructions"); instructions.Type != gjson.String || instructions.String() != "" {
				t.Fatalf("instructions = %s, want empty string; body=%s", instructions.Raw, gotBody)
			}
			if gjson.GetBytes(gotBody, "tools").Exists() {
				t.Fatalf("compact request injected image_generation tool: %s", gotBody)
			}
			input := gjson.GetBytes(gotBody, "input").Array()
			if len(input) != 2 || input[1].Get("type").String() != "compaction_trigger" {
				t.Fatalf("compact input order changed: %s", gotBody)
			}
			if string(resp.Payload) != `{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}` {
				t.Fatalf("payload = %s", string(resp.Payload))
			}
		})
	}
}

func TestCodexExecutorExecuteStreamCompactionTriggerUsesCompactEndpoint(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
  "id": "resp_compact_1",
  "object": "response.compaction",
  "created_at": 1775555723,
  "status": "completed",
  "output": [
    {"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},
    {"type":"compaction","encrypted_content":"opaque"}
  ],
  "usage": {"input_tokens": 1, "output_tokens": 2, "total_tokens": 3}
}`))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model: "gpt-5.5-aws",
		Payload: []byte(`{
  "model":"gpt-5.5-aws",
  "previous_response_id":"resp-prev",
  "stream":true,
  "store":false,
  "include":["reasoning.encrypted_content"],
  "tools":[],
  "tool_choice":"auto",
  "text":{"verbosity":"low"},
  "client_metadata":{"x":"y"},
  "input":[
    {"id":"msg-user","role":"user","content":[{"type":"input_text","text":"hello"}]},
    {"id":"rs-prev","type":"reasoning","summary":[]},
    {"id":"msg-prev","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello"}]},
    {"type":"compaction_trigger"}
  ]
}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Stream:       true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream compaction trigger error: %v", err)
	}
	if gotPath != "/responses/compact" {
		t.Fatalf("path = %q, want /responses/compact", gotPath)
	}
	if xaiInputHasItemType(gotBody, "compaction_trigger") {
		t.Fatalf("compaction_trigger reached compact body: %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id reached compact body: %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "stream").Exists() {
		t.Fatalf("stream reached compact body: %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "stream_options").Exists() {
		t.Fatalf("stream_options reached compact body: %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "store").Exists() {
		t.Fatalf("store reached compact body: %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "include").Exists() {
		t.Fatalf("include reached compact body: %s", string(gotBody))
	}
	for _, field := range []string{"tool_choice", "text", "client_metadata"} {
		if gjson.GetBytes(gotBody, field).Exists() {
			t.Fatalf("%s reached compact body: %s", field, string(gotBody))
		}
	}
	if got := len(gjson.GetBytes(gotBody, "input").Array()); got != 3 {
		t.Fatalf("compact input length = %d, want 3; body=%s", got, string(gotBody))
	}
	if bytes.Contains(gotBody, []byte(`"id":`)) {
		t.Fatalf("compact body should strip non-portable item ids: %s", string(gotBody))
	}

	var streamed bytes.Buffer
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error = %v", chunk.Err)
		}
		streamed.Write(chunk.Payload)
	}
	if !bytes.Contains(streamed.Bytes(), []byte("response.completed")) {
		t.Fatalf("compact trigger stream missing response.completed: %s", streamed.String())
	}
}

func TestCodexCompactShouldRetryWithoutEncryptedContent(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "unknown encrypted content parameter",
			err:  statusErr{code: http.StatusBadRequest, msg: `{"error":{"message":"Unknown parameter: 'input[1].encrypted_content'."}}`},
			want: true,
		},
		{
			name: "missing required encrypted content",
			err:  statusErr{code: http.StatusBadRequest, msg: `{"error":{"message":"Missing required parameter: 'input[1].encrypted_content'."}}`},
			want: false,
		},
		{
			name: "auth error mentioning encrypted content",
			err:  statusErr{code: http.StatusUnauthorized, msg: `{"error":{"message":"invalid encrypted_content"}}`},
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := codexCompactShouldRetryWithoutEncryptedContent(tc.err); got != tc.want {
				t.Fatalf("codexCompactShouldRetryWithoutEncryptedContent() = %v, want %v", got, tc.want)
			}
		})
	}
}
