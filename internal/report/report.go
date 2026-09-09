package report

import (
	"sort"
	"time"

	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/endpoint"
)

// Range is a half-open period [From, To).
type Range struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// EndpointResult is what the analyser produces for one endpoint: the findings,
// and the sample counts that give them their weight.
type EndpointResult struct {
	// Endpoint is the endpoint compared.
	Endpoint endpoint.Key `json:"endpoint"`
	// BaselineSamples and CurrentSamples are the responses in each window.
	// They belong on the group rather than on each finding, since they are a
	// property of the comparison.
	BaselineSamples uint64 `json:"baseline_samples"`
	CurrentSamples  uint64 `json:"current_samples"`
	// Findings are the changes detected, in any order.
	Findings []detect.Finding `json:"findings"`
}

// Group is one endpoint's findings, ordered for display.
type Group struct {
	EndpointResult
	// MaxSeverity is the worst severity in the group, and what the groups are
	// ordered by.
	MaxSeverity detect.Severity `json:"max_severity"`
}

// Summary is the headline count, so a reader knows the size of the problem
// before reading any of it.
type Summary struct {
	// Findings is the total across all endpoints.
	Findings int `json:"findings"`
	// Endpoints is how many endpoints have at least one finding.
	Endpoints int `json:"endpoints"`
	// EndpointsCompared is how many were examined, including quiet ones.
	EndpointsCompared int `json:"endpoints_compared"`
	// BySeverity counts findings per severity name.
	BySeverity map[string]int `json:"by_severity"`
	// ByKind counts findings per change kind name.
	ByKind map[string]int `json:"by_kind"`
}

// Report is a rendered view of one detection run.
type Report struct {
	// GeneratedAt is when the report was produced.
	GeneratedAt time.Time `json:"generated_at"`
	// Baseline and Current are the periods compared.
	Baseline Range `json:"baseline"`
	Current  Range `json:"current"`
	// Summary is the headline count.
	Summary Summary `json:"summary"`
	// Groups are the endpoints with findings, worst first.
	Groups []Group `json:"groups"`
	// Warnings describe ways this report is incomplete -- endpoints whose
	// detection pass failed, for instance. An empty report and an empty report
	// that failed to examine anything look identical without them, and they are
	// very different pieces of news.
	Warnings []string `json:"warnings,omitempty"`
}

// Build assembles a Report from per-endpoint results.
//
// Endpoints with no findings are dropped from Groups but still counted in
// Summary.EndpointsCompared, so a quiet report is distinguishable from a report
// that examined nothing -- which are very different pieces of news.
//
// Ordering is total and deterministic: groups by worst severity then by
// endpoint, findings by severity then by p-value then by path. Reports get
// diffed against yesterday's, and an unstable order makes that useless.
func Build(generatedAt time.Time, baseline, current Range, results []EndpointResult) Report {
	r := Report{
		GeneratedAt: generatedAt,
		Baseline:    baseline,
		Current:     current,
		Summary: Summary{
			EndpointsCompared: len(results),
			BySeverity:        make(map[string]int),
			ByKind:            make(map[string]int),
		},
	}

	for _, res := range results {
		if len(res.Findings) == 0 {
			continue
		}

		// Copy before sorting: the caller's slice is not ours to reorder.
		findings := make([]detect.Finding, len(res.Findings))
		copy(findings, res.Findings)
		sortFindings(findings)

		worst := findings[0].Severity
		for _, f := range findings {
			if f.Severity > worst {
				worst = f.Severity
			}
			r.Summary.Findings++
			r.Summary.BySeverity[f.Severity.String()]++
			r.Summary.ByKind[f.Kind.String()]++
		}

		res.Findings = findings
		r.Groups = append(r.Groups, Group{EndpointResult: res, MaxSeverity: worst})
	}

	r.Summary.Endpoints = len(r.Groups)
	sortGroups(r.Groups)
	return r
}

// sortFindings orders within a group: worst first, then most significant, then
// by path so the order is total.
func sortFindings(f []detect.Finding) {
	sort.SliceStable(f, func(i, j int) bool {
		if f[i].Severity != f[j].Severity {
			return f[i].Severity > f[j].Severity
		}
		if f[i].PValue != f[j].PValue {
			return f[i].PValue < f[j].PValue
		}
		if f[i].Path != f[j].Path {
			return f[i].Path < f[j].Path
		}
		return f[i].Kind < f[j].Kind
	})
}

// sortGroups orders endpoints: worst first, then most findings, then by name.
func sortGroups(g []Group) {
	sort.SliceStable(g, func(i, j int) bool {
		if g[i].MaxSeverity != g[j].MaxSeverity {
			return g[i].MaxSeverity > g[j].MaxSeverity
		}
		if len(g[i].Findings) != len(g[j].Findings) {
			return len(g[i].Findings) > len(g[j].Findings)
		}
		return g[i].Endpoint.String() < g[j].Endpoint.String()
	})
}
