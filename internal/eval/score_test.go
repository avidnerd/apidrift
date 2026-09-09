package eval_test

import (
	"testing"

	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/eval"
)

// -- specification for a stub -------------------------------------------------
//
// eval.Score is not implemented. Every test in this file fails today, on
// purpose: together they are the specification. See README.md for the prose
// version.

// scoreSafe calls Score and turns its TODO panic into an ordinary test failure,
// so every specification below runs and reports independently.
func scoreSafe(t *testing.T, findings []detect.Finding, truth []eval.Change) (out eval.ScoreResult) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Score panicked: %v", r)
		}
	}()
	return eval.Score(findings, truth)
}

var (
	epA = endpoint.Key{Method: "GET", Template: "/v1/customers/{id}", StatusClass: 2}
	epB = endpoint.Key{Method: "GET", Template: "/v1/charges", StatusClass: 2}
)

func truthOf(ep endpoint.Key, path string, kind detect.ChangeKind) eval.Change {
	return eval.Change{Endpoint: ep, Path: path, Kind: kind, Detail: "test"}
}

func findingOf(ep endpoint.Key, path string, kind detect.ChangeKind) detect.Finding {
	return detect.Finding{Endpoint: ep, Path: path, Kind: kind, Severity: detect.SeverityHigh}
}

func TestScoreBasicTally(t *testing.T) {
	tests := []struct {
		name                   string
		findings               []detect.Finding
		truth                  []eval.Change
		wantTP, wantFP, wantFN int
	}{
		{
			name:   "nothing at all",
			wantTP: 0, wantFP: 0, wantFN: 0,
		},
		{
			name:     "an exact match",
			findings: []detect.Finding{findingOf(epA, "legacy_id", detect.FieldRemoved)},
			truth:    []eval.Change{truthOf(epA, "legacy_id", detect.FieldRemoved)},
			wantTP:   1,
		},
		{
			name:     "a finding with no truth behind it",
			findings: []detect.Finding{findingOf(epA, "invented", detect.FieldRemoved)},
			wantFP:   1,
		},
		{
			name:   "a change nobody found",
			truth:  []eval.Change{truthOf(epA, "legacy_id", detect.FieldRemoved)},
			wantFN: 1,
		},
		{
			name: "a mix",
			findings: []detect.Finding{
				findingOf(epA, "legacy_id", detect.FieldRemoved),
				findingOf(epA, "invented", detect.FieldRemoved),
			},
			truth: []eval.Change{
				truthOf(epA, "legacy_id", detect.FieldRemoved),
				truthOf(epA, "balance", detect.TypeChanged),
			},
			wantTP: 1, wantFP: 1, wantFN: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scoreSafe(t, tt.findings, tt.truth)
			if got.TruePositives != tt.wantTP || got.FalsePositives != tt.wantFP || got.FalseNegatives != tt.wantFN {
				t.Errorf("TP/FP/FN = %d/%d/%d, want %d/%d/%d",
					got.TruePositives, got.FalsePositives, got.FalseNegatives,
					tt.wantTP, tt.wantFP, tt.wantFN)
			}
		})
	}
}

// TestScoreWrongKindIsNotATruePositive: a tool that says "the type changed" when
// the field was actually removed has told you something false, and you will act
// on it wrongly. Partial credit here would flatter the detector exactly where
// its output is most misleading.
func TestScoreWrongKindIsNotATruePositive(t *testing.T) {
	got := scoreSafe(t,
		[]detect.Finding{findingOf(epA, "legacy_id", detect.TypeChanged)},
		[]eval.Change{truthOf(epA, "legacy_id", detect.FieldRemoved)},
	)

	if got.TruePositives != 0 {
		t.Errorf("TruePositives = %d, want 0: the path matched but the claim was wrong", got.TruePositives)
	}
	if got.FalsePositives != 1 {
		t.Errorf("FalsePositives = %d, want 1: the finding asserted something untrue", got.FalsePositives)
	}
	if got.FalseNegatives != 1 {
		t.Errorf("FalseNegatives = %d, want 1: the real change went unreported", got.FalseNegatives)
	}
}

// TestScoreDuplicateFindingsCountOnce: the second report of a change adds no
// information, and letting it add a point would reward repetition.
func TestScoreDuplicateFindingsCountOnce(t *testing.T) {
	f := findingOf(epA, "legacy_id", detect.FieldRemoved)

	got := scoreSafe(t,
		[]detect.Finding{f, f, f},
		[]eval.Change{truthOf(epA, "legacy_id", detect.FieldRemoved)},
	)

	if got.TruePositives != 1 {
		t.Errorf("TruePositives = %d, want 1 for three reports of one change", got.TruePositives)
	}
	if got.FalsePositives != 0 {
		t.Errorf("FalsePositives = %d, want 0: the duplicates are not separate mistakes", got.FalsePositives)
	}
}

func TestScoreDuplicateFalsePositivesCountOnce(t *testing.T) {
	f := findingOf(epA, "invented", detect.FieldRemoved)

	got := scoreSafe(t, []detect.Finding{f, f}, nil)
	if got.FalsePositives != 1 {
		t.Errorf("FalsePositives = %d, want 1 for two reports of one non-change", got.FalsePositives)
	}
}

