package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexContinueDefaultMethod         = "rewind"
	codexContinueReplayMethod          = "replay"
	codexContinueDefaultTruncationStep = 518
	codexContinueDefaultMaxRounds      = 8
	codexContinueDefaultMinTier        = 1
	codexContinueDefaultMarkerText     = "Continue thinking..."
	codexContinueEncryptedInclude      = "reasoning.encrypted_content"
)

type codexContinueConfig struct {
	Enabled              bool
	Method               string
	TruncationStep       int
	MaxRounds            int
	MinTier              int
	MaxTier              int
	MaxTotalOutputTokens int64
	MarkerText           string
}

type codexContinueUsage struct {
	InputTokens     int64
	OutputTokens    int64
	TotalTokens     int64
	CachedTokens    int64
	ReasoningTokens int64
	HasCachedTokens bool
	Seen            bool
}

type codexContinueBufferedItem struct {
	UpstreamOutputIndex string
	ItemType            string
	Events              [][]byte
	Item                []byte
}

type codexContinueEventResult struct {
	Emit         [][]byte
	PublishUsage [][]byte
	Continue     bool
	Handled      bool
	Done         bool
}

type codexContinueFold struct {
	cfg codexContinueConfig

	round           int
	continuedRounds int
	everContinued   bool
	done            bool

	nextSequenceNumber int64
	nextOutputIndex    int64

	itemKind       map[string]string
	outputIndexMap map[string]int64
	buffered       []*codexContinueBufferedItem
	roundReasoning []json.RawMessage

	finalOutput [][]byte
	replayTail  []json.RawMessage

	firstUsage      codexContinueUsage
	totalUsage      codexContinueUsage
	finalRoundUsage codexContinueUsage

	baseResponse []byte

	retainedDraft    []*codexContinueBufferedItem
	retainedTerminal []byte
	retainedUsage    codexContinueUsage

	awaitingContinuation bool
	continuationStrategy string
	currentRoundHasItem  bool

	lastFinalTerminal []byte
	stoppedReason     string
}

func newCodexContinueFold(body []byte, cfg *config.Config) *codexContinueFold {
	continueCfg := codexContinueConfigFromConfig(cfg)
	if !continueCfg.Enabled || !codexContinueReasoningEnabled(body) {
		return nil
	}
	return &codexContinueFold{
		cfg:            continueCfg,
		round:          1,
		itemKind:       make(map[string]string),
		outputIndexMap: make(map[string]int64),
	}
}

func codexContinueConfigFromConfig(cfg *config.Config) codexContinueConfig {
	out := codexContinueConfig{
		Method:         codexContinueDefaultMethod,
		TruncationStep: codexContinueDefaultTruncationStep,
		MaxRounds:      codexContinueDefaultMaxRounds,
		MinTier:        codexContinueDefaultMinTier,
		MarkerText:     codexContinueDefaultMarkerText,
	}
	if cfg == nil {
		return out
	}
	raw := cfg.Codex.ContinueThinking
	out.Enabled = raw.Enabled
	if method := strings.TrimSpace(raw.Method); method != "" {
		out.Method = method
	}
	if raw.TruncationStep > 0 {
		out.TruncationStep = raw.TruncationStep
	}
	if raw.MaxRounds > 0 {
		out.MaxRounds = raw.MaxRounds
	}
	if raw.MinTier > 0 {
		out.MinTier = raw.MinTier
	}
	out.MaxTier = raw.MaxTier
	out.MaxTotalOutputTokens = raw.MaxTotalOutputTokens
	if marker := strings.TrimSpace(raw.MarkerText); marker != "" {
		out.MarkerText = marker
	}
	return out
}

func codexContinueReasoningEnabled(body []byte) bool {
	reasoning := gjson.GetBytes(body, "reasoning")
	return !(reasoning.Exists() && reasoning.Type == gjson.False)
}

func codexContinueIsTruncationPattern(tokens *int64, step int) bool {
	if tokens == nil {
		return false
	}
	if step <= 0 {
		step = codexContinueDefaultTruncationStep
	}
	return *tokens >= int64(step-2) && (*tokens+2)%int64(step) == 0
}

func codexContinueTruncationTier(tokens *int64, step int) *int64 {
	if !codexContinueIsTruncationPattern(tokens, step) {
		return nil
	}
	tier := (*tokens + 2) / int64(step)
	return &tier
}

func codexContinueReasoningTokens(eventData []byte) *int64 {
	usageNode := gjson.GetBytes(eventData, "response.usage.output_tokens_details.reasoning_tokens")
	if !usageNode.Exists() {
		return nil
	}
	tokens := usageNode.Int()
	return &tokens
}

func codexContinueCommentaryMessage(marker string) []byte {
	if strings.TrimSpace(marker) == "" {
		marker = codexContinueDefaultMarkerText
	}
	out := []byte(`{"type":"message","role":"assistant","content":[{"type":"output_text"}],"phase":"commentary"}`)
	out, _ = sjson.SetBytes(out, "content.0.text", marker)
	return out
}

