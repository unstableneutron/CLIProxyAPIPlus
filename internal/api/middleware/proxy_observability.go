package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

// ProxyObservabilityMiddleware attaches proxy-local trace context and injects
// standards-based response diagnostics. Trace context is intentionally not
// forwarded upstream.
func ProxyObservabilityMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c == nil || c.Request == nil {
			c.Next()
			return
		}

		ctx := c.Request.Context()
		trace := logging.GetTraceContext(ctx)
		if trace.Traceparent == "" {
			trace = logging.NewTraceContext(c.Request.Header.Get("traceparent"))
			ctx = logging.WithTraceContext(ctx, trace)
		}
		ctx = logging.WithProxyStatusHolder(ctx)
		logging.SetDownstreamProtocol(ctx, protocolToken(c.Request))
		if isWebsocketUpgradeRequest(c.Request) {
			logging.SetDownstreamTransport(ctx, "websocket")
		} else {
			logging.SetDownstreamTransport(ctx, "http")
		}

		c.Request = c.Request.WithContext(ctx)
		c.Writer = &proxyObservabilityWriter{ResponseWriter: c.Writer, ctx: ctx}

		c.Next()
		logging.ApplyProxyObservabilityHeaders(ctx, c.Writer.Header())
	}
}

type proxyObservabilityWriter struct {
	gin.ResponseWriter
	ctx     context.Context
	applied bool
}

func (w *proxyObservabilityWriter) WriteHeader(code int) {
	w.apply()
	w.ResponseWriter.WriteHeader(code)
}

func (w *proxyObservabilityWriter) WriteHeaderNow() {
	w.apply()
	w.ResponseWriter.WriteHeaderNow()
}

func (w *proxyObservabilityWriter) WriteString(s string) (int, error) {
	w.apply()
	return w.ResponseWriter.WriteString(s)
}

func (w *proxyObservabilityWriter) Write(data []byte) (int, error) {
	w.apply()
	return w.ResponseWriter.Write(data)
}

func (w *proxyObservabilityWriter) Flush() {
	w.apply()
	w.ResponseWriter.Flush()
}

func (w *proxyObservabilityWriter) apply() {
	if w == nil || w.applied {
		return
	}
	w.applied = true
	logging.ApplyProxyObservabilityHeaders(w.ctx, w.Header())
}

func isWebsocketUpgradeRequest(req *http.Request) bool {
	if req == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(req.Header.Get("Upgrade")), "websocket")
}

func protocolToken(req *http.Request) string {
	if req == nil {
		return "unk"
	}
	return logging.HTTPProtocolToken(req.ProtoMajor)
}
