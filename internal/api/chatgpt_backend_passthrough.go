package api

import (
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	codexauth "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

const (
	chatGPTBackendPassthroughDefaultBaseURL = "https://chatgpt.com"
	chatGPTAccountIDHeader                  = "ChatGPT-Account-ID"
)

var chatGPTBackendHopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"proxy-connection":    {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
}

func (s *Server) handleChatGPTBackendPassthroughNoRoute(c *gin.Context) {
	if !chatGPTBackendPassthroughEligible(c.Request) {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	if s == nil || s.accessManager == nil {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	result, err := s.accessManager.Authenticate(c.Request.Context(), c.Request)
	if err != nil {
		statusCode := err.HTTPStatusCode()
		if statusCode >= http.StatusInternalServerError {
			log.Errorf("chatgpt backend passthrough authentication error: %v", err)
		}
		c.AbortWithStatusJSON(statusCode, gin.H{"error": err.Message})
		return
	}
	if result != nil {
		c.Set("userApiKey", result.Principal)
		c.Set("accessProvider", result.Provider)
		if len(result.Metadata) > 0 {
			c.Set("accessMetadata", result.Metadata)
		}
	}

	s.proxyChatGPTBackend(c)
}

func chatGPTBackendPassthroughEligible(req *http.Request) bool {
	if req == nil || req.URL == nil {
		return false
	}
	if !strings.HasPrefix(req.URL.Path, "/backend-api/") {
		return false
	}
	if strings.TrimSpace(req.Header.Get("Authorization")) == "" {
		return false
	}
	return strings.TrimSpace(req.Header.Get(chatGPTAccountIDHeader)) != ""
}

func (s *Server) proxyChatGPTBackend(c *gin.Context) {
	start := time.Now()
	inboundAccountID := strings.TrimSpace(c.GetHeader(chatGPTAccountIDHeader))
	matchedAuth, matchedToken := s.matchChatGPTBackendAuth(inboundAccountID)
	authSource := "inbound"
	if matchedToken != "" {
		authSource = "stored"
	}

	upstreamURL, err := s.chatGPTBackendPassthroughURL(c.Request.URL)
	if err != nil {
		log.WithError(err).Warn("chatgpt backend passthrough failed to build upstream url")
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "failed to build upstream request"})
		return
	}

	upstreamReq, err := http.NewRequestWithContext(c.Request.Context(), c.Request.Method, upstreamURL, c.Request.Body)
	if err != nil {
		log.WithError(err).Warn("chatgpt backend passthrough failed to create upstream request")
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "failed to build upstream request"})
		return
	}
	upstreamReq.ContentLength = c.Request.ContentLength
	copyPassthroughRequestHeaders(upstreamReq.Header, c.Request.Header)
	if matchedToken != "" {
		upstreamReq.Header.Set("Authorization", "Bearer "+matchedToken)
	}
	upstreamReq.Header.Set(chatGPTAccountIDHeader, inboundAccountID)

	client := s.chatGPTBackendHTTPClient(c, matchedAuth)
	resp, err := client.Do(upstreamReq)
	if err != nil {
		log.WithError(err).WithFields(log.Fields{
			"method":      c.Request.Method,
			"path":        c.Request.URL.Path,
			"auth_source": authSource,
			"duration_ms": time.Since(start).Milliseconds(),
		}).Warn("chatgpt backend passthrough upstream request failed")
		c.AbortWithStatusJSON(http.StatusBadGateway, gin.H{"error": "upstream request failed"})
		return
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Warn("chatgpt backend passthrough response close failed")
		}
	}()

	copyPassthroughResponseHeaders(c.Writer.Header(), resp.Header)
	c.Status(resp.StatusCode)
	if errCopy := copyPassthroughResponseBody(c.Writer, resp.Body); errCopy != nil {
		log.WithError(errCopy).WithFields(log.Fields{
			"method":          c.Request.Method,
			"path":            c.Request.URL.Path,
			"upstream_status": resp.StatusCode,
			"auth_source":     authSource,
			"duration_ms":     time.Since(start).Milliseconds(),
		}).Warn("chatgpt backend passthrough response relay failed")
		return
	}

	log.WithFields(log.Fields{
		"method":          c.Request.Method,
		"path":            c.Request.URL.Path,
		"upstream_status": resp.StatusCode,
		"auth_source":     authSource,
		"duration_ms":     time.Since(start).Milliseconds(),
	}).Info("chatgpt backend passthrough completed")
}

