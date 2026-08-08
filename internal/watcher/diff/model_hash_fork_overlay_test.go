package diff

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

// The historical fork test name is retained for symbol-survival checks. The
// current routing hash intentionally normalizes entries but preserves their
// order and duplicates.
func TestComputeOpenAICompatModelsHash_NormalizesAndDedups(t *testing.T) {
	normalized := []config.OpenAICompatibilityModel{
		{Name: " GPT-4 ", Alias: " GPT4 "},
		{Name: " ", Alias: " "},
	}
	canonical := []config.OpenAICompatibilityModel{{Name: "gpt-4", Alias: "gpt4"}}
	if h1, h2 := ComputeOpenAICompatModelsHash(normalized), ComputeOpenAICompatModelsHash(canonical); h1 == "" || h1 != h2 {
		t.Fatalf("expected case, whitespace, and blank entries to normalize, got %q / %q", h1, h2)
	}

	duplicated := append(append([]config.OpenAICompatibilityModel{}, canonical...), canonical[0])
	if h1, h2 := ComputeOpenAICompatModelsHash(duplicated), ComputeOpenAICompatModelsHash(canonical); h1 == h2 {
		t.Fatalf("expected duplicate routing entries to remain significant, got %q", h1)
	}
}

// The historical fork test name is retained for symbol-survival checks. The
// current routing hash ignores blank entries but preserves order and duplicates.
func TestComputeVertexCompatModelsHash_IgnoresBlankAndOrder(t *testing.T) {
	normalized := []config.VertexCompatModel{
		{Name: " M1 ", Alias: " A1 "},
		{Name: " ", Alias: " "},
	}
	canonical := []config.VertexCompatModel{{Name: "m1", Alias: "a1"}}
	if h1, h2 := ComputeVertexCompatModelsHash(normalized), ComputeVertexCompatModelsHash(canonical); h1 == "" || h1 != h2 {
		t.Fatalf("expected case, whitespace, and blank entries to normalize, got %q / %q", h1, h2)
	}

	ordered := []config.VertexCompatModel{{Name: "m1"}, {Name: "m2"}}
	reversed := []config.VertexCompatModel{{Name: "m2"}, {Name: "m1"}}
	if h1, h2 := ComputeVertexCompatModelsHash(ordered), ComputeVertexCompatModelsHash(reversed); h1 == h2 {
		t.Fatalf("expected routing order to remain significant, got %q", h1)
	}
}

// The historical fork test name is retained for symbol-survival checks. The
// current routing hash ignores blank entries but preserves duplicates.
func TestComputeClaudeModelsHash_IgnoresBlankAndDedup(t *testing.T) {
	normalized := []config.ClaudeModel{
		{Name: " M1 ", Alias: " A1 "},
		{Name: " ", Alias: " "},
	}
	canonical := []config.ClaudeModel{{Name: "m1", Alias: "a1"}}
	if h1, h2 := ComputeClaudeModelsHash(normalized), ComputeClaudeModelsHash(canonical); h1 == "" || h1 != h2 {
		t.Fatalf("expected case, whitespace, and blank entries to normalize, got %q / %q", h1, h2)
	}

	duplicated := append(append([]config.ClaudeModel{}, canonical...), canonical[0])
	if h1, h2 := ComputeClaudeModelsHash(duplicated), ComputeClaudeModelsHash(canonical); h1 == h2 {
		t.Fatalf("expected duplicate routing entries to remain significant, got %q", h1)
	}
}

// The historical fork test name is retained for symbol-survival checks. The
// current routing hash ignores blank entries but preserves duplicates.
func TestComputeCodexModelsHash_IgnoresBlankAndDedup(t *testing.T) {
	normalized := []config.CodexModel{
		{Name: " M1 ", Alias: " A1 "},
		{Name: " ", Alias: " "},
	}
	canonical := []config.CodexModel{{Name: "m1", Alias: "a1"}}
	if h1, h2 := ComputeCodexModelsHash(normalized), ComputeCodexModelsHash(canonical); h1 == "" || h1 != h2 {
		t.Fatalf("expected case, whitespace, and blank entries to normalize, got %q / %q", h1, h2)
	}

	duplicated := append(append([]config.CodexModel{}, canonical...), canonical[0])
	if h1, h2 := ComputeCodexModelsHash(duplicated), ComputeCodexModelsHash(canonical); h1 == h2 {
		t.Fatalf("expected duplicate routing entries to remain significant, got %q", h1)
	}
}
