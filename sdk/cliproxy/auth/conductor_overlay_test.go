package auth

import (
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestResolveAPIKeyModelAliasWithResult_CommandCodeForceMapping(t *testing.T) {
	cfg := &internalconfig.Config{CommandCodeKey: []internalconfig.CommandCodeKey{{
		APIKey:  "commandcode-key",
		BaseURL: "https://commandcode.example",
		Models: []internalconfig.CommandCodeModel{{
			Name:         "anthropic/claude-sonnet-4.5",
			Alias:        "sonnet",
			ForceMapping: true,
		}},
	}}}
	auth := &Auth{
		ID:       "commandcode-auth",
		Provider: "commandcode",
		Attributes: map[string]string{
			"api_key":  "commandcode-key",
			"base_url": "https://commandcode.example",
		},
	}

	result := resolveAPIKeyModelAliasWithResult(cfg, auth, "sonnet")
	if result.UpstreamModel != "anthropic/claude-sonnet-4.5" || !result.ForceMapping || result.OriginalAlias != "sonnet" {
		t.Fatalf("resolveAPIKeyModelAliasWithResult() = %+v", result)
	}
}

func TestIsOpenAICompatAPIKeyAuth(t *testing.T) {
	tests := []struct {
		name string
		auth *Auth
		want bool
	}{
		{name: "nil", auth: nil},
		{name: "compat API key", auth: &Auth{Provider: "openai-compatibility", Attributes: map[string]string{"api_key": "key"}}, want: true},
		{name: "named compat API key", auth: &Auth{Provider: "custom", Attributes: map[string]string{"api_key": "key", "compat_name": "custom"}}, want: true},
		{name: "keyless config compat", auth: &Auth{Provider: "openai-compatibility", Attributes: map[string]string{"source": "config", "compat_name": "custom"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isOpenAICompatAPIKeyAuth(test.auth); got != test.want {
				t.Fatalf("isOpenAICompatAPIKeyAuth() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestIsRequestScopedNotFoundMessage(t *testing.T) {
	message := "Item with id 'rs_test' not found. Items are not persisted when `store` is set to false."
	if !isRequestScopedNotFoundMessage(message) {
		t.Fatal("request-scoped item miss was not recognized")
	}
	if isRequestScopedNotFoundMessage("model test was not found") {
		t.Fatal("model miss was classified as request-scoped")
	}
}

func TestTriedPredicate(t *testing.T) {
	predicate := triedPredicate(map[string]struct{}{"used": {}})
	if predicate(nil) || predicate(&scheduledAuth{}) || predicate(&scheduledAuth{auth: &Auth{ID: "used"}}) {
		t.Fatal("triedPredicate accepted an invalid or previously used auth")
	}
	if !predicate(&scheduledAuth{auth: &Auth{ID: "fresh"}}) {
		t.Fatal("triedPredicate rejected a fresh auth")
	}
}
