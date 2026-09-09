package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"
)

// Column widths for the human-readable form. Severity and kind are padded so
// findings line up down the page and the eye can scan one column.
const (
	severityWidth = 8  // "CRITICAL"
	kindWidth     = 22 // "nullability_introduced"
)

// WriteJSON writes the report as indented JSON, for machine consumers.
func WriteJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteText writes the report in the human-readable form.
//
// The layout puts the identity of the change first and the statistics second,
// because the reader's first question is always "what changed, and where". The
// evidence line carrying the raw counts comes last: it is what settles an
// argument about whether a rate is trustworthy, and nothing else needs it.
func WriteText(w io.Writer, r Report) error {
	bw := &errWriter{w: w}

	if r.Summary.Findings == 0 {
		fmt.Fprintf(bw, "apidrift: no findings across %s\n", plural(r.Summary.EndpointsCompared, "endpoint"))
	} else {
		fmt.Fprintf(bw, "apidrift: %s across %s (%s compared)\n",
			plural(r.Summary.Findings, "finding"),
			plural(r.Summary.Endpoints, "endpoint"),
			plural(r.Summary.EndpointsCompared, "endpoint"))
		fmt.Fprintf(bw, "          %s\n", severityBreakdown(r.Summary))
	}

	fmt.Fprintf(bw, "baseline  %s\n", formatRange(r.Baseline))
	fmt.Fprintf(bw, "current   %s\n", formatRange(r.Current))
	fmt.Fprintf(bw, "generated %s\n", r.GeneratedAt.UTC().Format(time.RFC3339))

	for _, warning := range r.Warnings {
		fmt.Fprintf(bw, "warning:  %s\n", warning)
	}

	for _, g := range r.Groups {
		fmt.Fprintf(bw, "\n%s\n", g.Endpoint)
		fmt.Fprintf(bw, "  %s baseline, %s current\n",
			plural(int(g.BaselineSamples), "sample"), plural(int(g.CurrentSamples), "sample"))

		for _, f := range g.Findings {
			fmt.Fprintf(bw, "\n  %-*s %-*s %s\n",
				severityWidth, strings.ToUpper(f.Severity.String()),
				kindWidth, f.Kind,
				f.Path)
			fmt.Fprintf(bw, "  %-*s %s → %s   p=%s\n",
				severityWidth, "",
				formatRate(f.BaselineRate), formatRate(f.CurrentRate), formatP(f.PValue))
			if f.Evidence != "" {
				fmt.Fprintf(bw, "  %-*s %s\n", severityWidth, "", f.Evidence)
			}
		}
	}

	return bw.err
}

// errWriter records the first write error and skips the rest, so the formatting
// above stays readable instead of checking an error after every line.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	n, err := e.w.Write(p)
	e.err = err
	return n, err
}

// severityBreakdown renders the per-severity counts, worst first, omitting
// severities with no findings.
func severityBreakdown(s Summary) string {
	parts := make([]string, 0, 3)
	for _, name := range []string{"critical", "high", "info"} {
		if n := s.BySeverity[name]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, name))
		}
	}
	return strings.Join(parts, ", ")
}

// formatRange renders a period, or a placeholder when it is unset.
func formatRange(r Range) string {
	if r.From.IsZero() && r.To.IsZero() {
		return "(unset)"
	}
	return r.From.UTC().Format(time.RFC3339) + " → " + r.To.UTC().Format(time.RFC3339)
}

// formatRate renders a presence rate as a percentage with one decimal, which is
// the precision the sample sizes here can actually support.
func formatRate(v float64) string {
	return fmt.Sprintf("%.1f%%", v*100)
}

// formatP renders a p-value, switching to scientific notation once the decimal
// form stops being informative.
func formatP(v float64) string {
	switch {
	case v == 0:
		return "0"
	case v < 0.0001:
		return fmt.Sprintf("%.1e", v)
	default:
		return fmt.Sprintf("%.4f", v)
	}
}

// plural renders a count with its noun, pluralised.
func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
