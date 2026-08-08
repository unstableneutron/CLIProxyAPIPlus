package helps

import "strings"

// ClaudeCodeEntrypoint exposes the current Claude Code User-Agent parser to
// executor compatibility shims.
func ClaudeCodeEntrypoint(userAgent string) string {
	entrypoint, _ := parseClaudeCodeUserAgentDetails(userAgent)
	return entrypoint
}

// isClaudeCodeClient preserves the historical helper using the current
// Claude-Code User-Agent classifier.
func isClaudeCodeClient(userAgent string) bool {
	return claudeCodeUserAgentPattern.MatchString(strings.TrimSpace(userAgent))
}

// ShouldCloak preserves the overlay API for callers that only have a cloak mode
// and User-Agent. Full request handling uses the stronger wire-policy detector.
func ShouldCloak(cloakMode string, userAgent string) bool {
	switch strings.ToLower(strings.TrimSpace(cloakMode)) {
	case "always":
		return true
	case "never":
		return false
	default:
		return !isClaudeCodeClient(userAgent)
	}
}
