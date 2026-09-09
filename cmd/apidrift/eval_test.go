package main

import (
	"bytes"
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	specV1 = "../../internal/eval/testdata/billing-v1.yaml"
	specV2 = "../../internal/eval/testdata/billing-v2.yaml"
)

func TestParseEvalFlags(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"both specs given", []string{"-v1", "a.yaml", "-v2", "b.yaml"}, ""},
		{"v1 missing", []string{"-v2", "b.yaml"}, "requires -v1 and -v2"},
		{"v2 missing", []string{"-v1", "a.yaml"}, "requires -v1 and -v2"},
		{"neither given", nil, "requires -v1 and -v2"},
		{"zero responses", []string{"-v1", "a", "-v2", "b", "-responses", "0"}, "at least 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseEvalFlags(tt.args, &bytes.Buffer{})
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

func TestEvalCmdMissingSpec(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"eval", "-v1", "does-not-exist.yaml", "-v2", specV2}, &out, &errOut)

	if code != exitError {
		t.Errorf("run() = %d, want %d", code, exitError)
	}
	if !strings.Contains(errOut.String(), "does-not-exist.yaml") {
		t.Errorf("stderr does not name the missing file:\n%s", errOut.String())
	}
}

// TestEvalCmdReportsGroundTruth: whatever the stubs do, the spec diff is
// computed from the specs alone and must always be printed.
func TestEvalCmdReportsGroundTruth(t *testing.T) {
	var out, errOut bytes.Buffer
	run([]string{"eval", "-v1", specV1, "-v2", specV2, "-responses", "5", "-no-latency"}, &out, &errOut)

	got := out.String()
	for _, want := range []string{
		"apidrift evaluation",
		"Billing API 1.0",
		"Billing API 2.0",
		"ground truth (11 changes)",
		"legacy_id field_removed",
		"balance type_changed",
		"email nullability_introduced",
		"tax_id field_added",
		"data[].status enum_value_added",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output is missing %q:\n%s", want, got)
		}
	}
}

func TestEvalCmdJSON(t *testing.T) {
	var out, errOut bytes.Buffer
	run([]string{"eval", "-v1", specV1, "-v2", specV2, "-responses", "5", "-no-latency", "-json"}, &out, &errOut)

	got := out.String()
	if !strings.HasPrefix(strings.TrimSpace(got), "{") {
		t.Fatalf("-json did not produce JSON:\n%s", got)
	}
	if !strings.Contains(got, `"truth"`) {
		t.Errorf("JSON output has no truth section:\n%s", got)
	}
}

func TestEvalCmdWritesLatencyCSV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "latency.csv")

	var out, errOut bytes.Buffer
	run([]string{
		"eval", "-v1", specV1, "-v2", specV2,
		"-responses", "5", "-latency-trials", "1", "-latency-csv", path,
	}, &out, &errOut)

	f, err := os.Open(path)
	if err != nil {
		// The sweep is blocked while detect.Detect is a stub, and then there
		// is nothing to write. Either outcome is correct; a malformed file is
		// not.
		if os.IsNotExist(err) && strings.Contains(errOut.String(), "detect.Detect") {
			t.Skip("blocked on detect.Detect, which is a stub: the sweep produced no grid to write")
		}
		t.Fatalf("opening the CSV: %v (stderr: %s)", err, errOut.String())
	}
	defer f.Close()

	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("the latency CSV is malformed: %v", err)
	}
	if len(rows) < 2 {
		t.Errorf("the CSV has %d rows, want a header and at least one point", len(rows))
	}
	if rows[0][0] != "presence_rate" {
		t.Errorf("first column = %q, want presence_rate", rows[0][0])
	}
}

func TestEvalCmdExitsNonZeroWhenBlocked(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"eval", "-v1", specV1, "-v2", specV2, "-responses", "5", "-no-latency"}, &out, &errOut)

	if code == exitOK && strings.Contains(out.String(), "NOT MEASURED") {
		t.Error("the evaluation did not run but the command reported success")
	}
	if code != exitOK && !strings.Contains(errOut.String(), "TODO") {
		t.Errorf("a non-zero exit does not say what blocked it:\n%s", errOut.String())
	}
}
