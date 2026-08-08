package registry

// commandCodeBuiltinModelInfosV115 is the static fallback for Command Code 1.15.0.
// Source: the published command-code@1.15.0 bundle (parent-package/cli.pretty.mjs,
// model registry lines 73114-73751 and getSupportedEfforts lines 56357-56385),
// cross-checked against the captured public /provider/v1/models response.
func commandCodeBuiltinModelInfosV115() []*ModelInfo {
	return []*ModelInfo{
		commandCodeModelInfoV115("claude-sonnet-5", "Claude Sonnet 5 (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high", "xhigh", "max"}),
		commandCodeModelInfoV115("claude-sonnet-4-6", "Claude Sonnet 4.6 (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high", "xhigh", "max"}),
		commandCodeModelInfoV115("claude-fable-5", "Claude Fable 5 (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high", "xhigh", "max"}),
		commandCodeModelInfoV115("claude-opus-5", "Claude Opus 5 (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high", "xhigh", "max"}),
		commandCodeModelInfoV115("claude-opus-4-8", "Claude Opus 4.8 (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high", "xhigh", "max"}),
		commandCodeModelInfoV115("claude-opus-4-7", "Claude Opus 4.7 (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high", "xhigh", "max"}),
		commandCodeModelInfoV115("claude-haiku-4-5-20251001", "Claude Haiku 4.5 (CC)", 200000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("gpt-5.6-sol", "GPT-5.6 Sol (CC)", 1050000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high", "xhigh", "max"}),
		commandCodeModelInfoV115("gpt-5.6-terra", "GPT-5.6 Terra (CC)", 1050000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high", "xhigh", "max"}),
		commandCodeModelInfoV115("gpt-5.6-luna", "GPT-5.6 Luna (CC)", 1050000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high", "xhigh", "max"}),
		commandCodeModelInfoV115("gpt-5.5", "GPT-5.5 (CC)", 200000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high", "xhigh"}),
		commandCodeModelInfoV115("gpt-5.4", "GPT-5.4 (CC)", 400000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high", "xhigh"}),
		commandCodeModelInfoV115("gpt-5.3-codex", "GPT-5.3 Codex (CC)", 400000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high", "xhigh"}),
		commandCodeModelInfoV115("gpt-5.4-mini", "GPT-5.4 Mini (CC)", 400000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high"}),
		commandCodeModelInfoV115("deepseek/deepseek-v4-pro", "DeepSeek V4 Pro (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text"}, []string{"high", "max"}),
		commandCodeModelInfoV115("deepseek/deepseek-v4-flash", "DeepSeek V4 Flash (latest) (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text"}, []string{"high", "max"}),
		commandCodeModelInfoV115("moonshotai/Kimi-K3", "Kimi K3 (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("moonshotai/Kimi-K2.7-Code", "Kimi K2.7 Code (CC)", 256000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("moonshotai/Kimi-K2.7-Code-Highspeed", "Kimi K2.7 Code HighSpeed (CC)", 262000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("moonshotai/Kimi-K2.6", "Kimi K2.6 (CC)", 256000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("moonshotai/Kimi-K2.5", "Kimi K2.5 (CC)", 256000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("zai-org/GLM-5.2", "GLM-5.2 (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text"}, []string{"high", "max"}),
		commandCodeModelInfoV115("zai-org/GLM-5.2-Fast", "GLM-5.2 Fast (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text"}, nil),
		commandCodeModelInfoV115("zai-org/GLM-5.1", "GLM-5.1 (CC)", 200000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text"}, nil),
		commandCodeModelInfoV115("zai-org/GLM-5", "GLM-5 (CC)", 200000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text"}, nil),
		commandCodeModelInfoV115("MiniMaxAI/MiniMax-M3", "MiniMax M3 (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("MiniMaxAI/MiniMax-M2.7", "MiniMax M2.7 (CC)", 200000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text"}, nil),
		commandCodeModelInfoV115("MiniMaxAI/MiniMax-M2.5", "MiniMax M2.5 (CC)", 200000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text"}, nil),
		commandCodeModelInfoV115("xiaomi/mimo-v2.5-pro", "MiMo V2.5 Pro (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text"}, nil),
		commandCodeModelInfoV115("xiaomi/mimo-v2.5", "MiMo V2.5 (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("Qwen/Qwen3.8-Max", "Qwen 3.8 Max (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "xhigh"}),
		commandCodeModelInfoV115("Qwen/Qwen3.7-Max", "Qwen 3.7 Max (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text"}, nil),
		commandCodeModelInfoV115("Qwen/Qwen3.7-Plus", "Qwen 3.7 Plus (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("Qwen/Qwen3.7-Flash", "Qwen 3.7 Flash (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("Qwen/Qwen3.6-Max-Preview", "Qwen 3.6 Max Preview (CC)", 200000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text"}, nil),
		commandCodeModelInfoV115("Qwen/Qwen3.6-Plus", "Qwen 3.6 Plus (CC)", 200000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("stepfun/Step-3.7-Flash", "Step 3.7 Flash (CC)", 256000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("stepfun/Step-3.5-Flash", "Step 3.5 Flash (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text"}, nil),
		commandCodeModelInfoV115("tencent/hy3-paid", "Tencent Hy3 (CC)", 262144, commandCodeBuiltinDefaultMaxOutputV115, []string{"text"}, nil),
		commandCodeModelInfoV115("google/gemini-3.6-flash", "Gemini 3.6 Flash (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high"}),
		commandCodeModelInfoV115("google/gemini-3.5-flash", "Gemini 3.5 Flash (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high"}),
		commandCodeModelInfoV115("google/gemini-3.5-flash-lite", "Gemini 3.5 Flash Lite (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high"}),
		commandCodeModelInfoV115("google/gemini-3.1-flash-lite", "Gemini 3.1 Flash Lite (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high"}),
		commandCodeModelInfoV115("sakana/fugu-ultra", "Fugu Ultra (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"high", "xhigh"}),
		commandCodeModelInfoV115("nvidia/nemotron-3-ultra-550b-a55b", "Nemotron 3 Ultra (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text"}, nil),
		commandCodeModelInfoV115("thinkingmachines/inkling", "Inkling (CC)", 256000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("thinkingmachines/inkling-small", "Inkling Small (CC)", 1000000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("poolside/laguna-s-2.1-free", "Laguna S 2.1 (CC)", 256000, 32768, []string{"text"}, nil),
		commandCodeModelInfoV115("meta/muse-spark-1.1", "Muse Spark 1.1 (CC)", 1048576, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("meta/muse-spark-1.2", "Muse Spark 1.2 (CC)", 1048576, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("meta/muse-spark-1.2-contributor", "Muse Spark 1.2 Contributor (CC)", 1048576, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, nil),
		commandCodeModelInfoV115("xai/grok-4.5", "Grok 4.5 (CC)", 500000, commandCodeBuiltinDefaultMaxOutputV115, []string{"text", "image"}, []string{"low", "medium", "high"}),
	}
}

const (
	commandCodeBuiltinCreatedV115          int64 = 1786168184
	commandCodeBuiltinDefaultMaxOutputV115       = 64000
	commandCodeBuiltinProviderV115               = "command-code"
	commandCodeBuiltinTypeV115                   = "commandcode"
)

func commandCodeModelInfoV115(id, displayName string, contextLength, maxCompletionTokens int, inputModalities, thinkingLevels []string) *ModelInfo {
	info := &ModelInfo{
		ID:                        id,
		Object:                    "model",
		Created:                   commandCodeBuiltinCreatedV115,
		OwnedBy:                   commandCodeBuiltinProviderV115,
		Type:                      commandCodeBuiltinTypeV115,
		DisplayName:               displayName,
		Version:                   id,
		ContextLength:             contextLength,
		MaxCompletionTokens:       maxCompletionTokens,
		SupportedParameters:       []string{"tools"},
		SupportedEndpoints:        []string{"/v1/chat/completions", "/v1/responses"},
		SupportedInputModalities:  append([]string(nil), inputModalities...),
		SupportedOutputModalities: []string{"text"},
	}
	if len(thinkingLevels) > 0 {
		info.Thinking = &ThinkingSupport{Levels: append([]string(nil), thinkingLevels...)}
	}
	return info
}
