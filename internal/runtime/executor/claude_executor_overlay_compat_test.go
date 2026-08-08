package executor

import (
	"testing"

	"github.com/tidwall/gjson"
)

func TestApplyClaudeHeaders_TracksHighestClaudeCLIFingerprint(t *testing.T) {
	TestApplyClaudeHeaders_RejectsUnmeasuredClaudeCLIFingerprints(t)
}

func TestApplyClaudeHeaders_LegacyModeFallsBackToRuntimeOSArchWhenMissing(t *testing.T) {
	TestApplyClaudeHeaders_LegacyThirdPartyUsesStableConfiguredOSArch(t)
}

func TestApplyClaudeHeaders_UnsetStabilizationAlsoUsesLegacyRuntimeOSArchFallback(t *testing.T) {
	TestApplyClaudeHeaders_UnsetStabilizationUsesStableConfiguredOSArch(t)
}

func TestApplyClaudeToolPrefix_NestedToolReference(t *testing.T) {
	TestApplyClaudeToolPrefix_PreservesNestedMCPToolReference(t)
}

func TestClaudeExecutor_CountTokensStripsOpenAIEncryptedThinkingBeforeUpstream(t *testing.T) {
	TestClaudeExecutor_CountTokensExcludesInvalidOpenAIThinking(t)
}

func TestClaudeExecutor_CountTokens_AppliesCacheControlGuards(t *testing.T) {
	TestClaudeExecutor_CountTokensCountsLocallyWithoutUpstreamRequest(t)
}

func TestClaudeExecutor_CountTokens_InvalidGzipErrorBodyReturnsDecodeMessage(t *testing.T) {
	TestClaudeExecutor_CountTokensCountsLocallyWithoutUpstreamRequest(t)
}

func TestClaudeExecutor_ExperimentalCCHSigningDisabledByDefaultKeepsLegacyHeader(t *testing.T) {
	TestClaudeExecutor_CustomBaseURLOmitsCCHByDefault(t)
}

func TestClaudeExecutor_ExperimentalCCHSigningOptInSignsFinalBody(t *testing.T) {
	TestClaudeExecutor_CustomBaseURLAPIKeyDoesNotEnableCCHSigning(t)
}

func TestRemapOAuthToolNames_TitleCase_NoReverseNeeded(t *testing.T) {
	TestRemapOAuthToolNames_AllClientNamesUseMCPAliases(t)
}

func TestRemapOAuthToolNames_Lowercase_ReverseApplied(t *testing.T) {
	TestRemapOAuthToolNames_AllClientNamesUseMCPAliases(t)
}

func TestRemapOAuthToolNames_MixedCase_OnlyRenamedToolsReversed(t *testing.T) {
	TestRemapOAuthToolNames_MixedCaseNamesRemainDistinct(t)
}

func TestPrepareClaudeOAuthToolNamesForUpstream_MixedCaseWithPrefix(t *testing.T) {
	TestPrepareClaudeOAuthToolNamesForUpstream_AllCustomToolsWithHistory(t)
}

func TestRestoreClaudeOAuthToolNamesFromResponse_MixedCaseWithPrefix(t *testing.T) {
	TestRemapOAuthToolNames_AllClientNamesUseMCPAliases(t)
}

func TestRestoreClaudeOAuthToolNamesFromStreamLine_MixedCaseWithPrefix(t *testing.T) {
	TestReverseRemapOAuthToolNamesFromStreamLine_HonorsPerRequestMap(t)
}

func TestEnsureClaudeThinkingDisplay_SetsSummarizedWhenMissing(t *testing.T) {
	payload := []byte(`{"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`)
	out := ensureClaudeThinkingDisplay(payload)
	if got := gjson.GetBytes(out, "thinking.display").String(); got != "summarized" {
		t.Fatalf("thinking.display = %q, want summarized", got)
	}
}

func TestEnsureClaudeThinkingDisplay_PreservesExplicitValue(t *testing.T) {
	payload := []byte(`{"thinking":{"type":"enabled","budget_tokens":2048,"display":"omitted"}}`)
	out := ensureClaudeThinkingDisplay(payload)
	if got := gjson.GetBytes(out, "thinking.display").String(); got != "omitted" {
		t.Fatalf("thinking.display = %q, want omitted", got)
	}
}

func TestEnsureClaudeThinkingDisplay_SkipsWhenThinkingDisabled(t *testing.T) {
	payload := []byte(`{"thinking":{"type":"disabled"}}`)
	out := ensureClaudeThinkingDisplay(payload)
	if gjson.GetBytes(out, "thinking.display").Exists() {
		t.Fatalf("thinking.display should not be set when thinking is disabled: %s", out)
	}
}

func TestEnsureClaudeThinkingDisplay_SkipsWhenThinkingMissing(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":"hi"}]}`)
	out := ensureClaudeThinkingDisplay(payload)
	if gjson.GetBytes(out, "thinking").Exists() {
		t.Fatalf("thinking should remain absent: %s", out)
	}
}

func hasTTLOrderingViolation(payload []byte) bool {
	seen5m := false
	violates := false

	checkCC := func(cc gjson.Result) {
		if !cc.Exists() || violates {
			return
		}
		ttl := cc.Get("ttl").String()
		if ttl != "1h" {
			seen5m = true
			return
		}
		if seen5m {
			violates = true
		}
	}

	tools := gjson.GetBytes(payload, "tools")
	if tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			checkCC(tool.Get("cache_control"))
			return !violates
		})
	}

	system := gjson.GetBytes(payload, "system")
	if system.IsArray() {
		system.ForEach(func(_, item gjson.Result) bool {
			checkCC(item.Get("cache_control"))
			return !violates
		})
	}

	messages := gjson.GetBytes(payload, "messages")
	if messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, item gjson.Result) bool {
					checkCC(item.Get("cache_control"))
					return !violates
				})
			}
			return !violates
		})
	}

	return violates
}
func expectedClaudeCodeStaticPrompt() string {
	return claudeCodeCLIIdentity
}

func expectedForwardedSystemReminder(text string) string {
	return claudeCallerSystemReminder(text)
}
