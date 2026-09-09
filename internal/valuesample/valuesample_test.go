package valuesample_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/schema"
	"github.com/avidnerd/apidrift/internal/valuesample"
)

var key = endpoint.Key{Method: "GET", Template: "/v1/charges/{id}", StatusClass: 2}

// always returns a config that walks every body, so tests are deterministic.
func always(maxPerPath int) valuesample.Config {
	return valuesample.Config{SampleRate: 1.0, MaxPerPath: maxPerPath, Seed: 7}
}

func vals(kind schema.Kind, raws ...string) []valuesample.Value {
	out := make([]valuesample.Value, 0, len(raws))
	for _, r := range raws {
		out = append(out, valuesample.Value{Kind: kind, Raw: r})
	}
	return out
}

func TestObserveCollectsScalarPaths(t *testing.T) {
	s := valuesample.New(always(32))

	body := `{"id":"ch_1","amount":2000,"paid":true,"refunded":null,
	          "outcome":{"risk_score":12},"items":[{"qty":3},{"qty":4}]}`
	if !s.Observe(key, []byte(body)) {
		t.Fatal("Observe returned false with a sample rate of 1.0")
	}
	s.Rotate() // move what we just saw into the baseline side

	tests := []struct {
		path string
		want []string
	}{
		{"id", []string{"ch_1"}},
		{"amount", []string{"2000"}},
		{"paid", []string{"true"}},
		{"refunded", []string{"null"}},
		{"outcome.risk_score", []string{"12"}},
		{"items[].qty", []string{"3", "4"}},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			baseline, _, ok := s.Samples(valuesample.Key{Endpoint: key, Path: tt.path})
			if !ok {
				t.Fatalf("no samples for %q", tt.path)
			}
			var got []string
			for _, v := range baseline {
				got = append(got, v.Raw)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("values = %v, want %v", got, tt.want)
			}
		})
	}

	// Containers are not values; there is nothing about units to learn from an
	// object or an array itself.
	for _, container := range []string{"outcome", "items"} {
		if _, _, ok := s.Samples(valuesample.Key{Endpoint: key, Path: container}); ok {
			t.Errorf("%q was sampled, but a container holds no scalar value", container)
		}
	}
}

func TestNumberKindFollowsWrittenForm(t *testing.T) {
	s := valuesample.New(always(32))
	s.Observe(key, []byte(`{"whole":1,"decimal":1.0,"exp":2e3}`))
	s.Rotate()

	tests := []struct {
		path string
		want schema.Kind
	}{
		{"whole", schema.KindInt},
		{"decimal", schema.KindFloat},
		{"exp", schema.KindFloat},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			baseline, _, ok := s.Samples(valuesample.Key{Endpoint: key, Path: tt.path})
			if !ok || len(baseline) != 1 {
				t.Fatalf("no sample for %q", tt.path)
			}
			if baseline[0].Kind != tt.want {
				t.Errorf("Kind = %v, want %v", baseline[0].Kind, tt.want)
			}
		})
	}
}

// TestReservoirIsBounded is the memory guarantee: however much traffic goes
// past, the sample stays the configured size.
func TestReservoirIsBounded(t *testing.T) {
	const perPath = 8
	s := valuesample.New(always(perPath))

	for i := 0; i < 5000; i++ {
		s.Observe(key, []byte(fmt.Sprintf(`{"amount":%d}`, i)))
	}
	s.Rotate()

	baseline, _, ok := s.Samples(valuesample.Key{Endpoint: key, Path: "amount"})
	if !ok {
		t.Fatal("no samples")
	}
	if len(baseline) != perPath {
		t.Errorf("kept %d values, want exactly %d", len(baseline), perPath)
	}
}

