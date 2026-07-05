package executor

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

func TestCodexContinueTruncationPattern(t *testing.T) {
	tests := []struct {
		name   string
		tokens *int64
		want   bool
	}{
		{name: "nil", tokens: nil, want: false},
		{name: "zero", tokens: int64Ptr(0), want: false},
		{name: "515", tokens: int64Ptr(515), want: false},
		{name: "516", tokens: int64Ptr(516), want: true},
		{name: "517", tokens: int64Ptr(517), want: false},
		{name: "1034", tokens: int64Ptr(1034), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := codexContinueIsTruncationPattern(tt.tokens, 518); got != tt.want {
				t.Fatalf("codexContinueIsTruncationPattern() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCodexContinueBuildContinuationRewindAndReplay(t *testing.T) {
	fold := newTestCodexContinueFold()
	fold.replayTail = []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"enc"}`),
		json.RawMessage(codexContinueCommentaryMessage("Continue thinking...")),
	}

	base := []byte(`{"previous_response_id":"resp_prev","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"include":["foo"]}`)
	rewind, strategy, err := fold.BuildContinuation(base, nil, true, false)
	if err != nil {
		t.Fatalf("BuildContinuation rewind: %v", err)
	}
	if strategy != codexContinueDefaultMethod {
		t.Fatalf("strategy = %q, want %q", strategy, codexContinueDefaultMethod)
	}
	if got := gjson.GetBytes(rewind, "previous_response_id").String(); got != "resp_prev" {
		t.Fatalf("rewind previous_response_id = %q", got)
	}
	if gjson.GetBytes(rewind, "input.1.id").Exists() {
		t.Fatalf("rewind replay reasoning id was not stripped: %s", rewind)
	}
	if got := gjson.GetBytes(rewind, "input.2.phase").String(); got != "commentary" {
		t.Fatalf("rewind marker phase = %q", got)
	}
	if !includeHasEncrypted(rewind) {
		t.Fatalf("rewind include missing encrypted content: %s", rewind)
	}

	replay, strategy, err := fold.BuildContinuation(base, []byte(`[{"type":"message","role":"system"}]`), true, true)
	if err != nil {
		t.Fatalf("BuildContinuation replay: %v", err)
	}
	if strategy != codexContinueReplayMethod {
		t.Fatalf("strategy = %q, want %q", strategy, codexContinueReplayMethod)
	}
	if gjson.GetBytes(replay, "previous_response_id").Exists() {
		t.Fatalf("replay kept previous_response_id: %s", replay)
	}
	if got := gjson.GetBytes(replay, "input.0.role").String(); got != "system" {
		t.Fatalf("replay did not use transcript input: %s", replay)
	}
}

func TestCodexContinueFoldSingleContinuation(t *testing.T) {
	fold := newTestCodexContinueFold()
	for _, ev := range [][]byte{
		[]byte(`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","status":"in_progress"}}`),
		[]byte(`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`),
		[]byte(`{"type":"response.output_item.done","sequence_number":2,"output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc1"}}`),
		[]byte(`{"type":"response.output_item.added","sequence_number":3,"output_index":1,"item":{"type":"message","id":"msg_1"}}`),
		[]byte(`{"type":"response.output_item.done","sequence_number":4,"output_index":1,"item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"draft"}]}}`),
	} {
		result := fold.HandleEvent(ev)
		if result.Continue || result.Done {
			t.Fatalf("unexpected early terminal for %s", ev)
		}
	}
	result := fold.HandleEvent([]byte(`{"type":"response.completed","sequence_number":5,"response":{"id":"resp_1","status":"completed","usage":{"input_tokens":10,"output_tokens":520,"total_tokens":530,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":516}}}}`))
	if !result.Continue {
		t.Fatalf("terminal did not request continuation: %+v", result)
	}
	if len(result.Emit) != 0 {
		t.Fatalf("truncated terminal emitted events: %d", len(result.Emit))
	}

	for _, ev := range [][]byte{
		[]byte(`{"type":"response.created","sequence_number":0,"response":{"id":"resp_2","status":"in_progress"}}`),
		[]byte(`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"reasoning","id":"rs_2"}}`),
		[]byte(`{"type":"response.output_item.done","sequence_number":2,"output_index":0,"item":{"type":"reasoning","id":"rs_2","encrypted_content":"enc2"}}`),
		[]byte(`{"type":"response.output_item.added","sequence_number":3,"output_index":1,"item":{"type":"message","id":"msg_2"}}`),
		[]byte(`{"type":"response.output_item.done","sequence_number":4,"output_index":1,"item":{"type":"message","id":"msg_2","content":[{"type":"output_text","text":"final"}]}}`),
	} {
		fold.HandleEvent(ev)
	}
	result = fold.HandleEvent([]byte(`{"type":"response.completed","sequence_number":5,"response":{"id":"resp_2","status":"completed","usage":{"input_tokens":20,"output_tokens":20,"total_tokens":40,"output_tokens_details":{"reasoning_tokens":10}}}}`))
	if !result.Done {
		t.Fatalf("final terminal did not finish: %+v", result)
	}
	if len(result.Emit) == 0 {
		t.Fatal("final terminal emitted no events")
	}
	terminal := result.Emit[len(result.Emit)-1]
	if got := gjson.GetBytes(terminal, "response.id").String(); got != "resp_2" {
		t.Fatalf("terminal response.id = %q, want resp_2: %s", got, terminal)
	}
	if got := gjson.GetBytes(terminal, "response.usage.output_tokens_details.reasoning_tokens").Int(); got != 526 {
		t.Fatalf("reasoning tokens = %d, want 526: %s", got, terminal)
	}
	if got := gjson.GetBytes(terminal, "response.output.#").Int(); got != 3 {
		t.Fatalf("output item count = %d, want 3: %s", got, terminal)
	}
}

func TestCodexContinueFoldCapsStopWithDraft(t *testing.T) {
	fold := newTestCodexContinueFold()
	fold.cfg.MaxRounds = 0
	fold.HandleEvent([]byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"type":"reasoning"}}`))
	fold.HandleEvent([]byte(`{"type":"response.output_item.done","sequence_number":1,"output_index":0,"item":{"type":"reasoning","encrypted_content":"enc"}}`))
	fold.HandleEvent([]byte(`{"type":"response.output_item.added","sequence_number":2,"output_index":1,"item":{"type":"message"}}`))
	fold.HandleEvent([]byte(`{"type":"response.output_item.done","sequence_number":3,"output_index":1,"item":{"type":"message","content":[{"type":"output_text","text":"draft"}]}}`))
	result := fold.HandleEvent([]byte(`{"type":"response.completed","sequence_number":4,"response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":516,"total_tokens":517,"output_tokens_details":{"reasoning_tokens":516}}}}`))
	if result.Continue {
		t.Fatal("cap stop requested continuation")
	}
	terminal := result.Emit[len(result.Emit)-1]
	if got := gjson.GetBytes(terminal, "response.metadata.continue_thinking_stopped_reason").String(); got != "max_rounds" {
		t.Fatalf("stopped reason = %q, want max_rounds: %s", got, terminal)
	}
}

func TestCodexContinueFoldEOFMakesIncompleteWithoutDraft(t *testing.T) {
	fold := newTestCodexContinueFold()
	fold.HandleEvent([]byte(`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1"}}`))
	fold.HandleEvent([]byte(`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message"}}`))
	fold.HandleEvent([]byte(`{"type":"response.output_item.done","sequence_number":2,"output_index":0,"item":{"type":"message","content":[{"type":"output_text","text":"draft"}]}}`))
	events := fold.HandleEOF()
	if len(events) != 1 {
		t.Fatalf("EOF events = %d, want 1", len(events))
	}
	if got := gjson.GetBytes(events[0], "type").String(); got != "response.incomplete" {
		t.Fatalf("EOF type = %q, want response.incomplete: %s", got, events[0])
	}
	if got := gjson.GetBytes(events[0], "response.output.#").Int(); got != 0 {
		t.Fatalf("EOF flushed draft output count = %d, want 0: %s", got, events[0])
	}
}

func TestCodexContinueFallbackFlushesRetainedDraft(t *testing.T) {
	fold := newTestCodexContinueFold()
	fold.HandleEvent([]byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"type":"reasoning"}}`))
	fold.HandleEvent([]byte(`{"type":"response.output_item.done","sequence_number":1,"output_index":0,"item":{"type":"reasoning","encrypted_content":"enc"}}`))
	fold.HandleEvent([]byte(`{"type":"response.output_item.added","sequence_number":2,"output_index":1,"item":{"type":"message"}}`))
	fold.HandleEvent([]byte(`{"type":"response.output_item.done","sequence_number":3,"output_index":1,"item":{"type":"message","content":[{"type":"output_text","text":"draft"}]}}`))
	result := fold.HandleEvent([]byte(`{"type":"response.completed","sequence_number":4,"response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":516,"total_tokens":517,"output_tokens_details":{"reasoning_tokens":516}}}}`))
	if !result.Continue {
		t.Fatal("setup did not request continuation")
	}
	events := fold.FinalizeAfterContinuationFailure()
	if len(events) < 2 {
		t.Fatalf("fallback events = %d, want draft + terminal", len(events))
	}
	terminal := events[len(events)-1]
	if got := gjson.GetBytes(terminal, "response.output.1.content.0.text").String(); got != "draft" {
		t.Fatalf("fallback draft text = %q, want draft: %s", got, terminal)
	}
}

func TestCodexContinueFoldCleanStreamPassthroughBytes(t *testing.T) {
	fold := newTestCodexContinueFold()
	events := [][]byte{
		[]byte(`{"type":"response.created","sequence_number":0,"response":{"id":"resp_1","status":"in_progress"}}`),
		[]byte(`{"type":"response.in_progress","sequence_number":1,"response":{"id":"resp_1"}}`),
		[]byte(`{"type":"response.output_item.added","sequence_number":2,"output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`),
		[]byte(`{"type":"response.reasoning_summary_text.delta","sequence_number":3,"output_index":0,"delta":"thinking"}`),
		[]byte(`{"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc"}}`),
	}
	for _, ev := range events {
		result := fold.HandleEvent(ev)
		if len(result.Emit) != 1 || !bytesEqualJSON(t, result.Emit[0], ev) {
			t.Fatalf("clean stream event mutated:\n in: %s\nout: %v", ev, result.Emit)
		}
	}
	// Buffered message events emit nothing until the terminal.
	added := []byte(`{"type":"response.output_item.added","sequence_number":5,"output_index":1,"item":{"type":"message","id":"msg_1"}}`)
	if result := fold.HandleEvent(added); len(result.Emit) != 0 {
		t.Fatalf("buffered added event leaked: %v", result.Emit)
	}
	done := []byte(`{"type":"response.output_item.done","sequence_number":6,"output_index":1,"item":{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"hi"}]}}`)
	if result := fold.HandleEvent(done); len(result.Emit) != 0 {
		t.Fatalf("buffered done event leaked: %v", result.Emit)
	}
	terminal := []byte(`{"type":"response.completed","sequence_number":7,"response":{"id":"resp_1","status":"completed","usage":{"input_tokens":5,"output_tokens":30,"total_tokens":35,"output_tokens_details":{"reasoning_tokens":20}}}}`)
	result := fold.HandleEvent(terminal)
	if !result.Done || result.Continue {
		t.Fatalf("clean terminal mishandled: %+v", result)
	}
	// Flush order: buffered events then the untouched terminal.
	if len(result.Emit) != 3 {
		t.Fatalf("clean flush emitted %d events, want 3", len(result.Emit))
	}
	if !bytesEqualJSON(t, result.Emit[0], added) || !bytesEqualJSON(t, result.Emit[1], done) || !bytesEqualJSON(t, result.Emit[2], terminal) {
		t.Fatalf("clean flush mutated events: %s %s %s", result.Emit[0], result.Emit[1], result.Emit[2])
	}
}

