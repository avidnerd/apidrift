package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/signal"
	"syscall"
	"time"

	"github.com/avidnerd/apidrift/internal/dashboard"
	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/proxy"
	"github.com/avidnerd/apidrift/internal/sample"
	"github.com/avidnerd/apidrift/internal/store"
	"github.com/avidnerd/apidrift/internal/valuesample"
)

// serveConfig is the parsed command line for "apidrift serve".
type serveConfig struct {
	upstream    string
	listen      string
	admin       string
	queueDepth  int
	maxBody     int
	currentFor  time.Duration
	baselineFor time.Duration
	minSamples  uint64
	persist     int
	shutdown    time.Duration
	sampleRate  float64
	noSemantic  bool
}

func parseServeFlags(args []string, stderr io.Writer) (serveConfig, error) {
	var c serveConfig

	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.upstream, "upstream", "", "upstream base URL to proxy to (required), e.g. https://api.stripe.com")
	fs.StringVar(&c.listen, "listen", ":8080", "address to serve the proxy on")
	fs.StringVar(&c.admin, "admin", "127.0.0.1:9090", "address to serve /stats and /findings on")
	fs.IntVar(&c.queueDepth, "queue", 4096, "sample queue depth; a full queue drops samples rather than delaying requests")
	fs.IntVar(&c.maxBody, "max-body", sample.MaxBodyBytes, "largest response body to analyse, in bytes")
	fs.DurationVar(&c.currentFor, "window", time.Hour, "width of the current window")
	fs.DurationVar(&c.baselineFor, "baseline", 24*time.Hour, "width of the baseline window, ending where the current window starts")
	fs.Uint64Var(&c.minSamples, "min-samples", detect.DefaultConfig().MinSamples, "responses required in each window before a comparison is made")
	fs.IntVar(&c.persist, "persist", detect.DefaultConfig().PersistWindows, "consecutive windows a finding must survive before it is reported")
	fs.DurationVar(&c.shutdown, "shutdown-timeout", 10*time.Second, "how long to let in-flight requests finish on shutdown")
	fs.Float64Var(&c.sampleRate, "value-sample-rate", valuesample.DefaultSampleRate, "share of response bodies sampled for field values, which feeds semantic candidate selection")
	fs.BoolVar(&c.noSemantic, "no-semantic", false, "do not sample field values; disables semantic candidate selection entirely")

	if err := fs.Parse(args); err != nil {
		return c, parseError(err)
	}
	if c.upstream == "" {
		return c, fmt.Errorf("%w: serve requires -upstream", errUsage)
	}
	if c.currentFor <= 0 || c.baselineFor <= 0 {
		return c, fmt.Errorf("%w: -window and -baseline must be positive", errUsage)
	}
	if c.queueDepth < 1 {
		return c, fmt.Errorf("%w: -queue must be at least 1", errUsage)
	}
	if c.sampleRate < 0 || c.sampleRate > 1 {
		return c, fmt.Errorf("%w: -value-sample-rate must be between 0 and 1", errUsage)
	}
	return c, nil
}

// serveCmd runs the proxy until interrupted.
func serveCmd(args []string, stdout, stderr io.Writer) error {
	cfg, err := parseServeFlags(args, stderr)
	if err != nil {
		return err
	}

	target, err := url.Parse(cfg.upstream)
	if err != nil {
		return fmt.Errorf("%w: parsing -upstream: %v", errUsage, err)
	}

	queue := sample.NewQueue(cfg.queueDepth)
	prx, err := proxy.New(proxy.Options{
		Upstream:     target,
		Sink:         queue,
		MaxBodyBytes: cfg.maxBody,
	})
	if err != nil {
		return fmt.Errorf("%w: %v", errUsage, err)
	}

	detectCfg := detect.Config{
		MinSamples:     cfg.minSamples,
		PersistWindows: cfg.persist,
	}
	var values *valuesample.Store
	if !cfg.noSemantic {
		values = valuesample.New(valuesample.Config{SampleRate: cfg.sampleRate})
	}

	an := &analyzer{
		templater:   endpoint.NewTrieTemplater(endpoint.Config{}),
		values:      values,
		store:       store.New(store.Config{BucketDuration: bucketFor(cfg.currentFor)}),
		tracker:     detect.NewTracker(detectCfg),
		cfg:         detectCfg,
		baselineFor: cfg.baselineFor,
		currentFor:  cfg.currentFor,
		logf: func(format string, args ...any) {
			fmt.Fprintf(stderr, "apidrift: "+format+"\n", args...)
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		an.consume(queue.C())
	}()

	evaluatorDone := make(chan struct{})
	go func() {
		defer close(evaluatorDone)
		runEvaluator(ctx, an, cfg.currentFor)
	}()

	proxySrv := &http.Server{Addr: cfg.listen, Handler: prx}
	adminSrv := &http.Server{Addr: cfg.admin, Handler: adminHandler(prx, an)}

	fmt.Fprintf(stdout, "apidrift: proxying %s -> %s\n", cfg.listen, target)
	fmt.Fprintf(stdout, "apidrift: dashboard on http://%s\n", cfg.admin)
	fmt.Fprintf(stdout, "apidrift: also /stats, /findings, /candidates, /healthz\n")
	fmt.Fprintf(stdout, "apidrift: comparing a %s current window against a %s baseline, evaluated every %s\n",
		cfg.currentFor, cfg.baselineFor, cfg.currentFor)

	errs := make(chan error, 2)
	go func() { errs <- listenAndServe(proxySrv, "proxy") }()
	go func() { errs <- listenAndServe(adminSrv, "admin") }()

	var runErr error
	select {
	case <-ctx.Done():
		fmt.Fprintln(stdout, "apidrift: shutting down")
	case runErr = <-errs:
	}

	// Stop accepting, let in-flight requests finish, then close the pipeline in
	// order: no more samples can arrive once the servers are down, so closing
	// the queue lets the consumer drain what is left and exit.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.shutdown)
	defer cancel()
	_ = proxySrv.Shutdown(shutdownCtx)
	_ = adminSrv.Shutdown(shutdownCtx)

	stop()
	<-evaluatorDone
	queue.Close()
	<-consumerDone

	return runErr
}

// listenAndServe runs srv, treating a clean shutdown as success.
func listenAndServe(srv *http.Server, name string) error {
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("%s listener: %w", name, err)
	}
	return nil
}

