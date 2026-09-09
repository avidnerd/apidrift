package detect

import (
	"fmt"
	"math"
	"sort"

	"github.com/avidnerd/apidrift/internal/schema"
)

// The drift decision. Its contract was specified before it was written: the
// prose version is in this package's README.md, the executable version is
// TestDetect* in detect_test.go -- including the false-positive guard that runs
// the detector over 3,000 unchanged fields and requires near-silence.

// minPValue is what a p-value is clamped to when the normal tail underflows to
// zero. A finding with p exactly 0 would sort correctly but reads as a
// certainty no finite sample can support.
const minPValue = 1e-300

// effectEpsilon absorbs floating-point error when an effect sits exactly on the
// configured threshold. 1.0 - 0.9 is 0.09999999999999998, and a configured
// minimum of 0.10 should admit it rather than reject it on the last bit.
const effectEpsilon = 1e-9

// hypothesis is one comparison of a rate between the two windows, before the
// significance and correction gates.
type hypothesis struct {
	path string
	kind ChangeKind

	baseX, baseN uint64
	currX, currN uint64

	// detail is appended to the evidence line, e.g. "string became int".
	detail string

	baseRate, currRate float64
	pValue             float64
}

// effect is the absolute change in rate.
func (h hypothesis) effect() float64 { return math.Abs(h.currRate - h.baseRate) }

// evidence renders the raw counts, because rates alone hide the sample size
// that makes them meaningful.
func (h hypothesis) evidence() string {
	s := fmt.Sprintf("%d/%d baseline, %d/%d current", h.baseX, h.baseN, h.currX, h.currN)
	if h.detail != "" {
		s = h.detail + "; " + s
	}
	return s
}

// Detect compares a baseline window against a current window and returns
// findings that survive significance testing, effect-size gating, and
// multiple-comparison correction.
//
// Four gates, in order, each killing a different kind of false positive:
//
//  1. Sample size. Below cfg.MinSamples in either window, nothing is reported
//     however large the apparent difference: 1-for-1 becoming 0-for-1 is a 100%
//     drop and means nothing.
//  2. Effect size. A difference has to be big enough to act on. With enough
//     traffic a drop from 60.0% to 59.9% is significant and irrelevant.
//  3. Significance. A two-proportion test on the counts.
//  4. Multiple comparisons. Benjamini-Hochberg across every test performed --
//     without it, thousands of paths at p < 0.05 produce a wall of noise.
//
// Detect does not know which endpoint it was given; the caller stamps
// Finding.Endpoint afterwards.
func Detect(baseline, current Window, cfg Config) []Finding {
	minSamples := cfg.MinSamples
	if minSamples == 0 {
		minSamples = DefaultConfig().MinSamples
	}
	alpha := cfg.FDRAlpha
	if alpha == 0 {
		alpha = DefaultConfig().FDRAlpha
	}
	// MinEffectSize is used exactly as given. Unlike the others, zero is a
	// meaningful value here -- it means "do not gate on effect size" -- so
	// defaulting it would silently override a deliberate choice.
	minEffect := cfg.MinEffectSize

	if baseline.Schema == nil || current.Schema == nil {
		return nil
	}
	if baseline.Schema.Samples < minSamples || current.Schema.Samples < minSamples {
		return nil
	}

	// Gates 1 and 2: collect the hypotheses worth testing at all.
	var tested []hypothesis
	for _, path := range unionPaths(baseline.Schema, current.Schema) {
		for _, h := range hypothesesFor(path, baseline.Schema, current.Schema) {
			if h.effect() < minEffect-effectEpsilon {
				continue
			}
			// Gate 3.
			h.pValue = twoProportionP(h.baseX, h.baseN, h.currX, h.currN)
			tested = append(tested, h)
		}
	}

	// Gate 4.
	return toFindings(benjaminiHochberg(tested, alpha))
}

