package auth

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func discardStreamChunks(ch <-chan cliproxyexecutor.StreamChunk) {
	if ch == nil {
		return
	}
	go func() {
		for range ch {
		}
	}()
}

// abandonStreamAttempt cancels a rejected or superseded provider stream and
// synchronously drains it so the attempt is fully released before another
// model, refreshed credential, or auth retry begins. Cancellation aborts the
// upstream transport (requests are created with the stream context), so the
// drain terminates promptly and no abandoned stream keeps consuming tokens
// concurrently with its replacement. When the caller's own context ends, no
// retry ordering remains to protect, so draining falls back to a detached
// discard rather than blocking the caller's return on a provider that ignores
// cancellation.
func abandonStreamAttempt(ctx context.Context, cancel context.CancelFunc, ch <-chan cliproxyexecutor.StreamChunk) {
	if cancel != nil {
		cancel()
	}
	if ch == nil {
		return
	}
	var callerDone <-chan struct{}
	if ctx != nil {
		callerDone = ctx.Done()
	}
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return
			}
		case <-callerDone:
			discardStreamChunks(ch)
			return
		}
	}
}

type streamBootstrapError struct {
	cause   error
	headers http.Header
}

func cloneHTTPHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}

func newStreamBootstrapError(err error, headers http.Header) error {
	if err == nil {
		return nil
	}
	upstreamAttempt := hasUpstreamExecutionAttempt(err)
	err = unwrapUpstreamExecutionAttempt(err)
	bootstrapErr := &streamBootstrapError{
		cause:   err,
		headers: cloneHTTPHeader(headers),
	}
	if upstreamAttempt {
		return markUpstreamExecutionAttempt(bootstrapErr)
	}
	return bootstrapErr
}

func (e *streamBootstrapError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *streamBootstrapError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *streamBootstrapError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return cloneHTTPHeader(e.headers)
}

func streamErrorResult(headers http.Header, err error) *cliproxyexecutor.StreamResult {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Err: err}
	close(ch)
	return &cliproxyexecutor.StreamResult{
		Headers: cloneHTTPHeader(headers),
		Chunks:  ch,
	}
}

func validateStreamResult(result *cliproxyexecutor.StreamResult, err error) (*cliproxyexecutor.StreamResult, error) {
	if err != nil {
		return result, err
	}
	if result == nil || result.Chunks == nil {
		return result, &Error{Code: "empty_stream", Message: "upstream stream has no source", Retryable: true}
	}
	return result, nil
}

func readStreamBootstrap(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk) ([]cliproxyexecutor.StreamChunk, bool, bool, error) {
	if ch == nil {
		return nil, true, false, nil
	}
	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	var bootstrap streamBootstrapState
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		if ctx != nil {
			select {
			case <-ctx.Done():
				return nil, false, false, ctx.Err()
			case chunk, ok = <-ch:
			}
		} else {
			chunk, ok = <-ch
		}
		if !ok {
			bootstrap.finish()
			if !bootstrap.sawPayload {
				return buffered, true, false, nil
			}
			return buffered, bootstrap.isEmptyCompletion(), false, nil
		}
		if chunk.Err != nil {
			if bootstrap.hasMeaningfulOutput() {
				buffered = append(buffered, chunk)
				return buffered, false, false, nil
			}
			return nil, false, false, chunk.Err
		}
		if chunk.Bootstrap {
			return buffered, false, true, nil
		}
		buffered = append(buffered, chunk)
		// Downstream websocket handlers own protocol-level startup buffering so
		// they can distinguish retryable pre-response metadata from a partial
		// response that must terminate the current session. Hand them the first
		// real frame instead of collapsing a prematurely closed response into an
		// empty-completion retry at the credential layer.
		if cliproxyexecutor.DownstreamWebsocket(ctx) && len(bytes.TrimSpace(chunk.Payload)) > 0 {
			return buffered, false, false, nil
		}
		if bootstrap.observe(chunk.Payload) {
			return buffered, false, false, nil
		}
		if bootstrap.isTerminalEmpty() {
			return buffered, true, false, nil
		}
	}
}

