package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (e *CodexWebsocketsExecutor) executeCompactionTriggerFromWebsocketContext(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, requestPayload []byte, sessionID string) (*cliproxyexecutor.StreamResult, bool, error) {
	state := getXAIWebsocketIDState(e.idStore, sessionID)
	if state == nil {
		return e.executeCompactionTriggerWithoutWebsocketTranscript(ctx, auth, req, opts)
	}
	transcriptInput := state.snapshotTranscriptInput()
	if len(transcriptInput) == 0 {
		return e.executeCompactionTriggerWithoutWebsocketTranscript(ctx, auth, req, opts)
	}
	transcriptInput = codexWebsocketCompactionReplayInput(transcriptInput, requestPayload)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	_, baseURL := codexCreds(auth)
	if baseURL == "" {
		baseURL = "https://chatgpt.com/backend-api/codex"
	}
	currentProvenance := codexWebsocketTranscriptProvenance(auth, baseURL, baseModel)
	stripEncryptedContent := false
	if transcriptProvenance, ok := state.snapshotTranscriptProvenance(); ok && !transcriptProvenance.sameOrigin(currentProvenance) {
		stripEncryptedContent = true
		helps.LogWithRequestID(ctx).Debugf("codex websockets executor: stripping compact replay encrypted_content because transcript provenance changed or is mixed")
	}
	compactPayload, err := buildCodexWebsocketCompactionPayloadWithOptions(requestPayload, transcriptInput, codexWebsocketCompactionPayloadOptions{StripEncryptedContent: stripEncryptedContent})
	if err != nil {
		return nil, true, err
	}
	compactReq := req
	compactReq.Payload = compactPayload
	compactOpts := opts
	compactOpts.Stream = false
	compactOpts.Alt = "responses/compact"
	compactOpts.ResponseFormat = sdktranslator.FromString("openai-response")

	resp, err := e.CodexExecutor.executeCompactWithEncryptedContentFallback(ctx, auth, compactReq, compactOpts)
	if err != nil {
		return nil, true, err
	}
	responseID := codexCompactionResponseID(resp.Payload)
	state.replaceTranscriptWithItemsAndProvenance(currentProvenance, codexCompactionOutputItems(resp.Payload, responseID)...)

	chunks := codexBuildCompactionTriggerStreamChunks(resp.Payload, responseID)
	out := make(chan cliproxyexecutor.StreamChunk, len(chunks))
	for _, chunk := range chunks {
		out <- cliproxyexecutor.StreamChunk{Payload: chunk}
	}
	close(out)
	headers := resp.Headers.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	headers.Set("Content-Type", "text/event-stream")
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, true, nil
}

func (e *CodexWebsocketsExecutor) executeCompactionTriggerWithoutWebsocketTranscript(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, bool, error) {
	helps.LogWithRequestID(ctx).Debugf("codex websockets executor: compact trigger falling back without websocket transcript context")
	streamResult, err := e.CodexExecutor.executeCompactionTriggerStream(ctx, auth, req, opts)
	return streamResult, true, err
}

func buildCodexWebsocketCompactionPayload(payload []byte, transcriptInput []byte) ([]byte, error) {
	return buildCodexWebsocketCompactionPayloadWithOptions(payload, transcriptInput, codexWebsocketCompactionPayloadOptions{})
}

type codexWebsocketCompactionPayloadOptions struct {
	StripEncryptedContent bool
}

func buildCodexWebsocketCompactionPayloadWithOptions(payload []byte, transcriptInput []byte, opts codexWebsocketCompactionPayloadOptions) ([]byte, error) {
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	if len(transcriptInput) == 0 {
		transcriptInput = []byte("[]")
	}
	out := bytes.Clone(payload)
	var err error
	out, err = sjson.SetRawBytes(out, "input", transcriptInput)
	if err != nil {
		return nil, err
	}
	out, _ = sjson.DeleteBytes(out, "previous_response_id")
	out, _ = sjson.DeleteBytes(out, "type")
	out, _ = sjson.DeleteBytes(out, "generate")
	out = sanitizeCodexWebsocketCompactionReplayPayloadWithOptions(out, codexWebsocketCompactionReplaySanitizeOptions{StripEncryptedContent: opts.StripEncryptedContent})
	return out, nil
}

