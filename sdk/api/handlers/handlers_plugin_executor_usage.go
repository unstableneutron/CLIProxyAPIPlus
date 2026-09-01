package handlers

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

func parsePluginExecutorResponseUsage(protocol string, payload []byte) usage.Detail {
	return helps.ParsePluginExecutorResponseUsage(protocol, payload)
}

func observePluginExecutorStreamUsage(protocol string, payload []byte, buffer *helps.StreamUsageBuffer) {
	helps.ObservePluginExecutorStreamUsage(protocol, payload, buffer)
}

func parseClaudePayloadUsage(payload []byte) usage.Detail {
	return helps.ParsePluginExecutorResponseUsage("claude", payload)
}

func parseClaudeStreamLine(line []byte) (usage.Detail, bool) {
	var buffer helps.StreamUsageBuffer
	helps.ObservePluginExecutorStreamUsage("claude", line, &buffer)
	return buffer.Detail()
}

func observeMergedStreamUsage(buffer *helps.StreamUsageBuffer, update usage.Detail) {
	helps.ObserveMergedStreamUsage(buffer, update)
}

func mergeStreamUsageDetail(existing, update usage.Detail) usage.Detail {
	return helps.MergeStreamUsageDetail(existing, update)
}

func iterateStreamLines(payload []byte, fn func(line []byte)) {
	helps.IterateStreamLines(payload, fn)
}

func extractStreamJSONPayload(line []byte) []byte {
	return helps.ExtractStreamJSONPayload(line)
}
