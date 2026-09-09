package main

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/avidnerd/apidrift/internal/assess"
	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/report"
	"github.com/avidnerd/apidrift/internal/sample"
	"github.com/avidnerd/apidrift/internal/schema"
	"github.com/avidnerd/apidrift/internal/store"
	"github.com/avidnerd/apidrift/internal/valuesample"
)

// analyzerStats counts what the analysis pipeline did with the samples it was
// given. Every sample lands in exactly one bucket.
type analyzerStats struct {
	// Consumed is samples taken off the queue.
	Consumed uint64 `json:"consumed"`
	// Stored is samples that produced a schema filed against an endpoint.
	Stored uint64 `json:"stored"`
	// Unresolved is samples whose path the templater could not yet place. This
	// is expected during warm-up and should fall to near zero.
	Unresolved uint64 `json:"unresolved"`
	// ParseErrors is samples whose body was not parseable JSON, despite the
	// content type saying otherwise.
	ParseErrors uint64 `json:"parse_errors"`
	// StaleDrops is samples the store rejected as older than retention.
	StaleDrops uint64 `json:"stale_drops"`
	// Panics counts samples whose analysis panicked. Analysis runs off the
	// request path precisely so that this cannot reach a client, and the
	// counter is how it stays visible instead of silent.
	Panics uint64 `json:"panics"`
	// Evaluations is how many detection passes have run.
	Evaluations uint64 `json:"evaluations"`
	// SemanticCandidates is how many paths the deterministic pre-filter
	// selected for a model to judge in the most recent window. It is the
	// number that bounds what the judgement layer costs to run.
	SemanticCandidates int `json:"semantic_candidates"`
	// DetectErrors counts endpoints whose detection pass failed. While
	// detect.Detect is a stub this equals the number of endpoints examined.
	DetectErrors uint64 `json:"detect_errors"`
}

// analyzer turns the stream of sampled responses into stored schemas, and
// periodically compares windows of them.
//
// It owns the templater, the store and the tracker. Nothing here is package
// state: a second analyzer would share nothing with this one.
type analyzer struct {
	templater endpoint.Templater
	store     *store.Store
	tracker   *detect.Tracker
	cfg       detect.Config
	// values samples the actual values seen at each path, which is what the
	// semantic layer reasons over. Nil disables semantic candidate selection
	// entirely; the statistical detector is unaffected either way.
	values *valuesample.Store

	// baselineFor and currentFor are the widths of the two windows compared.
	// The current window ends at the evaluation time; the baseline ends where
	// the current one starts.
	baselineFor time.Duration
	currentFor  time.Duration

	// logf, when set, receives operational notices. A counter in /stats is the
	// right place for a rate, but somebody has to be told the first time, or a
	// silently degraded pipeline looks exactly like a quiet one.
	logf func(format string, args ...any)

	// panicMu guards seenPanics, which holds one entry per distinct panic
	// cause already reported. Reporting only the very first panic would mean a
	// second, different bug appearing later was never mentioned at all -- which
	// is the strongest argument against containing panics, so it is worth the
	// ten lines to answer it.
	panicMu    sync.Mutex
	seenPanics map[string]bool

	counters struct {
		consumed     atomic.Uint64
		panics       atomic.Uint64
		stored       atomic.Uint64
		unresolved   atomic.Uint64
		parseErrors  atomic.Uint64
		staleDrops   atomic.Uint64
		evaluations  atomic.Uint64
		detectErrors atomic.Uint64
	}

	// latest holds the most recent report, served to /findings. A pointer
	// swapped under a mutex, so readers never see a half-built report and
	// never block the evaluation goroutine for longer than the swap.
	mu         sync.RWMutex
	latest     *report.Report
	candidates []assess.SemanticRequest
}

// consume drains the sample queue until it closes.
func (a *analyzer) consume(samples <-chan sample.Sample) {
	for s := range samples {
		a.handle(s)
	}
}

// handle files one sample, and contains any panic in doing so.
//
// The containment is not defensive habit. apidrift's central promise is that
// analysis cannot affect the service it proxies, and a panic on the consumer
// goroutine would take down the whole process -- the proxy with it. That is the
// one failure this design is not allowed to have, so the analysis path is
// isolated at its entry point and the failure is counted instead.
//
// It also happens to be what lets the pipeline run today: store.Add calls
// schema.Merge, which is a stub.
func (a *analyzer) handle(s sample.Sample) {
	defer func() {
		if r := recover(); r != nil {
			a.counters.panics.Add(1)
			a.notePanic(r)
		}
	}()
	a.file(s)
}

// maxReportedPanicCauses bounds how many distinct panic messages are logged.
// A cause that varies per sample -- one carrying an index, say -- would
// otherwise turn the log into the flood that rate-limiting exists to prevent.
const maxReportedPanicCauses = 8

