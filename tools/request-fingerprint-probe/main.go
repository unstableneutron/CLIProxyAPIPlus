package main

import (
	"bufio"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const (
	redactedValue    = "<redacted>"
	defaultListen    = "127.0.0.1:0"
	defaultHost      = "chatgpt.com"
	defaultPath      = "/backend-api/codex/models?client_version=fingerprint-probe"
	defaultUserAgent = "request-fingerprint-probe/0"
)

type HeaderLine struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type HTTPFingerprint struct {
	Method     string       `json:"method"`
	Target     string       `json:"target"`
	Proto      string       `json:"proto"`
	Host       string       `json:"host,omitempty"`
	Headers    []HeaderLine `json:"headers,omitempty"`
	BodyBytes  int64        `json:"body_bytes"`
	BodySHA256 string       `json:"body_sha256,omitempty"`
}

type TLSFingerprint struct {
	RecordVersion       uint16   `json:"record_version"`
	ClientVersion       uint16   `json:"client_version"`
	ServerName          string   `json:"server_name,omitempty"`
	CipherSuites        []uint16 `json:"cipher_suites,omitempty"`
	Extensions          []uint16 `json:"extensions,omitempty"`
	SupportedGroups     []uint16 `json:"supported_groups,omitempty"`
	ECPointFormats      []uint16 `json:"ec_point_formats,omitempty"`
	ALPNProtocols       []string `json:"alpn_protocols,omitempty"`
	SignatureAlgorithms []uint16 `json:"signature_algorithms,omitempty"`
	JA3                 string   `json:"ja3"`
	JA3Hash             string   `json:"ja3_hash"`
	JA3N                string   `json:"ja3n"`
	JA3NHash            string   `json:"ja3n_hash"`
	RawSHA256           string   `json:"raw_sha256"`
}

type Capture struct {
	Name          string           `json:"name,omitempty"`
	Mode          string           `json:"mode"`
	Timestamp     string           `json:"timestamp"`
	RequestLine   string           `json:"request_line,omitempty"`
	ConnectTarget string           `json:"connect_target,omitempty"`
	Headers       []HeaderLine     `json:"headers,omitempty"`
	TLSRecordHex  string           `json:"tls_record_hex,omitempty"`
	HTTP          *HTTPFingerprint `json:"http,omitempty"`
	TLS           *TLSFingerprint  `json:"tls,omitempty"`
	Error         string           `json:"error,omitempty"`
}

type RunReport struct {
	GeneratedAt string    `json:"generated_at"`
	TargetURL   string    `json:"target_url"`
	Captures    []Capture `json:"captures"`
	Diffs       []string  `json:"diffs,omitempty"`
	Suggestions []string  `json:"suggestions,omitempty"`
}

type captureServer struct {
	listener net.Listener
	captures chan Capture
	done     chan struct{}
	wg       sync.WaitGroup
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout io.Writer, stderr io.Writer) int {
	if len(args) == 0 {
		printUsage(stderr)
		return 2
	}
	switch args[0] {
	case "probe":
		return runProbe(args[1:], stdout, stderr)
	case "serve":
		return runServe(args[1:], stdout, stderr)
	case "compare":
		return runCompare(args[1:], stdout, stderr)
	case "suggest":
		return runSuggest(args[1:], stdout, stderr)
	case "sweep":
		return runSweep(args[1:], stdout, stderr)
	case "generate-profile":
		return runGenerateProfile(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		printUsage(stderr)
		return 2
	}
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `Usage:
  request-fingerprint-probe probe [--out DIR] [--host HOST] [--path PATH]
  request-fingerprint-probe serve [--out DIR] [--listen ADDR]
  request-fingerprint-probe compare --left FILE --right FILE
  request-fingerprint-probe suggest --reference FILE --candidate FILE
  request-fingerprint-probe sweep --reference FILE [--out DIR] [--host HOST] [--path PATH]
  request-fingerprint-probe generate-profile --reference FILE --name NAME --out FILE

The probe command captures local Go stdlib and CLIProxy uTLS ClientHello
fingerprints through a temporary CONNECT proxy. Generated captures are redacted
and should be written under scratch/request-fingerprint-probe/.`)
}

func runProbe(args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(stderr)
	outDir := fs.String("out", defaultOutputDir(), "directory for summary.json and compare.md")
	host := fs.String("host", defaultHost, "target HTTPS host")
	path := fs.String("path", defaultPath, "target path and query")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	targetURL := "https://" + strings.TrimSpace(*host) + normalizePath(*path)
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

	var captures []Capture
	for _, probe := range []struct {
		name string
		run  func(context.Context, string, string) error
	}{
		{name: "go-stdlib", run: runStdlibHandshakeProbe},
		{name: "cliproxy-utls", run: runCLIProxyUTLSHandshakeProbe},
		{name: "cliproxy-utls-websocket", run: runCLIProxyUTLSWebsocketHandshakeProbe},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		errRun := probe.run(ctx, targetURL, "http://"+server.Addr())
		capture := server.WaitForCapture(ctx)
		cancel()
		if capture == nil {
			capture = &Capture{Mode: "connect", Timestamp: time.Now().UTC().Format(time.RFC3339), Error: "no capture received"}
		}
		capture.Name = probe.name
		if capture.Error == "" && errRun != nil {
			capture.Error = summarizeProbeError(errRun)
		}
		captures = append(captures, *capture)
	}

	report := RunReport{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		TargetURL:   targetURL,
		Captures:    captures,
	}
	if len(captures) >= 2 {
		report.Diffs = compareCaptures(captures[0], captures[1])
		report.Suggestions = suggestCaptures(captures[0], captures[1])
	}
	if err := writeRunArtifacts(*outDir, report); err != nil {
		fmt.Fprintf(stderr, "write artifacts: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s\n", filepath.Join(*outDir, "summary.json"))
	fmt.Fprintf(stdout, "wrote %s\n", filepath.Join(*outDir, "compare.md"))
	return 0
}

func runServe(args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	outDir := fs.String("out", defaultOutputDir(), "directory for capture JSON files")
	listen := fs.String("listen", defaultListen, "listen address")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "create output directory: %v\n", err)
		return 1
	}
	server, err := newCaptureServer(*listen)
	if err != nil {
		fmt.Fprintf(stderr, "start capture server: %v\n", err)
		return 1
	}
	defer server.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(stdout, "listening on %s\n", server.Addr())
	fmt.Fprintf(stdout, "HTTPS proxy: HTTPS_PROXY=http://%s\n", server.Addr())
	fmt.Fprintf(stdout, "HTTP sink base URL: http://%s\n", server.Addr())
	fmt.Fprintf(stdout, "writing captures to %s\n", *outDir)

	for {
		select {
		case <-ctx.Done():
			return 0
		case capture := <-server.captures:
			name := capture.Name
			if name == "" {
				name = capture.Mode
			}
			file := filepath.Join(*outDir, fmt.Sprintf("%s-%d.json", sanitizeFilename(name), time.Now().UnixNano()))
			if err := writeJSONFile(file, capture); err != nil {
				fmt.Fprintf(stderr, "write capture: %v\n", err)
				continue
			}
			fmt.Fprintf(stdout, "capture %s -> %s\n", capture.Mode, file)
		}
	}
}

