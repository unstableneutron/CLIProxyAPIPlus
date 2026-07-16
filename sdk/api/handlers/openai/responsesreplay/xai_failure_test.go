package responsesreplay

import (
	"net/http"
	"testing"
)

func TestClassifyFailureForRequestRecognizesXAIMissingPreviousResponse(t *testing.T) {
	const responseID = "25a6b917-9417-9fa4-a21a-1e097d64a96b-xai-13"
	request := []byte(`{"model":"grok-4.3","previous_response_id":"` + responseID + `","input":[]}`)
	errorPayload := `{"type":"error","status":500,"error":{"message":"gRPC error: Response with id=` + responseID + ` not found","type":"api_error"}}`

	if got := ClassifyFailureForRequest(http.StatusInternalServerError, errorPayload, request); got != FailurePreviousResponseMissing {
		t.Fatalf("ClassifyFailureForRequest() = %v, want %v", got, FailurePreviousResponseMissing)
	}
}

func TestClassifyFailureForRequestRejectsUnmatchedXAIResponseStateErrors(t *testing.T) {
	const errorPayload = `{"type":"error","status":500,"error":{"message":"gRPC error: Response with id=resp-stale not found","type":"api_error"}}`
	tests := []struct {
		name    string
		request []byte
	}{
		{name: "missing previous response", request: []byte(`{"input":[]}`)},
		{name: "different previous response", request: []byte(`{"previous_response_id":"resp-other","input":[]}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyFailureForRequest(http.StatusInternalServerError, errorPayload, test.request); got != FailureAuthOrRoute {
				t.Fatalf("ClassifyFailureForRequest() = %v, want %v", got, FailureAuthOrRoute)
			}
		})
	}
}
