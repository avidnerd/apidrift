package report_test

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/report"
)

var (
	now      = time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	baseline = report.Range{From: now.Add(-25 * time.Hour), To: now.Add(-time.Hour)}
	current  = report.Range{From: now.Add(-time.Hour), To: now}
)

func key(template string) endpoint.Key {
	return endpoint.Key{Method: "GET", Template: template, StatusClass: 2}
}

func finding(path string, kind detect.ChangeKind, sev detect.Severity, p float64) detect.Finding {
	return detect.Finding{
		Path:         path,
		Kind:         kind,
		Severity:     sev,
		BaselineRate: 1.0,
		CurrentRate:  0.06,
		PValue:       p,
		Evidence:     "present in 2000/2000 baseline, 6/100 current",
	}
}

func TestBuildOrdering(t *testing.T) {
	results := []report.EndpointResult{
		{
			Endpoint:        key("/quiet"),
			BaselineSamples: 900, CurrentSamples: 90,
		},
		{
			Endpoint:        key("/info-only"),
			BaselineSamples: 500, CurrentSamples: 50,
			Findings: []detect.Finding{
				finding("a", detect.FieldAdded, detect.SeverityInfo, 0.01),
			},
		},
		{
			Endpoint:        key("/critical"),
			BaselineSamples: 2000, CurrentSamples: 100,
			Findings: []detect.Finding{
				// Deliberately out of display order.
				finding("z.info", detect.FieldAdded, detect.SeverityInfo, 0.001),
				finding("m.critical", detect.TypeChanged, detect.SeverityCritical, 0.02),
				finding("a.high", detect.FieldRemoved, detect.SeverityHigh, 0.03),
				finding("b.critical", detect.TypeChanged, detect.SeverityCritical, 0.01),
			},
		},
	}

	r := report.Build(now, baseline, current, results)

	// Quiet endpoints are dropped from the groups but still counted.
	if len(r.Groups) != 2 {
		t.Fatalf("%d groups, want 2 (endpoints with no findings are dropped)", len(r.Groups))
	}
	if r.Summary.EndpointsCompared != 3 {
		t.Errorf("EndpointsCompared = %d, want 3: a quiet report is not an empty one", r.Summary.EndpointsCompared)
	}
	if r.Summary.Endpoints != 2 {
		t.Errorf("Summary.Endpoints = %d, want 2", r.Summary.Endpoints)
	}
	if r.Summary.Findings != 5 {
		t.Errorf("Summary.Findings = %d, want 5", r.Summary.Findings)
	}

	// Groups: worst severity first.
	if r.Groups[0].Endpoint.Template != "/critical" {
		t.Errorf("first group = %q, want /critical", r.Groups[0].Endpoint.Template)
	}
	if r.Groups[0].MaxSeverity != detect.SeverityCritical {
		t.Errorf("MaxSeverity = %v, want critical", r.Groups[0].MaxSeverity)
	}
	if r.Groups[1].Endpoint.Template != "/info-only" {
		t.Errorf("second group = %q, want /info-only", r.Groups[1].Endpoint.Template)
	}

	// Findings: severity first, then p-value ascending.
	wantOrder := []string{"b.critical", "m.critical", "a.high", "z.info"}
	for i, want := range wantOrder {
		if got := r.Groups[0].Findings[i].Path; got != want {
			t.Errorf("finding %d = %q, want %q (order is severity, then p-value, then path)", i, got, want)
		}
	}

	if got := r.Summary.BySeverity["critical"]; got != 2 {
		t.Errorf("BySeverity[critical] = %d, want 2", got)
	}
	if got := r.Summary.ByKind["type_changed"]; got != 2 {
		t.Errorf("ByKind[type_changed] = %d, want 2", got)
	}
}

