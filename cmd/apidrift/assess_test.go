package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avidnerd/apidrift/internal/assess"
	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/report"
	"github.com/avidnerd/apidrift/internal/schema"
	"github.com/avidnerd/apidrift/internal/valuesample"
)

var assessKey = endpoint.Key{Method: "GET", Template: "/v1/charges/{id}", StatusClass: 2}

func TestParseAssessFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"defaults", nil, ""},
		{"zero max is rejected", []string{"-max", "0"}, "at least 1"},
		{"both judgements off is rejected", []string{"-semantic=false", "-impact=false"}, "nothing to do"},
		{"semantic only is fine", []string{"-impact=false"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseAssessFlags(tt.args, &bytes.Buffer{})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

// adminStub serves the two endpoints the assess command reads.
func adminStub(t *testing.T, candidates []assess.SemanticRequest, rep report.Report) *httptest.Server {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/candidates", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(candidates)
	})
	mux.HandleFunc("/findings", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(rep)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestAssessDryRunSendsNothing is the guard on the flag that exists so a
// reviewer can see exactly what a run would cost before paying for it.
func TestAssessDryRunSendsNothing(t *testing.T) {
	candidates := []assess.SemanticRequest{{
		Endpoint: assessKey,
		Path:     "amount",
		Shift:    valuesample.Shift{Reason: "the median magnitude moved 1/100x", MagnitudeRatio: 0.01},
		Baseline: []valuesample.Value{{Kind: schema.KindInt, Raw: "2000"}},
		Current:  []valuesample.Value{{Kind: schema.KindInt, Raw: "20"}},
	}}
	rep := report.Build(origin, report.Range{}, report.Range{}, []report.EndpointResult{{
		Endpoint: assessKey,
		Findings: []detect.Finding{{
			Endpoint: assessKey, Path: "legacy_id",
			Kind: detect.FieldRemoved, Severity: detect.SeverityHigh,
		}},
	}})
	srv := adminStub(t, candidates, rep)

	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\nvar x = c.LegacyID\n"), 0o644); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}

	var out, errOut bytes.Buffer
	code := run([]string{"assess", "-admin", srv.URL, "-repo", repo, "-dry-run"}, &out, &errOut)
	if code != exitOK {
		t.Fatalf("run() = %d, want %d (stderr: %s)", code, exitOK, errOut.String())
	}

	got := out.String()
	for _, want := range []string{
		"DRY RUN, nothing was sent to a model",
		"semantic candidates (1)",
		"amount",
		"the median magnitude moved 1/100x",
		"would ask the model",
		"impact of confirmed findings (1)",
		"legacy_id",
		"candidate call sites: 1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output is missing %q:\n%s", want, got)
		}
	}
}

// TestAssessJSONShape covers the machine-readable form, which is what a CI job
// or a PR-opening script would consume.
func TestAssessJSONShape(t *testing.T) {
	srv := adminStub(t, []assess.SemanticRequest{{Endpoint: assessKey, Path: "amount"}}, report.Report{})

	var out, errOut bytes.Buffer
	if code := run([]string{"assess", "-admin", srv.URL, "-repo", t.TempDir(), "-dry-run", "-json"}, &out, &errOut); code != exitOK {
		t.Fatalf("run() = %d (stderr: %s)", code, errOut.String())
	}

	var got Assessment
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if !got.DryRun {
		t.Error("DryRun = false in a dry run")
	}
	if len(got.Semantic) != 1 || got.Semantic[0].Request.Path != "amount" {
		t.Errorf("Semantic = %+v, want the one candidate", got.Semantic)
	}
}

// TestAssessRespectsMax is the cost guard: one pathological window must not be
// able to run up an unbounded bill.
func TestAssessRespectsMax(t *testing.T) {
	var candidates []assess.SemanticRequest
	for i := 0; i < 50; i++ {
		candidates = append(candidates, assess.SemanticRequest{Endpoint: assessKey, Path: "field" + string(rune('a'+i%26))})
	}
	srv := adminStub(t, candidates, report.Report{})

	var out, errOut bytes.Buffer
	if code := run([]string{"assess", "-admin", srv.URL, "-repo", t.TempDir(), "-dry-run", "-json", "-max", "3"}, &out, &errOut); code != exitOK {
		t.Fatalf("run() = %d (stderr: %s)", code, errOut.String())
	}

	var got Assessment
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if len(got.Semantic) != 3 {
		t.Errorf("assessed %d candidates, want the -max of 3", len(got.Semantic))
	}
	if !strings.Contains(errOut.String(), "assessing the first 3") {
		t.Errorf("the cap was applied silently; stderr should say so:\n%s", errOut.String())
	}
}

func TestAssessUnreachableProxy(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	addr := srv.URL
	srv.Close()

	var out, errOut bytes.Buffer
	if code := run([]string{"assess", "-admin", addr, "-dry-run"}, &out, &errOut); code == exitOK {
		t.Error("assess against a dead proxy exited 0")
	}
	// The message has to say what to start, not just that something is wrong:
	// assess being a client of a running proxy is not guessable from its name.
	for _, want := range []string{"Nothing is listening", "apidrift demo -serve", "apidrift serve -upstream"} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("error is missing %q:\n%s", want, errOut.String())
		}
	}
}

// TestAssessNoCandidatesIsNotAnError: the common, healthy case is that nothing
// shifted, and it must read as reassurance rather than a failure.
func TestAssessNoCandidatesIsNotAnError(t *testing.T) {
	srv := adminStub(t, nil, report.Report{})

	var out, errOut bytes.Buffer
	if code := run([]string{"assess", "-admin", srv.URL, "-repo", t.TempDir(), "-dry-run"}, &out, &errOut); code != exitOK {
		t.Fatalf("run() = %d, want 0 when there is nothing to assess", code)
	}
	if !strings.Contains(out.String(), "no field's values shifted enough") {
		t.Errorf("an empty run must say what it means:\n%s", out.String())
	}
}

func TestDescribeFinding(t *testing.T) {
	got := describeFinding(detect.Finding{
		Path: "data.amount", Kind: detect.TypeChanged,
		BaselineRate: 1.0, CurrentRate: 0.94,
		Evidence: "present in 2000/2000 baseline, 94/100 current",
	})
	for _, want := range []string{"type_changed", "data.amount", "100.0%", "94.0%", "2000/2000"} {
		if !strings.Contains(got, want) {
			t.Errorf("description is missing %q: %q", want, got)
		}
	}
}
