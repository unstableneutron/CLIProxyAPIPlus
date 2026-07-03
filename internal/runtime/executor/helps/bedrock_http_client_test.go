package helps

import (
	"crypto/tls"
	"net/http"
	"reflect"
	"testing"
)

func TestNewBedrockHTTPClientDisablesPostQuantumCurves(t *testing.T) {
	t.Parallel()

	client := NewBedrockHTTPClient(t.Context(), nil, nil, 0)
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", client.Transport)
	}
	if transport.TLSClientConfig == nil {
		t.Fatal("TLSClientConfig is nil")
	}
	want := []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384, tls.CurveP521}
	if !reflect.DeepEqual(transport.TLSClientConfig.CurvePreferences, want) {
		t.Fatalf("curve preferences = %v, want %v", transport.TLSClientConfig.CurvePreferences, want)
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("ForceAttemptHTTP2 = false, want true")
	}
}
