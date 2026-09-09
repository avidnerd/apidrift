# internal/eval

The evaluation harness.

## What the harness does

1. `LoadSpec` reads two versions of an OpenAPI document.
2. `Diff` turns them into a ground-truth changelist labelled by
   `detect.ChangeKind`.
3. `Generator` produces synthetic responses conforming to each version, with
   configurable per-field presence rates.
4. `Run` feeds v1 traffic through the pipeline as the baseline and v2 traffic as
   the current window, runs `detect.Detect`, and scores the output.
5. `LatencySweep` measures how many responses a finding takes to fire, as a
   function of the field's presence rate.

## What Score must do

This section is the specification for `Score`, written before the
implementation. The executable version is `TestScore*`.

```go
func Score(findings []detect.Finding, truth []Change) ScoreResult
```

Match findings to ground truth and tally. Both sides have an identity — endpoint,
path, kind — and matching is on that identity, exactly.

### The rules

**Identity is (Endpoint, Path, Kind).** All three must agree. `Change.ID()` and
`Finding.ID()` produce comparable values.

**A finding on the right path with the wrong kind is a false positive, and the
truth entry it failed to match is a false negative.** It is not a partial
credit and it is not a near miss. A tool that says "this field's type changed"
when the field was actually removed has told you something false, and you will
act on it wrongly. Counting it as a hit would flatter the detector precisely
where its output is most misleading.

**Duplicate findings for one change count once.** Two findings with the same ID
are one true positive, not two — the second adds no information and inflating
the score with it would reward a detector for repeating itself.

**A truth entry with no matching finding is a false negative.** One per truth
entry, once.

**A finding with no matching truth entry is a false positive.** One per distinct
finding ID.

**`ByKind` is tallied by the kind of the entry being counted**: a true positive
under the kind both sides agreed on; a false negative under the truth entry's
kind; a false positive under the *finding's* kind, since that is the claim that
was made. `ByKindName` must carry the same counts keyed by `Kind.String()`, for
JSON output.

Empty inputs are valid: no findings and no truth is an all-zero result, not an
error.

### The tests

In `score_test.go`:

- A finding on the right path with the wrong `ChangeKind` is not a true
  positive.
- Two findings on the same change count once.
- A truth entry with no matching finding is a false negative.
- Same path, different endpoint, is not a match.
- Per-kind tallies add up to the totals.

## What the harness reports

**Recall per `ChangeKind`** — of the changes of each kind that really happened,
how many were found.

**False positive rate under the null** — the headline number. v1 traffic is run
through *both* windows, so nothing changed by construction and every finding is
false. This is the number that decides whether the tool is worth running: a
detector that finds everything and also cries wolf every hour gets muted in a
week, at which point its recall is zero.

**Detection latency** — how many responses are needed before a finding fires, as
a function of the field's presence rate. A field that drops from 100% to 0% is
obvious in a handful of responses; one that drops from 100% to 90% needs
hundreds. Emitted as CSV.

## Note on what is measured

`Run` files responses under the endpoint key derived from the spec path, rather
than putting them through `endpoint.Templater`. The harness measures the
*detector*: mixing in the templater's warm-up would make a detection-latency
number that is partly about path templating, and there would be no way to tell
which half of a bad result came from where. The templater is tested on its own
terms in `internal/endpoint`.