// hypothesesFor produces every comparison worth making about one path.
func hypothesesFor(path string, base, curr *schema.Schema) []hypothesis {
	fb, inBase := base.Fields[path]
	fc, inCurr := curr.Fields[path]

	switch {
	case !inBase && !inCurr:
		return nil

	case !inBase:
		// The path did not exist in the baseline at all.
		return []hypothesis{rate(path, FieldAdded,
			0, opportunity(base, path),
			fc.Present, fc.Seen, "")}

	case !inCurr:
		// The path is gone entirely, which is the strongest form of removal.
		return []hypothesis{rate(path, FieldRemoved,
			fb.Present, fb.Seen,
			0, opportunity(curr, path), "")}
	}

	var out []hypothesis

	// Presence. Only a drop is reported: a field that became *more* reliably
	// present has not broken anybody, and alerting on it would make every
	// upstream bug fix look like drift.
	if h := rate(path, FieldRemoved, fb.Present, fb.Seen, fc.Present, fc.Seen, ""); h.currRate < h.baseRate {
		out = append(out, h)
	}

	// Nullability, measured among the responses where the key existed.
	if h := rate(path, NullabilityIntroduced,
		fb.ExplicitNull, fb.Present, fc.ExplicitNull, fc.Present, ""); h.currRate > h.baseRate {
		out = append(out, h)
	}

	if h, ok := typeHypothesis(path, fb, fc); ok {
		out = append(out, h)
	}
	if h, ok := enumHypothesis(path, fb, fc); ok {
		out = append(out, h)
	}
	return out
}

// typeHypothesis returns the largest shift in any single JSON type's share.
//
// Shares are computed among *non-null* observations, deliberately. Nulls have
// their own change kind, and counting them here would make every introduced
// null also read as a type change -- one upstream event, reported twice, with
// the less useful of the two labels arriving first.
//
// One hypothesis per path rather than one per kind: a string becoming an int is
// a single event that moves two shares, and reporting it twice would double-
// count it in every downstream tally.
func typeHypothesis(path string, fb, fc *schema.FieldStats) (hypothesis, bool) {
	baseN := fb.Present - fb.ExplicitNull
	currN := fc.Present - fc.ExplicitNull
	if baseN == 0 || currN == 0 {
		return hypothesis{}, false
	}

	kinds := make(map[schema.Kind]bool, len(fb.TypeCounts)+len(fc.TypeCounts))
	for k := range fb.TypeCounts {
		kinds[k] = true
	}
	for k := range fc.TypeCounts {
		kinds[k] = true
	}

	ordered := make([]schema.Kind, 0, len(kinds))
	for k := range kinds {
		if k != schema.KindNull {
			ordered = append(ordered, k)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })

	var best hypothesis
	found := false
	for _, k := range ordered {
		h := rate(path, TypeChanged, fb.TypeCounts[k], baseN, fc.TypeCounts[k], currN,
			fmt.Sprintf("share of %s", k))
		if !found || h.effect() > best.effect() {
			best, found = h, true
		}
	}
	if !found || best.effect() == 0 {
		return hypothesis{}, false
	}

	if best.currRate < best.baseRate {
		best.detail = fmt.Sprintf("%s stopped being the dominant type", kindOfDetail(best.detail))
	} else {
		best.detail = fmt.Sprintf("%s became more common", kindOfDetail(best.detail))
	}
	return best, true
}

// kindOfDetail recovers the type name from the placeholder detail string.
func kindOfDetail(detail string) string {
	const prefix = "share of "
	if len(detail) > len(prefix) {
		return detail[len(prefix):]
	}
	return "a type"
}

// enumHypothesis returns the most common string value that is new in the
// current window.
//
// Nothing is reported when either side's value set overflowed its cardinality
// cap: the baseline is then a partial view, so "this value is new" is unproven
// rather than merely uncertain.
func enumHypothesis(path string, fb, fc *schema.FieldStats) (hypothesis, bool) {
	if fb.StringOverflow || fc.StringOverflow || len(fb.StringValues) == 0 {
		return hypothesis{}, false
	}

	values := make([]string, 0, len(fc.StringValues))
	for v := range fc.StringValues {
		if _, known := fb.StringValues[v]; !known {
			values = append(values, v)
		}
	}
	if len(values) == 0 {
		return hypothesis{}, false
	}
	sort.Strings(values)

	best, bestCount := "", uint64(0)
	for _, v := range values {
		if n := fc.StringValues[v]; n > bestCount {
			best, bestCount = v, n
		}
	}
	if best == "" {
		return hypothesis{}, false
	}

	h := rate(path, EnumValueAdded, 0, fb.Present, bestCount, fc.Present,
		fmt.Sprintf("new value %q", best))
	if len(values) > 1 {
		h.detail = fmt.Sprintf("%s and %d other new values", h.detail, len(values)-1)
	}
	return h, true
}

