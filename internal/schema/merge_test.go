package schema_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/avidnerd/apidrift/internal/schema"
)

// -- specification for a stub -------------------------------------------------
//
// schema.Merge is not implemented. Every test in this file fails today, on
// purpose: together they are the specification, and they turn green when the
// implementation is correct. See README.md in this package for the prose
// version.

// mergeSafe calls Merge and turns its TODO panic into an ordinary test failure.
//
// Without this, the first unimplemented call aborts the whole test binary and
// the remaining specifications never run -- so the one thing this file exists
// to communicate, the full shape of the contract, would be invisible. It stays
// useful afterwards: a panic from a buggy implementation reports which case
// provoked it instead of taking the suite down.
func mergeSafe(t *testing.T, a, b *schema.Schema) (out *schema.Schema) {
	t.Helper()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Merge panicked: %v", r)
		}
	}()
	return schema.Merge(a, b)
}

// field builds a FieldStats for the tables below.
func field(seen, present, null uint64, kinds map[schema.Kind]uint64, strs map[string]uint64) *schema.FieldStats {
	f := &schema.FieldStats{
		Seen:         seen,
		Present:      present,
		ExplicitNull: null,
		TypeCounts:   map[schema.Kind]uint64{},
		StringValues: map[string]uint64{},
	}
	for k, v := range kinds {
		f.TypeCounts[k] = v
	}
	for k, v := range strs {
		f.StringValues[k] = v
	}
	return f
}

// sch builds a Schema for the tables below.
func sch(samples uint64, fields map[string]*schema.FieldStats) *schema.Schema {
	s := &schema.Schema{Samples: samples, Fields: map[string]*schema.FieldStats{}}
	for p, f := range fields {
		s.Fields[p] = f
	}
	return s
}

// describe renders a schema deterministically, so a mismatch reports what
// actually differed rather than just that something did.
func describe(s *schema.Schema) string {
	if s == nil {
		return "<nil>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "samples=%d\n", s.Samples)
	for _, p := range s.Paths() {
		f := s.Fields[p]
		kinds := make([]string, 0, len(f.TypeCounts))
		for k, n := range f.TypeCounts {
			if n > 0 {
				kinds = append(kinds, fmt.Sprintf("%v=%d", k, n))
			}
		}
		sort.Strings(kinds)
		vals := make([]string, 0, len(f.StringValues))
		for v, n := range f.StringValues {
			if n > 0 {
				vals = append(vals, fmt.Sprintf("%q=%d", v, n))
			}
		}
		sort.Strings(vals)
		fmt.Fprintf(&b, "  %s seen=%d present=%d null=%d overflow=%v kinds{%s} strings{%s}\n",
			p, f.Seen, f.Present, f.ExplicitNull, f.StringOverflow,
			strings.Join(kinds, " "), strings.Join(vals, " "))
	}
	return b.String()
}

func assertSame(t *testing.T, what string, got, want *schema.Schema) {
	t.Helper()

	if g, w := describe(got), describe(want); g != w {
		t.Errorf("%s\n got:\n%s\nwant:\n%s", what, g, w)
	}
}

// -- the invariants -----------------------------------------------------------

// TestMergeCommutative: the order of two schemas may not change the result.
func TestMergeCommutative(t *testing.T) {
	tests := []struct {
		name string
		a, b *schema.Schema
	}{
		{
			name: "same fields",
			a: sch(10, map[string]*schema.FieldStats{
				"id": field(10, 10, 0, map[schema.Kind]uint64{schema.KindString: 10}, map[string]uint64{"a": 10}),
			}),
			b: sch(5, map[string]*schema.FieldStats{
				"id": field(5, 5, 0, map[schema.Kind]uint64{schema.KindString: 5}, map[string]uint64{"b": 5}),
			}),
		},
		{
			name: "disjoint root fields",
			a: sch(10, map[string]*schema.FieldStats{
				"id": field(10, 10, 0, map[schema.Kind]uint64{schema.KindString: 10}, nil),
			}),
			b: sch(7, map[string]*schema.FieldStats{
				"email": field(7, 4, 1, map[schema.Kind]uint64{schema.KindString: 3, schema.KindNull: 1}, nil),
			}),
		},
		{
			name: "nested field missing from one side",
			a: sch(10, map[string]*schema.FieldStats{
				"data":        field(10, 10, 0, map[schema.Kind]uint64{schema.KindObject: 10}, nil),
				"data.status": field(10, 10, 0, map[schema.Kind]uint64{schema.KindString: 10}, map[string]uint64{"open": 10}),
			}),
			b: sch(6, map[string]*schema.FieldStats{
				"data": field(6, 6, 0, map[schema.Kind]uint64{schema.KindObject: 6}, nil),
			}),
		},
		{
			name: "one side empty",
			a: sch(10, map[string]*schema.FieldStats{
				"id": field(10, 9, 0, map[schema.Kind]uint64{schema.KindString: 9}, nil),
			}),
			b: sch(0, nil),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ab := mergeSafe(t, tt.a, tt.b)
			ba := mergeSafe(t, tt.b, tt.a)
			assertSame(t, "Merge(a,b) != Merge(b,a)", ab, ba)
		})
	}
}

