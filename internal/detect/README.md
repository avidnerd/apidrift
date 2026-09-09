# internal/detect

This file is the specification for `Detect`, written before the implementation
and kept independent of it. The executable version is `TestDetect*`. The
persistence wrapper (`Tracker`) is separate — bookkeeping over `Detect`'s
verdicts, not part of the decision.

## What Detect must do

```go
func Detect(baseline, current Window, cfg Config) []Finding
```

Compare two windows of the same endpoint and return the changes that are real.
The hard part is not finding differences — every field differs a little between
any two samples — it is refusing to report the ones that are noise.

### Four gates, in order

Each gate exists to stop a specific kind of false positive. A finding must pass
all four.

**1. Sample size.** If either window has fewer than `cfg.MinSamples` responses,
return nothing for that endpoint, whatever the difference looks like. A field
that went 1-for-1 to 0-for-1 is a 100% drop and means nothing.

**2. Effect size.** The absolute change in presence rate must be at least
`cfg.MinEffectSize`. This is separate from significance and cannot be replaced
by it: with a large enough n, a drop from 60.0% to 59.9% is statistically
significant and operationally irrelevant. Significance answers "is this real";
effect size answers "do I care". Both must be yes.

**3. Significance.** A two-proportion test on (present, seen) for the two
windows, yielding `Finding.PValue`. Fisher's exact test or a two-sided
z-test on proportions are both defensible; a z-test needs a guard for small
expected counts, which `MinSamples` largely provides.

**4. Multiple-comparison correction.** This is the gate that decides whether the
tool is usable. Testing 3,000 fields at p < 0.05 yields ~150 findings a window
from pure chance, which is a tool nobody reads. Apply Benjamini-Hochberg at
`cfg.FDRAlpha` across *all* tests in the call:

> Sort the p-values ascending. Find the largest k where p(k) ≤ (k/m)·α, where m
> is the number of tests. Reject the hypotheses for ranks 1..k.

Correct across every test performed, not per field — the whole point is that the
count of tests is what inflates the error rate.

### What counts as which kind

| Kind | Condition |
|---|---|
| `FieldRemoved` | presence rate dropped significantly |
| `FieldAdded` | path absent from baseline, present in current |
| `TypeChanged` | a kind's share of observations moved significantly |
| `NullabilityIntroduced` | `ExplicitNull`/`Present` rose significantly |
| `EnumValueAdded` | a string value appears in current and not baseline |

`EnumValueAdded` must not fire when either side has `StringOverflow` set: the
value set is partial there, so "not in the baseline" is unproven.

### Severity

- **`SeverityCritical`** — a type flip, e.g. string to int. It breaks parsing, or
  worse, silently coerces.
- **`SeverityHigh`** — a field removed, or nullability introduced. A caller that
  read the field now gets nothing or a null.
- **`SeverityInfo`** — a new optional field, or a new enum value. Existing
  callers are unaffected; new behaviour may be waiting.

### Two things Detect does not do

**It does not know its endpoint.** The signature takes two windows and nothing
else, so leave `Finding.Endpoint` zero. The caller stamps it — see
`cmd/apidrift`.

**It does not decide persistence.** `PersistWindows` is read by `Tracker`, not
by `Detect`. `Detect` answers about one pair of windows.

### Evidence

`Finding.Evidence` is a short line carrying the raw counts, because rates alone
hide the sample size that makes them meaningful:

```
present in 2000/2000 baseline, 94/100 current
```

The report prints it verbatim under the rates.

## The tests

All in `detect_test.go`:

- `2000/2000` baseline vs `94/100` current on one field → a finding.
- 60% vs 55% at n=100 → nothing: the effect is below `MinEffectSize`.
- Below `MinSamples` in either window → nothing, whatever the difference.
- **3,000 unchanged fields → near-zero findings.** This is the false-positive
  guard, and the one that actually decides whether the tool is worth running.
- A string→int flip is `SeverityCritical`.
- A brand-new optional field is `SeverityInfo`.

## Tracker

`Tracker.Observe(findings)` returns only those that have appeared in
`PersistWindows` consecutive windows.

Noise findings are uncorrelated between windows and vanish; a real upstream
change is in every window after the deploy. Persistence costs one window of
detection latency and removes a whole class of false positive.

A qualifying finding keeps being returned while it keeps appearing — it is a
condition that is still true, not an event that fired. A finding that misses a
window is forgotten and starts over.