func (f *codexContinueFold) HandleEvent(eventData []byte) codexContinueEventResult {
	if f == nil || f.done {
		return codexContinueEventResult{Emit: [][]byte{eventData}}
	}

	eventType := strings.TrimSpace(gjson.GetBytes(eventData, "type").String())
	if f.awaitingContinuation && eventType == "response.output_item.added" {
		f.awaitingContinuation = false
		f.currentRoundHasItem = true
		f.retainedDraft = nil
		f.retainedTerminal = nil
	}

	if eventType == "response.created" {
		if response := gjson.GetBytes(eventData, "response"); response.Exists() && len(f.baseResponse) == 0 {
			f.baseResponse = []byte(response.Raw)
		}
		if f.round == 1 && !f.everContinued {
			f.trackRawEmission(eventData)
			return codexContinueEventResult{Emit: [][]byte{eventData}, Handled: true}
		}
		return codexContinueEventResult{Handled: true}
	}
	if eventType == "response.in_progress" {
		if f.round == 1 && !f.everContinued {
			f.trackRawEmission(eventData)
			return codexContinueEventResult{Emit: [][]byte{eventData}, Handled: true}
		}
		return codexContinueEventResult{Handled: true}
	}

	if codexContinueIsTerminalEvent(eventType) {
		return f.handleTerminal(eventData, eventType)
	}

	upstreamOutputIndex := codexContinueOutputIndexKey(eventData, len(f.buffered))
	switch eventType {
	case "response.output_item.added":
		item := gjson.GetBytes(eventData, "item")
		itemType := strings.TrimSpace(item.Get("type").String())
		if itemType == "reasoning" {
			f.itemKind[upstreamOutputIndex] = "reasoning"
			emit := f.rewriteOrTrackReasoningEvent(eventData, upstreamOutputIndex, true)
			return codexContinueEventResult{Emit: [][]byte{emit}, Handled: true}
		}
		f.itemKind[upstreamOutputIndex] = "buffered"
		f.buffered = append(f.buffered, &codexContinueBufferedItem{
			UpstreamOutputIndex: upstreamOutputIndex,
			ItemType:            itemType,
			Events:              [][]byte{bytes.Clone(eventData)},
			Item:                codexContinueRawJSON(item),
		})
		return codexContinueEventResult{Handled: true}
	case "response.output_item.done":
		kind := f.itemKind[upstreamOutputIndex]
		if kind == "reasoning" {
			if item := gjson.GetBytes(eventData, "item"); item.Exists() && item.Type == gjson.JSON {
				rawItem := []byte(item.Raw)
				f.roundReasoning = append(f.roundReasoning, json.RawMessage(bytes.Clone(rawItem)))
				f.finalOutput = append(f.finalOutput, bytes.Clone(rawItem))
			}
			emit := f.rewriteOrTrackReasoningEvent(eventData, upstreamOutputIndex, false)
			return codexContinueEventResult{Emit: [][]byte{emit}, Handled: true}
		}
		if kind == "buffered" {
			entry := f.findBuffered(upstreamOutputIndex)
			if entry != nil {
				entry.Events = append(entry.Events, bytes.Clone(eventData))
				if item := gjson.GetBytes(eventData, "item"); item.Exists() && item.Type == gjson.JSON {
					entry.Item = []byte(item.Raw)
				}
				return codexContinueEventResult{Handled: true}
			}
		}
	default:
		kind := f.itemKind[upstreamOutputIndex]
		if kind == "reasoning" {
			emit := f.rewriteOrTrackReasoningEvent(eventData, upstreamOutputIndex, false)
			return codexContinueEventResult{Emit: [][]byte{emit}, Handled: true}
		}
		if kind == "buffered" {
			entry := f.findBuffered(upstreamOutputIndex)
			if entry != nil {
				entry.Events = append(entry.Events, bytes.Clone(eventData))
				return codexContinueEventResult{Handled: true}
			}
		}
	}

	if f.everContinued || f.round > 1 {
		eventData = f.rewriteSequence(eventData)
	} else {
		f.trackRawEmission(eventData)
	}
	return codexContinueEventResult{Emit: [][]byte{eventData}, Handled: true}
}

func (f *codexContinueFold) handleTerminal(eventData []byte, eventType string) codexContinueEventResult {
	usage := codexContinueParseUsage(eventData)
	f.addUsage(usage)
	if !f.firstUsage.Seen {
		f.firstUsage = usage
	}
	f.finalRoundUsage = usage

	if !codexContinueIsSuccessTerminalEvent(eventType) {
		// A hidden continuation round failed before producing output: flush the
		// retained draft from the last truncated round instead of surfacing a
		// failure the client cannot attribute to anything it sent.
		if f.awaitingContinuation && !f.currentRoundHasItem && len(f.retainedTerminal) > 0 {
			emit := f.FinalizeAfterContinuationFailure()
			return codexContinueEventResult{Emit: emit, PublishUsage: [][]byte{eventData}, Handled: true, Done: true}
		}
		f.done = true
		if f.everContinued || f.round > 1 {
			eventData = f.rewriteSequence(eventData)
		} else {
			f.trackRawEmission(eventData)
		}
		// Failure terminals are reported via PublishFailure by the emit path;
		// do not also publish usage or stats would double-count.
		return codexContinueEventResult{Emit: [][]byte{eventData}, Handled: true, Done: true}
	}

	shouldContinue, stoppedReason := f.shouldContinue(eventData)
	if shouldContinue {
		f.retainedDraft = codexContinueCloneBufferedItems(f.buffered)
		f.retainedTerminal = bytes.Clone(eventData)
		f.retainedUsage = usage
		f.appendReplayTail()
		f.continuedRounds++
		f.everContinued = true
		f.resetRound()
		return codexContinueEventResult{PublishUsage: [][]byte{eventData}, Continue: true, Handled: true}
	}

	f.stoppedReason = stoppedReason
	rewrite := f.everContinued || f.round > 1 || stoppedReason != ""
	emit := f.flushBuffered(f.buffered, rewrite, true)
	terminal := eventData
	publishUsage := [][]byte(nil)
	if rewrite {
		terminal = f.reconstructTerminal(eventData, true)
		publishUsage = append(publishUsage, eventData)
	} else {
		f.trackRawEmission(eventData)
	}
	f.done = true
	emit = append(emit, terminal)
	return codexContinueEventResult{Emit: emit, PublishUsage: publishUsage, Handled: true, Done: true}
}

