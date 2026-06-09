package helps

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	tls "github.com/refraction-networking/utls"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/proxy"
)

type utlsTransport string

const (
	utlsTransportHTTPS     utlsTransport = "https"
	utlsTransportWebsocket utlsTransport = "websocket"

	utlsProfileChromeAuto    = "chrome-auto"
	utlsProfileCodexMacHTTPS = "codex-rustls-macos-arm64-0.137-https"
	utlsProfileCodexMacWS    = "codex-rustls-macos-arm64-0.137-ws"
)

const codexMacOSArm640137HTTPSRawClientHelloHex = "16030100fb010000f70303557eb9863819a9b868c9590334d64d45d2aa64f866fa9f53455d7a9df77edaab2043514213b407874cab3212fac0d4c64073d5f42bc821259abfd6cee8ebe7f5640014130213011303c02cc02bcca9c030c02fcca800ff0100009a0023000000000010000e00000b636861746770742e636f6d002d00020101000a00080006001d00170018002b00050403040303000b0002010000170000003300260024001d0020de66028dd5801e82f03bd772da00337fd1c91ea10e281c11536855928eb5d3160010000e000c02683208687474702f312e31000d00140012050304030807080608050804060105010401000500050100000000"

const codexMacOSArm640137WebsocketRawClientHelloHex = "16030100e9010000e50303f1b6aa594f0fc564eeab398074dae6a15cdc8906bf6806a038605a625cad53cf2035045f371e57cda55eaf531fcd663e33f378141e54e5308c1241b2ea092979750014130213011303c02cc02bcca9c030c02fcca800ff01000088000b0002010000000010000e00000b636861746770742e636f6d003300260024001d0020a1fb5febbc7dd9ad13ef48039474e2500abaee0e9455b77cf43e3fc15836d37a002b00050403040303000d00140012050304030807080608050804060105010401000a00080006001d0017001800230000002d0002010100050005010000000000170000"

type utlsProfile struct {
	Name              string
	HelloID           tls.ClientHelloID
	RawClientHelloHex string
}

type customUTLSProfileFile struct {
	Name     string                `json:"name"`
	SpecJSON customUTLSProfileSpec `json:"spec_json"`
}

type customUTLSProfileSpec struct {
	RawClientHelloHex string `json:"raw_client_hello_hex"`
}

// NewUtlsWebsocketDialTLSContext returns a TLS-completing websocket dial hook
// for Codex auths. The returned function owns proxy tunneling before applying
// the selected uTLS profile, so callers should not also configure Gorilla's
// proxy wrapper.
func NewUtlsWebsocketDialTLSContext(cfg *config.Config, auth *cliproxyauth.Auth) (func(context.Context, string, string) (net.Conn, error), bool) {
	if !codexAuthProfileEligible(auth) {
		return nil, false
	}
	profile, errProfile := selectUtlsProfile(cfg, auth, utlsTransportWebsocket)
	if errProfile != nil {
		log.WithError(errProfile).Warn("utls websocket: falling back to standard TLS")
		return nil, false
	}

	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	dialer, errDialer := utlsProxyDialer(proxyURL)
	if errDialer != nil {
		log.WithError(errDialer).Warnf("utls websocket: falling back to standard TLS for proxy %q", proxyutil.Redact(proxyURL))
		return nil, false
	}

	return func(ctx context.Context, network string, addr string) (net.Conn, error) {
		if ctx == nil {
			ctx = context.Background()
		}
		conn, err := dialWithContext(ctx, dialer, network, addr)
		if err != nil {
			return nil, err
		}

		tlsConn, err := newProfiledUTLSConn(conn, serverNameFromAddr(addr), profile)
		if err != nil {
			if errClose := conn.Close(); errClose != nil {
				return nil, fmt.Errorf("%w; close failed: %v", err, errClose)
			}
			return nil, err
		}
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			if errClose := conn.Close(); errClose != nil {
				return nil, fmt.Errorf("uTLS websocket handshake failed: %w; close failed: %v", err, errClose)
			}
			return nil, fmt.Errorf("uTLS websocket handshake failed: %w", err)
		}
		return tlsConn, nil
	}, true
}

func utlsProxyDialer(proxyURL string) (proxy.Dialer, error) {
	var dialer proxy.Dialer = proxy.Direct
	if strings.TrimSpace(proxyURL) == "" {
		return dialer, nil
	}
	proxyDialer, mode, errBuild := proxyutil.BuildDialer(proxyURL)
	if errBuild != nil {
		return nil, fmt.Errorf("configure proxy dialer: %w", errBuild)
	}
	if mode != proxyutil.ModeInherit && proxyDialer != nil {
		dialer = proxyDialer
	}
	return dialer, nil
}

