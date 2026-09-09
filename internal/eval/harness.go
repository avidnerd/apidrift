package eval

import (
	"fmt"
	"time"

	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/schema"
)

// StubError reports that a run could not finish because one of the deliberately
// unimplemented functions was reached.
//
// The harness reports this rather than crashing, so that everything up to the
// stub -- spec loading, diffing, generation, extraction -- is still exercised
// and its failures are still visible.
type StubError struct {
	// Stage names what the harness was doing.
	Stage string
	// Cause is the recovered panic value.
	Cause any
}

func (e *StubError) Error() string {
	return fmt.Sprintf("eval: %s could not run: %v", e.Stage, e.Cause)
}

// RunConfig configures one evaluation.
type RunConfig struct {
	// V1 and V2 are the two spec versions. V1 supplies the baseline window and
	// V2 the current one.
	V1, V2 *Spec
	// Gen configures the traffic generator.
	Gen GenConfig
	// Responses is how many responses are generated per endpoint per window.
	Responses int
	// Detect configures the detector.
	Detect detect.Config
	// Now is the timestamp the windows are labelled with.
	Now time.Time
}

func (c RunConfig) withDefaults() RunConfig {
	if c.Responses <= 0 {
		c.Responses = 2000
	}
	if c.Now.IsZero() {
		c.Now = time.Unix(0, 0).UTC()
	}
	return c
}

// NullResult is the false-positive measurement: v1 traffic in both windows, so
// every finding is by construction false.
type NullResult struct {
	// Findings is how many findings were produced when nothing changed.
	Findings int `json:"findings"`
	// Endpoints is how many endpoints were compared.
	Endpoints int `json:"endpoints"`
	// Paths is how many distinct JSON paths were under test, which is the
	// number of chances the detector had to be wrong.
	Paths int `json:"paths"`
	// PerEndpoint is Findings/Endpoints.
	PerEndpoint float64 `json:"per_endpoint"`
	// PerPath is Findings/Paths -- the false positive rate proper.
	PerPath float64 `json:"per_path"`
}

// Result is one evaluation's output.
type Result struct {
	// Spec describes what was compared.
	SpecV1 string `json:"spec_v1"`
	SpecV2 string `json:"spec_v2"`
	// Endpoints is how many endpoints both specs define.
	Endpoints int `json:"endpoints"`
	// ResponsesPerWindow is how many responses each window was built from.
	ResponsesPerWindow int `json:"responses_per_window"`
	// Truth is the ground-truth changelist.
	Truth []Change `json:"truth"`
	// Findings is what the detector reported.
	Findings []detect.Finding `json:"findings"`
	// Score is the tally against ground truth.
	Score ScoreResult `json:"score"`
	// Null is the false-positive measurement.
	Null NullResult `json:"null"`
	// Blocked, when non-empty, says why the measurements were not taken. A
	// result with it set carries valid ground truth and nothing else -- the
	// zeros in Score and Null are unmeasured, not measured to be zero, and
	// every reader of this struct has to be able to tell those apart.
	Blocked string `json:"blocked,omitempty"`
}

// RecallByKind returns recall for each change kind that appears in ground truth.
func (r *Result) RecallByKind() map[string]float64 {
	out := make(map[string]float64)
	for kind, counts := range r.Score.ByKind {
		if counts.TruePositives+counts.FalseNegatives == 0 {
			continue
		}
		out[kind.String()] = counts.Recall()
	}
	return out
}

// Run performs one evaluation end to end.
func Run(cfg RunConfig) (*Result, error) {
	cfg = cfg.withDefaults()

	ids := ComparableEndpoints(cfg.V1, cfg.V2)
	res := &Result{
		SpecV1:             describeSpec(cfg.V1),
		SpecV2:             describeSpec(cfg.V2),
		Endpoints:          len(ids),
		ResponsesPerWindow: cfg.Responses,
		Truth:              Diff(cfg.V1, cfg.V2),
	}

	// Separate seeds per window, so the two windows are independent draws
	// rather than the same draw twice. Sharing a seed would make the null run
	// produce two byte-identical windows and report a false positive rate of
	// zero that meant nothing at all.
	baseGen := NewGenerator(cfg.V1, seeded(cfg.Gen, cfg.Gen.Seed))
	currGen := NewGenerator(cfg.V2, seeded(cfg.Gen, cfg.Gen.Seed+1))

	findings, _, err := compare(ids, baseGen, currGen, cfg)
	if err != nil {
		res.Blocked = err.Error()
		return res, err
	}
	res.Findings = findings

	score, err := safeScore(findings, res.Truth)
	if err != nil {
		res.Blocked = err.Error()
		return res, err
	}
	res.Score = score

	// The null run: v1 on both sides, with a third seed so the two windows
	// differ by sampling alone.
	nullBase := NewGenerator(cfg.V1, seeded(cfg.Gen, cfg.Gen.Seed+2))
	nullCurr := NewGenerator(cfg.V1, seeded(cfg.Gen, cfg.Gen.Seed+3))

	nullFindings, paths, err := compare(ids, nullBase, nullCurr, cfg)
	if err != nil {
		res.Blocked = err.Error()
		return res, err
	}
	res.Null = NullResult{
		Findings:  len(nullFindings),
		Endpoints: len(ids),
		Paths:     paths,
	}
	if len(ids) > 0 {
		res.Null.PerEndpoint = float64(len(nullFindings)) / float64(len(ids))
	}
	if paths > 0 {
		res.Null.PerPath = float64(len(nullFindings)) / float64(paths)
	}

	return res, nil
}