func (f *codexContinueFold) shouldContinue(eventData []byte) (bool, string) {
	if f == nil {
		return false, ""
	}
	tokens := codexContinueReasoningTokens(eventData)
	tier := codexContinueTruncationTier(tokens, f.cfg.TruncationStep)
	if tier == nil {
		return false, ""
	}
	if *tier < int64(f.cfg.MinTier) {
		return false, "tier_out_of_window"
	}
	if f.cfg.MaxTier > 0 && *tier > int64(f.cfg.MaxTier) {
		return false, "tier_out_of_window"
	}
	if f.continuedRounds >= f.cfg.MaxRounds {
		return false, "max_rounds"
	}
	if f.cfg.MaxTotalOutputTokens > 0 && f.totalUsage.OutputTokens >= f.cfg.MaxTotalOutputTokens {
		return false, "max_total_output_tokens"
	}
	if !codexContinueReasoningHasEncryptedContent(f.roundReasoning) {
		return false, "no_encrypted_content"
	}
	return true, ""
}

func (f *codexContinueFold) BuildContinuation(baseBody []byte, transcriptInput []byte, preferRewind bool, forceReplay bool) ([]byte, string, error) {
	if f == nil {
		return nil, "", fmt.Errorf("codex continue fold: nil fold")
	}
	body := bytes.Clone(baseBody)
	if len(bytes.TrimSpace(body)) == 0 {
		body = []byte(`{}`)
	}
	method := strings.TrimSpace(f.cfg.Method)
	useRewind := preferRewind && !forceReplay && method != codexContinueReplayMethod && gjson.GetBytes(body, "previous_response_id").Exists()
	var inputItems []json.RawMessage
	if useRewind {
		inputItems = xaiJSONRawMessages(gjson.GetBytes(body, "input"))
	} else if len(bytes.TrimSpace(transcriptInput)) > 0 {
		inputItems = xaiJSONRawMessages(gjson.ParseBytes(transcriptInput))
	} else {
		inputItems = xaiJSONRawMessages(gjson.GetBytes(body, "input"))
	}
	inputItems = append(inputItems, f.continuationReplayTail()...)
	body, _ = sjson.SetRawBytes(body, "input", xaiMarshalRawMessages(inputItems))
	body, _ = sjson.SetBytes(body, "stream", true)
	body = codexContinueEnsureEncryptedInclude(body)
	strategy := codexContinueDefaultMethod
	if !useRewind {
		body, _ = sjson.DeleteBytes(body, "previous_response_id")
		strategy = codexContinueReplayMethod
	}
	return body, strategy, nil
}

func (f *codexContinueFold) NoteContinuationSent(strategy string) {
	if f == nil {
		return
	}
	f.awaitingContinuation = true
	f.currentRoundHasItem = false
	f.continuationStrategy = strings.TrimSpace(strategy)
}

func (f *codexContinueFold) ShouldFallbackContinuation(err error) bool {
	if f == nil || !f.awaitingContinuation || f.currentRoundHasItem {
		return false
	}
	code := codexContinueStatusCode(err)
	return code >= 400 && code < 500
}

func (f *codexContinueFold) CanRetryContinuationAsReplay() bool {
	if f == nil {
		return false
	}
	return f.continuationStrategy == codexContinueDefaultMethod
}

func (f *codexContinueFold) FinalizeAfterContinuationFailure() [][]byte {
	if f == nil || len(f.retainedTerminal) == 0 {
		return nil
	}
	f.finalRoundUsage = f.retainedUsage
	emit := f.flushBuffered(f.retainedDraft, true, true)
	terminal := f.reconstructTerminal(f.retainedTerminal, true)
	f.done = true
	emit = append(emit, terminal)
	return emit
}

func (f *codexContinueFold) HandleEOF() [][]byte {
	if f == nil || f.done {
		return nil
	}
	terminal := []byte(`{"type":"response.incomplete","response":{"status":"incomplete","output":[]}}`)
	if len(f.baseResponse) > 0 {
		terminal, _ = sjson.SetRawBytes(terminal, "response", f.baseResponse)
		terminal, _ = sjson.SetBytes(terminal, "response.status", "incomplete")
	}
	terminal, _ = sjson.SetRawBytes(terminal, "response.output", codexContinueMarshalRaw(f.finalOutput))
	terminal, _ = sjson.SetRawBytes(terminal, "response.usage", codexContinueAgentUsage(f.firstUsage, f.totalUsage, f.finalRoundUsage, false))
	terminal, _ = sjson.SetBytes(terminal, "response.incomplete_details.reason", "upstream_eof")
	terminal = f.rewriteSequence(terminal)
	f.lastFinalTerminal = bytes.Clone(terminal)
	f.done = true
	return [][]byte{terminal}
}

func (f *codexContinueFold) IsFoldedTerminal(eventData []byte) bool {
	if f == nil || len(f.lastFinalTerminal) == 0 {
		return false
	}
	return bytes.Equal(bytes.TrimSpace(eventData), bytes.TrimSpace(f.lastFinalTerminal))
}

func (f *codexContinueFold) HasContinuationInFlight() bool {
	return f != nil && f.everContinued && !f.done
}

func (f *codexContinueFold) appendReplayTail() {
	for _, item := range f.roundReasoning {
		clean := codexContinueStripReplayID([]byte(item))
		f.replayTail = append(f.replayTail, json.RawMessage(clean))
	}
	f.replayTail = append(f.replayTail, json.RawMessage(codexContinueCommentaryMessage(f.cfg.MarkerText)))
}

func (f *codexContinueFold) continuationReplayTail() []json.RawMessage {
	if len(f.replayTail) == 0 {
		return nil
	}
	out := make([]json.RawMessage, 0, len(f.replayTail))
	for _, item := range f.replayTail {
		clean := []byte(item)
		if strings.TrimSpace(gjson.GetBytes(clean, "type").String()) == "reasoning" {
			clean = codexContinueStripReplayID(clean)
		}
		out = append(out, json.RawMessage(bytes.Clone(clean)))
	}
	return out
}

func (f *codexContinueFold) resetRound() {
	f.round++
	f.itemKind = make(map[string]string)
	f.outputIndexMap = make(map[string]int64)
	f.buffered = nil
	f.roundReasoning = nil
	f.currentRoundHasItem = false
}

