package helps

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"strings"
	"testing"
)

func TestForEachBedrockEventStreamPayloadExtractsPayloads(t *testing.T) {
	t.Parallel()

	stream := bytes.NewReader(append(bedrockEventStreamFrameForTest([]byte(`{"messageStart":{"role":"assistant"}}`)), bedrockEventStreamFrameForTest([]byte(`{"messageStop":{"stopReason":"end_turn"}}`))...))
	var got [][]byte
	err := ForEachBedrockEventStreamPayload(stream, func(payload []byte) bool {
		got = append(got, append([]byte(nil), payload...))
		return true
	})
	if err != nil {
		t.Fatalf("ForEachBedrockEventStreamPayload() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("payload count = %d, want 2", len(got))
	}
	if string(got[0]) != `{"messageStart":{"role":"assistant"}}` {
		t.Fatalf("first payload = %s", got[0])
	}
	if string(got[1]) != `{"messageStop":{"stopReason":"end_turn"}}` {
		t.Fatalf("second payload = %s", got[1])
	}
}

func TestBedrockStreamNormalizerClosesOpenedBlockIndex(t *testing.T) {
	t.Parallel()

	normalizer := NewBedrockStreamNormalizer("model")
	var joined []byte
	for _, line := range normalizer.ConvertLine([]byte(`{"contentBlockDelta":{"contentBlockIndex":2,"delta":{"text":"later"}}}`)) {
		joined = append(joined, line...)
		joined = append(joined, '\n')
	}
	for _, line := range normalizer.Finish() {
		joined = append(joined, line...)
		joined = append(joined, '\n')
	}
	if !bytes.Contains(joined, []byte(`"index":2,"type":"content_block_stop"`)) {
		t.Fatalf("stream did not close opened block index 2: %s", joined)
	}
}

func TestBedrockStreamNormalizerTranslatesToolUse(t *testing.T) {
	t.Parallel()

	normalizer := NewBedrockStreamNormalizer("model")
	var joined []byte
	for _, payload := range [][]byte{
		[]byte(`{"contentBlockStart":{"contentBlockIndex":0,"start":{"toolUse":{"toolUseId":"toolu_1","name":"lookup"}}}}`),
		[]byte(`{"contentBlockDelta":{"contentBlockIndex":0,"delta":{"toolUse":{"input":"{\"city\":\"sf\"}"}}}}`),
		[]byte(`{"contentBlockStop":{"contentBlockIndex":0}}`),
		[]byte(`{"messageStop":{"stopReason":"tool_use"}}`),
	} {
		for _, line := range normalizer.ConvertLine(payload) {
			joined = append(joined, line...)
			joined = append(joined, '\n')
		}
	}
	for _, line := range normalizer.Finish() {
		joined = append(joined, line...)
		joined = append(joined, '\n')
	}
	if !bytes.Contains(joined, []byte(`"type":"tool_use"`)) {
		t.Fatalf("stream did not include tool_use start: %s", joined)
	}
	if !bytes.Contains(joined, []byte(`"partial_json":"{\"city\":\"sf\"}"`)) {
		t.Fatalf("stream did not include tool input delta: %s", joined)
	}
	if !bytes.Contains(joined, []byte(`"stop_reason":"tool_use"`)) {
		t.Fatalf("stream did not include tool_use stop reason: %s", joined)
	}
}

func TestForEachBedrockEventStreamPayloadRejectsOversizedFrame(t *testing.T) {
	t.Parallel()

	frame := make([]byte, 16)
	binary.BigEndian.PutUint32(frame[0:4], maxBedrockEventStreamFrameSize+1)
	err := ForEachBedrockEventStreamPayload(bytes.NewReader(frame), func([]byte) bool {
		t.Fatal("oversized frame must not emit payload")
		return true
	})
	if err == nil || !strings.Contains(err.Error(), "frame too large") {
		t.Fatalf("error = %v, want frame too large", err)
	}
}

func TestForEachBedrockEventStreamMessageReturnsHeaders(t *testing.T) {
	t.Parallel()

	stream := bytes.NewReader(bedrockEventStreamFrameWithHeadersForTest(map[string]string{
		":message-type":   "exception",
		":exception-type": "throttlingException",
	}, []byte(`{"message":"slow down"}`)))
	var got BedrockEventStreamMessage
	err := ForEachBedrockEventStreamMessage(stream, func(msg BedrockEventStreamMessage) bool {
		got = msg
		return true
	})
	if err != nil {
		t.Fatalf("ForEachBedrockEventStreamMessage() error = %v", err)
	}
	if got.Headers[":message-type"] != "exception" {
		t.Fatalf("message type = %q, want exception", got.Headers[":message-type"])
	}
	if got.Headers[":exception-type"] != "throttlingException" {
		t.Fatalf("exception type = %q, want throttlingException", got.Headers[":exception-type"])
	}
	if string(got.Payload) != `{"message":"slow down"}` {
		t.Fatalf("payload = %s", got.Payload)
	}
}

func TestForEachBedrockEventStreamPayloadRejectsBadPreludeCRC(t *testing.T) {
	t.Parallel()

	frame := bedrockEventStreamFrameForTest([]byte(`{"messageStart":{"role":"assistant"}}`))
	frame[11] ^= 0xff

	err := ForEachBedrockEventStreamPayload(bytes.NewReader(frame), func([]byte) bool {
		t.Fatal("bad prelude CRC must not emit payload")
		return true
	})
	if err == nil || !strings.Contains(err.Error(), "prelude checksum") {
		t.Fatalf("error = %v, want prelude checksum error", err)
	}
}

func TestForEachBedrockEventStreamPayloadRejectsBadMessageCRC(t *testing.T) {
	t.Parallel()

	frame := bedrockEventStreamFrameForTest([]byte(`{"messageStart":{"role":"assistant"}}`))
	frame[len(frame)-1] ^= 0xff

	err := ForEachBedrockEventStreamPayload(bytes.NewReader(frame), func([]byte) bool {
		t.Fatal("bad message CRC must not emit payload")
		return true
	})
	if err == nil || !strings.Contains(err.Error(), "message checksum") {
		t.Fatalf("error = %v, want message checksum error", err)
	}
}

func bedrockEventStreamFrameForTest(payload []byte) []byte {
	return bedrockEventStreamFrameWithHeadersForTest(nil, payload)
}

func bedrockEventStreamFrameWithHeadersForTest(values map[string]string, payload []byte) []byte {
	var headers []byte
	for name, value := range values {
		headers = append(headers, byte(len(name)))
		headers = append(headers, name...)
		headers = append(headers, 7)
		headers = binary.BigEndian.AppendUint16(headers, uint16(len(value)))
		headers = append(headers, value...)
	}
	totalLen := uint32(16 + len(headers) + len(payload))
	frame := make([]byte, totalLen)
	binary.BigEndian.PutUint32(frame[0:4], totalLen)
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	binary.BigEndian.PutUint32(frame[8:12], crc32.ChecksumIEEE(frame[0:8]))
	copy(frame[12:], headers)
	copy(frame[12+len(headers):], payload)
	binary.BigEndian.PutUint32(frame[len(frame)-4:], crc32.ChecksumIEEE(frame[:len(frame)-4]))
	return frame
}
