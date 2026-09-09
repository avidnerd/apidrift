package schema_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/avidnerd/apidrift/internal/schema"
)

// want describes the expected stats for one path, in the compact form the
// tables below need. Zero fields mean zero.
type want struct {
	seen         uint64
	present      uint64
	explicitNull uint64
	kinds        map[schema.Kind]uint64
	strings      map[string]uint64
	overflow     bool
}

func checkField(t *testing.T, s *schema.Schema, path string, w want) {
	t.Helper()

	f, ok := s.Fields[path]
	if !ok {
		t.Errorf("no stats for path %q (have %v)", path, s.Paths())
		return
	}
	if f.Seen != w.seen {
		t.Errorf("%s: Seen = %d, want %d", path, f.Seen, w.seen)
	}
	if f.Present != w.present {
		t.Errorf("%s: Present = %d, want %d", path, f.Present, w.present)
	}
	if f.ExplicitNull != w.explicitNull {
		t.Errorf("%s: ExplicitNull = %d, want %d", path, f.ExplicitNull, w.explicitNull)
	}
	if f.StringOverflow != w.overflow {
		t.Errorf("%s: StringOverflow = %v, want %v", path, f.StringOverflow, w.overflow)
	}
	for k, n := range w.kinds {
		if f.TypeCounts[k] != n {
			t.Errorf("%s: TypeCounts[%v] = %d, want %d (have %v)", path, k, f.TypeCounts[k], n, f.TypeCounts)
		}
	}
	if w.kinds != nil && len(f.TypeCounts) != len(w.kinds) {
		t.Errorf("%s: TypeCounts has %d kinds, want %d (have %v)", path, len(f.TypeCounts), len(w.kinds), f.TypeCounts)
	}
	for v, n := range w.strings {
		if f.StringValues[v] != n {
			t.Errorf("%s: StringValues[%q] = %d, want %d", path, v, f.StringValues[v], n)
		}
	}
	if w.strings != nil && len(f.StringValues) != len(w.strings) {
		t.Errorf("%s: StringValues has %d values, want %d (have %v)", path, len(f.StringValues), len(w.strings), f.StringValues)
	}
}

func mustExtract(t *testing.T, body string) *schema.Schema {
	t.Helper()

	s, err := schema.Extract([]byte(body))
	if err != nil {
		t.Fatalf("Extract(%s): %v", body, err)
	}
	if s.Samples != 1 {
		t.Fatalf("Samples = %d, want 1: Extract summarises exactly one response", s.Samples)
	}
	return s
}

// TestExtractAbsentVersusNull is the distinction the whole design turns on: a
// key that is gone and a key holding null are different upstream changes.
func TestExtractAbsentVersusNull(t *testing.T) {
	present := mustExtract(t, `{"id":"cus_1","email":"a@example.com"}`)
	null := mustExtract(t, `{"id":"cus_1","email":null}`)
	absent := mustExtract(t, `{"id":"cus_1"}`)

	checkField(t, present, "email", want{
		seen: 1, present: 1,
		kinds:   map[schema.Kind]uint64{schema.KindString: 1},
		strings: map[string]uint64{"a@example.com": 1},
	})
	checkField(t, null, "email", want{
		seen: 1, present: 1, explicitNull: 1,
		kinds:   map[schema.Kind]uint64{schema.KindNull: 1},
		strings: map[string]uint64{},
	})
	if _, ok := absent.Fields["email"]; ok {
		t.Error("an absent key produced an entry; a single response cannot tell optional from nonexistent")
	}

	// And the three are genuinely distinguishable, not merely differently
	// spelled: null is Present, absent is not, and only null is ExplicitNull.
	if null.Fields["email"].ExplicitNull == present.Fields["email"].ExplicitNull {
		t.Error("null and non-null values produced the same ExplicitNull count")
	}
}

