package meter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileSink appends records as newline-delimited JSON.
//
// This is the durable half of the ledger and, per ADR-0005, it is also the
// spill path the data plane is required to have: when Postgres arrives in Phase
// 7 it becomes another sink behind this same interface, and this one keeps
// running so that "control plane unreachable" degrades metering durability
// rather than availability.
//
// JSONL rather than a database: the file is appendable without coordination,
// survives a crash mid-write with at most one truncated line, and can be read
// by anything. At Phase 2 volumes a query is a scan, and a scan is enough to
// produce a monthly figure.
type FileSink struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer

	// syncEvery forces an fsync after this many records. Zero disables it.
	//
	// Two different durability levels, deliberately separated:
	//
	// The buffer is flushed on *every* record, so the bytes reach the OS
	// immediately. That costs one write syscall on the meter's worker goroutine
	// — which is off the request path by construction, so it costs a request
	// nothing — and it buys two things worth far more: the ledger survives a
	// process crash, and `tail -f` shows records as they happen instead of in
	// silent batches. An earlier version buffered until 64 records had
	// accumulated, which made a freshly-started gateway look like it was
	// recording nothing at all.
	//
	// fsync stays periodic, because that one really is expensive — it waits on
	// the physical device — and it only buys durability across a machine
	// failure, where losing the last few seconds of a savings ledger is an
	// acceptable trade.
	syncEvery int
	since     int
}

// OpenFile opens or creates the ledger at path.
func OpenFile(path string, syncEvery int) (*FileSink, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("meter: creating %s: %w", dir, err)
		}
	}
	// 0o600: the ledger holds per-tenant cost data. Not secrets, but not
	// world-readable either.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("meter: opening %s: %w", path, err)
	}
	return &FileSink{f: f, w: bufio.NewWriterSize(f, 64<<10), syncEvery: syncEvery}, nil
}

func (s *FileSink) Write(r Record) {
	buf, err := json.Marshal(r)
	if err != nil {
		// A record that will not serialize is a programming error, and there is
		// nowhere useful to report it from a background worker. Dropping one
		// record is better than killing the worker and losing all of them.
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	_, _ = s.w.Write(buf)
	_ = s.w.WriteByte('\n')
	// Every record reaches the OS immediately. See the syncEvery comment for
	// why this is not the expensive half.
	_ = s.w.Flush()

	s.since++
	if s.syncEvery > 0 && s.since >= s.syncEvery {
		_ = s.f.Sync()
		s.since = 0
	}
}

// Flush writes and syncs everything buffered.
func (s *FileSink) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.w.Flush(); err != nil {
		return err
	}
	s.since = 0
	return s.f.Sync()
}

// Close flushes and releases the file.
func (s *FileSink) Close() error {
	if err := s.Flush(); err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		_ = s.f.Close()
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}
