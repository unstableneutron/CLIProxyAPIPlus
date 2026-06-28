package responsesreplay

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestRender_PortableTranscript_preserves_compaction_encrypted_content_when_stripping_provider_state(t *testing.T) {
	// Given
	payload := []byte(`{
		"previous_response_id":"resp_chatgpt",
		"include":["reasoning.encrypted_content","web_search_call.results"],
		"input":[
			{"type":"reasoning","id":"rs_1","encrypted_content":"rsn_chatgpt","signature":"sig_chatgpt","summary":[{"type":"summary_text","text":"kept"}]},
			{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"kept text"}]},
			{"type":"function_call","id":"fc_1","call_id":"call_1","name":"tool","arguments":"{}"},
			{"type":"function_call_output","id":"fco_1","call_id":"call_1","output":"kept output"},
			{"type":"compaction","id":"cmp_1","encrypted_content":"kept compaction"},
			{"type":"compaction_summary","id":"cmps_1","encrypted_content":"kept summary compaction","summary":"compressed"}
		]
	}`)

	// When
	got, changed := Render(payload, AttemptPortableTranscript)

	// Then
	if !changed {
		t.Fatalf("Render changed = false, want true")
	}
	for _, path := range []string{
		"previous_response_id",
		"input.0.id",
		"input.0.encrypted_content",
		"input.0.signature",
		"input.1.id",
		"input.2.id",
		"input.3.id",
	} {
		if gjson.GetBytes(got, path).Exists() {
			t.Fatalf("portable transcript kept %s: %s", path, got)
		}
	}
	if strings.Contains(string(got), "reasoning.encrypted_content") {
		t.Fatalf("portable transcript kept reasoning encrypted include: %s", got)
	}
	if gotValue := gjson.GetBytes(got, "input.4.encrypted_content").String(); gotValue != "kept compaction" {
		t.Fatalf("compaction encrypted_content = %q, want preserved: %s", gotValue, got)
	}
	if gotValue := gjson.GetBytes(got, "input.5.encrypted_content").String(); gotValue != "kept summary compaction" {
		t.Fatalf("compaction_summary encrypted_content = %q, want preserved: %s", gotValue, got)
	}
	if gotValue := gjson.GetBytes(got, "input.2.call_id").String(); gotValue != "call_1" {
		t.Fatalf("function_call call_id = %q, want preserved: %s", gotValue, got)
	}
	if gotValue := gjson.GetBytes(got, "input.3.call_id").String(); gotValue != "call_1" {
		t.Fatalf("function_call_output call_id = %q, want preserved: %s", gotValue, got)
	}
	if gotValue := gjson.GetBytes(got, "input.0.summary.0.text").String(); gotValue != "kept" {
		t.Fatalf("reasoning summary = %q, want preserved: %s", gotValue, got)
	}
}

func TestRender_WithoutProviderIdentifiers_keeps_reasoning_encrypted_content(t *testing.T) {
	// Given
	payload := []byte(`{
		"previous_response_id":"resp_chatgpt",
		"include":["reasoning.encrypted_content"],
		"input":[{"type":"reasoning","id":"rs_1","encrypted_content":"rsn_chatgpt"}]
	}`)

	// When
	got, changed := Render(payload, AttemptWithoutProviderIdentifiers)

	// Then
	if !changed {
		t.Fatalf("Render changed = false, want true")
	}
	if gjson.GetBytes(got, "previous_response_id").Exists() {
		t.Fatalf("identifier retry kept previous_response_id: %s", got)
	}
	if gjson.GetBytes(got, "input.0.id").Exists() {
		t.Fatalf("identifier retry kept input item id: %s", got)
	}
	if gotValue := gjson.GetBytes(got, "input.0.encrypted_content").String(); gotValue != "rsn_chatgpt" {
		t.Fatalf("identifier retry encrypted_content = %q, want preserved: %s", gotValue, got)
	}
	if gotValue := gjson.GetBytes(got, "include.0").String(); gotValue != "reasoning.encrypted_content" {
		t.Fatalf("identifier retry include.0 = %q, want reasoning.encrypted_content: %s", gotValue, got)
	}
}

func TestNextAttempt_moves_to_portable_transcript_after_second_provider_state_failure(t *testing.T) {
	// Given / When
	first, okFirst := NextAttempt(AttemptOriginal, ErrorProviderStateNotFound)
	second, okSecond := NextAttempt(first, ErrorProviderStateNotFound)

	// Then
	if !okFirst || first != AttemptWithoutProviderIdentifiers {
		t.Fatalf("first retry = %s ok=%v, want %s", first, okFirst, AttemptWithoutProviderIdentifiers)
	}
	if !okSecond || second != AttemptPortableTranscript {
		t.Fatalf("second retry = %s ok=%v, want %s", second, okSecond, AttemptPortableTranscript)
	}
}

func TestClassify_does_not_retry_missing_compaction_encrypted_content(t *testing.T) {
	// Given
	errText := `{"error":{"message":"[ObjectParam] [input[8].encrypted_content] [missing_required_parameter] Missing required parameter: 'input[8].encrypted_content'.","type":"invalid_request_error","code":"missing_required_parameter"}}`

	// When
	got := Classify(http.StatusBadRequest, errText)

	// Then
	if got != ErrorNone {
		t.Fatalf("Classify = %s, want %s", got, ErrorNone)
	}
}

func TestClassify_uses_structured_param_for_provider_state(t *testing.T) {
	// Given
	errText := `{"error":{"message":"input item not found","type":"invalid_request_error","code":"item_not_found","param":"input[2].id"}}`

	// When
	got := Classify(http.StatusBadRequest, errText)

	// Then
	if got != ErrorProviderStateNotFound {
		t.Fatalf("Classify = %s, want %s", got, ErrorProviderStateNotFound)
	}
}
