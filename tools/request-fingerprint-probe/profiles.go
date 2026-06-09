package main

import (
	"bufio"
	"context"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tls "github.com/refraction-networking/utls"
)

type profileCandidate struct {
	Name    string
	HelloID tls.ClientHelloID
}

type profileRank struct {
	Name                 string `json:"name"`
	Score                int    `json:"score"`
	JA3NMatch            bool   `json:"ja3n_match"`
	ALPNMatch            bool   `json:"alpn_match"`
	CipherSuitesMatch    bool   `json:"cipher_suites_match"`
	SupportedGroupsMatch bool   `json:"supported_groups_match"`
	ExtensionsMatch      bool   `json:"extensions_match"`
	JA3NHash             string `json:"ja3n_hash,omitempty"`
	ALPN                 string `json:"alpn,omitempty"`
}

type generatedCustomProfile struct {
	Name            string          `json:"name"`
	SourceJA3NHash  string          `json:"source_ja3n_hash"`
	SourceJA3Hash   string          `json:"source_ja3_hash"`
	SourceRawSHA256 string          `json:"source_raw_sha256"`
	SpecJSON        json.RawMessage `json:"spec_json"`
}

type customProfileSpecArtifact struct {
	Kind              string `json:"kind"`
	RawClientHelloHex string `json:"raw_client_hello_hex"`
	AllowBluntMimicry bool   `json:"allow_blunt_mimicry"`
	AlwaysAddPadding  bool   `json:"always_add_padding"`
	RealPSKResumption bool   `json:"real_psk_resumption"`
}

