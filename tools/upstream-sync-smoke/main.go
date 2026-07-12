package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	outcomePassed              = "passed"
	outcomeFailed              = "failed"
	outcomeExternalAuthBlocked = "external-auth-blocked"
	maxResponseBytes           = 4 << 20
)

var bearerValuePattern = regexp.MustCompile(`(?i)(bearer[[:space:]]+)[A-Za-z0-9._~+/=-]+`)

type smokeResult struct {
	Command       string `json:"command"`
	Transport     string `json:"transport,omitempty"`
	Model         string `json:"model,omitempty"`
	TerminalEvent string `json:"terminal_event,omitempty"`
	MarkerMatched bool   `json:"marker_matched,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
	HTTPStatus    int    `json:"http_status,omitempty"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	Outcome       string `json:"outcome"`
	Error         string `json:"error,omitempty"`
}

type responsesConfig struct {
	BaseURL    string
	Model      string
	Marker     string
	APIKey     string
	Transport  string
	HTTPClient *http.Client
	Dialer     *websocket.Dialer
}

func main() {
	os.Exit(runCLI(os.Args[1:], os.Stdout, os.Stderr))
}

func runCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: upstream-sync-smoke <health|responses|compact|preflight-jwt> [flags]")
		return 2
	}

	ctx := context.Background()
	switch args[0] {
	case "health":
		flags := flag.NewFlagSet("health", flag.ContinueOnError)
		flags.SetOutput(stderr)
		baseURL := flags.String("base-url", "", "CLIProxyAPI base URL")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		result, err := runHealth(ctx, *baseURL, http.DefaultClient)
		return emitResult(stdout, result, err)

	case "responses":
		flags := flag.NewFlagSet("responses", flag.ContinueOnError)
		flags.SetOutput(stderr)
		baseURL := flags.String("base-url", "", "CLIProxyAPI base URL")
		transport := flags.String("transport", "", "rest, sse, or websocket")
		model := flags.String("model", "", "model identifier")
		marker := flags.String("marker", "", "exact output marker")
		apiKeyEnv := flags.String("api-key-env", "", "environment variable containing the API key")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		apiKey, errKey := credentialFromEnv(*apiKeyEnv)
		if errKey != nil {
			result := smokeResult{Command: "responses", Transport: *transport, Model: *model, Outcome: outcomeExternalAuthBlocked}
			return emitResultWithSecrets(stdout, result, errKey)
		}
		result, err := runResponses(ctx, responsesConfig{
			BaseURL:   *baseURL,
			Model:     *model,
			Marker:    *marker,
			APIKey:    apiKey,
			Transport: *transport,
		})
		return emitResultWithSecrets(stdout, result, err, apiKey)

	case "compact":
		flags := flag.NewFlagSet("compact", flag.ContinueOnError)
		flags.SetOutput(stderr)
		baseURL := flags.String("base-url", "", "CLIProxyAPI base URL")
		model := flags.String("model", "", "model identifier")
		apiKeyEnv := flags.String("api-key-env", "", "environment variable containing the API key")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		apiKey, errKey := credentialFromEnv(*apiKeyEnv)
		if errKey != nil {
			result := smokeResult{Command: "compact", Model: *model, Outcome: outcomeExternalAuthBlocked}
			return emitResultWithSecrets(stdout, result, errKey)
		}
		result, err := runCompact(ctx, *baseURL, *model, apiKey, http.DefaultClient)
		return emitResultWithSecrets(stdout, result, err, apiKey)

	case "preflight-jwt":
		flags := flag.NewFlagSet("preflight-jwt", flag.ContinueOnError)
		flags.SetOutput(stderr)
		tokenEnv := flags.String("token-env", "", "environment variable containing the JWT")
		minimumValidity := flags.Duration("minimum-validity", 5*time.Minute, "required remaining token validity")
		if err := flags.Parse(args[1:]); err != nil {
			return 2
		}
		started := time.Now()
		token, errToken := credentialFromEnv(*tokenEnv)
		result := smokeResult{Command: "preflight-jwt", Outcome: outcomeExternalAuthBlocked}
		if errToken != nil {
			result.DurationMS = time.Since(started).Milliseconds()
			return emitResultWithSecrets(stdout, result, errToken)
		}
		expiresAt, errPreflight := preflightJWT(token, *minimumValidity, time.Now())
		result.DurationMS = time.Since(started).Milliseconds()
		if errPreflight == nil {
			result.Outcome = outcomePassed
			result.TerminalEvent = "jwt.valid"
			result.ExpiresAt = expiresAt.UTC().Format(time.RFC3339)
		}
		return emitResultWithSecrets(stdout, result, errPreflight, token)

	default:
		_, _ = fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		return 2
	}
}

