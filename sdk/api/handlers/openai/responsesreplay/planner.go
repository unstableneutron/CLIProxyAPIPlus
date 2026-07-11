package responsesreplay

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type Constraints uint8

const (
	RequireReplay Constraints = 1 << iota
	OmitEncryptedContent
	OmitProviderIdentifiers
)

func (c Constraints) Normalized() Constraints {
	if c&(OmitEncryptedContent|OmitProviderIdentifiers) != 0 {
		c |= RequireReplay
	}
	return c
}

type FailureKind uint8

const (
	FailureNone FailureKind = iota
	FailurePreviousResponseMissing
	FailureProviderItemMissing
	FailureInvalidEncryptedContent
	FailureAuthOrRoute
	FailureRequest
	FailureProtocol
	FailureCanceled
	FailureCompactionTakeover
)

func Advance(current Constraints, kind FailureKind, compact bool) (Constraints, bool) {
	current = current.Normalized()
	next := current
	switch kind {
	case FailurePreviousResponseMissing:
		if current&RequireReplay == 0 {
			next |= RequireReplay
		} else {
			next |= OmitProviderIdentifiers
		}
	case FailureProviderItemMissing:
		next |= RequireReplay | OmitProviderIdentifiers
	case FailureInvalidEncryptedContent:
		if !compact {
			next |= RequireReplay | OmitEncryptedContent
		}
	}
	next = next.Normalized()
	return next, next != current
}

func RenderWithConstraints(native, replay []byte, constraints Constraints) ([]byte, [32]byte, bool, error) {
	constraints = constraints.Normalized()
	base := native
	if constraints&RequireReplay != 0 {
		base = replay
	}
	if len(bytes.TrimSpace(base)) == 0 || !json.Valid(base) {
		return nil, [32]byte{}, false, fmt.Errorf("invalid responses replay JSON")
	}
	rendered := bytes.Clone(base)
	if constraints&OmitProviderIdentifiers != 0 {
		rendered, _ = stripProviderIdentifiers(rendered)
	}
	if constraints&OmitEncryptedContent != 0 {
		rendered, _ = stripNonPortableEncryptedContent(rendered)
	}
	if !json.Valid(rendered) {
		return nil, [32]byte{}, false, fmt.Errorf("rendered responses replay JSON is invalid")
	}
	return rendered, sha256.Sum256(rendered), !bytes.Equal(rendered, native), nil
}

func ClassifyFailure(status int, message string) FailureKind {
	message = strings.TrimSpace(message)
	if message != "" && gjson.Valid(message) {
		payload := []byte(message)
		if classifyStructuredInvalidEncryptedContent(payload) {
			return FailureInvalidEncryptedContent
		}
		if classifyStructuredPreviousResponse(payload) {
			return FailurePreviousResponseMissing
		}
		if classifyStructuredProviderItem(payload) {
			return FailureProviderItemMissing
		}
	}
	if invalidEncryptedContentMessage(message) {
		return FailureInvalidEncryptedContent
	}
	if previousResponseNotFoundMessage(message) {
		return FailurePreviousResponseMissing
	}
	if providerItemNotFoundMessage(message) {
		return FailureProviderItemMissing
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden ||
		status == http.StatusRequestTimeout || status == http.StatusConflict ||
		status == http.StatusTooManyRequests || status >= http.StatusInternalServerError {
		return FailureAuthOrRoute
	}
	if status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return FailureRequest
	}
	return FailureProtocol
}

type Attempt int

const (
	AttemptOriginal Attempt = iota
	AttemptWithoutReasoningEncryptedContent
	AttemptWithoutProviderIdentifiers
	AttemptPortableTranscript
)

func (a Attempt) String() string {
	switch a {
	case AttemptOriginal:
		return "original"
	case AttemptWithoutReasoningEncryptedContent:
		return "without_reasoning_encrypted_content"
	case AttemptWithoutProviderIdentifiers:
		return "without_provider_identifiers"
	case AttemptPortableTranscript:
		return "portable_transcript"
	default:
		return "unknown"
	}
}

type ErrorKind int

const (
	ErrorNone ErrorKind = iota
	ErrorInvalidEncryptedContent
	ErrorProviderStateNotFound
)

func (k ErrorKind) String() string {
	switch k {
	case ErrorNone:
		return "none"
	case ErrorInvalidEncryptedContent:
		return "invalid_encrypted_content"
	case ErrorProviderStateNotFound:
		return "provider_state_not_found"
	default:
		return "unknown"
	}
}

