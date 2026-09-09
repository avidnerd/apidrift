package detect_test

import (
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/schema"
)

// -- specification for a stub -------------------------------------------------
//
// detect.Detect is not implemented. Every test in this file fails today, on
// purpose: together they are the specification. See README.md for the prose
// version.

// detectSafe calls Detect and turns its TODO panic into an ordinary test
// failure, so that every specification below runs and reports independently
// rather than the first one taking down the binary.
func detectSafe(t *testing.T, baseline, current detect.Window, cfg detect.Config) (out []detect.Finding) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Detect panicked: %v", r)
		}
	}()
	return detect.Detect(baseline, current, cfg)
}

var (
	baseFrom = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	baseTo   = baseFrom.Add(24 * time.Hour)
	curFrom  = baseTo
	curTo    = curFrom.Add(time.Hour)
)

// presence builds a window in which one field was present in `present` of
// `seen` responses, all as strings.
func presence(from, to time.Time, path string, present, seen uint64) detect.Window {
	return detect.Window{
		From: from, To: to,
		Schema: &schema.Schema{
			Samples: seen,
			Fields: map[string]*schema.FieldStats{
				path: {
					Seen:         seen,
					Present:      present,
					TypeCounts:   map[schema.Kind]uint64{schema.KindString: present},
					StringValues: map[string]uint64{"open": present},
				},
			},
		},
	}
}

func window(from, to time.Time, samples uint64, fields map[string]*schema.FieldStats) detect.Window {
	return detect.Window{
		From: from, To: to,
		Schema: &schema.Schema{Samples: samples, Fields: fields},
	}
}

func findingFor(findings []detect.Finding, path string) (detect.Finding, bool) {
	for _, f := range findings {
		if f.Path == path {
			return f, true
		}
	}
	return detect.Finding{}, false
}

// TestDetectFieldRemoved is the headline positive case: a field that was
// always there is now mostly not.
func TestDetectFieldRemoved(t *testing.T) {
	baseline := presence(baseFrom, baseTo, "data.legacy_id", 2000, 2000)
	current := presence(curFrom, curTo, "data.legacy_id", 94, 100)

	got := detectSafe(t, baseline, current, detect.Config{MinSamples: 100})

	f, ok := findingFor(got, "data.legacy_id")
	if !ok {
		t.Fatalf("no finding for data.legacy_id; got %d findings: %+v", len(got), got)
	}
	if f.Kind != detect.FieldRemoved {
		t.Errorf("Kind = %v, want %v", f.Kind, detect.FieldRemoved)
	}
	if f.Severity != detect.SeverityHigh {
		t.Errorf("Severity = %v, want %v", f.Severity, detect.SeverityHigh)
	}
	if f.BaselineRate != 1.0 {
		t.Errorf("BaselineRate = %v, want 1.0", f.BaselineRate)
	}
	if f.CurrentRate != 0.94 {
		t.Errorf("CurrentRate = %v, want 0.94", f.CurrentRate)
	}
	if f.PValue <= 0 || f.PValue >= 0.05 {
		t.Errorf("PValue = %v, want a small positive value", f.PValue)
	}
	if f.Evidence == "" {
		t.Error("Evidence is empty; it must carry the raw counts")
	}
	if f.Endpoint != (detect.Finding{}).Endpoint {
		t.Errorf("Endpoint = %v, want the zero key: Detect does not know it, the caller stamps it", f.Endpoint)
	}
}

// TestDetectBelowEffectSize: significance is not enough. A difference has to be
// large enough to act on, or the tool reports weather.
func TestDetectBelowEffectSize(t *testing.T) {
	tests := []struct {
		name                  string
		basePresent, baseSeen uint64
		currPresent, currSeen uint64
		minEffect             float64
		wantFinding           bool
	}{
		{
			name:        "five points at n=100 is below a ten point threshold",
			basePresent: 60, baseSeen: 100,
			currPresent: 55, currSeen: 100,
			minEffect:   0.10,
			wantFinding: false,
		},
		{
			name:        "a tiny difference at enormous n is still below threshold",
			basePresent: 600000, baseSeen: 1000000,
			currPresent: 599000, currSeen: 1000000,
			minEffect:   0.10,
			wantFinding: false,
		},
		{
			name:        "exactly at the threshold with plenty of samples",
			basePresent: 2000, baseSeen: 2000,
			currPresent: 1800, currSeen: 2000,
			minEffect:   0.10,
			wantFinding: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseline := presence(baseFrom, baseTo, "status", tt.basePresent, tt.baseSeen)
			current := presence(curFrom, curTo, "status", tt.currPresent, tt.currSeen)

			got := detectSafe(t, baseline, current, detect.Config{
				MinSamples:    100,
				MinEffectSize: tt.minEffect,
			})

			_, ok := findingFor(got, "status")
			if ok != tt.wantFinding {
				t.Errorf("finding = %v, want %v (got %d findings: %+v)", ok, tt.wantFinding, len(got), got)
			}
		})
	}
}

