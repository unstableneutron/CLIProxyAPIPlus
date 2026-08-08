package handlers

import (
	"context"
	"reflect"
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestWithAdditionalSelectedAuthIDCallbackComposesInOrder(t *testing.T) {
	calls := make([]string, 0, 2)
	ctx := WithSelectedAuthIDCallback(context.Background(), func(authID string) {
		calls = append(calls, "existing:"+authID)
	})
	ctx = WithAdditionalSelectedAuthIDCallback(ctx, func(authID string) {
		calls = append(calls, "additional:"+authID)
	})

	callback := selectedAuthIDCallbackFromContext(ctx)
	if callback == nil {
		t.Fatal("selected auth callback missing")
	}
	callback("auth-one")

	want := []string{"existing:auth-one", "additional:auth-one"}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("callback calls = %v, want %v", calls, want)
	}
}

func TestWithResponsesStateModeCopiesNormalizedModeIntoExecutionMetadata(t *testing.T) {
	ctx := WithResponsesStateMode(nil, "  probe  ")
	metadata := requestExecutionMetadata(ctx)

	if got := metadata[coreexecutor.ResponsesStateModeMetadataKey]; got != coreexecutor.ResponsesStateModeProbe {
		t.Fatalf("responses state mode = %v, want %q", got, coreexecutor.ResponsesStateModeProbe)
	}
}

func TestWithResponsesStateModeEmptyLeavesContextUnchanged(t *testing.T) {
	ctx := context.Background()
	if got := WithResponsesStateMode(ctx, "  "); got != ctx {
		t.Fatal("empty Responses state mode should leave the context unchanged")
	}
}
