package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexCompactRequestValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload string
		valid   bool
	}{
		{"object", `{}`, true},
		{"null", `null`, false},
		{"array", `[]`, false},
		{"trailing", `{} {}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := codexCompactDecodeJSONObject([]byte(tc.payload))
			if (err == nil) != tc.valid {
				t.Fatalf("error = %v, valid = %v", err, tc.valid)
			}
		})
	}
	for _, raw := range []any{nil, "hello", []any{}} {
		if _, err := codexCompactResponsesInput(raw); err != nil {
			t.Fatalf("valid input %T rejected: %v", raw, err)
		}
	}
	if _, err := codexCompactResponsesInput(map[string]any{}); err == nil {
		t.Fatal("object input was accepted")
	}
	for _, request := range []map[string]any{{}, {"model": 7}, {"model": "gpt-5.4", "input": map[string]any{}}, {"model": "gpt-5.4", "stream": "false"}, {"model": "gpt-5.4", "input": []any{"text"}}} {
		if err := codexCompactValidateRequest(request); err == nil {
			t.Fatalf("malformed request accepted: %#v", request)
		}
	}
	if statusErr, ok := codexCompactInvalidRequestErr(errors.New("bad input")).(interface{ StatusCode() int }); !ok || statusErr.StatusCode() != http.StatusBadRequest {
		t.Fatalf("invalid request error = %#v, want HTTP 400", statusErr)
	}
}

func TestBuildCodexCompactEnvelopeValidation(t *testing.T) {
	valid := map[string]any{
		"id": "resp_1", "created_at": json.Number("1"), "status": "completed",
		"output": []any{map[string]any{"type": "compaction", "encrypted_content": "SENTINEL_SECRET"}},
		"usage":  map[string]any{"input_tokens": json.Number("1"), "output_tokens": json.Number("2"), "total_tokens": json.Number("3")},
	}
	mutations := []struct {
		name string
		fn   func(map[string]any)
	}{
		{"no compaction", func(v map[string]any) { v["output"] = []any{} }},
		{"multiple compactions", func(v map[string]any) { v["output"] = append(v["output"].([]any), v["output"].([]any)[0]) }},
		{"empty encrypted content", func(v map[string]any) { v["output"].([]any)[0].(map[string]any)["encrypted_content"] = "" }},
		{"missing id", func(v map[string]any) { delete(v, "id") }},
		{"invalid created at", func(v map[string]any) { v["created_at"] = json.Number("1.5") }},
		{"missing usage", func(v map[string]any) { delete(v, "usage") }},
		{"invalid usage", func(v map[string]any) { v["usage"].(map[string]any)["total_tokens"] = "3" }},
		{"fractional usage", func(v map[string]any) { v["usage"].(map[string]any)["total_tokens"] = json.Number("3.5") }},
		{"negative usage", func(v map[string]any) { v["usage"].(map[string]any)["input_tokens"] = json.Number("-1") }},
		{"fractional usage detail", func(v map[string]any) {
			v["usage"].(map[string]any)["input_tokens_details"] = map[string]any{"cached_tokens": json.Number("0.5")}
		}},
		{"unknown invalid input usage detail", func(v map[string]any) {
			v["usage"].(map[string]any)["input_tokens_details"] = map[string]any{"future_tokens": "SENTINEL_SECRET"}
		}},
		{"unknown invalid output usage detail", func(v map[string]any) {
			v["usage"].(map[string]any)["output_tokens_details"] = map[string]any{"future_tokens": json.Number("-1")}
		}},
		{"missing status", func(v map[string]any) { delete(v, "status") }},
		{"incomplete", func(v map[string]any) { v["status"] = "incomplete" }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			encoded, _ := json.Marshal(valid)
			var value map[string]any
			decoder := json.NewDecoder(bytes.NewReader(encoded))
			decoder.UseNumber()
			_ = decoder.Decode(&value)
			tc.fn(value)
			payload, _ := json.Marshal(value)
			if _, err := buildCodexCompactEnvelope(payload, nil); err == nil {
				t.Fatal("malformed response accepted")
			}
		})
	}
}

func TestBuildCodexCompactEnvelopeSynthesizesContractFields(t *testing.T) {
	payload := []byte(`{"id":"resp_1","created_at":1,"status":"completed","output":[{"type":"compaction","encrypted_content":"opaque"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`)
	envelope, err := buildCodexCompactEnvelope(payload, []map[string]any{{"type": "message", "role": "user", "content": []any{}}})
	if err != nil {
		t.Fatalf("build envelope: %v", err)
	}
	if id := gjson.GetBytes(envelope, "output.0.id").String(); !strings.HasPrefix(id, "msg_") {
		t.Fatalf("message id = %q, want msg_ prefix", id)
	}
	for _, path := range []string{"usage.input_tokens_details.cached_tokens", "usage.input_tokens_details.cache_write_tokens", "usage.output_tokens_details.reasoning_tokens"} {
		if value := gjson.GetBytes(envelope, path); !value.Exists() || value.Int() != 0 {
			t.Fatalf("%s = %s, want integer zero", path, value.Raw)
		}
	}
}

func TestCodexCompactTruncatesOversizedBoundaryUserMessages(t *testing.T) {
	large := strings.Repeat("x", codexCompactMessageTokenBudget*4)
	for _, tc := range []struct {
		name  string
		input []any
		ids   []string
	}{
		{name: "sole oversized newest", input: []any{map[string]any{"id": "only", "role": "user", "content": large + "最新"}}, ids: []string{"only"}},
		{name: "oversized boundary", input: []any{map[string]any{"id": "older", "role": "user", "content": "drop"}, map[string]any{"id": "boundary", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": large + "末尾"}, map[string]any{"type": "input_image", "image_url": "data:image/png;base64,opaque"}}}, map[string]any{"id": "new", "role": "user", "content": "newest"}}, ids: []string{"boundary", "new"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			retained := codexCompactRetainedUserMessages(tc.input)
			if len(retained) != len(tc.ids) {
				t.Fatalf("retained = %#v", retained)
			}
			total := 0
			for index, message := range retained {
				if message["id"] != tc.ids[index] {
					t.Fatalf("retained[%d].id = %v, want %s", index, message["id"], tc.ids[index])
				}
				text := message["content"].([]any)[0].(map[string]any)["text"].(string)
				if !utf8.ValidString(text) {
					t.Fatalf("retained text is invalid UTF-8")
				}
				total += codexCompactEstimatedTokens(message)
			}
			if total > codexCompactMessageTokenBudget {
				t.Fatalf("estimated tokens = %d, budget = %d", total, codexCompactMessageTokenBudget)
			}
			if tc.name == "oversized boundary" {
				nontext := retained[0]["content"].([]any)[1].(map[string]any)
				if nontext["type"] != "input_image" || nontext["image_url"] != "data:image/png;base64,opaque" {
					t.Fatalf("nontext content changed: %#v", nontext)
				}
			}
			latest := retained[len(retained)-1]["content"].([]any)[0].(map[string]any)["text"].(string)
			if !strings.HasSuffix(latest, map[bool]string{true: "最新", false: "newest"}[tc.name == "sole oversized newest"]) {
				t.Fatalf("latest text not preserved: %q", latest)
			}
		})
	}
}

func TestCodexExecutorCompactAddsDefaultInstructions(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{
			name:    "missing instructions",
			payload: `{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"legacy history"}]}`,
		},
		{
			name:    "null instructions",
			payload: `{"model":"gpt-5.4","instructions":null,"input":[{"type":"message","role":"user","content":"legacy history"}]}`,
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
			if len(input) != 1 || input[0].Get("content").String() != "legacy history" {
				t.Fatalf("compact history changed: %s", gotBody)
			}
			if string(resp.Payload) != `{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}` {
				t.Fatalf("payload = %s", string(resp.Payload))
			}
		})
	}
}

func TestCodexExecutorAPIKeyCompactPreservesPreviousResponseID(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","previous_response_id":"resp_prev","input":"hello"}`),
	}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Alt: "responses/compact"})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/responses/compact" {
		t.Fatalf("path = %q, want /responses/compact", gotPath)
	}
	if got := gjson.GetBytes(gotBody, "previous_response_id").String(); got != "resp_prev" {
		t.Fatalf("previous_response_id = %q, want resp_prev; body=%s", got, gotBody)
	}
}

