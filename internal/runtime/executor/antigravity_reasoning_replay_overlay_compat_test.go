package executor

import "testing"

// The replay refactor now drops stale signatures instead of appending an invalid
// carrier. Keep the overlay test name tied to that current behavior.
func TestPrepareAntigravityGeminiReasoningReplayPayloadAppendsStaleThoughtSignatureWithoutNullParts(t *testing.T) {
	TestPrepareAntigravityGeminiReasoningReplayPayloadDropsStaleThoughtSignature(t)
}
