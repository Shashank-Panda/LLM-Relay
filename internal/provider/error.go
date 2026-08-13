package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Error is a provider failure carrying everything the executor needs to decide
// what happens next, and everything the caller needs to understand what went
// wrong.
//
// It deliberately does not carry the request. An error value tends to end up in
// a log line eventually, and a request contains the customer's prompt — see
// architecture §1 on data handling. What is safe to log is here; what is not
// stays out of reach.
type Error struct {
	Provider string
	Endpoint string

	Class      ErrorClass
	HTTPStatus int

	// Message is the provider's own description, truncated. Provider error
	// bodies are the one upstream string worth surfacing verbatim: "you sent
	// 200k tokens to a 128k model" is more useful than anything Relay could
	// paraphrase.
	Message string

	// RetryAfter is honoured verbatim when the provider sets it. A gateway that
	// substitutes its own backoff for a stated one is guessing against a number
	// the provider already knows.
	RetryAfter time.Duration

	// Err is the underlying transport error, if the failure was not an HTTP
	// response at all.
	Err error
}

func (e *Error) Error() string {
	var b strings.Builder
	b.WriteString(e.Provider)
	if e.Endpoint != "" {
		b.WriteString(" ")
		b.WriteString(e.Endpoint)
	}
	fmt.Fprintf(&b, ": %s", e.Class)
	if e.HTTPStatus > 0 {
		fmt.Fprintf(&b, " (http %d)", e.HTTPStatus)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	if e.Err != nil {
		b.WriteString(": ")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *Error) Unwrap() error { return e.Err }

// ClassOf extracts the error class from any error, defaulting to Terminal.
//
// Terminal is the safe default precisely because it is the restrictive one: an
// unrecognised failure that gets retried three times costs money for nothing,
// whereas one that fails fast costs a request. When in doubt, do not spend.
func ClassOf(err error) ErrorClass {
	var pe *Error
	if errors.As(err, &pe) {
		return pe.Class
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ClassCancelled
	}
	return ClassTerminal
}

// HTTPStatusOf returns the status a caller should see for this error.
func HTTPStatusOf(err error) int {
	var pe *Error
	if errors.As(err, &pe) && pe.HTTPStatus > 0 {
		return pe.HTTPStatus
	}
	switch ClassOf(err) {
	case ClassCancelled:
		// 499 is nginx's non-standard "client closed request". It never reaches
		// a client — by definition the client is gone — but it keeps cancelled
		// requests out of the 5xx error budget, where they would look like
		// Relay failing.
		return 499
	case ClassRetrySame, ClassRetryOther:
		return http.StatusBadGateway
	default:
		return http.StatusBadRequest
	}
}

// classifyStatus is the default HTTP status mapping, shared by adapters that
// have no vendor-specific rule to add.
func classifyStatus(status int) ErrorClass {
	switch {
	case status == http.StatusTooManyRequests:
		return ClassRetrySame
	case status == http.StatusRequestTimeout:
		return ClassRetrySame
	case status >= 500:
		return ClassRetrySame
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		// Not retryable anywhere: a bad key is bad on the next attempt too, and
		// hammering an auth endpoint is how you get an account flagged.
		return ClassTerminal
	case status >= 400:
		return ClassTerminal
	}
	return ClassTerminal
}

// ClassifyTransport handles the errors that arrive without an HTTP response:
// dial failures, resets, and cancellation.
func ClassifyTransport(err error) ErrorClass {
	if err == nil {
		return ClassTerminal
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ClassCancelled
	}
	// A connection that failed to establish or died mid-flight says nothing
	// about the request's validity, so the same endpoint is worth one more try.
	return ClassRetrySame
}

// DefaultClassify implements the common case of Adapter.ClassifyError.
func DefaultClassify(resp *http.Response, err error) ErrorClass {
	if err != nil {
		return ClassifyTransport(err)
	}
	if resp == nil {
		return ClassTerminal
	}
	return classifyStatus(resp.StatusCode)
}

// ParseRetryAfter reads the header in both of its permitted forms: delay in
// seconds, or an HTTP date.
func ParseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// maxErrorBody bounds how much of a provider error body is read.
//
// Error paths are exactly where a hostile or broken upstream would send an
// unbounded body, and it would be read into memory on a request that has
// already failed.
const maxErrorBody = 8 << 10

// truncate keeps error messages loggable.
func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