// TestDetectBelowMinSamples: with too little data, the honest answer is
// silence, however dramatic the difference looks.
func TestDetectBelowMinSamples(t *testing.T) {
	tests := []struct {
		name        string
		baseSeen    uint64
		currSeen    uint64
		minSamples  uint64
		wantFinding bool
	}{
		{"both windows tiny", 3, 2, 200, false},
		{"baseline too small", 10, 5000, 200, false},
		{"current too small", 5000, 10, 200, false},
		{"baseline exactly at the threshold", 200, 5000, 200, true},
		{"both comfortably above", 5000, 5000, 200, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Always present in the baseline, never present in the current:
			// the largest possible difference.
			baseline := presence(baseFrom, baseTo, "status", tt.baseSeen, tt.baseSeen)
			current := presence(curFrom, curTo, "status", 0, tt.currSeen)

			got := detectSafe(t, baseline, current, detect.Config{MinSamples: tt.minSamples})

			_, ok := findingFor(got, "status")
			if ok != tt.wantFinding {
				t.Errorf("finding = %v, want %v (got %d findings)", ok, tt.wantFinding, len(got))
			}
		})
	}
}

// TestDetectFalsePositiveRateUnderTheNull is the guard the whole design exists
// to pass, and the number the evaluation reports as its headline.
//
// Three thousand fields, none of which changed: both windows are drawn from the
// same distribution. An uncorrected test at p < 0.05 would return roughly 150
// findings here purely by chance, which is a tool nobody reads twice.
func TestDetectFalsePositiveRateUnderTheNull(t *testing.T) {
	const (
		fields  = 3000
		samples = 2000
		// Benjamini-Hochberg controls the false discovery rate at alpha; under
		// the global null every rejection is false, so this is effectively a
		// bound on the chance of any rejection at all. A handful is tolerated
		// for the randomness of one seed; 150 is not.
		tolerated = 5
	)

	rng := rand.New(rand.NewSource(20260904))
	binomial := func(n int, p float64) uint64 {
		var k uint64
		for i := 0; i < n; i++ {
			if rng.Float64() < p {
				k++
			}
		}
		return k
	}

	baseFields := make(map[string]*schema.FieldStats, fields)
	currFields := make(map[string]*schema.FieldStats, fields)
	for i := 0; i < fields; i++ {
		path := fmt.Sprintf("data.field_%d", i)
		// A spread of presence rates, so the test is not all near 0.5 where
		// the variance is largest and any error would be most visible.
		p := 0.30 + 0.65*rng.Float64()

		for _, m := range []map[string]*schema.FieldStats{baseFields, currFields} {
			present := binomial(samples, p)
			m[path] = &schema.FieldStats{
				Seen:         samples,
				Present:      present,
				TypeCounts:   map[schema.Kind]uint64{schema.KindString: present},
				StringValues: map[string]uint64{"open": present},
			}
		}
	}

	baseline := window(baseFrom, baseTo, samples, baseFields)
	current := window(curFrom, curTo, samples, currFields)

	got := detectSafe(t, baseline, current, detect.DefaultConfig())

	if len(got) > tolerated {
		t.Errorf("%d findings from %d unchanged fields, want at most %d -- "+
			"multiple-comparison correction is what keeps this tool readable",
			len(got), fields, tolerated)
		for i, f := range got {
			if i >= 10 {
				t.Logf("... and %d more", len(got)-10)
				break
			}
			t.Logf("  false positive: %s %v p=%v %s", f.Path, f.Kind, f.PValue, f.Evidence)
		}
	}
}

// TestDetectTypeChangeIsCritical: a type flip breaks parsing, or worse, coerces
// silently. It is the most serious thing this tool can find.
func TestDetectTypeChangeIsCritical(t *testing.T) {
	baseline := window(baseFrom, baseTo, 2000, map[string]*schema.FieldStats{
		"data.amount": {
			Seen:       2000,
			Present:    2000,
			TypeCounts: map[schema.Kind]uint64{schema.KindString: 2000},
		},
	})
	current := window(curFrom, curTo, 2000, map[string]*schema.FieldStats{
		"data.amount": {
			Seen:       2000,
			Present:    2000,
			TypeCounts: map[schema.Kind]uint64{schema.KindInt: 2000},
		},
	})

	got := detectSafe(t, baseline, current, detect.DefaultConfig())

	f, ok := findingFor(got, "data.amount")
	if !ok {
		t.Fatalf("no finding for a string-to-int flip; got %d findings: %+v", len(got), got)
	}
	if f.Kind != detect.TypeChanged {
		t.Errorf("Kind = %v, want %v", f.Kind, detect.TypeChanged)
	}
	if f.Severity != detect.SeverityCritical {
		t.Errorf("Severity = %v, want %v", f.Severity, detect.SeverityCritical)
	}
}

