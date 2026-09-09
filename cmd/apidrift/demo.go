package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/proxy"
	"github.com/avidnerd/apidrift/internal/report"
	"github.com/avidnerd/apidrift/internal/sample"
	"github.com/avidnerd/apidrift/internal/store"
	"github.com/avidnerd/apidrift/internal/valuesample"
)

// The demo exists because you cannot try apidrift against a real API and see it
// work. Detection needs a baseline window and a current window that differ, and
// a real upstream will not break itself on request. So the demo supplies a fake
// upstream that does change, and runs it through the actual pipeline: the real
// proxy, the real templater, the real extractor, the real store, the real
// detector, the real report. Only the upstream and the traffic are synthetic.
//
// Timestamps are injected rather than waited for. The proxy takes a clock, so
// the demo stamps the first phase an hour in the past and the second phase now,
// which puts them in different buckets without sleeping through an hour.

type demoConfig struct {
	requests int
	seed     int64
	verbose  bool
	serve    string
}

func parseDemoFlags(args []string, stderr io.Writer) (demoConfig, error) {
	var c demoConfig

	fs := flag.NewFlagSet("demo", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.IntVar(&c.requests, "requests", 400, "requests to send per window")
	fs.Int64Var(&c.seed, "seed", 1, "seed for the fake upstream")
	fs.BoolVar(&c.verbose, "verbose", false, "print a sample response from each version")
	fs.StringVar(&c.serve, "serve", "", "after running, serve the dashboard on this address (e.g. 127.0.0.1:9090) instead of exiting")

	if err := fs.Parse(args); err != nil {
		return c, parseError(err)
	}
	if c.requests < 50 {
		return c, fmt.Errorf("%w: -requests must be at least 50, or there is not enough data to test", errUsage)
	}
	return c, nil
}

// demoUpstream serves a small charge object. Calling breakIt() switches it to a
// second version with four changes in it.
type demoUpstream struct {
	mu     sync.Mutex
	rng    *rand.Rand
	broken bool
}

func (u *demoUpstream) breakIt() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.broken = true
}

func (u *demoUpstream) body() []byte {
	u.mu.Lock()
	defer u.mu.Unlock()

	out := map[string]any{
		"id":     fmt.Sprintf("ch_%d", u.rng.Intn(1_000_000)),
		"object": "charge",
		"status": []string{"succeeded", "pending"}[u.rng.Intn(2)],
	}

	if !u.broken {
		out["amount"] = u.rng.Intn(500000)                     // integer
		out["legacy_id"] = fmt.Sprintf("%d", u.rng.Intn(9999)) // always present
		out["email"] = "customer@example.com"                  // never null
		b, _ := json.Marshal(out)
		return b
	}

	// Version 2, with four changes a caller would care about.
	out["amount"] = fmt.Sprintf("%.2f", float64(u.rng.Intn(500000))/100) // int -> string
	if u.rng.Float64() < 0.05 {
		out["legacy_id"] = fmt.Sprintf("%d", u.rng.Intn(9999)) // 100% -> 5%
	}
	if u.rng.Float64() < 0.4 {
		out["email"] = nil // now sometimes null
	} else {
		out["email"] = "customer@example.com"
	}
	if u.rng.Float64() < 0.3 {
		out["status"] = "disputed" // new enum value
	}
	b, _ := json.Marshal(out)
	return b
}

func (u *demoUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b := u.body()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	_, _ = w.Write(b)
}

