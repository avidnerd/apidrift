// Package valuesample retains a bounded, uniform sample of the actual values
// seen at each JSON path, alongside the structural statistics in package schema.
//
// It exists because of a limitation stated plainly in the project's README: a
// field that switches from cents to dollars changes neither its type nor its
// presence rate, so nothing in a Schema moves and no amount of tuning will make
// the detector fire. Catching that needs the values themselves, and a Schema
// deliberately does not keep them -- it keeps counts, because counts are what
// merge associatively and what a significance test can consume.
//
// So this is a parallel, deliberately separate store. It does not touch
// schema.FieldStats, whose shape is fixed by the detector's contract.
package valuesample

import (
	"bytes"
	"encoding/json"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/schema"
)

// Limits. Memory is bounded by MaxPaths x MaxPerPath x 2 epochs x the value
// size cap, which at the defaults is roughly 4096 x 32 x 2 x 96B ~= 25 MB.
const (
	// DefaultMaxPaths caps how many distinct JSON paths are sampled.
	DefaultMaxPaths = 4096
	// DefaultMaxPerPath is the reservoir size per path per epoch.
	DefaultMaxPerPath = 32
	// DefaultSampleRate is the share of bodies walked for values. Reservoir
	// sampling converges on a fraction of the traffic, and this walk is a
	// second pass over a body that schema.Extract has already parsed, so
	// spending it on every response buys precision nobody needs.
	DefaultSampleRate = 0.05
	// MaxValueBytes truncates a retained value. Values are evidence for a
	// judgement about units and formats, and no such judgement needs a
	// kilobyte of string.
	MaxValueBytes = 96
	// MaxDepth bounds the walk, matching schema.MaxDepth.
	MaxDepth = 32
)

// Key identifies one sampled path.
type Key struct {
	Endpoint endpoint.Key
	Path     string
}

// Value is one observed value, kept as the JSON text that produced it.
//
// The text rather than a parsed number, because the written form is itself
// evidence: "1000" and "10.00" are the same quantity and very different
// serialisations, and which one an upstream emits is exactly the sort of thing
// that changes without changing a type.
type Value struct {
	Kind schema.Kind `json:"kind"`
	Raw  string      `json:"raw"`
}

// Config tunes the store. Zero fields take their defaults.
type Config struct {
	MaxPaths   int
	MaxPerPath int
	// SampleRate is the probability that a given body is walked. Zero takes
	// DefaultSampleRate; use 1.0 to walk everything, which tests do.
	SampleRate float64
	// Seed makes sampling reproducible.
	Seed int64
}

func (c Config) withDefaults() Config {
	if c.MaxPaths <= 0 {
		c.MaxPaths = DefaultMaxPaths
	}
	if c.MaxPerPath <= 0 {
		c.MaxPerPath = DefaultMaxPerPath
	}
	if c.SampleRate <= 0 {
		c.SampleRate = DefaultSampleRate
	}
	return c
}

// reservoir is a uniform random sample of the values seen at one path, held at
// a fixed size however many values go past.
//
// Algorithm R: the first k values are kept; the i-th value after that replaces
// a uniformly chosen slot with probability k/i. Every value seen has the same
// chance of being in the final sample, which is the property that matters --
// keeping the first k would sample only the minutes after a deploy, and keeping
// the last k would sample only the minutes before the report.
type reservoir struct {
	values []Value
	seen   uint64
}

func (r *reservoir) offer(v Value, k int, rng *rand.Rand) {
	r.seen++
	if len(r.values) < k {
		r.values = append(r.values, v)
		return
	}
	if j := rng.Int63n(int64(r.seen)); j < int64(k) {
		r.values[j] = v
	}
}

// epoch pairs the two periods a comparison needs.
type epoch struct {
	baseline *reservoir
	current  *reservoir
}

// Store holds value samples for two consecutive periods.
//
// Two epochs rather than a ring of buckets: the only question asked of this
// data is "did the values at this path mean something different last window
// than this one", which needs exactly two sides. Retaining more would cost
// memory to answer a question nobody asks.
//
// Store is safe for concurrent use.
type Store struct {
	cfg Config

	mu     sync.Mutex
	paths  map[Key]*epoch
	rng    *rand.Rand
	walked uint64
	offers uint64
	capped uint64
}

