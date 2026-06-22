package synthesizer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	geminicli "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/geminicli"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// FileSynthesizer generates Auth entries from OAuth JSON files.
// It handles file-based authentication.
type FileSynthesizer struct{}

// NewFileSynthesizer creates a new FileSynthesizer instance.
func NewFileSynthesizer() *FileSynthesizer {
	return &FileSynthesizer{}
}

// Synthesize generates Auth entries from auth files in the auth directory.
func (s *FileSynthesizer) Synthesize(ctx *SynthesisContext) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, 16)
	if ctx == nil || ctx.AuthDir == "" {
		return out, nil
	}

	entries, err := os.ReadDir(ctx.AuthDir)
	if err != nil {
		// Not an error if directory doesn't exist
		return out, nil
	}

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".json") {
			continue
		}
		full := filepath.Join(ctx.AuthDir, name)
		data, errRead := os.ReadFile(full)
		if errRead != nil || len(data) == 0 {
			continue
		}
		auths := synthesizeFileAuths(ctx, full, data)
		if len(auths) == 0 {
			continue
		}
		out = append(out, auths...)
	}
	return out, nil
}

// SynthesizeAuthFile generates Auth entries for one auth JSON file payload.
// It shares exactly the same mapping behavior as FileSynthesizer.Synthesize.
func SynthesizeAuthFile(ctx *SynthesisContext, fullPath string, data []byte) []*coreauth.Auth {
	return synthesizeFileAuths(ctx, fullPath, data)
}

func synthesizeFileAuths(ctx *SynthesisContext, fullPath string, data []byte) []*coreauth.Auth {
	if ctx == nil || len(data) == 0 {
		return nil
	}
	now := ctx.Now
	cfg := ctx.Config
	var metadata map[string]any
	if errUnmarshal := json.Unmarshal(data, &metadata); errUnmarshal != nil {
		return nil
	}
	t, _ := metadata["type"].(string)
	provider := strings.ToLower(strings.TrimSpace(t))
	if ctx.PluginAuthParser != nil {
		auths, handled, errParse := parsePluginFileAuths(ctx.PluginAuthParser, pluginapi.AuthParseRequest{
			Provider: provider,
			Path:     fullPath,
			FileName: filepath.Base(fullPath),
			RawJSON:  data,
		})
		if errParse == nil && handled {
			auths = compactPluginAuths(auths)
			if len(auths) == 0 {
				return nil
			}
			perAccountExcluded := extractExcludedModelsFromMetadata(metadata)
			perAccountModelAliases := extractOAuthModelAliasesFromMetadata(metadata)
			for index, auth := range auths {
				if auth == nil {
					continue
				}
				if len(auths) > 1 {
					coreauth.MarkPluginVirtualAuth(auth, fullPath, index)
				}
				auth.CreatedAt = now
				auth.UpdatedAt = now
				if auth.Attributes == nil {
					auth.Attributes = make(map[string]string)
				}
				auth.Attributes["path"] = fullPath
				auth.Attributes["source"] = fullPath
				coreauth.SetOAuthModelAliasesAttribute(auth, perAccountModelAliases)
				ApplyAuthExcludedModelsMeta(auth, cfg, perAccountExcluded, "oauth")
				coreauth.ApplyCustomHeadersFromMetadata(auth)
			}
			return auths
		}
	}
	if provider == "" {
		return nil
	}
	if provider == "gemini" {
		provider = "gemini-cli"
	}
	label := provider
	if email, _ := metadata["email"].(string); email != "" {
		label = email
	}
	// Use relative path under authDir as ID to stay consistent with the file-based token store.
	id := fullPath
	if strings.TrimSpace(ctx.AuthDir) != "" {
		if rel, errRel := filepath.Rel(ctx.AuthDir, fullPath); errRel == nil && rel != "" {
			id = rel
		}
	}
	if runtime.GOOS == "windows" {
		id = strings.ToLower(id)
	}

	proxyURL := ""
	if p, ok := metadata["proxy_url"].(string); ok {
		proxyURL = p
	}

	prefix := ""
	if rawPrefix, ok := metadata["prefix"].(string); ok {
		trimmed := strings.TrimSpace(rawPrefix)
		trimmed = strings.Trim(trimmed, "/")
		if trimmed != "" && !strings.Contains(trimmed, "/") {
			prefix = trimmed
		}
	}

	disabled, _ := metadata["disabled"].(bool)
	status := coreauth.StatusActive
	if disabled {
		status = coreauth.StatusDisabled
	}

	// Read per-account excluded models from the OAuth JSON file.
	perAccountExcluded := extractExcludedModelsFromMetadata(metadata)
	perAccountModelAliases := extractOAuthModelAliasesFromMetadata(metadata)

	a := &coreauth.Auth{
		ID:       id,
		Provider: provider,
		Label:    label,
		Prefix:   prefix,
		Status:   status,
		Disabled: disabled,
		Attributes: map[string]string{
			"source": fullPath,
			"path":   fullPath,
		},
		ProxyURL:  proxyURL,
		Metadata:  metadata,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// Read priority from auth file.
	if rawPriority, ok := metadata["priority"]; ok {
		switch v := rawPriority.(type) {
		case float64:
			a.Attributes["priority"] = strconv.Itoa(int(v))
		case string:
			priority := strings.TrimSpace(v)
			if _, errAtoi := strconv.Atoi(priority); errAtoi == nil {
				a.Attributes["priority"] = priority
			}
		}
	}
	// Read note from auth file.
	if rawNote, ok := metadata["note"]; ok {
		if note, isStr := rawNote.(string); isStr {
			if trimmed := strings.TrimSpace(note); trimmed != "" {
				a.Attributes["note"] = trimmed
			}
		}
	}
	if provider == "commandcode" {
		if apiKey := extractCommandCodeAPIKey(metadata); apiKey != "" {
			a.Attributes["api_key"] = apiKey
		}
		if baseURL := extractStringMetadata(metadata, "base_url", "baseURL", "api_base", "apiBase"); baseURL != "" {
			a.Attributes["base_url"] = baseURL
		}
	}
	coreauth.ApplyCustomHeadersFromMetadata(a)
	coreauth.SetOAuthModelAliasesAttribute(a, perAccountModelAliases)
	ApplyAuthExcludedModelsMeta(a, cfg, perAccountExcluded, "oauth")
	// For codex auth files, extract plan_type from the JWT id_token.
	if provider == "codex" {
		if idTokenRaw, ok := metadata["id_token"].(string); ok && strings.TrimSpace(idTokenRaw) != "" {
			if claims, errParse := codex.ParseJWTToken(idTokenRaw); errParse == nil && claims != nil {
				if pt := strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType); pt != "" {
					a.Attributes["plan_type"] = pt
				}
			}
		}
	}
	if provider == "gemini-cli" {
		if virtuals := SynthesizeGeminiVirtualAuths(a, metadata, now); len(virtuals) > 0 {
			for _, v := range virtuals {
				ApplyAuthExcludedModelsMeta(v, cfg, perAccountExcluded, "oauth")
			}
			out := make([]*coreauth.Auth, 0, 1+len(virtuals))
			out = append(out, a)
			out = append(out, virtuals...)
			return out
		}
	}
	return []*coreauth.Auth{a}
}