// TestMergeAssociative: buckets come out of the ring buffer in whatever order
// the store walks them, and the window must not depend on that order.
//
// This is where an implementation is most likely to break. The absent-path rule
// reads the parent's Present to decide how much Seen to contribute; an
// implementation that reads it from a partially merged intermediate rather than
// from the original operand gets a different answer depending on grouping.
func TestMergeAssociative(t *testing.T) {
	tests := []struct {
		name    string
		a, b, c *schema.Schema
	}{
		{
			name: "all three share a field",
			a: sch(10, map[string]*schema.FieldStats{
				"id": field(10, 10, 0, map[schema.Kind]uint64{schema.KindString: 10}, map[string]uint64{"x": 10}),
			}),
			b: sch(20, map[string]*schema.FieldStats{
				"id": field(20, 18, 2, map[schema.Kind]uint64{schema.KindString: 16, schema.KindNull: 2}, map[string]uint64{"y": 16}),
			}),
			c: sch(30, map[string]*schema.FieldStats{
				"id": field(30, 30, 0, map[schema.Kind]uint64{schema.KindInt: 30}, nil),
			}),
		},
		{
			name: "a nested field present in only the first",
			a: sch(10, map[string]*schema.FieldStats{
				"data":        field(10, 10, 0, map[schema.Kind]uint64{schema.KindObject: 10}, nil),
				"data.legacy": field(10, 10, 0, map[schema.Kind]uint64{schema.KindString: 10}, nil),
			}),
			b: sch(20, map[string]*schema.FieldStats{
				"data": field(20, 20, 0, map[schema.Kind]uint64{schema.KindObject: 20}, nil),
			}),
			c: sch(30, map[string]*schema.FieldStats{
				"data": field(30, 30, 0, map[schema.Kind]uint64{schema.KindObject: 30}, nil),
			}),
		},
		{
			name: "array element paths, absent in the middle operand",
			a: sch(10, map[string]*schema.FieldStats{
				"items":          field(10, 10, 0, map[schema.Kind]uint64{schema.KindArray: 10}, nil),
				"items[]":        field(10, 10, 0, map[schema.Kind]uint64{schema.KindObject: 10}, nil),
				"items[].status": field(10, 10, 0, map[schema.Kind]uint64{schema.KindString: 10}, nil),
			}),
			b: sch(20, map[string]*schema.FieldStats{
				"items":   field(20, 20, 0, map[schema.Kind]uint64{schema.KindArray: 20}, nil),
				"items[]": field(20, 15, 0, map[schema.Kind]uint64{schema.KindObject: 15}, nil),
			}),
			c: sch(30, map[string]*schema.FieldStats{
				"items":          field(30, 30, 0, map[schema.Kind]uint64{schema.KindArray: 30}, nil),
				"items[]":        field(30, 30, 0, map[schema.Kind]uint64{schema.KindObject: 30}, nil),
				"items[].status": field(30, 28, 0, map[schema.Kind]uint64{schema.KindString: 28}, nil),
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			left := mergeSafe(t, mergeSafe(t, tt.a, tt.b), tt.c)
			right := mergeSafe(t, tt.a, mergeSafe(t, tt.b, tt.c))
			assertSame(t, "Merge(Merge(a,b),c) != Merge(a,Merge(b,c))", left, right)
		})
	}
}

// TestMergeAbsentFieldIncrementsSeenNotPresent is the rule that makes presence
// a rate rather than a fact. A field in one schema and not the other did not
// stop existing -- the other schema's responses were chances for it to appear
// that it did not take, and Seen has to record them.
func TestMergeAbsentFieldIncrementsSeenNotPresent(t *testing.T) {
	tests := []struct {
		name        string
		a, b        *schema.Schema
		path        string
		wantSeen    uint64
		wantPresent uint64
	}{
		{
			name: "root-level field absent from b uses b's Samples",
			a: sch(100, map[string]*schema.FieldStats{
				"legacy_id": field(100, 100, 0, map[schema.Kind]uint64{schema.KindString: 100}, nil),
			}),
			b:           sch(50, map[string]*schema.FieldStats{}),
			path:        "legacy_id",
			wantSeen:    150, // 100 chances in a, 50 more in b
			wantPresent: 100, // taken in a only
		},
		{
			name: "nested field absent from b uses b's parent Present",
			a: sch(100, map[string]*schema.FieldStats{
				"data":        field(100, 100, 0, map[schema.Kind]uint64{schema.KindObject: 100}, nil),
				"data.legacy": field(100, 100, 0, map[schema.Kind]uint64{schema.KindString: 100}, nil),
			}),
			b: sch(50, map[string]*schema.FieldStats{
				// data itself was present in only 40 of b's 50 responses, so
				// only 40 were chances for data.legacy to appear.
				"data": field(50, 40, 0, map[schema.Kind]uint64{schema.KindObject: 40}, nil),
			}),
			path:        "data.legacy",
			wantSeen:    140,
			wantPresent: 100,
		},
		{
			name: "the parent being absent too contributes nothing",
			a: sch(100, map[string]*schema.FieldStats{
				"data":        field(100, 100, 0, map[schema.Kind]uint64{schema.KindObject: 100}, nil),
				"data.legacy": field(100, 100, 0, map[schema.Kind]uint64{schema.KindString: 100}, nil),
			}),
			b:           sch(50, map[string]*schema.FieldStats{}),
			path:        "data.legacy",
			wantSeen:    100, // b never had data, so it never had a chance at data.legacy
			wantPresent: 100,
		},
		{
			name: "array element path uses the element path's Present",
			a: sch(100, map[string]*schema.FieldStats{
				"items":          field(100, 100, 0, map[schema.Kind]uint64{schema.KindArray: 100}, nil),
				"items[]":        field(100, 90, 0, map[schema.Kind]uint64{schema.KindObject: 90}, nil),
				"items[].status": field(90, 90, 0, map[schema.Kind]uint64{schema.KindString: 90}, nil),
			}),
			b: sch(50, map[string]*schema.FieldStats{
				"items":   field(50, 50, 0, map[schema.Kind]uint64{schema.KindArray: 50}, nil),
				"items[]": field(50, 30, 0, map[schema.Kind]uint64{schema.KindObject: 30}, nil),
			}),
			path:        "items[].status",
			wantSeen:    120, // 90 from a, plus b's 30 non-empty arrays
			wantPresent: 90,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeSafe(t, tt.a, tt.b)
			f, ok := got.Fields[tt.path]
			if !ok {
				t.Fatalf("merged schema has no %q (has %v)", tt.path, got.Paths())
			}
			if f.Seen != tt.wantSeen {
				t.Errorf("%s: Seen = %d, want %d", tt.path, f.Seen, tt.wantSeen)
			}
			if f.Present != tt.wantPresent {
				t.Errorf("%s: Present = %d, want %d", tt.path, f.Present, tt.wantPresent)
			}
		})
	}
}

