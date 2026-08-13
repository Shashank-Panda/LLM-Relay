package wire

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/Shashank-Panda/relay/internal/provider"
)

// ErrorResponse is the OpenAI error envelope. SDKs parse this shape to raise
// typed exceptions, so matching it is what makes a Relay failure look like a
// provider failure to code the customer already wrote.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

type ErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
	Param   string `json:"param,omitempty"`
}

const (
	TypeInvalidRequest = "invalid_request_error"
	TypeAuthentication = "authentication_error"
	TypeRateLimit      = "rate_limit_error"
	TypeAPIError       = "api_error"
	TypeOverloaded     = "overloaded_error"
)

func errorBody(message, errType, code string) ErrorResponse {
	return ErrorResponse{Error: ErrorBody{Message: message, Type: errType, Code: code}}
}

// WriteError sends a JSON error with the given status.
func WriteError(w http.ResponseWriter, status int, message, errType, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody(message, errType, code))
}

// WriteDecodeError reports a malformed request, naming the field.
func WriteDecodeError(w http.ResponseWriter, err error) {
	var de *DecodeError
	if errors.As(err, &de) {
		body := errorBody(de.Message, TypeInvalidRequest, "")
		body.Error.Param = de.Field
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(body)
		return
	}
	WriteError(w, http.StatusBadRequest, err.Error(), TypeInvalidRequest, "")
}

// WriteProviderError surfaces an upstream failure.
//
// The provider's own message is passed through verbatim. "This model's maximum
// context length is 128000 tokens, however you requested 200847" is a better
// error than anything Relay could paraphrase, and hiding it behind a generic
// message turns a self-service fix into a support ticket.
//
// The credential never appears: provider.Error carries the reference, never the
// secret, precisely so this function cannot leak one.
func WriteProviderError(w http.ResponseWriter, err error) {
	status := provider.HTTPStatusOf(err)

	// The client is already gone; writing a body would be shouting into a
	// closed socket. The status is recorded for metrics and nothing is sent.
	if status == 499 {
		w.WriteHeader(499)
		return
	}

	var pe *provider.Error
	if errors.As(err, &pe) {
		if pe.RetryAfter > 0 {
			w.Header().Set("Retry-After", formatSeconds(pe.RetryAfter.Seconds()))
		}
		WriteError(w, status, pe.Message, typeForStatus(status), string(pe.Class))
		return
	}

	WriteError(w, status, err.Error(), typeForStatus(status), "")
}

func typeForStatus(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return TypeRateLimit
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return TypeAuthentication
	case status >= 500:
		return TypeAPIError
	default:
		return TypeInvalidRequest
	}
}

// formatSeconds rounds up: Retry-After is a floor, and rounding a 1.4-second
// wait down to 1 sends the retry back before the provider is ready.
func formatSeconds(f float64) string {
	n := int(math.Ceil(f))
	if n < 1 {
		n = 1
	}
	return strconv.Itoa(n)
}
