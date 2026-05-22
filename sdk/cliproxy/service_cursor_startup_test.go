package cliproxy

import (
	"context"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

type serviceStartupMemoryStore struct {
	auths []*coreauth.Auth
}

func (s *serviceStartupMemoryStore) List(context.Context) ([]*coreauth.Auth, error) {
	out := make([]*coreauth.Auth, 0, len(s.auths))
	for _, auth := range s.auths {
		out = append(out, auth.Clone())
	}
	return out, nil
}

func (s *serviceStartupMemoryStore) Save(context.Context, *coreauth.Auth) (string, error) {
	return "", nil
}

func (s *serviceStartupMemoryStore) Delete(context.Context, string) error {
	return nil
}

func TestRegisterLoadedAuthModelsRegistersCursorModelsAfterStoreLoad(t *testing.T) {
	auth := &coreauth.Auth{
		ID:       "cursor-startup.json",
		Provider: "cursor",
		Status:   coreauth.StatusActive,
	}
	manager := coreauth.NewManager(&serviceStartupMemoryStore{auths: []*coreauth.Auth{auth}}, nil, nil)
	service := &Service{cfg: &config.Config{}, coreManager: manager}

	reg := registry.GetGlobalRegistry()
	reg.UnregisterClient(auth.ID)
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })

	if err := manager.Load(context.Background()); err != nil {
		t.Fatalf("manager.Load() error = %v", err)
	}
	if got := reg.GetModelsForClient(auth.ID); len(got) != 0 {
		t.Fatalf("models registered before startup registration = %+v, want none", got)
	}

	service.registerLoadedAuthModels(context.Background())

	models := reg.GetModelsForClient(auth.ID)
	seenPrefixed := false
	seenRaw := false
	for _, model := range models {
		switch model.ID {
		case "cursor-composer-2.5":
			seenPrefixed = true
		case "composer-2.5":
			seenRaw = true
		}
	}
	if !seenPrefixed || !seenRaw {
		t.Fatalf("expected prefixed and raw Cursor model aliases after startup registration, got %+v", models)
	}
}
