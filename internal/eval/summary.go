package eval

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/avidnerd/apidrift/internal/detect"
)

// WriteJSON writes a result as indented JSON.
func WriteJSON(w io.Writer, r *Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteSummary writes the human-readable evaluation.
//
// The false positive rate under the null is printed first and on its own,
// because it is the number that decides whether the tool is worth running. A
// detector that finds every real change and also cries wolf every hour gets
// muted within a week, at which point its recall is zero in practice whatever
// the table below says.
func WriteSummary(w io.Writer, r *Result) error {
	if _, err := fmt.Fprintf(w, "apidrift evaluation\n"); err != nil {
		return err
	}
	fmt.Fprintf(w, "  baseline spec   %s\n", r.SpecV1)
	fmt.Fprintf(w, "  current spec    %s\n", r.SpecV2)
	fmt.Fprintf(w, "  endpoints       %d\n", r.Endpoints)
	fmt.Fprintf(w, "  responses/window %d\n", r.ResponsesPerWindow)

	if r.Blocked != "" {
		// Printing the usual tables here would show a wall of zeros that reads
		// as "the detector found nothing", when what happened is that nothing
		// ran. Those are opposite conclusions and the output must not blur them.
		fmt.Fprintf(w, "\nNOT MEASURED\n")
		fmt.Fprintf(w, "  %s\n", r.Blocked)
		fmt.Fprintf(w, "  Ground truth below is still valid: it is computed from the specs alone.\n")
		writeGroundTruth(w, r)
		return nil
	}
	fmt.Fprintln(w)

	fmt.Fprintf(w, "false positives under the null (v1 traffic in both windows)\n")
	fmt.Fprintf(w, "  findings        %d\n", r.Null.Findings)
	fmt.Fprintf(w, "  paths tested    %d\n", r.Null.Paths)
	fmt.Fprintf(w, "  rate per path   %.5f\n", r.Null.PerPath)
	fmt.Fprintf(w, "  rate per endpoint %.3f\n\n", r.Null.PerEndpoint)

	totals := r.Score.Totals()
	fmt.Fprintf(w, "detection against ground truth\n")
	fmt.Fprintf(w, "  ground truth    %d changes\n", len(r.Truth))
	fmt.Fprintf(w, "  findings        %d\n", len(r.Findings))
	fmt.Fprintf(w, "  true positives  %d\n", totals.TruePositives)
	fmt.Fprintf(w, "  false positives %d\n", totals.FalsePositives)
	fmt.Fprintf(w, "  false negatives %d\n", totals.FalseNegatives)
	fmt.Fprintf(w, "  recall          %.3f\n", totals.Recall())
	fmt.Fprintf(w, "  precision       %.3f\n\n", totals.Precision())

	fmt.Fprintf(w, "recall by change kind\n")
	fmt.Fprintf(w, "  %-24s %8s %8s %8s %8s\n", "kind", "truth", "found", "missed", "recall")
	for _, kind := range detect.AllChangeKinds {
		c := r.Score.ByKind[kind]
		total := c.TruePositives + c.FalseNegatives
		if total == 0 {
			fmt.Fprintf(w, "  %-24s %8d %8s %8s %8s\n", kind, 0, "-", "-", "-")
			continue
		}
		fmt.Fprintf(w, "  %-24s %8d %8d %8d %8.3f\n", kind, total, c.TruePositives, c.FalseNegatives, c.Recall())
	}

	writeGroundTruth(w, r)
	return nil
}

// writeGroundTruth lists the changes the specs say happened.
func writeGroundTruth(w io.Writer, r *Result) {
	if len(r.Truth) == 0 {
		return
	}
	fmt.Fprintf(w, "\nground truth (%d changes)\n", len(r.Truth))

	truth := make([]Change, len(r.Truth))
	copy(truth, r.Truth)
	sort.Slice(truth, func(i, j int) bool { return truth[i].String() < truth[j].String() })
	for _, c := range truth {
		fmt.Fprintf(w, "  %s\n", c)
	}
}

// WriteLatencySummary writes the detection-latency headline: how many responses
// each presence rate needed before detection became more likely than not.
func WriteLatencySummary(w io.Writer, rows []LatencySummary, threshold float64) error {
	if _, err := fmt.Fprintf(w, "detection latency (responses needed for a %.0f%% chance of firing)\n", threshold*100); err != nil {
		return err
	}
	fmt.Fprintf(w, "  %-14s %s\n", "presence rate", "responses")
	for _, row := range rows {
		if !row.Reached {
			fmt.Fprintf(w, "  %-14.2f %s\n", row.PresenceRate, "not detected within the sweep")
			continue
		}
		fmt.Fprintf(w, "  %-14.2f %d\n", row.PresenceRate, row.ResponsesRequired)
	}
	return nil
}
