package helps

import (
	"context"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type utlsClientRoundTripFunc func(*http.Request) (*http.Response, error)

func (f utlsClientRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestSelectUtlsProfileDefaultsCodexByTransport(t *testing.T) {
	t.Parallel()

	auth := &cliproxyauth.Auth{Provider: "codex"}

	httpsProfile, err := selectUtlsProfile(nil, auth, utlsTransportHTTPS)
	if err != nil {
		t.Fatalf("select HTTPS profile: %v", err)
	}
	if httpsProfile.Name != "codex-rustls-macos-arm64-0.137-https" {
		t.Fatalf("HTTPS profile = %q", httpsProfile.Name)
	}
	if httpsProfile.RawClientHelloHex == "" {
		t.Fatal("HTTPS profile raw ClientHello hex was empty")
	}

	wsProfile, err := selectUtlsProfile(nil, auth, utlsTransportWebsocket)
	if err != nil {
		t.Fatalf("select websocket profile: %v", err)
	}
	if wsProfile.Name != "codex-rustls-macos-arm64-0.137-ws" {
		t.Fatalf("websocket profile = %q", wsProfile.Name)
	}
	if wsProfile.RawClientHelloHex == "" {
		t.Fatal("websocket profile raw ClientHello hex was empty")
	}
}

func TestSelectUtlsProfileHonorsExplicitChromeAuto(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Codex: config.CodexConfig{TLSProfile: "chrome-auto"}}
	auth := &cliproxyauth.Auth{Provider: "codex"}

	profile, err := selectUtlsProfile(cfg, auth, utlsTransportHTTPS)
	if err != nil {
		t.Fatalf("select profile: %v", err)
	}
	if profile.Name != "chrome-auto" {
		t.Fatalf("profile = %q, want chrome-auto", profile.Name)
	}
	if profile.RawClientHelloHex != "" {
		t.Fatalf("chrome profile raw ClientHello hex = %q, want empty", profile.RawClientHelloHex)
	}
}

func TestSelectUtlsProfileKeepsChromeForNonCodexAuth(t *testing.T) {
	t.Parallel()

	auth := &cliproxyauth.Auth{Provider: "claude"}

	profile, err := selectUtlsProfile(nil, auth, utlsTransportHTTPS)
	if err != nil {
		t.Fatalf("select profile: %v", err)
	}
	if profile.Name != "chrome-auto" {
		t.Fatalf("profile = %q, want chrome-auto", profile.Name)
	}
}

func TestSelectUtlsProfileLoadsGeneratedProfileFile(t *testing.T) {
	t.Parallel()

	profilePath := filepath.Join(t.TempDir(), "codex-ws-profile.json")
	profileJSON := `{
  "name": "custom-codex-ws",
  "source_ja3n_hash": "f81ecb4047f8a1cd5bad262f3c97a040",
  "spec_json": {
    "kind": "utls-fingerprinter-raw-client-hello",
    "raw_client_hello_hex": "` + codexMacOSArm640137WebsocketRawClientHelloHex + `",
    "allow_blunt_mimicry": true
  }
}`
	if err := os.WriteFile(profilePath, []byte(profileJSON), 0o644); err != nil {
		t.Fatalf("write profile file: %v", err)
	}

	cfg := &config.Config{Codex: config.CodexConfig{TLSProfile: "file:" + profilePath}}
	profile, err := selectUtlsProfile(cfg, &cliproxyauth.Auth{Provider: "codex"}, utlsTransportWebsocket)
	if err != nil {
		t.Fatalf("select profile: %v", err)
	}
	if profile.Name != "custom-codex-ws" {
		t.Fatalf("profile name = %q, want custom-codex-ws", profile.Name)
	}
	if profile.RawClientHelloHex != codexMacOSArm640137WebsocketRawClientHelloHex {
		t.Fatal("profile raw ClientHello did not match generated file")
	}
}

func TestCodexWebsocketProfileEmitsBaselineClientHelloFields(t *testing.T) {
	t.Parallel()

	profile, err := selectUtlsProfile(nil, &cliproxyauth.Auth{Provider: "codex"}, utlsTransportWebsocket)
	if err != nil {
		t.Fatalf("select websocket profile: %v", err)
	}
	clientConn, serverConn := net.Pipe()
	defer func() { _ = clientConn.Close() }()
	defer func() { _ = serverConn.Close() }()

	recordCh := make(chan []byte, 1)
	errCh := make(chan error, 1)
	go func() {
		_ = serverConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		record, errRead := readTLSRecordForTest(serverConn)
		if errRead != nil {
			errCh <- errRead
			return
		}
		recordCh <- record
		_ = serverConn.Close()
	}()

	tlsConn, err := newProfiledUTLSConn(clientConn, "chatgpt.com", profile)
	if err != nil {
		t.Fatalf("newProfiledUTLSConn: %v", err)
	}
	if errHandshake := tlsConn.Handshake(); errHandshake == nil {
		t.Fatal("expected handshake to fail after capture server closes")
	}

	var gotRecord []byte
	select {
	case errRead := <-errCh:
		t.Fatalf("read TLS record: %v", errRead)
	case gotRecord = <-recordCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for TLS record")
	}

	wantRecord, err := hex.DecodeString(codexMacOSArm640137WebsocketRawClientHelloHex)
	if err != nil {
		t.Fatalf("decode websocket baseline: %v", err)
	}
	want := fingerprintSpecForTest(t, wantRecord)
	got := fingerprintSpecForTest(t, gotRecord)

	if !reflect.DeepEqual(got.CipherSuites, want.CipherSuites) {
		t.Fatalf("cipher suites = %v, want %v", got.CipherSuites, want.CipherSuites)
	}
	if !reflect.DeepEqual(supportedCurvesForTest(got), supportedCurvesForTest(want)) {
		t.Fatalf("supported groups = %v, want %v", supportedCurvesForTest(got), supportedCurvesForTest(want))
	}
	if !reflect.DeepEqual(alpnProtocolsForTest(got), alpnProtocolsForTest(want)) {
		t.Fatalf("ALPN = %v, want %v", alpnProtocolsForTest(got), alpnProtocolsForTest(want))
	}
	if !reflect.DeepEqual(extensionTypesForTest(got), extensionTypesForTest(want)) {
		t.Fatalf("extensions = %v, want %v", extensionTypesForTest(got), extensionTypesForTest(want))
	}
}

func TestNewUtlsHTTPClientUsesContextRoundTripperForProtectedHost(t *testing.T) {
	t.Parallel()

	called := false
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", utlsClientRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		if req.URL.Hostname() != "chatgpt.com" {
			t.Fatalf("hostname = %q, want chatgpt.com", req.URL.Hostname())
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("{}")),
			Request:    req,
		}, nil
	}))

	client := NewUtlsHTTPClient(ctx, nil, nil, 0)
	resp, err := client.Get("https://chatgpt.com/backend-api/codex/responses")
	if err != nil {
		t.Fatalf("client.Get returned error: %v", err)
	}
	if errClose := resp.Body.Close(); errClose != nil {
		t.Fatalf("response body close returned error: %v", errClose)
	}
	if !called {
		t.Fatal("expected context RoundTripper to handle protected host request")
	}
}

