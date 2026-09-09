package schema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

// Map-shape detection. A JSON object is either a record -- a fixed set of
// hand-written field names -- or a map, whose keys are data. Treating a map as
// a record produces one path per key, which is unbounded by construction and
// buries the real structure under thousands of one-observation fields.
const (
	// MinMapKeys is the fewest keys any object can have and still be treated
	// as a map. One key is a record with one field.
	MinMapKeys = 2

	// MapKeyIDFraction is the share of keys that must be identifier-shaped for
	// key form alone to settle it.
	MapKeyIDFraction = 0.8

	// MapKeyHardLimit is the key count past which an object with structurally
	// identical values is treated as a map whatever its keys look like. Beyond
	// this, per-key paths cost more than the distinction is worth.
	MapKeyHardLimit = 64
)

// ErrEmptyBody is returned by Extract for an empty body.
var ErrEmptyBody = errors.New("schema: empty body")

// Extract builds a single-sample Schema from one response body.
//
// Every counter in the result is 0 or 1, because a Schema counts responses and
// this is one response. Repetition within the body -- fifty array elements
// carrying the same field -- sets the counter to 1 rather than to fifty; see
// FieldStats for why that is the honest unit.
//
// Extract records only what it saw. A key that is absent produces no entry at
// all, since a single response cannot distinguish "this key is optional" from
// "this key does not exist". Turning absence into a Seen without a Present is
// Merge's job, and needs a second schema to be meaningful.
func Extract(body []byte) (*Schema, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, ErrEmptyBody
	}

	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber() // so 1 and 1.0 stay distinguishable

	var doc any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("schema: decoding body: %w", err)
	}
	// A body with trailing content is not one JSON document, and silently
	// analysing its first half would misrepresent the response.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("schema: body has trailing content after the JSON document")
	}

	s := &Schema{Samples: 1, Fields: make(map[string]*FieldStats)}
	e := &extractor{schema: s}
	e.walk("", doc, 0)
	return s, nil
}

// extractor carries the per-call state Extract needs: the schema being built
// and whether it has run out of room for new paths.
type extractor struct {
	schema     *Schema
	overflowed bool
}

// field returns the stats for path, allocating within the MaxFields budget.
// It returns nil once the budget is spent, and the caller then stops descending.
func (e *extractor) field(path string) *FieldStats {
	if f, ok := e.schema.Fields[path]; ok {
		return f
	}
	if len(e.schema.Fields) >= MaxFields {
		e.noteOverflow()
		return nil
	}
	f := newFieldStats()
	e.schema.Fields[path] = f
	return f
}

// noteOverflow records, once, that this response lost paths to the cap.
func (e *extractor) noteOverflow() {
	if e.overflowed {
		return
	}
	e.overflowed = true
	// The overflow marker is recorded directly rather than through field(), so
	// that a full map still has room to admit that it is full.
	f, ok := e.schema.Fields[OverflowPath]
	if !ok {
		f = newFieldStats()
		e.schema.Fields[OverflowPath] = f
	}
	f.Seen, f.Present = 1, 1
}

// walk records v at path and descends into it.
//
// Counters are assigned rather than incremented: each means "observed in this
// response at least once", so a second occurrence at the same path is already
// accounted for. That is what makes repeated array elements cost one
// observation instead of many.
func (e *extractor) walk(path string, v any, depth int) {
	if path != "" {
		f := e.field(path)
		if f == nil {
			return // out of paths; record nothing further down this branch
		}
		f.Seen, f.Present = 1, 1
		f.TypeCounts[kindOf(v)] = 1

		switch tv := v.(type) {
		case nil:
			f.ExplicitNull = 1
		case string:
			e.recordString(f, tv)
		}
	}

	if depth >= MaxDepth {
		// The container's own kind is recorded above; its contents are not.
		return
	}

	switch tv := v.(type) {
	case map[string]any:
		e.walkObject(path, tv, depth)
	case []any:
		e.walkArray(path, tv, depth)
	}
}

// recordString accumulates a string value within the cardinality budget.
func (e *extractor) recordString(f *FieldStats, v string) {
	if _, ok := f.StringValues[v]; ok {
		f.StringValues[v] = 1
		return
	}
	if len(f.StringValues) >= MaxEnumCardinality {
		// The field is not an enum. Stop learning new values, and mark the set
		// as partial so nothing downstream reads "value added" into it.
		f.StringOverflow = true
		return
	}
	f.StringValues[v] = 1
}