// TestScoreMatchesOnEndpointToo: the same path on a different endpoint is a
// different change. Ignoring the endpoint would let a finding on /charges take
// credit for a change on /customers.
func TestScoreMatchesOnEndpointToo(t *testing.T) {
	got := scoreSafe(t,
		[]detect.Finding{findingOf(epB, "status", detect.FieldRemoved)},
		[]eval.Change{truthOf(epA, "status", detect.FieldRemoved)},
	)

	if got.TruePositives != 0 {
		t.Errorf("TruePositives = %d, want 0: the endpoints differ", got.TruePositives)
	}
	if got.FalsePositives != 1 || got.FalseNegatives != 1 {
		t.Errorf("FP/FN = %d/%d, want 1/1", got.FalsePositives, got.FalseNegatives)
	}
}

// TestScoreByKind: recall is reported per change kind, so a detector that is
// excellent at removals and blind to type flips is not hidden behind one
// average.
func TestScoreByKind(t *testing.T) {
	findings := []detect.Finding{
		findingOf(epA, "legacy_id", detect.FieldRemoved), // true positive
		findingOf(epA, "address", detect.FieldRemoved),   // true positive
		findingOf(epA, "invented", detect.TypeChanged),   // false positive
	}
	truth := []eval.Change{
		truthOf(epA, "legacy_id", detect.FieldRemoved),
		truthOf(epA, "address", detect.FieldRemoved),
		truthOf(epA, "balance", detect.TypeChanged), // false negative
		truthOf(epB, "data[].status", detect.EnumValueAdded),
	}

	got := scoreSafe(t, findings, truth)

	removed := got.ByKind[detect.FieldRemoved]
	if removed.TruePositives != 2 || removed.FalseNegatives != 0 || removed.FalsePositives != 0 {
		t.Errorf("FieldRemoved = %+v, want 2 true positives and nothing else", removed)
	}
	if got := removed.Recall(); got != 1.0 {
		t.Errorf("FieldRemoved recall = %v, want 1.0", got)
	}

	// The false positive is filed under the kind the finding claimed.
	typed := got.ByKind[detect.TypeChanged]
	if typed.FalsePositives != 1 {
		t.Errorf("TypeChanged false positives = %d, want 1 (filed under the claim that was made)", typed.FalsePositives)
	}
	if typed.FalseNegatives != 1 {
		t.Errorf("TypeChanged false negatives = %d, want 1", typed.FalseNegatives)
	}
	if got := typed.Recall(); got != 0.0 {
		t.Errorf("TypeChanged recall = %v, want 0.0", got)
	}

	enum := got.ByKind[detect.EnumValueAdded]
	if enum.FalseNegatives != 1 {
		t.Errorf("EnumValueAdded false negatives = %d, want 1", enum.FalseNegatives)
	}

	// Per-kind counts must add up to the totals, or the report contradicts
	// itself between its summary and its breakdown.
	var tp, fp, fn int
	for _, c := range got.ByKind {
		tp, fp, fn = tp+c.TruePositives, fp+c.FalsePositives, fn+c.FalseNegatives
	}
	if tp != got.TruePositives || fp != got.FalsePositives || fn != got.FalseNegatives {
		t.Errorf("per-kind totals %d/%d/%d do not match overall %d/%d/%d",
			tp, fp, fn, got.TruePositives, got.FalsePositives, got.FalseNegatives)
	}
}

// TestScoreByKindName guards the JSON-facing copy, which must not drift from
// the typed map.
func TestScoreByKindName(t *testing.T) {
	got := scoreSafe(t,
		[]detect.Finding{findingOf(epA, "legacy_id", detect.FieldRemoved)},
		[]eval.Change{truthOf(epA, "legacy_id", detect.FieldRemoved)},
	)

	if len(got.ByKindName) == 0 {
		t.Fatal("ByKindName is empty; JSON output would carry no breakdown")
	}
	for kind, counts := range got.ByKind {
		named, ok := got.ByKindName[kind.String()]
		if !ok {
			t.Errorf("ByKindName has no entry for %v", kind)
			continue
		}
		if named != counts {
			t.Errorf("ByKindName[%v] = %+v, want %+v", kind, named, counts)
		}
	}
}

// TestCounts covers the arithmetic on the result type, which is implemented.
func TestCounts(t *testing.T) {
	tests := []struct {
		name                    string
		counts                  eval.Counts
		wantRecall, wantPrecise float64
	}{
		{"perfect", eval.Counts{TruePositives: 10}, 1.0, 1.0},
		{"half recall", eval.Counts{TruePositives: 5, FalseNegatives: 5}, 0.5, 1.0},
		{"half precision", eval.Counts{TruePositives: 5, FalsePositives: 5}, 1.0, 0.5},
		{"nothing to find", eval.Counts{}, 0, 0},
		{"found nothing real", eval.Counts{FalsePositives: 3, FalseNegatives: 2}, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.counts.Recall(); got != tt.wantRecall {
				t.Errorf("Recall() = %v, want %v", got, tt.wantRecall)
			}
			if got := tt.counts.Precision(); got != tt.wantPrecise {
				t.Errorf("Precision() = %v, want %v", got, tt.wantPrecise)
			}
		})
	}
}
