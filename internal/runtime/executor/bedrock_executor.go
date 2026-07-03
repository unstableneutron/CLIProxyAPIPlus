package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/sjson"
)

const bedrockProviderKey = "bedrock"

type BedrockExecutor struct {
	cfg *config.Config
}

type bedrockCredentials struct {
	BaseURL  string
	APIKey   string
	AuthType string
}

type bedrockInvokePlan struct {
	Model   string
	API     string
	URL     string
	Payload []byte
}

func NewBedrockExecutor(cfg *config.Config) *BedrockExecutor {
	return &BedrockExecutor{cfg: cfg}
}

func (e *BedrockExecutor) Identifier() string { return bedrockProviderKey }

func (e *BedrockExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	creds := bedrockCreds(auth)
	if err := applyBedrockAuth(req, creds); err != nil {
		return err
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	util.ApplyCustomQueryParamsFromAttrs(req, attrs)
	return nil
}

func (e *BedrockExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("bedrock executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewBedrockHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *BedrockExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	creds := bedrockCreds(auth)
	if creds.BaseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return resp, err
	}

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("claude")
	bodyForTranslation, body, err := e.translateRequest(ctx, req, opts, from, to, baseModel, false)
	if err != nil {
		return resp, err
	}
	plan, err := e.prepareInvokePlan(auth, creds.BaseURL, baseModel, body, false)
	if err != nil {
		return resp, err
	}
	reporter.SetTranslatedReasoningEffort(body, to.String())

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, plan.URL, bytes.NewReader(plan.Payload))
	if err != nil {
		return resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", "cli-proxy-bedrock")
	if errPrepare := e.PrepareRequest(httpReq, auth); errPrepare != nil {
		return resp, errPrepare
	}
	recordBedrockRequest(ctx, e.cfg, e.Identifier(), auth, httpReq, plan.Payload)
	httpClient := helps.NewBedrockHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("bedrock executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIHTTPResponseMetadata(ctx, e.cfg, httpResp)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		err = statusErr{code: httpResp.StatusCode, msg: string(b)}
		return resp, err
	}
	data, err := io.ReadAll(httpResp.Body)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return resp, err
	}
	helps.AppendAPIResponseChunk(ctx, e.cfg, data)
	claudeSSE := helps.BedrockResponseToClaudeSSE(plan.Model, data)
	reporter.Publish(ctx, helps.ParseClaudeUsage(claudeSSE))
	reporter.EnsurePublished(ctx)
	if responseFormat == to {
		return cliproxyexecutor.Response{Payload: helps.BedrockResponseToClaudeMessage(plan.Model, data), Headers: httpResp.Header.Clone()}, nil
	}
	var param any
	out := sdktranslator.TranslateNonStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, bodyForTranslation, claudeSSE, &param)
	return cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}, nil
}