func dialWithContext(ctx context.Context, dialer proxy.Dialer, network string, addr string) (net.Conn, error) {
	if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
		return contextDialer.DialContext(ctx, network, addr)
	}
	type dialResult struct {
		conn net.Conn
		err  error
	}
	resultCh := make(chan dialResult, 1)
	go func() {
		conn, err := dialer.Dial(network, addr)
		resultCh <- dialResult{conn: conn, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case result := <-resultCh:
		if result.err != nil {
			return nil, result.err
		}
		select {
		case <-ctx.Done():
			if result.conn != nil {
				_ = result.conn.Close()
			}
			return nil, ctx.Err()
		default:
			return result.conn, nil
		}
	}
}

func serverNameFromAddr(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	return strings.Trim(host, "[]")
}

func selectUtlsProfile(cfg *config.Config, auth *cliproxyauth.Auth, transport utlsTransport) (utlsProfile, error) {
	name := ""
	if cfg != nil {
		name = codexTransportTLSProfileName(cfg, transport)
	}
	if name == "" || strings.EqualFold(name, "auto") {
		if codexAuthProfileEligible(auth) {
			switch transport {
			case utlsTransportWebsocket:
				name = utlsProfileCodexMacWS
			default:
				name = utlsProfileCodexMacHTTPS
			}
		} else {
			name = utlsProfileChromeAuto
		}
	}
	return utlsProfileByName(name)
}

func codexTransportTLSProfileName(cfg *config.Config, transport utlsTransport) string {
	if cfg == nil {
		return ""
	}
	switch transport {
	case utlsTransportWebsocket:
		return strings.TrimSpace(cfg.Codex.TLSProfile.Websocket)
	default:
		return strings.TrimSpace(cfg.Codex.TLSProfile.HTTPS)
	}
}

func codexAuthProfileEligible(auth *cliproxyauth.Auth) bool {
	if auth == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(auth.Provider), "codex")
}

func utlsProfileByName(name string) (utlsProfile, error) {
	trimmed := strings.TrimSpace(name)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "file:") {
		return utlsProfileFromFile(strings.TrimSpace(trimmed[len("file:"):]))
	}
	switch lower {
	case "", "auto", utlsProfileChromeAuto, "hello-chrome-auto":
		return utlsProfile{Name: utlsProfileChromeAuto, HelloID: tls.HelloChrome_Auto}, nil
	case utlsProfileCodexMacHTTPS:
		return utlsProfile{Name: utlsProfileCodexMacHTTPS, RawClientHelloHex: codexMacOSArm640137HTTPSRawClientHelloHex}, nil
	case utlsProfileCodexMacWS:
		return utlsProfile{Name: utlsProfileCodexMacWS, RawClientHelloHex: codexMacOSArm640137WebsocketRawClientHelloHex}, nil
	default:
		return utlsProfile{}, fmt.Errorf("unknown uTLS profile %q", name)
	}
}

func utlsProfileFromFile(path string) (utlsProfile, error) {
	if strings.TrimSpace(path) == "" {
		return utlsProfile{}, fmt.Errorf("uTLS profile file path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return utlsProfile{}, fmt.Errorf("read uTLS profile file: %w", err)
	}
	var fileProfile customUTLSProfileFile
	if err := json.Unmarshal(data, &fileProfile); err != nil {
		return utlsProfile{}, fmt.Errorf("parse uTLS profile file: %w", err)
	}
	rawHex := strings.TrimSpace(fileProfile.SpecJSON.RawClientHelloHex)
	if rawHex == "" {
		return utlsProfile{}, fmt.Errorf("uTLS profile file has no spec_json.raw_client_hello_hex")
	}
	if _, err := hex.DecodeString(rawHex); err != nil {
		return utlsProfile{}, fmt.Errorf("decode uTLS profile file raw ClientHello: %w", err)
	}
	name := strings.TrimSpace(fileProfile.Name)
	if name == "" {
		name = filepath.Base(path)
	}
	return utlsProfile{Name: name, RawClientHelloHex: rawHex}, nil
}

func newProfiledUTLSConn(conn net.Conn, serverName string, profile utlsProfile) (*tls.UConn, error) {
	tlsConfig := &tls.Config{ServerName: serverName}
	if strings.TrimSpace(profile.RawClientHelloHex) == "" {
		return tls.UClient(conn, tlsConfig, profile.HelloID), nil
	}
	record, err := hex.DecodeString(strings.TrimSpace(profile.RawClientHelloHex))
	if err != nil {
		return nil, fmt.Errorf("decode uTLS profile %s raw ClientHello: %w", profile.Name, err)
	}
	spec, err := (&tls.Fingerprinter{AllowBluntMimicry: true}).FingerprintClientHello(record)
	if err != nil {
		return nil, fmt.Errorf("fingerprint uTLS profile %s raw ClientHello: %w", profile.Name, err)
	}
	tlsConn := tls.UClient(conn, tlsConfig, tls.HelloCustom)
	if err := tlsConn.ApplyPreset(spec); err != nil {
		return nil, fmt.Errorf("apply uTLS profile %s: %w", profile.Name, err)
	}
	return tlsConn, nil
}
