package handlers

import (
	"context"
	"reflect"
	"testing"

	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestWithExcludedAuthIDsCopiesNormalizedIDsIntoExecutionMetadata(t *testing.T) {
	ids := []string{" auth-a ", "", "auth-b", "auth-a"}
	ctx := WithExcludedAuthIDs(context.Background(), ids)
	ids[0] = "mutated"

	got, ok := requestExecutionMetadata(ctx)[coreexecutor.ExcludedAuthIDsMetadataKey].([]string)
	if !ok {
		t.Fatalf("excluded auth metadata type = %T, want []string", requestExecutionMetadata(ctx)[coreexecutor.ExcludedAuthIDsMetadataKey])
	}
	if want := []string{"auth-a", "auth-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("excluded auth IDs = %#v, want %#v", got, want)
	}
	got[0] = "mutated-again"
	if next := requestExecutionMetadata(ctx)[coreexecutor.ExcludedAuthIDsMetadataKey].([]string); next[0] != "auth-a" {
		t.Fatalf("execution metadata was not defensively copied: %#v", next)
	}
}
