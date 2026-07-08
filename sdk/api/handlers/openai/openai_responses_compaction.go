package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	nativeCheckpointCompactionPrompt = "You are performing a CONTEXT CHECKPOINT COMPACTION."

	compactToolOutputTruncateThreshold = 24 * 1024
	compactToolOutputLongLineThreshold = 8 * 1024
	compactToolOutputHeadBytes         = 4 * 1024
	compactToolOutputTailBytes         = 4 * 1024
	compactToolOutputOmitThreshold     = 512 * 1024
	compactToolOutputTotalBudget       = 192 * 1024
)

func isNativeCheckpointCompactionRequest(rawJSON []byte) bool {
	input := gjson.GetBytes(rawJSON, "input")
	if !input.IsArray() {
		return false
	}
	items := input.Array()
	if len(items) == 0 {
		return false
	}
	last := items[len(items)-1]
	if role := strings.TrimSpace(last.Get("role").String()); role != "user" {
		return false
	}
	itemType := strings.TrimSpace(last.Get("type").String())
	if itemType != "" && itemType != "message" {
		return false
	}
	text := strings.TrimSpace(firstResponsesInputText(last.Get("content")))
	return strings.HasPrefix(text, nativeCheckpointCompactionPrompt)
}

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

func firstResponsesInputText(content gjson.Result) string {
	if content.Type == gjson.String {
		return content.String()
	}
	if !content.IsArray() {
		return ""
	}
	for _, part := range content.Array() {
		partType := strings.TrimSpace(part.Get("type").String())
		if partType != "" && partType != "input_text" && partType != "text" {
			continue
		}
		if text := part.Get("text"); text.Type == gjson.String {
			return text.String()
		}
	}
	return ""
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

func writeOpenAIResponsesCompactSSE(w io.Writer, compactData []byte) {
	for _, chunk := range buildOpenAIResponsesCompactSSEChunks(compactData) {
		writeResponsesSSEChunk(w, chunk)
	}
}

func buildOpenAIResponsesCompactSSEChunks(compactData []byte) [][]byte {
	responseID := compactResponseID(compactData)
	createdAt, completedAt := compactResponseTimes(compactData)
	outputItems := compactResponseOutputItems(compactData)
	outputRaw := compactMarshalRawMessages(outputItems)

	createdResponse := compactBaseResponse(compactData, responseID, createdAt, "in_progress")
	inProgressResponse := compactBaseResponse(compactData, responseID, createdAt, "in_progress")
	completedResponse := compactBaseResponse(compactData, responseID, createdAt, "completed")
	completedResponse, _ = sjson.SetBytes(completedResponse, "completed_at", completedAt)
	completedResponse, _ = sjson.SetRawBytes(completedResponse, "output", outputRaw)

	sequence := 0
	createdPayload := []byte(`{"type":"response.created"}`)
	createdPayload, _ = sjson.SetBytes(createdPayload, "sequence_number", sequence)
	createdPayload, _ = sjson.SetRawBytes(createdPayload, "response", createdResponse)
	sequence++

	inProgressPayload := []byte(`{"type":"response.in_progress"}`)
	inProgressPayload, _ = sjson.SetBytes(inProgressPayload, "sequence_number", sequence)
	inProgressPayload, _ = sjson.SetRawBytes(inProgressPayload, "response", inProgressResponse)
	sequence++

	chunks := [][]byte{
		openAIResponsesSSEFrame("response.created", createdPayload),
		openAIResponsesSSEFrame("response.in_progress", inProgressPayload),
	}
	for i, item := range outputItems {
		addedPayload := []byte(`{"type":"response.output_item.added"}`)
		addedPayload, _ = sjson.SetBytes(addedPayload, "sequence_number", sequence)
		addedPayload, _ = sjson.SetBytes(addedPayload, "output_index", i)
		addedPayload, _ = sjson.SetRawBytes(addedPayload, "item", item)
		sequence++
		chunks = append(chunks, openAIResponsesSSEFrame("response.output_item.added", addedPayload))

		donePayload := []byte(`{"type":"response.output_item.done"}`)
		donePayload, _ = sjson.SetBytes(donePayload, "sequence_number", sequence)
		donePayload, _ = sjson.SetBytes(donePayload, "output_index", i)
		donePayload, _ = sjson.SetRawBytes(donePayload, "item", item)
		sequence++
		chunks = append(chunks, openAIResponsesSSEFrame("response.output_item.done", donePayload))
	}
	completedPayload := []byte(`{"type":"response.completed"}`)
	completedPayload, _ = sjson.SetBytes(completedPayload, "sequence_number", sequence)
	completedPayload, _ = sjson.SetRawBytes(completedPayload, "response", completedResponse)
	chunks = append(chunks, openAIResponsesSSEFrame("response.completed", completedPayload))
	return chunks
}

func compactResponseID(compactData []byte) string {
	if id := strings.TrimSpace(gjson.GetBytes(compactData, "id").String()); id != "" {
		return id
	}
	return fmt.Sprintf("resp_compact_%d", time.Now().UnixNano())
}

func compactResponseTimes(compactData []byte) (int64, int64) {
	now := time.Now().Unix()
	createdAt := gjson.GetBytes(compactData, "created_at").Int()
	if createdAt <= 0 {
		createdAt = now
	}
	completedAt := gjson.GetBytes(compactData, "completed_at").Int()
	if completedAt <= 0 {
		completedAt = now
	}
	return createdAt, completedAt
}

func compactResponseOutputItems(compactData []byte) []json.RawMessage {
	output := gjson.GetBytes(compactData, "output")
	if !output.IsArray() {
		return nil
	}
	items := make([]json.RawMessage, 0, len(output.Array()))
	for _, item := range output.Array() {
		if item.Raw == "" || !json.Valid([]byte(item.Raw)) {
			continue
		}
		items = append(items, json.RawMessage(item.Raw))
	}
	return items
}

func compactMarshalRawMessages(items []json.RawMessage) []byte {
	if len(items) == 0 {
		return []byte("[]")
	}
	data, err := json.Marshal(items)
	if err != nil {
		return []byte("[]")
	}
	return data
}

func compactBaseResponse(compactData []byte, responseID string, createdAt int64, status string) []byte {
	response := bytes.TrimSpace(compactData)
	if len(response) == 0 || !json.Valid(response) || !gjson.ParseBytes(response).IsObject() {
		response = []byte(`{"object":"response","output":[]}`)
	} else {
		response = bytes.Clone(response)
	}
	response, _ = sjson.SetBytes(response, "id", responseID)
	response, _ = sjson.SetBytes(response, "object", "response")
	response, _ = sjson.SetBytes(response, "created_at", createdAt)
	response, _ = sjson.SetBytes(response, "status", status)
	response, _ = sjson.SetRawBytes(response, "output", []byte("[]"))
	return response
}

func openAIResponsesSSEFrame(event string, payload []byte) []byte {
	var out bytes.Buffer
	if strings.TrimSpace(event) != "" {
		out.WriteString("event: ")
		out.WriteString(event)
		out.WriteByte('\n')
	}
	for _, line := range bytes.Split(payload, []byte("\n")) {
		out.WriteString("data: ")
		out.Write(line)
		out.WriteByte('\n')
	}
	out.WriteByte('\n')
	return out.Bytes()
}