func TestCodexContinueFoldFailureTerminalPassthroughNoUsage(t *testing.T) {
	fold := newTestCodexContinueFold()
	result := fold.HandleEvent([]byte(`{"type":"response.failed","sequence_number":0,"response":{"id":"resp_1","status":"failed"}}`))
	if !result.Done {
		t.Fatalf("failure terminal did not finish fold: %+v", result)
	}
	if len(result.PublishUsage) != 0 {
		t.Fatalf("failure terminal published usage: %d", len(result.PublishUsage))
	}
}

func TestCodexContinueFoldContinuationFailureTerminalFlushesDraft(t *testing.T) {
	fold := newTestCodexContinueFold()
	fold.HandleEvent([]byte(`{"type":"response.output_item.added","sequence_number":0,"output_index":0,"item":{"type":"reasoning"}}`))
	fold.HandleEvent([]byte(`{"type":"response.output_item.done","sequence_number":1,"output_index":0,"item":{"type":"reasoning","encrypted_content":"enc"}}`))
	fold.HandleEvent([]byte(`{"type":"response.output_item.added","sequence_number":2,"output_index":1,"item":{"type":"message"}}`))
	fold.HandleEvent([]byte(`{"type":"response.output_item.done","sequence_number":3,"output_index":1,"item":{"type":"message","content":[{"type":"output_text","text":"draft"}]}}`))
	result := fold.HandleEvent([]byte(`{"type":"response.completed","sequence_number":4,"response":{"id":"resp_1","status":"completed","usage":{"input_tokens":1,"output_tokens":516,"total_tokens":517,"output_tokens_details":{"reasoning_tokens":516}}}}`))
	if !result.Continue {
		t.Fatal("setup did not request continuation")
	}
	fold.NoteContinuationSent(codexContinueDefaultMethod)
	result = fold.HandleEvent([]byte(`{"type":"response.failed","sequence_number":0,"response":{"id":"resp_2","status":"failed"}}`))
	if !result.Done {
		t.Fatalf("continuation failure terminal did not finish: %+v", result)
	}
	if len(result.Emit) == 0 {
		t.Fatal("continuation failure did not flush retained draft")
	}
	terminal := result.Emit[len(result.Emit)-1]
	if got := gjson.GetBytes(terminal, "type").String(); got != "response.completed" {
		t.Fatalf("fallback terminal type = %q, want response.completed: %s", got, terminal)
	}
	if got := gjson.GetBytes(terminal, "response.output.1.content.0.text").String(); got != "draft" {
		t.Fatalf("fallback draft text = %q: %s", got, terminal)
	}
}

func bytesEqualJSON(t *testing.T, a []byte, b []byte) bool {
	t.Helper()
	return string(a) == string(b)
}

func newTestCodexContinueFold() *codexContinueFold {
	return newCodexContinueFold([]byte(`{"stream":true,"input":[]}`), &config.Config{Codex: config.CodexConfig{ContinueThinking: config.CodexContinueThinking{Enabled: true}}})
}

func int64Ptr(v int64) *int64 { return &v }

func includeHasEncrypted(body []byte) bool {
	for _, item := range gjson.GetBytes(body, "include").Array() {
		if item.String() == codexContinueEncryptedInclude {
			return true
		}
	}
	return false
}
