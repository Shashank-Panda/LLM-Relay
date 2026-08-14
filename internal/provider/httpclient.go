package provider

import (
	"net"
	"net/http"
	"time"
)

// ClientOptions tunes the per-provider HTTP client.
type ClientOptions struct {
	// MaxConnsPerHost is the expected concurrent request count against one
	// provider host.
	//
	// Go's default MaxIdleConnsPerHost is 2. Above that concurrency every extra
	// connection is closed immediately after use instead of returning to the
	// pool, so a busy gateway pays a fresh TCP and TLS handshake on most
	// requests — tens of milliseconds added to a budget measured in single
	// digits, and it degrades exactly when load is highest. This is the single
	// most common performance bug in Go HTTP proxies.
	MaxConnsPerHost int

	// ResponseHeaderTimeout bounds the wait for response headers.
	//
	// This is the streaming-safe timeout. http.Client.Timeout bounds the entire
	// exchange including the body, which for a stream means it terminates a
	// working response mid-generation. Headers arriving is the signal that the
	// provider accepted the request; everything after that is bounded by the
	// request context instead.
	ResponseHeaderTimeout time.Duration

	DialTimeout     time.Duration
	IdleConnTimeout time.Duration
}

func DefaultClientOptions() ClientOptions {
	return ClientOptions{
		MaxConnsPerHost:       256,
		ResponseHeaderTimeout: 60 * time.Second,
		DialTimeout:           5 * time.Second,
		IdleConnTimeout:       90 * time.Second,
	}
}

// NewClient builds a provider HTTP client.
//
// Note what is absent: Timeout is never set. Deadlines come from the request
// context, which is derived from the inbound request, which is what makes
// client disconnect cancel the upstream call. A client-level timeout would
// silently override that relationship on every streaming request.
func NewClient(opts ClientOptions) *http.Client {
	d := DefaultClientOptions()
	if opts.MaxConnsPerHost <= 0 {
		opts.MaxConnsPerHost = d.MaxConnsPerHost
	}
	if opts.ResponseHeaderTimeout <= 0 {
		opts.ResponseHeaderTimeout = d.ResponseHeaderTimeout
	}
	if opts.DialTimeout <= 0 {
		opts.DialTimeout = d.DialTimeout
	}
	if opts.IdleConnTimeout <= 0 {
		opts.IdleConnTimeout = d.IdleConnTimeout
	}

	t := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   opts.DialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          opts.MaxConnsPerHost * 2,
		MaxIdleConnsPerHost:   opts.MaxConnsPerHost,
		IdleConnTimeout:       opts.IdleConnTimeout,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		ExpectContinueTimeout: time.Second,

		// Streaming responses must not be buffered by the transport, or every
		// chunk waits for the next one and time-to-first-token becomes
		// time-to-second-token.
		DisableCompression: false,
		WriteBufferSize:    32 << 10,
		ReadBufferSize:     32 << 10,
	}

	return &http.Client{Transport: t}
}
