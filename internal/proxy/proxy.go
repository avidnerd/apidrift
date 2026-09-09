package proxy

import (
	"context"
	"errors"
	"log"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/avidnerd/apidrift/internal/sample"
)

// Options configures a Proxy. Every dependency is passed explicitly; the
// package holds no package-level state.
type Options struct {
	// Upstream is the target the proxy forwards to. Required.
	Upstream *url.URL
	// Sink receives observed responses. Required. Its Offer must not block.
	Sink sample.Sink
	// MaxBodyBytes caps the retained body size. Zero means sample.MaxBodyBytes.
	MaxBodyBytes int
	// Transport is the round tripper used for upstream calls. Zero means
	// http.DefaultTransport.
	Transport http.RoundTripper
	// Now supplies the observation timestamp. Zero means time.Now.
	Now func() time.Time
	// ErrorLog receives proxy errors. Zero means the standard logger.
	ErrorLog *log.Logger
}

// counters are the proxy's lifetime tallies. Each is written from the response
// path and read by Stats, so all access is atomic.
type counters struct {
	requests          atomic.Uint64
	sampled           atomic.Uint64
	dropped           atomic.Uint64
	skippedNonJSON    atomic.Uint64
	skippedEncoded    atomic.Uint64
	skippedOversize   atomic.Uint64
	skippedIncomplete atomic.Uint64
	upstreamErrors    atomic.Uint64
}

// Proxy is a reverse proxy that copies the JSON responses passing through it
// into an analysis sink.
//
// Analysis is strictly off the critical path. Capture is an append into a
// preallocated buffer as the body streams to the client; hand-off is a
// non-blocking Offer. A saturated pipeline costs observations, never client
// latency and never correctness: the bytes the client receives are byte-for-byte
// what the upstream sent, whatever the analysis side is doing.
type Proxy struct {
	rp      *httputil.ReverseProxy
	sink    sample.Sink
	maxBody int
	now     func() time.Time
	c       counters
}

// observation carries the inbound request's identity to ModifyResponse, which
// otherwise only sees the rewritten outbound request.
type observation struct {
	method  string
	rawPath string
}

// ctxKey is the unexported context key type for observation, so no other
// package can collide with it.
type ctxKey struct{}

// New returns a Proxy forwarding to opts.Upstream. It returns an error if the
// upstream or the sink is missing.
func New(opts Options) (*Proxy, error) {
	if opts.Upstream == nil {
		return nil, errors.New("proxy: Upstream is required")
	}
	if opts.Upstream.Scheme != "http" && opts.Upstream.Scheme != "https" {
		return nil, errors.New("proxy: Upstream scheme must be http or https, got " + opts.Upstream.Scheme)
	}
	if opts.Sink == nil {
		return nil, errors.New("proxy: Sink is required")
	}

	p := &Proxy{
		sink:    opts.Sink,
		maxBody: opts.MaxBodyBytes,
		now:     opts.Now,
	}
	if p.maxBody <= 0 {
		p.maxBody = sample.MaxBodyBytes
	}
	if p.now == nil {
		p.now = time.Now
	}

	target := *opts.Upstream // copied so a later mutation by the caller cannot affect us
	p.rp = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(&target)
			pr.SetXForwarded()
		},
		ModifyResponse: p.modifyResponse,
		ErrorHandler:   p.handleUpstreamError,
		Transport:      opts.Transport,
		ErrorLog:       opts.ErrorLog,
	}
	return p, nil
}

// ServeHTTP forwards the request upstream and streams the response back,
// capturing eligible bodies on the way through.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.c.requests.Add(1)

	obs := observation{method: r.Method, rawPath: r.URL.EscapedPath()}
	p.rp.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, obs)))
}

// modifyResponse decides whether a response is worth capturing and, if so,
// wraps its body in a tee. It never reads the body itself: buffering here would
// stall the client until the whole response had arrived.
func (p *Proxy) modifyResponse(resp *http.Response) error {
	obs, ok := resp.Request.Context().Value(ctxKey{}).(observation)
	if !ok {
		// Cannot attribute the response to a request; nothing useful to record.
		return nil
	}

	if !isJSONContentType(resp.Header.Get("Content-Type")) {
		p.c.skippedNonJSON.Add(1)
		return nil
	}
	// A body still under a content coding is compressed bytes, not JSON. The
	// transport decodes transparently only when it added Accept-Encoding
	// itself, which it does not do when the client asked for an encoding.
	if enc := resp.Header.Get("Content-Encoding"); enc != "" && !strings.EqualFold(strings.TrimSpace(enc), "identity") {
		p.c.skippedEncoded.Add(1)
		return nil
	}
	// Declared oversize: skip before allocating anything.
	if resp.ContentLength > int64(p.maxBody) {
		p.c.skippedOversize.Add(1)
		return nil
	}

	resp.Body = newTeeBody(resp.Body, p.maxBody, resp.ContentLength, func(out teeOutcome) {
		p.record(obs, resp.StatusCode, out)
	})
	return nil
}

// record turns a finished capture into a sample, or into a skip counter.
func (p *Proxy) record(obs observation, status int, out teeOutcome) {
	switch {
	case out.Oversize:
		p.c.skippedOversize.Add(1)
	case !out.Complete:
		p.c.skippedIncomplete.Add(1)
	default:
		s := sample.Sample{
			Method:     obs.method,
			RawPath:    obs.rawPath,
			Status:     status,
			Body:       out.Body,
			ObservedAt: p.now(),
		}
		if p.sink.Offer(s) {
			p.c.sampled.Add(1)
		} else {
			p.c.dropped.Add(1)
		}
	}
}

// handleUpstreamError reports upstream failures to the client as 502 and counts
// them. It mirrors httputil.ReverseProxy's default behaviour, plus the counter.
func (p *Proxy) handleUpstreamError(w http.ResponseWriter, r *http.Request, err error) {
	p.c.upstreamErrors.Add(1)
	if errors.Is(err, context.Canceled) {
		// The client gave up; there is nobody left to tell.
		return
	}
	w.WriteHeader(http.StatusBadGateway)
}

// isJSONContentType reports whether a Content-Type header names a JSON media
// type, including the "+json" structured suffix (application/problem+json,
// application/vnd.api+json, and so on).
func isJSONContentType(ct string) bool {
	if ct == "" {
		return false
	}
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	mt = strings.ToLower(mt)
	return mt == "application/json" || mt == "text/json" || strings.HasSuffix(mt, "+json")
}
