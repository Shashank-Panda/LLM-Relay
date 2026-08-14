package anthropic

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

const ProviderID domain.ProviderID = "anthropic"

const (
	defaultBaseURL = "https://api.anthropic.com/v1"

	// apiVersion is required on every request. Anthropic pins breaking changes
	// behind it, so it is a constant rather than configuration: the code here
	// is written against this version and would need changing along with it.
	apiVersion = "2023-06-01"
)

type Adapter struct {
	client *http.Client
}

func New(client *http.Client) *Adapter {
	if client == nil {
		client = provider.NewClient(provider.DefaultClientOptions())
	}
	return &Adapter{client: client}
}

func (a *Adapter) ID() domain.ProviderID { return ProviderID }

// ClassifyError reads Anthropic's error type before falling back to the status.
func (a *Adapter) ClassifyError(resp *http.Response, err error) provider.ErrorClass {
	if err != nil {
		return provider.ClassifyTransport(err)
	}
	if resp == nil {
		return provider.ClassTerminal
	}

	switch t := peekErrorType(resp); t {
	case "overloaded_error":
		// Anthropic returns this as a 529, which is outside the range the
		// generic mapping knows about, and it is explicitly transient.
		return provider.ClassRetrySame
	case "rate_limit_error":
		return provider.ClassRetrySame
	case "invalid_request_error":
		// A context overflow is a statement that the constraint set used for
		// routing was wrong, not that the request is bad. Anthropic says so in
		// the message rather than in a code field.
		if isContextOverflow(resp) {
			return provider.ClassReroute
		}
		return provider.ClassTerminal
	case "api_error":
		return provider.ClassRetrySame
	}

	if resp.StatusCode == 529 {
		return provider.ClassRetrySame
	}
	return provider.DefaultClassify(resp, err)
}

// peekErrorType reads the error type without consuming the body, so the generic
// error path can still render the provider's own message.
func peekErrorType(resp *http.Response) string {
	body, ok := peekBody(resp)
	if !ok {
		return ""
	}
	var parsed errorBody
	if json.Unmarshal(body, &parsed) != nil {
		return ""
	}
	return parsed.Error.Type
}

func isContextOverflow(resp *http.Response) bool {
	body, ok := peekBody(resp)
	if !ok {
		return false
	}
	s := strings.ToLower(string(body))
	return strings.Contains(s, "prompt is too long") ||
		strings.Contains(s, "max_tokens") && strings.Contains(s, "context")
}

// peekBody reads a bounded prefix and puts it back.
//
// The read bytes are re-prefixed onto the body so a second peek, and the
// generic error renderer after it, still see the whole document. A classifier
// that consumed the body would leave every Anthropic failure reported with an
// empty message.
func peekBody(resp *http.Response) ([]byte, bool) {
	if resp.Body == nil {
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return nil, false
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(strings.NewReader(string(raw)), resp.Body), resp.Body}
	return raw, true
}

func (a *Adapter) call(ctx context.Context, req *domain.NormalizedRequest, ep *domain.ModelEndpoint, cred provider.Credential, stream bool) (*http.Response, error) {
	base := ep.BaseURL
	if base == "" {
		base = defaultBaseURL
	}

	// Anthropic authenticates with x-api-key, not a bearer token.
	headers := map[string]string{
		"x-api-key":         cred.APIKey,
		"anthropic-version": apiVersion,
	}
	for k, v := range cred.Headers {
		headers[k] = v
	}

	return provider.Call{
		Client:   a.client,
		URL:      base + "/messages",
		Headers:  headers,
		Body:     encodeRequest(req, ep, stream),
		Classify: a.ClassifyError,
		Provider: string(ProviderID),
		Endpoint: ep.ID,
	}.Do(ctx)
}