// TestMergeKeepsCountsNotBooleans guards against the tempting simplification of
// reducing presence to an "optional" flag. Detect needs the rate, and a field
// present in 999 of 1000 responses is not the same observation as one present
// in 500 of 1000.
func TestMergeKeepsCountsNotBooleans(t *testing.T) {
	tests := []struct {
		name                      string
		a, b                      *schema.Schema
		wantSeen, wantPresent     uint64
		wantSamples               uint64
		wantPresenceRateNumerator string
	}{
		{
			name: "almost always present",
			a: sch(1000, map[string]*schema.FieldStats{
				"status": field(1000, 999, 0, map[schema.Kind]uint64{schema.KindString: 999}, nil),
			}),
			b: sch(1000, map[string]*schema.FieldStats{
				"status": field(1000, 1000, 0, map[schema.Kind]uint64{schema.KindString: 1000}, nil),
			}),
			wantSeen: 2000, wantPresent: 1999, wantSamples: 2000,
		},
		{
			name: "half present",
			a: sch(1000, map[string]*schema.FieldStats{
				"status": field(1000, 500, 0, map[schema.Kind]uint64{schema.KindString: 500}, nil),
			}),
			b: sch(1000, map[string]*schema.FieldStats{
				"status": field(1000, 500, 0, map[schema.Kind]uint64{schema.KindString: 500}, nil),
			}),
			wantSeen: 2000, wantPresent: 1000, wantSamples: 2000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeSafe(t, tt.a, tt.b)
			if got.Samples != tt.wantSamples {
				t.Errorf("Samples = %d, want %d", got.Samples, tt.wantSamples)
			}
			f := got.Fields["status"]
			if f == nil {
				t.Fatal("merged schema has no status")
			}
			if f.Seen != tt.wantSeen || f.Present != tt.wantPresent {
				t.Errorf("Seen/Present = %d/%d, want %d/%d", f.Seen, f.Present, tt.wantSeen, tt.wantPresent)
			}
		})
	}

	// And the two cases above must remain distinguishable after merging.
	almost := mergeSafe(t, tests[0].a, tests[0].b).Fields["status"].PresenceRate()
	half := mergeSafe(t, tests[1].a, tests[1].b).Fields["status"].PresenceRate()
	if almost == half {
		t.Errorf("presence rates collapsed to the same value (%v); the counts must survive merging", almost)
	}
}

