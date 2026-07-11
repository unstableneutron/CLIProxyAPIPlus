package responsesreplay

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestAdvanceConstraintsIsMonotoneAndReachesFixedPoints(t *testing.T) {
	reachable := []Constraints{
		0,
		RequireReplay,
		RequireReplay | OmitEncryptedContent,
		RequireReplay | OmitProviderIdentifiers,
		RequireReplay | OmitEncryptedContent | OmitProviderIdentifiers,
	}
	kinds := []FailureKind{
		FailurePreviousResponseMissing,
		FailureProviderItemMissing,
		FailureInvalidEncryptedContent,
	}
	for _, current := range reachable {
		for _, kind := range kinds {
			next, _ := Advance(current, kind, false)
			if next|current != next {
				t.Fatalf("Advance(%v, %v) regressed to %v", current, kind, next)
			}
			fixed := next
			for range 3 {
				var changed bool
				fixed, changed = Advance(fixed, kind, false)
				if !changed {
					break
				}
			}
			again, changed := Advance(fixed, kind, false)
			if again != fixed || changed {
				t.Fatalf("Advance(%v, %v) did not reach fixed point: %v changed=%v", fixed, kind, again, changed)
			}
		}
	}
}

func TestAdvanceConstraintsStateErrorsCommute(t *testing.T) {
	first, _ := Advance(0, FailureInvalidEncryptedContent, false)
	first, _ = Advance(first, FailureProviderItemMissing, false)
	second, _ := Advance(0, FailureProviderItemMissing, false)
	second, _ = Advance(second, FailureInvalidEncryptedContent, false)
	if first != second {
		t.Fatalf("state transitions do not commute: %v != %v", first, second)
	}
}

func TestRenderWithConstraintsUsesReplayAndPreservesCompactionEncryptedContent(t *testing.T) {
	native := []byte(`{"previous_response_id":"resp-native","input":[]}`)
	replay := []byte(`{"previous_response_id":"resp-replay","input":[{"type":"reasoning","id":"reason-1","encrypted_content":"opaque"},{"type":"compaction","id":"compact-1","encrypted_content":"keep"}]}`)
	nativeBefore := bytes.Clone(native)
	replayBefore := bytes.Clone(replay)

	rendered, digest, changed, err := RenderWithConstraints(native, replay, RequireReplay|OmitProviderIdentifiers|OmitEncryptedContent)
	if err != nil {
		t.Fatalf("RenderWithConstraints() error = %v", err)
	}
	if !changed || !json.Valid(rendered) {
		t.Fatalf("rendered payload invalid or unchanged: changed=%v payload=%s", changed, rendered)
	}
	if digest == ([32]byte{}) {
		t.Fatal("render digest is empty")
	}
	if gjson.GetBytes(rendered, "previous_response_id").Exists() || gjson.GetBytes(rendered, "input.0.id").Exists() {
		t.Fatalf("provider identifiers survived: %s", rendered)
	}
	if gjson.GetBytes(rendered, "input.0.encrypted_content").Exists() {
		t.Fatalf("non-portable encrypted content survived: %s", rendered)
	}
	if got := gjson.GetBytes(rendered, "input.1.encrypted_content").String(); got != "keep" {
		t.Fatalf("compaction encrypted content = %q, want keep", got)
	}
	if !bytes.Equal(native, nativeBefore) || !bytes.Equal(replay, replayBefore) {
		t.Fatal("renderer mutated its inputs")
	}
}

func TestClassifyFailurePrefersStructuredStateCodes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		message string
		want    FailureKind
	}{
		{"previous", 404, `{"error":{"code":"previous_response_not_found"}}`, FailurePreviousResponseMissing},
		{"item", 404, `{"error":{"code":"item_not_found","param":"input.0.id"}}`, FailureProviderItemMissing},
		{"encrypted", 400, `{"error":{"code":"invalid_encrypted_content"}}`, FailureInvalidEncryptedContent},
		{"route", 503, "provider unavailable", FailureAuthOrRoute},
		{"request", 400, "unsupported schema", FailureRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyFailure(tc.status, tc.message); got != tc.want {
				t.Fatalf("ClassifyFailure() = %v, want %v", got, tc.want)
			}
		})
	}
}