func runGenerateProfile(args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("generate-profile", flag.ContinueOnError)
	fs.SetOutput(stderr)
	referencePath := fs.String("reference", "", "reference capture JSON with tls_record_hex")
	name := fs.String("name", "", "generated profile name")
	outPath := fs.String("out", "", "output profile JSON file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *referencePath == "" || *name == "" || *outPath == "" {
		fmt.Fprintln(stderr, "generate-profile requires --reference, --name, and --out")
		return 2
	}
	capture, err := readCapture(*referencePath)
	if err != nil {
		fmt.Fprintf(stderr, "read reference capture: %v\n", err)
		return 1
	}
	profile, err := generateCustomProfile(&capture, *name)
	if err != nil {
		fmt.Fprintf(stderr, "generate profile: %v\n", err)
		return 1
	}
	if err := writeJSONFile(*outPath, profile); err != nil {
		fmt.Fprintf(stderr, "write profile: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s\n", *outPath)
	return 0
}

func runSweep(args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	fs.SetOutput(stderr)
	referencePath := fs.String("reference", "", "reference capture JSON file")
	outDir := fs.String("out", defaultOutputDir(), "directory for profile captures and ranks")
	host := fs.String("host", defaultHost, "target HTTPS host")
	path := fs.String("path", defaultPath, "target path and query")
	profiles := fs.String("profiles", "", "comma-separated profile names; defaults to built-in deterministic profiles")
	includeRandom := fs.Bool("include-random", false, "include randomized uTLS profiles")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *referencePath == "" {
		fmt.Fprintln(stderr, "sweep requires --reference")
		return 2
	}
	reference, err := readCapture(*referencePath)
	if err != nil {
		fmt.Fprintf(stderr, "read reference capture: %v\n", err)
		return 1
	}
	candidates, err := selectedProfileCandidates(*profiles, *includeRandom)
	if err != nil {
		fmt.Fprintf(stderr, "select profiles: %v\n", err)
		return 2
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "create output directory: %v\n", err)
		return 1
	}
	server, err := newCaptureServer(defaultListen)
	if err != nil {
		fmt.Fprintf(stderr, "start capture server: %v\n", err)
		return 1
	}
	defer server.Close()

	targetURL := "https://" + strings.TrimSpace(*host) + normalizePath(*path)
	captures := make([]Capture, 0, len(candidates))
	for _, candidate := range candidates {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		errRun := runNamedUTLSHandshakeProbe(ctx, targetURL, "http://"+server.Addr(), candidate)
		capture := server.WaitForCapture(ctx)
		cancel()
		if capture == nil {
			capture = &Capture{Mode: "connect", Timestamp: nowUTC(), Error: "no capture received"}
		}
		capture.Name = candidate.Name
		if capture.Error == "" && errRun != nil {
			capture.Error = summarizeProbeError(errRun)
		}
		captures = append(captures, *capture)
		if err := writeJSONFile(filepath.Join(*outDir, sanitizeFilename(candidate.Name)+".json"), capture); err != nil {
			fmt.Fprintf(stderr, "write profile capture: %v\n", err)
			return 1
		}
	}
	ranks := rankProfileCaptures(reference, captures)
	if err := writeJSONFile(filepath.Join(*outDir, "profile-ranks.json"), ranks); err != nil {
		fmt.Fprintf(stderr, "write ranks: %v\n", err)
		return 1
	}
	if err := writeSweepMarkdown(filepath.Join(*outDir, "profile-ranks.md"), reference, ranks); err != nil {
		fmt.Fprintf(stderr, "write ranks markdown: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s\n", filepath.Join(*outDir, "profile-ranks.json"))
	return 0
}

func selectedProfileCandidates(names string, includeRandom bool) ([]profileCandidate, error) {
	if strings.TrimSpace(names) == "" {
		return profileCandidates(includeRandom), nil
	}
	var out []profileCandidate
	for _, raw := range strings.Split(names, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		candidate, ok := profileCandidateByNameOK(name)
		if !ok {
			return nil, fmt.Errorf("unknown profile %q", name)
		}
		out = append(out, candidate)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no profiles selected")
	}
	return out, nil
}

func writeSweepMarkdown(path string, reference Capture, ranks []profileRank) error {
	var b strings.Builder
	b.WriteString("# uTLS Profile Sweep\n\n")
	b.WriteString("Reference: `" + reference.Name + "`")
	if reference.TLS != nil {
		b.WriteString(" JA3N `" + reference.TLS.JA3NHash + "` ALPN `" + strings.Join(reference.TLS.ALPNProtocols, ",") + "`")
	}
	b.WriteString("\n\n")
	b.WriteString("| Rank | Profile | Score | JA3N | ALPN | Ciphers | Groups | Extensions |\n")
	b.WriteString("| --- | --- | ---: | --- | --- | --- | --- | --- |\n")
	for i, rank := range ranks {
		b.WriteString(fmt.Sprintf("| %d | `%s` | %d | %t | %t | %t | %t | %t |\n",
			i+1,
			rank.Name,
			rank.Score,
			rank.JA3NMatch,
			rank.ALPNMatch,
			rank.CipherSuitesMatch,
			rank.SupportedGroupsMatch,
			rank.ExtensionsMatch,
		))
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func profileCandidates(includeRandom bool) []profileCandidate {
	candidates := []profileCandidate{
		{Name: "hello-golang", HelloID: tls.HelloGolang},
		{Name: "hello-chrome-auto", HelloID: tls.HelloChrome_Auto},
		{Name: "hello-chrome-58", HelloID: tls.HelloChrome_58},
		{Name: "hello-chrome-62", HelloID: tls.HelloChrome_62},
		{Name: "hello-chrome-70", HelloID: tls.HelloChrome_70},
		{Name: "hello-chrome-72", HelloID: tls.HelloChrome_72},
		{Name: "hello-chrome-83", HelloID: tls.HelloChrome_83},
		{Name: "hello-chrome-87", HelloID: tls.HelloChrome_87},
		{Name: "hello-chrome-96", HelloID: tls.HelloChrome_96},
		{Name: "hello-chrome-100", HelloID: tls.HelloChrome_100},
		{Name: "hello-chrome-102", HelloID: tls.HelloChrome_102},
		{Name: "hello-chrome-106-shuffle", HelloID: tls.HelloChrome_106_Shuffle},
		{Name: "hello-chrome-115-pq", HelloID: tls.HelloChrome_115_PQ},
		{Name: "hello-chrome-120", HelloID: tls.HelloChrome_120},
		{Name: "hello-chrome-120-pq", HelloID: tls.HelloChrome_120_PQ},
		{Name: "hello-chrome-131", HelloID: tls.HelloChrome_131},
		{Name: "hello-chrome-133", HelloID: tls.HelloChrome_133},
		{Name: "hello-firefox-auto", HelloID: tls.HelloFirefox_Auto},
		{Name: "hello-firefox-55", HelloID: tls.HelloFirefox_55},
		{Name: "hello-firefox-56", HelloID: tls.HelloFirefox_56},
		{Name: "hello-firefox-63", HelloID: tls.HelloFirefox_63},
		{Name: "hello-firefox-65", HelloID: tls.HelloFirefox_65},
		{Name: "hello-firefox-99", HelloID: tls.HelloFirefox_99},
		{Name: "hello-firefox-102", HelloID: tls.HelloFirefox_102},
		{Name: "hello-firefox-105", HelloID: tls.HelloFirefox_105},
		{Name: "hello-firefox-120", HelloID: tls.HelloFirefox_120},
		{Name: "hello-safari-auto", HelloID: tls.HelloSafari_Auto},
		{Name: "hello-safari-16-0", HelloID: tls.HelloSafari_16_0},
		{Name: "hello-ios-auto", HelloID: tls.HelloIOS_Auto},
		{Name: "hello-ios-11-1", HelloID: tls.HelloIOS_11_1},
		{Name: "hello-ios-12-1", HelloID: tls.HelloIOS_12_1},
		{Name: "hello-ios-13", HelloID: tls.HelloIOS_13},
		{Name: "hello-ios-14", HelloID: tls.HelloIOS_14},
		{Name: "hello-android-11-okhttp", HelloID: tls.HelloAndroid_11_OkHttp},
		{Name: "hello-edge-85", HelloID: tls.HelloEdge_85},
		{Name: "hello-edge-106", HelloID: tls.HelloEdge_106},
	}
	if includeRandom {
		candidates = append(candidates,
			profileCandidate{Name: "hello-randomized", HelloID: tls.HelloRandomized},
			profileCandidate{Name: "hello-randomized-alpn", HelloID: tls.HelloRandomizedALPN},
			profileCandidate{Name: "hello-randomized-no-alpn", HelloID: tls.HelloRandomizedNoALPN},
		)
	}
	return candidates
}

func profileCandidateByName(name string) profileCandidate {
	candidate, ok := profileCandidateByNameOK(name)
	if ok {
		return candidate
	}
	return profileCandidate{Name: name, HelloID: tls.HelloChrome_Auto}
}

func profileCandidateByNameOK(name string) (profileCandidate, bool) {
	for _, candidate := range profileCandidates(true) {
		if candidate.Name == name {
			return candidate, true
		}
	}
	return profileCandidate{}, false
}

func runNamedUTLSHandshakeProbe(ctx context.Context, targetURL string, proxyRawURL string, candidate profileCandidate) error {
	parsedTarget, err := url.Parse(targetURL)
	if err != nil {
		return err
	}
	if parsedTarget.Scheme != "https" {
		return fmt.Errorf("profile probe target must be https, got %q", parsedTarget.Scheme)
	}
	host := parsedTarget.Hostname()
	port := parsedTarget.Port()
	if port == "" {
		port = "443"
	}
	targetAddr := net.JoinHostPort(host, port)

	proxyURL, err := url.Parse(proxyRawURL)
	if err != nil {
		return err
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxyURL.Host)
	if err != nil {
		return err
	}
	defer conn.Close()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", targetAddr, targetAddr); err != nil {
		return err
	}
	br := bufio.NewReader(conn)
	statusLine, err := readCRLFLine(br)
	if err != nil {
		return err
	}
	if _, err := readHeaderLines(br); err != nil {
		return err
	}
	fields := strings.Fields(statusLine)
	if len(fields) < 2 {
		return fmt.Errorf("malformed CONNECT response status %q", statusLine)
	}
	if fields[1] < "200" || fields[1] >= "300" {
		return fmt.Errorf("CONNECT returned status %s", statusLine)
	}

	tlsConn := tls.UClient(bufferedConn{Conn: conn, reader: br}, &tls.Config{ServerName: host}, candidate.HelloID)
	if err := tlsConn.Handshake(); err != nil {
		return err
	}
	return nil
}

func rankProfileCaptures(reference Capture, captures []Capture) []profileRank {
	ranks := make([]profileRank, 0, len(captures))
	for _, capture := range captures {
		rank := profileRank{Name: capture.Name}
		if capture.TLS != nil {
			rank.JA3NHash = capture.TLS.JA3NHash
			rank.ALPN = strings.Join(capture.TLS.ALPNProtocols, ",")
		}
		rank.JA3NMatch = tlsField(reference.TLS, func(t *TLSFingerprint) string { return t.JA3NHash }) == tlsField(capture.TLS, func(t *TLSFingerprint) string { return t.JA3NHash })
		rank.ALPNMatch = strings.Join(tlsStringSlice(reference.TLS, func(t *TLSFingerprint) []string { return t.ALPNProtocols }), ",") == strings.Join(tlsStringSlice(capture.TLS, func(t *TLSFingerprint) []string { return t.ALPNProtocols }), ",")
		rank.CipherSuitesMatch = joinUint16(tlsUint16Slice(reference.TLS, func(t *TLSFingerprint) []uint16 { return t.CipherSuites })) == joinUint16(tlsUint16Slice(capture.TLS, func(t *TLSFingerprint) []uint16 { return t.CipherSuites }))
		rank.SupportedGroupsMatch = joinUint16(tlsUint16Slice(reference.TLS, func(t *TLSFingerprint) []uint16 { return t.SupportedGroups })) == joinUint16(tlsUint16Slice(capture.TLS, func(t *TLSFingerprint) []uint16 { return t.SupportedGroups }))
		rank.ExtensionsMatch = joinUint16(tlsUint16Slice(reference.TLS, func(t *TLSFingerprint) []uint16 { return t.Extensions })) == joinUint16(tlsUint16Slice(capture.TLS, func(t *TLSFingerprint) []uint16 { return t.Extensions }))
		if rank.JA3NMatch {
			rank.Score += 100
		}
		if rank.ALPNMatch {
			rank.Score += 25
		}
		if rank.CipherSuitesMatch {
			rank.Score += 20
		}
		if rank.SupportedGroupsMatch {
			rank.Score += 15
		}
		if rank.ExtensionsMatch {
			rank.Score += 10
		}
		ranks = append(ranks, rank)
	}
	sort.SliceStable(ranks, func(i, j int) bool {
		if ranks[i].Score != ranks[j].Score {
			return ranks[i].Score > ranks[j].Score
		}
		return ranks[i].Name < ranks[j].Name
	})
	return ranks
}

func tlsField(fp *TLSFingerprint, fn func(*TLSFingerprint) string) string {
	if fp == nil {
		return ""
	}
	return fn(fp)
}

func generateCustomProfile(capture *Capture, name string) (*generatedCustomProfile, error) {
	if capture == nil {
		return nil, fmt.Errorf("capture is nil")
	}
	if strings.TrimSpace(capture.TLSRecordHex) == "" {
		return nil, fmt.Errorf("capture has no tls_record_hex")
	}
	record, err := hex.DecodeString(strings.TrimSpace(capture.TLSRecordHex))
	if err != nil {
		return nil, fmt.Errorf("decode tls_record_hex: %w", err)
	}
	fingerprinter := &tls.Fingerprinter{AllowBluntMimicry: true}
	if _, err := fingerprinter.FingerprintClientHello(record); err != nil {
		return nil, fmt.Errorf("fingerprint ClientHello: %w", err)
	}
	specJSON, err := json.MarshalIndent(customProfileSpecArtifact{
		Kind:              "utls-fingerprinter-raw-client-hello",
		RawClientHelloHex: strings.TrimSpace(capture.TLSRecordHex),
		AllowBluntMimicry: true,
	}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal ClientHelloSpec: %w", err)
	}
	out := &generatedCustomProfile{Name: name, SpecJSON: specJSON}
	if capture.TLS != nil {
		out.SourceJA3NHash = capture.TLS.JA3NHash
		out.SourceJA3Hash = capture.TLS.JA3Hash
		out.SourceRawSHA256 = capture.TLS.RawSHA256
	}
	return out, nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c bufferedConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}