func (s *Server) chatGPTBackendPassthroughURL(inbound *url.URL) (string, error) {
	base := strings.TrimSpace(s.chatGPTBackendPassthroughBaseURL)
	if base == "" {
		base = chatGPTBackendPassthroughDefaultBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	parsed.Path = joinURLPaths(parsed.Path, inbound.EscapedPath())
	parsed.RawQuery = inbound.RawQuery
	parsed.Fragment = ""
	return parsed.String(), nil
}

func joinURLPaths(basePath, requestPath string) string {
	basePath = strings.TrimRight(basePath, "/")
	if requestPath == "" {
		requestPath = "/"
	}
	if !strings.HasPrefix(requestPath, "/") {
		requestPath = "/" + requestPath
	}
	if basePath == "" {
		return requestPath
	}
	return basePath + requestPath
}

func (s *Server) chatGPTBackendHTTPClient(c *gin.Context, matchedAuth *coreauth.Auth) *http.Client {
	if s.chatGPTBackendPassthroughClient != nil {
		return s.chatGPTBackendPassthroughClient
	}
	return helps.NewUtlsHTTPClient(c.Request.Context(), s.cfg, matchedAuth, 0)
}

func (s *Server) matchChatGPTBackendAuth(accountID string) (*coreauth.Auth, string) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || s == nil || s.handlers == nil || s.handlers.AuthManager == nil {
		return nil, ""
	}

	auths := s.handlers.AuthManager.List()
	sort.SliceStable(auths, func(i, j int) bool {
		left := auths[i]
		right := auths[j]
		if left == nil || right == nil {
			return right != nil
		}
		if !left.CreatedAt.Equal(right.CreatedAt) {
			return left.CreatedAt.Before(right.CreatedAt)
		}
		return left.ID < right.ID
	})

	for _, candidate := range auths {
		if !chatGPTBackendAuthActive(candidate) {
			continue
		}
		if !strings.EqualFold(chatGPTBackendAuthAccountID(candidate), accountID) {
			continue
		}
		token := chatGPTBackendAuthAccessToken(candidate)
		if token == "" {
			return candidate, ""
		}
		return candidate, token
	}
	return nil, ""
}

func chatGPTBackendAuthActive(auth *coreauth.Auth) bool {
	if auth == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return false
	}
	if auth.Disabled || auth.Unavailable {
		return false
	}
	return auth.Status == "" || auth.Status == coreauth.StatusActive
}

func chatGPTBackendAuthAccountID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if value := firstStringMapValue(auth.Attributes, "chatgpt_account_id", "account_id"); value != "" {
		return value
	}
	if value := firstAnyMapValue(auth.Metadata, "chatgpt_account_id", "account_id"); value != "" {
		return value
	}
	if value := nestedAnyMapValue(auth.Metadata, "tokens", "account_id", "chatgpt_account_id"); value != "" {
		return value
	}
	idToken := firstAnyMapValue(auth.Metadata, "id_token")
	if idToken == "" {
		idToken = nestedAnyMapValue(auth.Metadata, "tokens", "id_token")
	}
	if idToken != "" {
		claims, err := codexauth.ParseJWTToken(idToken)
		if err == nil && claims != nil {
			return strings.TrimSpace(claims.GetAccountID())
		}
	}
	return ""
}

func chatGPTBackendAuthAccessToken(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if value := firstAnyMapValue(auth.Metadata, "access_token", "auth_token", "bearer_token"); value != "" {
		return value
	}
	if value := nestedAnyMapValue(auth.Metadata, "tokens", "access_token", "auth_token", "bearer_token"); value != "" {
		return value
	}
	return firstStringMapValue(auth.Attributes, "access_token", "auth_token", "bearer_token")
}

func firstStringMapValue(values map[string]string, keys ...string) string {
	if len(values) == 0 {
		return ""
	}
	for _, key := range keys {
		if value := strings.TrimSpace(values[key]); value != "" {
			return value
		}
	}
	return ""
}

func firstAnyMapValue(values map[string]any, keys ...string) string {
	if len(values) == 0 {
		return ""
	}
	for _, key := range keys {
		raw := values[key]
		switch value := raw.(type) {
		case string:
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		case []byte:
			if trimmed := strings.TrimSpace(string(value)); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

func nestedAnyMapValue(values map[string]any, parent string, keys ...string) string {
	if len(values) == 0 {
		return ""
	}
	raw := values[parent]
	switch nested := raw.(type) {
	case map[string]any:
		return firstAnyMapValue(nested, keys...)
	case map[string]string:
		return firstStringMapValue(nested, keys...)
	default:
		return ""
	}
}

func copyPassthroughRequestHeaders(dst, src http.Header) {
	skip := hopByHopHeaderSet(src)
	for key, values := range src {
		if _, ok := skip[strings.ToLower(key)]; ok {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func copyPassthroughResponseHeaders(dst, src http.Header) {
	skip := hopByHopHeaderSet(src)
	for key, values := range src {
		if _, ok := skip[strings.ToLower(key)]; ok {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func hopByHopHeaderSet(headers http.Header) map[string]struct{} {
	result := make(map[string]struct{}, len(chatGPTBackendHopByHopHeaders)+4)
	for key := range chatGPTBackendHopByHopHeaders {
		result[key] = struct{}{}
	}
	for _, connectionHeader := range headers.Values("Connection") {
		for _, token := range strings.Split(connectionHeader, ",") {
			token = strings.ToLower(strings.TrimSpace(token))
			if token != "" {
				result[token] = struct{}{}
			}
		}
	}
	return result
}

func copyPassthroughResponseBody(dst http.ResponseWriter, src io.Reader) error {
	buffer := make([]byte, 32*1024)
	flusher, canFlush := dst.(http.Flusher)
	for {
		n, err := src.Read(buffer)
		if n > 0 {
			if _, errWrite := dst.Write(buffer[:n]); errWrite != nil {
				return errWrite
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
