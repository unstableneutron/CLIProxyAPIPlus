package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func newChatGPTBackendPassthroughTestServer(t *testing.T, upstreamURL string, upstreamClient *http.Client) *Server {
	t.Helper()

	server := newTestServer(t)
	server.cfg.SDKConfig.APIKeys = []string{"acct-123"}
	server.cfg.SDKConfig.ExtraAuthHeaders = []string{"ChatGPT-Account-ID"}
	server.applyAccessConfig(nil, server.cfg)
	server.chatGPTBackendPassthroughBaseURL = upstreamURL
	server.chatGPTBackendPassthroughClient = upstreamClient
	return server
}

func registerCodexPassthroughAuth(t *testing.T, server *Server, id, accountID, token string, createdAt time.Time) {
	t.Helper()

	_, err := server.handlers.AuthManager.Register(context.Background(), &auth.Auth{
		ID:        id,
		Provider:  "codex",
		Status:    auth.StatusActive,
		CreatedAt: createdAt,
		Attributes: map[string]string{
			"chatgpt_account_id": accountID,
		},
		Metadata: map[string]any{
			"access_token": token,
		},
	})
	if err != nil {
		t.Fatalf("register codex auth: %v", err)
	}
}

func TestChatGPTBackendPassthroughForwardsUnmatchedBackendPathWithStoredAuth(t *testing.T) {
	var upstreamCalled bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		if r.Method != http.MethodPatch {
			t.Fatalf("upstream method = %q, want PATCH", r.Method)
		}
		if r.URL.Path != "/backend-api/codex/models" {
			t.Fatalf("upstream path = %q, want /backend-api/codex/models", r.URL.Path)
		}
		if r.URL.RawQuery != "client_version=0.137.0&trace=1" {
			t.Fatalf("upstream query = %q", r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer stored-token" {
			t.Fatalf("upstream Authorization = %q, want stored token", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-123" {
			t.Fatalf("upstream ChatGPT-Account-ID = %q, want acct-123", got)
		}
		if got := r.Header.Get("Proxy-Authorization"); got != "" {
			t.Fatalf("Proxy-Authorization forwarded = %q, want empty", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if string(body) != "request-payload" {
			t.Fatalf("upstream body = %q, want request-payload", string(body))
		}
		w.Header().Set("X-Upstream", "ok")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("upstream-body"))
	}))
	defer upstream.Close()

	server := newChatGPTBackendPassthroughTestServer(t, upstream.URL, upstream.Client())
	registerCodexPassthroughAuth(t, server, "older", "acct-123", "stored-token", time.Unix(100, 0))

	req := httptest.NewRequest(http.MethodPatch, "/backend-api/codex/models?client_version=0.137.0&trace=1", strings.NewReader("request-payload"))
	req.Header.Set("Authorization", "Bearer inbound-token")
	req.Header.Set("ChatGPT-Account-ID", "acct-123")
	req.Header.Set("Proxy-Authorization", "proxy-secret")
	rr := httptest.NewRecorder()

	server.engine.ServeHTTP(rr, req)

	if !upstreamCalled {
		t.Fatal("upstream was not called")
	}
	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusAccepted, rr.Body.String())
	}
	if got := rr.Body.String(); got != "upstream-body" {
		t.Fatalf("body = %q, want upstream-body", got)
	}
	if got := rr.Header().Get("X-Upstream"); got != "ok" {
		t.Fatalf("X-Upstream = %q, want ok", got)
	}
	if got := rr.Header().Get("Connection"); got != "" {
		t.Fatalf("Connection response header = %q, want empty", got)
	}
}

func TestChatGPTBackendPassthroughPreservesInboundAuthorizationWithoutMatchingAuth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer inbound-token" {
			t.Fatalf("upstream Authorization = %q, want inbound token", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-123" {
			t.Fatalf("upstream ChatGPT-Account-ID = %q, want acct-123", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer upstream.Close()

	server := newChatGPTBackendPassthroughTestServer(t, upstream.URL, upstream.Client())
	registerCodexPassthroughAuth(t, server, "other-account", "acct-other", "stored-token", time.Unix(100, 0))

	req := httptest.NewRequest(http.MethodGet, "/backend-api/wham/config/bundle", nil)
	req.Header.Set("Authorization", "Bearer inbound-token")
	req.Header.Set("ChatGPT-Account-ID", "acct-123")
	rr := httptest.NewRecorder()

	server.engine.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if got := rr.Body.String(); got != "ok" {
		t.Fatalf("body = %q, want ok", got)
	}
}

func TestChatGPTBackendPassthroughRequiresBothAuthHeaders(t *testing.T) {
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()

	server := newChatGPTBackendPassthroughTestServer(t, upstream.URL, upstream.Client())

	tests := []struct {
		name    string
		headers map[string]string
	}{
		{
			name: "missing authorization",
			headers: map[string]string{
				"ChatGPT-Account-ID": "acct-123",
			},
		},
		{
			name: "missing account id",
			headers: map[string]string{
				"Authorization": "Bearer inbound-token",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/backend-api/ps/plugins/list", nil)
			for key, value := range tc.headers {
				req.Header.Set(key, value)
			}
			rr := httptest.NewRecorder()

			server.engine.ServeHTTP(rr, req)

			if rr.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusNotFound, rr.Body.String())
			}
		})
	}

	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d, want 0", upstreamCalls)
	}
}

func TestChatGPTBackendPassthroughDoesNotOverrideExistingCodexRoutes(t *testing.T) {
	var upstreamCalls int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		w.WriteHeader(http.StatusTeapot)
	}))
	defer upstream.Close()

	server := newChatGPTBackendPassthroughTestServer(t, upstream.URL, upstream.Client())

	req := httptest.NewRequest(http.MethodPost, "/backend-api/codex/responses", strings.NewReader("{"))
	req.Header.Set("Authorization", "Bearer inbound-token")
	req.Header.Set("ChatGPT-Account-ID", "acct-123")
	rr := httptest.NewRecorder()

	server.engine.ServeHTTP(rr, req)

	if upstreamCalls != 0 {
		t.Fatalf("existing route called passthrough upstream %d times", upstreamCalls)
	}
	if rr.Code == http.StatusTeapot {
		t.Fatalf("status = %d, passthrough upstream response leaked", rr.Code)
	}
}
