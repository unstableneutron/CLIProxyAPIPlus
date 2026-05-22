package proto

import (
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func TestEncodeRunRequestIncludesRequestedModelAndFastParameter(t *testing.T) {
	encoded := EncodeRunRequest(&RunRequestParams{
		ModelId:        "composer-2",
		SystemPrompt:   "system",
		UserText:       "hello",
		MessageId:      "message-1",
		ConversationId: "conversation-1",
		ModelParameters: []ModelParameter{
			{ID: "fast", Value: "true"},
		},
	})

	runRequest := mustFirstBytesField(t, encoded, ACM_RunRequest)
	requestedModel := mustFirstBytesField(t, runRequest, ARR_RequestedModel)
	if got := mustFirstStringField(t, requestedModel, RM_ModelId); got != "composer-2" {
		t.Fatalf("requested model id = %q, want composer-2", got)
	}
	parameter := mustFirstBytesField(t, requestedModel, RM_Parameters)
	if got := mustFirstStringField(t, parameter, RMP_Id); got != "fast" {
		t.Fatalf("parameter id = %q, want fast", got)
	}
	if got := mustFirstStringField(t, parameter, RMP_Value); got != "true" {
		t.Fatalf("parameter value = %q, want true", got)
	}
}

func mustFirstBytesField(t *testing.T, data []byte, wantField int) []byte {
	t.Helper()
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			t.Fatalf("ConsumeTag failed: %v", protowire.ParseError(n))
		}
		data = data[n:]
		if typ == protowire.BytesType {
			value, n := protowire.ConsumeBytes(data)
			if n < 0 {
				t.Fatalf("ConsumeBytes failed: %v", protowire.ParseError(n))
			}
			if int(num) == wantField {
				return value
			}
			data = data[n:]
			continue
		}
		n = protowire.ConsumeFieldValue(num, typ, data)
		if n < 0 {
			t.Fatalf("ConsumeFieldValue failed: %v", protowire.ParseError(n))
		}
		data = data[n:]
	}
	t.Fatalf("field %d not found", wantField)
	return nil
}

func mustFirstStringField(t *testing.T, data []byte, wantField int) string {
	t.Helper()
	return string(mustFirstBytesField(t, data, wantField))
}