func runCompare(args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	leftPath := fs.String("left", "", "left capture JSON file")
	rightPath := fs.String("right", "", "right capture JSON file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *leftPath == "" || *rightPath == "" {
		fmt.Fprintln(stderr, "compare requires --left and --right")
		return 2
	}
	left, err := readCapture(*leftPath)
	if err != nil {
		fmt.Fprintf(stderr, "read left capture: %v\n", err)
		return 1
	}
	right, err := readCapture(*rightPath)
	if err != nil {
		fmt.Fprintf(stderr, "read right capture: %v\n", err)
		return 1
	}
	for _, diff := range compareCaptures(left, right) {
		fmt.Fprintln(stdout, diff)
	}
	return 0
}

func runSuggest(args []string, stdout io.Writer, stderr io.Writer) int {
	fs := flag.NewFlagSet("suggest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	referencePath := fs.String("reference", "", "reference capture JSON file, usually bundled Codex CLI/app-server")
	candidatePath := fs.String("candidate", "", "candidate capture JSON file, usually CLIProxy capture")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *referencePath == "" || *candidatePath == "" {
		fmt.Fprintln(stderr, "suggest requires --reference and --candidate")
		return 2
	}
	reference, err := readCapture(*referencePath)
	if err != nil {
		fmt.Fprintf(stderr, "read reference capture: %v\n", err)
		return 1
	}
	candidate, err := readCapture(*candidatePath)
	if err != nil {
		fmt.Fprintf(stderr, "read candidate capture: %v\n", err)
		return 1
	}
	for _, suggestion := range suggestCaptures(reference, candidate) {
		fmt.Fprintln(stdout, suggestion)
	}
	return 0
}

func newCaptureServer(listen string) (*captureServer, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	server := &captureServer{
		listener: ln,
		captures: make(chan Capture, 128),
		done:     make(chan struct{}),
	}
	server.wg.Add(1)
	go server.acceptLoop()
	return server, nil
}

func (s *captureServer) Addr() string {
	if s == nil || s.listener == nil {
		return ""
	}
	return s.listener.Addr().String()
}

func (s *captureServer) Close() {
	if s == nil {
		return
	}
	_ = s.listener.Close()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	s.wg.Wait()
}

func (s *captureServer) WaitForCapture(ctx context.Context) *Capture {
	if s == nil {
		return nil
	}
	select {
	case capture := <-s.captures:
		return &capture
	case <-ctx.Done():
		return nil
	}
}

func (s *captureServer) acceptLoop() {
	defer s.wg.Done()
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *captureServer) handleConn(conn net.Conn) {
	defer conn.Close()
	br := bufio.NewReader(conn)
	requestLine, err := readCRLFLine(br)
	if err != nil {
		return
	}
	headers, err := readHeaderLines(br)
	if err != nil {
		s.emit(Capture{Mode: "http", Timestamp: nowUTC(), RequestLine: requestLine, Error: err.Error()})
		return
	}
	parts := strings.Fields(requestLine)
	if len(parts) < 3 {
		s.emit(Capture{Mode: "http", Timestamp: nowUTC(), RequestLine: requestLine, Headers: redactHeaderLines(headers), Error: "malformed request line"})
		return
	}
	method, target, proto := parts[0], parts[1], parts[2]
	if strings.EqualFold(method, http.MethodConnect) {
		_, _ = io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n")
		record, errRecord := readTLSRecord(br)
		capture := Capture{
			Mode:          "connect",
			Timestamp:     nowUTC(),
			RequestLine:   requestLine,
			ConnectTarget: target,
			Headers:       redactHeaderLines(headers),
		}
		if errRecord != nil {
			capture.Error = errRecord.Error()
		} else if fp, errParse := parseClientHelloRecord(record); errParse != nil {
			capture.Error = errParse.Error()
		} else {
			capture.TLSRecordHex = hex.EncodeToString(record)
			capture.TLS = fp
		}
		s.emit(capture)
		return
	}

	body, bodyErr := readRequestBody(br, headers)
	capture := Capture{
		Mode:        "http",
		Timestamp:   nowUTC(),
		RequestLine: requestLine,
		Headers:     redactHeaderLines(headers),
		HTTP: &HTTPFingerprint{
			Method:     method,
			Target:     target,
			Proto:      proto,
			Host:       headerValue(headers, "host"),
			Headers:    redactHeaderLines(headers),
			BodyBytes:  int64(len(body)),
			BodySHA256: sha256Hex(body),
		},
	}
	if bodyErr != nil {
		capture.Error = bodyErr.Error()
	}
	s.emit(capture)
	writeSinkResponse(conn, target)
}

func (s *captureServer) emit(capture Capture) {
	select {
	case s.captures <- capture:
	default:
	}
}

func readCRLFLine(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func readHeaderLines(r *bufio.Reader) ([]HeaderLine, error) {
	var lines []HeaderLine
	for {
		line, err := readCRLFLine(r)
		if err != nil {
			return nil, err
		}
		if line == "" {
			return lines, nil
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("malformed header line %q", line)
		}
		lines = append(lines, HeaderLine{Name: strings.TrimSpace(name), Value: strings.TrimSpace(value)})
	}
}

func readRequestBody(r *bufio.Reader, headers []HeaderLine) ([]byte, error) {
	contentLength := strings.TrimSpace(headerValue(headers, "content-length"))
	if contentLength == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(contentLength, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("parse content-length: %w", err)
	}
	if n <= 0 {
		return nil, nil
	}
	body := make([]byte, n)
	_, err = io.ReadFull(r, body)
	return body, err
}

func readTLSRecord(r io.Reader) ([]byte, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	if header[0] != 22 {
		return nil, fmt.Errorf("unexpected TLS record content type %d", header[0])
	}
	length := int(binary.BigEndian.Uint16(header[3:5]))
	if length <= 0 {
		return nil, fmt.Errorf("empty TLS record")
	}
	record := make([]byte, 5+length)
	copy(record, header)
	if _, err := io.ReadFull(r, record[5:]); err != nil {
		return nil, err
	}
	return record, nil
}

func parseClientHelloRecord(record []byte) (*TLSFingerprint, error) {
	if len(record) < 5 {
		return nil, errors.New("TLS record too short")
	}
	if record[0] != 22 {
		return nil, fmt.Errorf("not a handshake record: %d", record[0])
	}
	recordLen := int(binary.BigEndian.Uint16(record[3:5]))
	if len(record) < 5+recordLen {
		return nil, errors.New("incomplete TLS record")
	}
	payload := record[5 : 5+recordLen]
	if len(payload) < 4 || payload[0] != 1 {
		return nil, errors.New("not a ClientHello handshake")
	}
	helloLen := int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
	if len(payload) < 4+helloLen {
		return nil, errors.New("incomplete ClientHello")
	}
	p := payload[4 : 4+helloLen]
	fp := &TLSFingerprint{
		RecordVersion: binary.BigEndian.Uint16(record[1:3]),
		RawSHA256:     sha256Hex(record[:5+recordLen]),
	}
	if len(p) < 35 {
		return nil, errors.New("ClientHello missing fixed fields")
	}
	fp.ClientVersion = binary.BigEndian.Uint16(p[0:2])
	p = p[34:]
	if len(p) < 1 {
		return nil, errors.New("ClientHello missing session id")
	}
	sessionLen := int(p[0])
	p = p[1:]
	if len(p) < sessionLen+2 {
		return nil, errors.New("ClientHello truncated before cipher suites")
	}
	p = p[sessionLen:]
	cipherLen := int(binary.BigEndian.Uint16(p[0:2]))
	p = p[2:]
	if cipherLen%2 != 0 || len(p) < cipherLen+1 {
		return nil, errors.New("ClientHello has invalid cipher suite list")
	}
	fp.CipherSuites = parseUint16List(p[:cipherLen], true)
	p = p[cipherLen:]
	compressionLen := int(p[0])
	p = p[1:]
	if len(p) < compressionLen {
		return nil, errors.New("ClientHello has invalid compression method list")
	}
	p = p[compressionLen:]
	if len(p) == 0 {
		fp.setJA3()
		return fp, nil
	}
	if len(p) < 2 {
		return nil, errors.New("ClientHello has truncated extensions length")
	}
	extensionsLen := int(binary.BigEndian.Uint16(p[0:2]))
	p = p[2:]
	if len(p) < extensionsLen {
		return nil, errors.New("ClientHello has truncated extensions")
	}
	extensions := p[:extensionsLen]
	for len(extensions) > 0 {
		if len(extensions) < 4 {
			return nil, errors.New("ClientHello has malformed extension header")
		}
		extType := binary.BigEndian.Uint16(extensions[0:2])
		extLen := int(binary.BigEndian.Uint16(extensions[2:4]))
		extensions = extensions[4:]
		if len(extensions) < extLen {
			return nil, errors.New("ClientHello has malformed extension body")
		}
		data := extensions[:extLen]
		extensions = extensions[extLen:]
		if !isGREASE(extType) {
			fp.Extensions = append(fp.Extensions, extType)
		}
		fp.applyExtension(extType, data)
	}
	fp.setJA3()
	return fp, nil
}

func (fp *TLSFingerprint) applyExtension(extType uint16, data []byte) {
	switch extType {
	case 0:
		fp.ServerName = parseSNI(data)
	case 10:
		if len(data) >= 2 {
			listLen := int(binary.BigEndian.Uint16(data[0:2]))
			if len(data[2:]) >= listLen {
				fp.SupportedGroups = parseUint16List(data[2:2+listLen], true)
			}
		}
	case 11:
		if len(data) >= 1 {
			listLen := int(data[0])
			if len(data[1:]) >= listLen {
				fp.ECPointFormats = parseUint8List(data[1 : 1+listLen])
			}
		}
	case 13:
		if len(data) >= 2 {
			listLen := int(binary.BigEndian.Uint16(data[0:2]))
			if len(data[2:]) >= listLen {
				fp.SignatureAlgorithms = parseUint16List(data[2:2+listLen], false)
			}
		}
	case 16:
		fp.ALPNProtocols = parseALPN(data)
	}
}

func (fp *TLSFingerprint) setJA3() {
	fp.JA3 = strings.Join([]string{
		strconv.Itoa(int(fp.ClientVersion)),
		joinUint16(fp.CipherSuites),
		joinUint16(fp.Extensions),
		joinUint16(fp.SupportedGroups),
		joinUint16(fp.ECPointFormats),
	}, ",")
	sum := md5.Sum([]byte(fp.JA3))
	fp.JA3Hash = hex.EncodeToString(sum[:])

	normalizedExtensions := append([]uint16(nil), fp.Extensions...)
	sort.Slice(normalizedExtensions, func(i, j int) bool {
		return normalizedExtensions[i] < normalizedExtensions[j]
	})
	fp.JA3N = strings.Join([]string{
		strconv.Itoa(int(fp.ClientVersion)),
		joinUint16(fp.CipherSuites),
		joinUint16(normalizedExtensions),
		joinUint16(fp.SupportedGroups),
		joinUint16(fp.ECPointFormats),
	}, ",")
	normalizedSum := md5.Sum([]byte(fp.JA3N))
	fp.JA3NHash = hex.EncodeToString(normalizedSum[:])
}

func parseSNI(data []byte) string {
	if len(data) < 2 {
		return ""
	}
	listLen := int(binary.BigEndian.Uint16(data[0:2]))
	data = data[2:]
	if len(data) < listLen {
		return ""
	}
	data = data[:listLen]
	for len(data) >= 3 {
		nameType := data[0]
		nameLen := int(binary.BigEndian.Uint16(data[1:3]))
		data = data[3:]
		if len(data) < nameLen {
			return ""
		}
		if nameType == 0 {
			return string(data[:nameLen])
		}
		data = data[nameLen:]
	}
	return ""
}

func parseALPN(data []byte) []string {
	if len(data) < 2 {
		return nil
	}
	listLen := int(binary.BigEndian.Uint16(data[0:2]))
	data = data[2:]
	if len(data) < listLen {
		return nil
	}
	data = data[:listLen]
	var out []string
	for len(data) > 0 {
		itemLen := int(data[0])
		data = data[1:]
		if len(data) < itemLen {
			return out
		}
		out = append(out, string(data[:itemLen]))
		data = data[itemLen:]
	}
	return out
}

func parseUint16List(data []byte, skipGREASE bool) []uint16 {
	var out []uint16
	for len(data) >= 2 {
		value := binary.BigEndian.Uint16(data[0:2])
		data = data[2:]
		if skipGREASE && isGREASE(value) {
			continue
		}
		out = append(out, value)
	}
	return out
}

func parseUint8List(data []byte) []uint16 {
	out := make([]uint16, 0, len(data))
	for _, value := range data {
		out = append(out, uint16(value))
	}
	return out
}

func isGREASE(value uint16) bool {
	high := byte(value >> 8)
	low := byte(value)
	return high == low && high&0x0f == 0x0a
}

func runStdlibHandshakeProbe(ctx context.Context, targetURL string, proxyRawURL string) error {
	proxyURL, err := url.Parse(proxyRawURL)
	if err != nil {
		return err
	}
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             http.ProxyURL(proxyURL),
			ForceAttemptHTTP2: true,
		},
	}
	return doProbeRequest(ctx, client, targetURL)
}