// opportunity returns the denominator for a path that is missing from a window:
// how many of that window's responses could have shown it.
//
// It differs from schema.OpportunityCount in the parent-absent case, and the
// difference matters. Merge answers "how many chances did this field have,
// given its parent existed", so no parent means no chances and the honest
// answer is zero. The detector is asking a different question -- "in how many
// responses did this path fail to appear" -- and when the container itself was
// never seen, the answer is all of them. Returning zero here would make the
// denominator vanish and the comparison untestable, which is how a genuinely
// new top-level object would go unreported.
func opportunity(s *schema.Schema, path string) uint64 {
	if n := schema.OpportunityCount(s, path); n > 0 {
		return n
	}
	return s.Samples
}

// rate builds a hypothesis and computes its two rates.
func rate(path string, kind ChangeKind, baseX, baseN, currX, currN uint64, detail string) hypothesis {
	h := hypothesis{
		path: path, kind: kind,
		baseX: baseX, baseN: baseN,
		currX: currX, currN: currN,
		detail: detail,
	}
	if baseN > 0 {
		h.baseRate = float64(baseX) / float64(baseN)
	}
	if currN > 0 {
		h.currRate = float64(currX) / float64(currN)
	}
	return h
}

// twoProportionP is a two-sided z-test on two proportions.
//
// A z-test rather than Fisher's exact: the sample-size gate keeps expected
// counts comfortably large, where the normal approximation is accurate, and it
// is orders of magnitude cheaper across thousands of paths per window.
func twoProportionP(x1, n1, x2, n2 uint64) float64 {
	if n1 == 0 || n2 == 0 {
		return 1
	}
	p1 := float64(x1) / float64(n1)
	p2 := float64(x2) / float64(n2)

	pooled := float64(x1+x2) / float64(n1+n2)
	se := math.Sqrt(pooled * (1 - pooled) * (1/float64(n1) + 1/float64(n2)))
	if se == 0 {
		// Both windows agree exactly, at 0% or at 100%. There is no difference
		// to be significant.
		return 1
	}

	p := math.Erfc(math.Abs(p1-p2) / (se * math.Sqrt2))
	switch {
	case p < minPValue:
		return minPValue
	case p > 1:
		return 1
	}
	return p
}

// benjaminiHochberg returns the hypotheses that survive false-discovery-rate
// control at alpha.
//
// Sort the p-values ascending; find the largest k where p(k) <= (k/m)*alpha;
// reject ranks 1..k. This controls the expected *share* of findings that are
// false, which is the right thing to control for a list a human reads --
// Bonferroni controls the chance of any false positive at all, and at three
// thousand tests it is so conservative that real changes stop being reported.
func benjaminiHochberg(hs []hypothesis, alpha float64) []hypothesis {
	m := len(hs)
	if m == 0 {
		return nil
	}

	sorted := make([]hypothesis, m)
	copy(sorted, hs)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].pValue < sorted[j].pValue })

	k := 0
	for i, h := range sorted {
		if h.pValue <= float64(i+1)/float64(m)*alpha {
			k = i + 1
		}
	}
	return sorted[:k]
}

// toFindings renders surviving hypotheses, in a stable order.
func toFindings(hs []hypothesis) []Finding {
	out := make([]Finding, 0, len(hs))
	for _, h := range hs {
		out = append(out, Finding{
			Path:         h.path,
			Kind:         h.kind,
			Severity:     severityOf(h.kind),
			BaselineRate: h.baseRate,
			CurrentRate:  h.currRate,
			PValue:       h.pValue,
			Evidence:     h.evidence(),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// severityOf maps a change kind to how much it should worry the reader.
func severityOf(k ChangeKind) Severity {
	switch k {
	case TypeChanged:
		// Breaks parsing outright, or worse, coerces silently.
		return SeverityCritical
	case FieldRemoved, NullabilityIntroduced:
		// A caller that read the field now gets nothing, or a null.
		return SeverityHigh
	default:
		// A new field or a new enum value breaks nobody today.
		return SeverityInfo
	}
}

// unionPaths returns every path in either schema, sorted.
func unionPaths(a, b *schema.Schema) []string {
	seen := make(map[string]bool, len(a.Fields)+len(b.Fields))
	out := make([]string, 0, len(a.Fields)+len(b.Fields))
	for _, s := range []*schema.Schema{a, b} {
		for p := range s.Fields {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}
