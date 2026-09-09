package main

import (
	"bytes"
	"strings"
	"testing"
)

// TestDemoDetectsThePlantedChanges is a smoke test for the whole pipeline. The
// demo drives real traffic through the real proxy, store and detector, so if
// any of them regresses this fails, and it fails with output a person can read.
func TestDemoDetectsThePlantedChanges(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"demo", "-requests", "300"}, &out, &errOut); code != exitOK {
		t.Fatalf("demo exited %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	got := out.String()

	// Every change planted in the fake upstream, with the kind and severity it
	// should be reported as.
	for _, want := range []string{
		"CRITICAL type_changed           amount",
		"HIGH     field_removed          legacy_id",
		"HIGH     nullability_introduced email",
		"INFO     enum_value_added       status",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("demo did not report %q\n%s", want, got)
		}
	}

	// Exactly the planted changes, nothing invented.
	if !strings.Contains(got, "4 findings across 1 endpoint") {
		t.Errorf("expected 4 findings on 1 endpoint\n%s", got)
	}
	// The templater has to collapse the ids, or this would be one endpoint per
	// request rather than one endpoint.
	if !strings.Contains(got, "GET /v1/charges/{id} 2xx") {
		t.Errorf("ids were not collapsed into a template\n%s", got)
	}
	// And the semantic layer should notice the int-to-string switch, which is
	// the one change that is also a change of meaning.
	if !strings.Contains(got, "flagged for semantic review") {
		t.Errorf("no semantic candidate was flagged\n%s", got)
	}
}

func TestDemoRejectsTooFewRequests(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"demo", "-requests", "10"}, &out, &errOut); code != exitUsage {
		t.Errorf("run() = %d, want %d for a request count below the sample floor", code, exitUsage)
	}
}