// SynthesizeGeminiVirtualAuths creates virtual Auth entries for multi-project Gemini credentials.
// It disables the primary auth and creates one virtual auth per project.
func SynthesizeGeminiVirtualAuths(primary *coreauth.Auth, metadata map[string]any, now time.Time) []*coreauth.Auth {
	if primary == nil || metadata == nil {
		return nil
	}
	projects := splitGeminiProjectIDs(metadata)
	if len(projects) <= 1 {
		return nil
	}
	email, _ := metadata["email"].(string)
	shared := geminicli.NewSharedCredential(primary.ID, email, metadata, projects)
	primary.Disabled = true
	primary.Status = coreauth.StatusDisabled
	primary.Runtime = shared
	if primary.Attributes == nil {
		primary.Attributes = make(map[string]string)
	}
	primary.Attributes["gemini_virtual_primary"] = "true"
	primary.Attributes["virtual_children"] = strings.Join(projects, ",")
	source := primary.Attributes["source"]
	authPath := primary.Attributes["path"]
	originalProvider := primary.Provider
	if originalProvider == "" {
		originalProvider = "gemini-cli"
	}
	label := primary.Label
	if label == "" {
		label = originalProvider
	}
	virtuals := make([]*coreauth.Auth, 0, len(projects))
	for _, projectID := range projects {
		attrs := map[string]string{
			"runtime_only":           "true",
			"gemini_virtual_parent":  primary.ID,
			"gemini_virtual_project": projectID,
		}
		if source != "" {
			attrs["source"] = source
		}
		if authPath != "" {
			attrs["path"] = authPath
		}
		for k, v := range primary.Attributes {
			if strings.HasPrefix(k, "excluded_models") || strings.HasPrefix(k, "oauth_model_alias") || k == "priority" || k == "note" {
				attrs[k] = v
			}
		}
		metadataCopy := map[string]any{
			"email":             email,
			"project_id":        projectID,
			"virtual":           true,
			"virtual_parent_id": primary.ID,
			"type":              metadata["type"],
		}
		if v, ok := metadata["disable_cooling"]; ok {
			metadataCopy["disable_cooling"] = v
		}
		if v, ok := metadata["refresh_interval_seconds"]; ok {
			metadataCopy["refresh_interval_seconds"] = v
		}
		if proxy := strings.TrimSpace(primary.ProxyURL); proxy != "" {
			metadataCopy["proxy_url"] = proxy
		}
		virtual := &coreauth.Auth{
			ID:         buildGeminiVirtualID(primary.ID, projectID),
			Provider:   originalProvider,
			Label:      fmt.Sprintf("%s [%s]", label, projectID),
			Status:     coreauth.StatusActive,
			Attributes: attrs,
			ProxyURL:   primary.ProxyURL,
			Metadata:   metadataCopy,
			CreatedAt:  primary.CreatedAt,
			UpdatedAt:  primary.UpdatedAt,
			Runtime:    geminicli.NewVirtualCredential(projectID, shared),
		}
		virtuals = append(virtuals, virtual)
	}
	return virtuals
}

