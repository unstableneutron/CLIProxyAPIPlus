package claude

import "testing"

// TestConvertCodexResponseToClaude_StreamThinkingFinalizesPendingBlockBeforeNextSummaryPart
// retains the historical fork test name. Original now deliberately keeps the
// block open across summary parts so one reasoning item has one Claude block.
func TestConvertCodexResponseToClaude_StreamThinkingFinalizesPendingBlockBeforeNextSummaryPart(t *testing.T) {
	digest := digestCodexThinkingStream(t, [][]byte{
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"First part\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.done\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
	})

	if digest.Starts != 1 {
		t.Fatalf("expected one thinking block across summary parts, got %d", digest.Starts)
	}
	if digest.Stops != 0 {
		t.Fatalf("expected thinking block to remain open until reasoning item completion, got %d stops", digest.Stops)
	}
	if want := "First part\n\n"; digest.Thinking != want {
		t.Fatalf("thinking text = %q, want %q", digest.Thinking, want)
	}
}

// TestConvertCodexResponseToClaude_StreamThinkingRetainsSignatureAcrossMultipartReasoning
// retains the historical fork test name while asserting Original's newer
// one-signature-per-reasoning-item invariant.
func TestConvertCodexResponseToClaude_StreamThinkingRetainsSignatureAcrossMultipartReasoning(t *testing.T) {
	digest := digestCodexThinkingStream(t, [][]byte{
		[]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"reasoning\",\"encrypted_content\":\"enc_sig_multipart\"}}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"First part\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.done\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.added\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"Second part\"}"),
		[]byte("data: {\"type\":\"response.reasoning_summary_part.done\"}"),
		[]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\"}}"),
	})

	if digest.Starts != 1 || digest.Stops != 1 {
		t.Fatalf("expected one completed thinking block, got %d starts and %d stops", digest.Starts, digest.Stops)
	}
	if len(digest.Signatures) != 1 || digest.Signatures[0] != "enc_sig_multipart" {
		t.Fatalf("expected one retained signature, got %v", digest.Signatures)
	}
	if want := "First part\n\nSecond part"; digest.Thinking != want {
		t.Fatalf("thinking text = %q, want %q", digest.Thinking, want)
	}
}
