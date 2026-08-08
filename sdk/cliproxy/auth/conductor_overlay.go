package auth

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/clienterror"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

// isOpenAICompatAPIKeyAuth reports whether auth is an API-key-backed
// OpenAI-compatible credential. Keyless config routes use the broader
// isConfiguredOpenAICompatAuth helper instead.
func isOpenAICompatAPIKeyAuth(auth *Auth) bool {
	if !isAPIKeyAuth(auth) {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(auth.Provider), "openai-compatibility") {
		return true
	}
	return auth.Attributes != nil && strings.TrimSpace(auth.Attributes["compat_name"]) != ""
}

// recordProxySelection exposes the selected credential and resolved upstream
// model through the request's proxy observability state.
func recordProxySelection(ctx context.Context, auth *Auth, routeModel, upstreamModel string) {
	if auth == nil {
		return
	}
	logging.SetSlot(ctx, auth.EnsureIndex())
	logging.SetSlotPriority(ctx, authPriority(auth))
	logging.SetRoute(ctx, routeModel, upstreamModel)
}

func isRequestScopedNotFoundMessage(message string) bool {
	return clienterror.IsItemNotPersisted(message)
}

func resolveCommandCodeAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.CommandCodeKey {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.CommandCodeKey, auth)
}

func resolveBedrockAPIKeyConfig(cfg *internalconfig.Config, auth *Auth) *internalconfig.BedrockProvider {
	if cfg == nil {
		return nil
	}
	return resolveAPIKeyConfig(cfg.Bedrock, auth)
}

func resolveUpstreamModelForCommandCodeAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveCommandCodeAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}

func resolveUpstreamModelForBedrockAPIKey(cfg *internalconfig.Config, auth *Auth, requestedModel string) string {
	entry := resolveBedrockAPIKeyConfig(cfg, auth)
	if entry == nil {
		return ""
	}
	return resolveModelAliasFromConfigModels(requestedModel, asModelAliasEntries(entry.Models))
}
