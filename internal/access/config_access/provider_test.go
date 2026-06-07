package configaccess

import (
	"context"
	"net/http/httptest"
	"testing"

	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
)

func TestAuthenticateAcceptsExtraAuthHeader(t *testing.T) {
	t.Parallel()

	provider := newProvider(sdkaccess.DefaultAccessProviderName, []string{"proxy-secret"}, []string{"X-Proxy-API-Key"})
	request := httptest.NewRequest("GET", "/v1/models", nil)
	request.Header.Set("X-Proxy-API-Key", "proxy-secret")

	result, authErr := provider.Authenticate(context.Background(), request)
	if authErr != nil {
		t.Fatalf("Authenticate returned error: %v", authErr)
	}
	if result == nil {
		t.Fatal("Authenticate returned nil result")
	}
	if result.Principal != "proxy-secret" {
		t.Fatalf("Principal = %q, want %q", result.Principal, "proxy-secret")
	}
	if result.Metadata["source"] != "header:x-proxy-api-key" {
		t.Fatalf("source metadata = %q, want %q", result.Metadata["source"], "header:x-proxy-api-key")
	}
}

func TestAuthenticateKeepsBuiltInHeadersWithExtraAuthHeaders(t *testing.T) {
	t.Parallel()

	provider := newProvider(sdkaccess.DefaultAccessProviderName, []string{"proxy-secret"}, []string{" X-Proxy-API-Key ", "", "Authorization"})
	request := httptest.NewRequest("GET", "/v1/models", nil)
	request.Header.Set("Authorization", "Bearer proxy-secret")

	result, authErr := provider.Authenticate(context.Background(), request)
	if authErr != nil {
		t.Fatalf("Authenticate returned error: %v", authErr)
	}
	if result == nil {
		t.Fatal("Authenticate returned nil result")
	}
	if result.Metadata["source"] != "authorization" {
		t.Fatalf("source metadata = %q, want %q", result.Metadata["source"], "authorization")
	}
}
