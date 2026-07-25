package openai

import (
	codexmodels "github.com/router-for-me/CLIProxyAPI/v7/internal/client/codex/models"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

type codexClientModelProvidersFunc = codexmodels.ProvidersForModelFunc

func (h *OpenAIAPIHandler) codexClientModelsResponse() map[string]any {
	optimizeMultiAgentV2 := h != nil && h.Cfg != nil && h.Cfg.CodexOptimizeMultiAgentV2
	return codexClientModelsResponse(h.Models(), registry.GetGlobalRegistry().GetModelProviders, optimizeMultiAgentV2)
}

// CodexClientModelsResponse builds a Codex client model response.
func CodexClientModelsResponse(models []map[string]any) map[string]any {
	return codexClientModelsResponse(models, nil, false)
}

// CodexClientModelsResponseWithMultiAgentV2 builds a Codex client model response
// and advertises multi-agent v2 for synthesized models when enabled.
func CodexClientModelsResponseWithMultiAgentV2(models []map[string]any, enabled bool) map[string]any {
	return codexClientModelsResponse(models, nil, enabled)
}

func codexClientModelsResponse(models []map[string]any, providersForModel codexClientModelProvidersFunc, optimizeMultiAgentV2 bool) map[string]any {
	return codexmodels.BuildResponse(models, providersForModel, optimizeMultiAgentV2)
}

func loadCodexClientModelTemplates() (map[string]map[string]any, map[string]any, error) {
	return codexmodels.LoadTemplates()
}
