package wire

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// StreamWriter emits an OpenAI-compatible SSE stream.
type StreamWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController

	// started records that headers went out and at least one frame was
	// flushed. After that point the status code is fixed and an error can only
	// be delivered inside the stream — see architecture §5.
	started bool
	err     error
}

// NewStreamWriter sets the streaming headers. It does not write anything yet:
// the status code stays open until the first frame, so a provider that fails
// before its first token still produces a normal JSON error response.
func NewStreamWriter(w http.ResponseWriter) *StreamWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")

	// Without this, nginx and most CDNs buffer the response and deliver it in
	// one piece at the end. The stream still "works" and time-to-first-token
	// becomes time-to-last-token, which is the whole value of streaming gone
	// with no error anywhere.
	h.Set("X-Accel-Buffering", "no")

	return &StreamWriter{w: w, rc: http.NewResponseController(w)}
}

// Started reports whether any frame has reached the client.
//
// The pre/post-first-byte boundary depends on this: before it, Relay may
// abandon an attempt and produce a clean error; after it, the client has
// already committed to a response and the only honest option is to terminate
// the stream.
func (s *StreamWriter) Started() bool { return s.started }

// Err returns the first write failure, which is almost always the client having
// disconnected.
func (s *StreamWriter) Err() error { return s.err }

// Send writes one frame and flushes it.
//
// Flushing per frame is the point of the exercise. A buffered writer that
// flushes when its buffer fills would batch tokens into clumps, which reads as
// a stuttering response and destroys the latency characteristic being paid for.
func (s *StreamWriter) Send(v any) error {
	if s.err != nil {
		return s.err
	}
	buf, err := json.Marshal(v)
	if err != nil {
		s.err = err
		return err
	}
	return s.raw("data: " + string(buf) + "\n\n")
}

// Done writes the terminator. Every OpenAI SDK waits for it; a stream that just
// closes the connection presents as a truncated response.
func (s *StreamWriter) Done() error {
	return s.raw("data: [DONE]\n\n")
}

// Comment writes an SSE comment, used as a heartbeat to stop intermediaries
// closing a connection that is waiting on a slow first token.
func (s *StreamWriter) Comment(text string) error {
	return s.raw(": " + text + "\n\n")
}

// Error terminates a stream that has already started.
//
// It emits an error frame followed by [DONE], because the alternative — closing
// the connection — is indistinguishable at the client from a network fault, and
// a client that cannot tell those apart will retry a request that is guaranteed
// to fail again.
func (s *StreamWriter) Error(message, errType, code string) {
	if s.err != nil {
		return
	}
	_ = s.Send(errorBody(message, errType, code))
	_ = s.Done()
}

func (s *StreamWriter) raw(frame string) error {
	if s.err != nil {
		return s.err
	}
	if _, err := fmt.Fprint(s.w, frame); err != nil {
		s.err = err
		return err
	}
	s.started = true

	// A flush failure means the client is gone. Recorded, not returned as a
	// distinct case: the caller's next Send reports it, and the request context
	// is already being cancelled by the server.
	if err := s.rc.Flush(); err != nil {
		s.err = err
		return err
	}
	return nil
}