func TestCodexExecutorOAuthCompactRejectsPreviousResponseID(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
	}{
		{name: "nonempty", value: `"resp_prev"`},
		{name: "empty", value: `""`},
		{name: "null", value: `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstreamCalls := 0
			server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { upstreamCalls++ }))
			defer server.Close()

			executor := NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "auth_kind": "oauth"}}
			_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "gpt-5.4",
				Payload: []byte(`{"model":"gpt-5.4","previous_response_id":` + tc.value + `,"input":"hello"}`),
			}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Alt: "responses/compact"})
			statusErr, ok := err.(interface{ StatusCode() int })
			if !ok || statusErr.StatusCode() != http.StatusBadRequest {
				t.Fatalf("error = %#v, want HTTP 400", err)
			}
			if err.Error() != codexCompactUnsupportedStatefulError {
				t.Fatalf("error = %s, want stable unsupported-stateful error", err)
			}
			if gjson.Get(err.Error(), "error.type").String() != "invalid_request_error" || gjson.Get(err.Error(), "error.code").String() != "unsupported_stateful_compact" {
				t.Fatalf("unexpected error contract: %s", err)
			}
			if upstreamCalls != 0 {
				t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
			}
		})
	}
}

func TestCodexExecutorOAuthCompactUsesResponsesAdapter(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"opaque\",\"created_by\":\"server\"}}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"created_at\":1700000000,\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"conflict\"}]}],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n"))
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{Payload: config.PayloadConfig{OverrideRaw: []config.PayloadRule{{
		Models: []config.PayloadModelRule{{Name: "gpt-5.4", Protocol: "codex", FromProtocol: "openai-response"}},
		Params: map[string]any{
			"stream":  "false",
			"store":   "true",
			"include": `["SENTINEL_SECRET"]`,
			"input":   `[{"type":"compaction_trigger","extra":"bad"},{"type":"compaction_trigger"}]`,
			"tools":   `[{"type":"function","name":"keep"},{"type":"image_generation"}]`,
		},
	}}}})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "auth_kind": "oauth"}}
	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"secret"},{"type":"compaction_trigger"}]}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Alt: "responses/compact"})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if gotPath != "/responses" {
		t.Fatalf("path = %q, want /responses", gotPath)
	}
	if !gjson.GetBytes(gotBody, "stream").Bool() || gjson.GetBytes(gotBody, "store").Bool() {
		t.Fatalf("required responses fields missing: %s", gotBody)
	}
	input := gjson.GetBytes(gotBody, "input").Array()
	if len(input) != 1 || input[0].Get("type").String() != "compaction_trigger" || len(input[0].Map()) != 1 {
		t.Fatalf("input must end in exactly one trigger: %s", gotBody)
	}
	if include := gjson.GetBytes(gotBody, "include").Array(); len(include) != 1 || include[0].String() != "reasoning.encrypted_content" {
		t.Fatalf("include invariant missing: %s", gotBody)
	}
	tools := gjson.GetBytes(gotBody, "tools").Array()
	if len(tools) != 1 || tools[0].Get("type").String() != "function" {
		t.Fatalf("image tool was not selectively removed: %s", gotBody)
	}
	if got := gjson.GetBytes(resp.Payload, "object").String(); got != "response.compaction" {
		t.Fatalf("object = %q; payload=%s", got, resp.Payload)
	}
	output := gjson.GetBytes(resp.Payload, "output").Array()
	if len(output) != 2 || output[0].Get("role").String() != "user" || output[1].Get("type").String() != "compaction" {
		t.Fatalf("unexpected compact output: %s", resp.Payload)
	}
	if resp.Headers.Get("Content-Type") != "application/json" {
		t.Fatalf("content-type = %q", resp.Headers.Get("Content-Type"))
	}
}

func TestCodexExecutorOAuthCompactSanitizesProtocolFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "empty eof"},
		{name: "truncated terminal", body: `data: {"type":"response.completed","response":{"secret":"SENTINEL_SECRET"}`},
		{name: "response incomplete", body: "data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"SENTINEL_SECRET\",\"status\":\"incomplete\"}}\n\n"},
		{name: "malformed completed", body: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"SENTINEL_SECRET\",\"status\":\"completed\"}}\n\n"},
		{name: "terminal only compaction", body: "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"created_at\":1,\"status\":\"completed\",\"output\":[{\"type\":\"compaction\",\"encrypted_content\":\"SENTINEL_SECRET\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n"},
		{name: "duplicate done output index", body: "data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"SENTINEL_SECRET\"}}\n\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"compaction\",\"encrypted_content\":\"other\"}}\n\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"created_at\":1,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			executor := NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{"base_url": server.URL, "auth_kind": "oauth"}}
			_, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{Model: "gpt-5.4", Payload: []byte(`{"model":"gpt-5.4"}`)}, cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("openai-response"), Alt: "responses/compact"})
			status, ok := err.(interface{ StatusCode() int })
			if !ok || status.StatusCode() != http.StatusBadGateway {
				t.Fatalf("error = %#v, want HTTP 502", err)
			}
			if strings.Contains(err.Error(), "SENTINEL_SECRET") || err.Error() != `{"error":{"message":"invalid compaction response from upstream","type":"upstream_protocol_error"}}` {
				t.Fatalf("unsanitized protocol error: %v", err)
			}
		})
	}
}

func TestCodexExecutorExecuteStreamCompactionTriggerUsesResponsesEndpoint(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			t.Fatalf("read body: %v", errRead)
		}
		gotBody = body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_compact_1\",\"status\":\"completed\",\"output\":[{\"type\":\"compaction\",\"encrypted_content\":\"opaque\"}],\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n"))
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
	if gotPath != "/responses" {
		t.Fatalf("path = %q, want /responses", gotPath)
	}
	if !xaiInputHasItemType(gotBody, "compaction_trigger") {
		t.Fatalf("compaction_trigger missing from responses body: %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "previous_response_id").Exists() {
		t.Fatalf("previous_response_id reached compact body: %s", string(gotBody))
	}
	if !gjson.GetBytes(gotBody, "stream").Bool() {
		t.Fatalf("stream = false, want true: %s", string(gotBody))
	}
	if gjson.GetBytes(gotBody, "stream_options").Exists() {
		t.Fatalf("stream_options reached compact body: %s", string(gotBody))
	}
	if got := len(gjson.GetBytes(gotBody, "input").Array()); got != 4 {
		t.Fatalf("responses input length = %d, want 4; body=%s", got, string(gotBody))
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

func TestCodexWebsocketsExecuteStreamCompactionTriggerUsesResponsesWebsocket(t *testing.T) {
	var gotPath string
	var gotBody []byte
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		conn, errUpgrade := upgrader.Upgrade(w, r, nil)
		if errUpgrade != nil {
			t.Errorf("upgrade websocket: %v", errUpgrade)
			return
		}
		defer func() { _ = conn.Close() }()
		_, body, errRead := conn.ReadMessage()
		if errRead != nil {
			t.Errorf("read websocket request: %v", errRead)
			return
		}
		gotBody = bytes.Clone(body)
		completed := []byte(`{"type":"response.completed","response":{"id":"resp_compact_1","status":"completed","output":[{"type":"compaction","encrypted_content":"opaque"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`)
		if errWrite := conn.WriteMessage(websocket.TextMessage, completed); errWrite != nil {
			t.Errorf("write websocket response: %v", errWrite)
		}
	}))
	defer server.Close()

	executor := NewCodexWebsocketsExecutor(&config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: config.DisableImageGenerationAll}})
	auth := &cliproxyauth.Auth{ID: "auth-compaction", Attributes: map[string]string{"base_url": server.URL, "api_key": "test"}}
	result, err := executor.ExecuteStream(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.5-aws",
		Payload: []byte(`{"model":"gpt-5.5-aws","stream":true,"input":[{"role":"user","content":"hello"},{"type":"compaction_trigger"}]}`),
	}, cliproxyexecutor.Options{
		SourceFormat:   sdktranslator.FromString("openai-response"),
		ResponseFormat: sdktranslator.FromString("openai-response"),
		Stream:         true,
	})
	if err != nil {
		t.Fatalf("ExecuteStream compaction trigger error: %v", err)
	}
	for chunk := range result.Chunks {
		if chunk.Err != nil {
			t.Fatalf("stream chunk error: %v", chunk.Err)
		}
	}
	if gotPath != "/responses" {
		t.Fatalf("path = %q, want /responses", gotPath)
	}
	if gjson.GetBytes(gotBody, "type").String() != "response.create" {
		t.Fatalf("websocket message type = %q, want response.create; body=%s", gjson.GetBytes(gotBody, "type").String(), gotBody)
	}
	if !xaiInputHasItemType(gotBody, "compaction_trigger") {
		t.Fatalf("compaction_trigger missing from websocket responses body: %s", gotBody)
	}
}
