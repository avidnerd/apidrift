package schema

import "sort"

// Kind is the JSON type of an observed value.
//
// Kinds are never collapsed into a single "mixed" value. A field that is a
// string in 1000 responses and an int in 3 is a different and far more
// interesting observation than a field that is simply "mixed", and the 3 is the
// part worth alerting on.
type Kind uint8

const (
	// KindNull is an explicit JSON null.
	KindNull Kind = iota
	// KindBool is true or false.
	KindBool
	// KindInt is a JSON number written without a fraction or exponent.
	KindInt
	// KindFloat is a JSON number written with a fraction or exponent.
	KindFloat
	// KindString is a JSON string.
	KindString
	// KindArray is a JSON array.
	KindArray
	// KindObject is a JSON object.
	KindObject
)

// String renders a Kind for reports and test failures.
func (k Kind) String() string {
	switch k {
	case KindNull:
		return "null"
	case KindBool:
		return "bool"
	case KindInt:
		return "int"
	case KindFloat:
		return "float"
	case KindString:
		return "string"
	case KindArray:
		return "array"
	case KindObject:
		return "object"
	}
	return "unknown"
}

// Path punctuation. A JSON path is the dotted sequence of keys from the root,
// with ArrayElem marking a step into an array's elements and Wildcard marking a
// step into the values of a map-shaped object.
const (
	// ArrayElem is appended to an array's path to name its elements, as in
	// "data.items[]" and "data.items[].status".
	ArrayElem = "[]"
	// Wildcard names the values of a map-shaped object, as in "{*}.status" for
	// a body like {"user_8123":{"status":"active"}}.
	Wildcard = "{*}"
	// OverflowPath is the well-known path whose Present count records responses
	// that exceeded MaxFields. It is a real entry in Fields so that the loss is
	// visible in reports and survives Merge as an ordinary count.
	OverflowPath = "{overflow}"
)

// Limits on what a single Schema will retain. Each is a hard cap with a defined
// behaviour on being hit, so no map here can grow without bound.
const (
	// MaxEnumCardinality caps the distinct string values retained per field.
	// On the cap being reached, StringOverflow is set and no further distinct
	// values are recorded; values already recorded keep being counted.
	MaxEnumCardinality = 64

	// MaxFields caps the paths retained per schema. Paths beyond it are
	// dropped and the response is counted at OverflowPath.
	MaxFields = 4096

	// MaxDepth caps how deep Extract descends. At the limit the container's own
	// kind is recorded but its contents are not, so a pathologically nested
	// document costs bounded stack and bounded paths.
	MaxDepth = 32
)

// FieldStats accumulates observations for one JSON path.
//
// Every counter here is in units of *responses*: each one is the number of
// responses in which the thing was observed at least once, never the number of
// occurrences within a response. A field inside a 50-element array therefore
// contributes 1, not 50.
//
// That choice is load-bearing rather than incidental. Detect tests a presence
// rate for significance, which assumes independent trials; the 50 elements of
// one array are emphatically not independent, since they came from one code
// path on one server at one moment. Counting occurrences would inflate n by an
// arbitrary factor and manufacture significance out of nothing.
//
// The cost is that variation within a single response is invisible: a response
// where 3 of 5 items carry a field looks identical to one where all 5 do.
//
// Absence and explicit null are tracked separately on purpose: a key that is
// gone and a key that is present holding null are different upstream changes,
// and client code breaks differently on each.
type FieldStats struct {
	// Seen is the number of responses in which this field's parent container
	// was present, and so this field could have appeared.
	Seen uint64
	// Present is the number of responses in which the key existed, whatever
	// value it held -- null included.
	Present uint64
	// ExplicitNull is the number of responses in which the key existed and
	// held null. It is a subset of Present.
	ExplicitNull uint64
	// TypeCounts is the number of responses in which each kind was observed.
	// Because one response can hold several kinds at one path -- an array
	// whose elements disagree -- these can sum to more than Present.
	TypeCounts map[Kind]uint64
	// StringValues is the number of responses in which each distinct string
	// value was observed, bounded by MaxEnumCardinality.
	StringValues map[string]uint64
	// StringOverflow reports that distinct string values exceeded
	// MaxEnumCardinality, so StringValues is a partial view and enum-addition
	// findings cannot be trusted for this path.
	StringOverflow bool
}