// New returns a Store using cfg, with zero fields defaulted.
func New(cfg Config) *Store {
	cfg = cfg.withDefaults()
	return &Store{
		cfg:   cfg,
		paths: make(map[Key]*epoch),
		rng:   rand.New(rand.NewSource(cfg.Seed)),
	}
}

// Stats describes what the store holds.
type Stats struct {
	Paths  int    `json:"paths"`
	Walked uint64 `json:"bodies_walked"`
	Values uint64 `json:"values_offered"`
	Capped uint64 `json:"paths_dropped_at_cap"`
}

// Stats returns a snapshot.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	return Stats{Paths: len(s.paths), Walked: s.walked, Values: s.offers, Capped: s.capped}
}

// Observe walks a response body and files its scalar values, subject to the
// sample rate. It reports whether the body was walked.
func (s *Store) Observe(key endpoint.Key, body []byte) bool {
	s.mu.Lock()
	if s.rng.Float64() >= s.cfg.SampleRate {
		s.mu.Unlock()
		return false
	}
	s.walked++
	s.mu.Unlock()

	var doc any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.walk(key, "", doc, 0)
	return true
}

// walk files every scalar under v. Objects and arrays are containers, not
// values -- there is nothing about the meaning of a unit to learn from them.
func (s *Store) walk(key endpoint.Key, path string, v any, depth int) {
	if depth >= MaxDepth {
		return
	}

	switch tv := v.(type) {
	case map[string]any:
		names := make([]string, 0, len(tv))
		for k := range tv {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, name := range names {
			s.walk(key, joinPath(path, name), tv[name], depth+1)
		}
	case []any:
		for _, e := range tv {
			s.walk(key, path+schema.ArrayElem, e, depth+1)
		}
	default:
		if path == "" {
			return
		}
		s.offer(Key{Endpoint: key, Path: path}, scalarValue(v))
	}
}

// offer files one value into the current epoch.
func (s *Store) offer(k Key, v Value) {
	e, ok := s.paths[k]
	if !ok {
		if len(s.paths) >= s.cfg.MaxPaths {
			s.capped++
			return
		}
		e = &epoch{baseline: &reservoir{}, current: &reservoir{}}
		s.paths[k] = e
	}
	s.offers++
	e.current.offer(v, s.cfg.MaxPerPath, s.rng)
}

// Rotate advances the epochs: the current period becomes the baseline and a
// fresh current period begins. Call it when the detection window advances, so
// the two sides line up with the two windows the detector compared.
func (s *Store) Rotate() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for k, e := range s.paths {
		e.baseline = e.current
		e.current = &reservoir{}
		// A path that stopped appearing entirely holds nothing to compare.
		if len(e.baseline.values) == 0 {
			delete(s.paths, k)
		}
	}
}

// Samples returns the two periods' values for a path.
func (s *Store) Samples(k Key) (baseline, current []Value, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	e, found := s.paths[k]
	if !found {
		return nil, nil, false
	}
	return append([]Value(nil), e.baseline.values...), append([]Value(nil), e.current.values...), true
}

// Keys returns every sampled path.
func (s *Store) Keys() []Key {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]Key, 0, len(s.paths))
	for k := range s.paths {
		out = append(out, k)
	}
	return out
}

// scalarValue renders one JSON scalar as a Value.
func scalarValue(v any) Value {
	switch tv := v.(type) {
	case nil:
		return Value{Kind: schema.KindNull, Raw: "null"}
	case bool:
		return Value{Kind: schema.KindBool, Raw: strconv.FormatBool(tv)}
	case string:
		return Value{Kind: schema.KindString, Raw: truncate(tv)}
	case json.Number:
		kind := schema.KindInt
		if strings.ContainsAny(tv.String(), ".eE") {
			kind = schema.KindFloat
		}
		return Value{Kind: kind, Raw: truncate(tv.String())}
	}
	return Value{Kind: schema.KindNull, Raw: "null"}
}

func truncate(s string) string {
	if len(s) <= MaxValueBytes {
		return s
	}
	return s[:MaxValueBytes]
}

func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// -- the cheap pre-filter -----------------------------------------------------

// Shift describes a measurable difference between two value samples that is
// worth asking a language model about.
//
// This is the gate in front of the expensive layer, and the reason the design
// stays affordable. Sending every path to a model would cost a fortune and
// drown the useful answers; a deterministic filter that costs microseconds can
// discard the overwhelming majority of paths that plainly did not move.
type Shift struct {
	// Reason names what moved, for the prompt and the report.
	Reason string `json:"reason"`
	// MagnitudeRatio is the ratio of median absolute values, when both sides
	// are numeric. A cents-to-dollars change lands near 100 or 0.01.
	MagnitudeRatio float64 `json:"magnitude_ratio,omitempty"`
}