// TestBuildIsDeterministic guards that two runs over the same data produce
// byte-identical output, since reports get diffed against yesterday's.
func TestBuildIsDeterministic(t *testing.T) {
	results := []report.EndpointResult{
		{
			Endpoint:        key("/a"),
			BaselineSamples: 100, CurrentSamples: 100,
			Findings: []detect.Finding{
				// Same severity and same p-value: only the tiebreak separates
				// them, and it has to be total.
				finding("z", detect.FieldRemoved, detect.SeverityHigh, 0.01),
				finding("a", detect.FieldRemoved, detect.SeverityHigh, 0.01),
				finding("m", detect.FieldRemoved, detect.SeverityHigh, 0.01),
			},
		},
		{
			Endpoint:        key("/b"),
			BaselineSamples: 100, CurrentSamples: 100,
			Findings: []detect.Finding{
				finding("q", detect.FieldRemoved, detect.SeverityHigh, 0.01),
			},
		},
	}

	var first bytes.Buffer
	if err := report.WriteText(&first, report.Build(now, baseline, current, results)); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	for i := 0; i < 5; i++ {
		var again bytes.Buffer
		if err := report.WriteText(&again, report.Build(now, baseline, current, results)); err != nil {
			t.Fatalf("WriteText: %v", err)
		}
		if again.String() != first.String() {
			t.Fatalf("run %d differs from the first:\n%s\n---\n%s", i, first.String(), again.String())
		}
	}

	// And the paths are in the order the tiebreak dictates.
	r := report.Build(now, baseline, current, results)
	for i, want := range []string{"a", "m", "z"} {
		if got := r.Groups[0].Findings[i].Path; got != want {
			t.Errorf("finding %d = %q, want %q", i, got, want)
		}
	}
}

// TestBuildDoesNotReorderCallerSlices guards that Build treats its input as
// read-only; the caller may still be holding it.
func TestBuildDoesNotReorderCallerSlices(t *testing.T) {
	findings := []detect.Finding{
		finding("z", detect.FieldAdded, detect.SeverityInfo, 0.5),
		finding("a", detect.TypeChanged, detect.SeverityCritical, 0.001),
	}
	results := []report.EndpointResult{
		{Endpoint: key("/a"), BaselineSamples: 100, CurrentSamples: 100, Findings: findings},
	}

	report.Build(now, baseline, current, results)

	if findings[0].Path != "z" || findings[1].Path != "a" {
		t.Errorf("Build reordered the caller's slice: got %q, %q", findings[0].Path, findings[1].Path)
	}
}

func TestWriteText(t *testing.T) {
	results := []report.EndpointResult{
		{
			Endpoint:        key("/v1/charges/{id}"),
			BaselineSamples: 2000, CurrentSamples: 100,
			Findings: []detect.Finding{
				finding("data.legacy_id", detect.FieldRemoved, detect.SeverityHigh, 0.0000000034),
			},
		},
	}

	var buf bytes.Buffer
	if err := report.WriteText(&buf, report.Build(now, baseline, current, results)); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"1 finding across 1 endpoint",
		"1 high",
		"GET /v1/charges/{id} 2xx",
		"2000 samples baseline, 100 samples current",
		"HIGH",
		"field_removed",
		"data.legacy_id",
		"100.0% → 6.0%",
		"p=3.4e-09",
		"present in 2000/2000 baseline, 6/100 current",
		"2026-09-04T12:00:00Z",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
}