func TestExtractPaths(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantPaths []string
	}{
		{
			name:      "flat object",
			body:      `{"id":"cus_1","livemode":false,"amount":2000}`,
			wantPaths: []string{"amount", "id", "livemode"},
		},
		{
			name:      "nested objects are dotted",
			body:      `{"data":{"customer":{"id":"cus_1"}}}`,
			wantPaths: []string{"data", "data.customer", "data.customer.id"},
		},
		{
			name:      "array elements take the [] step",
			body:      `{"data":{"items":[{"status":"open"},{"status":"paid"}]}}`,
			wantPaths: []string{"data", "data.items", "data.items[]", "data.items[].status"},
		},
		{
			name:      "arrays of scalars",
			body:      `{"tags":["a","b"]}`,
			wantPaths: []string{"tags", "tags[]"},
		},
		{
			name:      "nested arrays",
			body:      `{"matrix":[[1,2],[3,4]]}`,
			wantPaths: []string{"matrix", "matrix[]", "matrix[][]"},
		},
		{
			name:      "an empty array has no element path",
			body:      `{"data":{"items":[]}}`,
			wantPaths: []string{"data", "data.items"},
		},
		{
			name:      "a root array",
			body:      `[{"id":"a"},{"id":"b"}]`,
			wantPaths: []string{"[]", "[].id"},
		},
		{
			name:      "a root scalar has no paths",
			body:      `"just a string"`,
			wantPaths: []string{},
		},
		{
			name:      "an empty object has no paths",
			body:      `{}`,
			wantPaths: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustExtract(t, tt.body)
			got := s.Paths()
			if strings.Join(got, ",") != strings.Join(tt.wantPaths, ",") {
				t.Errorf("Paths() = %v, want %v", got, tt.wantPaths)
			}
		})
	}
}

func TestExtractKinds(t *testing.T) {
	tests := []struct {
		name string
		body string
		path string
		want schema.Kind
	}{
		{"string", `{"a":"x"}`, "a", schema.KindString},
		{"bool", `{"a":true}`, "a", schema.KindBool},
		{"null", `{"a":null}`, "a", schema.KindNull},
		{"object", `{"a":{"b":1}}`, "a", schema.KindObject},
		{"array", `{"a":[1]}`, "a", schema.KindArray},
		{"integer", `{"a":2000}`, "a", schema.KindInt},
		{"negative integer", `{"a":-3}`, "a", schema.KindInt},
		{"large integer", `{"a":9007199254740993}`, "a", schema.KindInt},
		{"decimal is a float", `{"a":20.00}`, "a", schema.KindFloat},
		{"a whole number written with a point is a float", `{"a":1.0}`, "a", schema.KindFloat},
		{"exponent form is a float", `{"a":2e3}`, "a", schema.KindFloat},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustExtract(t, tt.body)
			f := s.Fields[tt.path]
			if f == nil {
				t.Fatalf("no stats for %q", tt.path)
			}
			if n := f.TypeCounts[tt.want]; n != 1 {
				t.Errorf("TypeCounts[%v] = %d, want 1 (have %v)", tt.want, n, f.TypeCounts)
			}
		})
	}
}

// TestExtractCountsResponsesNotOccurrences pins the unit down: repetition
// inside one response is one observation, not many.
func TestExtractCountsResponsesNotOccurrences(t *testing.T) {
	var items []string
	for i := 0; i < 50; i++ {
		items = append(items, `{"status":"open"}`)
	}
	body := `{"items":[` + strings.Join(items, ",") + `]}`

	s := mustExtract(t, body)
	checkField(t, s, "items[].status", want{
		seen: 1, present: 1,
		kinds:   map[schema.Kind]uint64{schema.KindString: 1},
		strings: map[string]uint64{"open": 1},
	})
}

// TestExtractHeterogeneousArray covers a response that disagrees with itself:
// both kinds are recorded, and neither is collapsed into "mixed".
func TestExtractHeterogeneousArray(t *testing.T) {
	s := mustExtract(t, `{"items":[{"id":"a"},{"id":7},{"id":null}]}`)

	checkField(t, s, "items[].id", want{
		seen: 1, present: 1, explicitNull: 1,
		kinds: map[schema.Kind]uint64{
			schema.KindString: 1,
			schema.KindInt:    1,
			schema.KindNull:   1,
		},
		strings: map[string]uint64{"a": 1},
	})
}

