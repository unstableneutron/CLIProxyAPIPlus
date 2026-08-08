package executor

import "context"

// Flush preserves the fork overlay symbol while using the terminal-aware replay
// commit semantics introduced by the accumulator refactor.
func (a *antigravityReasoningReplayAccumulator) Flush(ctx context.Context) {
	a.Commit(ctx)
}
