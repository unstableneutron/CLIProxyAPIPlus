package synthesizer

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	kiroauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/kiro"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/diff"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// ConfigSynthesizer generates Auth entries from configuration API keys.
// It handles Gemini, Claude, Codex, CommandCode, OpenAI-compat, and Vertex-compat providers.
type ConfigSynthesizer struct{}

func configLabel(label, fallback string) string {
	trimmed := strings.TrimSpace(label)
	if trimmed != "" {
		return trimmed
	}
	return fallback
}

// NewConfigSynthesizer creates a new ConfigSynthesizer instance.
func NewConfigSynthesizer() *ConfigSynthesizer {
	return &ConfigSynthesizer{}
}

// Synthesize generates Auth entries from config API keys.
func (s *ConfigSynthesizer) Synthesize(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, 32)
	if ctx == nil || ctx.Config == nil {
		return out, nil
	}

	// Gemini API Keys
	out = append(out, s.synthesizeGeminiKeys(ctx)...)
	// Claude API Keys
	out = append(out, s.synthesizeClaudeKeys(ctx)...)
	// Codex API Keys
	out = append(out, s.synthesizeCodexKeys(ctx)...)
	// Kiro (AWS CodeWhisperer)
	out = append(out, s.synthesizeKiroKeys(ctx)...)
	// Command Code API Keys
	out = append(out, s.synthesizeCommandCodeKeys(ctx)...)
	// OpenAI-compat
	out = append(out, s.synthesizeOpenAICompat(ctx)...)
	out = append(out, s.synthesizeBedrock(ctx)...)
	// Vertex-compat
	out = append(out, s.synthesizeVertexCompat(ctx)...)

	return out, nil
}

// synthesizeGeminiKeys creates Auth entries for Gemini API keys.
func (s *ConfigSynthesizer) synthesizeGeminiKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.GeminiKey))
	for i := range cfg.GeminiKey {
		entry := cfg.GeminiKey[i]
		key := strings.TrimSpace(entry.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(entry.Prefix)
		base := strings.TrimSpace(entry.BaseURL)
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		id, token := idGen.Next("gemini:apikey", key, base)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:gemini[%s]", token),
			"api_key": key,
		}
		metadata := map[string]any{}
		if entry.DisableCooling {
			metadata["disable_cooling"] = true
		}
		if entry.Priority != 0 {
			attrs["priority"] = strconv.Itoa(entry.Priority)
		}
		if base != "" {
			attrs["base_url"] = base
		}
		if hash := diff.ComputeGeminiModelsHash(entry.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(entry.Headers, attrs)
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "gemini",
			Label:      configLabel(entry.Label, "gemini-apikey"),
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, entry.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		out = append(out, a)
	}
	return out
}

// synthesizeClaudeKeys creates Auth entries for Claude API keys.
func (s *ConfigSynthesizer) synthesizeClaudeKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.ClaudeKey))
	for i := range cfg.ClaudeKey {
		ck := cfg.ClaudeKey[i]
		key := strings.TrimSpace(ck.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(ck.Prefix)
		base := strings.TrimSpace(ck.BaseURL)
		id, token := idGen.Next("claude:apikey", key, base)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:claude[%s]", token),
			"api_key": key,
		}
		metadata := map[string]any{}
		if ck.DisableCooling {
			metadata["disable_cooling"] = true
		}
		if ck.Priority != 0 {
			attrs["priority"] = strconv.Itoa(ck.Priority)
		}
		if base != "" {
			attrs["base_url"] = base
		}
		if ck.RebuildMidSystemMessage {
			attrs["rebuild_mid_system_message"] = "true"
		}
		if hash := diff.ComputeClaudeModelsHash(ck.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(ck.Headers, attrs)
		proxyURL := strings.TrimSpace(ck.ProxyURL)
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "claude",
			Label:      configLabel(ck.Label, "claude-apikey"),
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, ck.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		out = append(out, a)
	}
	return out
}