func runCLIProxyUTLSHandshakeProbe(ctx context.Context, targetURL string, proxyRawURL string) error {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: proxyRawURL}}
	auth := &cliproxyauth.Auth{Provider: "codex"}
	client := helps.NewUtlsHTTPClient(ctx, cfg, auth, 0)
	return doProbeRequest(ctx, client, targetURL)
}

func runCLIProxyUTLSWebsocketHandshakeProbe(ctx context.Context, targetURL string, proxyRawURL string) error {
	parsedTarget, err := url.Parse(targetURL)
	if err != nil {
		return err
	}
	if parsedTarget.Scheme != "https" {
		return fmt.Errorf("websocket profile probe target must be https, got %q", parsedTarget.Scheme)
	}
	host := parsedTarget.Hostname()
	port := parsedTarget.Port()
	if port == "" {
		port = "443"
	}
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: proxyRawURL}}
	auth := &cliproxyauth.Auth{Provider: "codex"}
	dialTLS, ok := helps.NewUtlsWebsocketDialTLSContext(cfg, auth)
	if !ok {
		return fmt.Errorf("websocket uTLS dialer unavailable")
	}
	conn, err := dialTLS(ctx, "tcp", net.JoinHostPort(host, port))
	if conn != nil {
		_ = conn.Close()
	}
	return err
}

