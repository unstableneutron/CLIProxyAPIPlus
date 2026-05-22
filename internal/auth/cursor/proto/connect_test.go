package proto

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"testing"
)

func TestParseConnectFrameDecompressesGzipPayload(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write([]byte("hello cursor")); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	frame := make([]byte, ConnectFrameHeaderSize+compressed.Len())
	frame[0] = ConnectCompressionFlag
	binary.BigEndian.PutUint32(frame[1:5], uint32(compressed.Len()))
	copy(frame[5:], compressed.Bytes())

	flags, payload, consumed, ok := ParseConnectFrame(frame)
	if !ok {
		t.Fatal("ParseConnectFrame() ok = false, want true")
	}
	if consumed != len(frame) {
		t.Fatalf("consumed = %d, want %d", consumed, len(frame))
	}
	if flags&ConnectCompressionFlag == 0 {
		t.Fatalf("flags = 0x%02x, want compression flag", flags)
	}
	if string(payload) != "hello cursor" {
		t.Fatalf("payload = %q, want decompressed payload", string(payload))
	}
}
