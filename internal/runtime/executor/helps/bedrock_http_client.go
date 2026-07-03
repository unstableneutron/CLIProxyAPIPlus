package helps

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/proxyutil"
	log "github.com/sirupsen/logrus"
)

func NewBedrockHTTPClient(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, timeout time.Duration) *http.Client {
	var proxyURL string
	if auth != nil {
		proxyURL = strings.TrimSpace(auth.ProxyURL)
	}
	if proxyURL == "" && cfg != nil {
		proxyURL = strings.TrimSpace(cfg.ProxyURL)
	}

	var transport http.RoundTripper = bedrockDirectTransport()
	if proxyURL != "" {
		if proxyTransport := bedrockProxyTransport(proxyURL); proxyTransport != nil {
			transport = proxyTransport
		}
	} else if ctx != nil {
		if rt, ok := ctx.Value("cliproxy.roundtripper").(http.RoundTripper); ok && rt != nil {
			transport = rt
		}
	}

	client := &http.Client{Transport: transport}
	if timeout > 0 {
		client.Timeout = timeout
	}
	return client
}

func bedrockDirectTransport() *http.Transport {
	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok || base == nil {
		base = &http.Transport{}
	}
	transport := base.Clone()
	transport.TLSClientConfig = bedrockTLSConfig()
	transport.ForceAttemptHTTP2 = true
	return transport
}

func bedrockProxyTransport(proxyURL string) *http.Transport {
	transport, mode, errBuild := proxyutil.BuildHTTPTransport(proxyURL)
	if errBuild != nil {
		log.Errorf("%v", errBuild)
		return nil
	}
	if transport == nil || mode == proxyutil.ModeInherit {
		return nil
	}
	transport.TLSClientConfig = bedrockTLSConfig()
	transport.ForceAttemptHTTP2 = true
	return transport
}

func bedrockTLSConfig() *tls.Config {
	return &tls.Config{
		NextProtos:         []string{"h2", "http/1.1"},
		CurvePreferences:   []tls.CurveID{tls.X25519, tls.CurveP256, tls.CurveP384, tls.CurveP521},
		MinVersion:         tls.VersionTLS12,
		ClientSessionCache: tls.NewLRUClientSessionCache(32),
	}
}