func NextAttempt(current Attempt, kind ErrorKind) (Attempt, bool) {
	switch current {
	case AttemptOriginal:
		switch kind {
		case ErrorInvalidEncryptedContent:
			return AttemptWithoutReasoningEncryptedContent, true
		case ErrorProviderStateNotFound:
			return AttemptWithoutProviderIdentifiers, true
		default:
			return AttemptOriginal, false
		}
	case AttemptWithoutReasoningEncryptedContent, AttemptWithoutProviderIdentifiers:
		switch kind {
		case ErrorInvalidEncryptedContent, ErrorProviderStateNotFound:
			return AttemptPortableTranscript, true
		default:
			return current, false
		}
	case AttemptPortableTranscript:
		return current, false
	default:
		return current, false
	}
}

func Render(payload []byte, attempt Attempt) ([]byte, bool) {
	if len(bytes.TrimSpace(payload)) == 0 || !json.Valid(payload) {
		return payload, false
	}
	switch attempt {
	case AttemptOriginal:
		return bytes.Clone(payload), false
	case AttemptWithoutReasoningEncryptedContent:
		return stripReasoningEncryptedContent(payload)
	case AttemptWithoutProviderIdentifiers:
		return stripProviderIdentifiers(payload)
	case AttemptPortableTranscript:
		updated, changedIDs := stripProviderIdentifiers(payload)
		updated, changedEncrypted := stripNonPortableEncryptedContent(updated)
		return updated, changedIDs || changedEncrypted
	default:
		return bytes.Clone(payload), false
	}
}

func Classify(status int, message string) ErrorKind {
	message = strings.TrimSpace(message)
	if message == "" {
		return ErrorNone
	}
	payload := []byte(message)
	if gjson.ValidBytes(payload) {
		if classifyStructuredInvalidEncryptedContent(payload) {
			return ErrorInvalidEncryptedContent
		}
		if classifyStructuredProviderState(payload) {
			return ErrorProviderStateNotFound
		}
	}

	if invalidEncryptedContentMessage(message) {
		return ErrorInvalidEncryptedContent
	}
	if providerStateNotFoundMessage(message) {
		return ErrorProviderStateNotFound
	}
	if status == http.StatusNotFound && strings.Contains(strings.ToLower(message), "previous_response") {
		return ErrorProviderStateNotFound
	}
	return ErrorNone
}

func stripProviderIdentifiers(payload []byte) ([]byte, bool) {
	updated := bytes.Clone(payload)
	changed := false
	if gjson.GetBytes(updated, "previous_response_id").Exists() {
		if next, err := sjson.DeleteBytes(updated, "previous_response_id"); err == nil {
			updated = next
			changed = true
		}
	}
	for _, arrayPath := range []string{"input", "output"} {
		items := gjson.GetBytes(updated, arrayPath)
		if !items.IsArray() {
			continue
		}
		for index := range items.Array() {
			path := fmt.Sprintf("%s.%d.id", arrayPath, index)
			if !gjson.GetBytes(updated, path).Exists() {
				continue
			}
			next, err := sjson.DeleteBytes(updated, path)
			if err != nil {
				continue
			}
			updated = next
			changed = true
		}
	}
	return updated, changed
}

func stripReasoningEncryptedContent(payload []byte) ([]byte, bool) {
	updated, changedInclude := stripReasoningEncryptedInclude(payload)
	updated, changedItems := stripInputFields(updated, func(item gjson.Result) []string {
		if strings.TrimSpace(item.Get("type").String()) != "reasoning" {
			return nil
		}
		return []string{"encrypted_content"}
	})
	return updated, changedInclude || changedItems
}

func stripNonPortableEncryptedContent(payload []byte) ([]byte, bool) {
	updated, changedInclude := stripReasoningEncryptedInclude(payload)
	updated, changedItems := stripInputFields(updated, func(item gjson.Result) []string {
		itemType := strings.TrimSpace(item.Get("type").String())
		if isCompactionLikeItemType(itemType) {
			return nil
		}
		if itemType == "reasoning" {
			return []string{"encrypted_content", "signature"}
		}
		return []string{"encrypted_content"}
	})
	return updated, changedInclude || changedItems
}

func stripInputFields(payload []byte, fieldsForItem func(gjson.Result) []string) ([]byte, bool) {
	input := gjson.GetBytes(payload, "input")
	if !input.IsArray() {
		return payload, false
	}
	updated := payload
	changed := false
	for index, item := range input.Array() {
		for _, field := range fieldsForItem(item) {
			path := fmt.Sprintf("input.%d.%s", index, field)
			if !gjson.GetBytes(updated, path).Exists() {
				continue
			}
			next, err := sjson.DeleteBytes(updated, path)
			if err != nil {
				continue
			}
			updated = next
			changed = true
		}
	}
	return updated, changed
}

