# internal/schema

This file is the specification for `Merge`, written before the implementation
and kept independent of it. The executable version is `TestMerge*`.

## What Merge must do

```go
func Merge(a, b *Schema) *Schema
```

Combine two schemas into a new one. Neither input may be modified. Either may be
`nil`, and `Merge(nil, nil)` is an empty schema, not a nil pointer.

### The unit everything is counted in

Every counter in `FieldStats` is a number of **responses** — "how many responses
did we see this in at least once" — never a number of occurrences. `Extract`
already enforces this: a field appearing in fifty array elements of one response
contributes 1.

That is why `Merge` is almost entirely addition. It is also why the counters
mean anything statistically: `Detect` tests presence rates for significance,
which assumes independent trials, and responses are the largest unit that is
plausibly independent.

### The rules

**Samples** adds: `out.Samples = a.Samples + b.Samples`.

**A path present in both** adds field by field, and `StringOverflow` ors:

```
Seen         = a.Seen + b.Seen
Present      = a.Present + b.Present
ExplicitNull = a.ExplicitNull + b.ExplicitNull
TypeCounts[k] = a.TypeCounts[k] + b.TypeCounts[k]   for every k in either
StringValues[v] = a.StringValues[v] + b.StringValues[v]  for every v in either
StringOverflow = a.StringOverflow || b.StringOverflow
```

**A path present in only one** is the case with teeth, and the one the tests
lean on hardest. Suppose `"data.status"` is in `a` and not in `b`. It is
tempting to copy `a`'s stats over unchanged. That is wrong: `b`'s responses were
each an opportunity for `data.status` to appear, and it did not. Ignoring them
would make the field look 100% present when it may be 50% present.

So the absent side contributes to `Seen` and not to `Present`. How much it
contributes is however many of its responses *could* have carried the field —
that is, however many had the field's parent:

```
absentSeen = b.Fields[ParentPath(path)].Present    if the path has a parent
absentSeen = b.Samples                             if the path is root-level
absentSeen = 0                                     if b has no entry for the parent
```

Then `out.Seen = a.Seen + absentSeen`, and `out.Present = a.Present`.

`ParentPath` is provided. Note that `[]` is part of its container's path, so the
parent of `data.items[].status` is `data.items[]`, whose own parent is
`data.items`.

**String cardinality**: the merged `StringValues` is capped at
`MaxEnumCardinality`. On exceeding it, set `StringOverflow` and stop adding new
distinct values rather than growing the map. A merged schema with
`StringOverflow` set has a partial value set, and nothing downstream may read
"enum value added" out of it.

## The invariants

These are what the tests check, and what `Detect` relies on.

1. **Commutative.** `Merge(a, b)` equals `Merge(b, a)`, field for field.
2. **Associative.** `Merge(Merge(a, b), c)` equals `Merge(a, Merge(b, c))`.
   Windows are merged from ring-buffer buckets in whatever order the store
   walks them, and the answer may not depend on that order.
3. **Counts are never collapsed to a boolean.** There is no "optional" flag.
   `Present`/`Seen` is a rate, and a field present in 999 of 1000 responses is a
   different observation from one present in 500 of 1000. Detect needs both.
4. **Kinds are never collapsed to "mixed".** A field seen as a string 1000 times
   and an int 3 times keeps `{string: 1000, int: 3}`. The 3 is the finding.
5. **`Seen >= Present >= ExplicitNull`** for every field, always.
6. **Neither input is modified.** The store hands out schemas that other windows
   still reference.

Associativity and commutativity fall out of addition — the only place they can
break is the absent-path rule, where an implementation that reads `Seen` from a
partially-merged parent can produce order-dependent results. That is exactly
what `TestMergeAssociative` is built to catch.

## Extract, for contrast

`Extract`'s behaviour is settled:

- One response in, every counter 0 or 1 out.
- A missing key produces no entry; a key holding `null` produces
  `Present=1, ExplicitNull=1, TypeCounts[KindNull]=1`.
- Array elements share one path ending in `[]`.
- Map-shaped objects collapse to `{*}` (see `isMapShaped`, and DECISIONS.md).
- Caps: `MaxEnumCardinality` distinct strings per field, `MaxFields` paths per
  schema (overflow recorded at `OverflowPath`), `MaxDepth` levels of nesting.
