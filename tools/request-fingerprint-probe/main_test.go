package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestRedactHeaderLinesRedactsSensitiveValues(t *testing.T) {
	lines := []HeaderLine{
		{Name: "Authorization", Value: "Bearer secret"},
		{Name: "ChatGPT-Account-ID", Value: "account-id"},
		{Name: "User-Agent", Value: "codex-tui/0.137.0"},
		{Name: "X-Client-Request-Id", Value: "request-id"},
		{Name: "Sec-WebSocket-Key", Value: "websocket-key"},
		{Name: "Session-Id", Value: "session-id"},
		{Name: "Thread-Id", Value: "thread-id"},
		{Name: "X-Codex-Window-Id", Value: "window-id"},
		{Name: "X-Codex-Turn-Metadata", Value: `{"cwd":"/Users/example/project"}`},
	}

	got := redactHeaderLines(lines)

	want := []HeaderLine{
		{Name: "Authorization", Value: redactedValue},
		{Name: "ChatGPT-Account-ID", Value: redactedValue},
		{Name: "User-Agent", Value: "codex-tui/0.137.0"},
		{Name: "X-Client-Request-Id", Value: redactedValue},
		{Name: "Sec-WebSocket-Key", Value: redactedValue},
		{Name: "Session-Id", Value: redactedValue},
		{Name: "Thread-Id", Value: redactedValue},
		{Name: "X-Codex-Window-Id", Value: redactedValue},
		{Name: "X-Codex-Turn-Metadata", Value: redactedValue},
	}
	if len(got) != len(want) {
		t.Fatalf("redacted header count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("header %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestCompareCapturesReportsHTTPHeaderDrift(t *testing.T) {
	left := Capture{
		Name: "codex-exec",
		HTTP: &HTTPFingerprint{
			Method: "POST",
			Target: "/v1/responses",
			Headers: []HeaderLine{
				{Name: "User-Agent", Value: "Codex Desktop/0.137.0"},
				{Name: "OpenAI-Beta", Value: "responses_websockets=2026-02-06"},
				{Name: "Originator", Value: "Codex Desktop"},
			},
		},
	}
	right := Capture{
		Name: "cliproxy",
		HTTP: &HTTPFingerprint{
			Method: "POST",
			Target: "/v1/responses",
			Headers: []HeaderLine{
				{Name: "user-agent", Value: "CLIProxyAPI/0"},
				{Name: "Originator", Value: "Codex CLI"},
			},
		},
	}

	diffs := compareCaptures(left, right)

	joined := strings.Join(diffs, "\n")
	for _, want := range []string{
		"http.headers.names",
		"http.headers.order",
		"http.header.user-agent",
		"http.header.openai-beta",
		"http.header.originator",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("diffs missing %q:\n%s", want, joined)
		}
	}
}

func TestHTTPSinkCapturesHeaderOrderAndBodyDigest(t *testing.T) {
	server, err := newCaptureServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("new capture server: %v", err)
	}
	defer server.Close()

	body := strings.NewReader(`{"input":"hello"}`)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, fmt.Sprintf("http://%s/v1/responses", server.Addr()), body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	_ = resp.Body.Close()

	capture := server.WaitForCapture(context.Background())
	if capture == nil {
		t.Fatal("capture was nil")
	}
	if capture.HTTP == nil {
		t.Fatal("HTTP fingerprint was nil")
	}
	if capture.HTTP.Method != http.MethodPost {
		t.Fatalf("method = %q, want POST", capture.HTTP.Method)
	}
	if capture.HTTP.Target != "/v1/responses" {
		t.Fatalf("target = %q, want /v1/responses", capture.HTTP.Target)
	}
	if capture.HTTP.BodyBytes != int64(len(`{"input":"hello"}`)) {
		t.Fatalf("body bytes = %d", capture.HTTP.BodyBytes)
	}
	if capture.HTTP.BodySHA256 == "" {
		t.Fatal("body sha256 was empty")
	}
	if got := headerValue(capture.HTTP.Headers, "Authorization"); got != redactedValue {
		t.Fatalf("Authorization = %q, want redacted", got)
	}
	if got := headerValue(capture.HTTP.Headers, "Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
}

func TestConnectCaptureParsesClientHello(t *testing.T) {
	server, err := newCaptureServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("new capture server: %v", err)
	}
	defer server.Close()

	proxyURL, err := url.Parse("http://" + server.Addr())
	if err != nil {
		t.Fatalf("parse proxy url: %v", err)
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(proxyURL),
			TLSClientConfig: &tls.Config{
				ServerName: "example.com",
				NextProtos: []string{"h2", "http/1.1"},
			},
			ForceAttemptHTTP2: true,
		},
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "https://example.com/v1/models", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	_, _ = client.Do(req)

	capture := server.WaitForCapture(context.Background())
	if capture == nil {
		t.Fatal("capture was nil")
	}
	if capture.ConnectTarget != "example.com:443" {
		t.Fatalf("connect target = %q, want example.com:443", capture.ConnectTarget)
	}
	if capture.TLS == nil {
		t.Fatal("TLS fingerprint was nil")
	}
	if capture.TLS.ServerName != "example.com" {
		t.Fatalf("server name = %q, want example.com", capture.TLS.ServerName)
	}
	if capture.TLS.JA3 == "" || len(capture.TLS.JA3Hash) != 32 {
		t.Fatalf("JA3/JA3 hash not populated: %+v", capture.TLS)
	}
	if capture.TLS.JA3N == "" || len(capture.TLS.JA3NHash) != 32 {
		t.Fatalf("JA3N/JA3N hash not populated: %+v", capture.TLS)
	}
	if len(capture.TLS.CipherSuites) == 0 {
		t.Fatalf("cipher suites not populated: %+v", capture.TLS)
	}
	if !containsString(capture.TLS.ALPNProtocols, "h2") {
		t.Fatalf("ALPN protocols = %v, want h2", capture.TLS.ALPNProtocols)
	}
}

func TestCompareCapturesReportsTLSDrift(t *testing.T) {
	left := Capture{
		Name: "left",
		TLS: &TLSFingerprint{
			JA3Hash:       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			ALPNProtocols: []string{"h2", "http/1.1"},
		},
	}
	right := Capture{
		Name: "right",
		TLS: &TLSFingerprint{
			JA3Hash:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			ALPNProtocols: []string{"http/1.1"},
		},
	}

	diffs := compareCaptures(left, right)

	joined := strings.Join(diffs, "\n")
	if !strings.Contains(joined, "tls.ja3_hash") {
		t.Fatalf("diffs missing tls.ja3_hash: %v", diffs)
	}
	if !strings.Contains(joined, "tls.alpn_protocols") {
		t.Fatalf("diffs missing tls.alpn_protocols: %v", diffs)
	}
}

func TestSuggestCapturesReportsActionableKnobs(t *testing.T) {
	reference := Capture{
		Name: "codex-exec",
		TLS: &TLSFingerprint{
			JA3NHash:        "apple-native",
			ALPNProtocols:   []string{"h2", "http/1.1"},
			CipherSuites:    []uint16{4865, 4866},
			Extensions:      []uint16{0, 10, 11},
			SupportedGroups: []uint16{29, 23},
		},
		HTTP: &HTTPFingerprint{
			Method: "POST",
			Target: "/v1/responses",
			Headers: []HeaderLine{
				{Name: "User-Agent", Value: "codex-tui/0.137.0"},
				{Name: "OpenAI-Beta", Value: "responses_websockets=2026-02-06"},
			},
		},
	}
	candidate := Capture{
		Name: "cliproxy-utls",
		TLS: &TLSFingerprint{
			JA3NHash:        "chrome-utls",
			ALPNProtocols:   []string{"http/1.1"},
			CipherSuites:    []uint16{4865, 49195},
			Extensions:      []uint16{0, 13, 16},
			SupportedGroups: []uint16{4588, 29, 23},
		},
		HTTP: &HTTPFingerprint{
			Method: "POST",
			Target: "/backend-api/codex/responses",
			Headers: []HeaderLine{
				{Name: "User-Agent", Value: "codex-tui/0.135.0"},
			},
		},
	}

	suggestions := suggestCaptures(reference, candidate)

	joined := strings.Join(suggestions, "\n")
	for _, want := range []string{
		"transport profile",
		"HelloChrome_Auto",
		"ALPN",
		"codex-header-defaults.user-agent",
		"OpenAI-Beta",
		"applyCodexHeaders",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("suggestions missing %q:\n%s", want, joined)
		}
	}
}