// TestMergeKeepsAllKinds guards against collapsing type observations into a
// single "mixed" kind. The rare kind is the finding.
func TestMergeKeepsAllKinds(t *testing.T) {
	a := sch(1000, map[string]*schema.FieldStats{
		"amount": field(1000, 1000, 0, map[schema.Kind]uint64{schema.KindString: 1000}, nil),
	})
	b := sch(3, map[string]*schema.FieldStats{
		"amount": field(3, 3, 0, map[schema.Kind]uint64{schema.KindInt: 3}, nil),
	})

	got := mergeSafe(t, a, b)
	f := got.Fields["amount"]
	if f == nil {
		t.Fatal("merged schema has no amount")
	}
	if f.TypeCounts[schema.KindString] != 1000 {
		t.Errorf("TypeCounts[string] = %d, want 1000", f.TypeCounts[schema.KindString])
	}
	if f.TypeCounts[schema.KindInt] != 3 {
		t.Errorf("TypeCounts[int] = %d, want 3", f.TypeCounts[schema.KindInt])
	}
	if n := len(f.TypeCounts); n != 2 {
		t.Errorf("TypeCounts has %d kinds (%v), want exactly 2 -- kinds must not be collapsed", n, f.TypeCounts)
	}
}

func TestMergeStringValues(t *testing.T) {
	t.Run("counts add per value", func(t *testing.T) {
		a := sch(10, map[string]*schema.FieldStats{
			"status": field(10, 10, 0, nil, map[string]uint64{"open": 7, "paid": 3}),
		})
		b := sch(10, map[string]*schema.FieldStats{
			"status": field(10, 10, 0, nil, map[string]uint64{"paid": 4, "void": 6}),
		})

		f := mergeSafe(t, a, b).Fields["status"]
		if f == nil {
			t.Fatal("merged schema has no status")
		}
		for v, want := range map[string]uint64{"open": 7, "paid": 7, "void": 6} {
			if f.StringValues[v] != want {
				t.Errorf("StringValues[%q] = %d, want %d", v, f.StringValues[v], want)
			}
		}
	})

	t.Run("overflow propagates", func(t *testing.T) {
		a := sch(10, map[string]*schema.FieldStats{
			"status": field(10, 10, 0, nil, map[string]uint64{"open": 10}),
		})
		a.Fields["status"].StringOverflow = true
		b := sch(10, map[string]*schema.FieldStats{
			"status": field(10, 10, 0, nil, map[string]uint64{"paid": 10}),
		})

		if f := mergeSafe(t, a, b).Fields["status"]; f == nil || !f.StringOverflow {
			t.Error("StringOverflow did not propagate; a partial value set stays partial after merging")
		}
	})

	t.Run("merging past the cap overflows rather than growing", func(t *testing.T) {
		half := schema.MaxEnumCardinality
		mk := func(offset int) *schema.Schema {
			vals := map[string]uint64{}
			for i := 0; i < half; i++ {
				vals[fmt.Sprintf("value-%d", offset+i)] = 1
			}
			return sch(uint64(half), map[string]*schema.FieldStats{
				"status": field(uint64(half), uint64(half), 0, nil, vals),
			})
		}

		f := mergeSafe(t, mk(0), mk(half)).Fields["status"]
		if f == nil {
			t.Fatal("merged schema has no status")
		}
		if len(f.StringValues) > schema.MaxEnumCardinality {
			t.Errorf("StringValues holds %d values, want at most %d", len(f.StringValues), schema.MaxEnumCardinality)
		}
		if !f.StringOverflow {
			t.Error("StringOverflow = false after merging past the cap, want true")
		}
	})
}

