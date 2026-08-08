package cliproxy

import "testing"

// These legacy fork regression entry points remain part of the upstream-sync
// symbol contract. The Original service split made the execution registry
// run-owned, so they delegate to the equivalent replacement-path coverage.
func TestServiceReplacesRegistryOnlyAfterNewSubscriptionAck(t *testing.T) {
	TestServiceKeepsRegistryAcrossHeartbeatFailoverAndExposesOnlyAfterNewACK(t)
}

func TestServiceDrainsBeforePreAckRetriesAndExposesOnlyAfterNewAck(t *testing.T) {
	TestServiceAmbiguousDispatchDrainsRegistryBeforeRetry(t)
}

func TestServiceCancelsRunWhenBlockingScopeExceedsDrainBound(t *testing.T) {
	TestServiceExplicitReplacementCancelsRunWhenDrainTimesOut(t)
}

func TestServiceHeartbeatLossCancelsBlockedConfigFinalizationBeforeDrain(t *testing.T) {
	TestServiceHeartbeatLossCancelsBlockedConfigFinalizationWithoutDrainingRegistry(t)
}
