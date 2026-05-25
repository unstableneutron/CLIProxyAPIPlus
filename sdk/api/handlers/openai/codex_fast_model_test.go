package openai

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeCodexFastSpeedTierRequest(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"model":"gpt-5.5-fast","input":"hello"}`)
	got := normalizeCodexFastSpeedTierRequest(raw)

	if model := gjson.GetBytes(got, "model").String(); model != "gpt-5.5" {
		t.Fatalf("model = %q, want gpt-5.5; payload=%s", model, string(got))
	}
	if tier := gjson.GetBytes(got, "service_tier").String(); tier != "priority" {
		t.Fatalf("service_tier = %q, want priority; payload=%s", tier, string(got))
	}
}

func TestNormalizeCodexFastSpeedTierRequestPreservesThinkingSuffix(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"model":"gpt-5.5-fast(high)","input":"hello"}`)
	got := normalizeCodexFastSpeedTierRequest(raw)

	if model := gjson.GetBytes(got, "model").String(); model != "gpt-5.5(high)" {
		t.Fatalf("model = %q, want gpt-5.5(high); payload=%s", model, string(got))
	}
	if tier := gjson.GetBytes(got, "service_tier").String(); tier != "priority" {
		t.Fatalf("service_tier = %q, want priority; payload=%s", tier, string(got))
	}
}

func TestNormalizeCodexFastSpeedTierRequestLeavesUnsupportedFastModel(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"model":"unknown-fast","input":"hello"}`)
	got := normalizeCodexFastSpeedTierRequest(raw)

	if model := gjson.GetBytes(got, "model").String(); model != "unknown-fast" {
		t.Fatalf("model = %q, want unknown-fast; payload=%s", model, string(got))
	}
	if gjson.GetBytes(got, "service_tier").Exists() {
		t.Fatalf("service_tier should not be set for unsupported model; payload=%s", string(got))
	}
}
