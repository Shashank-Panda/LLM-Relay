package provider

import (
	"io"
	"strings"
	"testing"
)

func reader(s string) *EventReader {
	return NewEventReader(io.NopCloser(strings.NewReader(s)))
}

func drain(t *testing.T, r *EventReader) []Event {
	t.Helper()
	var out []Event
	for {
		ev, err := r.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, ev)
	}
}

func TestEventReader(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []Event
	}{
		{
			name: "plain data frames",
			in:   "data: one\n\ndata: two\n\n",
			want: []Event{{Data: "one"}, {Data: "two"}},
		},
		{
			name: "named events",
			in:   "event: message_start\ndata: {\"a\":1}\n\n",
			want: []Event{{Name: "message_start", Data: `{"a":1}`}},
		},
		{
			// The SSE spec joins repeated data lines with newlines. Providers do
			// this for pretty-printed JSON, and a reader that keeps only the
			// last line silently truncates the payload.
			name: "multi-line data joins with newlines",
			in:   "data: {\ndata:   \"a\": 1\ndata: }\n\n",
			want: []Event{{Data: "{\n  \"a\": 1\n}"}},
		},
		{
			// Heartbeats exist to stop intermediaries closing an idle
			// connection. Surfacing them as events would produce empty chunks.
			name: "comments are consumed",
			in:   ": keep-alive\n\ndata: real\n\n",
			want: []Event{{Data: "real"}},
		},
		{
			name: "CRLF line endings",
			in:   "event: ping\r\ndata: x\r\n\r\n",
			want: []Event{{Name: "ping", Data: "x"}},
		},
		{
			name: "unknown fields are ignored, not errors",
			in:   "id: 42\nretry: 3000\ndata: x\n\n",
			want: []Event{{Data: "x"}},
		},
		{
			// Providers do close streams without a final blank line. Dropping
			// the last frame usually means dropping the usage report.
			name: "final frame without trailing blank line",
			in:   "data: last",
			want: []Event{{Data: "last"}},
		},
		{
			name: "repeated blank lines are padding",
			in:   "\n\ndata: a\n\n\n\ndata: b\n\n",
			want: []Event{{Data: "a"}, {Data: "b"}},
		},
		{
			// Exactly one space after the colon is framing. A second space is
			// data, and stripping it would corrupt payloads that begin with
			// whitespace.
			name: "only one leading space is stripped",
			in:   "data:  x\n\n",
			want: []Event{{Data: " x"}},
		},
		{
			name: "field with no colon has an empty value",
			in:   "data\n\n",
			want: []Event{{Data: ""}},
		},
		{
			name: "empty stream",
			in:   "",
			want: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := drain(t, reader(tc.in))
			if len(got) != len(tc.want) {
				t.Fatalf("got %d events %+v, want %d", len(got), got, len(tc.want))
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("event %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestEventReaderExceeds64K is the reason this package does not use
// bufio.Scanner.
//
// Scanner's default limit is 64KB and exceeding it returns bufio.ErrTooLong.
// In the usual stream loop shape — any error ends the stream — that presents as
// a response that stops early with nothing logged. The payloads that get this
// big are large tool-call arguments and echoed base64 images, so the failure
// lands precisely on the requests that matter most.
func TestEventReaderExceeds64K(t *testing.T) {
	huge := strings.Repeat("x", 300<<10)
	got := drain(t, reader("data: "+huge+"\n\n"))

	if len(got) != 1 {
		t.Fatalf("got %d events, want 1 — the reader truncated a large frame", len(got))
	}
	if len(got[0].Data) != len(huge) {
		t.Errorf("data is %d bytes, want %d", len(got[0].Data), len(huge))
	}
}

func TestEventReaderCloses(t *testing.T) {
	c := &countingCloser{Reader: strings.NewReader("data: x\n\n")}
	r := NewEventReader(c)
	drain(t, r)
	if err := r.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if c.closed != 1 {
		t.Errorf("body closed %d times, want 1", c.closed)
	}
}

func TestLineReader(t *testing.T) {
	body := io.NopCloser(strings.NewReader("{\"a\":1}\n\n{\"b\":2}\n{\"c\":3}"))
	r := NewLineReader(body)

	var got []string
	for {
		line, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		got = append(got, line)
	}

	want := []string{`{"a":1}`, `{"b":2}`, `{"c":3}`}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestLineReaderExceeds64K(t *testing.T) {
	huge := `{"response":"` + strings.Repeat("y", 200<<10) + `"}`
	r := NewLineReader(io.NopCloser(strings.NewReader(huge + "\n")))

	line, err := r.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if len(line) != len(huge) {
		t.Errorf("line is %d bytes, want %d", len(line), len(huge))
	}
}

type countingCloser struct {
	io.Reader
	closed int
}

func (c *countingCloser) Close() error {
	c.closed++
	return nil
}