func (e *BedrockExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusNotImplemented, msg: "/responses/compact not supported"}
	}
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	creds := bedrockCreds(auth)
	if creds.BaseURL == "" {
		err = statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
		return nil, err
	}

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("claude")
	bodyForTranslation, body, err := e.translateRequest(ctx, req, opts, from, to, baseModel, true)
	if err != nil {
		return nil, err
	}
	plan, err := e.prepareInvokePlan(auth, creds.BaseURL, baseModel, body, true)
	if err != nil {
		return nil, err
	}
	reporter.SetTranslatedReasoningEffort(body, to.String())

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, plan.URL, bytes.NewReader(plan.Payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Cache-Control", "no-cache")
	httpReq.Header.Set("User-Agent", "cli-proxy-bedrock")
	if errPrepare := e.PrepareRequest(httpReq, auth); errPrepare != nil {
		return nil, errPrepare
	}
	recordBedrockRequest(ctx, e.cfg, e.Identifier(), auth, httpReq, plan.Payload)
	httpClient := helps.NewBedrockHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, err)
		return nil, err
	}
	helps.RecordAPIHTTPResponseMetadata(ctx, e.cfg, httpResp)
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		bodyBytes, errRead := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("bedrock executor: close response body error: %v", errClose)
		}
		if errRead != nil {
			helps.RecordAPIResponseError(ctx, e.cfg, errRead)
			return nil, errRead
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, bodyBytes)
		return nil, statusErr{code: httpResp.StatusCode, msg: string(bodyBytes)}
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("bedrock executor: close response body error: %v", errClose)
			}
			reporter.EnsurePublished(ctx)
		}()
		var param any
		normalizer := helps.NewBedrockStreamNormalizer(plan.Model)
		emitClaudeLines := func(lines [][]byte) bool {
			for _, claudeLine := range lines {
				if detail, ok := helps.ParseClaudeStreamUsage(claudeLine); ok {
					reporter.Publish(ctx, detail)
				}
				chunks := sdktranslator.TranslateStream(ctx, to, responseFormat, req.Model, opts.OriginalRequest, bodyForTranslation, claudeLine, &param)
				for i := range chunks {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
					case <-ctx.Done():
						return false
					}
				}
			}
			return true
		}

		if strings.Contains(strings.ToLower(httpResp.Header.Get("Content-Type")), "application/vnd.amazon.eventstream") {
			errEventStream := helps.ForEachBedrockEventStreamMessage(httpResp.Body, func(msg helps.BedrockEventStreamMessage) bool {
				helps.AppendAPIResponseChunk(ctx, e.cfg, msg.Payload)
				if errException := bedrockEventStreamException(msg); errException != nil {
					helps.RecordAPIResponseError(ctx, e.cfg, errException)
					reporter.PublishFailure(ctx, errException)
					select {
					case out <- cliproxyexecutor.StreamChunk{Err: errException}:
					case <-ctx.Done():
					}
					return false
				}
				return emitClaudeLines(normalizer.ConvertLine(msg.Payload))
			})
			if errEventStream != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, errEventStream)
				reporter.PublishFailure(ctx, errEventStream)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: errEventStream}:
				case <-ctx.Done():
				}
				return
			}
		} else {
			scanner := bufio.NewScanner(httpResp.Body)
			scanner.Buffer(nil, 52_428_800)
			for scanner.Scan() {
				line := bytes.TrimSpace(bytes.Clone(scanner.Bytes()))
				helps.AppendAPIResponseChunk(ctx, e.cfg, line)
				if len(line) == 0 || bytes.HasPrefix(line, []byte("event:")) || bytes.HasPrefix(line, []byte(":")) {
					continue
				}
				if !emitClaudeLines(normalizer.ConvertLine(line)) {
					return
				}
			}
			if errScan := scanner.Err(); errScan != nil {
				helps.RecordAPIResponseError(ctx, e.cfg, errScan)
				reporter.PublishFailure(ctx, errScan)
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: errScan}:
				case <-ctx.Done():
				}
				return
			}
		}
		emitClaudeLines(normalizer.Finish())
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

func (e *BedrockExecutor) CountTokens(context.Context, *cliproxyauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "bedrock token counting not supported"}
}

func (e *BedrockExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	if refreshed, handled, err := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, err
	}
	return auth, nil
}

func (e *BedrockExecutor) translateRequest(ctx context.Context, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, from, to sdktranslator.Format, baseModel string, stream bool) ([]byte, []byte, error) {
	originalPayloadSource := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayloadSource = opts.OriginalRequest
	}
	originalTranslated := sdktranslator.TranslateRequest(from, to, baseModel, originalPayloadSource, stream)
	body := sdktranslator.TranslateRequest(from, to, baseModel, req.Payload, stream)
	body, _ = sjson.SetBytes(body, "model", baseModel)
	var err error
	body, err = thinking.ApplyThinking(body, req.Model, from.String(), to.String(), e.Identifier())
	if err != nil {
		return nil, nil, err
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, "bedrock", from.String(), "", body, originalTranslated, requestedModel, requestPath, opts.Headers)
	body = ensureModelMaxTokens(body, baseModel)
	body = disableThinkingIfToolChoiceForced(body)
	body = normalizeClaudeSamplingForThinking(body)
	return body, body, nil
}

func (e *BedrockExecutor) prepareInvokePlan(auth *cliproxyauth.Auth, baseURL, requestedModel string, claudeBody []byte, stream bool) (bedrockInvokePlan, error) {
	model := resolveBedrockModel(auth, requestedModel)
	api := resolveBedrockAPI(auth, requestedModel, stream)
	payload := helps.BedrockPayloadForAPI(api, model, claudeBody, stream)
	return bedrockInvokePlan{
		Model:   model,
		API:     api,
		URL:     helps.BedrockRuntimeURL(baseURL, model, api),
		Payload: payload,
	}, nil
}

func bedrockCreds(auth *cliproxyauth.Auth) bedrockCredentials {
	if auth == nil || auth.Attributes == nil {
		return bedrockCredentials{}
	}
	return bedrockCredentials{
		BaseURL:  strings.TrimSpace(auth.Attributes["base_url"]),
		APIKey:   strings.TrimSpace(auth.Attributes["api_key"]),
		AuthType: strings.ToLower(strings.TrimSpace(auth.Attributes["auth_type"])),
	}
}

