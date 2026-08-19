package meter

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// Sink receives records off the hot path. Implementations may block; the worker
// that calls them may not be on a request goroutine.
type Sink interface {
	Write(Record)
}

// Flusher is an optional Sink capability, called once during shutdown.
type Flusher interface {
	Flush() error
}

// DefaultBuffer is how many records may queue before dropping begins.
//
// Sized for a burst rather than an outage. At the SLO's 1,000 rps, this is a
// few seconds of headroom — enough to ride out a slow disk write, not enough to
// hide a sink that has stopped working.
const DefaultBuffer = 4096

// Meter is the metering pipeline.
//
// One worker, one buffered channel, several sinks. The single worker is
// deliberate: records arrive in request order and a pool would interleave them,
// which makes a JSONL ledger harder to reason about for no throughput anyone
// needs — serializing a struct is nanoseconds.
type Meter struct {
	ch    chan Record
	sinks []Sink

	dropped atomic.Uint64
	written atomic.Uint64

	// closed, stop, and done implement shutdown without ever closing ch.
	//
	// The obvious implementation closes the record channel and lets the worker's
	// range loop end. It is also a panic waiting for the right moment: a request
	// still in flight during shutdown sends on a closed channel and takes the
	// process down — turning a graceful drain into a crash, and violating the
	// one rule this whole system is arranged around, that an internal failure
	// must never become the caller's error (ADR-0010). A separate stop signal
	// has no such window: a late send lands in the buffer and is simply never
	// read.
	closed   atomic.Bool
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func New(buffer int, sinks ...Sink) *Meter {
	if buffer <= 0 {
		buffer = DefaultBuffer
	}
	m := &Meter{
		ch:    make(chan Record, buffer),
		sinks: sinks,
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go m.run()
	return m
}

func (m *Meter) run() {
	defer close(m.done)
	for {
		select {
		case r := <-m.ch:
			m.write(r)
		case <-m.stop:
			// Drain what is already queued before exiting. Records for requests
			// that were in flight when shutdown began are already in the buffer,
			// and discarding them would silently undercount the last few seconds
			// of the ledger on every deploy.
			for {
				select {
				case r := <-m.ch:
					m.write(r)
				default:
					return
				}
			}
		}
	}
}

func (m *Meter) write(r Record) {
	for _, s := range m.sinks {
		s.Write(r)
	}
	m.written.Add(1)
}

// Record queues a record, dropping it if the buffer is full.
//
// The non-blocking send is the whole point. A blocking send would let a stalled
// disk or a slow database add latency to inference, which inverts the system's
// priorities: the request is mandatory and the telemetry about it is not. A
// dropped record is a counter to alert on, and Dropped() exists so the loss is
// never silent.
func (m *Meter) Record(r Record) {
	if m == nil {
		return
	}
	if m.closed.Load() {
		// Recorded after shutdown drained. The request still completed — that is
		// the point — and its ledger entry is counted as dropped rather than
		// silently discarded.
		m.dropped.Add(1)
		return
	}
	if r.At.IsZero() {
		// Stamped by the caller in normal use so the record reflects when the
		// request happened rather than when the queue drained.
		r.At = time.Now()
	}
	select {
	case m.ch <- r:
	default:
		m.dropped.Add(1)
	}
}

// Dropped counts records lost to a full buffer. A non-zero and rising value
// means the savings ledger is incomplete, which is a correctness problem for
// the report and not merely a monitoring one.
func (m *Meter) Dropped() uint64 {
	if m == nil {
		return 0
	}
	return m.dropped.Load()
}

// Written counts records delivered to every sink.
func (m *Meter) Written() uint64 {
	if m == nil {
		return 0
	}
	return m.written.Load()
}

// Close drains the queue and flushes every sink.
//
// Called during graceful shutdown, after the HTTP server has drained, so the
// records for in-flight requests are already queued. Bounded by ctx: a sink
// that will not flush must not prevent the process from exiting.
func (m *Meter) Close(ctx context.Context) error {
	if m == nil {
		return nil
	}
	m.stopOnce.Do(func() {
		m.closed.Store(true)
		close(m.stop)
	})

	select {
	case <-m.done:
	case <-ctx.Done():
		return ctx.Err()
	}

	var firstErr error
	for _, s := range m.sinks {
		f, ok := s.(Flusher)
		if !ok {
			continue
		}
		if err := f.Flush(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// MultiSink is unnecessary — Meter already fans out — but SinkFunc is handy for
// tests and for one-line adapters.
type SinkFunc func(Record)

func (f SinkFunc) Write(r Record) { f(r) }