func (a *Adapter) Chat(ctx context.Context, req *domain.NormalizedRequest, ep *domain.ModelEndpoint, cred provider.Credential) (*provider.Response, error) {
	httpResp, err := a.call(ctx, req, ep, cred, false)
	if err != nil {
		return nil, err
	}

	var body messagesResponse
	if err := provider.DecodeJSON(httpResp, &body); err != nil {
		return nil, &provider.Error{
			Provider: string(ProviderID), Endpoint: ep.ID,
			Class: provider.ClassRetrySame, Message: "decoding response", Err: err,
		}
	}

	var parts []domain.ContentPart
	for _, b := range body.Content {
		switch b.Type {
		case "text":
			parts = append(parts, domain.ContentPart{Kind: domain.PartText, Text: b.Text})
		case "thinking":
			parts = append(parts, domain.ContentPart{Kind: domain.PartReasoning, Text: b.Thinking})
		case "tool_use":
			parts = append(parts, domain.ContentPart{
				Kind:       domain.PartToolCall,
				ToolCallID: b.ID,
				ToolName:   b.Name,
				// The input arrives as an object; every other provider speaks
				// the string form, so it is flattened here rather than at each
				// consumer.
				Arguments: string(b.Input),
			})
		}
	}

	return &provider.Response{
		ID:           body.ID,
		Model:        body.Model,
		Parts:        parts,
		FinishReason: finishReason(body.StopReason),
		Usage:        usageFrom(&body.Usage),
	}, nil
}

func (a *Adapter) ChatStream(ctx context.Context, req *domain.NormalizedRequest, ep *domain.ModelEndpoint, cred provider.Credential) (provider.Stream, error) {
	httpResp, err := a.call(ctx, req, ep, cred, true)
	if err != nil {
		return nil, err
	}
	return &stream{
		events:   provider.NewEventReader(httpResp.Body),
		endpoint: ep.ID,
		blocks:   map[int]*blockState{},
	}, nil
}

// blockState tracks one open content block.
//
// Anthropic's stream is a state machine, not a sequence of self-describing
// deltas: a content_block_delta says only "index 0 got these bytes", and what
// those bytes mean depends on the content_block_start that opened index 0. So
// the adapter has to remember.
type blockState struct {
	kind string

	// toolIndex is the position among tool calls specifically, which is what
	// the OpenAI shape indexes by. Anthropic's index counts all blocks, so a
	// response with text before a tool call would otherwise emit a tool call at
	// index 1 with nothing at index 0 — a gap that some clients mis-assemble.
	toolIndex int

	identitySent bool
}

type stream struct {
	events   *provider.EventReader
	endpoint string

	blocks    map[int]*blockState
	toolCount int

	usage  provider.Usage
	closed bool
}

func (s *stream) Recv() (*provider.Chunk, error) {
	for {
		ev, err := s.events.Next()
		if err != nil {
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, &provider.Error{
				Provider: string(ProviderID), Endpoint: s.endpoint,
				Class: provider.ClassifyTransport(err), Message: "reading stream", Err: err,
			}
		}

		data := strings.TrimSpace(ev.Data)
		if data == "" {
			continue
		}

		var e streamEvent
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			return nil, &provider.Error{
				Provider: string(ProviderID), Endpoint: s.endpoint,
				Class: provider.ClassRetrySame, Message: "malformed stream event", Err: err,
			}
		}

		// The event name is carried in both the SSE event: field and the JSON
		// body. The body wins because it is the one that survives a proxy that
		// strips SSE fields it does not recognise.
		kind := e.Type
		if kind == "" {
			kind = ev.Name
		}

		switch kind {
		case "message_start":
			// Input tokens arrive here, at the very beginning; output tokens
			// arrive in message_delta at the very end. Reading only one of the
			// two is the easy way to report half the cost of every request.
			if e.Message != nil {
				s.usage = usageFrom(&e.Message.Usage)
			}

		case "content_block_start":
			if c := s.openBlock(e); c != nil {
				return c, nil
			}

		case "content_block_delta":
			if c := s.deltaFor(e); c != nil {
				return c, nil
			}

		case "content_block_stop":
			delete(s.blocks, e.Index)

		case "message_delta":
			// Output tokens are cumulative here and replace rather than add to
			// whatever message_start reported.
			if e.Usage != nil {
				s.usage.OutputTokens = e.Usage.OutputTokens
				if e.Usage.InputTokens > 0 {
					s.usage.InputTokens = e.Usage.InputTokens
				}
			}
			if e.Delta != nil && e.Delta.StopReason != "" {
				return &provider.Chunk{FinishReason: finishReason(e.Delta.StopReason)}, nil
			}

		case "message_stop":
			return nil, io.EOF

		case "error":
			msg := "stream error"
			class := provider.ClassRetrySame
			if e.Error != nil {
				msg = e.Error.Message
				if e.Error.Type == "invalid_request_error" {
					class = provider.ClassTerminal
				}
			}
			return nil, provider.ErrorFromBody(string(ProviderID), s.endpoint, class, msg)

		case "ping":
			// Keepalive. Emitting a chunk for it would be an empty delta that
			// clients render as a stutter.
		}
	}
}