// demoCmd runs the whole pipeline against a fake upstream that breaks halfway.
func demoCmd(args []string, stdout, stderr io.Writer) error {
	cfg, err := parseDemoFlags(args, stderr)
	if err != nil {
		return err
	}

	up := &demoUpstream{rng: rand.New(rand.NewSource(cfg.seed))}
	upstream := httptest.NewServer(up)
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		return err
	}

	// The clock the proxy stamps samples with. Phase one is filed two hours back
	// and phase two an hour back, so the two land in different buckets without
	// the demo having to sleep through an hour.
	//
	// evalAt is deliberately a separate variable from the clock. They were the
	// same one at first, which meant advancing the clock silently dragged the
	// evaluation time backwards with it and no window ever lined up.
	evalAt := time.Now().UTC().Truncate(time.Hour)

	var (
		clockMu sync.Mutex
		stampAt = evalAt
	)
	clock := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return stampAt
	}
	setClock := func(t time.Time) {
		clockMu.Lock()
		stampAt = t
		clockMu.Unlock()
	}

	queue := sample.NewQueue(8192)
	prx, err := proxy.New(proxy.Options{Upstream: target, Sink: queue, Now: clock})
	if err != nil {
		return err
	}

	detectCfg := detect.Config{MinSamples: 50, PersistWindows: 1}
	an := &analyzer{
		templater:   endpoint.NewTrieTemplater(endpoint.Config{MinSamples: 20, StatMinSamples: 100}),
		values:      valuesample.New(valuesample.Config{SampleRate: 1.0}),
		store:       store.New(store.Config{BucketDuration: time.Hour}),
		tracker:     detect.NewTracker(detectCfg),
		cfg:         detectCfg,
		baselineFor: time.Hour,
		currentFor:  time.Hour,
	}

	done := make(chan struct{})
	go func() { defer close(done); an.consume(queue.C()) }()

	front := httptest.NewServer(prx)
	defer front.Close()
	client := front.Client()

	baselineAt := evalAt.Add(-2 * time.Hour)
	currentAt := evalAt.Add(-1 * time.Hour)
	// The first evaluation happens between the two phases. It finds nothing,
	// because there is only one window of data at that point, but it is what
	// rolls the value samples over so the semantic layer has a baseline to
	// compare against. Real deployments get this for free by running for more
	// than one window.
	firstEvalAt := evalAt.Add(-1 * time.Hour)

	fmt.Fprintf(stdout, "apidrift demo\n\n")
	fmt.Fprintf(stdout, "A fake upstream serving /v1/charges/{id}. It answers normally for the\n")
	fmt.Fprintf(stdout, "first window, then four things change in the second one. Everything below\n")
	fmt.Fprintf(stdout, "runs through the real proxy, store and detector.\n\n")

	setClock(baselineAt)
	fmt.Fprintf(stdout, "  sending %d requests (baseline)...\n", cfg.requests)
	if err := demoTraffic(client, front.URL, cfg.requests); err != nil {
		return err
	}
	if cfg.verbose {
		fmt.Fprintf(stdout, "\n  a baseline response:\n    %s\n\n", up.body())
	}

	an.evaluate(firstEvalAt)

	up.breakIt()
	setClock(currentAt)
	fmt.Fprintf(stdout, "  upstream changes\n")
	fmt.Fprintf(stdout, "  sending %d requests (current)...\n", cfg.requests)
	if err := demoTraffic(client, front.URL, cfg.requests); err != nil {
		return err
	}
	if cfg.verbose {
		fmt.Fprintf(stdout, "\n  a current response:\n    %s\n", up.body())
	}

	// Drain the queue before comparing, so no sample is still in flight.
	queue.Close()
	<-done

	rep := an.evaluate(evalAt)
	fmt.Fprintf(stdout, "\n")
	if err := report.WriteText(stdout, rep); err != nil {
		return err
	}

	st := an.stats()
	fmt.Fprintf(stdout, "\n%d responses analysed, %d dropped, %d unresolved during templater warm-up\n",
		st.Stored, prx.Stats().Dropped, st.Unresolved)

	if c := an.semanticCandidates(); len(c) > 0 {
		fmt.Fprintf(stdout, "\n%d path(s) flagged for semantic review (run `apidrift assess` to judge them):\n", len(c))
		for _, r := range c {
			fmt.Fprintf(stdout, "  %s %s: %s\n", r.Endpoint, r.Path, r.Shift.Reason)
		}
	}

	if cfg.serve != "" {
		return serveDemoDashboard(cfg.serve, prx, an, stdout)
	}
	return nil
}

// serveDemoDashboard keeps the process up after the demo has run, serving the
// dashboard over the findings it just produced. It is the quickest way to see
// what the web view looks like with something in it, since a real upstream
// will not break itself on request.
func serveDemoDashboard(addr string, prx *proxy.Proxy, an *analyzer, stdout io.Writer) error {
	srv := &http.Server{Addr: addr, Handler: adminHandler(prx, an)}

	fmt.Fprintf(stdout, "\ndashboard on http://%s  (ctrl-c to stop)\n", addr)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errs := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs <- err
			return
		}
		errs <- nil
	}()

	select {
	case <-ctx.Done():
		fmt.Fprintln(stdout, "\nstopping")
	case err := <-errs:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// demoTraffic sends n requests through the proxy, varying the id so the
// templater has something to learn.
//
// The ids are the length real ones are. A short id like "ch_7" is not
// identifier-shaped by the templater's rules, which want a long body after the
// prefix, so it would be treated as a static route name and every request would
// get its own endpoint until the growth tier caught up.
func demoTraffic(client *http.Client, base string, n int) error {
	for i := 0; i < n; i++ {
		resp, err := client.Get(fmt.Sprintf("%s/v1/charges/ch_3PxYz%08dLmNoPqR", base, i))
		if err != nil {
			return fmt.Errorf("demo traffic: %w", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	return nil
}