func TestWriteTextEmptyReport(t *testing.T) {
	var buf bytes.Buffer
	r := report.Build(now, baseline, current, []report.EndpointResult{
		{Endpoint: key("/a")}, {Endpoint: key("/b")},
	})
	if err := report.WriteText(&buf, r); err != nil {
		t.Fatalf("WriteText: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "no findings across 2 endpoints") {
		t.Errorf("output does not say what was examined:\n%s", out)
	}
}

func TestWriteTextNoEndpointsAtAll(t *testing.T) {
	var buf bytes.Buffer
	if err := report.WriteText(&buf, report.Build(now, baseline, current, nil)); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	if !strings.Contains(buf.String(), "no findings across 0 endpoints") {
		t.Errorf("a report over nothing must say so:\n%s", buf.String())
	}
}

func TestWriteJSONRoundTrips(t *testing.T) {
	results := []report.EndpointResult{
		{
			Endpoint:        key("/v1/charges/{id}"),
			BaselineSamples: 2000, CurrentSamples: 100,
			Findings: []detect.Finding{
				finding("data.amount", detect.TypeChanged, detect.SeverityCritical, 0.001),
				finding("data.legacy_id", detect.FieldRemoved, detect.SeverityHigh, 0.002),
			},
		},
	}
	want := report.Build(now, baseline, current, results)

	var buf bytes.Buffer
	if err := report.WriteJSON(&buf, want); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	var got report.Report
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("decoding: %v\n%s", err, buf.String())
	}

	if got.Summary.Findings != want.Summary.Findings {
		t.Errorf("Summary.Findings = %d, want %d", got.Summary.Findings, want.Summary.Findings)
	}
	if len(got.Groups) != 1 {
		t.Fatalf("%d groups, want 1", len(got.Groups))
	}
	if got.Groups[0].Endpoint != want.Groups[0].Endpoint {
		t.Errorf("Endpoint = %+v, want %+v", got.Groups[0].Endpoint, want.Groups[0].Endpoint)
	}
	if got.Groups[0].Findings[0].Kind != detect.TypeChanged {
		t.Errorf("Kind = %v, want type_changed", got.Groups[0].Findings[0].Kind)
	}
	if got.Groups[0].MaxSeverity != detect.SeverityCritical {
		t.Errorf("MaxSeverity = %v, want critical", got.Groups[0].MaxSeverity)
	}

	// Names, not numbers, so the JSON survives a constant being reordered.
	if !strings.Contains(buf.String(), `"kind": "type_changed"`) {
		t.Errorf("JSON does not spell kinds by name:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"severity": "critical"`) {
		t.Errorf("JSON does not spell severities by name:\n%s", buf.String())
	}
}

func TestFormatting(t *testing.T) {
	tests := []struct {
		name     string
		finding  detect.Finding
		contains []string
	}{
		{
			name:     "an ordinary p-value stays decimal",
			finding:  detect.Finding{Path: "a", PValue: 0.0321, BaselineRate: 0.5, CurrentRate: 0.25},
			contains: []string{"p=0.0321", "50.0% → 25.0%"},
		},
		{
			name:     "a tiny p-value goes scientific",
			finding:  detect.Finding{Path: "a", PValue: 1.2e-12, BaselineRate: 1, CurrentRate: 0},
			contains: []string{"p=1.2e-12", "100.0% → 0.0%"},
		},
		{
			name:     "a zero p-value is not rendered as scientific zero",
			finding:  detect.Finding{Path: "a", PValue: 0},
			contains: []string{"p=0"},
		},
		{
			name:     "a finding with no evidence line still renders",
			finding:  detect.Finding{Path: "a", PValue: 0.5, Evidence: ""},
			contains: []string{"a", "p=0.5000"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := report.Build(now, baseline, current, []report.EndpointResult{
				{Endpoint: key("/x"), Findings: []detect.Finding{tt.finding}},
			})
			var buf bytes.Buffer
			if err := report.WriteText(&buf, r); err != nil {
				t.Fatalf("WriteText: %v", err)
			}
			for _, want := range tt.contains {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("output is missing %q:\n%s", want, buf.String())
				}
			}
		})
	}
}

// TestWriteTextReportsWriteErrors guards that a failed write is surfaced rather
// than swallowed, so a broken pipe does not look like an empty report.
func TestWriteTextReportsWriteErrors(t *testing.T) {
	r := report.Build(now, baseline, current, []report.EndpointResult{
		{Endpoint: key("/x"), Findings: []detect.Finding{finding("a", detect.FieldRemoved, detect.SeverityHigh, 0.01)}},
	})
	if err := report.WriteText(failingWriter{}, r); err == nil {
		t.Error("WriteText to a failing writer = nil error, want an error")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errFailed }

var errFailed = &writeError{}

type writeError struct{}

func (*writeError) Error() string { return "write failed" }