type codexWebsocketCompactionReplaySanitizeOptions struct {
	StripEncryptedContent bool
}

func sanitizeCodexWebsocketCompactionReplayPayload(payload []byte) []byte {
	return sanitizeCodexWebsocketCompactionReplayPayloadWithOptions(payload, codexWebsocketCompactionReplaySanitizeOptions{})
}

func sanitizeCodexWebsocketCompactionReplayPayloadWithOptions(payload []byte, opts codexWebsocketCompactionReplaySanitizeOptions) []byte {
	if len(bytes.TrimSpace(payload)) == 0 || !json.Valid(payload) {
		return payload
	}
	updated := bytes.Clone(payload)
	for _, field := range []string{
		"stream",
		"stream_options",
		"store",
		"tools",
		"tool_choice",
		"text",
		"client_metadata",
		"prompt_cache_key",
		"prompt_cache_retention",
		"safety_identifier",
	} {
		if next, errDelete := sjson.DeleteBytes(updated, field); errDelete == nil {
			updated = next
		}
	}
	if include := gjson.GetBytes(updated, "include"); include.Exists() && include.IsArray() {
		kept := make([]string, 0, len(include.Array()))
		changed := false
		for _, item := range include.Array() {
			if strings.TrimSpace(item.String()) == "reasoning.encrypted_content" {
				changed = true
				continue
			}
			kept = append(kept, item.Raw)
		}
		if changed {
			if len(kept) == 0 {
				if next, errDelete := sjson.DeleteBytes(updated, "include"); errDelete == nil {
					updated = next
				}
			} else if next, errSet := sjson.SetRawBytes(updated, "include", []byte("["+strings.Join(kept, ",")+"]")); errSet == nil {
				updated = next
			}
		}
	}
	input := gjson.GetBytes(updated, "input")
	if !input.Exists() || !input.IsArray() {
		return updated
	}
	for index := range input.Array() {
		fields := []string{"id"}
		if opts.StripEncryptedContent {
			fields = append(fields, "encrypted_content")
		}
		for _, field := range fields {
			path := fmt.Sprintf("input.%d.%s", index, field)
			if !gjson.GetBytes(updated, path).Exists() {
				continue
			}
			if next, errDelete := sjson.DeleteBytes(updated, path); errDelete == nil {
				updated = next
			}
		}
	}
	return updated
}

func codexBuildCompactionTriggerStreamChunks(compactData []byte, responseID string) [][]byte {
	createdAt, completedAt := codexCompactionTimes(compactData)
	outputItems := codexCompactionOutputItems(compactData, responseID)
	output := codexMarshalRawMessages(outputItems)

	createdResponse := codexCompactionBaseResponse(compactData, responseID, createdAt, "in_progress")
	inProgressResponse := codexCompactionBaseResponse(compactData, responseID, createdAt, "in_progress")
	completedResponse := codexCompactionBaseResponse(compactData, responseID, createdAt, "completed")
	completedResponse, _ = sjson.SetBytes(completedResponse, "completed_at", completedAt)
	completedResponse, _ = sjson.SetRawBytes(completedResponse, "output", output)

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
		xaiBuildSSEFrame("response.created", createdPayload),
		xaiBuildSSEFrame("response.in_progress", inProgressPayload),
	}
	for i, item := range outputItems {
		addedPayload := []byte(`{"type":"response.output_item.added"}`)
		addedPayload, _ = sjson.SetBytes(addedPayload, "sequence_number", sequence)
		addedPayload, _ = sjson.SetBytes(addedPayload, "output_index", i)
		addedPayload, _ = sjson.SetRawBytes(addedPayload, "item", item)
		sequence++
		chunks = append(chunks, xaiBuildSSEFrame("response.output_item.added", addedPayload))

		donePayload := []byte(`{"type":"response.output_item.done"}`)
		donePayload, _ = sjson.SetBytes(donePayload, "sequence_number", sequence)
		donePayload, _ = sjson.SetBytes(donePayload, "output_index", i)
		donePayload, _ = sjson.SetRawBytes(donePayload, "item", item)
		sequence++
		chunks = append(chunks, xaiBuildSSEFrame("response.output_item.done", donePayload))
	}
	completedPayload := []byte(`{"type":"response.completed"}`)
	completedPayload, _ = sjson.SetBytes(completedPayload, "sequence_number", sequence)
	completedPayload, _ = sjson.SetRawBytes(completedPayload, "response", completedResponse)
	chunks = append(chunks, xaiBuildSSEFrame("response.completed", completedPayload))
	return chunks
}