func doProbeRequest(ctx context.Context, client *http.Client, targetURL string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", defaultUserAgent)
	req.Header.Set("Authorization", "Bearer fingerprint-probe")
	req.Header.Set("ChatGPT-Account-ID", "fingerprint-probe")
	resp, err := client.Do(req)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	return err
}

func writeSinkResponse(conn net.Conn, target string) {
	body := `{"ok":true}`
	contentType := "application/json"
	if strings.Contains(target, "/models") {
		body = `{"data":[],"models":[]}`
	} else if strings.Contains(target, "/responses") {
		body = `{"id":"fingerprint-probe","object":"response","status":"completed","output":[]}`
	}
	_, _ = fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", contentType, len(body), body)
}

func redactHeaderLines(lines []HeaderLine) []HeaderLine {
	out := make([]HeaderLine, 0, len(lines))
	for _, line := range lines {
		value := line.Value
		if isSensitiveHeader(line.Name) {
			value = redactedValue
		}
		out = append(out, HeaderLine{Name: line.Name, Value: value})
	}
	return out
}

func isSensitiveHeader(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return false
	}
	if lower == "authorization" || lower == "proxy-authorization" || lower == "cookie" || lower == "set-cookie" {
		return true
	}
	if lower == "sec-websocket-key" || lower == "x-codex-turn-metadata" {
		return true
	}
	return strings.Contains(lower, "token") ||
		strings.Contains(lower, "secret") ||
		strings.Contains(lower, "api-key") ||
		strings.Contains(lower, "account-id") ||
		strings.Contains(lower, "management-key") ||
		strings.Contains(lower, "request-id") ||
		strings.Contains(lower, "session-id") ||
		strings.Contains(lower, "thread-id") ||
		strings.Contains(lower, "window-id")
}