// TestDetectNewFieldIsInfo: a field that appears breaks nobody. It is worth
// knowing about and not worth waking anyone for.
func TestDetectNewFieldIsInfo(t *testing.T) {
	baseline := window(baseFrom, baseTo, 2000, map[string]*schema.FieldStats{
		"data.id": {Seen: 2000, Present: 2000, TypeCounts: map[schema.Kind]uint64{schema.KindString: 2000}},
	})
	current := window(curFrom, curTo, 2000, map[string]*schema.FieldStats{
		"data.id":          {Seen: 2000, Present: 2000, TypeCounts: map[schema.Kind]uint64{schema.KindString: 2000}},
		"data.new_feature": {Seen: 2000, Present: 1200, TypeCounts: map[schema.Kind]uint64{schema.KindBool: 1200}},
	})

	got := detectSafe(t, baseline, current, detect.DefaultConfig())

	f, ok := findingFor(got, "data.new_feature")
	if !ok {
		t.Fatalf("no finding for a new field; got %d findings: %+v", len(got), got)
	}
	if f.Kind != detect.FieldAdded {
		t.Errorf("Kind = %v, want %v", f.Kind, detect.FieldAdded)
	}
	if f.Severity != detect.SeverityInfo {
		t.Errorf("Severity = %v, want %v", f.Severity, detect.SeverityInfo)
	}

	// The unchanged field alongside it must stay quiet.
	if _, ok := findingFor(got, "data.id"); ok {
		t.Error("an unchanged field produced a finding")
	}
}

// TestDetectNullabilityIntroduced: the key is still there, so a caller's
// lookup succeeds and then hands it a null. This is the change that breaks
// quietly rather than loudly.
func TestDetectNullabilityIntroduced(t *testing.T) {
	baseline := window(baseFrom, baseTo, 2000, map[string]*schema.FieldStats{
		"data.email": {
			Seen: 2000, Present: 2000, ExplicitNull: 0,
			TypeCounts: map[schema.Kind]uint64{schema.KindString: 2000},
		},
	})
	current := window(curFrom, curTo, 2000, map[string]*schema.FieldStats{
		"data.email": {
			Seen: 2000, Present: 2000, ExplicitNull: 800,
			TypeCounts: map[schema.Kind]uint64{schema.KindString: 1200, schema.KindNull: 800},
		},
	})

	got := detectSafe(t, baseline, current, detect.DefaultConfig())

	f, ok := findingFor(got, "data.email")
	if !ok {
		t.Fatalf("no finding for introduced nullability; got %d findings: %+v", len(got), got)
	}
	if f.Kind != detect.NullabilityIntroduced {
		t.Errorf("Kind = %v, want %v", f.Kind, detect.NullabilityIntroduced)
	}
	if f.Severity != detect.SeverityHigh {
		t.Errorf("Severity = %v, want %v", f.Severity, detect.SeverityHigh)
	}
}

// TestDetectEnumValueAdded covers new enum values, and the case where the value
// set is known to be incomplete and so cannot support the claim.
func TestDetectEnumValueAdded(t *testing.T) {
	tests := []struct {
		name             string
		baselineOverflow bool
		currentOverflow  bool
		wantFinding      bool
	}{
		{"complete value sets on both sides", false, false, true},
		{"baseline value set is partial", true, false, false},
		{"current value set is partial", false, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			baseline := window(baseFrom, baseTo, 2000, map[string]*schema.FieldStats{
				"status": {
					Seen: 2000, Present: 2000,
					TypeCounts:     map[schema.Kind]uint64{schema.KindString: 2000},
					StringValues:   map[string]uint64{"open": 1000, "paid": 1000},
					StringOverflow: tt.baselineOverflow,
				},
			})
			current := window(curFrom, curTo, 2000, map[string]*schema.FieldStats{
				"status": {
					Seen: 2000, Present: 2000,
					TypeCounts:     map[schema.Kind]uint64{schema.KindString: 2000},
					StringValues:   map[string]uint64{"open": 900, "paid": 900, "disputed": 200},
					StringOverflow: tt.currentOverflow,
				},
			})

			got := detectSafe(t, baseline, current, detect.DefaultConfig())

			f, ok := findingFor(got, "status")
			if ok != tt.wantFinding {
				t.Fatalf("finding = %v, want %v (got %+v)", ok, tt.wantFinding, got)
			}
			if !tt.wantFinding {
				return
			}
			if f.Kind != detect.EnumValueAdded {
				t.Errorf("Kind = %v, want %v", f.Kind, detect.EnumValueAdded)
			}
			if f.Severity != detect.SeverityInfo {
				t.Errorf("Severity = %v, want %v", f.Severity, detect.SeverityInfo)
			}
		})
	}
}

// TestDetectEmptyWindows covers the degenerate inputs the caller can hand it.
func TestDetectEmptyWindows(t *testing.T) {
	empty := window(baseFrom, baseTo, 0, map[string]*schema.FieldStats{})
	full := presence(curFrom, curTo, "status", 2000, 2000)

	tests := []struct {
		name              string
		baseline, current detect.Window
	}{
		{"both empty", empty, empty},
		{"empty baseline", empty, full},
		{"empty current", full, empty},
		{"nil baseline schema", detect.Window{From: baseFrom, To: baseTo}, full},
		{"nil current schema", full, detect.Window{From: curFrom, To: curTo}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectSafe(t, tt.baseline, tt.current, detect.DefaultConfig()); len(got) != 0 {
				t.Errorf("got %d findings from a degenerate window pair, want 0: %+v", len(got), got)
			}
		})
	}
}
