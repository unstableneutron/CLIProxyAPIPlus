package qoder

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

const (
	// QoderInferURL is the base URL for Qoder inference API.
	QoderInferURL = "https://api1.qoder.sh"
	// QoderSigPath is the signing path for COSY authentication.
	QoderSigPath = "/api/v2/service/pro/sse/agent_chat_generation"
	// QoderChatURL is the full URL for the streaming chat endpoint.
	QoderChatURL = QoderInferURL + "/algo" + QoderSigPath + "?AgentId=agent_common"
)

// ModelMap maps user-facing model identifiers to upstream Qoder model keys.
// Based on the Qoder CLI reverse-engineering notes
// (https://github.com/alingse/qodercli-reverse, docs/03-llm-integration.md):
//
//   - Tier models: auto, efficient, performance, ultimate, lite
//   - Frontier ("Q" family) models: qmodel, q35model, gmodel, kmodel, mmodel
//
// All identifiers are passed through as-is — the upstream accepts these strings
// directly, so the map is identity. The map exists so we can cheaply validate
// "is this a known qoder model?" and emit a stable set in /v1/models.
var ModelMap = map[string]string{
	"auto":        "auto",
	"efficient":   "efficient",
	"performance": "performance",
	"ultimate":    "ultimate",
	"lite":        "lite",
	"qmodel":      "qmodel",
	"q35model":    "q35model",
	"gmodel":      "gmodel",
	"kmodel":      "kmodel",
	"mmodel":      "mmodel",
}

// doRefreshToken performs a token refresh and persists the result to authFilePath.
// When authFilePath is empty, it falls back to AuthDir/qoder-<email>.json for
// backward compatibility with auth records that lack a recorded path.
func doRefreshToken(ctx context.Context, cfg *config.Config, storage *QoderTokenStorage, authFilePath string) error {
	auth := NewQoderAuth(cfg)

	tokenData, err := auth.RefreshTokens(ctx, storage.Token, storage.RefreshToken)
	if err != nil {
		return fmt.Errorf("failed to refresh token: %w", err)
	}

	auth.UpdateTokenStorage(storage, tokenData)

	if authFilePath == "" {
		if storage.Email == "" {
			return fmt.Errorf("cannot save token: email is empty and no file path provided")
		}
		fileName := fmt.Sprintf("qoder-%s.json", storage.Email)
		authFilePath = filepath.Join(cfg.AuthDir, fileName)
	}
	return storage.SaveTokenToFile(authFilePath)
}

// RefreshTokenIfNeeded refreshes the access token when the remaining lifetime
// drops below bufferSeconds. authFilePath is the on-disk location of the auth
// record; an empty value triggers the email-derived fallback path.
func RefreshTokenIfNeeded(ctx context.Context, cfg *config.Config, storage *QoderTokenStorage, bufferSeconds int64, authFilePath string) error {
	if storage.ExpireTime == 0 {
		return nil
	}

	now := time.Now().UnixMilli()
	bufferMs := bufferSeconds * 1000

	if storage.ExpireTime-now-bufferMs <= 0 {
		return doRefreshToken(ctx, cfg, storage, authFilePath)
	}

	return nil
}