func compareCaptures(left Capture, right Capture) []string {
	var diffs []string
	if left.TLS != nil || right.TLS != nil {
		diffs = appendDiff(diffs, "tls.ja3n_hash", tlsValue(left.TLS, func(t *TLSFingerprint) string { return t.JA3NHash }), tlsValue(right.TLS, func(t *TLSFingerprint) string { return t.JA3NHash }))
		diffs = appendDiff(diffs, "tls.ja3_hash", tlsValue(left.TLS, func(t *TLSFingerprint) string { return t.JA3Hash }), tlsValue(right.TLS, func(t *TLSFingerprint) string { return t.JA3Hash }))
		diffs = appendDiff(diffs, "tls.alpn_protocols", strings.Join(tlsStringSlice(left.TLS, func(t *TLSFingerprint) []string { return t.ALPNProtocols }), ","), strings.Join(tlsStringSlice(right.TLS, func(t *TLSFingerprint) []string { return t.ALPNProtocols }), ","))
		diffs = appendDiff(diffs, "tls.extensions", joinUint16(tlsUint16Slice(left.TLS, func(t *TLSFingerprint) []uint16 { return t.Extensions })), joinUint16(tlsUint16Slice(right.TLS, func(t *TLSFingerprint) []uint16 { return t.Extensions })))
		diffs = appendDiff(diffs, "tls.cipher_suites", joinUint16(tlsUint16Slice(left.TLS, func(t *TLSFingerprint) []uint16 { return t.CipherSuites })), joinUint16(tlsUint16Slice(right.TLS, func(t *TLSFingerprint) []uint16 { return t.CipherSuites })))
	}
	if left.HTTP != nil || right.HTTP != nil {
		diffs = appendDiff(diffs, "http.method", httpValue(left.HTTP, func(h *HTTPFingerprint) string { return h.Method }), httpValue(right.HTTP, func(h *HTTPFingerprint) string { return h.Method }))
		diffs = appendDiff(diffs, "http.target", httpValue(left.HTTP, func(h *HTTPFingerprint) string { return h.Target }), httpValue(right.HTTP, func(h *HTTPFingerprint) string { return h.Target }))
		leftHeaders := httpHeaders(left.HTTP)
		rightHeaders := httpHeaders(right.HTTP)
		diffs = appendDiff(diffs, "http.headers.names", headerNameSet(leftHeaders), headerNameSet(rightHeaders))
		diffs = appendDiff(diffs, "http.headers.order", headerNameOrder(leftHeaders), headerNameOrder(rightHeaders))
		for _, name := range headerNameUnion(leftHeaders, rightHeaders) {
			diffs = appendDiff(diffs, "http.header."+name, strings.Join(headerValues(leftHeaders, name), " | "), strings.Join(headerValues(rightHeaders, name), " | "))
		}
		diffs = appendDiff(diffs, "http.body_sha256", httpValue(left.HTTP, func(h *HTTPFingerprint) string { return h.BodySHA256 }), httpValue(right.HTTP, func(h *HTTPFingerprint) string { return h.BodySHA256 }))
	}
	if len(diffs) == 0 {
		return []string{"no normalized drift detected"}
	}
	return diffs
}

