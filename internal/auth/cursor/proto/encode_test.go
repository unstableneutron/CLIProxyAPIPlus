package proto

import (
	"bytes"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func TestEncodeExecRequestContextResultOmitsTools(t *testing.T) {
	payload := EncodeExecRequestContextResult(5, "exec-ctx", []McpToolDef{{
		Name:        "get_weather",
		Description: "weather lookup",
		InputSchema: []byte(`{"type":"object"}`),
	}})

	if bytes.Contains(payload, []byte("get_weather")) {
		t.Fatalf("request_context ack should not echo tools; payload contains tool name: %x", payload)
	}
	execClient := mustFirstBytesField(t, payload, ACM_ExecClientMessage)
	if got := mustFirstStringField(t, execClient, ECM_ExecId); got != "exec-ctx" {
		t.Fatalf("exec id = %q, want exec-ctx", got)
	}
	_ = mustFirstBytesField(t, execClient, ECM_RequestContextResult)
}

func TestEncodeKvResponsesEchoRequestMetadata(t *testing.T) {
	metadata := []byte("opaque-request-metadata")

	getPayload := EncodeKvGetBlobResult(7, []byte("blob-data"), metadata)
	getClient := mustFirstBytesField(t, getPayload, ACM_KvClientMessage)
	if got := mustFirstStringField(t, getClient, KCM_RequestMetadata); got != string(metadata) {
		t.Fatalf("get metadata = %q, want %q", got, string(metadata))
	}

	setPayload := EncodeKvSetBlobResult(9, metadata)
	setClient := mustFirstBytesField(t, setPayload, ACM_KvClientMessage)
	if got := mustFirstStringField(t, setClient, KCM_RequestMetadata); got != string(metadata) {
		t.Fatalf("set metadata = %q, want %q", got, string(metadata))
	}
}

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
