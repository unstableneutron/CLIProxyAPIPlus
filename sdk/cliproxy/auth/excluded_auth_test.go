package auth

import (
	"reflect"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestExcludedAuthIDsFromMetadataNormalizesAndCopies(t *testing.T) {
	metadata := map[string]any{
		cliproxyexecutor.ExcludedAuthIDsMetadataKey: []string{" auth-a ", "", "auth-b", "auth-a"},
	}
	got := excludedAuthIDsFromMetadata(metadata)
	if want := []string{"auth-a", "auth-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("excluded auth IDs = %#v, want %#v", got, want)
	}
	got[0] = "mutated"
	if source := metadata[cliproxyexecutor.ExcludedAuthIDsMetadataKey].([]string); source[0] != " auth-a " {
		t.Fatalf("metadata source mutated: %#v", source)
	}
}

func TestSeedTriedWithExcludedAuthIDsDoesNotIncrementAttempted(t *testing.T) {
	tried := map[string]struct{}{}
	attempted := map[string]struct{}{}
	seedTriedWithExcludedAuthIDs(tried, map[string]any{
		cliproxyexecutor.ExcludedAuthIDsMetadataKey: []string{"auth-a", "auth-b"},
	})
	if _, ok := tried["auth-a"]; !ok {
		t.Fatal("auth-a was not seeded into tried")
	}
	if len(attempted) != 0 {
		t.Fatalf("pre-seeded exclusions consumed retry accounting: %#v", attempted)
	}
}