// walkObject descends into an object, either per key or collapsed to a wildcard
// if the object is map-shaped.
func (e *extractor) walkObject(path string, obj map[string]any, depth int) {
	if isMapShaped(obj) {
		// One path stands for every key. Each value is walked into it, and the
		// assign-not-increment rule keeps the whole map costing one observation.
		wildPath := join(path, Wildcard)
		for _, k := range sortedKeys(obj) {
			e.walk(wildPath, obj[k], depth+1)
		}
		return
	}
	for _, k := range sortedKeys(obj) {
		e.walk(join(path, k), obj[k], depth+1)
	}
}

// walkArray descends into an array's elements, which all share one path.
//
// An empty array leaves the element path absent rather than present-but-empty:
// no element was observed, so there is nothing to have observed a type for. The
// array's own path still records that the array itself was present.
func (e *extractor) walkArray(path string, arr []any, depth int) {
	if len(arr) == 0 {
		return
	}
	elemPath := path + ArrayElem
	for _, v := range arr {
		e.walk(elemPath, v, depth+1)
	}
}

// join appends a key to a path, handling the root case where there is no dot.
func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// sortedKeys returns an object's keys in order, so that a body which overruns
// MaxFields keeps a deterministic subset rather than a random one.
func sortedKeys(obj map[string]any) []string {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// kindOf maps a decoded JSON value to its Kind.
func kindOf(v any) Kind {
	switch tv := v.(type) {
	case nil:
		return KindNull
	case bool:
		return KindBool
	case string:
		return KindString
	case json.Number:
		return numberKind(tv)
	case []any:
		return KindArray
	case map[string]any:
		return KindObject
	}
	// Unreachable for values produced by encoding/json with UseNumber.
	return KindNull
}

// numberKind classifies a number by how it was written, not by its value: 1 is
// an int and 1.0 is a float. The written form is what a client's parser sees,
// and an upstream that starts emitting 1.0 where it emitted 1 has changed
// something worth knowing about.
func numberKind(n json.Number) Kind {
	if strings.ContainsAny(n.String(), ".eE") {
		return KindFloat
	}
	return KindInt
}

// isMapShaped reports whether an object's keys are data rather than field names.
//
// Two independent grounds, requiring very different amounts of evidence:
//
//   - The keys look like identifiers. Key form is strong evidence in itself --
//     nobody hand-writes a field called "user_8123" -- so this needs only that
//     the values agree on a kind, and applies from two keys upward. Waiting for
//     a key count would be waiting for evidence already in hand, and would make
//     the decision depend on how many entries a particular response happened to
//     return.
//   - The object has more keys than per-key paths are worth. Word-shaped keys
//     are weak evidence, so this needs both a large count and values that are
//     structurally identical -- which is what a map's values are, and what a
//     record's heterogeneous fields are not.
func isMapShaped(obj map[string]any) bool {
	n := len(obj)
	if n < MinMapKeys {
		return false
	}
	if !valuesShareKind(obj) {
		return false
	}

	idKeys := 0
	for k := range obj {
		if isKeyIDShaped(k) {
			idKeys++
		}
	}
	if float64(idKeys)/float64(n) >= MapKeyIDFraction {
		return true
	}
	return n >= MapKeyHardLimit && valuesShareObjectShape(obj)
}

// valuesShareKind reports whether every value in obj has the same Kind.
func valuesShareKind(obj map[string]any) bool {
	first, set := KindNull, false
	for _, v := range obj {
		k := kindOf(v)
		if !set {
			first, set = k, true
			continue
		}
		if k != first {
			return false
		}
	}
	return true
}

// valuesShareObjectShape reports whether every value is an object with exactly
// the same key set. Non-object values trivially qualify, since valuesShareKind
// has already established they agree.
func valuesShareObjectShape(obj map[string]any) bool {
	var want string
	set := false
	for _, v := range obj {
		child, ok := v.(map[string]any)
		if !ok {
			return true
		}
		sig := strings.Join(sortedKeys(child), "\x00")
		if !set {
			want, set = sig, true
			continue
		}
		if sig != want {
			return false
		}
	}
	return true
}
