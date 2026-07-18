package responses

import (
	"context"
	"strings"
	"testing"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func parseKiroResponsesSSEEvent(t *testing.T, chunk []byte) (string, gjson.Result) {
	t.Helper()

	var event string
	var data string
	for _, line := range strings.Split(string(chunk), "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		}
	}
	if data == "" {
		t.Fatalf("SSE chunk has no data line: %s", string(chunk))
	}
	return event, gjson.Parse(data)
}

func TestKiroResponsesTranslatorRegistrationAndRequestDelegation(t *testing.T) {
	kiroFormat := sdktranslator.FromString("kiro")
	if !sdktranslator.HasRequestTransformer(sdktranslator.FormatOpenAIResponse, kiroFormat) {
		t.Fatal("OpenAI Responses to Kiro request transformer is not registered")
	}
	if !sdktranslator.HasStreamResponseTransformer(sdktranslator.FormatOpenAIResponse, kiroFormat) {
		t.Fatal("Kiro to OpenAI Responses stream transformer is not registered")
	}
	if !sdktranslator.HasNonStreamResponseTransformer(sdktranslator.FormatOpenAIResponse, kiroFormat) {
		t.Fatal("Kiro to OpenAI Responses non-stream transformer is not registered")
	}

	raw := []byte(`{
		"model":"client-model",
		"max_output_tokens":128,
		"input":[
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"Run pwd"}]
			}
		]
	}`)
	out := sdktranslator.TranslateRequest(
		sdktranslator.FormatOpenAIResponse,
		kiroFormat,
		"kiro-claude-sonnet-4-6",
		raw,
		true,
	)
	root := gjson.ParseBytes(out)

	if got := root.Get("model").String(); got != "kiro-claude-sonnet-4-6" {
		t.Fatalf("model = %q, want kiro-claude-sonnet-4-6. Output: %s", got, string(out))
	}
	if got := root.Get("max_tokens").Int(); got != 128 {
		t.Fatalf("max_tokens = %d, want 128. Output: %s", got, string(out))
	}
	if !root.Get("stream").Bool() {
		t.Fatalf("stream = false, want true. Output: %s", string(out))
	}
	if got := root.Get("messages.0.role").String(); got != "user" {
		t.Fatalf("message role = %q, want user. Output: %s", got, string(out))
	}
	if got := root.Get("messages.0.content").String(); got != "Run pwd" {
		t.Fatalf("message content = %q, want Run pwd. Output: %s", got, string(out))
	}
	if root.Get("input").Exists() {
		t.Fatalf("OpenAI Responses input leaked into Claude request. Output: %s", string(out))
	}
}

func TestKiroResponsesStreamFinalizesMessageBeforeFunctionCall(t *testing.T) {
	chunks := [][]byte{
		[]byte(`data: {"type":"message_start","message":{"id":"msg_123","usage":{"input_tokens":1,"output_tokens":0}}}`),
		[]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		[]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Checking the workspace."}}`),
		[]byte(`data: {"type":"content_block_stop","index":0}`),
		[]byte(`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_123","name":"exec_command","input":{}}}`),
		[]byte(`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"pwd\"}"}}`),
		[]byte(`data: {"type":"content_block_stop","index":1}`),
		[]byte(`data: {"type":"message_stop"}`),
	}

	var param any
	var outputs [][]byte
	for _, chunk := range chunks {
		outputs = append(outputs, sdktranslator.TranslateStream(
			context.Background(),
			sdktranslator.FromString("kiro"),
			sdktranslator.FormatOpenAIResponse,
			"kiro-claude-sonnet-4-6",
			nil,
			nil,
			chunk,
			&param,
		)...)
	}

	messageDonePosition := -1
	functionAddedPosition := -1
	messageDoneCount := 0
	functionDoneCount := 0
	var completed gjson.Result
	for position, output := range outputs {
		event, data := parseKiroResponsesSSEEvent(t, output)
		itemType := data.Get("item.type").String()
		switch {
		case event == "response.output_item.done" && itemType == "message":
			messageDonePosition = position
			messageDoneCount++
			if got := data.Get("output_index").Int(); got != 0 {
				t.Fatalf("message done output_index = %d, want 0", got)
			}
		case event == "response.output_item.added" && itemType == "function_call":
			functionAddedPosition = position
			if got := data.Get("output_index").Int(); got != 1 {
				t.Fatalf("function added output_index = %d, want 1", got)
			}
		case event == "response.output_item.done" && itemType == "function_call":
			functionDoneCount++
		case event == "response.completed":
			completed = data
		}
	}

	if messageDonePosition < 0 || functionAddedPosition < 0 {
		t.Fatalf("missing lifecycle event: message done=%d, function added=%d", messageDonePosition, functionAddedPosition)
	}
	if messageDonePosition >= functionAddedPosition {
		t.Fatalf("message done position = %d, want before function added position %d", messageDonePosition, functionAddedPosition)
	}
	if messageDoneCount != 1 || functionDoneCount != 1 {
		t.Fatalf("output_item.done counts: message=%d function=%d, want 1 each", messageDoneCount, functionDoneCount)
	}
	if got := completed.Get("response.output.0.type").String(); got != "message" {
		t.Fatalf("completed output[0] type = %q, want message", got)
	}
	if got := completed.Get("response.output.1.type").String(); got != "function_call" {
		t.Fatalf("completed output[1] type = %q, want function_call", got)
	}
}
