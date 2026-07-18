package responses

import (
	"context"

	clauderesponses "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/claude/openai/responses"
)

func ConvertOpenAIResponsesRequestToKiro(modelName string, inputRawJSON []byte, stream bool) []byte {
	return clauderesponses.ConvertOpenAIResponsesRequestToClaude(modelName, inputRawJSON, stream)
}

func ConvertKiroStreamToOpenAIResponses(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) [][]byte {
	return clauderesponses.ConvertClaudeResponseToOpenAIResponses(ctx, modelName, originalRequestRawJSON, requestRawJSON, rawJSON, param)
}

func ConvertKiroNonStreamToOpenAIResponses(ctx context.Context, modelName string, originalRequestRawJSON, requestRawJSON, rawJSON []byte, param *any) []byte {
	return clauderesponses.ConvertClaudeResponseToOpenAIResponsesNonStream(ctx, modelName, originalRequestRawJSON, requestRawJSON, rawJSON, param)
}
