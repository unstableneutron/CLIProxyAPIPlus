package registry

import (
	"reflect"
	"testing"
)

func TestGetCommandCodeModelsMatchesPublished115Catalog(t *testing.T) {
	type expectedModel struct {
		id, displayName                    string
		contextLength, maxCompletionTokens int
		inputModalities, thinkingLevels    []string
	}
	want := []expectedModel{
		{id: "claude-sonnet-5", displayName: "Claude Sonnet 5 (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}},
		{id: "claude-sonnet-4-6", displayName: "Claude Sonnet 4.6 (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}},
		{id: "claude-fable-5", displayName: "Claude Fable 5 (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}},
		{id: "claude-opus-5", displayName: "Claude Opus 5 (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}},
		{id: "claude-opus-4-8", displayName: "Claude Opus 4.8 (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}},
		{id: "claude-opus-4-7", displayName: "Claude Opus 4.7 (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}},
		{id: "claude-haiku-4-5-20251001", displayName: "Claude Haiku 4.5 (CC)", contextLength: 200000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "gpt-5.6-sol", displayName: "GPT-5.6 Sol (CC)", contextLength: 1050000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}},
		{id: "gpt-5.6-terra", displayName: "GPT-5.6 Terra (CC)", contextLength: 1050000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}},
		{id: "gpt-5.6-luna", displayName: "GPT-5.6 Luna (CC)", contextLength: 1050000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high", "xhigh", "max"}},
		{id: "gpt-5.5", displayName: "GPT-5.5 (CC)", contextLength: 200000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high", "xhigh"}},
		{id: "gpt-5.4", displayName: "GPT-5.4 (CC)", contextLength: 400000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high", "xhigh"}},
		{id: "gpt-5.3-codex", displayName: "GPT-5.3 Codex (CC)", contextLength: 400000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high", "xhigh"}},
		{id: "gpt-5.4-mini", displayName: "GPT-5.4 Mini (CC)", contextLength: 400000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high"}},
		{id: "deepseek/deepseek-v4-pro", displayName: "DeepSeek V4 Pro (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text"}, thinkingLevels: []string{"high", "max"}},
		{id: "deepseek/deepseek-v4-flash", displayName: "DeepSeek V4 Flash (latest) (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text"}, thinkingLevels: []string{"high", "max"}},
		{id: "moonshotai/Kimi-K3", displayName: "Kimi K3 (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "moonshotai/Kimi-K2.7-Code", displayName: "Kimi K2.7 Code (CC)", contextLength: 256000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "moonshotai/Kimi-K2.7-Code-Highspeed", displayName: "Kimi K2.7 Code HighSpeed (CC)", contextLength: 262000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "moonshotai/Kimi-K2.6", displayName: "Kimi K2.6 (CC)", contextLength: 256000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "moonshotai/Kimi-K2.5", displayName: "Kimi K2.5 (CC)", contextLength: 256000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "zai-org/GLM-5.2", displayName: "GLM-5.2 (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text"}, thinkingLevels: []string{"high", "max"}},
		{id: "zai-org/GLM-5.2-Fast", displayName: "GLM-5.2 Fast (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text"}, thinkingLevels: nil},
		{id: "zai-org/GLM-5.1", displayName: "GLM-5.1 (CC)", contextLength: 200000, maxCompletionTokens: 64000, inputModalities: []string{"text"}, thinkingLevels: nil},
		{id: "zai-org/GLM-5", displayName: "GLM-5 (CC)", contextLength: 200000, maxCompletionTokens: 64000, inputModalities: []string{"text"}, thinkingLevels: nil},
		{id: "MiniMaxAI/MiniMax-M3", displayName: "MiniMax M3 (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "MiniMaxAI/MiniMax-M2.7", displayName: "MiniMax M2.7 (CC)", contextLength: 200000, maxCompletionTokens: 64000, inputModalities: []string{"text"}, thinkingLevels: nil},
		{id: "MiniMaxAI/MiniMax-M2.5", displayName: "MiniMax M2.5 (CC)", contextLength: 200000, maxCompletionTokens: 64000, inputModalities: []string{"text"}, thinkingLevels: nil},
		{id: "xiaomi/mimo-v2.5-pro", displayName: "MiMo V2.5 Pro (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text"}, thinkingLevels: nil},
		{id: "xiaomi/mimo-v2.5", displayName: "MiMo V2.5 (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "Qwen/Qwen3.8-Max", displayName: "Qwen 3.8 Max (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "xhigh"}},
		{id: "Qwen/Qwen3.7-Max", displayName: "Qwen 3.7 Max (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text"}, thinkingLevels: nil},
		{id: "Qwen/Qwen3.7-Plus", displayName: "Qwen 3.7 Plus (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "Qwen/Qwen3.7-Flash", displayName: "Qwen 3.7 Flash (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "Qwen/Qwen3.6-Max-Preview", displayName: "Qwen 3.6 Max Preview (CC)", contextLength: 200000, maxCompletionTokens: 64000, inputModalities: []string{"text"}, thinkingLevels: nil},
		{id: "Qwen/Qwen3.6-Plus", displayName: "Qwen 3.6 Plus (CC)", contextLength: 200000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "stepfun/Step-3.7-Flash", displayName: "Step 3.7 Flash (CC)", contextLength: 256000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "stepfun/Step-3.5-Flash", displayName: "Step 3.5 Flash (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text"}, thinkingLevels: nil},
		{id: "tencent/hy3-paid", displayName: "Tencent Hy3 (CC)", contextLength: 262144, maxCompletionTokens: 64000, inputModalities: []string{"text"}, thinkingLevels: nil},
		{id: "google/gemini-3.6-flash", displayName: "Gemini 3.6 Flash (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high"}},
		{id: "google/gemini-3.5-flash", displayName: "Gemini 3.5 Flash (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high"}},
		{id: "google/gemini-3.5-flash-lite", displayName: "Gemini 3.5 Flash Lite (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high"}},
		{id: "google/gemini-3.1-flash-lite", displayName: "Gemini 3.1 Flash Lite (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high"}},
		{id: "sakana/fugu-ultra", displayName: "Fugu Ultra (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"high", "xhigh"}},
		{id: "nvidia/nemotron-3-ultra-550b-a55b", displayName: "Nemotron 3 Ultra (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text"}, thinkingLevels: nil},
		{id: "thinkingmachines/inkling", displayName: "Inkling (CC)", contextLength: 256000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "thinkingmachines/inkling-small", displayName: "Inkling Small (CC)", contextLength: 1000000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "poolside/laguna-s-2.1-free", displayName: "Laguna S 2.1 (CC)", contextLength: 256000, maxCompletionTokens: 32768, inputModalities: []string{"text"}, thinkingLevels: nil},
		{id: "meta/muse-spark-1.1", displayName: "Muse Spark 1.1 (CC)", contextLength: 1048576, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "meta/muse-spark-1.2", displayName: "Muse Spark 1.2 (CC)", contextLength: 1048576, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "meta/muse-spark-1.2-contributor", displayName: "Muse Spark 1.2 Contributor (CC)", contextLength: 1048576, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: nil},
		{id: "xai/grok-4.5", displayName: "Grok 4.5 (CC)", contextLength: 500000, maxCompletionTokens: 64000, inputModalities: []string{"text", "image"}, thinkingLevels: []string{"low", "medium", "high"}},
	}

	models := GetCommandCodeModels()
	if len(models) != len(want) {
		t.Fatalf("CommandCode model count = %d, want %d", len(models), len(want))
	}
	byID := make(map[string]*ModelInfo, len(models))
	for _, model := range models {
		if model == nil {
			t.Fatal("CommandCode catalog contains nil model")
		}
		if _, exists := byID[model.ID]; exists {
			t.Fatalf("duplicate CommandCode model ID %q", model.ID)
		}
		byID[model.ID] = model
	}

	for _, expected := range want {
		model := byID[expected.id]
		if model == nil {
			t.Fatalf("missing published CommandCode model %q", expected.id)
		}
		if model.DisplayName != expected.displayName || model.ContextLength != expected.contextLength || model.MaxCompletionTokens != expected.maxCompletionTokens {
			t.Fatalf("model %q metadata = display=%q context=%d max=%d, want display=%q context=%d max=%d", expected.id, model.DisplayName, model.ContextLength, model.MaxCompletionTokens, expected.displayName, expected.contextLength, expected.maxCompletionTokens)
		}
		if model.Created != commandCodeBuiltinCreatedV115 || model.OwnedBy != commandCodeBuiltinProviderV115 || model.Type != commandCodeBuiltinTypeV115 {
			t.Fatalf("model %q identity metadata = created=%d owned_by=%q type=%q", expected.id, model.Created, model.OwnedBy, model.Type)
		}
		if !reflect.DeepEqual(model.SupportedParameters, []string{"tools"}) || !reflect.DeepEqual(model.SupportedEndpoints, []string{"/v1/chat/completions", "/v1/responses"}) || !reflect.DeepEqual(model.SupportedOutputModalities, []string{"text"}) {
			t.Fatalf("model %q protocol metadata = parameters=%v endpoints=%v output=%v", expected.id, model.SupportedParameters, model.SupportedEndpoints, model.SupportedOutputModalities)
		}
		if !reflect.DeepEqual(model.SupportedInputModalities, expected.inputModalities) {
			t.Fatalf("model %q input modalities = %v, want %v", expected.id, model.SupportedInputModalities, expected.inputModalities)
		}
		var gotThinking []string
		if model.Thinking != nil {
			gotThinking = model.Thinking.Levels
		}
		if !reflect.DeepEqual(gotThinking, expected.thinkingLevels) {
			t.Fatalf("model %q thinking levels = %v, want %v", expected.id, gotThinking, expected.thinkingLevels)
		}
	}

	if byID["Qwen/Qwen3.7-Max-Free"] != nil {
		t.Fatal("stale CommandCode model Qwen/Qwen3.7-Max-Free should not be in the static catalog")
	}
}

func TestGetStaticModelDefinitionsByChannelCommandCode(t *testing.T) {
	if got := GetStaticModelDefinitionsByChannel("commandcode"); len(got) != 52 {
		t.Fatalf("commandcode static definitions = %d, want 52", len(got))
	}
	if info := LookupStaticModelInfo("deepseek/deepseek-v4-flash"); info == nil || info.Type != "commandcode" {
		t.Fatalf("LookupStaticModelInfo deepseek flash = %+v", info)
	}
	if info := LookupStaticModelInfo("xiaomi/mimo-v2.5-pro"); info == nil || info.Type != "commandcode" {
		t.Fatalf("LookupStaticModelInfo MiMo Pro = %+v", info)
	}
}
