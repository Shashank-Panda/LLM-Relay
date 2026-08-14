package server

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Shashank-Panda/relay/internal/wire"
)

type ctxKey int

const ctxRequestID ctxKey = 0

// HeaderRequestID lets a caller supply their own correlation ID so their traces
// and Relay's line up. Relay generates one when they do not.
const HeaderRequestID = "X-Request-Id"

// requestIDs is a process-local counter.
//
// Deliberately not random: Phase 1 has no distributed tracing to correlate
// with, and a monotonic per-process ID combined with the start timestamp is
// unique enough to find a request in a log while staying reproducible in tests.
// Phase 6 replaces this with the OpenTelemetry trace ID, which is the identifier
// that will actually matter.
var requestIDs atomic.Uint64

func newRequestID(start time.Time) string {
	return strconv.FormatInt(start.UnixMilli(), 36) + "-" + strconv.FormatUint(requestIDs.Add(1), 36)
}

// RequestID returns the ID assigned to this request, or "" outside the chain.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxRequestID).(string)
	return id
}

// statusRecorder captures what was actually sent.
type statusRecorder struct {
	http.ResponseWriter
	status  int
	written int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.written += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer's Flush.
// Without it every streaming response silently buffers, because the controller
// cannot find a Flusher through the wrapper.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// started reports whether any byte has reached the client. After that the
// status is fixed and an error can only be delivered inside the response body.
func (r *statusRecorder) started() bool { return r.status != 0 }

// withRequestID assigns or adopts a correlation ID and echoes it back.
func withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if id == "" {
			id = newRequestID(time.Now())
		}
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

// withRecovery turns a panic into a response instead of a dropped connection.
//
// The branch on rec.started() is the load-bearing part. Once a stream has begun
// the status code is already sent and the body is mid-SSE-frame; writing a JSON
// error into it produces a document no client can parse. So a panic before the
// first byte becomes a clean 500, and a panic after it terminates the stream
// with an error event — see architecture §5.
func withRecovery(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rec, ok := w.(*statusRecorder)
			if !ok {
				rec = &statusRecorder{ResponseWriter: w}
				w = rec
			}

			defer func() {
				v := recover()
				if v == nil {
					return
				}
				// Deliberately not http.ErrAbortHandler-aware beyond this: that
				// panic is the standard library's way of saying the connection
				// is already gone, and there is nothing to write to.
				if v == http.ErrAbortHandler {
					panic(v)
				}

				log.ErrorContext(r.Context(), "panic serving request",
					"request_id", RequestID(r.Context()),
					"path", r.URL.Path,
					"panic", v,
				)

				if rec.started() {
					sw := wire.NewStreamWriter(rec)
					sw.Error("internal error", wire.TypeAPIError, "")
					return
				}
				wire.WriteError(rec, http.StatusInternalServerError,
					"internal error", wire.TypeAPIError, "")
			}()

			next.ServeHTTP(w, r)
		})
	}
}

// withAccessLog emits one structured line per request.
//
// It records identity, timing, and outcome. It never records prompt or response
// content — that is the most sensitive data flowing through the system, and
// logging it creates a compliance liability far easier to avoid than to unwind.
// See architecture §1.
func withAccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			rec, ok := w.(*statusRecorder)
			if !ok {
				rec = &statusRecorder{ResponseWriter: w}
				w = rec
			}

			next.ServeHTTP(w, r)

			status := rec.status
			if status == 0 {
				status = http.StatusOK
			}

			level := slog.LevelInfo
			if status >= 500 {
				level = slog.LevelError
			}

			log.LogAttrs(r.Context(), level, "request",
				slog.String("request_id", RequestID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", status),
				slog.Int64("bytes", rec.written),
				slog.Duration("duration", time.Since(start)),
			)
		})
	}
}

// wrapWriter installs the recorder once, so the recovery and access-log
// middleware share one view of what was written.
func wrapWriter(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&statusRecorder{ResponseWriter: w}, r)
	})
}

// chain applies middleware so that the first listed is the outermost.
func chain(h http.Handler, mw ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mw) - 1; i >= 0; i-- {
		h = mw[i](h)
	}
	return h
}
