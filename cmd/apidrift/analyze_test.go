package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/proxy"
	"github.com/avidnerd/apidrift/internal/report"
	"github.com/avidnerd/apidrift/internal/sample"
	"github.com/avidnerd/apidrift/internal/schema"
	"github.com/avidnerd/apidrift/internal/store"
)

var origin = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// newAnalyzer builds an analyzer with minute-wide buckets, so tests can span
// several windows without simulating hours of traffic.
func newAnalyzer() *analyzer {
	cfg := detect.Config{MinSamples: 10, PersistWindows: 2}
	return &analyzer{
		templater: endpoint.NewTrieTemplater(endpoint.Config{}),
		// One-second buckets, so a test that emits one sample per second files
		// each into an empty bucket. That keeps the ingest path off
		// schema.Merge, which is a stub, while still exercising it fully.
		store:       store.New(store.Config{BucketDuration: time.Second, Buckets: 4000}),
		tracker:     detect.NewTracker(cfg),
		cfg:         cfg,
		baselineFor: 10 * time.Minute,
		currentFor:  5 * time.Minute,
	}
}

func mkSample(path string, at time.Time, body string) sample.Sample {
	return sample.Sample{
		Method:     "GET",
		RawPath:    path,
		Status:     200,
		Body:       []byte(body),
		ObservedAt: at,
	}
}

// TestAnalyzerWarmsUpThenStores covers the handoff from "the templater cannot
// place this path" to "filed against an endpoint".
func TestAnalyzerWarmsUpThenStores(t *testing.T) {
	a := newAnalyzer()

	const requests = 300
	for i := 0; i < requests; i++ {
		at := origin.Add(time.Duration(i) * time.Second)
		a.handle(mkSample(fmt.Sprintf("/users/%d/orders", i), at, `{"id":"cus_1","status":"open"}`))
	}

	st := a.stats()
	if st.Consumed != requests {
		t.Errorf("Consumed = %d, want %d", st.Consumed, requests)
	}
	if st.Unresolved == 0 {
		t.Error("Unresolved = 0, want the early samples to have arrived before the templater could place them")
	}
	// Samples land one per second in one-second buckets, so nothing merges and
	// the store's writes complete. Realistic traffic puts many samples in one
	// bucket, which needs schema.Merge -- see TestAnalyzerAccumulatesWithinABucket.
	if st.Stored == 0 {
		t.Fatalf("Stored = 0, want the later samples to have been filed (stats %+v)", st)
	}
	if st.Panics != 0 {
		t.Errorf("Panics = %d, want 0 for one sample per bucket", st.Panics)
	}
	if st.Consumed != st.Stored+st.Unresolved+st.ParseErrors+st.StaleDrops+st.Panics {
		t.Errorf("counters do not account for every sample: %+v", st)
	}

	// And they were filed under one templated endpoint, not three hundred.
	keys := a.store.Keys()
	if len(keys) != 1 {
		t.Fatalf("%d endpoints, want 1: %v", len(keys), keys)
	}
	if want := "/users/{id}/orders"; keys[0].Template != want {
		t.Errorf("template = %q, want %q", keys[0].Template, want)
	}
}

func TestAnalyzerCountsUnparseableBodies(t *testing.T) {
	a := newAnalyzer()

	// Warm the templater up on a fixed path first, one sample per bucket.
	for i := 0; i < 100; i++ {
		a.handle(mkSample("/health", origin.Add(time.Duration(i)*time.Second), `{"ok":true}`))
	}
	before := a.stats().ParseErrors

	// A body that fails to parse never reaches the store, so these can share a
	// timestamp.
	for i := 0; i < 10; i++ {
		a.handle(mkSample("/health", origin, `{"truncated":`))
	}

	if got := a.stats().ParseErrors - before; got != 10 {
		t.Errorf("ParseErrors rose by %d, want 10", got)
	}
}

func TestAnalyzerCountsStaleSamples(t *testing.T) {
	a := newAnalyzer()
	a.store = store.New(store.Config{BucketDuration: time.Second, Buckets: 300})

	for i := 0; i < 100; i++ {
		a.handle(mkSample("/health", origin.Add(time.Duration(i)*time.Second), `{"ok":true}`))
	}
	// Far older than the five minutes of retention.
	a.handle(mkSample("/health", origin.Add(-time.Hour), `{"ok":true}`))

	if got := a.stats().StaleDrops; got != 1 {
		t.Errorf("StaleDrops = %d, want 1", got)
	}
}