// runEvaluator advances the detection window on a ticker.
//
// Windows advance with time, not with requests to /findings: the persistence
// tracker counts calls as windows, so evaluating on demand would let a reader
// qualify a finding early just by refreshing the page.
func runEvaluator(ctx context.Context, an *analyzer, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			an.evaluate(now)
		}
	}
}

// bucketFor picks a store bucket width fine enough to express the requested
// window. A window is assembled from whole buckets, so buckets coarser than the
// window would round it beyond recognition.
func bucketFor(window time.Duration) time.Duration {
	if window < time.Hour {
		return time.Minute
	}
	return time.Hour
}

// adminHandler serves the operational surface: counters, the latest findings,
// and a liveness check.
//
// It is a separate listener from the proxy on purpose. Mounting these paths on
// the proxy would shadow any upstream route of the same name, which is a
// monitoring tool silently changing the API it monitors.
func adminHandler(prx *proxy.Proxy, an *analyzer) http.Handler {
	mux := http.NewServeMux()

	mux.Handle("/", dashboard.Handler())
	mux.Handle("/stats", statsHandler(prx, an))
	mux.Handle("/findings", findingsHandler(an))
	mux.Handle("/candidates", candidatesHandler(an))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux
}

// serveStats is the combined counter snapshot served at /stats.
type serveStats struct {
	Proxy    proxy.Stats         `json:"proxy"`
	Analyzer analyzerStats       `json:"analyzer"`
	Store    store.Stats         `json:"store"`
	Values   *valuesample.Stats  `json:"values,omitempty"`
	Endpoint endpoint.Stats      `json:"endpoint"`
	Tracker  detect.TrackerStats `json:"tracker"`
}

func statsHandler(prx *proxy.Proxy, an *analyzer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !readOnly(w, r) {
			return
		}
		st := serveStats{
			Proxy:    prx.Stats(),
			Analyzer: an.stats(),
			Store:    an.store.Stats(),
			Tracker:  an.tracker.Stats(),
		}
		if t, ok := an.templater.(*endpoint.TrieTemplater); ok {
			st.Endpoint = t.Stats()
		}
		if an.values != nil {
			vs := an.values.Stats()
			st.Values = &vs
		}
		writeJSON(w, st)
	})
}

func findingsHandler(an *analyzer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !readOnly(w, r) {
			return
		}
		rep, ok := an.report()
		if !ok {
			// No window has closed yet. This is a normal state during warm-up,
			// not an error, but there is genuinely nothing to return.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			writeJSONBody(w, map[string]string{
				"error": "no evaluation has run yet; the first one happens when the current window closes",
			})
			return
		}
		writeJSON(w, rep)
	})
}

// candidatesHandler serves the paths whose values moved enough to be worth a
// model's opinion. The wire format is the request the model will be given, so
// a reader can see exactly what would be sent before spending anything on it.
func candidatesHandler(an *analyzer) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !readOnly(w, r) {
			return
		}
		writeJSON(w, an.semanticCandidates())
	})
}

// readOnly rejects anything but GET and HEAD, reporting whether to continue.
func readOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	return false
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	writeJSONBody(w, v)
}

// writeJSONBody encodes v, ignoring write errors: a failure here means the
// monitoring client went away, and there is nothing useful to do about it.
func writeJSONBody(w http.ResponseWriter, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
