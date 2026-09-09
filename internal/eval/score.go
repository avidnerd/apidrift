package eval

import "github.com/avidnerd/apidrift/internal/detect"

// Counts is the confusion matrix for one change kind. There is no true-negative
// count: the negatives are every path that did not change, which is nearly all
// of them, so a true-negative rate would be ~1.0 for any detector and would
// measure nothing.
type Counts struct {
	TruePositives  int `json:"true_positives"`
	FalsePositives int `json:"false_positives"`
	FalseNegatives int `json:"false_negatives"`
}

// Recall is the share of real changes that were detected. It returns 0 when
// there was nothing to detect.
func (c Counts) Recall() float64 {
	if c.TruePositives+c.FalseNegatives == 0 {
		return 0
	}
	return float64(c.TruePositives) / float64(c.TruePositives+c.FalseNegatives)
}

// Precision is the share of findings that were real. It returns 0 when there
// were no findings.
func (c Counts) Precision() float64 {
	if c.TruePositives+c.FalsePositives == 0 {
		return 0
	}
	return float64(c.TruePositives) / float64(c.TruePositives+c.FalsePositives)
}

// ScoreResult is the tally of detector output against ground truth.
type ScoreResult struct {
	TruePositives  int                          `json:"true_positives"`
	FalsePositives int                          `json:"false_positives"`
	FalseNegatives int                          `json:"false_negatives"`
	ByKind         map[detect.ChangeKind]Counts `json:"-"`
	// ByKindName is ByKind rendered with string keys, since JSON object keys
	// must be strings and a numeric kind would be unreadable in the output.
	ByKindName map[string]Counts `json:"by_kind"`
}

// Totals returns the overall confusion matrix.
func (r ScoreResult) Totals() Counts {
	return Counts{
		TruePositives:  r.TruePositives,
		FalsePositives: r.FalsePositives,
		FalseNegatives: r.FalseNegatives,
	}
}

// Score matches findings against ground-truth changes and tallies the result.
//
// Matching is on identity -- endpoint, path and kind -- and all three must
// agree. A finding on the right path with the wrong kind is a false positive
// and leaves its truth entry a false negative: a tool that says "the type
// changed" when the field was removed has told you something false, and you
// will act on it wrongly. Partial credit would flatter the detector exactly
// where its output is most misleading.
//
// Both sides are deduplicated by identity first. Two findings about one change
// are one true positive, because the second adds no information and counting it
// twice would reward a detector for repeating itself.
func Score(findings []detect.Finding, truth []Change) ScoreResult {
	truthIDs := make(map[ID]bool, len(truth))
	for _, c := range truth {
		truthIDs[c.ID()] = true
	}

	findingIDs := make(map[ID]bool, len(findings))
	for _, f := range findings {
		findingIDs[findingID(f)] = true
	}

	res := ScoreResult{
		ByKind:     make(map[detect.ChangeKind]Counts),
		ByKindName: make(map[string]Counts),
	}

	// Each distinct finding is a hit or a mistake, and is filed under the kind
	// it claimed -- that is the assertion that was made.
	for id := range findingIDs {
		counts := res.ByKind[id.Kind]
		if truthIDs[id] {
			res.TruePositives++
			counts.TruePositives++
		} else {
			res.FalsePositives++
			counts.FalsePositives++
		}
		res.ByKind[id.Kind] = counts
	}

	// Each distinct real change was either found or missed, and a miss is filed
	// under the kind it actually was.
	for id := range truthIDs {
		if findingIDs[id] {
			continue
		}
		counts := res.ByKind[id.Kind]
		counts.FalseNegatives++
		res.ByKind[id.Kind] = counts
		res.FalseNegatives++
	}

	for kind, counts := range res.ByKind {
		res.ByKindName[kind.String()] = counts
	}
	return res
}

// findingID converts a detector finding to the identity ground truth uses.
func findingID(f detect.Finding) ID {
	return ID{Endpoint: f.Endpoint, Path: f.Path, Kind: f.Kind}
}