// seed files a schema against an endpoint directly, bypassing the templater and
// the extractor. It is how the evaluation tests below place exactly one schema
// in exactly one bucket, which is what keeps them off the schema.Merge stub.
func seed(t *testing.T, a *analyzer, key endpoint.Key, at time.Time, samples uint64) {
	t.Helper()

	sc := &schema.Schema{
		Samples: samples,
		Fields: map[string]*schema.FieldStats{
			"status": {
				Seen:         samples,
				Present:      samples,
				TypeCounts:   map[schema.Kind]uint64{schema.KindString: samples},
				StringValues: map[string]uint64{"open": samples},
			},
		},
	}
	if !a.store.Add(key, at, sc) {
		t.Fatalf("store.Add(%v, %v) = false", key, at)
	}
}

// singleBucketAnalyzer compares two windows one bucket wide, so neither window
// query has to merge.
func singleBucketAnalyzer() *analyzer {
	a := newAnalyzer()
	a.store = store.New(store.Config{BucketDuration: time.Second, Buckets: 120})
	a.baselineFor = time.Second
	a.currentFor = time.Second
	return a
}

// TestAnalyzerEvaluate is the end-to-end shape of a detection pass: two windows
// are built from the store, compared, and rendered into a report over the
// ranges that were asked for.
func TestAnalyzerEvaluate(t *testing.T) {
	a := singleBucketAnalyzer()
	key := endpoint.Key{Method: "GET", Template: "/health", StatusClass: 2}

	now := origin.Add(10 * time.Second)
	seed(t, a, key, now.Add(-2*time.Second), 500) // baseline window
	seed(t, a, key, now.Add(-1*time.Second), 400) // current window

	rep := a.evaluate(now)

	st := a.stats()
	if st.Evaluations != 1 {
		t.Errorf("Evaluations = %d, want 1", st.Evaluations)
	}
	if st.DetectErrors != 0 {
		t.Errorf("DetectErrors = %d, want 0: the comparison should complete cleanly", st.DetectErrors)
	}
	if len(rep.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none when nothing failed", rep.Warnings)
	}
	// Both windows hold the same shape, so there is nothing to report -- and
	// the endpoint must still be counted as compared, or a quiet report would
	// be indistinguishable from one that examined nothing.
	if rep.Summary.Findings != 0 {
		t.Errorf("Findings = %d, want 0 for two identical windows", rep.Summary.Findings)
	}
	if rep.Summary.EndpointsCompared != 1 {
		t.Errorf("EndpointsCompared = %d, want 1", rep.Summary.EndpointsCompared)
	}

	// The report is still well-formed, and the ranges are the ones asked for.
	if !rep.Current.To.Equal(now) {
		t.Errorf("Current.To = %v, want %v", rep.Current.To, now)
	}
	if want := now.Add(-a.currentFor); !rep.Current.From.Equal(want) {
		t.Errorf("Current.From = %v, want %v", rep.Current.From, want)
	}
	if want := now.Add(-a.currentFor - a.baselineFor); !rep.Baseline.From.Equal(want) {
		t.Errorf("Baseline.From = %v, want %v", rep.Baseline.From, want)
	}

	// And it is the one served at /findings.
	cached, ok := a.report()
	if !ok {
		t.Fatal("no cached report after evaluating")
	}
	if cached.GeneratedAt != rep.GeneratedAt {
		t.Error("the cached report is not the one evaluate returned")
	}
}

// TestAnalyzerSkipsEndpointsMissingAWindow: an endpoint that stopped being
// called has not changed shape, and must not be reported as though it had.
func TestAnalyzerSkipsEndpointsMissingAWindow(t *testing.T) {
	tests := []struct {
		name        string
		seedAt      func(now time.Time) time.Time
		wantSkipped bool
	}{
		{"only the baseline has data", func(now time.Time) time.Time { return now.Add(-2 * time.Second) }, true},
		{"only the current window has data", func(now time.Time) time.Time { return now.Add(-1 * time.Second) }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := singleBucketAnalyzer()
			key := endpoint.Key{Method: "GET", Template: "/health", StatusClass: 2}

			now := origin.Add(10 * time.Second)
			seed(t, a, key, tt.seedAt(now), 500)

			a.evaluate(now)

			if got := a.stats().DetectErrors; got != 0 {
				t.Errorf("DetectErrors = %d, want 0: an endpoint missing a window is skipped before analysis", got)
			}
		})
	}
}

