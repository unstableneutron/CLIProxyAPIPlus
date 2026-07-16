package responsesreplay

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
)

const (
	xaiMissingResponsePrefix = "gRPC error: Response with id="
	xaiMissingResponseSuffix = " not found"
)

// ClassifyFailureForRequest refines failures whose meaning depends on the
// attempted request. xAI reports a stale previous response as a generic 500,
// so only the exact structured response-ID error for the attempted ID is
// eligible for state reconciliation.
func ClassifyFailureForRequest(status int, message string, request []byte) FailureKind {
	kind := ClassifyFailure(status, message)
	if kind != FailureAuthOrRoute || !isXAIMissingPreviousResponse(status, message, request) {
		return kind
	}
	return FailurePreviousResponseMissing
}

func isXAIMissingPreviousResponse(status int, message string, request []byte) bool {
	if status < http.StatusInternalServerError {
		return false
	}
	previousResponseID := strings.TrimSpace(gjson.GetBytes(request, "previous_response_id").String())
	if previousResponseID == "" {
		return false
	}

	message = strings.TrimSpace(message)
	if !gjson.Valid(message) {
		return false
	}
	errorPayload := []byte(message)
	if strings.TrimSpace(gjson.GetBytes(errorPayload, "error.type").String()) != "api_error" {
		return false
	}
	errorMessage := strings.TrimSpace(gjson.GetBytes(errorPayload, "error.message").String())
	responseID, ok := strings.CutPrefix(errorMessage, xaiMissingResponsePrefix)
	if !ok {
		return false
	}
	responseID, ok = strings.CutSuffix(responseID, xaiMissingResponseSuffix)
	return ok && strings.TrimSpace(responseID) == previousResponseID
}
