package registry

import "testing"

func TestGetCommandCodeModelsIncludesReferenceCatalog(t *testing.T) {
	models := GetCommandCodeModels()
	if len(models) != 18 {
		t.Fatalf("expected 18 CommandCode models, got %d", len(models))
	}

	var flash *ModelInfo
	for _, model := range models {
		if model != nil && model.ID == "deepseek/deepseek-v4-flash" {
			flash = model
			break
		}
	}
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
	if got := GetStaticModelDefinitionsByChannel("commandcode"); len(got) != 18 {
		t.Fatalf("commandcode static definitions = %d, want 18", len(got))
	}
	if info := LookupStaticModelInfo("deepseek/deepseek-v4-flash"); info == nil || info.Type != "commandcode" {
		t.Fatalf("LookupStaticModelInfo deepseek flash = %+v", info)
	}
}
