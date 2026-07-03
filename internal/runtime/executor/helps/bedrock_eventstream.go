package helps

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"io"
)

const (
	minBedrockEventStreamFrameSize = 16
	maxBedrockEventStreamFrameSize = 10 << 20
)

type BedrockEventStreamMessage struct {
	Headers map[string]string
	Payload []byte
}

func ForEachBedrockEventStreamPayload(r io.Reader, emit func([]byte) bool) error {
	return ForEachBedrockEventStreamMessage(r, func(msg BedrockEventStreamMessage) bool {
		if len(msg.Payload) == 0 {
			return true
		}
		if emit == nil {
			return true
		}
		return emit(msg.Payload)
	})
}

func ForEachBedrockEventStreamMessage(r io.Reader, emit func(BedrockEventStreamMessage) bool) error {
	if r == nil {
		return nil
	}
	for {
		msg, err := readBedrockEventStreamMessage(r)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if len(msg.Payload) == 0 {
			continue
		}
		if emit != nil && !emit(msg) {
			return nil
		}
	}
}

func readBedrockEventStreamPayload(r io.Reader) ([]byte, error) {
	msg, err := readBedrockEventStreamMessage(r)
	if err != nil {
		return nil, err
	}
	return msg.Payload, nil
}

func readBedrockEventStreamMessage(r io.Reader) (BedrockEventStreamMessage, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(r, prelude[:]); err != nil {
		return BedrockEventStreamMessage{}, err
	}
	totalLen := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	if totalLen < minBedrockEventStreamFrameSize {
		return BedrockEventStreamMessage{}, fmt.Errorf("bedrock event stream frame too short: %d", totalLen)
	}
	if totalLen > maxBedrockEventStreamFrameSize {
		return BedrockEventStreamMessage{}, fmt.Errorf("bedrock event stream frame too large: %d", totalLen)
	}
	if headersLen > totalLen-minBedrockEventStreamFrameSize {
		return BedrockEventStreamMessage{}, fmt.Errorf("bedrock event stream headers too long: %d", headersLen)
	}
	if got, want := binary.BigEndian.Uint32(prelude[8:12]), crc32.ChecksumIEEE(prelude[0:8]); got != want {
		return BedrockEventStreamMessage{}, fmt.Errorf("bedrock event stream prelude checksum mismatch")
	}
	restLen := int(totalLen) - len(prelude)
	rest := make([]byte, restLen)
	if _, err := io.ReadFull(r, rest); err != nil {
		return BedrockEventStreamMessage{}, err
	}
	message := make([]byte, 0, len(prelude)+len(rest)-4)
	message = append(message, prelude[:]...)
	message = append(message, rest[:len(rest)-4]...)
	if got, want := binary.BigEndian.Uint32(rest[len(rest)-4:]), crc32.ChecksumIEEE(message); got != want {
		return BedrockEventStreamMessage{}, fmt.Errorf("bedrock event stream message checksum mismatch")
	}
	payloadLen := int(totalLen) - minBedrockEventStreamFrameSize - int(headersLen)
	if payloadLen <= 0 {
		return BedrockEventStreamMessage{Headers: parseBedrockEventStreamHeaders(rest[:headersLen])}, nil
	}
	payloadStart := int(headersLen)
	payloadEnd := payloadStart + payloadLen
	if payloadEnd > len(rest)-4 {
		return BedrockEventStreamMessage{}, fmt.Errorf("bedrock event stream payload overflow")
	}
	payload := make([]byte, payloadLen)
	copy(payload, rest[payloadStart:payloadEnd])
	return BedrockEventStreamMessage{
		Headers: parseBedrockEventStreamHeaders(rest[:headersLen]),
		Payload: payload,
	}, nil
}

func parseBedrockEventStreamHeaders(data []byte) map[string]string {
	headers := make(map[string]string)
	for offset := 0; offset < len(data); {
		nameLen := int(data[offset])
		offset++
		if nameLen == 0 || offset+nameLen+3 > len(data) {
			return headers
		}
		name := string(data[offset : offset+nameLen])
		offset += nameLen
		headerType := data[offset]
		offset++
		if headerType != 7 {
			return headers
		}
		valueLen := int(binary.BigEndian.Uint16(data[offset : offset+2]))
		offset += 2
		if offset+valueLen > len(data) {
			return headers
		}
		headers[name] = string(data[offset : offset+valueLen])
		offset += valueLen
	}
	return headers
}
