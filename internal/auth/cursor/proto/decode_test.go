package proto

import (
	"bytes"
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