// notePanic reports each distinct analysis panic once.
//
// Once per distinct cause rather than once per process: a rate belongs in a
// counter, but a new *kind* of failure is news every time, and a process that
// has been contentedly containing one bug for a week must not swallow the
// arrival of a second one.
func (a *analyzer) notePanic(cause any) {
	if a.logf == nil {
		return
	}
	message := fmt.Sprint(cause)

	a.panicMu.Lock()
	if a.seenPanics == nil {
		a.seenPanics = make(map[string]bool)
	}
	novel := !a.seenPanics[message] && len(a.seenPanics) < maxReportedPanicCauses
	if novel {
		a.seenPanics[message] = true
	}
	distinct := len(a.seenPanics)
	a.panicMu.Unlock()

	if !novel {
		return
	}
	a.logf("analysis panicked and was contained (distinct cause %d): %v\n"+
		"          the proxy is unaffected, but samples are being lost; see \"panics\" in /stats",
		distinct, message)
}

// file does the real work of handle: learn the path, resolve the endpoint,
// extract the shape, store it.
func (a *analyzer) file(s sample.Sample) {
	a.counters.consumed.Add(1)

	a.templater.Observe(s.Method, s.RawPath)
	key, ok := a.templater.Resolve(s.Method, s.RawPath, s.Status)
	if !ok {
		// Not enough traffic through this path yet. The sample is discarded
		// rather than held: filing it under a provisional key would split the
		// endpoint's history once the real key was decided.
		a.counters.unresolved.Add(1)
		return
	}

	// Values are sampled before the schema is stored, and deliberately so.
	// The semantic layer has no dependency on the structural pipeline -- it
	// reasons over raw values, not over merged counts -- so putting it
	// downstream of the store would make it fail whenever the store did, for
	// no reason. Today that matters concretely: schema.Merge is a stub, and
	// this keeps semantic candidate selection working anyway.
	if a.values != nil {
		a.values.Observe(key, s.Body)
	}

	sc, err := schema.Extract(s.Body)
	if err != nil {
		a.counters.parseErrors.Add(1)
		return
	}

	if !a.store.Add(key, s.ObservedAt, sc) {
		a.counters.staleDrops.Add(1)
		return
	}
	a.counters.stored.Add(1)
}

// evaluate compares each endpoint's baseline window against its current window
// and returns the report, having first passed the findings through the
// persistence tracker.
//
// It must be called once per window rather than once per request: the tracker
// counts calls as windows, so evaluating on demand would let a reader qualify a
// finding early simply by refreshing.
func (a *analyzer) evaluate(now time.Time) report.Report {
	a.counters.evaluations.Add(1)

	currentFrom := now.Add(-a.currentFor)
	baselineFrom := currentFrom.Add(-a.baselineFor)

	type pending struct {
		key                      endpoint.Key
		baseSamples, currSamples uint64
		findings                 []detect.Finding
	}

	var (
		pendings []pending
		all      []detect.Finding
		failed   int
	)
	for _, key := range a.store.Keys() {
		baseSchema, currSchema, okBase, okCurr, err := a.safeWindows(key, baselineFrom, currentFrom, now)
		if err != nil {
			a.counters.detectErrors.Add(1)
			failed++
			continue
		}
		if !okBase || !okCurr {
			// One side has no data, so there is nothing to compare. This is
			// not a finding: an endpoint that stopped being called has not
			// changed shape, and reporting it as drift would be wrong.
			continue
		}

		found, err := safeDetect(
			detect.Window{From: baselineFrom, To: currentFrom, Schema: baseSchema},
			detect.Window{From: currentFrom, To: now, Schema: currSchema},
			a.cfg,
		)
		if err != nil {
			a.counters.detectErrors.Add(1)
			failed++
			continue
		}

		// Detect's inputs are two windows and nothing else, so it cannot know
		// which endpoint they came from. The caller stamps it.
		for i := range found {
			found[i].Endpoint = key
		}

		pendings = append(pendings, pending{
			key:         key,
			baseSamples: baseSchema.Samples,
			currSamples: currSchema.Samples,
			findings:    found,
		})
		all = append(all, found...)
	}

	// One tracker call per window, over every endpoint's findings together.
	persisted := make(map[detect.ID]bool, len(all))
	for _, f := range a.tracker.Observe(all) {
		persisted[f.ID()] = true
	}

	results := make([]report.EndpointResult, 0, len(pendings))
	for _, p := range pendings {
		kept := make([]detect.Finding, 0, len(p.findings))
		for _, f := range p.findings {
			if persisted[f.ID()] {
				kept = append(kept, f)
			}
		}
		results = append(results, report.EndpointResult{
			Endpoint:        p.key,
			BaselineSamples: p.baseSamples,
			CurrentSamples:  p.currSamples,
			Findings:        kept,
		})
	}

	r := report.Build(now,
		report.Range{From: baselineFrom, To: currentFrom},
		report.Range{From: currentFrom, To: now},
		results)

	if lost := a.counters.panics.Load(); lost > 0 {
		r.Warnings = append(r.Warnings, fmt.Sprintf(
			"%d samples were lost to a contained analysis panic; the figures below rest on less data than the proxy saw",
			lost))
	}
	if failed > 0 {
		// Without this the report reads as "nothing changed" when what
		// actually happened is that nothing was examined. Today it fires for
		// every endpoint, because detect.Detect is a stub.
		r.Warnings = append(r.Warnings, fmt.Sprintf(
			"analysis failed for %d of %d endpoints; see detect_errors in /stats",
			failed, failed+len(pendings)))
	}

	candidates := a.selectCandidates()

	a.mu.Lock()
	a.latest = &r
	a.candidates = candidates
	a.mu.Unlock()

	return r
}