// synthesizeCodexKeys creates Auth entries for Codex API keys.
func (s *ConfigSynthesizer) synthesizeCodexKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.CodexKey))
	for i := range cfg.CodexKey {
		ck := cfg.CodexKey[i]
		key := strings.TrimSpace(ck.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(ck.Prefix)
		id, token := idGen.Next("codex:apikey", key, ck.BaseURL)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:codex[%s]", token),
			"api_key": key,
		}
		metadata := map[string]any{}
		if ck.DisableCooling {
			metadata["disable_cooling"] = true
		}
		if ck.Priority != 0 {
			attrs["priority"] = strconv.Itoa(ck.Priority)
		}
		if ck.BaseURL != "" {
			attrs["base_url"] = ck.BaseURL
		}
		if ck.Websockets {
			attrs["websockets"] = "true"
		}
		if responsesState := strings.TrimSpace(string(ck.ResponsesState)); responsesState != "" {
			attrs["responses_state"] = responsesState
			if modelsAttr := codexResponsesStateModelsAttribute(prefix, ck.Models); modelsAttr != "" {
				attrs["responses_state_models"] = modelsAttr
			}
		}
		if hash := diff.ComputeCodexModelsHash(ck.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(ck.Headers, attrs)
		addConfigQueryParamsToAttrs(ck.QueryParams, attrs)
		proxyURL := strings.TrimSpace(ck.ProxyURL)
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "codex",
			Label:      configLabel(ck.Label, "codex-apikey"),
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, ck.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		out = append(out, a)
	}
	return out
}

func codexResponsesStateModelsAttribute(prefix string, models []config.CodexModel) string {
	seen := make(map[string]struct{}, len(models)*4)
	values := make([]string, 0, len(models)*4)
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		if _, ok := seen[value]; ok {
			return
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	for _, model := range models {
		name := strings.TrimSpace(model.Name)
		alias := strings.TrimSpace(model.Alias)
		add(name)
		add(alias)
		if prefix != "" {
			add(prefix + "/" + name)
			add(prefix + "/" + alias)
		}
	}
	if len(values) == 0 {
		return ""
	}
	data, err := json.Marshal(values)
	if err != nil {
		return ""
	}
	return string(data)
}

// synthesizeCommandCodeKeys creates Auth entries for Command Code API keys.
func (s *ConfigSynthesizer) synthesizeCommandCodeKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.CommandCodeKey))
	for i := range cfg.CommandCodeKey {
		entry := cfg.CommandCodeKey[i]
		key := strings.TrimSpace(entry.APIKey)
		if key == "" {
			continue
		}
		prefix := strings.TrimSpace(entry.Prefix)
		base := strings.TrimSpace(entry.BaseURL)
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		id, token := idGen.Next("commandcode:apikey", key, base, proxyURL)
		attrs := map[string]string{
			"source":  fmt.Sprintf("config:commandcode[%s]", token),
			"api_key": key,
		}
		metadata := map[string]any{}
		if entry.DisableCooling {
			metadata["disable_cooling"] = true
		}
		if entry.Priority != 0 {
			attrs["priority"] = strconv.Itoa(entry.Priority)
		}
		if base != "" {
			attrs["base_url"] = base
		}
		if hash := diff.ComputeCommandCodeModelsHash(entry.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(entry.Headers, attrs)
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "commandcode",
			Label:      configLabel(entry.Label, "commandcode-apikey"),
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, entry.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		out = append(out, a)
	}
	return out
}