func suggestCaptures(reference Capture, candidate Capture) []string {
	var suggestions []string
	if reference.TLS != nil && candidate.TLS != nil {
		if reference.TLS.JA3NHash != "" && candidate.TLS.JA3NHash != "" && reference.TLS.JA3NHash != candidate.TLS.JA3NHash {
			suggestions = append(suggestions,
				"TLS transport profile differs: compare the reference ClientHello against CLIProxy's helps.NewUtlsHTTPClient path. The current production knob is tls.HelloChrome_Auto in internal/runtime/executor/helps/utls_client.go; matching a non-Chrome Codex CLI fingerprint may require a selectable uTLS ClientHelloID/profile or a non-uTLS transport for that path.")
		}
		if strings.Join(reference.TLS.ALPNProtocols, ",") != strings.Join(candidate.TLS.ALPNProtocols, ",") {
			suggestions = append(suggestions,
				fmt.Sprintf("ALPN differs: reference advertises %q while candidate advertises %q. Check uTLS NextProtos/profile behavior for HTTP clients, and remember Codex websocket transport uses gorilla/websocket rather than helps.NewUtlsHTTPClient.", strings.Join(reference.TLS.ALPNProtocols, ","), strings.Join(candidate.TLS.ALPNProtocols, ",")))
		}
		if joinUint16(reference.TLS.CipherSuites) != joinUint16(candidate.TLS.CipherSuites) {
			suggestions = append(suggestions,
				"TLS cipher suite ordering differs: this usually follows the transport profile. Prefer changing/pinning the uTLS profile before hand-editing cipher lists.")
		}
		if joinUint16(reference.TLS.Extensions) != joinUint16(candidate.TLS.Extensions) {
			suggestions = append(suggestions,
				"TLS extension ordering differs: raw JA3 can drift when Chrome-style profiles randomize extension order. Treat ja3n_hash as the stable signal before changing code.")
		}
	}

	if reference.HTTP != nil && candidate.HTTP != nil {
		suggestions = append(suggestions, suggestHTTPHeaders(reference.HTTP.Headers, candidate.HTTP.Headers)...)
		if reference.HTTP.Target != "" && candidate.HTTP.Target != "" && reference.HTTP.Target != candidate.HTTP.Target {
			suggestions = append(suggestions,
				fmt.Sprintf("HTTP target differs: reference uses %q while candidate uses %q. Confirm whether you are comparing OpenAI-compatible /v1 traffic, explicit /backend-api/codex handling, or unmatched backend passthrough before changing headers.", reference.HTTP.Target, candidate.HTTP.Target))
		}
		if reference.HTTP.BodySHA256 != "" && candidate.HTTP.BodySHA256 != "" && reference.HTTP.BodySHA256 != candidate.HTTP.BodySHA256 {
			suggestions = append(suggestions,
				"HTTP body hash differs: for explicit Codex executor requests inspect translation/body normalization before transport tweaks. For backend passthrough, the proxy should preserve the inbound body stream.")
		}
	}

	if len(suggestions) == 0 {
		return []string{"no actionable suggestions from normalized capture fields"}
	}
	return suggestions
}

