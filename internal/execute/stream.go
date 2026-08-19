package execute

import (
	"context"
	"sync"
	"time"

	"github.com/Shashank-Panda/relay/internal/provider"
)

// release owns the cancellation of one derived context.
//
// It exists because a streaming request has two different lifetimes stacked on
// one context, and the standard library has no type for that. Getting the stream
// open must be bounded — a provider that accepts a connection and then says
// nothing would otherwise hold the request forever, and the retry budget exists
// precisely so that case moves on. Generating the answer must not be bounded the
// same way: a long response legitimately takes minutes, and a timer that fires
// mid-generation terminates a working stream and presents at the client as a
// truncated answer with no error.
//
// context.WithTimeout cannot express that, because there is no way to stand its
// timer down without cancelling the context it guards. So the context is a plain
// WithCancel plus an AfterFunc that can be stopped: disarm at the moment the
// stream opens, leaving a context that still dies with the request and no longer
// dies with the clock.
type release struct {
	timer *time.Timer

	once   sync.Once
	cancel context.CancelFunc
}

// bounded derives a context that cancels after d, or with its parent.
func bounded(ctx context.Context, d time.Duration) (context.Context, *release) {
	inner, cancel := context.WithCancel(ctx)
	r := &release{cancel: cancel}
	if d > 0 {
		r.timer = time.AfterFunc(d, cancel)
	}
	return inner, r
}

// disarm stops the timeout without cancelling. Called once a stream is open.
func (r *release) disarm() {
	if r != nil && r.timer != nil {
		r.timer.Stop()
	}
}

// close cancels and stops the timer. Idempotent, because it is reached both
// from the executor's error paths and from a caller's Close — which callers are
// told to call on every path and therefore sometimes call twice.
func (r *release) close() {
	if r == nil {
		return
	}
	r.once.Do(func() {
		if r.timer != nil {
			r.timer.Stop()
		}
		if r.cancel != nil {
			r.cancel()
		}
	})
}

// attemptContext derives a bounded context for one attempt.
func (r *run) attemptContext(ctx context.Context) (context.Context, *release) {
	return bounded(ctx, r.pol.AttemptTimeout)
}

// managedStream ties a provider stream's lifetime to the contexts opened for it.
//
// Both of them. The attempt timeout and the request's total deadline are each
// derived contexts with a live cancel, and neither may fire while a stream is
// being read — but both must fire when the caller is done with it, or every
// completed stream leaks a context and its timer for the life of the process.
//
// This is why Run cannot simply `defer cancel()`. On the non-streaming path that
// is correct and the response is already in hand; on the streaming path it
// cancels the stream it just opened, which shows up as every stream terminating
// the instant execution returns.
type managedStream struct {
	provider.Stream
	releases []*release
}

func (s *managedStream) Close() error {
	err := s.Stream.Close()
	for _, r := range s.releases {
		r.close()
	}
	return err
}

// adopt hands a second context's lifetime to the stream.
func (s *managedStream) adopt(r *release) {
	r.disarm()
	s.releases = append(s.releases, r)
}