// synthesizeOpenAICompat creates Auth entries for OpenAI-compatible providers.
func (s *ConfigSynthesizer) synthesizeOpenAICompat(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0)
	for i := range cfg.OpenAICompatibility {
		compat := &cfg.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		prefix := strings.TrimSpace(compat.Prefix)
		providerName := strings.ToLower(strings.TrimSpace(compat.Name))
		if providerName == "" {
			providerName = "openai-compatibility"
		}
		internalProviderKey := util.OpenAICompatibleProviderKey(providerName)
		base := strings.TrimSpace(compat.BaseURL)
		disableCooling := compat.DisableCooling
		envKeyEntry := openAICompatEnvKeyEntry(compat)

		// Handle new APIKeyEntries format (preferred)
		createdEntries := 0
		keyEntries := compat.APIKeyEntries
		if envKeyEntry.APIKey != "" {
			keyEntries = append(keyEntries, envKeyEntry)
		}
		for j := range keyEntries {
			entry := &keyEntries[j]
			key := strings.TrimSpace(entry.APIKey)
			proxyURL := strings.TrimSpace(entry.ProxyURL)
			idKind := fmt.Sprintf("openai-compatibility:%s", providerName)
			id, token := idGen.Next(idKind, key, base, proxyURL)
			attrs := map[string]string{
				"source":       fmt.Sprintf("config:%s[%s]", providerName, token),
				"base_url":     base,
				"compat_name":  compat.Name,
				"provider_key": internalProviderKey,
			}
			metadata := map[string]any{}
			if disableCooling {
				metadata["disable_cooling"] = true
			}
			if compat.Priority != 0 {
				attrs["priority"] = strconv.Itoa(compat.Priority)
			}
			if key != "" {
				attrs["api_key"] = key
			}
			if hash := diff.ComputeOpenAICompatModelsHash(compat.Models); hash != "" {
				attrs["models_hash"] = hash
			}
			addConfigHeadersToAttrs(compat.Headers, attrs)
			addConfigQueryParamsToAttrs(compat.QueryParams, attrs)
			a := &coreauth.Auth{
				ID:         id,
				Provider:   internalProviderKey,
				Label:      configLabel(entry.Label, compat.Name),
				Prefix:     prefix,
				Status:     coreauth.StatusActive,
				ProxyURL:   proxyURL,
				Attributes: attrs,
				Metadata:   metadata,
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			if len(a.Metadata) == 0 {
				a.Metadata = nil
			}
			out = append(out, a)
			createdEntries++
		}
		// Fallback: create entry without API key if no APIKeyEntries
		if createdEntries == 0 {
			idKind := fmt.Sprintf("openai-compatibility:%s", providerName)
			id, token := idGen.Next(idKind, base)
			attrs := map[string]string{
				"source":       fmt.Sprintf("config:%s[%s]", providerName, token),
				"base_url":     base,
				"compat_name":  compat.Name,
				"provider_key": internalProviderKey,
			}
			metadata := map[string]any{}
			if disableCooling {
				metadata["disable_cooling"] = true
			}
			if compat.Priority != 0 {
				attrs["priority"] = strconv.Itoa(compat.Priority)
			}
			if hash := diff.ComputeOpenAICompatModelsHash(compat.Models); hash != "" {
				attrs["models_hash"] = hash
			}
			addConfigHeadersToAttrs(compat.Headers, attrs)
			addConfigQueryParamsToAttrs(compat.QueryParams, attrs)
			a := &coreauth.Auth{
				ID:         id,
				Provider:   internalProviderKey,
				Label:      compat.Name,
				Prefix:     prefix,
				Status:     coreauth.StatusActive,
				Attributes: attrs,
				Metadata:   metadata,
				CreatedAt:  now,
				UpdatedAt:  now,
			}
			if len(a.Metadata) == 0 {
				a.Metadata = nil
			}
			out = append(out, a)
		}
	}
	return out
}

func openAICompatEnvKeyEntry(compat *config.OpenAICompatibility) config.OpenAICompatibilityAPIKey {
	if compat == nil {
		return config.OpenAICompatibilityAPIKey{}
	}
	envName := strings.TrimSpace(compat.APIKeyEnv)
	if envName == "" {
		return config.OpenAICompatibilityAPIKey{}
	}
	key := strings.TrimSpace(os.Getenv(envName))
	if key == "" {
		return config.OpenAICompatibilityAPIKey{}
	}
	return config.OpenAICompatibilityAPIKey{APIKey: key, Label: envName}
}

