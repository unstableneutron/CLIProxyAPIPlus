package executor

import (
	"bytes"
	"context"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/sjson"
)

func codexExecutorPreservePreviousResponseID(opts cliproxyexecutor.Options) bool {
	if len(opts.Metadata) == 0 {
		return false
	}
	raw, ok := opts.Metadata[cliproxyexecutor.ResponsesStateModeMetadataKey]
	if !ok || raw == nil {
		return false
	}
	mode := ""
	switch v := raw.(type) {
	case string:
		mode = strings.TrimSpace(v)
	case []byte:
		mode = strings.TrimSpace(string(v))
	default:
		return false
	}
	switch strings.ToLower(mode) {
	case cliproxyexecutor.ResponsesStateModeProbe, cliproxyexecutor.ResponsesStateModeStateful:
		return true
	default:
		return false
	}
}

func (e *CodexExecutor) executeCompactionTriggerStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	compactPayload, err := buildCodexCompactionTriggerPayload(req.Payload)
	if err != nil {
		return nil, err
	}
	compactReq := req
	compactReq.Payload = compactPayload
	compactOpts := opts
	compactOpts.Stream = false
	compactOpts.Alt = "responses/compact"
	compactOpts.ResponseFormat = sdktranslator.FromString("openai-response")

	resp, err := e.executeCompactWithEncryptedContentFallback(ctx, auth, compactReq, compactOpts)
	if err != nil {
		return nil, err
	}
	responseID := codexCompactionResponseID(resp.Payload)
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
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, nil
}

func buildCodexCompactionTriggerPayload(payload []byte) ([]byte, error) {
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	out := xaiRemoveInputItemsByType(bytes.Clone(payload), "compaction_trigger")
	out, _ = sjson.DeleteBytes(out, "previous_response_id")
	out, _ = sjson.DeleteBytes(out, "type")
	out, _ = sjson.DeleteBytes(out, "generate")
	out, _ = sjson.DeleteBytes(out, "stream")
	out, _ = sjson.DeleteBytes(out, "stream_options")
	out, _ = sjson.DeleteBytes(out, "store")
	out, _ = sjson.DeleteBytes(out, "include")
	out = sanitizeCodexWebsocketCompactionReplayPayload(out)
	return out, nil
}

func (e *CodexExecutor) executeCompactWithEncryptedContentFallback(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	resp, err := e.executeCompact(ctx, auth, req, opts)
	if err == nil || !codexCompactShouldRetryWithoutEncryptedContent(err) {
		return resp, err
	}

	strippedPayload := sanitizeCodexWebsocketCompactionReplayPayloadWithOptions(req.Payload, codexWebsocketCompactionReplaySanitizeOptions{StripEncryptedContent: true})
	if bytes.Equal(bytes.TrimSpace(strippedPayload), bytes.TrimSpace(req.Payload)) {
		return resp, err
	}

	helps.LogWithRequestID(ctx).Debugf("codex executor: retrying compact request without encrypted_content after upstream rejected encrypted_content: %v", err)
	retryReq := req
	retryReq.Payload = strippedPayload
	return e.executeCompact(ctx, auth, retryReq, opts)
}

func codexCompactShouldRetryWithoutEncryptedContent(err error) bool {
	if err == nil {
		return false
	}
	statusCode := 0
	if statusErr, ok := err.(interface{ StatusCode() int }); ok {
		statusCode = statusErr.StatusCode()
	}
	if statusCode != 0 && statusCode != http.StatusBadRequest && statusCode != http.StatusUnprocessableEntity {
		return false
	}
	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "encrypted_content") {
		return false
	}
	if strings.Contains(message, "missing required parameter") || strings.Contains(message, "required parameter") || strings.Contains(message, "missing") {
		return false
	}
	for _, marker := range []string{
		"unknown",
		"unrecognized",
		"unsupported",
		"not supported",
		"extra",
		"unexpected",
		"invalid",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return statusCode == http.StatusBadRequest || statusCode == http.StatusUnprocessableEntity
}