func emitResult(w io.Writer, result smokeResult, err error) int {
	return emitResultWithSecrets(w, result, err)
}

func emitResultWithSecrets(w io.Writer, result smokeResult, err error, secrets ...string) int {
	if err != nil {
		if result.Outcome == "" {
			result.Outcome = outcomeFailed
		}
		result.Error = err.Error()
	}
	if result.Outcome == "" {
		result.Outcome = outcomePassed
	}
	if errWrite := writeResult(w, result, secrets...); errWrite != nil {
		return 1
	}
	if err != nil {
		return 1
	}
	return 0
}

func credentialFromEnv(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("credential environment variable name is required")
	}
	value := os.Getenv(name)
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("credential environment variable %s is empty", name)
	}
	return value, nil
}

func runHealth(ctx context.Context, baseURL string, client *http.Client) (result smokeResult, err error) {
	started := time.Now()
	result = smokeResult{Command: "health", Outcome: outcomeFailed}
	defer func() { result.DurationMS = time.Since(started).Milliseconds() }()
	if strings.TrimSpace(baseURL) == "" {
		return result, fmt.Errorf("base URL is required")
	}
	if client == nil {
		client = http.DefaultClient
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL(baseURL, "/healthz"), nil)
	if errRequest != nil {
		return result, fmt.Errorf("create health request: %w", errRequest)
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return result, fmt.Errorf("health request: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	result.HTTPStatus = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return result, fmt.Errorf("health returned HTTP %d", resp.StatusCode)
	}
	result.Outcome = outcomePassed
	result.TerminalEvent = "health.ok"
	return result, nil
}

func runResponses(ctx context.Context, cfg responsesConfig) (result smokeResult, err error) {
	started := time.Now()
	result = smokeResult{
		Command:   "responses",
		Transport: cfg.Transport,
		Model:     cfg.Model,
		Outcome:   outcomeFailed,
	}
	defer func() { result.DurationMS = time.Since(started).Milliseconds() }()

	if strings.TrimSpace(cfg.BaseURL) == "" {
		return result, fmt.Errorf("base URL is required")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return result, fmt.Errorf("model is required")
	}
	if cfg.Marker == "" {
		return result, fmt.Errorf("marker is required")
	}
	if cfg.APIKey == "" {
		result.Outcome = outcomeExternalAuthBlocked
		return result, fmt.Errorf("API key is required")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}

	switch cfg.Transport {
	case "rest":
		err = runResponsesREST(ctx, cfg, &result)
	case "sse":
		err = runResponsesSSE(ctx, cfg, &result)
	case "websocket":
		err = runResponsesWebsocket(ctx, cfg, &result)
	default:
		err = fmt.Errorf("transport must be rest, sse, or websocket")
	}
	return result, err
}

func responsesPayload(cfg responsesConfig, stream bool) ([]byte, error) {
	payload := map[string]any{
		"model":             cfg.Model,
		"input":             "Reply exactly with " + cfg.Marker,
		"max_output_tokens": 64,
	}
	if stream {
		payload["stream"] = true
	}
	return json.Marshal(payload)
}

func newAuthorizedRequest(ctx context.Context, method, target, apiKey string, body []byte) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

func runResponsesREST(ctx context.Context, cfg responsesConfig, result *smokeResult) error {
	payload, errPayload := responsesPayload(cfg, false)
	if errPayload != nil {
		return fmt.Errorf("encode Responses request: %w", errPayload)
	}
	req, errRequest := newAuthorizedRequest(ctx, http.MethodPost, endpointURL(cfg.BaseURL, "/v1/responses"), cfg.APIKey, payload)
	if errRequest != nil {
		return fmt.Errorf("create Responses request: %w", errRequest)
	}
	resp, errDo := cfg.HTTPClient.Do(req)
	if errDo != nil {
		return fmt.Errorf("Responses REST request: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	result.HTTPStatus = resp.StatusCode
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if errRead != nil {
		return fmt.Errorf("read Responses REST body: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		setAuthOutcome(resp.StatusCode, result)
		return fmt.Errorf("Responses REST returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if !json.Valid(body) {
		return fmt.Errorf("Responses REST returned invalid JSON")
	}
	var envelope struct {
		Status string `json:"status"`
	}
	if errUnmarshal := json.Unmarshal(body, &envelope); errUnmarshal != nil {
		return fmt.Errorf("decode Responses REST body: %w", errUnmarshal)
	}
	if envelope.Status != "" && envelope.Status != "completed" {
		result.TerminalEvent = "response." + envelope.Status
		return fmt.Errorf("Responses REST ended with status %s", envelope.Status)
	}
	result.MarkerMatched = bytes.Contains(body, []byte(cfg.Marker))
	if !result.MarkerMatched {
		return fmt.Errorf("Responses REST output did not contain the expected marker")
	}
	result.TerminalEvent = "response.completed"
	result.Outcome = outcomePassed
	return nil
}

func runResponsesSSE(ctx context.Context, cfg responsesConfig, result *smokeResult) error {
	payload, errPayload := responsesPayload(cfg, true)
	if errPayload != nil {
		return fmt.Errorf("encode Responses SSE request: %w", errPayload)
	}
	req, errRequest := newAuthorizedRequest(ctx, http.MethodPost, endpointURL(cfg.BaseURL, "/v1/responses"), cfg.APIKey, payload)
	if errRequest != nil {
		return fmt.Errorf("create Responses SSE request: %w", errRequest)
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, errDo := cfg.HTTPClient.Do(req)
	if errDo != nil {
		return fmt.Errorf("Responses SSE request: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	result.HTTPStatus = resp.StatusCode
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
		setAuthOutcome(resp.StatusCode, result)
		return fmt.Errorf("Responses SSE returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), maxResponseBytes)
	eventType := ""
	dataLines := make([]string, 0, 1)
	processEvent := func() (bool, error) {
		data := strings.Join(dataLines, "\n")
		dataLines = dataLines[:0]
		if strings.Contains(data, cfg.Marker) {
			result.MarkerMatched = true
		}
		if strings.TrimSpace(data) == "[DONE]" {
			result.TerminalEvent = "response.completed"
			if !result.MarkerMatched {
				return true, fmt.Errorf("Responses SSE output did not contain the expected marker")
			}
			result.Outcome = outcomePassed
			return true, nil
		}
		resolvedType := eventType
		if data != "" {
			var envelope struct {
				Type string `json:"type"`
			}
			if json.Unmarshal([]byte(data), &envelope) == nil && envelope.Type != "" {
				resolvedType = envelope.Type
			}
		}
		eventType = ""
		switch resolvedType {
		case "response.completed":
			result.TerminalEvent = resolvedType
			if !result.MarkerMatched {
				return true, fmt.Errorf("Responses SSE output did not contain the expected marker")
			}
			result.Outcome = outcomePassed
			return true, nil
		case "response.failed", "error":
			result.TerminalEvent = resolvedType
			return true, fmt.Errorf("Responses SSE ended with %s: %s", resolvedType, strings.TrimSpace(data))
		default:
			return false, nil
		}
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		switch {
		case line == "":
			done, errEvent := processEvent()
			if done {
				return errEvent
			}
		case strings.HasPrefix(line, "event:"):
			eventType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return fmt.Errorf("read Responses SSE stream: %w", errScan)
	}
	if done, errEvent := processEvent(); done {
		return errEvent
	}
	return fmt.Errorf("Responses SSE stream closed before a terminal event")
}

func runResponsesWebsocket(ctx context.Context, cfg responsesConfig, result *smokeResult) error {
	payload, errPayload := responsesPayload(cfg, false)
	if errPayload != nil {
		return fmt.Errorf("encode Responses websocket request: %w", errPayload)
	}
	var create map[string]any
	if errUnmarshal := json.Unmarshal(payload, &create); errUnmarshal != nil {
		return fmt.Errorf("prepare Responses websocket request: %w", errUnmarshal)
	}
	create["type"] = "response.create"
	payload, _ = json.Marshal(create)

	wsURL, errURL := websocketURL(cfg.BaseURL, "/v1/responses")
	if errURL != nil {
		return errURL
	}
	header := http.Header{}
	header.Set("Authorization", "Bearer "+cfg.APIKey)
	dialer := cfg.Dialer
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	conn, resp, errDial := dialer.DialContext(ctx, wsURL, header)
	if errDial != nil {
		if resp != nil {
			result.HTTPStatus = resp.StatusCode
			setAuthOutcome(resp.StatusCode, result)
			body, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
			_ = resp.Body.Close()
			return fmt.Errorf("Responses websocket handshake returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		return fmt.Errorf("dial Responses websocket: %w", errDial)
	}
	defer func() { _ = conn.Close() }()
	result.HTTPStatus = http.StatusSwitchingProtocols
	if errWrite := conn.WriteMessage(websocket.TextMessage, payload); errWrite != nil {
		return fmt.Errorf("write Responses websocket request: %w", errWrite)
	}

	for {
		_, message, errRead := conn.ReadMessage()
		if errRead != nil {
			return fmt.Errorf("Responses websocket closed before a terminal event: %w", errRead)
		}
		if bytes.Contains(message, []byte(cfg.Marker)) {
			result.MarkerMatched = true
		}
		var envelope struct {
			Type string `json:"type"`
		}
		if errUnmarshal := json.Unmarshal(message, &envelope); errUnmarshal != nil {
			return fmt.Errorf("Responses websocket returned invalid JSON: %w", errUnmarshal)
		}
		switch envelope.Type {
		case "response.completed":
			result.TerminalEvent = envelope.Type
			if !result.MarkerMatched {
				return fmt.Errorf("Responses websocket output did not contain the expected marker")
			}
			result.Outcome = outcomePassed
			return nil
		case "response.failed", "error":
			result.TerminalEvent = envelope.Type
			return fmt.Errorf("Responses websocket ended with %s: %s", envelope.Type, strings.TrimSpace(string(message)))
		}
	}
}

func runCompact(ctx context.Context, baseURL, model, apiKey string, client *http.Client) (result smokeResult, err error) {
	started := time.Now()
	result = smokeResult{Command: "compact", Model: model, Outcome: outcomeFailed}
	defer func() { result.DurationMS = time.Since(started).Milliseconds() }()
	if strings.TrimSpace(baseURL) == "" {
		return result, fmt.Errorf("base URL is required")
	}
	if strings.TrimSpace(model) == "" {
		return result, fmt.Errorf("model is required")
	}
	if apiKey == "" {
		result.Outcome = outcomeExternalAuthBlocked
		return result, fmt.Errorf("API key is required")
	}
	if client == nil {
		client = http.DefaultClient
	}
	payload, errMarshal := json.Marshal(map[string]any{
		"model": model,
		"input": "Create a compact checkpoint for this smoke test.",
	})
	if errMarshal != nil {
		return result, fmt.Errorf("encode compact request: %w", errMarshal)
	}
	req, errRequest := newAuthorizedRequest(ctx, http.MethodPost, endpointURL(baseURL, "/v1/responses/compact"), apiKey, payload)
	if errRequest != nil {
		return result, fmt.Errorf("create compact request: %w", errRequest)
	}
	resp, errDo := client.Do(req)
	if errDo != nil {
		return result, fmt.Errorf("compact request: %w", errDo)
	}
	defer func() { _ = resp.Body.Close() }()
	result.HTTPStatus = resp.StatusCode
	body, errRead := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if errRead != nil {
		return result, fmt.Errorf("read compact body: %w", errRead)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		setAuthOutcome(resp.StatusCode, &result)
		return result, fmt.Errorf("compact returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if !json.Valid(body) {
		return result, fmt.Errorf("compact returned invalid JSON")
	}
	result.TerminalEvent = "response.compacted"
	result.Outcome = outcomePassed
	return result, nil
}

func preflightJWT(token string, minimumValidity time.Duration, now time.Time) (time.Time, error) {
	if minimumValidity < 0 {
		return time.Time{}, fmt.Errorf("minimum validity must not be negative")
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, fmt.Errorf("credential is not a three-part JWT")
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil {
		return time.Time{}, fmt.Errorf("decode JWT payload: %w", errDecode)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var claims map[string]any
	if errJSON := decoder.Decode(&claims); errJSON != nil {
		return time.Time{}, fmt.Errorf("decode JWT claims: %w", errJSON)
	}
	expNumber, ok := claims["exp"].(json.Number)
	if !ok {
		return time.Time{}, fmt.Errorf("JWT has no numeric exp claim")
	}
	expUnix, errInt := expNumber.Int64()
	if errInt != nil {
		return time.Time{}, fmt.Errorf("JWT exp claim is not an integer: %w", errInt)
	}
	expiresAt := time.Unix(expUnix, 0)
	if expiresAt.Sub(now) < minimumValidity {
		return time.Time{}, fmt.Errorf("JWT expires before the required minimum validity")
	}
	return expiresAt, nil
}

func endpointURL(baseURL, path string) string {
	return strings.TrimRight(baseURL, "/") + path
}

func websocketURL(baseURL, path string) (string, error) {
	parsed, errParse := url.Parse(endpointURL(baseURL, path))
	if errParse != nil {
		return "", fmt.Errorf("parse websocket base URL: %w", errParse)
	}
	switch parsed.Scheme {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("websocket base URL must use http, https, ws, or wss")
	}
	return parsed.String(), nil
}

func setAuthOutcome(status int, result *smokeResult) {
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		result.Outcome = outcomeExternalAuthBlocked
	}
}

func writeResult(w io.Writer, result smokeResult, secrets ...string) error {
	result.Command = redact(result.Command, secrets...)
	result.Transport = redact(result.Transport, secrets...)
	result.Model = redact(result.Model, secrets...)
	result.TerminalEvent = redact(result.TerminalEvent, secrets...)
	result.Error = redact(result.Error, secrets...)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(result)
}

func redact(value string, secrets ...string) string {
	redacted := value
	for _, secret := range secrets {
		if secret == "" {
			continue
		}
		redacted = strings.ReplaceAll(redacted, secret, "[REDACTED]")
	}
	return bearerValuePattern.ReplaceAllString(redacted, "${1}[REDACTED]")
}
