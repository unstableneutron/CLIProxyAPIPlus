package responses

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
)

// convertResponsesToolToClaudeTools keeps the fork helper surface while
// delegating each tool shape to the current Responses descriptor converters.
func convertResponsesToolToClaudeTools(tool gjson.Result, toolNameMap map[string]string) [][]byte {
	toolType := strings.TrimSpace(tool.Get("type").String())
	switch toolType {
	case "", "function":
		if converted, ok := convertResponsesFunctionToolToClaude(tool, ""); ok {
			return [][]byte{converted}
		}
	case "custom":
		if converted, ok := convertResponsesCustomToolToClaude(tool, ""); ok {
			return [][]byte{converted}
		}
	case "namespace":
		return convertResponsesNamespaceToolToClaude(tool, toolNameMap)
	case "web_search":
		if converted, ok := convertResponsesWebSearchToolToClaude(tool); ok {
			if name := gjson.GetBytes(converted, "name").String(); name != "" && toolNameMap != nil {
				toolNameMap[name] = name
			}
			return [][]byte{converted}
		}
	default:
		if isUnsupportedOpenAIBuiltinToolType(toolType) || tool.Get("name").String() == "" {
			return nil
		}
		return [][]byte{[]byte(tool.Raw)}
	}
	return nil
}

// convertResponsesNamespaceToolToClaude preserves the fork's direct helper for
// callers that convert one namespace independently of the descriptor pipeline.
func convertResponsesNamespaceToolToClaude(tool gjson.Result, toolNameMap map[string]string) [][]byte {
	namespaceName := strings.TrimSpace(tool.Get("name").String())
	children := tool.Get("tools")
	if !children.Exists() || !children.IsArray() {
		return nil
	}

	var convertedTools [][]byte
	children.ForEach(func(_, child gjson.Result) bool {
		childName := responsesToolName(child)
		qualifiedName := qualifyResponsesNamespaceToolName(namespaceName, childName)

		var (
			converted []byte
			ok        bool
		)
		switch strings.TrimSpace(child.Get("type").String()) {
		case "custom":
			converted, ok = convertResponsesCustomToolToClaude(child, qualifiedName)
		case "web_search":
			converted, ok = convertResponsesWebSearchToolToClaude(child)
		default:
			converted, ok = convertResponsesFunctionToolToClaude(child, qualifiedName)
		}
		if !ok {
			return true
		}

		convertedTools = append(convertedTools, converted)
		if toolNameMap != nil {
			finalName := gjson.GetBytes(converted, "name").String()
			if finalName != "" {
				toolNameMap[finalName] = finalName
			}
			if childName != "" {
				toolNameMap[childName] = finalName
			}
		}
		return true
	})
	return convertedTools
}

// normalizeClaudeToolInputSchema retains the package-local fork seam while the
// canonical normalization implementation lives in internal/util.
func normalizeClaudeToolInputSchema(parameters gjson.Result) []byte {
	return util.NormalizeClaudeToolInputSchema([]byte(parameters.Raw))
}

// responsesReasoningSummaryText retains the fork seam and delegates to the
// newer reasoning-chain reader, which also supports content fallback.
func responsesReasoningSummaryText(item gjson.Result) string {
	return responsesReasoningText(item)
}
