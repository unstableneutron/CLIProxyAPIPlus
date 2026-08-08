package auth

import (
	"strings"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func seedTriedWithExcludedAuthIDs(tried map[string]struct{}, metadata map[string]any) {
	for _, authID := range excludedAuthIDsFromMetadata(metadata) {
		tried[authID] = struct{}{}
	}
}

func excludedAuthIDsFromMetadata(metadata map[string]any) []string {
	if len(metadata) == 0 {
		return nil
	}
	raw, ok := metadata[cliproxyexecutor.ExcludedAuthIDsMetadataKey]
	if !ok {
		return nil
	}
	values := make([]string, 0)
	switch typed := raw.(type) {
	case []string:
		values = append(values, typed...)
	case []any:
		for _, value := range typed {
			if text, okText := value.(string); okText {
				values = append(values, text)
			}
		}
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, authID := range values {
		authID = strings.TrimSpace(authID)
		if authID == "" {
			continue
		}
		if _, exists := seen[authID]; exists {
			continue
		}
		seen[authID] = struct{}{}
		result = append(result, authID)
	}
	return result
}
