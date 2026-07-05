package registry

import "testing"

func TestGetCommandCodeModelsIncludesReferenceCatalog(t *testing.T) {
	models := GetCommandCodeModels()
	if len(models) != 35 {
		t.Fatalf("expected 35 CommandCode models, got %d", len(models))
	}

	byID := make(map[string]*ModelInfo, len(models))
	for _, model := range models {
		if model != nil {
			byID[model.ID] = model
		}
	}

	required := []string{
		"deepseek/deepseek-v4-flash",
		"deepseek/deepseek-v4-pro",
		"moonshotai/Kimi-K2.7-Code",
		"moonshotai/Kimi-K2.7-Code-Highspeed",
		"zai-org/GLM-5.2",
		"zai-org/GLM-5.2-Fast",
		"MiniMaxAI/MiniMax-M3",
		"xiaomi/mimo-v2.5",
		"xiaomi/mimo-v2.5-pro",
		"stepfun/Step-3.5-Flash",
		"stepfun/Step-3.7-Flash",
		"nvidia/nemotron-3-ultra-550b-a55b",
		"claude-sonnet-5",
		"claude-fable-5",
		"sakana/fugu-ultra",
	}
	for _, id := range required {
		if byID[id] == nil {
			t.Fatalf("expected current CommandCode model %q", id)
		}
	}
	if byID["Qwen/Qwen3.7-Max-Free"] != nil {
		t.Fatal("stale CommandCode model Qwen/Qwen3.7-Max-Free should not be in the static catalog")
	}

	flash := byID["deepseek/deepseek-v4-flash"]
	if flash == nil {
		t.Fatal("expected deepseek/deepseek-v4-flash model")
	}
	if flash.Type != "commandcode" {
		t.Fatalf("type = %q", flash.Type)
	}
	if flash.ContextLength != 1000000 {
		t.Fatalf("context_length = %d", flash.ContextLength)
	}
	if flash.MaxCompletionTokens != 384000 {
		t.Fatalf("max_completion_tokens = %d", flash.MaxCompletionTokens)
	}
	if flash.Thinking == nil || len(flash.Thinking.Levels) == 0 {
		t.Fatalf("expected thinking levels, got %+v", flash.Thinking)
	}
}

func TestGetStaticModelDefinitionsByChannelCommandCode(t *testing.T) {
	if got := GetStaticModelDefinitionsByChannel("commandcode"); len(got) != 35 {
		t.Fatalf("commandcode static definitions = %d, want 35", len(got))
	}
	if info := LookupStaticModelInfo("deepseek/deepseek-v4-flash"); info == nil || info.Type != "commandcode" {
		t.Fatalf("LookupStaticModelInfo deepseek flash = %+v", info)
	}
	if info := LookupStaticModelInfo("xiaomi/mimo-v2.5-pro"); info == nil || info.Type != "commandcode" {
		t.Fatalf("LookupStaticModelInfo MiMo Pro = %+v", info)
	}
}
