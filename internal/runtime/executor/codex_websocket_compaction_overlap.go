package executor

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func codexWebsocketCompactionReplayInput(transcriptInput []byte, requestPayload []byte) []byte {
	pendingItems := codexWebsocketCompactionPendingInputItems(requestPayload)
	if len(pendingItems) == 0 {
		return transcriptInput
	}
	transcriptItems := xaiJSONRawMessages(gjson.ParseBytes(transcriptInput))
	if len(transcriptItems) == 0 {
		return transcriptInput
	}
	transcriptItems = appendCodexWebsocketCompactionPendingInputItems(transcriptItems, pendingItems)
	return xaiMarshalRawMessages(transcriptItems)
}

func codexWebsocketCompactionPendingInputItems(requestPayload []byte) []json.RawMessage {
	input := gjson.GetBytes(requestPayload, "input")
	if !input.Exists() || !input.IsArray() {
		return nil
	}
	items := xaiJSONRawMessages(input)
	pendingItems := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(gjson.GetBytes(item, "type").String()) == "compaction_trigger" {
			continue
		}
		pendingItems = append(pendingItems, bytes.Clone(item))
	}
	return pendingItems
}

func appendCodexWebsocketCompactionPendingInputItems(transcriptItems, pendingItems []json.RawMessage) []json.RawMessage {
	overlap := codexWebsocketCompactionInputOverlap(transcriptItems, pendingItems)
	merged := make([]json.RawMessage, 0, len(transcriptItems)+len(pendingItems)-overlap)
	for _, item := range transcriptItems {
		merged = append(merged, bytes.Clone(item))
	}
	for _, item := range pendingItems[overlap:] {
		merged = append(merged, bytes.Clone(item))
	}
	return merged
}

func codexWebsocketCompactionInputOverlap(transcriptItems, pendingItems []json.RawMessage) int {
	maxOverlap := min(len(transcriptItems), len(pendingItems))
	if maxOverlap == 0 {
		return 0
	}

	transcriptComparable := make([]string, len(transcriptItems))
	for index, item := range transcriptItems {
		transcriptComparable[index] = codexWebsocketCompactionComparableItem(item)
	}
	pendingComparable := make([]string, len(pendingItems))
	for index, item := range pendingItems {
		pendingComparable[index] = codexWebsocketCompactionComparableItem(item)
	}

	for overlap := maxOverlap; overlap > 0; overlap-- {
		transcriptStart := len(transcriptComparable) - overlap
		matched := true
		for index := 0; index < overlap; index++ {
			if transcriptComparable[transcriptStart+index] != pendingComparable[index] {
				matched = false
				break
			}
		}
		if matched {
			return overlap
		}
	}
	return 0
}

func codexWebsocketCompactionComparableItem(item json.RawMessage) string {
	comparable := bytes.Clone(item)
	for _, field := range []string{"id", "metadata"} {
		if !gjson.GetBytes(comparable, field).Exists() {
			continue
		}
		if updated, errDelete := sjson.DeleteBytes(comparable, field); errDelete == nil {
			comparable = updated
		}
	}
	return string(bytes.TrimSpace(comparable))
}
