# apidrift

[![ci](https://github.com/avidnerd/apidrift/actions/workflows/ci.yml/badge.svg)](https://github.com/avidnerd/apidrift/actions/workflows/ci.yml)
[![go](https://img.shields.io/badge/go-1.24-00ADD8)](https://go.dev)
[![license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

Ongoing personal project, Fall 2026. Go, ~7k lines.

**Goal:** build a basic version of YC's [Self-Maintaining APIs](https://www.ycombinator.com/rfs)
request for startups, which asks for an agent that "should scan customer
codebases, identify affected usages, and open a PR with the fix" when a
third-party API changes.

**Overview:** most breaking API changes are easy to catch. Your client stops
compiling, or something throws and lands in your error tracker. The ones that
cause real damage are the ones where nothing fails at all. A field goes from
always present to present 94% of the time, your code reads it, gets an empty
string 6% of the time, and carries on, and nobody notices for weeks. That is
what this targets.

The RFS is written provider-side, where the provider pushes fixes out to its
customers. I built the consumer side, since the reason the problem exists is
that providers mostly do not do this. apidrift needs no cooperation from the API
it watches.

**Approach:** two detection surfaces, in order of how much they cost to set up.

1. *Spec diffing.* Fetch a provider's OpenAPI spec, diff it against the copy
   cached on the previous run, and keep the changes that can break an existing
   caller. Requires no infrastructure at all, so this is the default path and
   runs as a scheduled GitHub Action.
2. *Traffic observation.* A reverse proxy tees JSON response bodies into a
   bounded queue, groups raw paths into endpoints with a prefix trie, flattens
   bodies into per-path statistics, and accumulates them into hourly buckets.
   Two windows of one endpoint are then compared. Catches what a spec cannot,
   such as a field documented as required that is actually absent 6% of the
   time, at the cost of routing production traffic through it.

Given a confirmed change from either surface, it greps the repository for the
field in every spelling it plausibly takes in source (`legacy_id`, `legacyId`,
`LegacyID`), sends the real files to an LLM along with what changed, and gets
back exact text replacements. Each replacement is verified before anything is
written: the file has to exist, be inside the repo, and contain the old text
exactly once. Then it commits on a branch and opens a PR.

**Statistical detail (the traffic path):** the hard part is not finding
differences, since any two samples of anything differ. It is refusing to report
the ones that are noise. Testing 3000 JSON paths at p < 0.05 produces roughly
150 findings per window from chance alone. Four gates in order: a minimum sample
size, a minimum effect size, a two-proportion z-test, and Benjamini-Hochberg
correction across every test in the run. Findings must also survive several
consecutive windows before being reported, since sampling noise is uncorrelated
between windows and a real change is not.

**Results:**

* Against three months of Stripe's published spec (2026-05-27 to 2026-08-26,
  7.7 MB, 419 paths, 1454 schemas) it found 57 removed fields, including `iin`,
  `issuer` and `three_d_secure.cryptogram` disappearing from card objects. Parses
  in 0.37s, diffs in 0.84s.
* The same run found 552 newly added enum values, which is why those are now off
  by default. At 552 against 57 the real signal is buried. My synthetic fixture
  had 1 enum change out of 11, a ratio that made including them look reasonable.
* The traffic detector scores recall 1.000 and precision 1.000 on a synthetic
  benchmark of 11 planted changes, with 0 findings when the null is run through
  both windows. The benchmark is mine and the traffic is independent by
  construction, so this shows the machinery is calibrated. It says nothing
  about production.
* Proxy overhead is below what the benchmark can measure. Both instrumented arms
  come out fractionally faster than a bare `httputil.ReverseProxy`, which is
  impossible, so the overhead is smaller than the 2.4 microsecond run-to-run
  variance.

**Todos:**

* I need to run this against a repository that actually consumes Stripe, because
  every codesearch result so far has been against test fixtures or apidrift's own
  source code, which tells me nothing about whether the call sites it finds in a
  real codebase are the right ones.
* There is no persistence anywhere. The traffic path holds a week of history in
  memory and loses all of it when the process restarts, which is a serious
  problem for something whose baseline window is 24 hours, so it needs to write
  its buckets to disk or to an embedded key-value store.
* The LLM layer has no measured false positive rate. It is non-deterministic and
  I have no ground truth for whether the meaning of a value actually changed, so
  I would need to build a labelled set of real semantic changes and score it the
  same way the statistical detector is scored.
* The path templater currently learns from URL structure alone. It would be
  considerably better if it could read the provider's spec to know the real route
  templates in advance, which would remove the warm-up period entirely and stop
  it guessing about segments it has not seen enough of.
* Detection currently compares two fixed windows, which means a change that
  happens gradually over several days is invisible because no single pair of
  windows differs by more than the effect threshold. A cumulative or sequential
  test such as CUSUM would catch slow drift that the current design cannot see.
* The semantic layer only looks at one JSON path at a time, so it cannot notice
  that two fields changed together in a way that is meaningful, such as a
  currency field appearing at the same time an amount changes scale. Passing
  correlated paths to the model as a group would let it reason about the change
  as one event.
* The traffic path treats every response as an independent observation, which is
  not quite true because responses in one window come from the same deployed
  version on the provider's side and are therefore correlated at the deploy
  level. This inflates apparent significance somewhat. Correcting for it
  properly would mean moving to a hierarchical model.
* A GitHub App would be a much better product than the current GitHub Action.
  The Action requires the user to add a workflow file, know the spec URL, and
  supply their own API key. An App could be installed in one click and work out
  which APIs a repository depends on by reading its imports and lockfile.
* I would like to know whether the observation that providers are disciplined
  about types and careless about removing fields holds beyond Stripe. Three
  months of their spec produced zero type changes and zero introduced nulls
  against 57 removals. If that pattern generalises, this is really a tool about
  removed fields specifically, and the scope could narrow to match.

```sh
make build
./bin/apidrift demo                                      # runs the whole pipeline, no setup
./bin/apidrift watch --spec <openapi-url> --repo . --pr  # the spec path
```

`DECISIONS.md` records the design choices as *chose / over / because / revisit
if*, including two places where I had to correct numbers I had already published.