func (s *stream) openBlock(e streamEvent) *provider.Chunk {
	if e.ContentBlock == nil {
		return nil
	}
	st := &blockState{kind: e.ContentBlock.Type}
	s.blocks[e.Index] = st

	if st.kind != "tool_use" {
		return nil
	}

	st.toolIndex = s.toolCount
	s.toolCount++
	st.identitySent = true

	// The opening delta carries the identity and no arguments. Anthropic sends
	// an empty input object here; treating it as a fragment would prepend "{}"
	// to the reassembled document and make it unparseable.
	return &provider.Chunk{ToolCall: &provider.ToolCallDelta{
		Index: st.toolIndex,
		ID:    e.ContentBlock.ID,
		Name:  e.ContentBlock.Name,
	}}
}

func (s *stream) deltaFor(e streamEvent) *provider.Chunk {
	if e.Delta == nil {
		return nil
	}
	st := s.blocks[e.Index]

	switch e.Delta.Type {
	case "text_delta":
		if e.Delta.Text == "" {
			return nil
		}
		return &provider.Chunk{Text: e.Delta.Text}

	case "thinking_delta":
		if e.Delta.Thinking == "" {
			return nil
		}
		return &provider.Chunk{Reasoning: e.Delta.Thinking}

	case "input_json_delta":
		if st == nil || e.Delta.PartialJSON == "" {
			return nil
		}
		// Passed through as raw text. These fragments are split at arbitrary
		// byte offsets — mid-key, mid-string, sometimes mid-escape — so parsing
		// one is guaranteed to fail and only the concatenation is valid.
		return &provider.Chunk{ToolCall: &provider.ToolCallDelta{
			Index:     st.toolIndex,
			Arguments: e.Delta.PartialJSON,
		}}

	case "signature_delta":
		// Signs a thinking block for replay. Relay does not echo thinking
		// blocks, so there is nothing to sign.
	}
	return nil
}

func (s *stream) Usage() *provider.Usage { return &s.usage }

func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.events.Close()
}

// finishReason maps Anthropic's stop_reason onto the OpenAI vocabulary Relay
// emits.
func finishReason(r string) provider.FinishReason {
	switch r {
	case "":
		return ""
	case "max_tokens":
		return provider.FinishLength
	case "tool_use":
		return provider.FinishToolCalls
	case "refusal":
		return provider.FinishContentFilter
	default:
		// end_turn, stop_sequence, and anything new.
		return provider.FinishStop
	}
}

func usageFrom(u *usage) provider.Usage {
	if u == nil {
		return provider.Usage{Estimated: true}
	}
	return provider.Usage{
		// Anthropic reports cache reads *separately* from input tokens rather
		// than as a subset of them, which is the opposite of OpenAI. Adding
		// them here makes InputTokens mean the same thing across providers, so
		// that Usage.Cost can apply one rule.
		InputTokens:       u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		CachedInputTokens: u.CacheReadInputTokens,
		OutputTokens:      u.OutputTokens,
		Estimated:         u.InputTokens == 0 && u.OutputTokens == 0 && u.CacheReadInputTokens == 0,
	}
}