func TestMergeNilOperands(t *testing.T) {
	s := sch(10, map[string]*schema.FieldStats{
		"id": field(10, 10, 0, map[schema.Kind]uint64{schema.KindString: 10}, nil),
	})

	tests := []struct {
		name string
		a, b *schema.Schema
		want *schema.Schema
	}{
		{"both nil", nil, nil, sch(0, nil)},
		{"nil left", nil, s, s},
		{"nil right", s, nil, s},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mergeSafe(t, tt.a, tt.b)
			if got == nil {
				t.Fatal("Merge returned nil; it must always return a usable schema")
			}
			assertSame(t, "merged with nil", got, tt.want)
		})
	}
}

// TestMergeDoesNotModifyInputs guards the store: window queries merge buckets
// that other windows still reference.
func TestMergeDoesNotModifyInputs(t *testing.T) {
	a := sch(10, map[string]*schema.FieldStats{
		"id":     field(10, 10, 0, map[schema.Kind]uint64{schema.KindString: 10}, map[string]uint64{"x": 10}),
		"legacy": field(10, 5, 0, map[schema.Kind]uint64{schema.KindString: 5}, nil),
	})
	b := sch(20, map[string]*schema.FieldStats{
		"id": field(20, 20, 0, map[schema.Kind]uint64{schema.KindInt: 20}, nil),
	})
	beforeA, beforeB := describe(a), describe(b)

	got := mergeSafe(t, a, b)
	if got == nil {
		t.Fatal("Merge returned nil")
	}
	if describe(a) != beforeA {
		t.Errorf("Merge modified its first argument\nbefore:\n%s\nafter:\n%s", beforeA, describe(a))
	}
	if describe(b) != beforeB {
		t.Errorf("Merge modified its second argument\nbefore:\n%s\nafter:\n%s", beforeB, describe(b))
	}

	// The result must not alias the inputs' maps either.
	if got.Fields["id"] == a.Fields["id"] || got.Fields["id"] == b.Fields["id"] {
		t.Error("merged schema shares a *FieldStats with an input; a later merge would corrupt the bucket")
	}
}

// TestMergeCountInvariants: whatever else it does, the merged counts must stay
// coherent. Detect divides by Seen and reads ExplicitNull as a subset of
// Present, and neither survives these being violated.
func TestMergeCountInvariants(t *testing.T) {
	a := sch(100, map[string]*schema.FieldStats{
		"data":        field(100, 80, 0, map[schema.Kind]uint64{schema.KindObject: 80}, nil),
		"data.status": field(80, 70, 20, map[schema.Kind]uint64{schema.KindString: 50, schema.KindNull: 20}, map[string]uint64{"open": 50}),
	})
	b := sch(50, map[string]*schema.FieldStats{
		"data": field(50, 50, 0, map[schema.Kind]uint64{schema.KindObject: 50}, nil),
	})

	got := mergeSafe(t, a, b)
	for _, p := range got.Paths() {
		f := got.Fields[p]
		if f.Present > f.Seen {
			t.Errorf("%s: Present(%d) > Seen(%d)", p, f.Present, f.Seen)
		}
		if f.ExplicitNull > f.Present {
			t.Errorf("%s: ExplicitNull(%d) > Present(%d)", p, f.ExplicitNull, f.Present)
		}
	}
	if got.Samples != 150 {
		t.Errorf("Samples = %d, want 150", got.Samples)
	}
}

// TestMergeExtractedSchemas is the end-to-end shape of real use: a window is
// built by merging one single-response schema after another.
func TestMergeExtractedSchemas(t *testing.T) {
	bodies := []string{
		`{"id":"cus_1","status":"open"}`,
		`{"id":"cus_2","status":"paid"}`,
		`{"id":"cus_3"}`, // status absent
		`{"id":"cus_4","status":null}`,
	}

	var acc *schema.Schema
	for _, body := range bodies {
		s, err := schema.Extract([]byte(body))
		if err != nil {
			t.Fatalf("Extract(%s): %v", body, err)
		}
		acc = mergeSafe(t, acc, s)
	}

	if acc.Samples != 4 {
		t.Errorf("Samples = %d, want 4", acc.Samples)
	}
	f := acc.Fields["status"]
	if f == nil {
		t.Fatal("merged schema has no status")
	}
	if f.Seen != 4 {
		t.Errorf("status Seen = %d, want 4 -- every response was a chance for it to appear", f.Seen)
	}
	if f.Present != 3 {
		t.Errorf("status Present = %d, want 3 -- two strings and one explicit null", f.Present)
	}
	if f.ExplicitNull != 1 {
		t.Errorf("status ExplicitNull = %d, want 1", f.ExplicitNull)
	}
	if got := f.PresenceRate(); got != 0.75 {
		t.Errorf("status PresenceRate = %v, want 0.75", got)
	}
}
