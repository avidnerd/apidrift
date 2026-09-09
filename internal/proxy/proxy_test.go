package proxy_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/avidnerd/apidrift/internal/proxy"
	"github.com/avidnerd/apidrift/internal/sample"
)

// recordingSink collects everything offered to it. accept=false makes it
// refuse, standing in for a saturated pipeline.
type recordingSink struct {
	mu      sync.Mutex
	samples []sample.Sample
	accept  bool
}

func newSink() *recordingSink { return &recordingSink{accept: true} }

func (r *recordingSink) Offer(s sample.Sample) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.accept {
		return false
	}
	r.samples = append(r.samples, s)
	return true
}

func (r *recordingSink) all() []sample.Sample {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sample.Sample(nil), r.samples...)
}

var fixedNow = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)

// newProxy stands a proxy in front of upstream and returns it with its sink.
func newProxy(t *testing.T, upstream *httptest.Server, maxBody int) (*proxy.Proxy, *recordingSink) {
	t.Helper()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parsing upstream URL: %v", err)
	}
	sink := newSink()
	p, err := proxy.New(proxy.Options{
		Upstream:     target,
		Sink:         sink,
		MaxBodyBytes: maxBody,
		Now:          func() time.Time { return fixedNow },
	})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	return p, sink
}

// get drives one request through h and returns the status and body the client
// sees. It reads the body to completion, as a well-behaved client does.
func get(t *testing.T, h http.Handler, method, path string, body io.Reader) (*http.Response, string) {
	t.Helper()

	front := httptest.NewServer(h)
	defer front.Close()

	req, err := http.NewRequest(method, front.URL+path, body)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer resp.Body.Close()

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading proxied body: %v", err)
	}
	return resp, string(got)
}

