package openai_compat

import (
	"encoding/json"
	"net/http"

	"github.com/gin-gonic/gin"
)

// jsonUnmarshal / jsonMarshal indirections keep the type file free of extra
// imports.
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }
func jsonMarshal(v any) ([]byte, error)     { return json.Marshal(v) }

func trimSpaceBytes(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpaceByte(b[start]) {
		start++
	}
	for end > start && isSpaceByte(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpaceByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r':
		return true
	}
	return false
}

// writeError answers with the OpenAI error envelope and aborts processing.
func writeError(c *gin.Context, status int, code, message string) {
	c.AbortWithStatusJSON(status, ErrorEnvelope{Error: ErrorBody{
		Message: message,
		Type:    errorTypeFor(status),
		Code:    code,
	}})
}

// writeErrorParam is writeError with the offending parameter filled in.
func writeErrorParam(c *gin.Context, status int, code, param, message string) {
	c.AbortWithStatusJSON(status, ErrorEnvelope{Error: ErrorBody{
		Message: message,
		Type:    errorTypeFor(status),
		Param:   param,
		Code:    code,
	}})
}

// errorTypeFor maps HTTP status to the OpenAI error.type vocabulary.
func errorTypeFor(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "invalid_request_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusConflict:
		return "invalid_request_error"
	default:
		return "server_error"
	}
}