func (f *codexContinueFold) findBuffered(outputIndex string) *codexContinueBufferedItem {
	for _, entry := range f.buffered {
		if entry != nil && entry.UpstreamOutputIndex == outputIndex {
			return entry
		}
	}
	return nil
}

func (f *codexContinueFold) rewriteOrTrackReasoningEvent(eventData []byte, upstreamOutputIndex string, added bool) []byte {
	if f.everContinued || f.round > 1 {
		downstreamOutputIndex, ok := f.outputIndexMap[upstreamOutputIndex]
		if added || !ok {
			downstreamOutputIndex = f.nextOutputIndex
			f.nextOutputIndex++
			f.outputIndexMap[upstreamOutputIndex] = downstreamOutputIndex
		}
		eventData, _ = sjson.SetBytes(eventData, "output_index", downstreamOutputIndex)
		return f.rewriteSequence(eventData)
	}
	if added {
		f.trackOutputIndex(eventData)
	}
	f.trackRawEmission(eventData)
	return eventData
}

func (f *codexContinueFold) rewriteSequence(eventData []byte) []byte {
	seq := f.nextSequenceNumber
	f.nextSequenceNumber++
	eventData, _ = sjson.SetBytes(eventData, "sequence_number", seq)
	return eventData
}

func (f *codexContinueFold) trackRawEmission(eventData []byte) {
	if seq := gjson.GetBytes(eventData, "sequence_number"); seq.Exists() && seq.Int() >= f.nextSequenceNumber {
		f.nextSequenceNumber = seq.Int() + 1
	}
}

func (f *codexContinueFold) trackOutputIndex(eventData []byte) {
	if outputIndex := gjson.GetBytes(eventData, "output_index"); outputIndex.Exists() && outputIndex.Int() >= f.nextOutputIndex {
		f.nextOutputIndex = outputIndex.Int() + 1
	}
}

func (f *codexContinueFold) flushBuffered(entries []*codexContinueBufferedItem, rewrite bool, recordFinal bool) [][]byte {
	emit := make([][]byte, 0)
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		downstreamOutputIndex := f.nextOutputIndex
		if rewrite {
			f.nextOutputIndex++
		}
		for _, eventData := range entry.Events {
			eventData = bytes.Clone(eventData)
			if rewrite {
				if gjson.GetBytes(eventData, "output_index").Exists() {
					eventData, _ = sjson.SetBytes(eventData, "output_index", downstreamOutputIndex)
				}
				eventData = f.rewriteSequence(eventData)
			} else {
				f.trackRawEmission(eventData)
			}
			emit = append(emit, eventData)
		}
		if recordFinal && len(entry.Item) > 0 {
			f.finalOutput = append(f.finalOutput, bytes.Clone(entry.Item))
		}
	}
	return emit
}

func (f *codexContinueFold) reconstructTerminal(eventData []byte, flushedFinal bool) []byte {
	terminal := bytes.Clone(eventData)
	terminal, _ = sjson.SetRawBytes(terminal, "response.output", codexContinueMarshalRaw(f.finalOutput))
	terminal, _ = sjson.SetRawBytes(terminal, "response.usage", codexContinueAgentUsage(f.firstUsage, f.totalUsage, f.finalRoundUsage, flushedFinal))
	if f.stoppedReason != "" {
		terminal, _ = sjson.SetBytes(terminal, "response.metadata.continue_thinking_stopped_reason", f.stoppedReason)
	}
	terminal = f.rewriteSequence(terminal)
	f.lastFinalTerminal = bytes.Clone(terminal)
	return terminal
}

func (f *codexContinueFold) addUsage(usage codexContinueUsage) {
	if !usage.Seen {
		return
	}
	f.totalUsage.Seen = true
	f.totalUsage.InputTokens += usage.InputTokens
	f.totalUsage.OutputTokens += usage.OutputTokens
	f.totalUsage.TotalTokens += usage.TotalTokens
	f.totalUsage.ReasoningTokens += usage.ReasoningTokens
	if usage.HasCachedTokens {
		f.totalUsage.HasCachedTokens = true
		f.totalUsage.CachedTokens += usage.CachedTokens
	}
}

func codexContinueParseUsage(eventData []byte) codexContinueUsage {
	usageNode := gjson.GetBytes(eventData, "response.usage")
	if !usageNode.Exists() || !usageNode.IsObject() {
		return codexContinueUsage{}
	}
	out := codexContinueUsage{
		InputTokens:     usageNode.Get("input_tokens").Int(),
		OutputTokens:    usageNode.Get("output_tokens").Int(),
		TotalTokens:     usageNode.Get("total_tokens").Int(),
		ReasoningTokens: usageNode.Get("output_tokens_details.reasoning_tokens").Int(),
		Seen:            true,
	}
	if cached := usageNode.Get("input_tokens_details.cached_tokens"); cached.Exists() {
		out.HasCachedTokens = true
		out.CachedTokens = cached.Int()
	}
	return out
}

func codexContinueAgentUsage(first codexContinueUsage, total codexContinueUsage, finalRound codexContinueUsage, flushedFinal bool) []byte {
	finalNonReasoning := int64(0)
	if flushedFinal && finalRound.Seen {
		finalNonReasoning = finalRound.OutputTokens - finalRound.ReasoningTokens
		if finalNonReasoning < 0 {
			finalNonReasoning = 0
		}
	}
	outputTokens := total.ReasoningTokens + finalNonReasoning
	out := []byte(`{"output_tokens_details":{}}`)
	out, _ = sjson.SetBytes(out, "input_tokens", first.InputTokens)
	out, _ = sjson.SetBytes(out, "output_tokens", outputTokens)
	out, _ = sjson.SetBytes(out, "total_tokens", first.InputTokens+outputTokens)
	out, _ = sjson.SetBytes(out, "output_tokens_details.reasoning_tokens", total.ReasoningTokens)
	if first.HasCachedTokens {
		out, _ = sjson.SetBytes(out, "input_tokens_details.cached_tokens", first.CachedTokens)
	}
	return out
}

func codexContinueIsTerminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled":
		return true
	default:
		return false
	}
}

func codexContinueIsSuccessTerminalEvent(eventType string) bool {
	return eventType == "response.completed" || eventType == "response.done"
}

func codexContinueOutputIndexKey(eventData []byte, fallback int) string {
	if outputIndex := gjson.GetBytes(eventData, "output_index"); outputIndex.Exists() {
		return outputIndex.Raw
	}
	return fmt.Sprintf("missing-%d", fallback)
}

func codexContinueRawJSON(result gjson.Result) []byte {
	if result.Exists() && result.Type == gjson.JSON {
		return []byte(result.Raw)
	}
	return nil
}

func codexContinueReasoningHasEncryptedContent(items []json.RawMessage) bool {
	if len(items) == 0 {
		return false
	}
	last := items[len(items)-1]
	encrypted := gjson.GetBytes(last, "encrypted_content")
	return encrypted.Exists() && strings.TrimSpace(encrypted.String()) != ""
}

func codexContinueStripReplayID(item []byte) []byte {
	clean := bytes.Clone(item)
	clean, _ = sjson.DeleteBytes(clean, "id")
	return clean
}

func codexContinueEnsureEncryptedInclude(body []byte) []byte {
	include := gjson.GetBytes(body, "include")
	items := make([]string, 0)
	found := false
	if include.Exists() && include.IsArray() {
		for _, item := range include.Array() {
			if strings.TrimSpace(item.String()) == codexContinueEncryptedInclude {
				found = true
			}
			items = append(items, item.Raw)
		}
	}
	if !found {
		encoded, _ := json.Marshal(codexContinueEncryptedInclude)
		items = append(items, string(encoded))
	}
	return codexContinueSetRawArray(body, "include", items)
}

func codexContinueSetRawArray(body []byte, path string, rawItems []string) []byte {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, item := range rawItems {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.WriteString(item)
	}
	buf.WriteByte(']')
	body, _ = sjson.SetRawBytes(body, path, buf.Bytes())
	return body
}

