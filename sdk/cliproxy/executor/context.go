package executor

import "context"

type downstreamWebsocketContextKey struct{}
type requireUpstreamWebsocketContextKey struct{}
type streamActivityContextKey struct{}

// WithDownstreamWebsocket marks the current request as coming from a downstream websocket connection.
func WithDownstreamWebsocket(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, downstreamWebsocketContextKey{}, true)
}

// DownstreamWebsocket reports whether the current request originates from a downstream websocket connection.
func DownstreamWebsocket(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	raw := ctx.Value(downstreamWebsocketContextKey{})
	enabled, ok := raw.(bool)
	return ok && enabled
}

// WithRequiredUpstreamWebsocket marks a request whose incremental context is valid only on the current upstream websocket.
func WithRequiredUpstreamWebsocket(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, requireUpstreamWebsocketContextKey{}, true)
}

// RequiredUpstreamWebsocket reports whether falling back to an HTTP upstream would lose request context.
func RequiredUpstreamWebsocket(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	raw := ctx.Value(requireUpstreamWebsocketContextKey{})
	enabled, ok := raw.(bool)
	return ok && enabled
}

// WithStreamActivityCallback installs an internal callback for provider protocol
// activity that is not yet sufficient to commit a streaming auth attempt.
func WithStreamActivityCallback(ctx context.Context, callback func()) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if callback == nil {
		return ctx
	}
	return context.WithValue(ctx, streamActivityContextKey{}, callback)
}

// NotifyStreamActivity reports provider protocol activity without committing the
// streaming auth attempt or exposing a control marker to downstream clients.
func NotifyStreamActivity(ctx context.Context) {
	if ctx == nil {
		return
	}
	callback, ok := ctx.Value(streamActivityContextKey{}).(func())
	if ok && callback != nil {
		callback()
	}
}
