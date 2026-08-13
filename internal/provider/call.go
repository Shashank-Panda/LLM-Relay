package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// Call is one outbound provider request, shared by every adapter.
type Call struct {
	Client  *http.Client
	Method  string
	URL     string
	Headers map[string]string
	Body    any

	// Classify is the adapter's own error classifier, so vendor-specific rules
	// (Anthropic's overloaded_error, OpenAI's context_length_exceeded) apply to
	// the response before the generic status mapping does.
	Classify func(*http.Response, error) ErrorClass

	// Provider and Endpoint label errors. Never a credential.
	Provider string
	Endpoint string
}

// Do sends the request and returns the response for a 2xx.
//
// On any other status it drains a bounded prefix of the body, closes it, and
// returns an *Error. Callers therefore never have to decide whether a returned
// response needs closing on the error path — there is no response on the error
// path.
func (c Call) Do(ctx context.Context) (*http.Response, error) {
	var body io.Reader
	if c.Body != nil {
		buf, err := json.Marshal(c.Body)
		if err != nil {
			return nil, &Error{
				Provider: c.Provider, Endpoint: c.Endpoint,
				Class: ClassTerminal, Message: "encoding request", Err: err,
			}
		}
		body = bytes.NewReader(buf)
	}

	method := c.Method
	if method == "" {
		method = http.MethodPost
	}

	// NewRequestWithContext, always. This is the line that makes client
	// disconnect cancel the provider call: the context chains back to the
	// inbound request, so when the customer hangs up, the transport aborts and
	// token generation stops. Without it Relay keeps paying for output nobody
	// will ever read — the most expensive bug a gateway can have, and one that
	// is invisible in every test that does not explicitly disconnect.
	req, err := http.NewRequestWithContext(ctx, method, c.URL, body)
	if err != nil {
		return nil, &Error{
			Provider: c.Provider, Endpoint: c.Endpoint,
			Class: ClassTerminal, Message: "building request", Err: err,
		}
	}
	if c.Body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.Headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, &Error{
			Provider: c.Provider, Endpoint: c.Endpoint,
			Class: c.classify(nil, err), Err: err,
		}
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}

	return nil, c.errorFrom(resp)
}

func (c Call) classify(resp *http.Response, err error) ErrorClass {
	if c.Classify != nil {
		return c.Classify(resp, err)
	}
	return DefaultClassify(resp, err)
}

// errorFrom consumes a failed response and turns it into an *Error.
func (c Call) errorFrom(resp *http.Response) error {
	class := c.classify(resp, nil)
	retryAfter := ParseRetryAfter(resp.Header)

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	// Draining the rest lets the connection return to the pool. Skipped when
	// the body is large — reading megabytes of an error nobody will read is a
	// worse trade than closing one connection.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBody))
	resp.Body.Close()

	return &Error{
		Provider:   c.Provider,
		Endpoint:   c.Endpoint,
		Class:      class,
		HTTPStatus: resp.StatusCode,
		Message:    truncate(extractMessage(raw), 512),
		RetryAfter: retryAfter,
	}
}

// extractMessage pulls the human-readable part out of a provider error body.
//
// All three vendors nest it differently and all three sometimes return HTML
// from a load balancer instead of JSON. Falling back to the raw text is
// correct: a truncated HTML error page in a log at least says "your request
// never reached the provider", which is the useful fact.
func extractMessage(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var body struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
		Message string `json:"message"`
		Detail  string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &body); err == nil {
		switch {
		case body.Error.Message != "":
			if body.Error.Type != "" {
				return body.Error.Type + ": " + body.Error.Message
			}
			return body.Error.Message
		case body.Message != "":
			return body.Message
		case body.Detail != "":
			return body.Detail
		}
	}
	return strings.TrimSpace(string(raw))
}

// DecodeJSON reads and closes a successful response body.
func DecodeJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(v)
}

// ErrorFromBody builds an *Error for a failure discovered inside an otherwise
// successful stream — a provider that returns 200 and then emits an error
// event, which all three do under load.
func ErrorFromBody(providerID, endpoint string, class ErrorClass, message string) *Error {
	return &Error{
		Provider: providerID,
		Endpoint: endpoint,
		Class:    class,
		Message:  truncate(message, 512),
	}
}
