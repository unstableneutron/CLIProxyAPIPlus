package openai

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers/openai/responsesreplay"
	"github.com/tidwall/gjson"
)

const responsesWebsocketStartupBufferLimit = 1 << 20

type responsesWebsocketTurnInput struct {
	ModelName           string
	NativePayload       []byte
	ReplayPayload       []byte
	NativeProviderBound bool
	Compaction          bool
	InitialPinnedAuthID string
}

type responsesWebsocketTurnOutcome struct {
	SelectedAuthID string
	Constraints    responsesreplay.Constraints
	Committed      bool
	Completed      bool
	Failure        responsesreplay.FailureKind
	Attempts       int
}

type responsesWebsocketTurnStream struct {
	Data    <-chan []byte
	Errors  <-chan *interfaces.ErrorMessage
	outcome <-chan responsesWebsocketTurnOutcome
}

type responsesWebsocketAttemptExecutor func(context.Context, []byte, string, []string, func(string)) (<-chan []byte, <-chan *interfaces.ErrorMessage)

type responsesWebsocketTurnRunner struct {
	execute responsesWebsocketAttemptExecutor
}

type responsesWebsocketStartupBuffer struct {
	payloads [][]byte
	bytes    int
}

func (b *responsesWebsocketStartupBuffer) append(payload []byte) bool {
	if b.bytes+len(payload) > responsesWebsocketStartupBufferLimit {
		return false
	}
	b.payloads = append(b.payloads, append([]byte(nil), payload...))
	b.bytes += len(payload)
	return true
}

func (b *responsesWebsocketStartupBuffer) reset() {
	b.payloads = nil
	b.bytes = 0
}

func (r responsesWebsocketTurnRunner) Start(ctx context.Context, input responsesWebsocketTurnInput) responsesWebsocketTurnStream {
	dataOut := make(chan []byte)
	errOut := make(chan *interfaces.ErrorMessage, 1)
	outcomeOut := make(chan responsesWebsocketTurnOutcome, 1)
	if ctx == nil {
		ctx = context.Background()
	}
	go r.run(ctx, input, dataOut, errOut, outcomeOut)
	return responsesWebsocketTurnStream{Data: dataOut, Errors: errOut, outcome: outcomeOut}
}