func readTLSRecordForTest(conn net.Conn) ([]byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return nil, err
	}
	length := int(header[3])<<8 | int(header[4])
	body := make([]byte, length)
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, err
	}
	return append(header, body...), nil
}

func fingerprintSpecForTest(t *testing.T, record []byte) *tls.ClientHelloSpec {
	t.Helper()
	spec, err := (&tls.Fingerprinter{AllowBluntMimicry: true}).FingerprintClientHello(record)
	if err != nil {
		t.Fatalf("fingerprint ClientHello: %v", err)
	}
	return spec
}

func supportedCurvesForTest(spec *tls.ClientHelloSpec) []tls.CurveID {
	for _, ext := range spec.Extensions {
		if curves, ok := ext.(*tls.SupportedCurvesExtension); ok {
			return curves.Curves
		}
	}
	return nil
}

func alpnProtocolsForTest(spec *tls.ClientHelloSpec) []string {
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*tls.ALPNExtension); ok {
			return alpn.AlpnProtocols
		}
	}
	return nil
}

func extensionTypesForTest(spec *tls.ClientHelloSpec) []string {
	out := make([]string, 0, len(spec.Extensions))
	for _, ext := range spec.Extensions {
		out = append(out, reflect.TypeOf(ext).String())
	}
	return out
}