func (s *ConfigSynthesizer) synthesizeBedrock(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.Bedrock))
	for i := range cfg.Bedrock {
		entry := cfg.Bedrock[i]
		if entry.Disabled {
			continue
		}
		base := strings.TrimSpace(entry.BaseURL)
		if base == "" {
			continue
		}
		key := entry.ResolvedAPIKey()
		authType := entry.ResolvedAuthType()
		if authType == "" {
			continue
		}
		proxyURL := strings.TrimSpace(entry.ProxyURL)
		id, token := idGen.Next("bedrock:apikey", key, base, entry.Name, proxyURL)
		attrs := map[string]string{
			"source":             fmt.Sprintf("config:bedrock[%s]", token),
			"base_url":           base,
			"auth_type":          authType,
			"bedrock_name":       strings.TrimSpace(entry.Name),
			"bedrock_model_map":  bedrockModelMapAttr(entry.Models),
			"bedrock_api_map":    bedrockAPIMapAttr(entry.Models, false),
			"bedrock_stream_map": bedrockAPIMapAttr(entry.Models, true),
		}
		if key != "" {
			attrs["api_key"] = key
		}
		if entry.Priority != 0 {
			attrs["priority"] = strconv.Itoa(entry.Priority)
		}
		if hash := diff.ComputeBedrockModelsHash(entry.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(entry.Auth.Headers, attrs)
		addConfigHeadersToAttrs(entry.Headers, attrs)
		addConfigQueryParamsToAttrs(entry.QueryParams, attrs)
		metadata := map[string]any{}
		if entry.DisableCooling {
			metadata["disable_cooling"] = true
		}
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "bedrock",
			Label:      configLabel(entry.Label, configLabel(entry.Name, "bedrock-apikey")),
			Prefix:     strings.TrimSpace(entry.Prefix),
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, entry.ExcludedModels, "apikey")
		if len(a.Metadata) == 0 {
			a.Metadata = nil
		}
		out = append(out, a)
	}
	return out
}