func stripReasoningEncryptedInclude(payload []byte) ([]byte, bool) {
	include := gjson.GetBytes(payload, "include")
	if !include.IsArray() {
		return payload, false
	}
	kept := make([]string, 0, len(include.Array()))
	changed := false
	for _, item := range include.Array() {
		if strings.TrimSpace(item.String()) == "reasoning.encrypted_content" {
			changed = true
			continue
		}
		kept = append(kept, item.Raw)
	}
	if !changed {
		return payload, false
	}
	if len(kept) == 0 {
		updated, err := sjson.DeleteBytes(payload, "include")
		return updated, err == nil
	}
	updated, err := sjson.SetRawBytes(payload, "include", []byte("["+strings.Join(kept, ",")+"]"))
	return updated, err == nil
}

func isCompactionLikeItemType(itemType string) bool {
	switch strings.TrimSpace(itemType) {
	case "compaction", "compaction_summary":
		return true
	default:
		return false
	}
}

func classifyStructuredInvalidEncryptedContent(payload []byte) bool {
	for _, path := range []string{"error.code", "body.error.code", "code", "response.error.code"} {
		if strings.TrimSpace(gjson.GetBytes(payload, path).String()) == "invalid_encrypted_content" {
			return true
		}
	}
	for _, path := range []string{"error.message", "body.error.message", "message", "response.error.message"} {
		if invalidEncryptedContentMessage(gjson.GetBytes(payload, path).String()) {
			return true
		}
	}
	return false
}

func classifyStructuredProviderState(payload []byte) bool {
	for _, path := range []string{"error.param", "body.error.param", "param", "response.error.param"} {
		if providerStateParam(gjson.GetBytes(payload, path).String()) {
			return true
		}
	}
	for _, path := range []string{"error.code", "body.error.code", "code", "response.error.code"} {
		code := strings.TrimSpace(gjson.GetBytes(payload, path).String())
		if code == "previous_response_not_found" || code == "item_not_found" {
			return true
		}
	}
	for _, path := range []string{"error.message", "body.error.message", "message", "response.error.message"} {
		if providerStateNotFoundMessage(gjson.GetBytes(payload, path).String()) {
			return true
		}
	}
	return false
}

func classifyStructuredPreviousResponse(payload []byte) bool {
	for _, path := range []string{"error.code", "body.error.code", "code", "response.error.code"} {
		if strings.TrimSpace(gjson.GetBytes(payload, path).String()) == "previous_response_not_found" {
			return true
		}
	}
	for _, path := range []string{"error.param", "body.error.param", "param", "response.error.param"} {
		if strings.Contains(strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, path).String())), "previous_response_id") {
			return true
		}
	}
	for _, path := range []string{"error.message", "body.error.message", "message", "response.error.message"} {
		if previousResponseNotFoundMessage(gjson.GetBytes(payload, path).String()) {
			return true
		}
	}
	return false
}

func classifyStructuredProviderItem(payload []byte) bool {
	for _, path := range []string{"error.code", "body.error.code", "code", "response.error.code"} {
		if strings.TrimSpace(gjson.GetBytes(payload, path).String()) == "item_not_found" {
			return true
		}
	}
	for _, path := range []string{"error.param", "body.error.param", "param", "response.error.param"} {
		param := strings.ToLower(strings.TrimSpace(gjson.GetBytes(payload, path).String()))
		if (strings.Contains(param, "input") || strings.Contains(param, "output")) && strings.Contains(param, ".id") {
			return true
		}
	}
	for _, path := range []string{"error.message", "body.error.message", "message", "response.error.message"} {
		if providerItemNotFoundMessage(gjson.GetBytes(payload, path).String()) {
			return true
		}
	}
	return false
}

func providerStateParam(param string) bool {
	param = strings.ToLower(strings.TrimSpace(param))
	return strings.Contains(param, "previous_response_id") ||
		strings.Contains(param, "input") && strings.Contains(param, ".id") ||
		strings.Contains(param, "output") && strings.Contains(param, ".id")
}

func invalidEncryptedContentMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if !strings.Contains(lower, "encrypted content") {
		return false
	}
	return strings.Contains(lower, "could not be verified") || strings.Contains(lower, "recognized prefix")
}

func providerStateNotFoundMessage(message string) bool {
	return previousResponseNotFoundMessage(message) || providerItemNotFoundMessage(message)
}

func previousResponseNotFoundMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if strings.Contains(lower, "previous_response_not_found") {
		return true
	}
	if strings.Contains(lower, "previous response") && strings.Contains(lower, "not found") {
		return true
	}
	return false
}

func providerItemNotFoundMessage(message string) bool {
	lower := strings.ToLower(strings.TrimSpace(message))
	if !strings.Contains(lower, "item") || !strings.Contains(lower, "not found") {
		return false
	}
	return strings.Contains(lower, "item with id") ||
		strings.Contains(lower, "item id") ||
		strings.Contains(lower, "input item") ||
		strings.Contains(lower, "output item")
}