func TestExtractStringCardinality(t *testing.T) {
	tests := []struct {
		name         string
		values       int
		wantOverflow bool
		wantStored   int
	}{
		{"well inside the cap", 5, false, 5},
		{"one below the cap", schema.MaxEnumCardinality - 1, false, schema.MaxEnumCardinality - 1},
		{"exactly at the cap", schema.MaxEnumCardinality, false, schema.MaxEnumCardinality},
		{"one past the cap", schema.MaxEnumCardinality + 1, true, schema.MaxEnumCardinality},
		{"far past the cap", schema.MaxEnumCardinality * 4, true, schema.MaxEnumCardinality},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			elems := make([]string, 0, tt.values)
			for i := 0; i < tt.values; i++ {
				elems = append(elems, fmt.Sprintf(`{"status":"value-%d"}`, i))
			}
			body := `{"items":[` + strings.Join(elems, ",") + `]}`

			s := mustExtract(t, body)
			f := s.Fields["items[].status"]
			if f == nil {
				t.Fatal("no stats for items[].status")
			}
			if f.StringOverflow != tt.wantOverflow {
				t.Errorf("StringOverflow = %v, want %v", f.StringOverflow, tt.wantOverflow)
			}
			if len(f.StringValues) != tt.wantStored {
				t.Errorf("stored %d distinct values, want %d", len(f.StringValues), tt.wantStored)
			}
		})
	}
}

// TestExtractMapShapedObjects covers the collapse that keeps an id-keyed object
// from producing one path per key.
func TestExtractMapShapedObjects(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantPaths []string
	}{
		{
			name:      "identifier keys collapse from two keys up",
			body:      `{"user_8123":{"status":"active"},"user_9944":{"status":"idle"}}`,
			wantPaths: []string{"{*}", "{*}.status"},
		},
		{
			name:      "collapse under a nested path",
			body:      `{"data":{"cus_1a2b3c4d":{"balance":0},"cus_9z8y7x6w":{"balance":5}}}`,
			wantPaths: []string{"data", "data.{*}", "data.{*}.balance"},
		},
		{
			name:      "numeric keys collapse",
			body:      `{"1":{"n":1},"2":{"n":2},"3":{"n":3}}`,
			wantPaths: []string{"{*}", "{*}.n"},
		},
		{
			name:      "uuid keys collapse",
			body:      `{"3f2504e0-4f89-11d3-9a0c-0305e82c3301":{"n":1},"886313e1-3b8a-5372-9b90-0c9aee199e5d":{"n":2}}`,
			wantPaths: []string{"{*}", "{*}.n"},
		},
		{
			name:      "scalar-valued maps collapse too",
			body:      `{"user_1":10,"user_2":20,"user_3":30}`,
			wantPaths: []string{"{*}"},
		},
		{
			name:      "a record keeps its field names",
			body:      `{"id":"cus_1","email":"a@example.com","balance":0}`,
			wantPaths: []string{"balance", "email", "id"},
		},
		{
			name:      "one identifier key among field names is not a map",
			body:      `{"id":"cus_1","user_8123":{"a":1}}`,
			wantPaths: []string{"id", "user_8123", "user_8123.a"},
		},
		{
			name:      "identifier keys with disagreeing value kinds are not a map",
			body:      `{"user_8123":{"status":"active"},"user_9944":"idle"}`,
			wantPaths: []string{"user_8123", "user_8123.status", "user_9944"},
		},
		{
			name:      "a single identifier key is a record with one field",
			body:      `{"user_8123":{"status":"active"}}`,
			wantPaths: []string{"user_8123", "user_8123.status"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := mustExtract(t, tt.body)
			got := s.Paths()
			if strings.Join(got, ",") != strings.Join(tt.wantPaths, ",") {
				t.Errorf("Paths() = %v, want %v", got, tt.wantPaths)
			}
		})
	}
}

