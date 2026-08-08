package handlers

import (
	"strings"

	"golang.org/x/net/context"
)

type responsesStateModeContextKey struct{}

// WithResponsesStateMode passes an explicit Responses state mode to executors.
func WithResponsesStateMode(ctx context.Context, mode string) context.Context {
	mode = strings.TrimSpace(mode)
	if mode == "" {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, responsesStateModeContextKey{}, mode)
}

func responsesStateModeFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	raw := ctx.Value(responsesStateModeContextKey{})
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}
