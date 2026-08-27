package executor

import (
	"net/url"
	"strings"
)

func xaiBaseURLForLog(baseURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "<redacted>"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String()
}

func restoreXAINamespaceToolCallAtPath(data []byte, path string, refs map[string]xaiNamespaceToolRef) []byte {
	return newXAINamespaceRestorer(refs).restoreAtPath(data, path)
}