func applyBedrockAuth(req *http.Request, creds bedrockCredentials) error {
	if strings.TrimSpace(creds.APIKey) == "" {
		return nil
	}
	switch creds.AuthType {
	case config.BedrockAuthTypeNone:
		return nil
	case config.BedrockAuthTypeRaw, "authorization":
		req.Header.Set("Authorization", creds.APIKey)
		return nil
	case "", config.BedrockAuthTypeBearer:
		req.Header.Set("Authorization", "Bearer "+creds.APIKey)
		return nil
	default:
		return fmt.Errorf("bedrock executor: unsupported auth type %q", creds.AuthType)
	}
}

func resolveBedrockModel(auth *cliproxyauth.Auth, requested string) string {
	model := strings.TrimSpace(requested)
	if auth == nil || auth.Attributes == nil {
		return model
	}
	if mapped := lookupBedrockStringMap(auth.Attributes["bedrock_model_map"], model); mapped != "" {
		return mapped
	}
	return model
}

func resolveBedrockAPI(auth *cliproxyauth.Auth, requested string, stream bool) string {
	defaultAPI := "converse"
	if stream {
		defaultAPI = "converse-stream"
	}
	if auth == nil || auth.Attributes == nil {
		return defaultAPI
	}
	key := "bedrock_api_map"
	if stream {
		key = "bedrock_stream_map"
	}
	if mapped := lookupBedrockStringMap(auth.Attributes[key], strings.TrimSpace(requested)); mapped != "" {
		return helps.NormalizeBedrockAPI(mapped, stream)
	}
	model := resolveBedrockModel(auth, requested)
	if mapped := lookupBedrockStringMap(auth.Attributes[key], model); mapped != "" {
		return helps.NormalizeBedrockAPI(mapped, stream)
	}
	if stream {
		if mapped := lookupBedrockStringMap(auth.Attributes["bedrock_api_map"], strings.TrimSpace(requested)); mapped != "" {
			if helps.NormalizeBedrockAPI(mapped, false) == "invoke" {
				return "invoke-stream"
			}
		}
		if mapped := lookupBedrockStringMap(auth.Attributes["bedrock_api_map"], model); mapped != "" {
			if helps.NormalizeBedrockAPI(mapped, false) == "invoke" {
				return "invoke-stream"
			}
		}
	}
	return defaultAPI
}

func lookupBedrockStringMap(raw, key string) string {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(key) == "" {
		return ""
	}
	var values map[string]string
	if err := json.Unmarshal([]byte(raw), &values); err != nil {
		return ""
	}
	return strings.TrimSpace(values[key])
}

func recordBedrockRequest(ctx context.Context, cfg *config.Config, provider string, auth *cliproxyauth.Auth, httpReq *http.Request, body []byte) {
	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	logReq := bedrockRequestForLog(httpReq)
	helps.RecordAPIRequest(ctx, cfg, helps.UpstreamRequestLog{
		URL:       logReq.URL.String(),
		Method:    logReq.Method,
		Headers:   logReq.Header.Clone(),
		Body:      body,
		Provider:  provider,
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
}

func bedrockRequestForLog(req *http.Request) *http.Request {
	if req == nil || req.URL == nil {
		return req
	}
	out := req.Clone(req.Context())
	copiedURL := *req.URL
	copiedURL.RawQuery = maskBedrockQueryForLog(req.URL.Query())
	out.URL = &copiedURL
	return out
}

func maskBedrockQueryForLog(values url.Values) string {
	if len(values) == 0 {
		return ""
	}
	masked := make(url.Values, len(values))
	for key := range values {
		masked[key] = []string{"***"}
	}
	return masked.Encode()
}

func bedrockEventStreamException(msg helps.BedrockEventStreamMessage) error {
	if strings.ToLower(strings.TrimSpace(msg.Headers[":message-type"])) != "exception" {
		return nil
	}
	exceptionType := strings.TrimSpace(msg.Headers[":exception-type"])
	message := strings.TrimSpace(gjsonGetString(msg.Payload, "message"))
	if message == "" {
		message = strings.TrimSpace(string(msg.Payload))
	}
	if exceptionType == "" {
		exceptionType = "bedrockException"
	}
	return errors.New(exceptionType + ": " + message)
}

func gjsonGetString(data []byte, path string) string {
	var value struct {
		Message string `json:"message"`
	}
	if path != "message" || len(data) == 0 {
		return ""
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return ""
	}
	return value.Message
}