func suggestHTTPHeaders(reference []HeaderLine, candidate []HeaderLine) []string {
	var suggestions []string
	for _, header := range []string{"User-Agent", "Accept", "Originator", "OpenAI-Beta", "Version", "Connection", "X-Codex-Beta-Features", "X-Codex-Turn-Metadata", "X-Client-Request-Id", "ChatGPT-Account-ID"} {
		refValue, refOK := lookupHeader(reference, header)
		candValue, candOK := lookupHeader(candidate, header)
		if refOK == candOK && refValue == candValue {
			continue
		}
		switch strings.ToLower(header) {
		case "user-agent":
			suggestions = append(suggestions,
				fmt.Sprintf("User-Agent differs: reference has %q and candidate has %q. Prefer configuring codex-header-defaults.user-agent for Codex auths; fallback constants live near codexUserAgent and applyCodexHeaders.", headerDisplayValue(refValue, refOK), headerDisplayValue(candValue, candOK)))
		case "openai-beta":
			suggestions = append(suggestions,
				fmt.Sprintf("OpenAI-Beta differs: reference has %q and candidate has %q. Check applyCodexWebsocketHeaders for websocket fallback and applyCodexHeaders/passthrough preservation for HTTP routes.", headerDisplayValue(refValue, refOK), headerDisplayValue(candValue, candOK)))
		case "chatgpt-account-id":
			suggestions = append(suggestions,
				"ChatGPT account header differs: backend passthrough should preserve ChatGPT-Account-ID while explicit Codex auth may synthesize it from stored auth metadata. Verify casing expectations separately because HTTP/2 lowercases names.")
		default:
			suggestions = append(suggestions,
				fmt.Sprintf("%s header differs: reference has %q and candidate has %q. Check applyCodexHeaders, applyCodexWebsocketHeaders, auth custom header attrs, or passthrough preservation depending on the route.", header, headerDisplayValue(refValue, refOK), headerDisplayValue(candValue, candOK)))
		}
	}
	return suggestions
}

func lookupHeader(headers []HeaderLine, name string) (string, bool) {
	for _, header := range headers {
		if strings.EqualFold(header.Name, name) {
			return header.Value, true
		}
	}
	return "", false
}

func headerDisplayValue(value string, ok bool) string {
	if !ok {
		return "<missing>"
	}
	return value
}

func appendDiff(diffs []string, field string, left string, right string) []string {
	if left == right {
		return diffs
	}
	return append(diffs, fmt.Sprintf("%s: %q != %q", field, left, right))
}

func tlsValue(fp *TLSFingerprint, fn func(*TLSFingerprint) string) string {
	if fp == nil {
		return ""
	}
	return fn(fp)
}

