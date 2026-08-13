package providertest

import (
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// collected is a whole stream reassembled the way a client would.
type collected struct {
	text      string
	reasoning string
	finish    provider.FinishReason
}

// collectedCall is one tool call rebuilt from its deltas.
type collectedCall struct {
	index int
	id    string
	name  string
	args  string

	// identityDeltas counts how many deltas carried the id or name. Providers
	// send identity once, on the opening delta for an index; an adapter that
	// repeats it produces a stream some SDKs assemble into duplicate calls.
	identityDeltas int
}

// collect drains a stream into the pieces a client would assemble.
//
// Deliberately written the way a naive but correct client would write it —
// concatenating fragments in arrival order, keyed by index — because that is
// the contract adapters owe. If reassembly needs anything cleverer than this,
// the adapter has pushed its problem downstream.
func collect(t *testing.T, a provider.Adapter, ep *domain.ModelEndpoint) (collected, []collectedCall, provider.Usage) {
	t.Helper()

	st, err := a.ChatStream(t.Context(), request(), ep, provider.Credential{})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer st.Close()

	var (
		out   collected
		text  strings.Builder
		think strings.Builder
		byIdx = map[int]*collectedCall{}
		order []int
	)

	for {
		c, err := st.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}

		text.WriteString(c.Text)
		think.WriteString(c.Reasoning)
		if c.FinishReason != "" {
			out.finish = c.FinishReason
		}

		if d := c.ToolCall; d != nil {
			call, ok := byIdx[d.Index]
			if !ok {
				call = &collectedCall{index: d.Index}
				byIdx[d.Index] = call
				order = append(order, d.Index)
			}
			if d.ID != "" || d.Name != "" {
				call.identityDeltas++
			}
			if d.ID != "" {
				call.id = d.ID
			}
			if d.Name != "" {
				call.name = d.Name
			}
			call.args += d.Arguments
		}
	}

	out.text = text.String()
	out.reasoning = think.String()

	calls := make([]collectedCall, 0, len(order))
	for _, i := range order {
		calls = append(calls, *byIdx[i])
	}

	usage := provider.Usage{}
	if u := st.Usage(); u != nil {
		usage = *u
	}
	return out, calls, usage
}

func textOf(parts []domain.ContentPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Kind == domain.PartText {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

func callsOf(parts []domain.ContentPart) []domain.ContentPart {
	var out []domain.ContentPart
	for _, p := range parts {
		if p.Kind == domain.PartToolCall {
			out = append(out, p)
		}
	}
	return out
}

// sameJSON compares two documents by structure.
//
// Byte equality would be the wrong assertion: one provider sends arguments as a
// string and another as an object, so exactly one adapter has to re-encode, and
// Go's encoder writes keys in its own order with its own spacing. What the
// contract requires is that the *document* survives, not its formatting.
func sameJSON(got, want string) bool {
	var a, b any
	if err := json.Unmarshal([]byte(got), &a); err != nil {
		return false
	}
	if err := json.Unmarshal([]byte(want), &b); err != nil {
		return false
	}
	return reflect.DeepEqual(a, b)
}
