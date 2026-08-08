package config

import "strings"

// SanitizeCommandCodeKeys deduplicates and normalizes Command Code credentials.
func (cfg *Config) SanitizeCommandCodeKeys() {
	if cfg == nil || len(cfg.CommandCodeKey) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(cfg.CommandCodeKey))
	out := cfg.CommandCodeKey[:0]
	for i := range cfg.CommandCodeKey {
		entry := cfg.CommandCodeKey[i]
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		if entry.APIKey == "" {
			continue
		}
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.BaseURL = strings.TrimSpace(entry.BaseURL)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
		uniqueKey := entry.APIKey + "|" + entry.BaseURL
		if _, exists := seen[uniqueKey]; exists {
			continue
		}
		seen[uniqueKey] = struct{}{}
		out = append(out, entry)
	}
	cfg.CommandCodeKey = out
}

// NormalizeQueryParams trims query parameter keys and values and removes empty pairs.
func NormalizeQueryParams(params map[string]string) map[string]string {
	return normalizeStringMap(params)
}

func normalizeStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	clean := make(map[string]string, len(values))
	for k, v := range values {
		key := strings.TrimSpace(k)
		val := strings.TrimSpace(v)
		if key == "" || val == "" {
			continue
		}
		clean[key] = val
	}
	if len(clean) == 0 {
		return nil
	}
	return clean
}
