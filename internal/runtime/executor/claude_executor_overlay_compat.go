package executor

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
)

// ensureClaudeThinkingDisplay preserves the fork overlay entry point while
// delegating summary visibility to the canonical thinking pipeline.
func ensureClaudeThinkingDisplay(body []byte) []byte {
	if claudeThinkingDisplaySet(body) {
		return body
	}
	return thinking.ApplySummaryConfig(body, "claude", thinking.SummaryConfig{Mode: thinking.SummaryEnabled})
}

func checkSystemInstructions(payload []byte) []byte {
	return checkSystemInstructionsWithMode(payload, false)
}

func getClientUserAgent(ctx context.Context) string {
	return resolveIncomingClaudeHeaders(ctx, nil).Get("User-Agent")
}

func parseEntrypointFromUA(userAgent string) string {
	entrypoint := helps.ClaudeCodeEntrypoint(userAgent)
	if entrypoint == "" {
		return "cli"
	}
	return entrypoint
}

func sanitizeForwardedSystemPrompt(text string) string {
	blocks := collectForwardedClaudeSystemPromptBlocks(gjson.Result{Type: gjson.String, Str: text})
	if len(blocks) == 0 {
		return ""
	}
	return blocks[0]
}

func prependToFirstUserMessage(payload []byte, text string) []byte {
	if strings.TrimSpace(text) == "" {
		return payload
	}
	return prependClaudeSystemRemindersToFirstUserMessage(payload, []string{text})
}