func codexContinueMarshalRaw(items [][]byte) []byte {
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(bytes.TrimSpace(item))
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

func codexContinueCloneBufferedItems(items []*codexContinueBufferedItem) []*codexContinueBufferedItem {
	if len(items) == 0 {
		return nil
	}
	out := make([]*codexContinueBufferedItem, 0, len(items))
	for _, item := range items {
		if item == nil {
			continue
		}
		clone := &codexContinueBufferedItem{
			UpstreamOutputIndex: item.UpstreamOutputIndex,
			ItemType:            item.ItemType,
			Item:                bytes.Clone(item.Item),
		}
		for _, eventData := range item.Events {
			clone.Events = append(clone.Events, bytes.Clone(eventData))
		}
		out = append(out, clone)
	}
	return out
}

func codexContinueStatusCode(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface{ StatusCode() int }
	if sc, ok := err.(statusCoder); ok {
		return sc.StatusCode()
	}
	return 0
}

func codexContinueBuildHTTPRequest(ctx context.Context, baseReq *http.Request, body []byte) (*http.Request, error) {
	if baseReq == nil || baseReq.URL == nil {
		return nil, fmt.Errorf("codex continue fold: base request is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req := baseReq.Clone(ctx)
	req.Body = io.NopCloser(bytes.NewReader(body))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	req.ContentLength = int64(len(body))
	req.Header = baseReq.Header.Clone()
	return req, nil
}

func (e *CodexExecutor) executeCodexContinueFoldHTTPStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, responseFormat sdktranslator.Format, to sdktranslator.Format, originalPayload []byte, clientBody []byte, initialRequestBody []byte, httpReq *http.Request, httpResp *http.Response, httpClient *http.Client, reporter *helps.UsageReporter, replayScope codexReasoningReplayScope, identityState codexIdentityConfuseState, authID string, authLabel string, authType string, authValue string) (*cliproxyexecutor.StreamResult, bool, error) {
	fold := newCodexContinueFold(clientBody, e.cfg)
	if fold == nil {
		return nil, false, nil
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	headers := httpResp.Header.Clone()
	go func() {
		defer close(out)
		currentResp := httpResp
		baseBody := bytes.Clone(initialRequestBody)
		defer func() {
			if currentResp != nil && currentResp.Body != nil {
				if errClose := currentResp.Body.Close(); errClose != nil {
					log.Errorf("codex executor: close response body error: %v", errClose)
				}
			}
		}()

		var param any
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		for currentResp != nil {
			scanner := bufio.NewScanner(currentResp.Body)
			scanner.Buffer(nil, 52_428_800)
			continueOuter := false
			for scanner.Scan() {
				line := applyCodexIdentityConfuseResponsePayload(scanner.Bytes(), identityState)
				helps.AppendAPIResponseChunk(ctx, e.cfg, line)
				if !bytes.HasPrefix(line, dataTag) {
					if !codexContinueSendTranslated(ctx, out, to, responseFormat, req.Model, originalPayload, clientBody, applyCodexIdentityExposeResponsePayload(line, identityState), &param) {
						return
					}
					continue
				}

				data := bytes.TrimSpace(line[5:])
				if streamErr, terminalBody, ok := codexTerminalStreamErr(data); ok {
					if errClearReplay := clearCodexReasoningReplayOnInvalidSignature(ctx, replayScope, streamErr.StatusCode(), terminalBody); errClearReplay != nil {
						helps.RecordAPIResponseError(ctx, e.cfg, errClearReplay)
						reporter.PublishFailure(ctx, errClearReplay)
						_ = codexContinueSendError(ctx, out, errClearReplay)
						return
					}
					helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
					reporter.PublishFailure(ctx, streamErr)
					_ = codexContinueSendError(ctx, out, streamErr)
					return
				}

				result := fold.HandleEvent(data)
				for _, usageEvent := range result.PublishUsage {
					if detail, ok := helps.ParseCodexUsage(usageEvent); ok {
						reporter.Publish(ctx, detail)
					}
					publishCodexImageToolUsage(ctx, reporter, clientBody, usageEvent)
				}
				if result.Continue {
					// Prefer rewind when the HTTP body carries previous_response_id
					// (stateful client): its input is incremental, so dropping P for a
					// replay would lose context. Stateless bodies fall back to replay.
					nextBody, strategy, errBuild := fold.BuildContinuation(baseBody, nil, true, false)
					if errBuild != nil {
						helps.RecordAPIResponseError(ctx, e.cfg, errBuild)
						reporter.PublishFailure(ctx, errBuild)
						_ = codexContinueSendError(ctx, out, errBuild)
						return
					}
					nextResp, errRound := e.openCodexContinueHTTPRound(ctx, auth, httpReq, httpClient, nextBody, authID, authLabel, authType, authValue)
					if errRound != nil && strategy == codexContinueDefaultMethod && codexContinueStatusCode(errRound) >= 400 && codexContinueStatusCode(errRound) < 500 {
						// A′ rewind rejected: retry the same round as a full replay.
						if replayBody, replayStrategy, errReplay := fold.BuildContinuation(baseBody, nil, false, true); errReplay == nil {
							strategy = replayStrategy
							nextResp, errRound = e.openCodexContinueHTTPRound(ctx, auth, httpReq, httpClient, replayBody, authID, authLabel, authType, authValue)
						}
					}
					if errRound != nil {
						if codexContinueStatusCode(errRound) >= 400 && codexContinueStatusCode(errRound) < 500 {
							if !codexContinueProcessHTTPEvents(ctx, out, fold, fold.FinalizeAfterContinuationFailure(), to, responseFormat, req.Model, originalPayload, clientBody, &param, reporter, replayScope, identityState, outputItemsByIndex, &outputItemsFallback) {
								return
							}
							return
						}
						helps.RecordAPIResponseError(ctx, e.cfg, errRound)
						reporter.PublishFailure(ctx, errRound)
						_ = codexContinueSendError(ctx, out, errRound)
						return
					}
					fold.NoteContinuationSent(strategy)
					if currentResp.Body != nil {
						if errClose := currentResp.Body.Close(); errClose != nil {
							log.Errorf("codex executor: close response body error: %v", errClose)
						}
					}
					currentResp = nextResp
					continueOuter = true
					break
				}

				if !codexContinueProcessHTTPEvents(ctx, out, fold, result.Emit, to, responseFormat, req.Model, originalPayload, clientBody, &param, reporter, replayScope, identityState, outputItemsByIndex, &outputItemsFallback) {
					return
				}
				if result.Done {
					return
				}
			}
			if continueOuter {
				continue
			}
			if errScan := scanner.Err(); errScan != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, errScan)
				reporter.PublishFailure(ctx, errScan)
				_ = codexContinueSendError(ctx, out, errScan)
				return
			}
			if eofEvents := fold.HandleEOF(); len(eofEvents) > 0 {
				_ = codexContinueProcessHTTPEvents(ctx, out, fold, eofEvents, to, responseFormat, req.Model, originalPayload, clientBody, &param, reporter, replayScope, identityState, outputItemsByIndex, &outputItemsFallback)
			}
			return
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}, true, nil
}

func (e *CodexExecutor) openCodexContinueHTTPRound(ctx context.Context, auth *cliproxyauth.Auth, baseReq *http.Request, httpClient *http.Client, body []byte, authID string, authLabel string, authType string, authValue string) (*http.Response, error) {
	req, err := codexContinueBuildHTTPRequest(ctx, baseReq, body)
	if err != nil {
		return nil, err
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       req.URL.String(),
		Method:    req.Method,
		Headers:   req.Header.Clone(),
		Body:      body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	helps.RecordAPIHTTPResponseMetadata(ctx, e.cfg, resp)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, readErr := io.ReadAll(resp.Body)
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("codex executor: close response body error: %v", errClose)
		}
		if readErr != nil {
			return nil, readErr
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, data)
		return nil, newCodexStatusErr(resp.StatusCode, data)
	}
	return resp, nil
}

func codexContinueProcessHTTPEvents(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk, fold *codexContinueFold, events [][]byte, to sdktranslator.Format, responseFormat sdktranslator.Format, model string, originalPayload []byte, clientBody []byte, param *any, reporter *helps.UsageReporter, replayScope codexReasoningReplayScope, identityState codexIdentityConfuseState, outputItemsByIndex map[int64][]byte, outputItemsFallback *[][]byte) bool {
	for _, data := range events {
		switch gjson.GetBytes(data, "type").String() {
		case "response.output_item.done":
			collectCodexOutputItemDone(data, outputItemsByIndex, outputItemsFallback)
		case "response.completed", "response.done":
			if !fold.IsFoldedTerminal(data) {
				if detail, ok := helps.ParseCodexUsage(data); ok {
					reporter.Publish(ctx, detail)
				}
				publishCodexImageToolUsage(ctx, reporter, clientBody, data)
			}
			data = patchCodexCompletedOutput(data, outputItemsByIndex, *outputItemsFallback)
			cacheCodexReasoningReplayFromCompleted(replayScope, data)
		}
		line := append([]byte("data: "), data...)
		line = applyCodexIdentityExposeResponsePayload(line, identityState)
		if !codexContinueSendTranslated(ctx, out, to, responseFormat, model, originalPayload, clientBody, line, param) {
			return false
		}
	}
	return true
}

func codexContinueSendTranslated(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk, to sdktranslator.Format, responseFormat sdktranslator.Format, model string, originalPayload []byte, clientBody []byte, line []byte, param *any) bool {
	chunks := sdktranslator.TranslateStream(ctx, to, responseFormat, model, originalPayload, clientBody, line, param)
	for i := range chunks {
		select {
		case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

func codexContinueSendError(ctx context.Context, out chan<- cliproxyexecutor.StreamChunk, err error) bool {
	select {
	case out <- cliproxyexecutor.StreamChunk{Err: err}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (e *CodexWebsocketsExecutor) executeCodexContinueFoldWebsocketStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, responseFormat sdktranslator.Format, to sdktranslator.Format, originalPayload []byte, clientBody []byte, upstreamBody []byte, identityState codexIdentityConfuseState, reporter *helps.UsageReporter, executionSessionID string, sess *codexWebsocketSession, readCh chan codexWebsocketRead, conn *websocket.Conn, wsReqBody []byte, wsURL string, wsHeaders http.Header, upstreamHeaders http.Header, authID string, baseURL string, baseModel string) (*cliproxyexecutor.StreamResult, bool) {
	fold := newCodexContinueFold(clientBody, e.cfg)
	if fold == nil {
		return nil, false
	}
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		terminateReason := "completed"
		var terminateErr error
		defer close(out)
		defer func() {
			if terminateReason == "context_done" && sess != nil && fold.HasContinuationInFlight() {
				e.invalidateUpstreamConn(sess, conn, "fold_abandoned", terminateErr)
			}
			if sess != nil {
				sess.clearActive(readCh)
				sess.reqMu.Unlock()
				return
			}
			logCodexWebsocketDisconnected(executionSessionID, authID, wsURL, terminateReason, terminateErr)
			if errClose := conn.Close(); errClose != nil {
				log.Errorf("codex websockets executor: close websocket error: %v", errClose)
			}
		}()

		send := func(chunk cliproxyexecutor.StreamChunk) bool {
			select {
			case out <- chunk:
				return true
			case <-ctx.Done():
				terminateReason = "context_done"
				terminateErr = ctx.Err()
				return false
			}
		}

		var param any
		transcriptState := getXAIWebsocketIDState(e.idStore, executionSessionID)
		recordedTranscript := false
		outputItemsByIndex := make(map[int64][]byte)
		var outputItemsFallback [][]byte
		currentReqBody := bytes.Clone(wsReqBody)

		for {
			if ctx != nil && ctx.Err() != nil {
				terminateReason = "context_done"
				terminateErr = ctx.Err()
				_ = send(cliproxyexecutor.StreamChunk{Err: ctx.Err()})
				return
			}
			msgType, payload, errRead := readCodexWebsocketMessage(ctx, sess, conn, readCh)
			if errRead != nil {
				if sess != nil && ctx != nil && ctx.Err() != nil {
					terminateReason = "context_done"
					terminateErr = ctx.Err()
					_ = send(cliproxyexecutor.StreamChunk{Err: ctx.Err()})
					return
				}
				terminateReason = "read_error"
				terminateErr = errRead
				helps.RecordAPIWebsocketError(ctx, e.cfg, "read", errRead)
				reporter.PublishFailure(ctx, errRead)
				_ = send(cliproxyexecutor.StreamChunk{Err: errRead})
				return
			}
			if msgType != websocket.TextMessage {
				if msgType == websocket.BinaryMessage {
					errBinary := fmt.Errorf("codex websockets executor: unexpected binary message")
					terminateReason = "unexpected_binary"
					terminateErr = errBinary
					helps.RecordAPIWebsocketError(ctx, e.cfg, "unexpected_binary", errBinary)
					reporter.PublishFailure(ctx, errBinary)
					if sess != nil {
						e.invalidateUpstreamConn(sess, conn, "unexpected_binary", errBinary)
					}
					_ = send(cliproxyexecutor.StreamChunk{Err: errBinary})
					return
				}
				continue
			}

			payload = bytes.TrimSpace(payload)
			if len(payload) == 0 {
				continue
			}
			reporter.MarkFirstResponseByte()
			payload = applyCodexIdentityConfuseResponsePayload(payload, identityState)
			helps.AppendAPIWebsocketResponse(ctx, e.cfg, payload)

			if wsErr, ok := parseCodexWebsocketError(payload); ok {
				if fold.ShouldFallbackContinuation(wsErr) {
					if fold.CanRetryContinuationAsReplay() {
						if okSend, errSend := e.sendCodexContinueWebsocketRound(ctx, auth, fold, currentReqBody, transcriptState, true, sess, conn, wsURL, wsHeaders, authID); !okSend {
							if errSend != nil {
								terminateReason = "continue_send_error"
								terminateErr = errSend
								reporter.PublishFailure(ctx, errSend)
								_ = send(cliproxyexecutor.StreamChunk{Err: errSend})
								return
							}
							terminateReason = "context_done"
							terminateErr = ctx.Err()
							return
						}
						continue
					}
					if !e.processCodexContinueWebsocketEvents(ctx, send, fold, fold.FinalizeAfterContinuationFailure(), to, responseFormat, req.Model, originalPayload, clientBody, &param, reporter, transcriptState, &recordedTranscript, currentReqBody, auth, baseURL, baseModel, identityState, outputItemsByIndex, &outputItemsFallback) {
						return
					}
					return
				}
				terminateReason = "upstream_error"
				terminateErr = wsErr
				helps.RecordAPIWebsocketError(ctx, e.cfg, "upstream_error", wsErr)
				reporter.PublishFailure(ctx, wsErr)
				if sess != nil {
					e.invalidateUpstreamConn(sess, conn, "upstream_error", wsErr)
				}
				_ = send(cliproxyexecutor.StreamChunk{Err: wsErr})
				return
			}

			result := fold.HandleEvent(payload)
			for _, usageEvent := range result.PublishUsage {
				if detail, ok := helps.ParseCodexUsage(usageEvent); ok {
					reporter.Publish(ctx, detail)
				}
			}
			if result.Continue {
				if okSend, errSend := e.sendCodexContinueWebsocketRound(ctx, auth, fold, currentReqBody, transcriptState, false, sess, conn, wsURL, wsHeaders, authID); !okSend {
					if errSend != nil {
						terminateReason = "continue_send_error"
						terminateErr = errSend
						reporter.PublishFailure(ctx, errSend)
						_ = send(cliproxyexecutor.StreamChunk{Err: errSend})
						return
					}
					terminateReason = "context_done"
					terminateErr = ctx.Err()
					return
				}
				continue
			}
			if !e.processCodexContinueWebsocketEvents(ctx, send, fold, result.Emit, to, responseFormat, req.Model, originalPayload, clientBody, &param, reporter, transcriptState, &recordedTranscript, currentReqBody, auth, baseURL, baseModel, identityState, outputItemsByIndex, &outputItemsFallback) {
				return
			}
			if result.Done {
				return
			}
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: upstreamHeaders, Chunks: out}, true
}

func (e *CodexWebsocketsExecutor) sendCodexContinueWebsocketRound(ctx context.Context, auth *cliproxyauth.Auth, fold *codexContinueFold, baseReqBody []byte, transcriptState *xaiWebsocketIDState, forceReplay bool, sess *codexWebsocketSession, conn *websocket.Conn, wsURL string, wsHeaders http.Header, authID string) (bool, error) {
	var transcriptInput []byte
	if transcriptState != nil {
		transcriptInput = transcriptState.snapshotTranscriptInput()
		transcriptInput = codexWebsocketCompactionReplayInput(transcriptInput, baseReqBody)
	}
	nextBody, strategy, errBuild := fold.BuildContinuation(baseReqBody, transcriptInput, true, forceReplay)
	if errBuild != nil {
		helps.RecordAPIWebsocketError(ctx, e.cfg, "continue_build", errBuild)
		return false, errBuild
	}
	nextReqBody := buildCodexWebsocketRequestBody(nextBody, wsURL)
	helps.RecordAPIWebsocketRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:      wsURL,
		Method:   "WEBSOCKET",
		Headers:  wsHeaders.Clone(),
		Body:     nextReqBody,
		Provider: e.Identifier(),
		AuthID:   authID,
	})
	if errSend := writeCodexWebsocketMessage(sess, conn, nextReqBody); errSend != nil {
		helps.RecordAPIWebsocketError(ctx, e.cfg, "continue_send", errSend)
		if sess != nil {
			e.invalidateUpstreamConn(sess, conn, "continue_send_error", errSend)
		}
		return false, errSend
	}
	fold.NoteContinuationSent(strategy)
	return true, nil
}

func (e *CodexWebsocketsExecutor) processCodexContinueWebsocketEvents(ctx context.Context, send func(cliproxyexecutor.StreamChunk) bool, fold *codexContinueFold, events [][]byte, to sdktranslator.Format, responseFormat sdktranslator.Format, model string, originalPayload []byte, clientBody []byte, param *any, reporter *helps.UsageReporter, transcriptState *xaiWebsocketIDState, recordedTranscript *bool, wsReqBody []byte, auth *cliproxyauth.Auth, baseURL string, baseModel string, identityState codexIdentityConfuseState, outputItemsByIndex map[int64][]byte, outputItemsFallback *[][]byte) bool {
	for _, payload := range events {
		eventType := gjson.GetBytes(payload, "type").String()
		switch eventType {
		case "response.output_item.done":
			collectCodexOutputItemDone(payload, outputItemsByIndex, outputItemsFallback)
		case "response.completed", "response.done":
			payload = patchCodexCompletedOutput(payload, outputItemsByIndex, *outputItemsFallback)
		}
		// Capture folded-terminal identity before any further payload rewriting
		// (normalizeCodexWebsocketCompletion mutates response.done terminals).
		isFolded := fold.IsFoldedTerminal(payload)
		if !*recordedTranscript && (eventType == "response.completed" || eventType == "response.done") && transcriptState != nil {
			transcriptState.recordTranscriptTurnWithProvenance(wsReqBody, payload, codexWebsocketTranscriptProvenance(auth, baseURL, baseModel))
			*recordedTranscript = true
		}
		if cliproxyexecutor.DownstreamWebsocket(ctx) {
			if eventType == "response.completed" || eventType == "response.done" {
				if !isFolded {
					if detail, ok := helps.ParseCodexUsage(payload); ok {
						reporter.Publish(ctx, detail)
					}
				}
			} else if isCodexWebsocketFailureTerminalEvent(eventType) {
				reporter.PublishFailure(ctx, codexWebsocketTerminalResponseErr(payload))
			}
			clientPayload := applyCodexIdentityExposeResponsePayload(payload, identityState)
			if !send(cliproxyexecutor.StreamChunk{Payload: clientPayload}) {
				return false
			}
			continue
		}

		payload = normalizeCodexWebsocketCompletion(payload)
		eventType = gjson.GetBytes(payload, "type").String()
		if eventType == "response.completed" || eventType == "response.done" {
			if !isFolded {
				if detail, ok := helps.ParseCodexUsage(payload); ok {
					reporter.Publish(ctx, detail)
				}
			}
		} else if isCodexWebsocketFailureTerminalEvent(eventType) {
			reporter.PublishFailure(ctx, codexWebsocketTerminalResponseErr(payload))
		}
		clientPayload := applyCodexIdentityExposeResponsePayload(payload, identityState)
		line := encodeCodexWebsocketAsSSE(clientPayload)
		chunks := sdktranslator.TranslateStream(ctx, to, responseFormat, model, clientBody, clientBody, line, param)
		for i := range chunks {
			if !send(cliproxyexecutor.StreamChunk{Payload: chunks[i]}) {
				return false
			}
		}
	}
	return true
}