func tlsStringSlice(fp *TLSFingerprint, fn func(*TLSFingerprint) []string) []string {
	if fp == nil {
		return nil
	}
	return fn(fp)
}

func tlsUint16Slice(fp *TLSFingerprint, fn func(*TLSFingerprint) []uint16) []uint16 {
	if fp == nil {
		return nil
	}
	return fn(fp)
}

func httpValue(fp *HTTPFingerprint, fn func(*HTTPFingerprint) string) string {
	if fp == nil {
		return ""
	}
	return fn(fp)
}

func httpHeaders(fp *HTTPFingerprint) []HeaderLine {
	if fp == nil {
		return nil
	}
	return fp.Headers
}

func headerNameSet(headers []HeaderLine) string {
	names := make(map[string]struct{}, len(headers))
	for _, header := range headers {
		name := normalizedHeaderName(header.Name)
		if name == "" {
			continue
		}
		names[name] = struct{}{}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func headerNameOrder(headers []HeaderLine) string {
	out := make([]string, 0, len(headers))
	for _, header := range headers {
		name := normalizedHeaderName(header.Name)
		if name == "" {
			continue
		}
		out = append(out, name)
	}
	return strings.Join(out, ",")
}

func headerNameUnion(left []HeaderLine, right []HeaderLine) []string {
	names := make(map[string]struct{}, len(left)+len(right))
	for _, headers := range [][]HeaderLine{left, right} {
		for _, header := range headers {
			name := normalizedHeaderName(header.Name)
			if name == "" {
				continue
			}
			names[name] = struct{}{}
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func headerValues(headers []HeaderLine, name string) []string {
	normalized := normalizedHeaderName(name)
	var out []string
	for _, header := range headers {
		if normalizedHeaderName(header.Name) == normalized {
			out = append(out, header.Value)
		}
	}
	return out
}

func normalizedHeaderName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

func writeRunArtifacts(outDir string, report RunReport) error {
	if err := writeJSONFile(filepath.Join(outDir, "summary.json"), report); err != nil {
		return err
	}
	for _, capture := range report.Captures {
		name := capture.Name
		if name == "" {
			name = capture.Mode
		}
		if err := writeJSONFile(filepath.Join(outDir, sanitizeFilename(name)+".json"), capture); err != nil {
			return err
		}
	}
	var md strings.Builder
	md.WriteString("# Request Fingerprint Probe\n\n")
	md.WriteString("Target: `" + report.TargetURL + "`\n\n")
	md.WriteString("## Captures\n\n")
	for _, capture := range report.Captures {
		md.WriteString("- `" + capture.Name + "`")
		if capture.TLS != nil {
			md.WriteString(": JA3N `" + capture.TLS.JA3NHash + "`, JA3 `" + capture.TLS.JA3Hash + "`, ALPN `" + strings.Join(capture.TLS.ALPNProtocols, ",") + "`")
		}
		if capture.Error != "" {
			md.WriteString(", error `" + capture.Error + "`")
		}
		md.WriteString("\n")
	}
	md.WriteString("\n## Diff\n\n")
	for _, diff := range report.Diffs {
		md.WriteString("- " + diff + "\n")
	}
	md.WriteString("\n## Suggestions\n\n")
	for _, suggestion := range report.Suggestions {
		md.WriteString("- " + suggestion + "\n")
	}
	return os.WriteFile(filepath.Join(outDir, "compare.md"), []byte(md.String()), 0o644)
}

func writeJSONFile(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o644)
}

func readCapture(path string) (Capture, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Capture{}, err
	}
	var capture Capture
	if err := json.Unmarshal(data, &capture); err != nil {
		return Capture{}, err
	}
	return capture, nil
}

func headerValue(headers []HeaderLine, name string) string {
	for _, header := range headers {
		if strings.EqualFold(header.Name, name) {
			return header.Value
		}
	}
	return ""
}

func normalizePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	if strings.HasPrefix(path, "/") {
		return path
	}
	return "/" + path
}

func defaultOutputDir() string {
	return filepath.Join("scratch", "request-fingerprint-probe", "runs", time.Now().UTC().Format("20060102T150405Z"))
}

func sanitizeFilename(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "capture"
	}
	return out
}

func nowUTC() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func joinUint16(values []uint16) string {
	parts := make([]string, 0, len(values))
	for _, value := range values {
		parts = append(parts, strconv.Itoa(int(value)))
	}
	return strings.Join(parts, "-")
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func summarizeProbeError(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	msg = strings.ReplaceAll(msg, "\n", " ")
	if len(msg) > 240 {
		msg = msg[:240] + "..."
	}
	return msg
}