func TestNewValidatesOptions(t *testing.T) {
	valid, _ := url.Parse("http://example.test")
	badScheme, _ := url.Parse("ftp://example.test")

	tests := []struct {
		name    string
		opts    proxy.Options
		wantErr string
	}{
		{"missing upstream", proxy.Options{Sink: newSink()}, "Upstream is required"},
		{"missing sink", proxy.Options{Upstream: valid}, "Sink is required"},
		{"non-http scheme", proxy.Options{Upstream: badScheme, Sink: newSink()}, "scheme must be http or https"},
		{"valid", proxy.Options{Upstream: valid, Sink: newSink()}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := proxy.New(tt.opts)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("New() = %v, want nil", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("New() = nil, want error containing %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Errorf("New() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestProxyForwardsFaithfully is the load-bearing test: whatever apidrift does
// with its analysis, the request that arrives upstream and the response that
// reaches the client must be unchanged.
func TestProxyForwardsFaithfully(t *testing.T) {
	var (
		gotMethod, gotPath, gotQuery, gotHeader, gotBody string
		gotXFF                                           string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		gotHeader, gotBody, gotXFF = r.Header.Get("X-Api-Key"), string(b), r.Header.Get("X-Forwarded-For")

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req_123")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"cus_1","object":"customer"}`)
	}))
	defer upstream.Close()

	p, sink := newProxy(t, upstream, 0)

	front := httptest.NewServer(p)
	defer front.Close()

	req, err := http.NewRequest(http.MethodPost, front.URL+"/v1/customers?limit=3", strings.NewReader(`{"name":"a"}`))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	req.Header.Set("X-Api-Key", "sk_test")
	resp, err := front.Client().Do(req)
	if err != nil {
		t.Fatalf("request through proxy: %v", err)
	}
	defer resp.Body.Close()
	clientBody, _ := io.ReadAll(resp.Body)

	// Request side.
	if gotMethod != http.MethodPost {
		t.Errorf("upstream method = %q, want POST", gotMethod)
	}
	if gotPath != "/v1/customers" {
		t.Errorf("upstream path = %q, want /v1/customers", gotPath)
	}
	if gotQuery != "limit=3" {
		t.Errorf("upstream query = %q, want limit=3", gotQuery)
	}
	if gotHeader != "sk_test" {
		t.Errorf("upstream X-Api-Key = %q, want sk_test", gotHeader)
	}
	if gotBody != `{"name":"a"}` {
		t.Errorf("upstream request body = %q, want {\"name\":\"a\"}", gotBody)
	}
	if gotXFF == "" {
		t.Error("upstream X-Forwarded-For is empty, want the client address")
	}

	// Response side.
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("client status = %d, want 201", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Request-Id"); got != "req_123" {
		t.Errorf("client X-Request-Id = %q, want req_123", got)
	}
	if want := `{"id":"cus_1","object":"customer"}`; string(clientBody) != want {
		t.Errorf("client body = %q, want %q", clientBody, want)
	}

	// Observation side.
	got := sink.all()
	if len(got) != 1 {
		t.Fatalf("sampled %d responses, want 1", len(got))
	}
	s := got[0]
	if s.Method != http.MethodPost || s.RawPath != "/v1/customers" || s.Status != http.StatusCreated {
		t.Errorf("sample = {%q %q %d}, want {POST /v1/customers 201}", s.Method, s.RawPath, s.Status)
	}
	if string(s.Body) != `{"id":"cus_1","object":"customer"}` {
		t.Errorf("sample body = %q, want the response body", s.Body)
	}
	if !s.ObservedAt.Equal(fixedNow) {
		t.Errorf("sample ObservedAt = %v, want the injected clock %v", s.ObservedAt, fixedNow)
	}
}

// TestProxyEligibility covers which responses are sampled and, when one is not,
// which counter accounts for it. Every response must land in exactly one bucket.
func TestProxyEligibility(t *testing.T) {
	const big = 4096

	tests := []struct {
		name        string
		contentType string
		encoding    string
		body        string
		gzipOnWire  bool // body is really gzip-compressed on the wire
		setLength   bool
		maxBody     int
		wantSampled bool
		wantCounter func(proxy.Stats) uint64
	}{
		{
			name:        "application/json is sampled",
			contentType: "application/json",
			body:        `{"a":1}`,
			wantSampled: true,
		},
		{
			name:        "json with charset parameters is sampled",
			contentType: "application/json; charset=utf-8",
			body:        `{"a":1}`,
			wantSampled: true,
		},
		{
			name:        "the +json structured suffix is sampled",
			contentType: "application/problem+json",
			body:        `{"type":"about:blank"}`,
			wantSampled: true,
		},
		{
			name:        "vendor +json is sampled",
			contentType: "application/vnd.api+json",
			body:        `{"data":[]}`,
			wantSampled: true,
		},
		{
			name:        "text/html is skipped as non-JSON",
			contentType: "text/html",
			body:        "<html></html>",
			wantCounter: func(s proxy.Stats) uint64 { return s.SkippedNonJSON },
		},
		{
			name:        "a missing content type is skipped as non-JSON",
			contentType: "",
			body:        `{"a":1}`,
			wantCounter: func(s proxy.Stats) uint64 { return s.SkippedNonJSON },
		},
		{
			name:        "a malformed content type is skipped as non-JSON",
			contentType: "application/json;;;",
			body:        `{"a":1}`,
			wantCounter: func(s proxy.Stats) uint64 { return s.SkippedNonJSON },
		},
		{
			name:        "javascript is not JSON",
			contentType: "application/javascript",
			body:        "var a = 1",
			wantCounter: func(s proxy.Stats) uint64 { return s.SkippedNonJSON },
		},
		{
			// A client that asks for gzip gets gzip: the proxy forwards the
			// Accept-Encoding, so the transport does not decode transparently
			// and the body arrives compressed. Compressed bytes are not JSON.
			name:        "a gzip-encoded body is skipped rather than mis-parsed",
			contentType: "application/json",
			encoding:    "gzip",
			body:        `{"a":1}`,
			gzipOnWire:  true,
			wantCounter: func(s proxy.Stats) uint64 { return s.SkippedEncoded },
		},
		{
			name:        "an identity encoding is still sampled",
			contentType: "application/json",
			encoding:    "identity",
			body:        `{"a":1}`,
			wantSampled: true,
		},
		{
			name:        "a declared oversize body is skipped before it is read",
			contentType: "application/json",
			body:        strings.Repeat("x", 200),
			setLength:   true,
			maxBody:     100,
			wantCounter: func(s proxy.Stats) uint64 { return s.SkippedOversize },
		},
		{
			name:        "an undeclared oversize body is skipped once it exceeds the cap",
			contentType: "application/json",
			body:        strings.Repeat("x", big),
			maxBody:     100,
			wantCounter: func(s proxy.Stats) uint64 { return s.SkippedOversize },
		},
		{
			name:        "a body exactly at the cap is sampled",
			contentType: "application/json",
			body:        strings.Repeat("x", 100),
			maxBody:     100,
			wantSampled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.contentType != "" {
					w.Header().Set("Content-Type", tt.contentType)
				}
				if tt.encoding != "" {
					w.Header().Set("Content-Encoding", tt.encoding)
				}
				wire := []byte(tt.body)
				if tt.gzipOnWire {
					var buf bytes.Buffer
					zw := gzip.NewWriter(&buf)
					_, _ = zw.Write([]byte(tt.body))
					_ = zw.Close()
					wire = buf.Bytes()
				}
				if tt.setLength {
					w.Header().Set("Content-Length", strconv.Itoa(len(wire)))
				}
				_, _ = w.Write(wire)
			}))
			defer upstream.Close()

			p, sink := newProxy(t, upstream, tt.maxBody)
			resp, clientBody := get(t, p, http.MethodGet, "/thing", nil)

			// Whatever the eligibility decision, the client is unaffected.
			if resp.StatusCode != http.StatusOK {
				t.Errorf("client status = %d, want 200", resp.StatusCode)
			}
			if clientBody != tt.body {
				t.Errorf("client body = %q, want %q", clientBody, tt.body)
			}

			got := sink.all()
			if tt.wantSampled {
				if len(got) != 1 {
					t.Fatalf("sampled %d responses, want 1", len(got))
				}
				if string(got[0].Body) != tt.body {
					t.Errorf("sample body = %q, want %q", got[0].Body, tt.body)
				}
				return
			}
			if len(got) != 0 {
				t.Fatalf("sampled %d responses, want 0", len(got))
			}
			if n := tt.wantCounter(p.Stats()); n != 1 {
				t.Errorf("expected skip counter = %d, want 1 (stats: %+v)", n, p.Stats())
			}
		})
	}
}