func (m *Manager) wrapStreamResult(ctx context.Context, auth *Auth, provider, resultModel, routeModel string, headers http.Header, buffered []cliproxyexecutor.StreamChunk, remaining <-chan cliproxyexecutor.StreamChunk, bootstrapped bool, aliasResult OAuthModelAliasResult, ephemeralResult bool, opts cliproxyexecutor.Options, releaseAttempt func()) *cliproxyexecutor.StreamResult {
	out := make(chan cliproxyexecutor.StreamChunk)
	streamStart := time.Now()
	go func() {
		defer close(out)
		if releaseAttempt != nil {
			defer releaseAttempt()
		}
		var failed bool
		forward := true
		var rewriter *StreamRewriter
		if aliasResult.ForceMapping && strings.TrimSpace(aliasResult.OriginalAlias) != "" {
			rewriter = NewStreamRewriter(StreamRewriteOptions{RewriteModel: aliasResult.OriginalAlias})
		}
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if chunk.Err != nil && !failed {
				failed = true
				entry := logEntryWithRequestID(ctx)
				warnLogUpstreamFailure(ctx, entry, provider, resultModel, auth, time.Since(streamStart), chunk.Err)
				rerr := resultErrorFromError(chunk.Err)
				action, okAction := matchRequestScopedErrorAction(auth, chunk.Err, m.runtimeConfigSnapshot())
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: opts}
				applyRequestScopedActionToResult(action, okAction, &result)
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			}
			if !forward {
				return false
			}
			if chunk.Err != nil || chunk.Bootstrap {
				if ctx == nil {
					out <- chunk
					return true
				}
				select {
				case <-ctx.Done():
					forward = false
					return false
				case out <- chunk:
					return true
				}
			}
			if len(chunk.Payload) == 0 {
				return true
			}
			payload := rewriteForceMappedStreamChunk(rewriter, chunk.Payload)
			if len(payload) == 0 {
				return true
			}
			chunk.Payload = payload
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				forward = false
				return false
			case out <- chunk:
				return true
			}
		}
		if bootstrapped {
			if ok := emit(cliproxyexecutor.StreamChunk{Bootstrap: true}); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		for _, chunk := range buffered {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		for chunk := range remaining {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		if tail := finishForceMappedStreamChunks(rewriter); len(tail) > 0 {
			tailChunk := cliproxyexecutor.StreamChunk{Payload: tail}
			if !emit(tailChunk) {
				return
			}
		}
		if !failed && (ephemeralResult || claudeOAuthRequestCancellation(ctx, auth, nil) == nil) {
			m.recordExecutionResult(ctx, Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: true, Options: opts}, auth, ephemeralResult)
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}
}

func (m *Manager) replaceHomeExecutionLifecycleAuth(lifecycle cliproxyexecutor.ExecutionLifecycle, auth *Auth) {
	selection, ok := lifecycle.(*HomeDispatchSelection)
	if !ok || selection == nil {
		return
	}
	m.replaceHomeSelectionAuth(selection, auth)
}

func (m *Manager) executeStreamWithModelPool(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, routeModel, executionModel string, execModels []string, pooled bool, aliasResult OAuthModelAliasResult, routing *apiKeyModelRoutingSnapshot, allowRetry bool, ephemeralResult bool, unauthorizedRefreshTried map[string]struct{}, releaseAttempt func()) (*cliproxyexecutor.StreamResult, error) {
	if executor == nil {
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	ctx = contextWithRequestedModelAlias(ctx, opts, routeModel)
	var lastErr error
	var upstreamErr error
	didRefreshOnUnauthorized := false
	if auth != nil && unauthorizedRefreshTried != nil {
		_, didRefreshOnUnauthorized = unauthorizedRefreshTried[auth.ID]
	}
	for idx, execModel := range execModels {
		ctx = newUpstreamAttemptContext(ctx)
		resultModel := m.stateModelForExecution(auth, routeModel, execModel, pooled)
		recordProxySelection(ctx, auth, routeModel, execModel)
		execReq := req
		execReq.Model = execModel
		if executionModel != "" {
			execReq.Model = executionModel
		}
		execOpts := opts
		var errIntercept error
		execReq, execOpts, errIntercept = applyRequestAfterAuthInterceptor(ctx, executor, provider, execReq, execOpts, requestedModelAliasFromOptions(execOpts, routeModel))
		if errIntercept != nil {
			return nil, errIntercept
		}
		if executionModel == "" {
			execReq = attachResolvedAPIKeyModelInfo(routing, execReq, auth, routeModel, execModel)
		}
		if errCtx := ctx.Err(); errCtx != nil {
			return nil, errCtx
		}
		entry := logEntryWithRequestID(ctx)
		startStream := time.Now()
		// Each provider stream gets its own child context so a rejected or
		// superseded attempt can be canceled and drained before the next
		// model, refreshed credential, or auth attempt starts.
		streamCtx, cancelStream := context.WithCancel(ctx)
		streamResult, errStream := executor.ExecuteStream(streamCtx, auth, execReq, execOpts)
		errStream = markUpstreamExecutionAttemptFromContext(ctx, errStream)
		if hasUpstreamExecutionAttempt(errStream) {
			upstreamErr = errStream
		}
		durationStream := time.Since(startStream)
		if errStream != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				cancelStream()
				return nil, errCtx
			}
			if allowRetry {
				alreadyTried := didRefreshOnUnauthorized
				willAttemptHomeRefresh := ephemeralResult && !alreadyTried && auth != nil && auth.AuthKind() == AuthKindOAuth && isUnauthorizedError(errStream)
				refreshCtx := newUpstreamAttemptContext(ctx)
				refreshed, okRefresh, errRefresh := m.tryRefreshExecutionAuthAfterUnauthorized(refreshCtx, executor, auth, errStream, alreadyTried, ephemeralResult)
				if willAttemptHomeRefresh {
					didRefreshOnUnauthorized = true
					if unauthorizedRefreshTried != nil {
						unauthorizedRefreshTried[auth.ID] = struct{}{}
					}
				}
				if errRefresh != nil {
					errStream = errRefresh
					upstreamErr = nil
					warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, durationStream, errStream)
				} else if okRefresh {
					auth = refreshed
					m.replaceHomeExecutionLifecycleAuth(execOpts.ExecutionLifecycle, auth)
					publishSelectedAuthMetadata(execOpts.Metadata, auth)
					didRefreshOnUnauthorized = true
					var staleChunks <-chan cliproxyexecutor.StreamChunk
					if streamResult != nil {
						staleChunks = streamResult.Chunks
					}
					abandonStreamAttempt(ctx, cancelStream, staleChunks)
					ctx = newUpstreamAttemptContext(ctx)
					streamCtx, cancelStream = context.WithCancel(ctx)
					startRetry := time.Now()
					streamResult, errStream = executor.ExecuteStream(streamCtx, auth, execReq, execOpts)
					errStream = markUpstreamExecutionAttemptFromContext(ctx, errStream)
					if hasUpstreamExecutionAttempt(errStream) {
						upstreamErr = errStream
					}
					durationRetry := time.Since(startRetry)
					if errStream != nil {
						warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, durationRetry, errStream)
						if errCtx := ctx.Err(); errCtx != nil {
							cancelStream()
							return nil, errCtx
						}
					}
				} else {
					warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, durationStream, errStream)
				}
			} else {
				warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, durationStream, errStream)
			}
		}
		if !ephemeralResult {
			if errCancel := claudeOAuthRequestCancellation(ctx, auth, errStream); errCancel != nil {
				cancelStream()
				return nil, errCancel
			}
		}
		streamResult, errStream = validateStreamResult(streamResult, errStream)
		errStream = markUpstreamExecutionAttemptFromContext(ctx, errStream)
		if errStream != nil {
			var staleChunks <-chan cliproxyexecutor.StreamChunk
			if streamResult != nil {
				staleChunks = streamResult.Chunks
			}
			abandonStreamAttempt(ctx, cancelStream, staleChunks)
			rerr := resultErrorFromError(errStream)
			action, okAction := matchRequestScopedErrorAction(auth, errStream, m.runtimeConfigSnapshot())
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts}
			result.RetryAfter = retryAfterFromError(errStream)
			if isCredentialScopedError(errStream) {
				result.CredentialScope = true
			}
			applyRequestScopedActionToResult(action, okAction, &result)
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			if okAction {
				if isRequestScopedStop(action, okAction) {
					return nil, wrapRequestStopError(errStream)
				}
				lastErr = errStream
				if result.CredentialScope {
					return nil, preferredExecutionAttemptError(errStream, upstreamErr)
				}
				continue
			}
			if isRequestInvalidError(errStream) {
				return nil, errStream
			}
			lastErr = errStream
			if result.CredentialScope {
				return nil, preferredExecutionAttemptError(errStream, upstreamErr)
			}
			continue
		}

		buffered, closed, bootstrapped, bootstrapErr := readStreamBootstrap(ctx, streamResult.Chunks)
		bootstrapErr = markUpstreamExecutionAttemptFromContext(ctx, bootstrapErr)
		if hasUpstreamExecutionAttempt(bootstrapErr) {
			upstreamErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
		}
		if bootstrapErr != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				abandonStreamAttempt(ctx, cancelStream, streamResult.Chunks)
				return nil, errCtx
			}
			if allowRetry {
				alreadyTried := didRefreshOnUnauthorized
				willAttemptHomeRefresh := ephemeralResult && !alreadyTried && auth != nil && auth.AuthKind() == AuthKindOAuth && isUnauthorizedError(bootstrapErr)
				refreshCtx := newUpstreamAttemptContext(ctx)
				refreshed, okRefresh, errRefresh := m.tryRefreshExecutionAuthAfterUnauthorized(refreshCtx, executor, auth, bootstrapErr, alreadyTried, ephemeralResult)
				if willAttemptHomeRefresh {
					didRefreshOnUnauthorized = true
					if unauthorizedRefreshTried != nil {
						unauthorizedRefreshTried[auth.ID] = struct{}{}
					}
				}
				if errRefresh != nil {
					abandonStreamAttempt(ctx, cancelStream, streamResult.Chunks)
					bootstrapErr = errRefresh
					upstreamErr = nil
					warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startStream), bootstrapErr)
					streamResult = &cliproxyexecutor.StreamResult{}
				} else if okRefresh {
					abandonStreamAttempt(ctx, cancelStream, streamResult.Chunks)
					auth = refreshed
					m.replaceHomeExecutionLifecycleAuth(execOpts.ExecutionLifecycle, auth)
					publishSelectedAuthMetadata(execOpts.Metadata, auth)
					didRefreshOnUnauthorized = true
					ctx = newUpstreamAttemptContext(ctx)
					streamCtx, cancelStream = context.WithCancel(ctx)
					startRetry := time.Now()
					retryStream, retryErr := executor.ExecuteStream(streamCtx, auth, execReq, execOpts)
					retryErr = markUpstreamExecutionAttemptFromContext(ctx, retryErr)
					retryStream, retryErr = validateStreamResult(retryStream, retryErr)
					retryErr = markUpstreamExecutionAttemptFromContext(ctx, retryErr)
					if retryErr != nil {
						if retryStream != nil {
							discardStreamChunks(retryStream.Chunks)
						}
						if errCtx := ctx.Err(); errCtx != nil {
							var retryChunks <-chan cliproxyexecutor.StreamChunk
							if retryStream != nil {
								retryChunks = retryStream.Chunks
							}
							abandonStreamAttempt(ctx, cancelStream, retryChunks)
							return nil, errCtx
						}
						bootstrapErr = retryErr
						warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startRetry), bootstrapErr)
						streamResult = &cliproxyexecutor.StreamResult{}
					} else {
						streamResult = retryStream
						buffered, closed, bootstrapped, bootstrapErr = readStreamBootstrap(ctx, streamResult.Chunks)
						bootstrapErr = markUpstreamExecutionAttemptFromContext(ctx, bootstrapErr)
						if bootstrapErr != nil {
							warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startRetry), bootstrapErr)
						}
					}
				} else {
					warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startStream), bootstrapErr)
				}
			} else {
				warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startStream), bootstrapErr)
			}
			if hasUpstreamExecutionAttempt(bootstrapErr) {
				upstreamErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
			}
		}
		if !ephemeralResult {
			if errCancel := claudeOAuthRequestCancellation(ctx, auth, bootstrapErr); errCancel != nil {
				abandonStreamAttempt(ctx, cancelStream, streamResult.Chunks)
				return nil, errCancel
			}
		}
		if bootstrapErr != nil {
			action, okAction := matchRequestScopedErrorAction(auth, bootstrapErr, m.runtimeConfigSnapshot())
			if okAction {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				if isCredentialScopedError(bootstrapErr) {
					result.CredentialScope = true
				}
				applyRequestScopedActionToResult(action, okAction, &result)
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
				abandonStreamAttempt(ctx, cancelStream, streamResult.Chunks)
				if isRequestScopedStop(action, okAction) {
					return nil, wrapRequestStopError(bootstrapErr)
				}
				lastErr = bootstrapErr
				if result.CredentialScope {
					currentErr := newStreamBootstrapError(bootstrapErr, streamResult.Headers)
					return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
				}
				continue
			}
			if isRequestInvalidError(bootstrapErr) {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				if isCredentialScopedError(bootstrapErr) {
					result.CredentialScope = true
				}
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
				abandonStreamAttempt(ctx, cancelStream, streamResult.Chunks)
				return nil, bootstrapErr
			}
			if idx < len(execModels)-1 {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				if isCredentialScopedError(bootstrapErr) {
					result.CredentialScope = true
				}
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
				abandonStreamAttempt(ctx, cancelStream, streamResult.Chunks)
				lastErr = bootstrapErr
				if result.CredentialScope {
					currentErr := newStreamBootstrapError(bootstrapErr, streamResult.Headers)
					return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
				}
				continue
			}
			rerr := resultErrorFromError(bootstrapErr)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts}
			result.RetryAfter = retryAfterFromError(bootstrapErr)
			if isCredentialScopedError(bootstrapErr) {
				result.CredentialScope = true
			}
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			abandonStreamAttempt(ctx, cancelStream, streamResult.Chunks)
			currentErr := newStreamBootstrapError(bootstrapErr, streamResult.Headers)
			return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
		}
		payloadBytes := 0
		for _, chunk := range buffered {
			payloadBytes += len(chunk.Payload)
		}
		if closed && (payloadBytes == 0 || isEmptyCompletion(buffered)) {
			cancelStream()
			var emptyErr error = errEmptyCompletion
			if payloadBytes == 0 {
				emptyErr = &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true}
			}
			emptyErr = markUpstreamExecutionAttemptFromContext(ctx, emptyErr)
			currentErr := newStreamBootstrapError(emptyErr, streamResult.Headers)
			if hasUpstreamExecutionAttempt(emptyErr) {
				upstreamErr = currentErr
			}
			warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startStream), emptyErr)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: resultErrorFromError(emptyErr), Options: execOpts}
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			discardStreamChunks(streamResult.Chunks)
			if idx < len(execModels)-1 {
				lastErr = emptyErr
				continue
			}
			return nil, preferredExecutionAttemptError(currentErr, upstreamErr)
		}

		remaining := streamResult.Chunks
		if closed {
			discardStreamChunks(streamResult.Chunks)
			closedCh := make(chan cliproxyexecutor.StreamChunk)
			close(closedCh)
			remaining = closedCh
		}
		attemptAliasResult := resolveAttemptAliasResult(routing, auth, routeModel, execModel, aliasResult)
		cancelAcceptedStream := cancelStream
		releaseAcceptedStream := func() {
			cancelAcceptedStream()
			if releaseAttempt != nil {
				releaseAttempt()
			}
		}
		return m.wrapStreamResult(ctx, auth.Clone(), provider, resultModel, routeModel, streamResult.Headers, buffered, remaining, bootstrapped, attemptAliasResult, ephemeralResult, execOpts, releaseAcceptedStream), nil
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no upstream model available"}
	}
	return nil, preferredExecutionAttemptError(lastErr, upstreamErr)
}
