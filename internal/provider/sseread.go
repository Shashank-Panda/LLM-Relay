package provider

import (
	"bufio"
	"io"
	"strings"
)

// sseInitialBuffer is the starting read buffer. It grows as needed; this is
// only the allocation that avoids growing for typical frames.
const sseInitialBuffer = 32 << 10

// Event is one server-sent event.
type Event struct {
	// Name is the SSE "event:" field. Empty for providers that send only data
	// frames, which is most of them.
	Name string

	// Data is the concatenation of every "data:" line in the frame, joined by
	// newlines as the SSE specification requires.
	Data string
}

// EventReader parses an SSE stream.
//
// It reads with bufio.Reader.ReadString rather than bufio.Scanner, and that is
// not a stylistic preference. Scanner has a 64KB token limit by default, and
// exceeding it returns bufio.ErrTooLong — which, in the common shape where a
// stream loop treats any read error as end-of-stream, presents as a response
// that simply stops early with no error anywhere. The payloads that exceed 64KB
// are exactly the valuable ones: large tool-call arguments and echoed base64
// images. ReadString has no such limit.
type EventReader struct {
	r    *bufio.Reader
	body io.Closer

	name string
	data []string
}

func NewEventReader(body io.ReadCloser) *EventReader {
	return &EventReader{
		r:    bufio.NewReaderSize(body, sseInitialBuffer),
		body: body,
	}
}

// Next returns the next complete event, or io.EOF.
//
// Comment lines (":" heartbeats, which providers and proxies send to keep
// intermediaries from timing the connection out) are consumed and never
// surfaced. So are unrecognised fields — id and retry — which Relay does not
// use and which must not become parse errors.
func (e *EventReader) Next() (Event, error) {
	e.name = ""
	e.data = e.data[:0]

	for {
		line, err := e.r.ReadString('\n')
		if err != nil {
			// A final frame with no trailing newline is still a frame. Providers
			// do close streams this way, and discarding the last event would
			// drop the chunk that usually carries usage.
			//
			// The trailing bytes must be consumed before the check: on the last
			// frame they are the only thing that has arrived, so testing for
			// accumulated data first would always find none and drop it.
			if err == io.EOF {
				e.consumeLine(line)
				if len(e.data) > 0 || e.name != "" {
					return e.event(), nil
				}
			}
			return Event{}, err
		}

		line = strings.TrimRight(line, "\r\n")

		// A blank line terminates the frame.
		if line == "" {
			if len(e.data) > 0 || e.name != "" {
				return e.event(), nil
			}
			// Leading or repeated blank lines between frames are legal padding.
			continue
		}

		e.consumeLine(line)
	}
}

func (e *EventReader) consumeLine(line string) {
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return
	}
	// ":" opens a comment. Heartbeats look like ": keep-alive".
	if strings.HasPrefix(line, ":") {
		return
	}

	field, value, found := strings.Cut(line, ":")
	if !found {
		// A field with no colon is a field with an empty value, per spec.
		field, value = line, ""
	}
	// Exactly one leading space after the colon is part of the framing, not the
	// data. Stripping more would corrupt JSON that legitimately begins with
	// whitespace.
	value = strings.TrimPrefix(value, " ")

	switch field {
	case "data":
		e.data = append(e.data, value)
	case "event":
		e.name = value
	}
}

func (e *EventReader) event() Event {
	return Event{Name: e.name, Data: strings.Join(e.data, "\n")}
}

// Close releases the underlying body.
func (e *EventReader) Close() error {
	if e.body == nil {
		return nil
	}
	return e.body.Close()
}

// LineReader reads newline-delimited JSON, which is what Ollama speaks.
//
// Same buffer reasoning as EventReader: no scanner, no size limit.
type LineReader struct {
	r    *bufio.Reader
	body io.Closer
}

func NewLineReader(body io.ReadCloser) *LineReader {
	return &LineReader{
		r:    bufio.NewReaderSize(body, sseInitialBuffer),
		body: body,
	}
}

// Next returns the next non-empty line, or io.EOF.
func (l *LineReader) Next() (string, error) {
	for {
		line, err := l.r.ReadString('\n')
		line = strings.TrimSpace(line)
		if err != nil {
			if err == io.EOF && line != "" {
				return line, nil
			}
			return "", err
		}
		if line == "" {
			continue
		}
		return line, nil
	}
}

func (l *LineReader) Close() error {
	if l.body == nil {
		return nil
	}
	return l.body.Close()
}
