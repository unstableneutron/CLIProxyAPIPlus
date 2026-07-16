package helps

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"
)

const (
	xaiMissingResponsePrefix   = "gRPC error: Response with id="
	xaiMissingResponseSuffix   = " not found"
	codexMissingResponsePrefix = "Previous response with id '"
	codexMissingResponseSuffix = "' not found."
)

// IsXAIMissingPreviousResponseError reports whether xAI rejected the exact
// previous_response_id carried by the attempted request.
func IsXAIMissingPreviousResponseError(status int, message string, request []byte) bool {
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

// MarkXAIMissingPreviousResponseRequestScoped prevents a stale provider-state
// error from cooling down an otherwise healthy credential. The wrapper keeps
// the provider payload, status, headers, and retry hint intact.
func MarkXAIMissingPreviousResponseRequestScoped(err error, request []byte) error {
	if err == nil {
		return nil
	}
	status := 0
	var statusCoder interface{ StatusCode() int }
	if errors.As(err, &statusCoder) && statusCoder != nil {
		status = statusCoder.StatusCode()
	}
	if !IsXAIMissingPreviousResponseError(status, err.Error(), request) {
		return err
	}
	return responseStateRequestScopedError{cause: err}
}

// IsCodexMissingPreviousResponseError reports whether Codex rejected the exact
// previous_response_id carried by the attempted request.
func IsCodexMissingPreviousResponseError(status int, message string, request []byte) bool {
	if status < http.StatusBadRequest {
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
	errorMessage := strings.TrimSpace(gjson.GetBytes([]byte(message), "error.message").String())
	responseID, ok := strings.CutPrefix(errorMessage, codexMissingResponsePrefix)
	if !ok {
		return false
	}
	responseID, ok = strings.CutSuffix(responseID, codexMissingResponseSuffix)
	return ok && strings.TrimSpace(responseID) == previousResponseID
}

// MarkCodexMissingPreviousResponseRequestScoped keeps stale Codex response
// state from cooling down the credential selected for a repairable request.
func MarkCodexMissingPreviousResponseRequestScoped(err error, request []byte) error {
	if err == nil {
		return nil
	}
	status := 0
	var statusCoder interface{ StatusCode() int }
	if errors.As(err, &statusCoder) && statusCoder != nil {
		status = statusCoder.StatusCode()
	}
	if !IsCodexMissingPreviousResponseError(status, err.Error(), request) {
		return err
	}
	return responseStateRequestScopedError{cause: err}
}

type responseStateRequestScopedError struct {
	cause error
}

func (e responseStateRequestScopedError) Error() string {
	if e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e responseStateRequestScopedError) Unwrap() error { return e.cause }

func (e responseStateRequestScopedError) IsRequestScoped() bool { return true }

func (e responseStateRequestScopedError) StatusCode() int {
	var statusCoder interface{ StatusCode() int }
	if errors.As(e.cause, &statusCoder) && statusCoder != nil {
		return statusCoder.StatusCode()
	}
	return 0
}

func (e responseStateRequestScopedError) Headers() http.Header {
	var headerProvider interface{ Headers() http.Header }
	if errors.As(e.cause, &headerProvider) && headerProvider != nil {
		return headerProvider.Headers()
	}
	return nil
}

func (e responseStateRequestScopedError) RetryAfter() *time.Duration {
	var retryProvider interface{ RetryAfter() *time.Duration }
	if errors.As(e.cause, &retryProvider) && retryProvider != nil {
		return retryProvider.RetryAfter()
	}
	return nil
}
