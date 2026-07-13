package config

import (
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

const (
	BedrockAuthTypeBearer = "bearer"
	BedrockAuthTypeRaw    = "raw"
	BedrockAuthTypeNone   = "none"
)

type BedrockProvider struct {
	Name           string            `yaml:"name" json:"name"`
	Label          string            `yaml:"label,omitempty" json:"label,omitempty"`
	Priority       int               `yaml:"priority,omitempty" json:"priority,omitempty"`
	Disabled       bool              `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	Prefix         string            `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	BaseURL        string            `yaml:"base-url" json:"base-url"`
	APIKey         string            `yaml:"api-key,omitempty" json:"api-key,omitempty"`
	APIKeyEnv      string            `yaml:"api-key-env,omitempty" json:"api-key-env,omitempty"`
	Auth           BedrockAuth       `yaml:"auth,omitempty" json:"auth,omitempty"`
	Models         []BedrockModel    `yaml:"models" json:"models"`
	Headers        map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	QueryParams    map[string]string `yaml:"query-params,omitempty" json:"query-params,omitempty"`
	ProxyURL       string            `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`
	DisableCooling bool              `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`
	ExcludedModels []string          `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`
}

func (p BedrockProvider) GetAPIKey() string  { return p.ResolvedAPIKey() }
func (p BedrockProvider) GetBaseURL() string { return p.BaseURL }

type BedrockAuth struct {
	Type      string            `yaml:"type,omitempty" json:"type,omitempty"`
	APIKey    string            `yaml:"api-key,omitempty" json:"api-key,omitempty"`
	APIKeyEnv string            `yaml:"api-key-env,omitempty" json:"api-key-env,omitempty"`
	Headers   map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
}

type BedrockModel struct {
	Name         string                    `yaml:"name" json:"name"`
	Alias        string                    `yaml:"alias" json:"alias"`
	DisplayName  string                    `yaml:"display-name,omitempty" json:"display-name,omitempty"`
	ForceMapping bool                      `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`
	API          string                    `yaml:"api,omitempty" json:"api,omitempty"`
	StreamAPI    string                    `yaml:"stream-api,omitempty" json:"stream-api,omitempty"`
	Thinking     *registry.ThinkingSupport `yaml:"thinking,omitempty" json:"thinking,omitempty"`
}

func (m BedrockModel) GetName() string        { return m.Name }
func (m BedrockModel) GetAlias() string       { return m.Alias }
func (m BedrockModel) GetDisplayName() string { return m.DisplayName }
func (m BedrockModel) GetForceMapping() bool  { return m.ForceMapping }

func (p BedrockProvider) ResolvedAPIKey() string {
	if key := strings.TrimSpace(p.Auth.APIKey); key != "" {
		return key
	}
	if key := strings.TrimSpace(p.APIKey); key != "" {
		return key
	}
	if envName := strings.TrimSpace(p.Auth.APIKeyEnv); envName != "" {
		return strings.TrimSpace(os.Getenv(envName))
	}
	if envName := strings.TrimSpace(p.APIKeyEnv); envName != "" {
		return strings.TrimSpace(os.Getenv(envName))
	}
	return ""
}

func (p BedrockProvider) ResolvedAuthType() string {
	rawAuthType := strings.TrimSpace(p.Auth.Type)
	authType := normalizeBedrockAuthType(p.Auth.Type)
	if authType == "" && rawAuthType == "" {
		return BedrockAuthTypeBearer
	}
	return authType
}

func normalizeBedrockAuthType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", BedrockAuthTypeBearer:
		return BedrockAuthTypeBearer
	case "authorization", BedrockAuthTypeRaw:
		return BedrockAuthTypeRaw
	case BedrockAuthTypeNone:
		return BedrockAuthTypeNone
	default:
		return ""
	}
}

func normalizeBedrockModelAPI(value string, stream bool) string {
	normalized := strings.ToLower(strings.TrimSpace(value))
	switch normalized {
	case "", "converse":
		if stream {
			return "converse-stream"
		}
		return "converse"
	case "conversestream", "converse_stream", "converse-stream":
		return "converse-stream"
	case "invoke", "invoke-model", "invokemodel":
		if stream {
			return "invoke-stream"
		}
		return "invoke"
	case "invoke-stream", "invoke_model_with_response_stream", "invokemodelwithresponsestream", "invoke-with-response-stream":
		return "invoke-stream"
	default:
		return normalized
	}
}

func (cfg *Config) SanitizeBedrockProviders() {
	if cfg == nil || len(cfg.Bedrock) == 0 {
		return
	}
	out := make([]BedrockProvider, 0, len(cfg.Bedrock))
	for i := range cfg.Bedrock {
		entry := cfg.Bedrock[i]
		entry.Name = strings.TrimSpace(entry.Name)
		entry.Label = strings.TrimSpace(entry.Label)
		entry.Prefix = normalizeModelPrefix(entry.Prefix)
		entry.BaseURL = strings.TrimSpace(entry.BaseURL)
		entry.APIKey = strings.TrimSpace(entry.APIKey)
		entry.APIKeyEnv = strings.TrimSpace(entry.APIKeyEnv)
		rawAuthType := strings.TrimSpace(entry.Auth.Type)
		entry.Auth.Type = normalizeBedrockAuthType(entry.Auth.Type)
		if rawAuthType != "" && entry.Auth.Type == "" {
			continue
		}
		entry.Auth.APIKey = strings.TrimSpace(entry.Auth.APIKey)
		entry.Auth.APIKeyEnv = strings.TrimSpace(entry.Auth.APIKeyEnv)
		entry.Auth.Headers = NormalizeHeaders(entry.Auth.Headers)
		entry.Headers = NormalizeHeaders(entry.Headers)
		entry.QueryParams = NormalizeQueryParams(entry.QueryParams)
		entry.ProxyURL = strings.TrimSpace(entry.ProxyURL)
		entry.ExcludedModels = NormalizeExcludedModels(entry.ExcludedModels)
		if entry.BaseURL == "" {
			continue
		}
		models := make([]BedrockModel, 0, len(entry.Models))
		for _, model := range entry.Models {
			model.Name = strings.TrimSpace(model.Name)
			model.Alias = strings.TrimSpace(model.Alias)
			model.API = normalizeBedrockModelAPI(model.API, false)
			if strings.TrimSpace(model.StreamAPI) == "" && model.API == "invoke" {
				model.StreamAPI = "invoke-stream"
			} else {
				model.StreamAPI = normalizeBedrockModelAPI(model.StreamAPI, true)
			}
			if model.Name == "" && model.Alias == "" {
				continue
			}
			models = append(models, model)
		}
		entry.Models = models
		out = append(out, entry)
	}
	cfg.Bedrock = out
}
