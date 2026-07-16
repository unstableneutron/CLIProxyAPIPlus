package responsesreplay

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

// ClassifyFailureForRequest refines failures whose meaning depends on the
// attempted request. xAI reports a stale previous response as a generic 500,
// so only the exact structured response-ID error for the attempted ID is
// eligible for state reconciliation.
func ClassifyFailureForRequest(status int, message string, request []byte) FailureKind {
	kind := ClassifyFailure(status, message)
	if kind != FailureAuthOrRoute || !helps.IsXAIMissingPreviousResponseError(status, message, request) {
		return kind
	}
	return FailurePreviousResponseMissing
}