// newFieldStats returns stats with its maps ready to use.
func newFieldStats() *FieldStats {
	return &FieldStats{
		TypeCounts:   make(map[Kind]uint64),
		StringValues: make(map[string]uint64),
	}
}

// PresenceRate returns Present/Seen, or 0 when nothing has been seen.
func (f *FieldStats) PresenceRate() float64 {
	if f == nil || f.Seen == 0 {
		return 0
	}
	return float64(f.Present) / float64(f.Seen)
}

// NullRate returns ExplicitNull/Present, or 0 when the field was never present.
func (f *FieldStats) NullRate() float64 {
	if f == nil || f.Present == 0 {
		return 0
	}
	return float64(f.ExplicitNull) / float64(f.Present)
}

// Clone returns a deep copy, so callers can merge or mutate without disturbing
// the original.
func (f *FieldStats) Clone() *FieldStats {
	if f == nil {
		return nil
	}
	c := &FieldStats{
		Seen:           f.Seen,
		Present:        f.Present,
		ExplicitNull:   f.ExplicitNull,
		StringOverflow: f.StringOverflow,
		TypeCounts:     make(map[Kind]uint64, len(f.TypeCounts)),
		StringValues:   make(map[string]uint64, len(f.StringValues)),
	}
	for k, v := range f.TypeCounts {
		c.TypeCounts[k] = v
	}
	for k, v := range f.StringValues {
		c.StringValues[k] = v
	}
	return c
}

// Schema is a flat map from JSON path to stats.
// Paths use dot notation with [] for array elements: "data.items[].status".
type Schema struct {
	// Samples is the number of responses this schema summarises.
	Samples uint64
	// Fields maps JSON path to accumulated stats, capped at MaxFields.
	Fields map[string]*FieldStats
}

// New returns an empty Schema ready to accumulate.
func New() *Schema {
	return &Schema{Fields: make(map[string]*FieldStats)}
}

// Clone returns a deep copy of s.
func (s *Schema) Clone() *Schema {
	if s == nil {
		return nil
	}
	c := &Schema{Samples: s.Samples, Fields: make(map[string]*FieldStats, len(s.Fields))}
	for p, f := range s.Fields {
		c.Fields[p] = f.Clone()
	}
	return c
}

// Paths returns the schema's paths in sorted order, for stable reporting.
func (s *Schema) Paths() []string {
	if s == nil {
		return nil
	}
	out := make([]string, 0, len(s.Fields))
	for p := range s.Fields {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// ParentPath returns the path of the container that holds path, and whether one
// exists. A root-level path has no parent, and its Seen is the schema's total
// sample count rather than any field's.
//
// Merge needs this: when a path is absent from one of the two schemas, the
// responses that schema examined still counted as chances for the field to
// appear, and only the parent knows how many there were.
func ParentPath(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	// An element step is part of its container's path, not a level of its own:
	// the parent of "data.items[].status" is "data.items[]".
	if i := lastSep(path); i >= 0 {
		return path[:i], true
	}
	return "", false
}

// lastSep returns the index at which path's final component begins its
// separator, or -1 if path is a root-level name.
func lastSep(path string) int {
	// Trailing "[]" makes the array itself the parent: "a.b[]" -> "a.b".
	if len(path) >= 2 && path[len(path)-2:] == ArrayElem {
		return len(path) - 2
	}
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '.' {
			return i
		}
	}
	return -1
}

// OpportunityCount returns how many of a schema's responses could have carried
// the field at path: the parent container's Present count, or the total sample
// count for a root-level path.
//
// It is the denominator for a field that is absent from a schema entirely --
// the number of chances it had to appear and did not. Merge uses it, and so
// does the detector, which needs the same denominator to compare a field that
// vanished against the window it vanished from.
func OpportunityCount(s *Schema, path string) uint64 {
	if s == nil {
		return 0
	}
	parent, hasParent := ParentPath(path)
	if !hasParent {
		return s.Samples
	}
	pf, ok := s.Fields[parent]
	if !ok {
		return 0
	}
	return pf.Present
}