func (r responsesWebsocketTurnRunner) run(
	ctx context.Context,
	input responsesWebsocketTurnInput,
	dataOut chan<- []byte,
	errOut chan<- *interfaces.ErrorMessage,
	outcomeOut chan<- responsesWebsocketTurnOutcome,
) {
	defer close(dataOut)
	defer close(errOut)
	defer close(outcomeOut)

	outcome := responsesWebsocketTurnOutcome{}
	defer func() { outcomeOut <- outcome }()
	if r.execute == nil {
		outcome.Failure = responsesreplay.FailureProtocol
		errOut <- &interfaces.ErrorMessage{StatusCode: http.StatusInternalServerError, Error: fmt.Errorf("responses websocket turn runner has no executor")}
		return
	}

	constraints := responsesreplay.Constraints(0)
	hardExcluded := make(map[string]struct{})
	attempted := make(map[[32]byte]map[string]struct{})
	pinnedAuthID := strings.TrimSpace(input.InitialPinnedAuthID)
	providerStateRepair := false
	var lastRealError *interfaces.ErrorMessage
	const anonymousAuthID = "<anonymous>"

	for {
		if err := ctx.Err(); err != nil {
			outcome.Failure = responsesreplay.FailureCanceled
			return
		}
		payload, digest, _, errRender := responsesreplay.RenderWithConstraints(input.NativePayload, input.ReplayPayload, constraints)
		if errRender != nil {
			outcome.Failure = responsesreplay.FailureProtocol
			errOut <- &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: errRender}
			return
		}
		excluded := unionResponsesWebsocketAuthIDs(hardExcluded, attempted[digest])
		if _, excludedPin := hardExcluded[pinnedAuthID]; excludedPin {
			pinnedAuthID = ""
		}

		attemptCtx, cancelAttempt := context.WithCancel(ctx)
		var traceMu sync.Mutex
		selectedTrace := make([]string, 0, 2)
		selected := func(authID string) {
			authID = strings.TrimSpace(authID)
			if authID == "" {
				return
			}
			traceMu.Lock()
			selectedTrace = append(selectedTrace, authID)
			traceMu.Unlock()
		}
		data, errs := r.execute(attemptCtx, payload, pinnedAuthID, excluded, selected)
		outcome.Attempts++
		buffer := responsesWebsocketStartupBuffer{}
		committed := false
		completed := false
		dataOpen, errsOpen := data != nil, errs != nil

		snapshotTrace := func() []string {
			traceMu.Lock()
			defer traceMu.Unlock()
			return append([]string(nil), selectedTrace...)
		}
		activeAuth := func() string {
			trace := snapshotTrace()
			if len(trace) == 0 {
				return anonymousAuthID
			}
			return trace[len(trace)-1]
		}
		markAttempt := func() bool {
			authID := activeAuth()
			if attempted[digest] == nil {
				attempted[digest] = make(map[string]struct{})
			}
			if _, exists := attempted[digest][authID]; exists {
				return false
			}
			attempted[digest][authID] = struct{}{}
			outcome.SelectedAuthID = strings.TrimSpace(authID)
			if authID == anonymousAuthID {
				outcome.SelectedAuthID = ""
			}
			return true
		}
		marked := false
		ensureMarked := func() bool {
			if marked {
				return true
			}
			marked = markAttempt()
			return marked
		}
		emit := func(payload []byte) bool {
			select {
			case dataOut <- append([]byte(nil), payload...):
				return true
			case <-ctx.Done():
				return false
			}
		}
		flush := func() bool {
			for _, buffered := range buffer.payloads {
				if !emit(buffered) {
					return false
				}
			}
			buffer.reset()
			return true
		}
		retry := false
		terminalFailure := responsesreplay.FailureNone
		terminalErrorForwarded := false

		processFailure := func(kind responsesreplay.FailureKind) bool {
			if committed {
				terminalFailure = kind
				return false
			}
			next, changed := responsesreplay.Advance(constraints, kind, input.Compaction)
			if kind == responsesreplay.FailureAuthOrRoute {
				if providerStateRepair {
					terminalFailure = kind
					return false
				}
				trace := snapshotTrace()
				if len(trace) == 0 {
					terminalFailure = kind
					return false
				}
				for _, authID := range trace {
					hardExcluded[authID] = struct{}{}
				}
				pinnedAuthID = ""
				if input.NativeProviderBound {
					next |= responsesreplay.RequireReplay
					changed = next != constraints
				}
				retry = true
				constraints = next.Normalized()
				return true
			}
			if changed {
				if kind == responsesreplay.FailurePreviousResponseMissing || kind == responsesreplay.FailureProviderItemMissing {
					providerStateRepair = true
					trace := snapshotTrace()
					if len(trace) > 0 {
						pinnedAuthID = strings.TrimSpace(trace[len(trace)-1])
					}
				}
				constraints = next
				retry = true
				return true
			}
			terminalFailure = kind
			return false
		}

		for dataOpen || errsOpen {
			if !ensureMarked() {
				terminalFailure = responsesreplay.FailureProtocol
				break
			}
			var chunk []byte
			var gotData bool
			select {
			case chunk, gotData = <-data:
				if !gotData {
					dataOpen = false
					data = nil
				}
			default:
			}
			if !gotData && dataOpen {
				select {
				case <-ctx.Done():
					terminalFailure = responsesreplay.FailureCanceled
					dataOpen, errsOpen = false, false
					continue
				case chunk, gotData = <-data:
					if !gotData {
						dataOpen = false
						data = nil
					}
				case errMsg, ok := <-errs:
					if !ok {
						errsOpen = false
						errs = nil
						continue
					}
					if errMsg != nil {
						lastRealError = errMsg
						message := ""
						if errMsg.Error != nil {
							message = errMsg.Error.Error()
						}
						kind := responsesreplay.ClassifyFailureForRequest(responsesErrorStatus(errMsg), message, payload)
						if processFailure(kind) {
							dataOpen, errsOpen = false, false
							continue
						}
						if !flush() {
							terminalFailure = responsesreplay.FailureCanceled
							dataOpen, errsOpen = false, false
							continue
						}
						errOut <- errMsg
						terminalErrorForwarded = true
						dataOpen, errsOpen = false, false
					}
					continue
				}
			}
			if !gotData && !dataOpen && errsOpen {
				select {
				case <-ctx.Done():
					terminalFailure = responsesreplay.FailureCanceled
					errsOpen = false
				case errMsg, ok := <-errs:
					if !ok {
						errsOpen = false
						errs = nil
						continue
					}
					if errMsg != nil {
						lastRealError = errMsg
						message := ""
						if errMsg.Error != nil {
							message = errMsg.Error.Error()
						}
						kind := responsesreplay.ClassifyFailureForRequest(responsesErrorStatus(errMsg), message, payload)
						if processFailure(kind) {
							errsOpen = false
							continue
						}
						if flush() {
							errOut <- errMsg
							terminalErrorForwarded = true
						}
						errsOpen = false
					}
				}
			}
			if !gotData {
				continue
			}
			for _, event := range websocketJSONPayloadsFromChunk(chunk) {
				if !gjson.ValidBytes(event) {
					terminalFailure = responsesreplay.FailureProtocol
					dataOpen, errsOpen = false, false
					break
				}
				eventType := strings.TrimSpace(gjson.GetBytes(event, "type").String())
				if eventType == wsEventTypeError {
					errMsg := responsesWebsocketErrorMessageFromPayload(event)
					lastRealError = errMsg
					kind := responsesreplay.ClassifyFailureForRequest(responsesErrorStatus(errMsg), string(event), payload)
					if processFailure(kind) {
						dataOpen, errsOpen = false, false
						break
					}
					if !flush() || !emit(event) {
						terminalFailure = responsesreplay.FailureCanceled
					} else {
						terminalErrorForwarded = true
					}
					dataOpen, errsOpen = false, false
					break
				}
				if !committed && responsesWebsocketBufferableStartupEvent(eventType) {
					if buffer.append(event) {
						continue
					}
					if !flush() || !emit(event) {
						terminalFailure = responsesreplay.FailureCanceled
						dataOpen, errsOpen = false, false
						break
					}
					committed = true
					outcome.Committed = true
					continue
				}
				if !committed {
					if !flush() {
						terminalFailure = responsesreplay.FailureCanceled
						dataOpen, errsOpen = false, false
						break
					}
					committed = true
					outcome.Committed = true
				}
				if !emit(event) {
					terminalFailure = responsesreplay.FailureCanceled
					dataOpen, errsOpen = false, false
					break
				}
				if isResponsesWebsocketCompletionEvent(eventType) {
					completed = true
					dataOpen, errsOpen = false, false
					break
				}
			}
		}

		if !completed && !retry && terminalFailure == responsesreplay.FailureNone {
			if processFailure(responsesreplay.FailureAuthOrRoute) {
				retry = true
			}
		}
		cancelAttempt()
		if retry {
			buffer.reset()
			continue
		}
		outcome.Constraints = constraints
		outcome.Completed = completed
		outcome.Failure = terminalFailure
		if completed {
			outcome.Failure = responsesreplay.FailureNone
		} else if !terminalErrorForwarded && terminalFailure != responsesreplay.FailureCanceled && lastRealError != nil {
			errOut <- lastRealError
		}
		return
	}
}

func responsesWebsocketBufferableStartupEvent(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "codex.rate_limits", "response.created", "response.in_progress":
		return true
	default:
		return false
	}
}

func unionResponsesWebsocketAuthIDs(sets ...map[string]struct{}) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0)
	for _, set := range sets {
		for authID := range set {
			authID = strings.TrimSpace(authID)
			if authID == "" || authID == "<anonymous>" {
				continue
			}
			if _, ok := seen[authID]; ok {
				continue
			}
			seen[authID] = struct{}{}
			result = append(result, authID)
		}
	}
	return result
}