func codexCompactionBaseResponse(compactData []byte, responseID string, createdAt int64, status string) []byte {
	response := compactJSONBytes(bytes.TrimSpace(compactData))
	if len(response) == 0 || !gjson.ParseBytes(response).IsObject() {
		response = []byte(`{"object":"response","output":[]}`)
	} else {
		response = bytes.Clone(response)
	}
	response, _ = sjson.SetBytes(response, "id", responseID)
	response, _ = sjson.SetBytes(response, "created_at", createdAt)
	response, _ = sjson.SetBytes(response, "status", status)
	if status != "completed" {
		response, _ = sjson.SetRawBytes(response, "output", []byte("[]"))
	}
	if !gjson.GetBytes(response, "object").Exists() {
		response, _ = sjson.SetBytes(response, "object", "response")
	}
	return response
}

func codexCompactionOutputItems(compactData []byte, responseID string) [][]byte {
	items := xaiJSONRawMessages(gjson.GetBytes(compactData, "output"))
	if len(items) == 0 {
		item := []byte(`{"type":"compaction"}`)
		item, _ = sjson.SetBytes(item, "id", xaiCompactionItemID(responseID))
		return [][]byte{item}
	}
	out := make([][]byte, 0, len(items))
	for i, item := range items {
		item = compactJSONBytes(item)
		if len(item) == 0 {
			continue
		}
		if !gjson.GetBytes(item, "id").Exists() {
			item, _ = sjson.SetBytes(item, "id", fmt.Sprintf("%s_%d", xaiCompactionItemID(responseID), i))
		}
		if !gjson.GetBytes(item, "type").Exists() {
			item, _ = sjson.SetBytes(item, "type", "compaction")
		}
		out = append(out, item)
	}
	return out
}

func compactJSONBytes(raw []byte) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || !json.Valid(raw) {
		return raw
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}

func codexCompactionResponseID(compactData []byte) string {
	if responseID := strings.TrimSpace(gjson.GetBytes(compactData, "id").String()); responseID != "" {
		if strings.HasPrefix(responseID, "resp_") {
			return responseID
		}
		return "resp_" + strings.TrimPrefix(responseID, "cmp_")
	}
	return fmt.Sprintf("resp_codex_compaction_%d", time.Now().UnixNano())
}

func codexCompactionTimes(compactData []byte) (int64, int64) {
	now := time.Now().Unix()
	createdAt := gjson.GetBytes(compactData, "created_at").Int()
	if createdAt == 0 {
		createdAt = now
	}
	completedAt := gjson.GetBytes(compactData, "completed_at").Int()
	if completedAt == 0 {
		completedAt = now
	}
	return createdAt, completedAt
}

func codexMarshalRawMessages(items [][]byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(bytes.TrimSpace(item))
	}
	buf.WriteByte(']')
	return buf.Bytes()
}
