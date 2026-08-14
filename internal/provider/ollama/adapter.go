package ollama

import (
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/Shashank-Panda/relay/internal/domain"
	"github.com/Shashank-Panda/relay/internal/provider"
)

// ProviderID is the value catalog entries put in their provider field.
const ProviderID domain.ProviderID = "ollama"

const defaultBaseURL = "http://localhost:11434"

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

// ClassifyError adds Ollama's one distinctive case to the generic mapping.
//
// A 404 from Ollama means the model is not pulled on that host, not that the
// URL is wrong. It is a Reroute rather than a Terminal: the request is fine and
// another endpoint can serve it, so failing the caller would be giving up on a
// request that has somewhere else to go.
func (a *Adapter) ClassifyError(resp *http.Response, err error) provider.ErrorClass {
	if err != nil {
		return provider.ClassifyTransport(err)
	}
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return provider.ClassReroute
	}
	return provider.DefaultClassify(resp, err)
}

func (a *Adapter) call(ctx context.Context, req *domain.NormalizedRequest, ep *domain.ModelEndpoint, stream bool) (*http.Response, error) {
	base := ep.BaseURL
	if base == "" {
		base = defaultBaseURL
	}
	return provider.Call{
		Client:   a.client,
		URL:      base + "/api/chat",
		Body:     encodeRequest(req, ep, stream),
		Classify: a.ClassifyError,
		Provider: string(ProviderID),
		Endpoint: ep.ID,
	}.Do(ctx)
}

// Chat performs a non-streaming completion.
//
// Credential is unused: Ollama is a local daemon with no authentication. The
// parameter stays for the interface, and the catalog still names a
// credential_ref so that a future authenticated deployment — Ollama behind a
// reverse proxy — needs no code change.
func (a *Adapter) Chat(ctx context.Context, req *domain.NormalizedRequest, ep *domain.ModelEndpoint, _ provider.Credential) (*provider.Response, error) {
	httpResp, err := a.call(ctx, req, ep, false)
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

	// A 200 carrying an error field: Ollama does this when a model fails to
	// load. Treated as a failure, not as an empty answer, or the caller would
	// be billed for and shown a blank completion.
	if body.Error != "" {
		return nil, provider.ErrorFromBody(string(ProviderID), ep.ID, provider.ClassRetrySame, body.Error)
	}

	parts, _ := decodeParts(body.Message, 0)

	return &provider.Response{
		ID:           "chatcmpl-" + ep.Deployment,
		Model:        body.Model,
		Parts:        parts,
		FinishReason: finishReason(body.DoneReason, len(body.Message.ToolCalls) > 0),
		Usage:        usageFrom(&body),
	}, nil
}

func (a *Adapter) ChatStream(ctx context.Context, req *domain.NormalizedRequest, ep *domain.ModelEndpoint, _ provider.Credential) (provider.Stream, error) {
	httpResp, err := a.call(ctx, req, ep, true)
	if err != nil {
		return nil, err
	}
	return &stream{
		lines:    provider.NewLineReader(httpResp.Body),
		endpoint: ep.ID,
	}, nil
}

// stream reads Ollama's newline-delimited JSON.
type stream struct {
	lines    *provider.LineReader
	endpoint string

	usage     provider.Usage
	callIndex int
	pending   []provider.Chunk
	closed    bool
}

func (s *stream) Recv() (*provider.Chunk, error) {
	// One Ollama message can produce several chunks — text plus a tool call, or
	// several tool calls — so completed chunks queue here and drain before the
	// next line is read.
	if len(s.pending) > 0 {
		c := s.pending[0]
		s.pending = s.pending[1:]
		return &c, nil
	}

	for {
		line, err := s.lines.Next()
		if err != nil {
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, &provider.Error{
				Provider: string(ProviderID), Endpoint: s.endpoint,
				Class: provider.ClassifyTransport(err), Message: "reading stream", Err: err,
			}
		}

		var msg chatResponse
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			// A malformed line is not recoverable: the stream's framing is now
			// in doubt, and continuing would deliver a response with a silent
			// hole in it.
			return nil, &provider.Error{
				Provider: string(ProviderID), Endpoint: s.endpoint,
				Class: provider.ClassRetrySame, Message: "malformed stream line", Err: err,
			}
		}

		if msg.Error != "" {
			return nil, provider.ErrorFromBody(string(ProviderID), s.endpoint,
				provider.ClassRetrySame, msg.Error)
		}

		if msg.Done {
			s.usage = usageFrom(&msg)
		}

		s.enqueue(&msg)

		if len(s.pending) > 0 {
			c := s.pending[0]
			s.pending = s.pending[1:]
			return &c, nil
		}
		if msg.Done {
			return nil, io.EOF
		}
		// A line with no content and no done flag is a keepalive. Loop rather
		// than emit an empty chunk, which clients render as a stutter.
	}
}

func (s *stream) enqueue(msg *chatResponse) {
	if msg.Message.Content != "" {
		s.pending = append(s.pending, provider.Chunk{Text: msg.Message.Content})
	}

	// Ollama sends a tool call complete in one message rather than in
	// fragments, so it becomes a single delta carrying the whole arguments
	// document. The re-emitted OpenAI stream is still well-formed: one
	// fragment is a valid fragment sequence.
	for _, tc := range msg.Message.ToolCalls {
		s.pending = append(s.pending, provider.Chunk{
			ToolCall: &provider.ToolCallDelta{
				Index:     s.callIndex,
				ID:        synthesizeCallID(s.callIndex),
				Name:      tc.Function.Name,
				Arguments: string(tc.Function.Arguments),
			},
		})
		s.callIndex++
	}

	if msg.Done && len(s.pending) > 0 {
		reason := finishReason(msg.DoneReason, s.callIndex > 0)
		s.pending[len(s.pending)-1].FinishReason = reason
	} else if msg.Done {
		s.pending = append(s.pending, provider.Chunk{
			FinishReason: finishReason(msg.DoneReason, s.callIndex > 0),
		})
	}
}

func (s *stream) Usage() *provider.Usage { return &s.usage }

func (s *stream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	return s.lines.Close()
}