// TestProxySamplesErrorResponses guards that non-2xx bodies are observed too:
// error shapes drift as readily as success shapes, and the endpoint key carries
// the status class precisely so they can be tracked apart.
func TestProxySamplesErrorResponses(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusNotFound, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"code":"x"}}`)
			}))
			defer upstream.Close()

			p, sink := newProxy(t, upstream, 0)
			resp, _ := get(t, p, http.MethodGet, "/v1/charges/ch_1", nil)

			if resp.StatusCode != status {
				t.Errorf("client status = %d, want %d", resp.StatusCode, status)
			}
			got := sink.all()
			if len(got) != 1 {
				t.Fatalf("sampled %d responses, want 1", len(got))
			}
			if got[0].Status != status {
				t.Errorf("sample status = %d, want %d", got[0].Status, status)
			}
			if got[0].StatusClass() != status/100 {
				t.Errorf("sample status class = %d, want %d", got[0].StatusClass(), status/100)
			}
		})
	}
}

// TestProxyDropsWhenSinkIsSaturated is the promise that matters under load: a
// full pipeline costs observations, not client requests.
func TestProxyDropsWhenSinkIsSaturated(t *testing.T) {
	const body = `{"id":"cus_1"}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	defer upstream.Close()

	p, sink := newProxy(t, upstream, 0)
	sink.accept = false // the pipeline refuses everything

	const requests = 5
	for i := 0; i < requests; i++ {
		resp, clientBody := get(t, p, http.MethodGet, "/v1/customers", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: client status = %d, want 200", i, resp.StatusCode)
		}
		if clientBody != body {
			t.Fatalf("request %d: client body = %q, want %q", i, clientBody, body)
		}
	}

	st := p.Stats()
	if st.Dropped != requests {
		t.Errorf("Stats().Dropped = %d, want %d", st.Dropped, requests)
	}
	if st.Sampled != 0 {
		t.Errorf("Stats().Sampled = %d, want 0", st.Sampled)
	}
	if st.Requests != requests {
		t.Errorf("Stats().Requests = %d, want %d", st.Requests, requests)
	}
}

func TestProxyUpstreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parsing upstream URL: %v", err)
	}
	upstream.Close() // nothing is listening now

	sink := newSink()
	p, err := proxy.New(proxy.Options{Upstream: target, Sink: sink})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	resp, _ := get(t, p, http.MethodGet, "/v1/customers", nil)
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("client status = %d, want 502", resp.StatusCode)
	}
	if st := p.Stats(); st.UpstreamErrors != 1 {
		t.Errorf("Stats().UpstreamErrors = %d, want 1", st.UpstreamErrors)
	}
	if n := len(sink.all()); n != 0 {
		t.Errorf("sampled %d responses from a failed upstream, want 0", n)
	}
}

func TestStatsHandler(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"a":1}`)
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatalf("parsing upstream URL: %v", err)
	}
	// A real Queue, so the handler's nested queue snapshot is exercised.
	q := sample.NewQueue(2)
	p, err := proxy.New(proxy.Options{Upstream: target, Sink: q})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	// Three requests against a queue of depth two: two land, one drops.
	for i := 0; i < 3; i++ {
		if resp, _ := get(t, p, http.MethodGet, "/v1/things", nil); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, resp.StatusCode)
		}
	}

	rec := httptest.NewRecorder()
	p.StatsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("stats status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("stats Content-Type = %q, want application/json", ct)
	}

	var got proxy.Stats
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("stats body is not JSON: %v (%s)", err, rec.Body.String())
	}
	if got.Requests != 3 || got.Sampled != 2 || got.Dropped != 1 {
		t.Errorf("stats = {Requests:%d Sampled:%d Dropped:%d}, want {3 2 1}", got.Requests, got.Sampled, got.Dropped)
	}
	if got.Queue == nil {
		t.Fatal("stats has no queue snapshot, want one from a sample.Queue sink")
	}
	if got.Queue.Capacity != 2 || got.Queue.Enqueued != 2 || got.Queue.Dropped != 1 {
		t.Errorf("queue stats = %+v, want Capacity 2, Enqueued 2, Dropped 1", *got.Queue)
	}
}

func TestStatsHandlerRejectsWrites(t *testing.T) {
	target, _ := url.Parse("http://example.test")
	p, err := proxy.New(proxy.Options{Upstream: target, Sink: newSink()})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}

	rec := httptest.NewRecorder()
	p.StatsHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/stats", nil))

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /stats = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("Allow = %q, want \"GET, HEAD\"", allow)
	}
}

// TestProxyConcurrentRequests is the race-detector case: many in-flight
// requests sharing one proxy, one sink and one set of counters.
func TestProxyConcurrentRequests(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"path":"`+r.URL.Path+`"}`)
	}))
	defer upstream.Close()

	p, sink := newProxy(t, upstream, 0)

	front := httptest.NewServer(p)
	defer front.Close()

	const (
		workers = 8
		each    = 25
	)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				resp, err := front.Client().Get(front.URL + "/v1/things/" + strconv.Itoa(w))
				if err != nil {
					t.Errorf("worker %d request %d: %v", w, i, err)
					return
				}
				if _, err := io.ReadAll(resp.Body); err != nil {
					t.Errorf("worker %d request %d: reading body: %v", w, i, err)
				}
				resp.Body.Close()
			}
		}(w)
	}
	wg.Wait()

	if n := len(sink.all()); n != workers*each {
		t.Errorf("sampled %d responses, want %d", n, workers*each)
	}
	if st := p.Stats(); st.Requests != workers*each || st.Sampled != workers*each {
		t.Errorf("stats = {Requests:%d Sampled:%d}, want both %d", st.Requests, st.Sampled, workers*each)
	}
}