// magnitudeFactor is how far the median has to move before it is worth an
// opinion. Two-fold covers unit changes (100x), scale changes (1000x) and
// seconds-to-milliseconds (1000x) with room to spare, while ignoring the
// ordinary drift of a business metric.
const magnitudeFactor = 2.0

// DetectShift reports whether two samples differ in a way that could mean the
// values changed meaning while keeping their type -- and returns nil when they
// did not, which is the common case and the whole point.
func DetectShift(baseline, current []Value) *Shift {
	if len(baseline) < 4 || len(current) < 4 {
		// Too little to say anything. Silence is the honest answer, and it is
		// also the cheap one.
		return nil
	}

	if bMed, ok := medianAbs(baseline); ok {
		if cMed, ok2 := medianAbs(current); ok2 && bMed > 0 && cMed > 0 {
			ratio := cMed / bMed
			if ratio >= magnitudeFactor || ratio <= 1/magnitudeFactor {
				return &Shift{
					Reason:         "the median magnitude of this numeric field moved by " + formatRatio(ratio),
					MagnitudeRatio: ratio,
				}
			}
		}
	}

	if b, c := dominantFormat(baseline), dominantFormat(current); b != c && b != "" && c != "" {
		return &Shift{Reason: "the dominant string format changed from " + b + " to " + c}
	}

	if b, c := dominantKind(baseline), dominantKind(current); b != c {
		return &Shift{Reason: "the written number form changed from " + b.String() + " to " + c.String()}
	}
	return nil
}

// medianAbs returns the median absolute numeric value of a sample.
func medianAbs(vs []Value) (float64, bool) {
	nums := make([]float64, 0, len(vs))
	for _, v := range vs {
		if v.Kind != schema.KindInt && v.Kind != schema.KindFloat {
			continue
		}
		f, err := strconv.ParseFloat(v.Raw, 64)
		if err != nil {
			continue
		}
		nums = append(nums, math.Abs(f))
	}
	if len(nums) < 4 {
		return 0, false
	}
	sort.Float64s(nums)
	return nums[len(nums)/2], true
}

// dominantFormat classifies the shape of the majority of string values, so a
// change of encoding is visible without knowing what the field means.
func dominantFormat(vs []Value) string {
	counts := map[string]int{}
	total := 0
	for _, v := range vs {
		if v.Kind != schema.KindString {
			continue
		}
		counts[classifyString(v.Raw)]++
		total++
	}
	if total == 0 {
		return ""
	}
	best, bestN := "", 0
	for f, n := range counts {
		if n > bestN {
			best, bestN = f, n
		}
	}
	if float64(bestN)/float64(total) < 0.8 {
		return ""
	}
	return best
}

// classifyString buckets a string by shape. The buckets are chosen to separate
// encodings that carry the same information -- an RFC 3339 timestamp and a Unix
// epoch as a string are the same instant, and swapping one for the other breaks
// every consumer while changing no type.
func classifyString(s string) string {
	switch {
	case s == "":
		return "empty"
	case looksNumeric(s):
		return "numeric-string"
	case len(s) >= 20 && strings.Count(s, "-") >= 2 && (strings.Contains(s, "T") || strings.Contains(s, ":")):
		return "timestamp"
	case strings.Count(s, "-") == 4 && len(s) == 36:
		return "uuid"
	case strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://"):
		return "url"
	case strings.Contains(s, "@") && strings.Contains(s, "."):
		return "email"
	}
	return "text"
}

func looksNumeric(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.ParseFloat(s, 64)
	return err == nil
}

// dominantKind returns the most common Kind in a sample.
func dominantKind(vs []Value) schema.Kind {
	counts := map[schema.Kind]int{}
	for _, v := range vs {
		counts[v.Kind]++
	}
	best, bestN := schema.KindNull, -1
	for k, n := range counts {
		if n > bestN {
			best, bestN = k, n
		}
	}
	return best
}

func formatRatio(r float64) string {
	if r >= 1 {
		return strconv.FormatFloat(r, 'g', 3, 64) + "x"
	}
	return "1/" + strconv.FormatFloat(1/r, 'g', 3, 64) + "x"
}
