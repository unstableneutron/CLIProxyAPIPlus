package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

func TestProxyObservabilityMiddlewareAddsServerTimingHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ProxyObservabilityMiddleware())
	router.GET("/ping", func(c *gin.Context) {
		logging.SetUpstreamTransport(c.Request.Context(), "http")
		logging.SetUpstreamProtocol(c.Request.Context(), "h1")
		logging.SetSlot(c.Request.Context(), "6f2a9c0e1b3d4a55")
		c.String(http.StatusOK, "ok")
	})

	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	router.ServeHTTP(recorder, req)

	if got := recorder.Header().Get("Via"); got != "" {
		t.Fatalf("Via should not be emitted: %q", got)
	}
	if got := recorder.Header().Get("Proxy-Status"); got != "" {
		t.Fatalf("Proxy-Status should not be emitted: %q", got)
	}
	serverTiming := recorder.Header().Get("Server-Timing")
	for _, want := range []string{`cpa.trace;desc=`, `cpa.path;desc="http.h1>http.h1"`, `cpa.slot;desc="6f2a9c0"`} {
		if !strings.Contains(serverTiming, want) {
			t.Fatalf("Server-Timing missing %q: %q", want, serverTiming)
		}
	}
}