// TestReservoirSamplesTheWholeStream guards the property that makes the sample
// worth anything: it must not be only the first values seen, or it would
// describe the minutes after a deploy and nothing else.
func TestReservoirSamplesTheWholeStream(t *testing.T) {
	const perPath = 16
	s := valuesample.New(always(perPath))

	const n = 4000
	for i := 0; i < n; i++ {
		s.Observe(key, []byte(fmt.Sprintf(`{"amount":%d}`, i)))
	}
	s.Rotate()

	baseline, _, _ := s.Samples(valuesample.Key{Endpoint: key, Path: "amount"})

	late := 0
	for _, v := range baseline {
		var got int
		fmt.Sscanf(v.Raw, "%d", &got)
		if got > n/2 {
			late++
		}
	}
	if late == 0 {
		t.Errorf("every retained value came from the first half of the stream: %v", baseline)
	}
}

func TestPathCapIsEnforced(t *testing.T) {
	s := valuesample.New(valuesample.Config{SampleRate: 1.0, MaxPaths: 4, Seed: 1})

	for i := 0; i < 50; i++ {
		s.Observe(key, []byte(fmt.Sprintf(`{"field_%d":%d}`, i, i)))
	}

	st := s.Stats()
	if st.Paths > 4 {
		t.Errorf("Paths = %d, want at most 4", st.Paths)
	}
	if st.Capped == 0 {
		t.Error("Capped = 0, want the cap to have been hit and counted")
	}
}

func TestSampleRate(t *testing.T) {
	s := valuesample.New(valuesample.Config{SampleRate: 0.25, Seed: 3})

	walked := 0
	for i := 0; i < 2000; i++ {
		if s.Observe(key, []byte(`{"amount":1}`)) {
			walked++
		}
	}
	if walked == 0 || walked == 2000 {
		t.Fatalf("walked %d of 2000 bodies; the sample rate is not being applied", walked)
	}
	if rate := float64(walked) / 2000; rate < 0.20 || rate > 0.30 {
		t.Errorf("walk rate = %.3f, want about 0.25", rate)
	}
}

func TestRotate(t *testing.T) {
	s := valuesample.New(always(32))

	s.Observe(key, []byte(`{"amount":100}`))
	s.Rotate()
	s.Observe(key, []byte(`{"amount":200}`))

	baseline, current, ok := s.Samples(valuesample.Key{Endpoint: key, Path: "amount"})
	if !ok {
		t.Fatal("no samples")
	}
	if len(baseline) != 1 || baseline[0].Raw != "100" {
		t.Errorf("baseline = %v, want the pre-rotation value", baseline)
	}
	if len(current) != 1 || current[0].Raw != "200" {
		t.Errorf("current = %v, want the post-rotation value", current)
	}

	// A path that stops appearing entirely is dropped rather than kept forever
	// with an empty side nobody can compare against.
	s.Rotate()
	s.Rotate()
	if _, _, ok := s.Samples(valuesample.Key{Endpoint: key, Path: "amount"}); ok {
		t.Error("a path with no values on either side survived two rotations")
	}
}

func TestObserveRejectsUnparseableBodies(t *testing.T) {
	s := valuesample.New(always(32))
	if s.Observe(key, []byte(`{"truncated":`)) {
		t.Error("Observe returned true for a body that does not parse")
	}
}

