package config

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// ResponsesStateCapability stores a Codex route's Responses state capability.
type ResponsesStateCapability string

func (v *ResponsesStateCapability) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Tag == "!!null" {
		*v = ""
		return nil
	}
	if node.Kind != yaml.ScalarNode {
		return fmt.Errorf("responses-state must be a scalar")
	}
	if node.Tag == "!!bool" {
		parsed, err := strconv.ParseBool(node.Value)
		if err != nil {
			return fmt.Errorf("parse responses-state bool: %w", err)
		}
		*v = ResponsesStateCapability(strconv.FormatBool(parsed))
		return nil
	}
	*v = ResponsesStateCapability(strings.TrimSpace(node.Value))
	return nil
}

func (v *ResponsesStateCapability) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*v = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		*v = ResponsesStateCapability(strings.TrimSpace(s))
		return nil
	}
	var b bool
	if err := json.Unmarshal(data, &b); err == nil {
		*v = ResponsesStateCapability(strconv.FormatBool(b))
		return nil
	}
	return fmt.Errorf("responses-state must be a string or boolean")
}

// CodexContinueThinking configures optional Codex reasoning truncation folding.
type CodexContinueThinking struct {
	Enabled              bool   `yaml:"enabled" json:"enabled"`
	Method               string `yaml:"method" json:"method"`
	TruncationStep       int    `yaml:"truncation-step" json:"truncation-step"`
	MaxRounds            int    `yaml:"max-rounds" json:"max-rounds"`
	MinTier              int    `yaml:"min-tier" json:"min-tier"`
	MaxTier              int    `yaml:"max-tier" json:"max-tier"`
	MaxTotalOutputTokens int64  `yaml:"max-total-output-tokens" json:"max-total-output-tokens"`
	MarkerText           string `yaml:"marker-text" json:"marker-text"`
}

// CodexTLSProfileConfig configures transport-specific Codex uTLS profiles.
type CodexTLSProfileConfig struct {
	HTTPS     string `yaml:"https" json:"https"`
	Websocket string `yaml:"websocket" json:"websocket"`
}

// CommandCodeKey represents the configuration for a Command Code API key.
type CommandCodeKey struct {
	APIKey         string             `yaml:"api-key" json:"api-key"`
	Label          string             `yaml:"label,omitempty" json:"label,omitempty"`
	Priority       int                `yaml:"priority,omitempty" json:"priority,omitempty"`
	Prefix         string             `yaml:"prefix,omitempty" json:"prefix,omitempty"`
	BaseURL        string             `yaml:"base-url,omitempty" json:"base-url,omitempty"`
	ProxyURL       string             `yaml:"proxy-url,omitempty" json:"proxy-url,omitempty"`
	Models         []CommandCodeModel `yaml:"models,omitempty" json:"models,omitempty"`
	Headers        map[string]string  `yaml:"headers,omitempty" json:"headers,omitempty"`
	ExcludedModels []string           `yaml:"excluded-models,omitempty" json:"excluded-models,omitempty"`
	DisableCooling bool               `yaml:"disable-cooling,omitempty" json:"disable-cooling,omitempty"`
}

func (k CommandCodeKey) GetAPIKey() string { return k.APIKey }

func (k CommandCodeKey) GetBaseURL() string { return k.BaseURL }

func (k CommandCodeKey) GetPrefix() string { return k.Prefix }

func (k CommandCodeKey) GetProxyURL() string { return k.ProxyURL }

// CommandCodeModel describes a mapping between an alias and the upstream model name.
type CommandCodeModel struct {
	Name         string `yaml:"name" json:"name"`
	Alias        string `yaml:"alias" json:"alias"`
	DisplayName  string `yaml:"display-name,omitempty" json:"display-name,omitempty"`
	ForceMapping bool   `yaml:"force-mapping,omitempty" json:"force-mapping,omitempty"`
}

func (m CommandCodeModel) GetName() string { return m.Name }

func (m CommandCodeModel) GetAlias() string { return m.Alias }

func (m CommandCodeModel) GetDisplayName() string { return m.DisplayName }

func (m CommandCodeModel) GetForceMapping() bool { return m.ForceMapping }