func TestAnalyzerReportBeforeAnyEvaluation(t *testing.T) {
	if _, ok := newAnalyzer().report(); ok {
		t.Error("report() = ok before any evaluation, want not ok")
	}
}

// -- the admin surface --------------------------------------------------------

func newTestProxy(t *testing.T, sink sample.Sink) *proxy.Proxy {
	t.Helper()

	target, err := url.Parse("http://upstream.invalid")
	if err != nil {
		t.Fatalf("parsing target: %v", err)
	}
	p, err := proxy.New(proxy.Options{Upstream: target, Sink: sink})
	if err != nil {
		t.Fatalf("proxy.New: %v", err)
	}
	return p
}

func TestAdminHandler(t *testing.T) {
	a := newAnalyzer()
	h := adminHandler(newTestProxy(t, sample.NewQueue(16)), a)

	t.Run("healthz", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("stats reports every layer", func(t *testing.T) {
		a.handle(mkSample("/health", origin, `{"ok":true}`))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stats", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}

		var got serveStats
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("stats body is not JSON: %v\n%s", err, rec.Body.String())
		}
		if got.Analyzer.Consumed != 1 {
			t.Errorf("Analyzer.Consumed = %d, want 1", got.Analyzer.Consumed)
		}
		if got.Endpoint.Observations != 1 {
			t.Errorf("Endpoint.Observations = %d, want 1 -- templater stats must be reported", got.Endpoint.Observations)
		}
	})

	t.Run("findings before the first window closes", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/findings", nil))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("status = %d, want 503 during warm-up", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "no evaluation has run yet") {
			t.Errorf("body does not explain the wait:\n%s", rec.Body.String())
		}
	})

	t.Run("findings after an evaluation", func(t *testing.T) {
		a.evaluate(origin.Add(time.Hour))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/findings", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var got report.Report
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("findings body is not a report: %v\n%s", err, rec.Body.String())
		}
	})

	t.Run("writes are rejected", func(t *testing.T) {
		for _, path := range []string{"/stats", "/findings"} {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Errorf("POST %s = %d, want 405", path, rec.Code)
			}
		}
	})
}

// -- the report command's client side -----------------------------------------

func TestFetchReport(t *testing.T) {
	want := report.Build(origin,
		report.Range{From: origin.Add(-time.Hour), To: origin},
		report.Range{From: origin, To: origin.Add(time.Hour)},
		nil)

	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr string
	}{
		{
			name: "a report is decoded",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/findings" {
					t.Errorf("requested %q, want /findings", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(want)
			},
		},
		{
			name: "a warm-up 503 explains itself",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "no evaluation has run yet"})
			},
			wantErr: "no evaluation has run yet",
		},
		{
			name: "a non-JSON body is reported as such",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "<html>not apidrift</html>")
			},
			wantErr: "decoding the report",
		},
		{
			name: "a plain error body is passed through",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "boom", http.StatusInternalServerError)
			},
			wantErr: "boom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(tt.handler)
			defer srv.Close()

			got, err := fetchReport(context.Background(), srv.Client(), srv.URL)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !got.GeneratedAt.Equal(want.GeneratedAt) {
				t.Errorf("GeneratedAt = %v, want %v", got.GeneratedAt, want.GeneratedAt)
			}
		})
	}
}

func TestFetchReportUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close() // nothing is listening now

	_, err := fetchReport(context.Background(), http.DefaultClient, addr)
	if err == nil {
		t.Fatal("fetchReport against a dead address = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "Nothing is listening") {
		t.Errorf("error does not say what to do about it: %v", err)
	}
}

