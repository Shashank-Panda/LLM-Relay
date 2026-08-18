package respcache

import (
	"io"
	"strings"
	"sync"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// Stream replays a stored answer as a provider.Stream.
//
// Returning a provider.Stream rather than a special-cased response is what keeps
// the cache out of the streaming handler. That handler is a hundred lines of SSE
// framing, tool-call delta assembly, cancellation, and disconnect handling; a
// second copy of it for cached answers would be a second place for every one of
// those bugs to live.
//
// One chunk per content part, not artificial token-by-token dribbling. A cache
// hit is fast and the honest thing is to look fast — simulating generation
// latency would be theatre, and a client that measures time-to-first-token would
// be measuring a number Relay made up.
func (e *Entry) Stream() provider.Stream {
	chunks := make([]*provider.Chunk, 0, len(e.Parts)+1)

	toolIndex := 0
	for _, p := range e.Parts {
		switch p.Kind {
		case domain.PartText:
			if p.Text == "" {
				continue
			}
			chunks = append(chunks, &provider.Chunk{Text: p.Text})
		case domain.PartReasoning:
			if p.Text == "" {
				continue
			}
			chunks = append(chunks, &provider.Chunk{Reasoning: p.Text})
		case domain.PartToolCall:
			// Name, ID, and arguments in a single delta for this index. The
			// re-emitter puts ID and name on the opening delta for an index and
			// arguments on the rest; one delta carrying all three is a valid
			// instance of that shape and is what every SDK assembles correctly.
			chunks = append(chunks, &provider.Chunk{ToolCall: &provider.ToolCallDelta{
				Index:     toolIndex,
				ID:        p.ToolCallID,
				Name:      p.ToolName,
				Arguments: p.Arguments,
			}})
			toolIndex++
		}
	}

	finish := e.FinishReason
	if finish == "" {
		finish = provider.FinishStop
	}
	if n := len(chunks); n > 0 {
		chunks[n-1].FinishReason = finish
	} else {
		// An empty answer is still an answer — a content filter, or a model that
		// returned nothing. The finish reason has to reach the client on
		// something.
		chunks = append(chunks, &provider.Chunk{FinishReason: finish})
	}

	usage := e.Usage
	return &replay{chunks: chunks, usage: &usage}
}

type replay struct {
	chunks []*provider.Chunk
	i      int
	usage  *provider.Usage
	closed bool
}

func (r *replay) Recv() (*provider.Chunk, error) {
	if r.closed || r.i >= len(r.chunks) {
		return nil, io.EOF
	}
	c := r.chunks[r.i]
	r.i++
	return c, nil
}

// Usage reports the stored counts.
//
// Returned unconditionally rather than only after EOF, unlike a live stream
// where the numbers do not exist until the provider sends them. Nothing depends
// on the stricter contract, and the looser one cannot mislead: these counts were
// final before this stream started.
func (r *replay) Usage() *provider.Usage { return r.usage }

func (r *replay) Close() error {
	r.closed = true
	return nil
}

// Recorder wraps a live stream and assembles the entry it would be stored as.
//
// Wrapping rather than storing afterwards, because afterwards is too late: the
// chunks have been written to the client and forgotten. This sees each one on its
// way past and costs a string append per chunk.
//
// It stores only on a clean end of stream. A cancelled or failed stream is a
// partial answer, and a partial answer in a response cache is the worst possible
// entry — it looks like a complete one to every subsequent reader.
type Recorder struct {
	provider.Stream

	mu        sync.Mutex
	text      strings.Builder
	reasoning strings.Builder
	calls     []*call
	byIndex   map[int]*call
	finish    provider.FinishReason
	done      bool

	// onComplete runs once, at clean EOF. Never on an error path.
	onComplete func(parts []domain.ContentPart, finish provider.FinishReason, u provider.Usage)
}

type call struct {
	id   string
	name string
	args strings.Builder
}

// Record wraps st so that a cleanly-completed stream is handed to onComplete.
func Record(
	st provider.Stream,
	onComplete func(parts []domain.ContentPart, finish provider.FinishReason, u provider.Usage),
) provider.Stream {
	return &Recorder{Stream: st, byIndex: map[int]*call{}, onComplete: onComplete}
}

func (r *Recorder) Recv() (*provider.Chunk, error) {
	c, err := r.Stream.Recv()
	if err == io.EOF {
		r.complete()
		return nil, err
	}
	if err != nil {
		// Any other error abandons the recording. Nothing partial is stored:
		// the alternative is a cache entry that silently truncates every
		// response it later serves.
		r.mu.Lock()
		r.done = true
		r.mu.Unlock()
		return c, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if c.Text != "" {
		r.text.WriteString(c.Text)
	}
	if c.Reasoning != "" {
		r.reasoning.WriteString(c.Reasoning)
	}
	if tc := c.ToolCall; tc != nil {
		cl, ok := r.byIndex[tc.Index]
		if !ok {
			cl = &call{}
			r.byIndex[tc.Index] = cl
			r.calls = append(r.calls, cl)
		}
		// ID and name arrive once, on the opening delta for an index; arguments
		// arrive in fragments and are concatenated in arrival order, which the
		// Stream contract guarantees yields the complete JSON.
		if tc.ID != "" {
			cl.id = tc.ID
		}
		if tc.Name != "" {
			cl.name = tc.Name
		}
		cl.args.WriteString(tc.Arguments)
	}
	if c.FinishReason != "" {
		r.finish = c.FinishReason
	}
	return c, nil
}

func (r *Recorder) complete() {
	r.mu.Lock()
	if r.done {
		r.mu.Unlock()
		return
	}
	r.done = true

	parts := make([]domain.ContentPart, 0, len(r.calls)+2)
	if s := r.reasoning.String(); s != "" {
		parts = append(parts, domain.ContentPart{Kind: domain.PartReasoning, Text: s})
	}
	if s := r.text.String(); s != "" {
		parts = append(parts, domain.ContentPart{Kind: domain.PartText, Text: s})
	}
	for _, cl := range r.calls {
		parts = append(parts, domain.ContentPart{
			Kind:       domain.PartToolCall,
			ToolCallID: cl.id,
			ToolName:   cl.name,
			Arguments:  cl.args.String(),
		})
	}
	finish, onComplete := r.finish, r.onComplete
	r.mu.Unlock()

	var usage provider.Usage
	if u := r.Stream.Usage(); u != nil {
		usage = *u
	}

	// An answer with nothing in it is not worth an entry, and storing one would
	// let a provider hiccup poison a key for the whole TTL.
	if len(parts) == 0 || onComplete == nil {
		return
	}
	onComplete(parts, finish, usage)
}