// selectCandidates advances the value epochs and returns the paths whose values
// moved enough to be worth a model's opinion.
//
// This is the gate that makes the judgement layer affordable. The statistical
// detector runs over every path; this runs over the few whose values shifted in
// a way that a cheap deterministic test can see. Sending every path to a model
// would cost a fortune and bury the useful answers among thousands of shrugs.
func (a *analyzer) selectCandidates() []assess.SemanticRequest {
	if a.values == nil {
		return nil
	}

	var out []assess.SemanticRequest
	for _, key := range a.values.Keys() {
		baseline, current, ok := a.values.Samples(key)
		if !ok {
			continue
		}
		shift := valuesample.DetectShift(baseline, current)
		if shift == nil {
			continue
		}
		out = append(out, assess.SemanticRequest{
			Endpoint: key.Endpoint,
			Path:     key.Path,
			Shift:    *shift,
			Baseline: baseline,
			Current:  current,
		})
	}

	// Rotate after selecting, so this window's values become the next
	// window's baseline.
	a.values.Rotate()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Endpoint != out[j].Endpoint {
			return out[i].Endpoint.String() < out[j].Endpoint.String()
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// semanticCandidates returns the paths selected by the most recent evaluation.
func (a *analyzer) semanticCandidates() []assess.SemanticRequest {
	a.mu.RLock()
	defer a.mu.RUnlock()

	return append([]assess.SemanticRequest(nil), a.candidates...)
}

// report returns the most recent evaluation, and whether one has happened yet.
func (a *analyzer) report() (report.Report, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	if a.latest == nil {
		return report.Report{}, false
	}
	return *a.latest, true
}

// stats returns a snapshot of the analyzer's counters.
func (a *analyzer) stats() analyzerStats {
	return analyzerStats{
		Consumed:     a.counters.consumed.Load(),
		Stored:       a.counters.stored.Load(),
		Unresolved:   a.counters.unresolved.Load(),
		ParseErrors:  a.counters.parseErrors.Load(),
		StaleDrops:   a.counters.staleDrops.Load(),
		Evaluations:  a.counters.evaluations.Load(),
		DetectErrors: a.counters.detectErrors.Load(),
		Panics:       a.counters.panics.Load(),

		SemanticCandidates: len(a.semanticCandidates()),
	}
}

// safeWindows fetches one endpoint's two windows, isolating a panic in the
// merge that builds them for the same reason safeDetect isolates one in the
// statistics: one endpoint's data must not cost the process.
func (a *analyzer) safeWindows(key endpoint.Key, baselineFrom, currentFrom, now time.Time) (baseSchema, currSchema *schema.Schema, okBase, okCurr bool, err error) {
	defer func() {
		if r := recover(); r != nil {
			baseSchema, currSchema, okBase, okCurr = nil, nil, false, false
			err = &detectFailed{cause: r}
		}
	}()

	baseSchema, okBase = a.store.Window(key, baselineFrom, currentFrom)
	currSchema, okCurr = a.store.Window(key, currentFrom, now)
	return baseSchema, currSchema, okBase, okCurr, nil
}

// detectFailed reports that one endpoint's detection pass did not complete.
type detectFailed struct{ cause any }

func (e *detectFailed) Error() string {
	return "detection failed"
}

// safeDetect isolates one endpoint's detection pass.
//
// Today this catches the deliberate panic in the detect.Detect stub, which is
// what lets the proxy and the reporting path be exercised end to end before the
// detector exists. It earns its place afterwards too: an arithmetic edge in one
// endpoint's statistics should cost that endpoint's findings, not the process
// that is proxying live traffic.
func safeDetect(baseline, current detect.Window, cfg detect.Config) (out []detect.Finding, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, &detectFailed{cause: r}
		}
	}()
	return detect.Detect(baseline, current, cfg), nil
}
