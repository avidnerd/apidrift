package eval_test

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/avidnerd/apidrift/internal/eval"
	"github.com/avidnerd/apidrift/internal/schema"
)

var customerV1 = eval.EndpointID{Method: "GET", Path: "/v1/customers/{customer}", Status: 200}

// bodies generates n responses and returns them decoded.
func bodies(t *testing.T, spec *eval.Spec, cfg eval.GenConfig, id eval.EndpointID, n int) []map[string]any {
	t.Helper()

	g := eval.NewGenerator(spec, cfg)
	out := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		raw, err := g.Body(id)
		if err != nil {
			t.Fatalf("Body: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("generated body is not a JSON object: %v\n%s", err, raw)
		}
		out = append(out, m)
	}
	return out
}

// rateOf returns the share of bodies in which key is present.
func rateOf(bodies []map[string]any, key string) float64 {
	n := 0
	for _, b := range bodies {
		if _, ok := b[key]; ok {
			n++
		}
	}
	return float64(n) / float64(len(bodies))
}

// TestGeneratorIsDeterministic: the whole evaluation rests on being able to
// re-run a measurement and get the same number.
func TestGeneratorIsDeterministic(t *testing.T) {
	spec := loadV1(t)
	cfg := eval.DefaultGenConfig()

	first := eval.NewGenerator(spec, cfg)
	second := eval.NewGenerator(spec, cfg)
	for i := 0; i < 50; i++ {
		a, err := first.Body(customerV1)
		if err != nil {
			t.Fatalf("Body: %v", err)
		}
		b, err := second.Body(customerV1)
		if err != nil {
			t.Fatalf("Body: %v", err)
		}
		if string(a) != string(b) {
			t.Fatalf("response %d differs between two generators with the same seed:\n%s\n%s", i, a, b)
		}
	}

	// And a different seed gives different traffic, or the two windows of the
	// null run would be identical and its false positive rate meaningless.
	cfg.Seed = 99
	other := eval.NewGenerator(spec, cfg)
	same := 0
	for i := 0; i < 50; i++ {
		a, _ := eval.NewGenerator(spec, eval.DefaultGenConfig()).Body(customerV1)
		b, _ := other.Body(customerV1)
		if string(a) == string(b) {
			same++
		}
	}
	if same == 50 {
		t.Error("a different seed produced identical traffic")
	}
}

func TestGeneratorPresenceRates(t *testing.T) {
	const n = 4000
	const tolerance = 0.03

	tests := []struct {
		name string
		cfg  eval.GenConfig
		key  string
		want float64
	}{
		{
			name: "a required field is always present",
			cfg:  eval.GenConfig{Seed: 1, RequiredPresence: 1.0, OptionalPresence: 0.5},
			key:  "id", want: 1.0,
		},
		{
			name: "an optional field follows OptionalPresence",
			cfg:  eval.GenConfig{Seed: 1, RequiredPresence: 1.0, OptionalPresence: 0.5},
			key:  "address", want: 0.5,
		},
		{
			name: "a per-path override beats the default",
			cfg: eval.GenConfig{
				Seed: 1, RequiredPresence: 1.0, OptionalPresence: 0.5,
				PresenceByPath: map[string]float64{"address": 0.2},
			},
			key: "address", want: 0.2,
		},
		{
			name: "a per-path override applies to required fields too",
			cfg: eval.GenConfig{
				Seed: 1, RequiredPresence: 1.0, OptionalPresence: 0.5,
				PresenceByPath: map[string]float64{"legacy_id": 0.35},
			},
			key: "legacy_id", want: 0.35,
		},
		{
			name: "an upstream that does not honour its own spec",
			cfg:  eval.GenConfig{Seed: 1, RequiredPresence: 0.8, OptionalPresence: 0.5},
			key:  "id", want: 0.8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rateOf(bodies(t, loadV1(t), tt.cfg, customerV1, n), tt.key)
			if math.Abs(got-tt.want) > tolerance {
				t.Errorf("presence rate for %q = %.3f, want %.3f ± %.2f", tt.key, got, tt.want, tolerance)
			}
		})
	}
}