func bedrockModelMapAttr(models []config.BedrockModel) string {
	values := make(map[string]string, len(models)*2)
	for _, model := range models {
		name := strings.TrimSpace(model.Name)
		alias := strings.TrimSpace(model.Alias)
		if name == "" && alias == "" {
			continue
		}
		if name == "" {
			name = alias
		}
		if alias == "" {
			alias = name
		}
		values[name] = name
		values[alias] = name
	}
	if len(values) == 0 {
		return "{}"
	}
	data, err := json.Marshal(values)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func bedrockAPIMapAttr(models []config.BedrockModel, stream bool) string {
	values := make(map[string]string, len(models)*2)
	for _, model := range models {
		name := strings.TrimSpace(model.Name)
		alias := strings.TrimSpace(model.Alias)
		if name == "" && alias == "" {
			continue
		}
		keyName := name
		if keyName == "" {
			keyName = alias
		}
		keyAlias := alias
		if keyAlias == "" {
			keyAlias = keyName
		}
		api := model.API
		if stream {
			api = model.StreamAPI
		}
		if strings.TrimSpace(api) == "" {
			if stream && strings.TrimSpace(model.API) == "invoke" {
				api = "invoke-stream"
			} else if stream {
				api = "converse-stream"
			} else {
				api = "converse"
			}
		}
		values[keyName] = strings.TrimSpace(api)
		values[keyAlias] = strings.TrimSpace(api)
	}
	if len(values) == 0 {
		return "{}"
	}
	data, err := json.Marshal(values)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// synthesizeVertexCompat creates Auth entries for Vertex-compatible providers.
func (s *ConfigSynthesizer) synthesizeVertexCompat(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	out := make([]*coreauth.Auth, 0, len(cfg.VertexCompatAPIKey))
	for i := range cfg.VertexCompatAPIKey {
		compat := &cfg.VertexCompatAPIKey[i]
		providerName := "vertex"
		base := strings.TrimSpace(compat.BaseURL)

		key := strings.TrimSpace(compat.APIKey)
		prefix := strings.TrimSpace(compat.Prefix)
		proxyURL := strings.TrimSpace(compat.ProxyURL)
		idKind := "vertex:apikey"
		id, token := idGen.Next(idKind, key, base, proxyURL)
		attrs := map[string]string{
			"source":       fmt.Sprintf("config:vertex-apikey[%s]", token),
			"base_url":     base,
			"provider_key": providerName,
		}
		if compat.Priority != 0 {
			attrs["priority"] = strconv.Itoa(compat.Priority)
		}
		if key != "" {
			attrs["api_key"] = key
		}
		if hash := diff.ComputeVertexCompatModelsHash(compat.Models); hash != "" {
			attrs["models_hash"] = hash
		}
		addConfigHeadersToAttrs(compat.Headers, attrs)
		a := &coreauth.Auth{
			ID:         id,
			Provider:   providerName,
			Label:      configLabel(compat.Label, "vertex-apikey"),
			Prefix:     prefix,
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			CreatedAt:  now,
			UpdatedAt:  now,
		}
		ApplyAuthExcludedModelsMeta(a, cfg, compat.ExcludedModels, "apikey")
		out = append(out, a)
	}
	return out
}

// synthesizeKiroKeys creates Auth entries for Kiro (AWS CodeWhisperer) tokens.
func (s *ConfigSynthesizer) synthesizeKiroKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	if len(cfg.KiroKey) == 0 {
		return nil
	}

	out := make([]*coreauth.Auth, 0, len(cfg.KiroKey))
	kAuth := kiroauth.NewKiroAuth(cfg)

	for i := range cfg.KiroKey {
		kk := cfg.KiroKey[i]
		var accessToken, profileArn, refreshToken string

		// Try to load from token file first
		if kk.TokenFile != "" && kAuth != nil {
			tokenData, err := kAuth.LoadTokenFromFile(kk.TokenFile)
			if err != nil {
				log.Warnf("failed to load kiro token file %s: %v", kk.TokenFile, err)
			} else {
				accessToken = tokenData.AccessToken
				profileArn = tokenData.ProfileArn
				refreshToken = tokenData.RefreshToken
			}
		}

		// Override with direct config values if provided
		if kk.AccessToken != "" {
			accessToken = kk.AccessToken
		}
		if kk.ProfileArn != "" {
			profileArn = kk.ProfileArn
		}
		if kk.RefreshToken != "" {
			refreshToken = kk.RefreshToken
		}

		if accessToken == "" {
			log.Warnf("kiro config[%d] missing access_token, skipping", i)
			continue
		}

		// profileArn is optional for AWS Builder ID users
		id, token := idGen.Next("kiro:token", accessToken, profileArn)
		attrs := map[string]string{
			"source":       fmt.Sprintf("config:kiro[%s]", token),
			"access_token": accessToken,
		}
		if profileArn != "" {
			attrs["profile_arn"] = profileArn
		}
		if kk.Region != "" {
			attrs["region"] = kk.Region
		}
		if kk.AgentTaskType != "" {
			attrs["agent_task_type"] = kk.AgentTaskType
		}
		if kk.PreferredEndpoint != "" {
			attrs["preferred_endpoint"] = kk.PreferredEndpoint
		} else if cfg.KiroPreferredEndpoint != "" {
			// Apply global default if not overridden by specific key
			attrs["preferred_endpoint"] = cfg.KiroPreferredEndpoint
		}
		if refreshToken != "" {
			attrs["refresh_token"] = refreshToken
		}
		proxyURL := strings.TrimSpace(kk.ProxyURL)
		a := &coreauth.Auth{
			ID:         id,
			Provider:   "kiro",
			Label:      "kiro-token",
			Status:     coreauth.StatusActive,
			ProxyURL:   proxyURL,
			Attributes: attrs,
			CreatedAt:  now,
			UpdatedAt:  now,
		}

		if refreshToken != "" {
			if a.Metadata == nil {
				a.Metadata = make(map[string]any)
			}
			a.Metadata["refresh_token"] = refreshToken
		}

		out = append(out, a)
	}
	return out
}
