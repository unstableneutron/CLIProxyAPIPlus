package cliproxy

import (
	"strings"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func (s *Service) resolveConfigCommandCodeKey(auth *coreauth.Auth) *config.CommandCodeKey {
	if auth == nil || s == nil || s.cfg == nil {
		return nil
	}
	if entry := configEntryForAuthIndex(auth, s.cfg.CommandCodeKey); entry != nil {
		return entry
	}
	var attrKey, attrBase string
	if auth.Attributes != nil {
		attrKey = strings.TrimSpace(auth.Attributes["api_key"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.CommandCodeKey {
		entry := &s.cfg.CommandCodeKey[i]
		cfgKey := strings.TrimSpace(entry.APIKey)
		cfgBase := strings.TrimSpace(entry.BaseURL)
		if attrKey != "" && strings.EqualFold(cfgKey, attrKey) {
			if cfgBase == "" || strings.EqualFold(cfgBase, attrBase) {
				return entry
			}
			continue
		}
		if attrKey == "" && attrBase != "" && strings.EqualFold(cfgBase, attrBase) {
			return entry
		}
	}
	return nil
}

func (s *Service) resolveConfigBedrockProvider(auth *coreauth.Auth) *config.BedrockProvider {
	if auth == nil || s == nil || s.cfg == nil {
		return nil
	}
	if entry := configEntryForAuthIndex(auth, s.cfg.Bedrock); entry != nil {
		return entry
	}
	var attrName, attrBase string
	if auth.Attributes != nil {
		attrName = strings.TrimSpace(auth.Attributes["bedrock_name"])
		attrBase = strings.TrimSpace(auth.Attributes["base_url"])
	}
	for i := range s.cfg.Bedrock {
		entry := &s.cfg.Bedrock[i]
		if attrName != "" && strings.EqualFold(strings.TrimSpace(entry.Name), attrName) {
			return entry
		}
		if attrBase != "" && strings.EqualFold(strings.TrimSpace(entry.BaseURL), attrBase) {
			return entry
		}
	}
	return nil
}

type commandCodeModelEntry struct {
	config.CommandCodeModel
}

func (commandCodeModelEntry) GetThinking() *registry.ThinkingSupport {
	return nil
}

func buildCommandCodeConfigModels(entry *config.CommandCodeKey) []*ModelInfo {
	if entry == nil {
		return nil
	}
	models := make([]commandCodeModelEntry, len(entry.Models))
	for i := range entry.Models {
		models[i] = commandCodeModelEntry{CommandCodeModel: entry.Models[i]}
	}
	return buildConfigModels(models, "commandcode", "commandcode")
}

func buildBedrockConfigModels(entry *config.BedrockProvider) []*ModelInfo {
	if entry == nil {
		return nil
	}
	models := buildConfigModels(entry.Models, "aws-bedrock", "bedrock")
	thinkingByAlias := make(map[string]*registry.ThinkingSupport, len(entry.Models)*2)
	for i := range entry.Models {
		model := entry.Models[i]
		if model.Thinking == nil {
			continue
		}
		thinkingByAlias[strings.TrimSpace(model.Name)] = model.Thinking
		thinkingByAlias[strings.TrimSpace(model.Alias)] = model.Thinking
	}
	for _, model := range models {
		if model != nil && thinkingByAlias[model.ID] != nil {
			model.Thinking = thinkingByAlias[model.ID]
		}
	}
	return models
}

// filterAgenticVariants removes Kiro agentic model variants when prompt injection is disabled.
func filterAgenticVariants(models []*ModelInfo) []*ModelInfo {
	result := make([]*ModelInfo, 0, len(models))
	for _, model := range models {
		if model != nil && strings.HasSuffix(model.ID, "-agentic") {
			continue
		}
		result = append(result, model)
	}
	return result
}

func (s *Service) extractKiroTokenData(auth *coreauth.Auth) *kiroauth.KiroTokenData {
	return extractKiroTokenData(auth)
}
