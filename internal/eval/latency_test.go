package eval_test

import (
	"bytes"
	"encoding/csv"
	"errors"
	"strings"
	"testing"

	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/eval"
)

func TestSummarizeLatency(t *testing.T) {
	points := []eval.LatencyPoint{
		// A rate that never crosses the threshold.
		{PresenceRate: 0.95, Responses: 100, Trials: 10, Detections: 0},
		{PresenceRate: 0.95, Responses: 200, Trials: 10, Detections: 2},
		// A rate that crosses at 200, given out of order to prove the
		// summariser sorts rather than assuming.
		{PresenceRate: 0.50, Responses: 400, Trials: 10, Detections: 10},
		{PresenceRate: 0.50, Responses: 100, Trials: 10, Detections: 1},
		{PresenceRate: 0.50, Responses: 200, Trials: 10, Detections: 8},
		// A rate detected immediately.
		{PresenceRate: 0.0, Responses: 100, Trials: 10, Detections: 10},
		{PresenceRate: 0.0, Responses: 200, Trials: 10, Detections: 10},
	}

	got := eval.SummarizeLatency(points, 0.5)

	if len(got) != 3 {
		t.Fatalf("%d summary rows, want 3", len(got))
	}
	// Ordered by presence rate ascending.
	if got[0].PresenceRate != 0.0 || got[1].PresenceRate != 0.50 || got[2].PresenceRate != 0.95 {
		t.Errorf("rates = %v %v %v, want ascending", got[0].PresenceRate, got[1].PresenceRate, got[2].PresenceRate)
	}

	tests := []struct {
		rate    float64
		wantN   uint64
		wantHit bool
	}{
		{0.0, 100, true},
		{0.50, 200, true},
		{0.95, 0, false},
	}
	for i, tt := range tests {
		row := got[i]
		if row.Reached != tt.wantHit {
			t.Errorf("rate %.2f: Reached = %v, want %v", tt.rate, row.Reached, tt.wantHit)
		}
		if row.Reached && row.ResponsesRequired != tt.wantN {
			t.Errorf("rate %.2f: ResponsesRequired = %d, want %d", tt.rate, row.ResponsesRequired, tt.wantN)
		}
	}
}

func TestLatencyPointProbability(t *testing.T) {
	tests := []struct {
		name  string
		point eval.LatencyPoint
		want  float64
	}{
		{"none", eval.LatencyPoint{Trials: 10}, 0},
		{"half", eval.LatencyPoint{Trials: 10, Detections: 5}, 0.5},
		{"all", eval.LatencyPoint{Trials: 10, Detections: 10}, 1},
		{"no trials", eval.LatencyPoint{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.point.DetectionProbability(); got != tt.want {
				t.Errorf("DetectionProbability() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWriteLatencyCSV(t *testing.T) {
	points := []eval.LatencyPoint{
		{PresenceRate: 0.9, Responses: 100, Trials: 40, Detections: 4},
		{PresenceRate: 0.9, Responses: 200, Trials: 40, Detections: 26},
	}

	var buf bytes.Buffer
	if err := eval.WriteLatencyCSV(&buf, points); err != nil {
		t.Fatalf("WriteLatencyCSV: %v", err)
	}

	rows, err := csv.NewReader(&buf).ReadAll()
	if err != nil {
		t.Fatalf("output is not valid CSV: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("%d rows, want a header and two data rows", len(rows))
	}

	wantHeader := []string{"presence_rate", "responses", "trials", "detections", "detection_probability"}
	for i, want := range wantHeader {
		if rows[0][i] != want {
			t.Errorf("header column %d = %q, want %q", i, rows[0][i], want)
		}
	}
	if rows[1][0] != "0.9" || rows[1][1] != "100" || rows[1][4] != "0.1000" {
		t.Errorf("first data row = %v, want the point rendered plottably", rows[1])
	}
	if rows[2][4] != "0.6500" {
		t.Errorf("second row probability = %q, want 0.6500", rows[2][4])
	}
}

func TestWriteLatencySummary(t *testing.T) {
	rows := []eval.LatencySummary{
		{PresenceRate: 0.0, ResponsesRequired: 25, Reached: true},
		{PresenceRate: 0.95, Reached: false},
	}

	var buf bytes.Buffer
	if err := eval.WriteLatencySummary(&buf, rows, 0.5); err != nil {
		t.Fatalf("WriteLatencySummary: %v", err)
	}
	out := buf.String()

	if !strings.Contains(out, "detection latency") {
		t.Errorf("summary has no heading:\n%s", out)
	}
	if !strings.Contains(out, "not detected within the sweep") {
		t.Errorf("a rate that never fired must say so, not report a misleading number:\n%s", out)
	}
}

// TestLatencySweepReachesTheStubCleanly: like the harness, the sweep must
// surface a typed error rather than a panic while Detect is unimplemented, and
// must simply work once it is.
func TestLatencySweepReachesTheStubCleanly(t *testing.T) {
	cfg := eval.LatencyConfig{
		Path:            "status",
		BaselineSamples: 200,
		BaselineRate:    1.0,
		Rates:           []float64{0.0, 0.5},
		Steps:           []uint64{50, 100},
		Trials:          2,
	}

	points, err := eval.LatencySweep(cfg)
	if err != nil {
		var stub *eval.StubError
		if !errors.As(err, &stub) {
			t.Fatalf("LatencySweep failed for a reason other than a stub: %v", err)
		}
		return
	}
	if len(points) != 4 {
		t.Errorf("%d points, want one per (rate, step) cell", len(points))
	}
}

// -- formerly blocked on detect.Detect ----------------------------------------

// TestLatencySweepShape is the curve the evaluation reports: the smaller the
// drop, the more responses it takes to see it.
func TestLatencySweepShape(t *testing.T) {
	points, err := eval.LatencySweep(eval.DefaultLatencyConfig())
	if err != nil {
		t.Fatalf("LatencySweep: %v", err)
	}
	rows := eval.SummarizeLatency(points, 0.5)

	// A field that disappears entirely is found at the first step that clears
	// the detector's minimum sample size -- below that the detector declines to
	// test at all, however obvious the difference. For large effects the floor
	// on detection latency is MinSamples, not statistical power, which is worth
	// knowing when tuning either.
	minSamples := detect.DefaultConfig().MinSamples
	if !rows[0].Reached {
		t.Fatalf("a total disappearance was never detected")
	}
	if rows[0].ResponsesRequired < minSamples {
		t.Errorf("a total disappearance fired at %d responses, below the %d sample floor",
			rows[0].ResponsesRequired, minSamples)
	}
	if rows[0].ResponsesRequired > 2*minSamples {
		t.Errorf("a total disappearance took %d responses, want it found as soon as the sample floor allows (%d)",
			rows[0].ResponsesRequired, minSamples)
	}

	// And latency rises monotonically as the drop gets subtler.
	prev := uint64(0)
	for _, row := range rows {
		if !row.Reached {
			continue
		}
		if row.ResponsesRequired < prev {
			t.Errorf("rate %.2f needed %d responses, fewer than a larger drop's %d", row.PresenceRate, row.ResponsesRequired, prev)
		}
		prev = row.ResponsesRequired
	}
}