func TestFetchReportTrailingSlash(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(report.Report{})
	}))
	defer srv.Close()

	if _, err := fetchReport(context.Background(), srv.Client(), srv.URL+"/"); err != nil {
		t.Fatalf("fetchReport: %v", err)
	}
	if gotPath != "/findings" {
		t.Errorf("requested %q, want /findings -- a trailing slash on -admin must not double up", gotPath)
	}
}

// -- formerly blocked on schema.Merge ----------------------------------------

// TestAnalyzerAccumulatesWithinABucket is the realistic ingest case -- many
// responses inside one bucket -- and is what the counters above cannot show.
func TestAnalyzerAccumulatesWithinABucket(t *testing.T) {
	a := newAnalyzer()
	a.store = store.New(store.Config{BucketDuration: time.Hour, Buckets: 24})

	for i := 0; i < 500; i++ {
		a.handle(mkSample("/health", origin, `{"ok":true,"status":"green"}`))
	}

	if st := a.stats(); st.Panics != 0 {
		t.Fatalf("Panics = %d, want 0", st.Panics)
	}
	keys := a.store.Keys()
	if len(keys) != 1 {
		t.Fatalf("%d endpoints, want 1", len(keys))
	}
	sc, ok := a.store.Window(keys[0], origin, origin.Add(time.Hour))
	if !ok {
		t.Fatal("no window")
	}
	if sc.Samples < 400 {
		t.Errorf("Samples = %d, want the bucket to have accumulated most of the 500 responses", sc.Samples)
	}
}

// TestAnalyzerReportsEachDistinctPanicOnce is the guard that makes containing
// panics defensible: containment is only acceptable while the failure stays
// loud, and a process that has been quietly containing one bug must still
// announce the arrival of a different one.
func TestAnalyzerReportsEachDistinctPanicOnce(t *testing.T) {
	a := singleBucketAnalyzer()

	var logged []string
	a.logf = func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	// panicSink stands in for any analysis step that blows up. The cause is
	// under the test's control, so two distinct bugs can be simulated.
	cause := "first bug"
	a.templater = panicTemplater{cause: &cause}

	for i := 0; i < 5; i++ {
		a.handle(mkSample("/health", origin, `{"ok":true}`))
	}
	if len(logged) != 1 {
		t.Fatalf("logged %d times for one repeated cause, want 1: %v", len(logged), logged)
	}
	if !strings.Contains(logged[0], "first bug") {
		t.Errorf("the log line does not name the cause: %q", logged[0])
	}

	// A different failure arrives. It must be reported, not swallowed because
	// the process has already said something once.
	cause = "second bug"
	for i := 0; i < 5; i++ {
		a.handle(mkSample("/health", origin, `{"ok":true}`))
	}
	if len(logged) != 2 {
		t.Fatalf("logged %d times for two distinct causes, want 2: %v", len(logged), logged)
	}
	if !strings.Contains(logged[1], "second bug") {
		t.Errorf("the second cause was not reported: %q", logged[1])
	}

	// Every panic is still counted, whether or not it was logged.
	if got := a.stats().Panics; got != 10 {
		t.Errorf("Panics = %d, want 10 -- the rate lives in the counter, not the log", got)
	}
}

// TestAnalyzerPanicLoggingIsBounded guards against a cause that varies per
// sample turning the log into a flood.
func TestAnalyzerPanicLoggingIsBounded(t *testing.T) {
	a := singleBucketAnalyzer()

	var logged int
	a.logf = func(format string, args ...any) { logged++ }

	cause := ""
	a.templater = panicTemplater{cause: &cause}
	for i := 0; i < 100; i++ {
		cause = fmt.Sprintf("bug number %d", i)
		a.handle(mkSample("/health", origin, `{"ok":true}`))
	}

	if logged > maxReportedPanicCauses {
		t.Errorf("logged %d times for 100 distinct causes, want at most %d", logged, maxReportedPanicCauses)
	}
	if logged == 0 {
		t.Error("logged nothing at all; the first causes must still be reported")
	}
}

// panicTemplater panics on Observe with a caller-controlled cause.
type panicTemplater struct{ cause *string }

func (p panicTemplater) Observe(method, rawPath string) { panic(*p.cause) }

func (p panicTemplater) Resolve(method, rawPath string, status int) (endpoint.Key, bool) {
	return endpoint.Key{}, false
}
