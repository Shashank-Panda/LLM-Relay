package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

const ProviderID domain.ProviderID = "openai"

const defaultBaseURL = "https://api.openai.com/v1"

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

// ClassifyError reads OpenAI's error code before falling back to the status.
//
// The status alone is not enough. A context overflow and a malformed tool
// schema are both 400, but one should be rerouted to a larger endpoint and the
// other must not be retried anywhere. Treating them alike either wastes three
// calls on a guaranteed failure or fails a request that had somewhere to go.
func (a *Adapter) ClassifyError(resp *http.Response, err error) provider.ErrorClass {
	if err != nil {
		return provider.ClassifyTransport(err)
	}
	if resp == nil {
		return provider.ClassTerminal
	}

	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
		if code := peekErrorCode(resp); code != "" {
			switch code {
			case "context_length_exceeded", "string_above_max_length":
				return provider.ClassReroute
			case "model_not_found":
				// The catalog is out of date, not the request. Another endpoint
				// can serve it.
				return provider.ClassReroute
			}
		}
	}

	// 429 splits two ways. A rate limit clears on its own; an exhausted quota
	// does not, and retrying it just burns the request deadline before failing.
	if resp.StatusCode == http.StatusTooManyRequests {
		if peekErrorCode(resp) == "insufficient_quota" {
			return provider.ClassTerminal
		}
		return provider.ClassRetrySame
	}

	return provider.DefaultClassify(resp, err)
}

// peekErrorCode reads the error code without consuming the body.
//
// The body is replaced with a fresh reader over the bytes read, so the generic
// error path can still render the provider's message. A classifier that
// silently ate the body would leave every OpenAI failure reported with an empty
// message.
func peekErrorCode(resp *http.Response) string {
	if resp.Body == nil {
		return ""
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return ""
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(strings.NewReader(string(raw)), resp.Body), resp.Body}

	var body errorBody
	if json.Unmarshal(raw, &body) != nil {
		return ""
	}
	return body.Error.Code
}

func (a *Adapter) call(ctx context.Context, req *domain.NormalizedRequest, ep *domain.ModelEndpoint, cred provider.Credential, stream bool) (*http.Response, error) {
	base := ep.BaseURL
	if base == "" {
		base = defaultBaseURL
	}

	headers := map[string]string{"Authorization": "Bearer " + cred.APIKey}
	for k, v := range cred.Headers {
		headers[k] = v
	}

	return provider.Call{
		Client:   a.client,
		URL:      base + "/chat/completions",
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

	var body chatResponse
	if err := provider.DecodeJSON(httpResp, &body); err != nil {
		return nil, &provider.Error{
			Provider: string(ProviderID), Endpoint: ep.ID,
			Class: provider.ClassRetrySame, Message: "decoding response", Err: err,
		}
	}
	if len(body.Choices) == 0 {
		return nil, provider.ErrorFromBody(string(ProviderID), ep.ID,
			provider.ClassRetrySame, "response contained no choices")
	}

	ch := body.Choices[0]
	msg := ch.Message
	if msg == nil {
		msg = &delta{}
	}

	var parts []domain.ContentPart
	if msg.Reasoning != "" {
		parts = append(parts, domain.ContentPart{Kind: domain.PartReasoning, Text: msg.Reasoning})
	}
	if msg.Content != nil && *msg.Content != "" {
		parts = append(parts, domain.ContentPart{Kind: domain.PartText, Text: *msg.Content})
	}
	for _, tc := range msg.ToolCalls {
		parts = append(parts, domain.ContentPart{
			Kind:       domain.PartToolCall,
			ToolCallID: tc.ID,
			ToolName:   tc.Function.Name,
			Arguments:  tc.Function.Arguments,
		})
	}

	return &provider.Response{
		ID:           body.ID,
		Model:        body.Model,
		Parts:        parts,
		FinishReason: finishReason(ch.FinishReason),
		Usage:        usageFrom(body.Usage),
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
	}, nil
}

type stream struct {
	events   *provider.EventReader
	endpoint string

	usage  provider.Usage
	closed bool

	// seen records which tool-call indices have already had their identity
	// emitted, so a provider that repeats the id and name on every fragment
	// does not produce a stream that reassembles into duplicate calls.
	seen map[int]bool
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
		// The terminator, not a chunk. Treating it as JSON produces a parse
		// error on every successful stream.
		if data == "[DONE]" {
			return nil, io.EOF
		}

		var frame chatResponse
		if err := json.Unmarshal([]byte(data), &frame); err != nil {
			return nil, &provider.Error{
				Provider: string(ProviderID), Endpoint: s.endpoint,
				Class: provider.ClassRetrySame, Message: "malformed stream frame", Err: err,
			}
		}

		// The usage frame carries an empty choices array. It arrives last and
		// is the only place streamed token counts appear.
		if frame.Usage != nil {
			s.usage = usageFrom(frame.Usage)
		}
		if len(frame.Choices) == 0 {
			continue
		}

		ch := frame.Choices[0]
		if ch.Delta == nil {
			if ch.FinishReason != "" {
				return &provider.Chunk{FinishReason: finishReason(ch.FinishReason)}, nil
			}
			continue
		}

		if c := s.chunkFrom(ch); c != nil {
			return c, nil
		}
		// A frame carrying only role:"assistant" opens the stream and has no
		// content. Emitting it would be an empty chunk clients render as a
		// stutter.
	}
}