// compare builds both windows for every endpoint and runs the detector,
// returning the findings and how many distinct paths were tested.
func compare(ids []EndpointID, baseGen, currGen *Generator, cfg RunConfig) ([]detect.Finding, int, error) {
	var (
		findings []detect.Finding
		paths    int
	)
	for _, id := range ids {
		baseSchema, err := buildWindow(baseGen, id, cfg.Responses)
		if err != nil {
			return nil, 0, err
		}
		currSchema, err := buildWindow(currGen, id, cfg.Responses)
		if err != nil {
			return nil, 0, err
		}
		paths += countPaths(baseSchema, currSchema)

		found, err := safeDetect(
			detect.Window{From: cfg.Now.Add(-2 * time.Hour), To: cfg.Now.Add(-time.Hour), Schema: baseSchema},
			detect.Window{From: cfg.Now.Add(-time.Hour), To: cfg.Now, Schema: currSchema},
			cfg.Detect,
		)
		if err != nil {
			return nil, 0, err
		}

		key := id.Key()
		for i := range found {
			found[i].Endpoint = key
		}
		findings = append(findings, found...)
	}
	return findings, paths, nil
}

// buildWindow generates n responses and merges them into one schema, which is
// exactly what the store does with a window's worth of traffic.
func buildWindow(g *Generator, id EndpointID, n int) (out *schema.Schema, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, &StubError{Stage: "building a window (schema.Merge)", Cause: r}
		}
	}()

	for i := 0; i < n; i++ {
		body, genErr := g.Body(id)
		if genErr != nil {
			return nil, genErr
		}
		one, exErr := schema.Extract(body)
		if exErr != nil {
			return nil, fmt.Errorf("eval: extracting a generated body for %s: %w", id, exErr)
		}
		if out == nil {
			// The first response needs no merging, which is both marginally
			// cheaper and what lets a one-response window be built while
			// schema.Merge is a stub.
			out = one
			continue
		}
		out = schema.Merge(out, one)
	}
	if out == nil {
		out = schema.New()
	}
	return out, nil
}

// countPaths returns how many distinct JSON paths appear across two schemas.
// It is the number of opportunities the detector had to report something, and
// so the denominator the null run's rate is quoted against.
func countPaths(a, b *schema.Schema) int {
	seen := make(map[string]bool, len(a.Fields)+len(b.Fields))
	for _, s := range []*schema.Schema{a, b} {
		if s == nil {
			continue
		}
		for p := range s.Fields {
			seen[p] = true
		}
	}
	return len(seen)
}

// safeDetect isolates the Detect stub's panic.
func safeDetect(baseline, current detect.Window, cfg detect.Config) (out []detect.Finding, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = nil, &StubError{Stage: "detection (detect.Detect)", Cause: r}
		}
	}()
	return detect.Detect(baseline, current, cfg), nil
}

// safeScore isolates the Score stub's panic.
func safeScore(findings []detect.Finding, truth []Change) (out ScoreResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			out, err = ScoreResult{}, &StubError{Stage: "scoring (eval.Score)", Cause: r}
		}
	}()
	return Score(findings, truth), nil
}

// seeded returns cfg with a different seed.
func seeded(cfg GenConfig, seed int64) GenConfig {
	cfg.Seed = seed
	return cfg
}

// describeSpec renders a spec's identity for the report.
func describeSpec(s *Spec) string {
	switch {
	case s == nil:
		return "(none)"
	case s.Title != "" && s.Version != "":
		return s.Title + " " + s.Version
	case s.Title != "":
		return s.Title
	case s.Version != "":
		return s.Version
	}
	return "(untitled)"
}
