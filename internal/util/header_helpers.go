package util

import (
	"net/http"
	"net/url"
	"strings"
)

// ApplyCustomHeadersFromAttrs applies user-defined headers stored in the provided attributes map.
// Custom headers override built-in defaults when conflicts occur.
func ApplyCustomHeadersFromAttrs(r *http.Request, attrs map[string]string) {
	if r == nil {
		return
	}
	applyCustomHeaders(r, extractCustomHeaders(attrs))
}

// ApplyCustomQueryParamsFromAttrs applies user-defined query parameters stored in
// the provided attributes map. Custom query parameters override existing values.
func ApplyCustomQueryParamsFromAttrs(r *http.Request, attrs map[string]string) {
	if r == nil || r.URL == nil {
		return
	}
	applyCustomQueryParams(r.URL, extractCustomQueryParams(attrs))
}

// ApplyCustomQueryParamsToURL applies user-defined query parameters to a raw URL
// string. Custom query parameters override existing values.
func ApplyCustomQueryParamsToURL(rawURL string, attrs map[string]string) (string, error) {
	params := extractCustomQueryParams(attrs)
	if len(params) == 0 {
		return rawURL, nil
	}
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return "", err
	}
	applyCustomQueryParams(parsed, params)
	return parsed.String(), nil
}

func extractCustomHeaders(attrs map[string]string) map[string]string {
	return extractCustomStringMap(attrs, "header:")
}

func extractCustomQueryParams(attrs map[string]string) map[string]string {
	return extractCustomStringMap(attrs, "query:")
}

func extractCustomStringMap(attrs map[string]string, prefix string) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	values := make(map[string]string)
	for k, v := range attrs {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(k, prefix))
		if name == "" {
			continue
		}
		val := strings.TrimSpace(v)
		if val == "" {
			continue
		}
		values[name] = val
	}
	if len(values) == 0 {
		return nil
	}
	return values
}

func applyCustomHeaders(r *http.Request, headers map[string]string) {
	if r == nil || len(headers) == 0 {
		return
	}
	for k, v := range headers {
		if k == "" || v == "" {
			continue
		}
		// net/http reads Host from req.Host (not req.Header) when writing
		// a real request, so we must mirror it there. Some callers pass
		// synthetic requests (e.g. &http.Request{Header: ...}) and only
		// consume r.Header afterwards, so keep the value in the header
		// map too.
		if http.CanonicalHeaderKey(k) == "Host" {
			r.Host = v
		}
		r.Header.Set(k, v)
	}
}

func applyCustomQueryParams(parsed *url.URL, params map[string]string) {
	if parsed == nil || len(params) == 0 {
		return
	}
	query := parsed.Query()
	for key, value := range params {
		if key == "" || value == "" {
			continue
		}
		query.Set(key, value)
	}
	parsed.RawQuery = query.Encode()
}
