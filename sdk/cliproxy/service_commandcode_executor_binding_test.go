package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestEnsureExecutorsForAuth_CommandCodeBindsIndependentExecutor(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "commandcode-auth-1",
		Provider: "commandcode",
		Status:   coreauth.StatusActive,
		Attributes: map[string]string{
			"api_key": "user_test",
		},
	}

	service.ensureExecutorsForAuth(auth)
	resolved, ok := service.coreManager.Executor("commandcode")
	if !ok || resolved == nil {
		t.Fatal("expected commandcode executor after bind")
	}
	if _, isCommandCode := resolved.(*executor.CommandCodeExecutor); !isCommandCode {
		t.Fatalf("executor type = %T, want *executor.CommandCodeExecutor", resolved)
	}
	if _, isOpenAICompat := resolved.(*executor.OpenAICompatExecutor); isOpenAICompat {
		t.Fatal("commandcode must not bind the generic OpenAI-compatible executor")
	}
}
