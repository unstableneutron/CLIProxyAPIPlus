package handlers

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ForceModelPrefixHeader is a request-scoped routing override for clients that
// hardcode model names. When present, the prefix is applied before model lookup,
// so existing prefixed-auth routing is reused and selected auths still strip the
// prefix before the upstream executor call.
const ForceModelPrefixHeader = "X-Force-Model-Prefix"

// ApplyForceModelPrefixHeader applies X-Force-Model-Prefix to the JSON model
// field. Header values may include leading/trailing slashes, but embedded
// slashes are rejected because credential prefixes are single path segments.
func ApplyForceModelPrefixHeader(c *gin.Context, rawJSON []byte) ([]byte, *interfaces.ErrorMessage) {
	prefix, err := forceModelPrefixFromContext(c)
	if err != nil {
		return rawJSON, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: err}
	}
	if prefix == "" || len(rawJSON) == 0 {
		return rawJSON, nil
	}

	model := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
	if model == "" {
		return rawJSON, nil
	}

	forcedModel := forceModelPrefixForModel(prefix, model)
	if forcedModel == model {
		return rawJSON, nil
	}
	updated, err := sjson.SetBytes(rawJSON, "model", forcedModel)
	if err != nil {
		return rawJSON, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: fmt.Errorf("failed to apply %s: %w", ForceModelPrefixHeader, err)}
	}
	return updated, nil
}

// ApplyForceModelPrefixToModel applies X-Force-Model-Prefix to a parsed model
// value. Use this for entrypoints where the model comes from a path or multipart
// form field rather than a JSON body.
func ApplyForceModelPrefixToModel(c *gin.Context, model string) (string, *interfaces.ErrorMessage) {
	prefix, err := forceModelPrefixFromContext(c)
	if err != nil {
		return model, &interfaces.ErrorMessage{StatusCode: http.StatusBadRequest, Error: err}
	}
	model = strings.TrimSpace(model)
	if prefix == "" || model == "" {
		return model, nil
	}
	return forceModelPrefixForModel(prefix, model), nil
}

func forceModelPrefixFromContext(c *gin.Context) (string, error) {
	if c == nil {
		return "", nil
	}
	raw := strings.TrimSpace(c.GetHeader(ForceModelPrefixHeader))
	if raw == "" {
		return "", nil
	}
	prefix := strings.TrimSpace(strings.Trim(raw, "/"))
	if prefix == "" {
		return "", fmt.Errorf("%s must contain a non-empty prefix", ForceModelPrefixHeader)
	}
	if strings.Contains(prefix, "/") {
		return "", fmt.Errorf("%s must be a single model prefix segment", ForceModelPrefixHeader)
	}
	return prefix, nil
}

func forceModelPrefixForModel(prefix, model string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	model = strings.TrimSpace(model)
	if prefix == "" || model == "" || model == prefix || strings.HasPrefix(model, prefix+"/") {
		return model
	}
	return prefix + "/" + model
}