// splitGeminiProjectIDs extracts and deduplicates project IDs from metadata.
func splitGeminiProjectIDs(metadata map[string]any) []string {
	raw, _ := metadata["project_id"].(string)
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	seen := map[string]struct{}{}
	result := make([]string, 0)
	for _, part := range strings.Split(trimmed, ",") {
		project := strings.TrimSpace(part)
		if project == "" {
			continue
		}
		if _, ok := seen[project]; ok {
			continue
		}
		seen[project] = struct{}{}
		result = append(result, project)
	}
	return result
}

// buildGeminiVirtualID constructs a virtual auth ID from base ID and project ID.
func buildGeminiVirtualID(baseID, projectID string) string {
	project := strings.TrimSpace(projectID)
	if project == "" {
		project = "project"
	}
	replacer := strings.NewReplacer("/", "-", "\\", "-", " ", "-", ":", "-")
	return fmt.Sprintf("%s::%s", baseID, replacer.Replace(project))
}

func extractCommandCodeAPIKey(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	if value := extractStringMetadata(metadata, "api_key", "apiKey", "access_token", "access", "commandcode"); value != "" {
		return value
	}
	if nested, ok := metadata["commandcode"].(map[string]any); ok {
		return extractStringMetadata(nested, "access", "access_token", "apiKey", "api_key")
	}
	return ""
}

func extractStringMetadata(metadata map[string]any, keys ...string) string {
	for _, key := range keys {
		if raw, ok := metadata[key]; ok {
			if value, okString := raw.(string); okString {
				if trimmed := strings.TrimSpace(value); trimmed != "" {
					return trimmed
				}
			}
		}
	}
	return ""
}

func parsePluginFileAuths(parser PluginAuthParser, req pluginapi.AuthParseRequest) ([]*coreauth.Auth, bool, error) {
	if parser == nil {
		return nil, false, nil
	}
	if multiParser, ok := parser.(PluginMultiAuthParser); ok {
		return multiParser.ParseAuths(context.Background(), req)
	}
	auth, handled, errParse := parser.ParseAuth(context.Background(), req)
	if errParse != nil || !handled || auth == nil {
		return nil, handled, errParse
	}
	return []*coreauth.Auth{auth}, true, nil
}

func compactPluginAuths(auths []*coreauth.Auth) []*coreauth.Auth {
	if len(auths) == 0 {
		return nil
	}
	out := auths[:0]
	for _, auth := range auths {
		if auth == nil {
			continue
		}
		out = append(out, auth)
	}
	return out
}

// extractOAuthModelAliasesFromMetadata reads per-account model aliases from OAuth JSON metadata.
// Supports both "model_aliases" and "model-aliases" keys.
func extractOAuthModelAliasesFromMetadata(metadata map[string]any) []config.OAuthModelAlias {
	if metadata == nil {
		return nil
	}
	raw, ok := metadata["model_aliases"]
	if !ok {
		raw, ok = metadata["model-aliases"]
	}
	if !ok || raw == nil {
		return nil
	}
	data, errMarshal := json.Marshal(raw)
	if errMarshal != nil {
		return nil
	}
	var aliases []config.OAuthModelAlias
	if errUnmarshal := json.Unmarshal(data, &aliases); errUnmarshal != nil {
		return nil
	}
	cfg := config.Config{
		OAuthModelAlias: map[string][]config.OAuthModelAlias{
			"auth": aliases,
		},
	}
	cfg.SanitizeOAuthModelAlias()
	return cfg.OAuthModelAlias["auth"]
}

// extractExcludedModelsFromMetadata reads per-account excluded models from the OAuth JSON metadata.
// Supports both "excluded_models" and "excluded-models" keys, and accepts both []string and []interface{}.
func extractExcludedModelsFromMetadata(metadata map[string]any) []string {
	if metadata == nil {
		return nil
	}
	// Try both key formats
	raw, ok := metadata["excluded_models"]
	if !ok {
		raw, ok = metadata["excluded-models"]
	}
	if !ok || raw == nil {
		return nil
	}
	var stringSlice []string
	switch v := raw.(type) {
	case []string:
		stringSlice = v
	case []interface{}:
		stringSlice = make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				stringSlice = append(stringSlice, s)
			}
		}
	default:
		return nil
	}
	result := make([]string, 0, len(stringSlice))
	for _, s := range stringSlice {
		if trimmed := strings.TrimSpace(s); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}
