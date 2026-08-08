package handlers

import (
	"strings"

	"golang.org/x/net/context"
)

type excludedAuthIDsContextKey struct{}

// WithExcludedAuthIDs returns a child context with normalized turn-local auth exclusions.
func WithExcludedAuthIDs(ctx context.Context, authIDs []string) context.Context {
	normalized := normalizeExcludedAuthIDs(authIDs)
	if len(normalized) == 0 {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, excludedAuthIDsContextKey{}, normalized)
}

func normalizeExcludedAuthIDs(authIDs []string) []string {
	seen := make(map[string]struct{}, len(authIDs))
	normalized := make([]string, 0, len(authIDs))
	for _, authID := range authIDs {
		authID = strings.TrimSpace(authID)
		if authID == "" {
			continue
		}
		if _, ok := seen[authID]; ok {
			continue
		}
		seen[authID] = struct{}{}
		normalized = append(normalized, authID)
	}
	return normalized
}

func excludedAuthIDsFromContext(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	excluded, ok := ctx.Value(excludedAuthIDsContextKey{}).([]string)
	if !ok || len(excluded) == 0 {
		return nil
	}
	return append([]string(nil), excluded...)
}
