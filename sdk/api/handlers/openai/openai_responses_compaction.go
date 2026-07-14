package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	compactToolOutputTruncateThreshold = 24 * 1024
	compactToolOutputLongLineThreshold = 8 * 1024
	compactToolOutputHeadBytes         = 4 * 1024
	compactToolOutputTailBytes         = 4 * 1024
	compactToolOutputOmitThreshold     = 512 * 1024
	compactToolOutputTotalBudget       = 192 * 1024
)

func isResponsesCompactionTriggerRequest(rawJSON []byte) bool {
	input := gjson.GetBytes(rawJSON, "input")
	if !input.IsArray() {
		return false
	}
	for _, item := range input.Array() {
		if strings.TrimSpace(item.Get("type").String()) == "compaction_trigger" {
			return true
		}
	}
	return false
}

func sanitizeOpenAIResponsesCompactRequest(rawJSON []byte) []byte {
	if len(bytes.TrimSpace(rawJSON)) == 0 || !json.Valid(rawJSON) {
		return rawJSON
	}
	updated := bytes.Clone(rawJSON)
	for _, field := range []string{
		"stream",
		"stream_options",
		"store",
		"include",
		"tools",
		"tool_choice",
		"text",
		"client_metadata",
		"prompt_cache_key",
		"prompt_cache_retention",
		"safety_identifier",
		"previous_response_id",
		"generate",
		"type",
	} {
		if next, errDelete := sjson.DeleteBytes(updated, field); errDelete == nil {
			updated = next
		}
	}
	updated = stripUnsupportedResponsesWebsocketInputItemMetadata(updated)
	updated = truncateOpenAIResponsesCompactToolOutputs(updated)
	return updated
}

type compactToolOutputEntry struct {
	index         int
	originalBytes int
	currentBytes  int
	replacement   string
	changed       bool
}

func truncateOpenAIResponsesCompactToolOutputs(rawJSON []byte) []byte {
	input := gjson.GetBytes(rawJSON, "input")
	if !input.IsArray() {
		return rawJSON
	}
	entries := make([]compactToolOutputEntry, 0)
	totalBytes := 0
	for index, item := range input.Array() {
		if item.Get("type").String() != "function_call_output" {
			continue
		}
		output := item.Get("output")
		if output.Type != gjson.String {
			continue
		}
		value := output.String()
		entry := compactToolOutputEntry{
			index:         index,
			originalBytes: len(value),
			currentBytes:  len(value),
			replacement:   value,
		}
		maxLineBytes := maxLineBytes(value)
		if len(value) > compactToolOutputTruncateThreshold || maxLineBytes > compactToolOutputLongLineThreshold {
			if shouldOmitCompactToolOutput(value, maxLineBytes) {
				entry.replacement = compactToolOutputOmittedMarker(len(value))
			} else {
				entry.replacement = compactToolOutputTruncatedValue(value)
			}
			entry.currentBytes = len(entry.replacement)
			entry.changed = true
		}
		totalBytes += entry.currentBytes
		entries = append(entries, entry)
	}
	if len(entries) == 0 {
		return rawJSON
	}
	if totalBytes > compactToolOutputTotalBudget {
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].currentBytes > entries[j].currentBytes
		})
		for i := range entries {
			if totalBytes <= compactToolOutputTotalBudget {
				break
			}
			marker := compactToolOutputOmittedMarker(entries[i].originalBytes)
			if entries[i].replacement == marker {
				continue
			}
			totalBytes -= entries[i].currentBytes - len(marker)
			entries[i].replacement = marker
			entries[i].currentBytes = len(marker)
			entries[i].changed = true
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].index < entries[j].index })
	updated := rawJSON
	for _, entry := range entries {
		if !entry.changed {
			continue
		}
		path := fmt.Sprintf("input.%d.output", entry.index)
		if next, errSet := sjson.SetBytes(updated, path, entry.replacement); errSet == nil {
			updated = next
		}
	}
	return updated
}

func shouldOmitCompactToolOutput(output string, maxLineBytes int) bool {
	if len(output) > compactToolOutputOmitThreshold {
		return true
	}
	if len(output) > 64*1024 && maxLineBytes*10 >= len(output)*8 {
		return true
	}
	return compactToolOutputLooksBinary(output)
}

func compactToolOutputLooksBinary(output string) bool {
	if output == "" {
		return false
	}
	sample := output
	if len(sample) > 4096 {
		sample = utf8SafePrefix(sample, 4096)
	}
	bad := 0
	for i := 0; i < len(sample); i++ {
		b := sample[i]
		if b == '\t' || b == '\n' || b == '\r' {
			continue
		}
		if b < 0x20 || b == 0x7f {
			bad++
		}
	}
	return bad*100 > len(sample)*5 || !utf8.ValidString(sample)
}

func maxLineBytes(s string) int {
	maxLen := 0
	lineStart := 0
	for i := 0; i < len(s); i++ {
		if s[i] != '\n' {
			continue
		}
		if lineLen := i - lineStart; lineLen > maxLen {
			maxLen = lineLen
		}
		lineStart = i + 1
	}
	if lineLen := len(s) - lineStart; lineLen > maxLen {
		maxLen = lineLen
	}
	return maxLen
}

func compactToolOutputTruncatedValue(output string) string {
	return fmt.Sprintf(
		"[tool output truncated: %d bytes]\n\n%s\n\n[...truncated...]\n\n%s",
		len(output),
		utf8SafePrefix(output, compactToolOutputHeadBytes),
		utf8SafeSuffix(output, compactToolOutputTailBytes),
	)
}

func compactToolOutputOmittedMarker(originalBytes int) string {
	return fmt.Sprintf("[tool output omitted: %d bytes]", originalBytes)
}

func utf8SafePrefix(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(s[:end]) {
		end--
	}
	return s[:end]
}

func utf8SafeSuffix(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.ValidString(s[start:]) {
		start++
	}
	return s[start:]
}