// TestDetectShift is the deterministic gate in front of the model, and the
// reason the design is affordable. Everything it rejects costs nothing.
func TestDetectShift(t *testing.T) {
	tests := []struct {
		name              string
		baseline, current []valuesample.Value
		wantShift         bool
		wantReason        string
	}{
		{
			name:      "cents to dollars",
			baseline:  vals(schema.KindInt, "2000", "1500", "3000", "999", "4200", "1200"),
			current:   vals(schema.KindInt, "20", "15", "30", "9", "42", "12"),
			wantShift: true, wantReason: "magnitude",
		},
		{
			name:      "seconds to milliseconds",
			baseline:  vals(schema.KindInt, "1724457600", "1724457601", "1724457602", "1724457603", "1724457604"),
			current:   vals(schema.KindInt, "1724457600000", "1724457601000", "1724457602000", "1724457603000", "1724457604000"),
			wantShift: true, wantReason: "magnitude",
		},
		{
			name:      "ordinary business variation is not a shift",
			baseline:  vals(schema.KindInt, "2000", "1500", "3000", "999", "4200", "1200"),
			current:   vals(schema.KindInt, "2300", "1700", "2800", "1100", "3900", "1400"),
			wantShift: false,
		},
		{
			name:      "a 50% rise is still not a shift",
			baseline:  vals(schema.KindInt, "100", "110", "90", "105", "95", "100"),
			current:   vals(schema.KindInt, "150", "160", "140", "155", "145", "150"),
			wantShift: false,
		},
		{
			name:      "epoch integer to timestamp string",
			baseline:  vals(schema.KindString, "1724457600", "1724457601", "1724457602", "1724457603"),
			current:   vals(schema.KindString, "2026-09-01T00:00:00Z", "2026-09-01T00:00:01Z", "2026-09-01T00:00:02Z", "2026-09-01T00:00:03Z"),
			wantShift: true, wantReason: "format",
		},
		{
			name:      "int to float written form",
			baseline:  vals(schema.KindInt, "10", "11", "12", "13", "14"),
			current:   vals(schema.KindFloat, "10.0", "11.0", "12.0", "13.0", "14.0"),
			wantShift: true, wantReason: "written number form",
		},
		{
			name:      "too few baseline values to judge",
			baseline:  vals(schema.KindInt, "2000", "1500"),
			current:   vals(schema.KindInt, "20", "15", "30", "9", "42"),
			wantShift: false,
		},
		{
			name:      "too few current values to judge",
			baseline:  vals(schema.KindInt, "2000", "1500", "3000", "999", "4200"),
			current:   vals(schema.KindInt, "20", "15"),
			wantShift: false,
		},
		{
			name:      "identical samples",
			baseline:  vals(schema.KindInt, "100", "200", "300", "400", "500"),
			current:   vals(schema.KindInt, "100", "200", "300", "400", "500"),
			wantShift: false,
		},
		{
			name:      "stable text values",
			baseline:  vals(schema.KindString, "open", "paid", "open", "void", "paid"),
			current:   vals(schema.KindString, "paid", "open", "void", "open", "paid"),
			wantShift: false,
		},
		{
			name:      "zeros do not produce a divide-by-zero shift",
			baseline:  vals(schema.KindInt, "0", "0", "0", "0", "0"),
			current:   vals(schema.KindInt, "0", "0", "0", "0", "0"),
			wantShift: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := valuesample.DetectShift(tt.baseline, tt.current)

			if (got != nil) != tt.wantShift {
				t.Fatalf("DetectShift() = %+v, want shift=%v", got, tt.wantShift)
			}
			if got == nil {
				return
			}
			if tt.wantReason != "" && !strings.Contains(got.Reason, tt.wantReason) {
				t.Errorf("Reason = %q, want it to mention %q", got.Reason, tt.wantReason)
			}
		})
	}
}

// TestDetectShiftRatioIsReported checks the number handed to the model, since a
// ratio near a round figure is the strongest evidence it gets.
func TestDetectShiftRatioIsReported(t *testing.T) {
	baseline := vals(schema.KindInt, "1000", "1000", "1000", "1000", "1000")
	current := vals(schema.KindInt, "10", "10", "10", "10", "10")

	got := valuesample.DetectShift(baseline, current)
	if got == nil {
		t.Fatal("no shift detected for a 100x drop")
	}
	if got.MagnitudeRatio < 0.009 || got.MagnitudeRatio > 0.011 {
		t.Errorf("MagnitudeRatio = %v, want about 0.01", got.MagnitudeRatio)
	}
}

func TestConcurrentUse(t *testing.T) {
	s := valuesample.New(always(16))

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				s.Observe(key, []byte(fmt.Sprintf(`{"amount":%d}`, i)))
				s.Samples(valuesample.Key{Endpoint: key, Path: "amount"})
				s.Stats()
			}
		}(w)
	}
	wg.Wait()

	if st := s.Stats(); st.Walked != 1600 {
		t.Errorf("Walked = %d, want 1600", st.Walked)
	}
}
