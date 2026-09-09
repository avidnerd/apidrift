package eval_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/avidnerd/apidrift/internal/eval"
)

// runConfig returns a small, fast evaluation over the test specs.
func runConfig(t *testing.T, responses int) eval.RunConfig {
	t.Helper()

	return eval.RunConfig{
		V1:        loadV1(t),
		V2:        loadV2(t),
		Gen:       eval.DefaultGenConfig(),
		Responses: responses,
	}
}

// TestRunReachesTheStubsCleanly is the strongest claim the harness can make
// today: everything up to the unimplemented functions works, and reaching one
// produces a typed error naming it rather than a panic.
//
// It stays meaningful afterwards -- once the stubs land, err is nil and the
// same assertions about ground truth still hold.
func TestRunReachesTheStubsCleanly(t *testing.T) {
	for _, responses := range []int{1, 5} {
		res, err := eval.Run(runConfig(t, responses))

		if res == nil {
			t.Fatalf("responses=%d: Run returned no result at all", responses)
		}
		if err != nil {
			var stub *eval.StubError
			if !errors.As(err, &stub) {
				t.Fatalf("responses=%d: Run failed for a reason other than a stub: %v", responses, err)
			}
			if stub.Stage == "" {
				t.Errorf("responses=%d: StubError does not name the stage that failed: %v", responses, stub)
			}
		}

		// Ground truth is computed before anything statistical happens, so it
		// is available whatever the stubs do.
		want := eval.Diff(loadV1(t), loadV2(t))
		if len(res.Truth) != len(want) {
			t.Errorf("responses=%d: Truth has %d changes, want %d", responses, len(res.Truth), len(want))
		}
		if res.Endpoints != 3 {
			t.Errorf("responses=%d: Endpoints = %d, want 3", responses, res.Endpoints)
		}
		if res.ResponsesPerWindow != responses {
			t.Errorf("ResponsesPerWindow = %d, want %d", res.ResponsesPerWindow, responses)
		}
	}
}

func TestRunDefaults(t *testing.T) {
	cfg := runConfig(t, 0)
	res, _ := eval.Run(cfg)

	if res.ResponsesPerWindow == 0 {
		t.Error("ResponsesPerWindow = 0; a zero should take a default rather than generating nothing")
	}
	if res.SpecV1 != "Billing API 1.0" || res.SpecV2 != "Billing API 2.0" {
		t.Errorf("specs = %q / %q, want the titles and versions", res.SpecV1, res.SpecV2)
	}
}

func TestStubErrorMessage(t *testing.T) {
	err := &eval.StubError{Stage: "scoring (eval.Score)", Cause: "boom"}
	got := err.Error()
	for _, want := range []string{"scoring", "eval.Score", "boom"} {
		if !strings.Contains(got, want) {
			t.Errorf("StubError message %q is missing %q", got, want)
		}
	}
}

// TestWriteSummaryComplete covers the full report, which is what will be
// printed once the stubs land.
func TestWriteSummaryComplete(t *testing.T) {
	res, _ := eval.Run(runConfig(t, 1))
	res.Blocked = "" // pretend the measurement completed

	var buf bytes.Buffer
	if err := eval.WriteSummary(&buf, res); err != nil {
		t.Fatalf("WriteSummary: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"apidrift evaluation",
		"false positives under the null",
		"recall by change kind",
		"field_removed",
		"type_changed",
		"ground truth",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary is missing %q:\n%s", want, out)
		}
	}
}

// TestWriteSummaryBlocked is the presentation that matters most today, and the
// one most easily got wrong: a result that was never measured must not print a
// table of zeros, because a wall of zeros reads as "the detector found
// nothing" when what happened is that nothing ran.
func TestWriteSummaryBlocked(t *testing.T) {
	res, err := eval.Run(runConfig(t, 5))
	if err == nil {
		t.Skip("nothing is blocking the harness any more; this case cannot arise")
	}
	if res.Blocked == "" {
		t.Fatal("Run returned an error but left Blocked empty; readers cannot tell measured zeros from unmeasured ones")
	}

	var buf bytes.Buffer
	if err := eval.WriteSummary(&buf, res); err != nil {
		t.Fatalf("WriteSummary: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "NOT MEASURED") {
		t.Errorf("a blocked result does not say so:\n%s", out)
	}
	for _, unwanted := range []string{"recall by change kind", "false positives under the null"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("a blocked result printed the %q table, which would be all zeros and misleading:\n%s", unwanted, out)
		}
	}
	if !strings.Contains(out, "ground truth") {
		t.Errorf("the ground truth is still valid and should still be printed:\n%s", out)
	}
}

func TestWriteJSON(t *testing.T) {
	res, _ := eval.Run(runConfig(t, 1))

	var buf bytes.Buffer
	if err := eval.WriteJSON(&buf, res); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	if !strings.Contains(buf.String(), `"truth"`) {
		t.Errorf("JSON has no truth section:\n%s", buf.String())
	}
}

// -- formerly blocked on the stubs --------------------------------------------

// TestRunEndToEnd is the measurement the whole project exists to produce. It
// needs all three stubs: schema.Merge to build windows, detect.Detect to find
// anything, and eval.Score to grade it.
func TestRunEndToEnd(t *testing.T) {
	res, err := eval.Run(runConfig(t, 3000))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Every change kind in the test specs should be found.
	for kind, recall := range res.RecallByKind() {
		if recall < 0.9 {
			t.Errorf("recall for %s = %.3f, want at least 0.9", kind, recall)
		}
	}

	// And the headline: nothing changed, so nothing should be reported.
	if res.Null.PerPath > 0.01 {
		t.Errorf("false positive rate under the null = %.4f per path, want under 0.01", res.Null.PerPath)
	}
}
