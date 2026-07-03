package diff

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func ComputeBedrockModelsHash(models []config.BedrockModel) string {
	if len(models) == 0 {
		return ""
	}
	parts := make([]string, 0, len(models))
	for _, model := range models {
		name := strings.TrimSpace(model.Name)
		alias := strings.TrimSpace(model.Alias)
		if name == "" && alias == "" {
			continue
		}
		api := strings.TrimSpace(model.API)
		streamAPI := strings.TrimSpace(model.StreamAPI)
		parts = append(parts, strings.Join([]string{
			strings.ToLower(name),
			strings.ToLower(alias),
			strings.ToLower(api),
			strings.ToLower(streamAPI),
			fmt.Sprintf("force=%t", model.ForceMapping),
		}, "|"))
	}
	if len(parts) == 0 {
		return ""
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:])
}
