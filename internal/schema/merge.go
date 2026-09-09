package schema

import "sort"

// This file implements a contract that was specified before it was written:
// the prose version is in this package's README.md, the executable version is
// TestMerge* in merge_test.go. The invariants there -- associativity,
// commutativity, and the absent-path rule -- are what the store and the
// detector rely on, so they are stated independently of this implementation.

// Merge combines two schemas. MUST be associative and commutative so partial
// schemas can be merged in any order.
//
// Almost all of this is addition, because every counter in a FieldStats is a
// number of responses and responses from two windows simply add. The one place
// with any subtlety is a path that appears in one schema and not the other,
// handled by absentSeen below.
//
// Neither argument is modified; both may be nil.
func Merge(a, b *Schema) *Schema {
	switch {
	case a == nil && b == nil:
		return New()
	case a == nil:
		return b.Clone()
	case b == nil:
		return a.Clone()
	}

	out := &Schema{
		Samples: a.Samples + b.Samples,
		Fields:  make(map[string]*FieldStats, len(a.Fields)+len(b.Fields)),
	}

	for _, path := range unionPaths(a, b) {
		fa, inA := a.Fields[path]
		fb, inB := b.Fields[path]

		switch {
		case inA && inB:
			out.Fields[path] = mergeStats(fa, fb)
		case inA:
			out.Fields[path] = withAbsentSide(fa, b, path)
		default:
			out.Fields[path] = withAbsentSide(fb, a, path)
		}
	}
	return out
}

// mergeStats adds two sets of observations for the same path.
func mergeStats(a, b *FieldStats) *FieldStats {
	out := &FieldStats{
		Seen:           a.Seen + b.Seen,
		Present:        a.Present + b.Present,
		ExplicitNull:   a.ExplicitNull + b.ExplicitNull,
		StringOverflow: a.StringOverflow || b.StringOverflow,
		TypeCounts:     make(map[Kind]uint64, len(a.TypeCounts)+len(b.TypeCounts)),
	}
	for k, n := range a.TypeCounts {
		out.TypeCounts[k] += n
	}
	for k, n := range b.TypeCounts {
		out.TypeCounts[k] += n
	}
	out.StringValues, out.StringOverflow = mergeStringValues(a, b, out.StringOverflow)
	return out
}

// withAbsentSide builds the merged stats for a path that only one schema has.
//
// This is the rule with teeth. Copying present's counters over unchanged would
// be wrong: the other schema's responses were each an opportunity for this
// field to appear, and it did not take them. Ignoring that makes a field look
// 100% present when it may be 50% present.
//
// How many opportunities there were is however many of the other schema's
// responses had this field's parent -- which only the parent knows.
func withAbsentSide(present *FieldStats, absent *Schema, path string) *FieldStats {
	out := present.Clone()
	out.Seen += absentSeen(absent, path)
	return out
}

// absentSeen returns how many of a schema's responses could have carried the
// field at path, given that none of them did.
//
// Reading this from the *operand* rather than from any partially merged result
// is what keeps Merge associative: the parent's Present is itself additive, so
// grouping the operands differently produces the same total either way.
func absentSeen(absent *Schema, path string) uint64 {
	return OpportunityCount(absent, path)
}

// mergeStringValues adds two value sets within the cardinality budget.
//
// On exceeding the cap the surviving subset is the lexicographically smallest,
// which is an arbitrary choice made for one reason: it is deterministic and it
// composes. Keeping "the most frequent" would read better but is not
// associative -- a value dropped in an early merge loses the counts that would
// have kept it in a later one, so the answer would depend on grouping.
//
// Which subset survives matters little in practice, because overflow marks the
// set as partial and nothing downstream may read "enum value added" out of it.
func mergeStringValues(a, b *FieldStats, overflow bool) (map[string]uint64, bool) {
	merged := make(map[string]uint64, len(a.StringValues)+len(b.StringValues))
	for v, n := range a.StringValues {
		merged[v] += n
	}
	for v, n := range b.StringValues {
		merged[v] += n
	}

	if len(merged) <= MaxEnumCardinality {
		return merged, overflow
	}

	keys := make([]string, 0, len(merged))
	for v := range merged {
		keys = append(keys, v)
	}
	sort.Strings(keys)

	capped := make(map[string]uint64, MaxEnumCardinality)
	for _, v := range keys[:MaxEnumCardinality] {
		capped[v] = merged[v]
	}
	return capped, true
}

// unionPaths returns every path in either schema, in sorted order so a merge is
// deterministic regardless of map iteration.
func unionPaths(a, b *Schema) []string {
	seen := make(map[string]bool, len(a.Fields)+len(b.Fields))
	out := make([]string, 0, len(a.Fields)+len(b.Fields))

	for _, s := range []*Schema{a, b} {
		for p := range s.Fields {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}
