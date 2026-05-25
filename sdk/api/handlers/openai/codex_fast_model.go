package openai

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexFastModelSuffix = "-fast"

func normalizeCodexFastSpeedTierRequest(rawJSON []byte) []byte {
	model := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	baseModel, ok := codexFastSpeedTierBaseModel(model)
	if !ok || !codexClientModelSupportsSpeedTier(baseModel, "fast") {
		return rawJSON
	}

	routedModel := baseModel
	if parsed := thinking.ParseSuffix(model); parsed.HasSuffix && parsed.RawSuffix != "" {
		routedModel = baseModel + "(" + parsed.RawSuffix + ")"
	}

	updated, err := sjson.SetBytes(rawJSON, "model", routedModel)
	if err != nil {
		return rawJSON
	}
	updated, err = sjson.SetBytes(updated, "service_tier", "priority")
	if err != nil {
		return rawJSON
	}
	return updated
}

func codexFastSpeedTierBaseModel(model string) (string, bool) {
	parsed := thinking.ParseSuffix(strings.TrimSpace(model))
	base := strings.TrimSpace(parsed.ModelName)
	if base == "" || !strings.HasSuffix(base, codexFastModelSuffix) {
		return "", false
	}
	base = strings.TrimSpace(strings.TrimSuffix(base, codexFastModelSuffix))
	if base == "" {
		return "", false
	}
	return base, true
}

func codexClientModelSupportsSpeedTier(model, tier string) bool {
	model = strings.TrimSpace(model)
	tier = strings.ToLower(strings.TrimSpace(tier))
	if model == "" || tier == "" {
		return false
	}
	templates, _, err := loadCodexClientModelTemplates()
	if err != nil || templates == nil {
		return false
	}
	template := templates[model]
	if template == nil {
		return false
	}
	for _, rawTier := range anySlice(template["additional_speed_tiers"]) {
		if strings.EqualFold(strings.TrimSpace(stringAnyValue(rawTier)), tier) {
			return true
		}
	}
	return false
}

func anySlice(value any) []any {
	if items, ok := value.([]any); ok {
		return items
	}
	return nil
}

func stringAnyValue(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}
