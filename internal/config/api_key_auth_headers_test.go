package config

import "testing"

func TestParseConfigBytes_ExtraAuthHeaders(t *testing.T) {
	t.Parallel()

	cfg, err := ParseConfigBytes([]byte(`
api-keys:
  - proxy-secret
extra-api-key-auth-headers:
  - X-Proxy-API-Key
  - X-CLIProxy-Key
`))
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}

	want := []string{"X-Proxy-API-Key", "X-CLIProxy-Key"}
	if len(cfg.ExtraAuthHeaders) != len(want) {
		t.Fatalf("ExtraAuthHeaders len = %d, want %d", len(cfg.ExtraAuthHeaders), len(want))
	}
	for index, wantHeader := range want {
		if got := cfg.ExtraAuthHeaders[index]; got != wantHeader {
			t.Fatalf("ExtraAuthHeaders[%d] = %q, want %q", index, got, wantHeader)
		}
	}
}