// TestExtractMapShapedByStructure covers the second, weaker ground for
// collapsing: word-shaped keys, but far too many of them, all holding the same
// structure.
func TestExtractMapShapedByStructure(t *testing.T) {
	build := func(n int, vary bool) string {
		parts := make([]string, 0, n)
		for i := 0; i < n; i++ {
			if vary && i%2 == 1 {
				parts = append(parts, fmt.Sprintf(`"currency-%d":{"rate":1,"extra":true}`, i))
				continue
			}
			parts = append(parts, fmt.Sprintf(`"currency-%d":{"rate":1}`, i))
		}
		return "{" + strings.Join(parts, ",") + "}"
	}

	t.Run("many uniform values collapse", func(t *testing.T) {
		s := mustExtract(t, build(schema.MapKeyHardLimit, false))
		if got := s.Paths(); strings.Join(got, ",") != "{*},{*}.rate" {
			t.Errorf("Paths() = %v, want [{*} {*}.rate]", got)
		}
	})

	t.Run("just under the limit stays per key", func(t *testing.T) {
		s := mustExtract(t, build(schema.MapKeyHardLimit-1, false))
		if _, ok := s.Fields["{*}"]; ok {
			t.Errorf("collapsed below MapKeyHardLimit; Paths() = %v", s.Paths())
		}
	})

	t.Run("many values that disagree structurally stay per key", func(t *testing.T) {
		s := mustExtract(t, build(schema.MapKeyHardLimit, true))
		if _, ok := s.Fields["{*}"]; ok {
			t.Errorf("collapsed a heterogeneous record; Paths() = %v", s.Paths())
		}
	})
}

func TestExtractErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"empty", ``},
		{"whitespace only", "  \n\t "},
		{"truncated object", `{"a":1`},
		{"not json at all", `<html></html>`},
		{"trailing content after the document", `{"a":1} {"b":2}`},
		{"trailing garbage", `{"a":1}garbage`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := schema.Extract([]byte(tt.body)); err == nil {
				t.Errorf("Extract(%q) = nil error, want an error", tt.body)
			}
		})
	}
}

// TestExtractDepthLimit covers the nesting bound: a deeply nested document
// costs bounded paths and bounded stack.
func TestExtractDepthLimit(t *testing.T) {
	depth := schema.MaxDepth + 20
	body := strings.Repeat(`{"a":`, depth) + `1` + strings.Repeat(`}`, depth)

	s := mustExtract(t, body)
	if len(s.Fields) > schema.MaxDepth+1 {
		t.Errorf("%d paths from a %d-deep document, want at most %d", len(s.Fields), depth, schema.MaxDepth+1)
	}
	// The outermost levels are still recorded.
	if _, ok := s.Fields["a"]; !ok {
		t.Error("no stats for the outermost path")
	}
}

// TestExtractFieldLimit covers the path bound, and that exhausting it is
// recorded rather than silent.
func TestExtractFieldLimit(t *testing.T) {
	// Word-shaped keys whose values disagree structurally, so the object stays
	// a record and every key costs its own path. Each entry contributes two.
	const keys = schema.MaxFields
	parts := make([]string, 0, keys)
	for i := 0; i < keys; i++ {
		parts = append(parts, fmt.Sprintf(`"field-name-%d":{"inner-%d":%d}`, i, i, i))
	}
	body := "{" + strings.Join(parts, ",") + "}"

	s := mustExtract(t, body)
	if len(s.Fields) > schema.MaxFields+1 { // +1 for the overflow marker
		t.Errorf("%d paths retained, want at most %d", len(s.Fields), schema.MaxFields+1)
	}
	f, ok := s.Fields[schema.OverflowPath]
	if !ok {
		t.Fatalf("no entry at %s; the loss must be visible", schema.OverflowPath)
	}
	if f.Present != 1 {
		t.Errorf("%s Present = %d, want 1", schema.OverflowPath, f.Present)
	}
}

func TestParentPath(t *testing.T) {
	tests := []struct {
		path   string
		want   string
		wantOK bool
	}{
		{"id", "", false},
		{"data.id", "data", true},
		{"data.customer.id", "data.customer", true},
		{"data.items[]", "data.items", true},
		{"data.items[].status", "data.items[]", true},
		{"[]", "", true},
		{"[].id", "[]", true},
		{"{*}.status", "{*}", true},
		{"", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got, ok := schema.ParentPath(tt.path)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("ParentPath(%q) = (%q, %v), want (%q, %v)", tt.path, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
