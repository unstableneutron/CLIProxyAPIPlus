package handlers

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"golang.org/x/net/context"
)

func (h *BaseAPIHandler) recordSelectedUpstream(ctx context.Context, authID string) {
	if h == nil || h.AuthManager == nil {
		return
	}
	auth, ok := h.AuthManager.GetByID(strings.TrimSpace(authID))
	if !ok || auth == nil {
		return
	}
	logging.SetSlot(ctx, auth.EnsureIndex())
}