func TestGeneratorRespectsEnums(t *testing.T) {
	allowed := map[string]bool{"failed": true, "pending": true, "succeeded": true}

	charges := eval.EndpointID{Method: "GET", Path: "/v1/charges", Status: 200}
	g := eval.NewGenerator(loadV1(t), eval.GenConfig{Seed: 1, RequiredPresence: 1.0})

	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		raw, err := g.Body(charges)
		if err != nil {
			t.Fatalf("Body: %v", err)
		}
		var doc struct {
			Data []struct {
				Status string `json:"status"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("decoding: %v\n%s", err, raw)
		}
		for _, item := range doc.Data {
			if !allowed[item.Status] {
				t.Fatalf("generated status %q, which the enum does not permit", item.Status)
			}
			seen[item.Status] = true
		}
	}
	if len(seen) != len(allowed) {
		t.Errorf("generated only %v; the whole enum should be exercised", seen)
	}
}

func TestGeneratorNullRate(t *testing.T) {
	const n = 3000
	const tolerance = 0.04

	// v2's email is nullable; v1's is not.
	spec := loadV2(t)
	cfg := eval.GenConfig{Seed: 1, RequiredPresence: 1.0, NullRate: 0.3}

	nulls := 0
	for _, b := range bodies(t, spec, cfg, customerV1, n) {
		if v, ok := b["email"]; ok && v == nil {
			nulls++
		}
	}
	if got := float64(nulls) / n; math.Abs(got-0.3) > tolerance {
		t.Errorf("null rate = %.3f, want 0.30 ± %.2f", got, tolerance)
	}

	// A non-nullable field is never null, whatever the rate says.
	for _, b := range bodies(t, loadV1(t), cfg, customerV1, 500) {
		if v, ok := b["email"]; ok && v == nil {
			t.Fatal("a non-nullable field was generated as null")
		}
	}
}

// TestGeneratedBodiesExtractCleanly closes the loop: what the generator emits
// has to be something the real extractor understands, or the harness would be
// measuring the detector against paths that never occur in production.
func TestGeneratedBodiesExtractCleanly(t *testing.T) {
	g := eval.NewGenerator(loadV1(t), eval.GenConfig{Seed: 1, RequiredPresence: 1.0, OptionalPresence: 1.0})

	raw, err := g.Body(customerV1)
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	got, err := schema.Extract(raw)
	if err != nil {
		t.Fatalf("Extract on a generated body: %v\n%s", err, raw)
	}

	for _, want := range []string{"id", "object", "email", "legacy_id", "balance", "address", "address.city", "address.line1"} {
		if _, ok := got.Fields[want]; !ok {
			t.Errorf("extracted schema has no %q; have %v", want, got.Paths())
		}
	}

	// Array paths line up with what the differ emits.
	charges := eval.EndpointID{Method: "GET", Path: "/v1/charges", Status: 200}
	raw, err = g.Body(charges)
	if err != nil {
		t.Fatalf("Body: %v", err)
	}
	got, err = schema.Extract(raw)
	if err != nil {
		t.Fatalf("Extract: %v\n%s", err, raw)
	}
	for _, want := range []string{"data", "data[]", "data[].status", "data[].amount"} {
		if _, ok := got.Fields[want]; !ok {
			t.Errorf("extracted schema has no %q; have %v", want, got.Paths())
		}
	}
}

// TestGeneratorTypesMatchTheSpec guards that an integer field is not generated
// as something schema.Extract would call a float -- a spurious type change in
// every run would swamp the results.
func TestGeneratorTypesMatchTheSpec(t *testing.T) {
	g := eval.NewGenerator(loadV1(t), eval.GenConfig{Seed: 1, RequiredPresence: 1.0, OptionalPresence: 1.0})

	for i := 0; i < 100; i++ {
		raw, err := g.Body(customerV1)
		if err != nil {
			t.Fatalf("Body: %v", err)
		}
		got, err := schema.Extract(raw)
		if err != nil {
			t.Fatalf("Extract: %v", err)
		}
		if n := got.Fields["balance"].TypeCounts[schema.KindInt]; n != 1 {
			t.Fatalf("balance was not extracted as an int on response %d: %v\n%s", i, got.Fields["balance"].TypeCounts, raw)
		}
	}
}

func TestGeneratorUnknownEndpoint(t *testing.T) {
	g := eval.NewGenerator(loadV1(t), eval.DefaultGenConfig())
	_, err := g.Body(eval.EndpointID{Method: "GET", Path: "/nope", Status: 200})
	if err == nil {
		t.Fatal("Body for an unknown endpoint = nil error, want an error")
	}
	if !strings.Contains(err.Error(), "no response") {
		t.Errorf("error = %v, want it to say the endpoint is unknown", err)
	}
}