func (s *stream) chunkFrom(ch choice) *provider.Chunk {
	d := ch.Delta
	out := &provider.Chunk{FinishReason: finishReason(ch.FinishReason)}

	if d.Content != nil {
		out.Text = *d.Content
	}
	out.Reasoning = d.Reasoning

	if len(d.ToolCalls) > 0 {
		tc := d.ToolCalls[0]
		idx := 0
		if tc.Index != nil {
			idx = *tc.Index
		}
		if s.seen == nil {
			s.seen = map[int]bool{}
		}

		out.ToolCall = &provider.ToolCallDelta{
			Index: idx,
			// Arguments pass through as raw text. Parsing a fragment would fail
			// — fragments are split mid-token — and re-encoding the whole
			// document would reorder its keys.
			Arguments: tc.Function.Arguments,
		}
		if !s.seen[idx] && (tc.ID != "" || tc.Function.Name != "") {
			out.ToolCall.ID = tc.ID
			out.ToolCall.Name = tc.Function.Name
			s.seen[idx] = true
		}
	}

	if out.Text == "" && out.Reasoning == "" && out.ToolCall == nil && out.FinishReason == "" {
		return nil
	}
	return out
}

func (s *stream) Usage() *provider.Usage { return &s.usage }

func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.events.Close()
}

// finishReason maps OpenAI's vocabulary, which is the one Relay re-emits, so
// this is mostly identity. It exists so that an unrecognised value becomes a
// known one rather than passing through to confuse a client.
func finishReason(r string) provider.FinishReason {
	switch r {
	case "":
		return ""
	case "length":
		return provider.FinishLength
	case "tool_calls", "function_call":
		return provider.FinishToolCalls
	case "content_filter":
		return provider.FinishContentFilter
	default:
		return provider.FinishStop
	}
}

func usageFrom(u *usage) provider.Usage {
	if u == nil {
		// No usage block at all. Flagged rather than reported as zeros, so it
		// can be excluded from cost reporting instead of quietly deflating it.
		return provider.Usage{Estimated: true}
	}
	out := provider.Usage{
		InputTokens:  u.PromptTokens,
		OutputTokens: u.CompletionTokens,
	}
	if d := u.PromptTokensDetails; d != nil {
		out.CachedInputTokens = d.CachedTokens
	}
	if d := u.CompletionTokensDetails; d != nil {
		out.ReasoningTokens = d.ReasoningTokens
	}
	return out
}
