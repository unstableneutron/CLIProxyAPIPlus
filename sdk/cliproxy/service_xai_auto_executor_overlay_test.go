package cliproxy

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestEnsureExecutorsForAuth_XAIBindsAutoExecutor(t *testing.T) {
	service := &Service{
		cfg:         &config.Config{},
		coreManager: coreauth.NewManager(nil, nil, nil),
	}
	auth := &coreauth.Auth{
		ID:       "xai-auth-1",
		Provider: "xai",
		Status:   coreauth.StatusActive,
	}

	service.ensureExecutorsForAuth(auth)

	gotExecutor, ok := service.coreManager.Executor("xai")
	if !ok || gotExecutor == nil {
		t.Fatal("expected xai executor after bind")
	}
	if _, ok := gotExecutor.(*executor.XAIAutoExecutor); !ok {
		t.Fatalf("xai executor type = %T, want *executor.XAIAutoExecutor", gotExecutor)
	}
}
