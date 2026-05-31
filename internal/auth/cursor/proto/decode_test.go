package proto

import (
	"bytes"
	"encoding/json"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func appendTestBytesField(buf []byte, fieldNumber int, value []byte) []byte {
	buf = protowire.AppendTag(buf, protowire.Number(fieldNumber), protowire.BytesType)
	return protowire.AppendBytes(buf, value)
}

func appendTestUint32Field(buf []byte, fieldNumber int, value uint32) []byte {
	buf = protowire.AppendTag(buf, protowire.Number(fieldNumber), protowire.VarintType)
	return protowire.AppendVarint(buf, uint64(value))
}

func TestDecodeAgentServerMessageDecodesReadLintsToolCallStarted(t *testing.T) {
	var args []byte
	args = appendTestBytesField(args, 1, []byte("AGENTS.md"))
	args = appendTestBytesField(args, 1, []byte("internal/auth/cursor/proto/decode.go"))

	var readLintsCall []byte
	readLintsCall = appendTestBytesField(readLintsCall, 1, args)

	var toolCall []byte
	toolCall = appendTestBytesField(toolCall, 14, readLintsCall)

	var started []byte
	started = appendTestBytesField(started, 1, []byte("call_read_lints"))
	started = appendTestBytesField(started, 2, toolCall)

	var interaction []byte
	interaction = appendTestBytesField(interaction, 2, started)

	var agentServer []byte
	agentServer = appendTestBytesField(agentServer, ASM_InteractionUpdate, interaction)

	msg, err := DecodeAgentServerMessage(agentServer)
	if err != nil {
		t.Fatalf("DecodeAgentServerMessage() error = %v", err)
	}
	if msg.Type != ServerMsgExecMcpArgs {
		t.Fatalf("Type = %v, want ServerMsgExecMcpArgs", msg.Type)
	}
	if msg.McpToolName != "readLints" {
		t.Fatalf("McpToolName = %q, want readLints", msg.McpToolName)
	}
	if msg.McpToolCallId != "call_read_lints" {
		t.Fatalf("McpToolCallId = %q, want call_read_lints", msg.McpToolCallId)
	}
	if !msg.InteractionToolCall {
		t.Fatal("InteractionToolCall = false, want true")
	}
	var paths []string
	if err := json.Unmarshal(msg.McpArgs["paths"], &paths); err != nil {
		t.Fatalf("paths args are not JSON: %v; raw=%q", err, msg.McpArgs["paths"])
	}
	want := []string{"AGENTS.md", "internal/auth/cursor/proto/decode.go"}
	if !sliceStringsEqual(paths, want) {
		t.Fatalf("paths = %#v, want %#v", paths, want)
	}
}

func TestDecodeAgentServerMessageDecodesPartialToolCallArgsDeltaWithoutToolCallPayload(t *testing.T) {
	var partial []byte
	partial = appendTestBytesField(partial, 1, []byte("call_grep"))
	partial = appendTestBytesField(partial, 3, []byte(`{"pattern":"InteractionUpdate"}`))

	var interaction []byte
	interaction = appendTestBytesField(interaction, IU_PartialToolCall, partial)

	var agentServer []byte
	agentServer = appendTestBytesField(agentServer, ASM_InteractionUpdate, interaction)

	msg, err := DecodeAgentServerMessage(agentServer)
	if err != nil {
		t.Fatalf("DecodeAgentServerMessage() error = %v", err)
	}
	if msg.Type != ServerMsgExecMcpArgs {
		t.Fatalf("Type = %v, want ServerMsgExecMcpArgs", msg.Type)
	}
	if !msg.InteractionToolCall {
		t.Fatal("InteractionToolCall = false, want true")
	}
	if msg.McpToolCallId != "call_grep" {
		t.Fatalf("McpToolCallId = %q, want call_grep", msg.McpToolCallId)
	}
	if msg.InteractionArgsTextDelta != `{"pattern":"InteractionUpdate"}` {
		t.Fatalf("InteractionArgsTextDelta = %q", msg.InteractionArgsTextDelta)
	}
}

func TestDecodeAgentServerMessageIgnoresCompletedToolCallWithResult(t *testing.T) {
	var args []byte
	args = appendTestBytesField(args, 1, []byte("AGENTS.md"))

	var success []byte
	success = appendTestBytesField(success, 1, []byte("[]"))

	var result []byte
	result = appendTestBytesField(result, 1, success)

	var readLintsCall []byte
	readLintsCall = appendTestBytesField(readLintsCall, 1, args)
	readLintsCall = appendTestBytesField(readLintsCall, 2, result)

	var toolCall []byte
	toolCall = appendTestBytesField(toolCall, 14, readLintsCall)

	var completed []byte
	completed = appendTestBytesField(completed, 1, []byte("call_read_lints"))
	completed = appendTestBytesField(completed, 2, toolCall)

	var interaction []byte
	interaction = appendTestBytesField(interaction, 3, completed)

	var agentServer []byte
	agentServer = appendTestBytesField(agentServer, ASM_InteractionUpdate, interaction)

	msg, err := DecodeAgentServerMessage(agentServer)
	if err != nil {
		t.Fatalf("DecodeAgentServerMessage() error = %v", err)
	}
	if msg.Type != ServerMsgUnknown {
		t.Fatalf("Type = %v, want ServerMsgUnknown for result-bearing completion", msg.Type)
	}
}

func sliceStringsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDecodeAgentServerMessageCapturesKVRequestMetadata(t *testing.T) {
	blobID := bytes.Repeat([]byte{0xab}, 32)
	metadata := []byte("opaque-request-metadata")

	var getBlobArgs []byte
	getBlobArgs = appendTestBytesField(getBlobArgs, GBA_BlobId, blobID)

	var kvServer []byte
	kvServer = appendTestUint32Field(kvServer, KSM_Id, 7)
	kvServer = appendTestBytesField(kvServer, KSM_GetBlobArgs, getBlobArgs)
	kvServer = appendTestBytesField(kvServer, KSM_RequestMetadata, metadata)

	var agentServer []byte
	agentServer = appendTestBytesField(agentServer, ASM_KvServerMessage, kvServer)

	msg, err := DecodeAgentServerMessage(agentServer)
	if err != nil {
		t.Fatalf("DecodeAgentServerMessage() error = %v", err)
	}
	if msg.Type != ServerMsgKvGetBlob {
		t.Fatalf("Type = %v, want ServerMsgKvGetBlob", msg.Type)
	}
	if msg.KvId != 7 {
		t.Fatalf("KvId = %d, want 7", msg.KvId)
	}
	if !bytes.Equal(msg.BlobId, blobID) {
		t.Fatalf("BlobId = %x, want %x", msg.BlobId, blobID)
	}
	if !bytes.Equal(msg.RequestMetadata, metadata) {
		t.Fatalf("RequestMetadata = %q, want %q", msg.RequestMetadata, metadata)
	}
}

func TestDecodeAgentServerMessageCapturesKVSetRequestMetadata(t *testing.T) {
	blobID := bytes.Repeat([]byte{0xcd}, 32)
	blobData := []byte("assistant turn")
	metadata := []byte("set-metadata")

	var setBlobArgs []byte
	setBlobArgs = appendTestBytesField(setBlobArgs, SBA_BlobId, blobID)
	setBlobArgs = appendTestBytesField(setBlobArgs, SBA_BlobData, blobData)

	var kvServer []byte
	kvServer = appendTestUint32Field(kvServer, KSM_Id, 9)
	kvServer = appendTestBytesField(kvServer, KSM_SetBlobArgs, setBlobArgs)
	kvServer = appendTestBytesField(kvServer, KSM_RequestMetadata, metadata)

	var agentServer []byte
	agentServer = appendTestBytesField(agentServer, ASM_KvServerMessage, kvServer)

	msg, err := DecodeAgentServerMessage(agentServer)
	if err != nil {
		t.Fatalf("DecodeAgentServerMessage() error = %v", err)
	}
	if msg.Type != ServerMsgKvSetBlob {
		t.Fatalf("Type = %v, want ServerMsgKvSetBlob", msg.Type)
	}
	if !bytes.Equal(msg.RequestMetadata, metadata) {
		t.Fatalf("RequestMetadata = %q, want %q", msg.RequestMetadata, metadata)
	}
}
