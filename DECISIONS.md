# Decisions

A running log of every non-obvious choice made while building apidrift, and of
every design question that had to be guessed at. Newest entries at the bottom.

Format:

```
## <date> — <decision>
**Chose:** ...
**Over:** ...
**Because:** ...
**Revisit if:** ...
```

---

## 2026-09-04 — Module path `github.com/avidnerd/apidrift`
**Chose:** A GitHub-shaped module path matching the configured git user.
**Over:** A bare `apidrift` module path.
**Because:** A bare path works locally but breaks the moment anything imports the
repo by URL, and renaming a module path later churns every import line in the
tree. The cost of guessing right now is zero; the cost of guessing wrong is one
`sed`.
**Revisit if:** The repo lives somewhere other than `github.com/avidnerd`.

## 2026-09-04 — `gofmt` only, no external linter
**Chose:** `make lint` = `gofmt -l` (reporting, not rewriting) plus `go vet`.
**Over:** `golangci-lint`, `staticcheck` or `gofumpt`.
**Because:** The project constraint is standard library wherever possible and
"ask before adding a dependency". An external linter is a real dependency —
version pinning, CI install time, and a rule set someone has to own. `vet`
catches the class of bug that actually matters here (printf mismatches, lost
struct tags, unreachable code) and is free.
**Revisit if:** Review starts catching things mechanically — unchecked errors,
shadowed variables — that `staticcheck` would have caught first. That is the
signal to pay the dependency cost, and it should be paid deliberately.

## 2026-09-04 — CI pins Go 1.22 with `check-latest`
**Chose:** `go-version: '1.22'` in the workflow, matching the `go` directive in
`go.mod`.
**Over:** `go-version-file: go.mod`, or tracking the newest release.
**Because:** 1.22 is the floor the project claims to support, so the floor is
what CI should prove. Building only on the latest toolchain lets a 1.23+ feature
slip in unnoticed and silently breaks the stated minimum.
**Revisit if:** The project adopts a language feature newer than 1.22, in which
case raise both the `go.mod` directive and this pin together. (Local development
is currently on 1.27.1, which is fine — the pin is about the floor, not the
ceiling.)

## 2026-09-04 — The sample queue drops instead of blocking
**Chose:** A fixed-depth buffered channel whose `Offer` is a `select` with a
`default`: full means drop and increment a counter.
**Over:** Blocking until there is room, or growing the queue on demand.
**Because:** apidrift sits on someone else's critical path. A blocking hand-off
turns a slow analysis consumer into upstream latency, and then into upstream
timeouts — a monitoring tool causing the incident it exists to detect. An
unbounded queue converts the same backlog into unbounded memory, which fails
later and worse. Dropping degrades the only thing that is safe to degrade: the
statistics. Drift detection is inherently a sampling problem, so a detector fed
95% of traffic is barely distinguishable from one fed 100%, whereas a service
made 200ms slower is very distinguishable indeed. The drop counter is on
`/stats` so the degradation is measurable rather than silent.
**Revisit if:** Drop rates get high enough to hurt detection power. The fix is a
deeper queue or more consumers, both of which are bounded, tunable knobs — not
backpressure.

## 2026-09-04 — Oversize bodies are skipped entirely, not truncated
**Chose:** A body that exceeds `MaxBodyBytes` is dropped and counted in
`skipped_oversize`; captured bytes are released as soon as the cap is passed.
**Over:** Retaining the first `MaxBodyBytes` and analysing the prefix, which is
what "truncated to MaxBodyBytes" in the spec literally asks for.
**Because:** `schema.Extract` parses whole JSON documents. A document cut off at
256 KiB is not a smaller document, it is a syntax error — so a truncated sample
cannot produce a schema, only a parse failure. Keeping it would spend the memory
and the analysis time to reach the same outcome one step later, and would
account for the loss as "parse error" rather than "too big", which is the
diagnostically useless label of the two. The struct invariant the spec cares
about still holds and is now exact: `len(Sample.Body) <= MaxBodyBytes` always.
**Revisit if:** Real traffic shows meaningful volume above the cap. The blind
spot is real — an endpoint that always returns 400 KiB is never observed at all
— and the honest fixes are a higher cap or a streaming parser that extracts
structure without materialising the document. Truncation is not one of them.

## 2026-09-04 — Capture streams through a tee, rather than buffering in ModifyResponse
**Chose:** Wrap `resp.Body` in a tee that copies into a capped buffer as the
proxy streams it to the client, and hand off when the body reaches EOF.
**Over:** Reading the whole body in `ModifyResponse` and replacing it with a
`bytes.Reader`, which is the shorter and more obvious implementation.
**Because:** Buffering in `ModifyResponse` makes the client wait for the last
byte of the response before it receives the first. For a small JSON body that is
nearly free, but it silently converts every streaming or slow-trickling response
into a stall proportional to the upstream's own latency — the exact failure the
"never delay a client request" requirement rules out. The tee never holds more
than the cap, and passes the upstream's bytes and errors through verbatim.
**Revisit if:** Never, for the response path. The buffering approach would only
make sense if capture needed the whole body before forwarding, which it does not.

## 2026-09-04 — A capture that did not reach EOF is discarded
**Chose:** Track whether the body reached `io.EOF`. A body closed early — a
client that hung up mid-response — is counted in `skipped_incomplete` and never
sampled.
**Over:** Sampling whatever bytes arrived before the disconnect.
**Because:** Same reasoning as truncation: a partial document is a parse error,
not a smaller observation. It is worth calling out separately because the failure
correlates with load — clients time out and disconnect exactly when the upstream
is struggling — so sampling partials would inject a burst of "fields missing"
observations precisely when the upstream is misbehaving. That is a false-positive
generator wired directly to incidents.
**Revisit if:** `skipped_incomplete` becomes a large share of traffic, which
would mean the sample is biased toward fast responses and worth reporting.

## 2026-09-04 — Content-encoded responses are skipped, not decoded
**Chose:** A JSON response still carrying `Content-Encoding: gzip` (or any
non-identity coding) is skipped and counted in `skipped_encoded`.
**Over:** Decompressing in the proxy, or stripping `Accept-Encoding` from the
outbound request so the transport decodes transparently.
**Because:** Go's transport decompresses transparently only when it added
`Accept-Encoding` itself, which it does not do when the client asked for an
encoding — so a gzip-requesting client yields compressed bytes here, and
compressed bytes are not JSON. Decoding in the tee would move a CPU-bound gunzip
onto the response path, the one place this design refuses to spend time.
Stripping the client's `Accept-Encoding` would work and is tempting, but it
changes what the client receives from what it asked for; a monitoring proxy
should not quietly rewrite the transfer encoding of the traffic it observes.
**Revisit if:** Coverage measurably suffers. The right fix is then to decode in
the *consumer* rather than the proxy, which needs an encoding field on Sample —
a change to a spec-fixed type, so worth raising rather than assuming.

## 2026-09-04 — /stats is a separate handler, not a path the proxy intercepts
**Chose:** `Proxy.StatsHandler()` returns an `http.Handler` that the CLI mounts
on its own admin listener.
**Over:** Special-casing `/stats` inside `Proxy.ServeHTTP`.
**Because:** Intercepting a path makes the proxy lie. Any upstream with a real
`/stats` route would be shadowed, and the failure mode is a monitoring tool
silently corrupting the API it monitors — precisely the class of bug apidrift
exists to catch. Keeping the handler separate also keeps the operational surface
separate: the admin port can be bound to localhost while the proxy port is not.
**Revisit if:** Running a second listener is genuinely inconvenient, in which
case an explicitly configured, non-default prefix is the compromise — never a
silent default.

## 2026-09-04 — Non-2xx responses are sampled too
**Chose:** Sample every JSON response regardless of status; `endpoint.Key`
carries the status class so 2xx and 4xx shapes are tracked separately.
**Over:** Only sampling successful responses.
**Because:** Error envelopes drift as readily as success payloads, and code that
reads `err.code` breaks just as silently when that field is renamed. Tracking
them under one key would be the actual mistake — merging a 200 shape with a 404
shape produces a schema where every field looks intermittently optional, which
is a false-positive factory. Separate keys make error shapes observable without
contaminating success shapes.
**Revisit if:** 5xx volume during an upstream outage floods the store with junk
endpoints; the answer is eviction policy in phase 2, not dropping the samples.

## 2026-09-04 — Phase 1 benchmark: added latency
**Chose:** Report apidrift against a bare `httputil.ReverseProxy`, not against
talking to the upstream directly.
**Over:** The direct-vs-proxied comparison alone.
**Because:** Deploying apidrift means accepting a proxy hop whatever it does with
the bytes. Direct-vs-proxied measures the cost of proxying (~136µs here, almost
all of it the second local HTTP round trip) and would flatter or damn apidrift
for something it did not cause. The question the requirement actually asks is
what the *analysis* costs, and only the bare-proxy baseline answers it.

Apple M3, local upstream, ~2 KB Stripe-shaped JSON body, sequential client,
`-benchtime 5000x -count 5`, median across the five runs:

| arm | p50 | p99 | B/op | allocs/op |
|---|---|---|---|---|
| direct (no proxy) | 132.3µs | 216.0µs | 5,895 | 67 |
| bare reverse proxy | 268.0µs | 390.4µs | 45,688 | 142 |
| apidrift, pipeline drained | 268.5µs | 401.9µs | 47,904 | 157 |
| apidrift, pipeline saturated | 269.5µs | 397.4µs | 47,906 | 157 |

Added by apidrift over a bare reverse proxy: **+0.5µs p50 drained, +1.5µs p50
saturated** — around 0.2% and 0.6% of the hop it already costs. The p99 deltas
(+11.5µs and +7.0µs) are inside run-to-run noise: the bare-proxy arm itself
produced p99s ranging from 378µs to 678µs across its five runs, a spread far
wider than the difference being measured, so the honest claim is that no p99
regression is detectable at this sample size, not that one was measured at
+11.5µs. Memory cost is +2.2 KB and +15 allocations per request: the capture
buffer, sized from Content-Length, plus the Sample.

Saturated is no slower than drained, which is the load-bearing result: `Offer`
on a full queue is a `select` that takes the `default` branch, so the drop path
is if anything cheaper than the enqueue path. Nothing about a stalled analysis
pipeline reaches the client.
**Revisit if:** Bodies get much larger (the capture copy scales with body size
and the numbers above are for 2 KB), or the upstream is remote enough that these
microseconds stop being worth measuring at all.

## 2026-09-04 — GUESS: defaults picked without confirmation
**Chose:** `MaxBodyBytes` = 256 KiB. Queue depth is left to the CLI in phase 3,
where 4096 is the intended default — a worst case of 4096 × 256 KiB = 1 GiB if
every queued sample were at the cap, but ~8 MB at a realistic 2 KB median body.
**Over:** Asking first. These were flagged in the phase 0 review and the reply
was "yes start", so they are assumptions rather than agreed numbers.
**Because:** 256 KiB clears typical JSON API responses by a wide margin — Stripe
list responses run well under 100 KiB — while capping the memory a single
pathological endpoint can consume. 4096 samples is roughly a second of headroom
at 4k requests/second, enough to absorb a GC pause or a slow consumer without
dropping.
**Revisit if:** The real deployment has a different body-size distribution, or
the 1 GiB theoretical worst case is unacceptable in the target environment — in
which case cap total queued *bytes* rather than sample count.

## 2026-09-04 — Path templating is a prefix trie, not a table of path positions
**Chose:** Learn each position's behaviour in a trie, so the decision about
segment 1 under `/users` is independent of the decision about segment 1 under
`/docs`.
**Over:** Keying statistics by (method, depth, index), which is far cheaper.
**Because:** The same index means different things under different prefixes.
`/users/8123` and `/docs/pricing` both have a second segment, and they are an
identifier and a route name respectively. A position table would pool them,
after which either `/docs/pricing` collapses into `/docs/{id}` — losing every
documentation route into one endpoint — or `/users/{id}` fails to collapse and
the store fills with one endpoint per user. Only a structure that remembers the
prefix can answer the question that was actually asked.
**Revisit if:** Node counts become a problem at very high route cardinality. The
trie is already bounded three ways; the next step would be sharing subtrees, not
flattening to positions.

## 2026-09-04 — Two tiers of evidence for "is this segment a variable"
**Chose:** A shape tier that fires at `MinSamples` (50) when ≥80% of a
position's traffic is identifier-shaped, and a growth tier that fires only at
`StatMinSamples` (500) when distinct values keep arriving in proportion to
traffic. Below 50 observations, `Resolve` refuses to answer at all.
**Over:** One rule. Either a pure shape test, which cannot see opaque variables
like slugs and usernames, or a pure growth test at a single threshold.
**Because:** The two kinds of evidence deserve very different amounts of
patience, and collapsing them to one threshold gets one of the cases wrong.

Shape is per-segment and strong: nothing hand-written looks like
`3f2504e0-4f89-11d3-9a0c-0305e82c3301`, so fifty observations settle it.

Growth is per-position and weak early on, because a large static route set and a
variable are *genuinely indistinguishable* in small samples — fifty requests
spread over forty-five documentation pages look exactly like forty-five
identifiers, and no cleverness recovers the difference from that data. The only
thing that separates them is that one set stops growing. So the growth tier
waits for ten times as much traffic, by which point a real route set has
visibly plateaued (40 routes over 2000 requests is a distinct-per-request rate
of 0.02, nowhere near the 0.5 threshold) and a real variable has not (rate ≈ 1).

This is exactly the case the spec calls out: `/docs/webhooks`, `/docs/pricing`,
`/docs/auth` must not collapse despite high cardinality in that position. They
do not, because they stop arriving.
**Revisit if:** Real traffic shows opaque variables that take too long to be
recognised. The lever is `StatMinSamples`, and lowering it trades warm-up time
for a risk of swallowing large static route sets.

## 2026-09-04 — Resolve refuses to answer below the threshold, even when the shape is obvious
**Chose:** Any undecided position along a path fails the whole `Resolve`, so
`/users/8123/orders` returns ok=false at 49 observations even though `8123` is
plainly an identifier.
**Over:** Answering from shape alone as soon as a segment looks like an id.
**Because:** An endpoint key that changes meaning once more traffic arrives
splits one endpoint's history in two, and the split is invisible: the baseline
window is filed under one key and the current window under another, so drift
detection quietly compares nothing against nothing. Every key resting on the
same amount of evidence is worth a short warm-up.
**Revisit if:** Warm-up latency matters more than key stability — but note the
cost lands on exactly the low-traffic endpoints where statistics are weakest
anyway.

## 2026-09-04 — The variable verdict is sticky; the static verdict is not
**Chose:** Once a position is judged a variable it stays one. Static and
undecided are recomputed on every access.
**Over:** Recomputing both, or caching both.
**Because:** The two errors are not symmetric. Traffic can reveal that a
seemingly static position is a variable — that is the growth tier's whole job —
so "static" must stay revisable. But flapping the other way would renumber
endpoints under live traffic, and the ratio that drives the decision *can* dip:
a burst of repeated identifiers briefly lowers the distinct-per-request rate. A
verdict that flips back would fragment the endpoint's history for the duration
of the burst.
**Revisit if:** A position is ever wrongly judged variable, which is
unrecoverable by design. The protection is that the shape tier is conservative
and the growth tier waits for 500 observations.

## 2026-09-04 — Collapsing a position discards its literal subtrees rather than merging them
**Chose:** When a position flips to variable, drop the children learned under
each literal and start a fresh wildcard subtree.
**Over:** Merging the n subtrees into the wildcard child, preserving what was
learned.
**Because:** The discarded structure is precisely the structure about to be
re-learned in the next few hundred requests, and merging n tries correctly is a
great deal of machinery to save that. Discarding also makes the memory bound
trivially true rather than argued. The cost is a second warm-up for paths below
the collapsed position, during which `Resolve` returns ok=false — visible,
bounded, and self-healing.
**Revisit if:** Collapses turn out to be frequent rather than one-per-position,
which would mean the classification is flapping and is a different bug.

## 2026-09-04 — Segment shape rules are biased toward false negatives
**Chose:** Rules that require forms a hand-written route segment would
essentially never take: all-digits, canonical UUID, ≥12 hex characters, a short
lowercase prefix plus a long qualified body, or ≥20 unbroken alphanumerics
carrying a digit or mixed case.
**Over:** Looser rules that catch more identifiers sooner.
**Because:** The two errors cost wildly different amounts. A false positive
merges two distinct static routes into one endpoint permanently — the verdict is
sticky — and every real drift between them is then averaged away and invisible.
A false negative just means the growth tier does the work a few hundred requests
later. So the thresholds are set where words stop: 12 hex characters excludes
`facade` and `decade` with room to spare, and the mixed-case-or-digit
requirement is what separates `cus_QxYzABCDEFgh` from `payment_intents`.
**Revisit if:** A specific identifier convention in real traffic is being
missed; add a rule for it rather than loosening an existing one.

## 2026-09-04 — endpoint and schema keep separate shape rules
**Chose:** `endpoint.isIDShaped` and `schema.isKeyIDShaped` are separate code
with separate thresholds, despite the family resemblance.
**Over:** One shared helper, or a shared internal package.
**Because:** They answer different questions against differently-shaped data. A
path segment is a bare identifier — `/users/8123` — while a map key is usually a
*qualified* one — `{"user_8123": ...}` — where the prefix is the norm rather
than a special case, and so a bare numeric body needs no minimum length to be
convincing. Sharing one rule set would mean one set of thresholds serving
neither case well, and worse, every later tuning change to one caller would
silently retune the other. This is duplication that buys independence, and it is
about fifty lines.
**Revisit if:** A third caller appears. Two is coincidence; three is a pattern
worth extracting.

## 2026-09-04 — Every schema counter is in units of responses, never occurrences
**Chose:** `Seen`, `Present`, `ExplicitNull`, `TypeCounts` and `StringValues`
all count *responses in which the thing was observed at least once*. A field
appearing in fifty array elements of one response contributes 1.
**Over:** Counting occurrences, which is the obvious reading and gives larger,
more satisfying-looking numbers.
**Because:** `Detect` tests a presence rate for statistical significance, and
significance testing assumes independent trials. The fifty elements of one array
are emphatically not independent — they came from one code path, on one server,
at one instant. Counting them as fifty trials inflates n by an arbitrary,
payload-dependent factor, and inflating n is precisely how you manufacture
significance out of noise: the same 2% difference that is unremarkable at n=100
is overwhelming at n=5000. An endpoint returning big arrays would produce
findings continuously.

The cost is real and worth stating: variation *within* one response is invisible.
A response where 3 of 5 items carry a field is indistinguishable from one where
all 5 do.
**Revisit if:** Within-response variation turns out to matter — a plausible case
is a paginated list where one item type is being deprecated. The fix is a
separate per-occurrence counter used for a differently-designed test, not a
change to these.

## 2026-09-04 — Map-shaped objects collapse to a wildcard path, on two grounds
**Chose:** An object collapses to `{*}` when its values agree on a kind and
either (a) ≥80% of its keys are identifier-shaped, from two keys upward, or
(b) it has ≥64 keys whose values are structurally identical.
**Over:** A single key-count threshold for both.
**Because:** An object is either a record — a fixed set of hand-written field
names — or a map, whose keys are data. Treating a map as a record is unbounded
by construction: `{"user_8123":{...},"user_9944":{...}}` yields one path per
user, and the real structure is buried under thousands of one-observation
fields that each look like a brand-new field appearing.

The two grounds need different evidence, for the same reason the templater's
tiers do. Key form is strong: nobody hand-writes a field called `user_8123`, so
two such keys settle it, and waiting for a key count would be waiting for
evidence already in hand — worse, it would make the decision depend on how many
entries a particular response happened to return, so the same endpoint would
collapse or not depending on the page size. Word-shaped keys are weak evidence,
so that branch needs both a large count and values that are structurally
identical, which a map's values are and a record's heterogeneous fields are not.

The known false positive: a genuine record with ≥64 uniform sub-objects
collapses. It is rare, and at that size per-key paths cost more than the
distinction is worth.
**Revisit if:** Collapses are observed on things that were really records. The
`{*}` path makes this visible in a report rather than silent.

## 2026-09-04 — Numbers are typed by how they are written, not by their value
**Chose:** `1` is `KindInt` and `1.0` is `KindFloat`, decided from the JSON text
via `json.Number`.
**Over:** Decoding to float64 and checking whether the value is integral, which
would call both of those ints.
**Because:** The written form is what a client's parser sees. An upstream that
starts emitting `1.0` where it emitted `1` has changed something — a
serialisation library, a schema type, a currency representation — and in several
languages that is the difference between an int and a float at the call site.
Normalising it away discards a real signal to make the output tidier. Decoding
to float64 would also silently corrupt integers above 2^53, which is a
significant fraction of the id space.
**Revisit if:** It proves noisy in practice. Note that the noise would itself be
a finding: an upstream inconsistently formatting the same field.

## 2026-09-04 — GUESS: the field cap is recorded at a sentinel path
**Chose:** `MaxFields` (4096) caps a schema's paths; a response that exceeds it
is recorded at the well-known path `{overflow}`.
**Over:** Adding a `Truncated bool` to `Schema`, or dropping the sample, or
capping silently.
**Because:** The constraint says every cap needs documented behaviour when hit,
and a silent cap fails that — a whole subtree would vanish with no trace, and
its fields would later read as "removed". A sentinel path costs no change to
`Schema`, which is a type the owner is writing `Merge` against, and it merges as
an ordinary count with no special case. Dropping the sample would lose a valid,
merely large, response.

This is flagged as a guess because it is the clever option and the alternative —
one boolean field on `Schema`, which `Merge` would OR — is arguably plainer.
**Revisit if:** The sentinel reads as too cute in review; converting to a struct
field is mechanical, and only touches `Merge`.

## 2026-09-04 — The store addresses buckets by absolute time, with no clock
**Chose:** A bucket's slot is derived arithmetically from its start time, and a
slot is stale exactly when its recorded start disagrees with the one being
written. Retention is measured against the newest observation the store has
accepted, not against wall-clock now.
**Over:** A head pointer advanced on a timer or on write, and a `Now func()`
dependency for expiry.
**Because:** Absolute addressing needs no rotation bookkeeping at all — there is
no "advance the ring" step to get wrong at a boundary, under concurrency, or
after an idle period long enough to lap the ring. Measuring retention against
the newest observation removes the clock entirely, which makes the store
deterministic: replaying a captured trace gives the same answer today as next
week, and the tests need no injected time.

The trade is that a store receiving no traffic never expires anything. That is
the correct behaviour here — with no new observations there is no new window to
compare against, so nothing would be gained by emptying the ring.
**Revisit if:** Memory needs reclaiming during idle periods, which would want a
sweep rather than a change to addressing.

## 2026-09-04 — Store memory is bounded by live buckets, not by endpoints alone
**Chose:** Two limits: `MaxEndpoints` (4096) for cardinality, and
`MaxLiveBuckets` (20000) for memory. Exceeding either evicts the coldest
endpoint.
**Over:** `MaxEndpoints` alone, which is what "bounded by endpoints × fields ×
buckets" most directly suggests.
**Because:** The product is not a useful bound. 4096 endpoints × 168 buckets is
688,000 schemas; at a few kilobytes each that is multiple gigabytes, so quoting
it as "the bound" would be technically true and operationally worthless. What
actually costs memory is the number of *non-empty* (endpoint, bucket) pairs, and
that number is far smaller in practice because most endpoints are idle in most
hours. Bounding it directly gives a figure that means something: 20,000 buckets
at roughly 8 KB of schema each is about 160 MB, and that ceiling holds no matter
how the traffic is distributed across endpoints and hours.

Eviction is oldest-write, taking any endpoint whose newest data has already
aged out first. An endpoint with no recent traffic has no recent window to
compare against, so it can produce no findings; it is the right thing to shed.
The scan is linear in endpoint count, paid only at the limits, which is cheaper
in complexity than a heap kept permanently correct.
**Revisit if:** 8 KB per schema is wrong for the real traffic. It is the
quantity to measure — `MaxFields` × `MaxEnumCardinality` makes the theoretical
per-schema worst case much larger than the typical one.

## 2026-09-04 — The store clones on first write to a bucket and merges only after
**Chose:** An empty bucket takes `sc.Clone()`; only a second write into the same
bucket calls `schema.Merge`. Likewise, a window covering one bucket returns a
clone rather than merging.
**Over:** Always merging, including into an empty bucket via a zero-value
schema.
**Because:** Cloning is needed anyway — the store must not alias the caller's
schema, and must not hand out aliases of its own buckets — so the special case
costs nothing. What it buys is that ring addressing, rotation, retention and
eviction are all exercisable independently of `schema.Merge`: 91.7% of the
store's statements are under test without invoking it at all, so a bug in the
merge cannot masquerade as a bug in the ring.
**Revisit if:** Never; this is strictly better than the alternative even once
`Merge` lands.

## 2026-09-04 — Windows merge buckets in a deterministic order
**Chose:** Sort the in-range buckets by start time before merging, rather than
walking slots in ring order.
**Over:** Merging in whatever order the slots come out, which is what
associativity and commutativity say is safe.
**Because:** It *is* safe, if `Merge` is correct — and depending on that for a
deterministic answer means depending on someone else's correctness for
reproducible output. A window query that returns different results on different
runs would be a miserable thing to debug, and the sort costs an insertion pass
over at most a few hundred nearly-ordered indices.
**Revisit if:** Never; the cost is negligible.

## 2026-09-04 — Analysis panics are contained, counted, and announced once
**Chose:** `analyzer.handle` and each endpoint's comparison run under a
`recover`. Contained panics increment counters visible in `/stats`, log once to
stderr, and add a warning line to the report.
**Over:** Letting them crash the process, which is the usual and usually correct
Go instinct.
**Because:** The consumer goroutine shares a process with the proxy. A panic
there kills the proxy too, which turns "the monitoring tool has a bug" into "the
service is down" — the one failure mode this whole design exists to prevent.
Phase 1 went to some trouble to keep analysis off the request path, and it would
be undone by a single unrecovered panic on the analysis path.

Containment is only defensible if the failure stays loud, so it is loud in three
places at three timescales: a counter for the rate, one log line for the first
occurrence (a silently degraded pipeline otherwise looks exactly like a quiet
one), and a warning on the report, because a reader who sees "no findings"
deserves to know it was computed from a fraction of the traffic.

It also happens to be what makes the tool runnable today: `store.Add` calls
`schema.Merge`, so a fault there reaches every sample after the first in a
bucket. A smoke run against a local upstream proxies 300 requests with zero
client-visible errors, contains 250 panics and says so on every surface.
**Revisit if:** Nothing. Once `Merge` lands the panic count should sit at zero,
and if it does not, that is a bug the counters will surface.

## 2026-09-04 — report is a client of serve, not a second analyser
**Chose:** `apidrift serve` exposes the latest report at `/findings`; `apidrift
report` fetches and renders it.
**Over:** `report` reading a shared on-disk store, or rebuilding schemas itself.
**Because:** The schemas live in the serving process's memory, and a `report`
command that rebuilt them would be reporting on different data than the process
making the decisions — two answers to the same question, diverging silently.
Fetching keeps exactly one source of truth. It also keeps the tool honest about
what it is: an online detector with a view, not a batch job.
**Revisit if:** Findings need to outlive the process. That wants persistence in
`serve`, with `report` still reading through it — not a second analyser.

## 2026-09-04 — Windows advance on a ticker, not on requests to /findings
**Chose:** `serve` evaluates every `-window`, caches the report, and `/findings`
returns the cache.
**Over:** Evaluating on demand when `/findings` is requested, which would be
simpler and always current.
**Because:** The persistence tracker counts calls as windows. Evaluating on
demand would let a reader qualify a finding early simply by refreshing the page,
and would make `PersistWindows` mean "consecutive HTTP requests" instead of
"consecutive windows" — turning a statistical guard into an artefact of how
often someone looks at it.
**Revisit if:** A forced re-evaluation is wanted for debugging; that should be a
separate endpoint that does not touch the tracker.

## 2026-09-04 — The admin surface binds to localhost by default
**Chose:** `-admin 127.0.0.1:9090`, while `-listen` defaults to `:8080`.
**Over:** Matching the proxy's default and binding both to all interfaces.
**Because:** `/stats` and `/findings` describe the shape of the upstream's
responses, including field names and enum values. That is a mild information
leak and there is no reason to expose it by default; the proxy has to be
reachable, and the admin surface does not.
**Revisit if:** A metrics scraper needs to reach it from another host, which is
a deliberate `-admin 0.0.0.0:9090` rather than a changed default.

## 2026-09-04 — The report warns about what it could not examine
**Chose:** `Report.Warnings`, populated when endpoints fail analysis or samples
are lost, and printed above the findings.
**Over:** Leaving that information in `/stats` only.
**Because:** "No findings" and "no findings, because nothing was examined" print
identically without it, and they are opposite pieces of news — one is
reassurance, the other is an outage in the monitoring. The reader of a report is
usually not the person watching the counters.
**Revisit if:** Warnings become noisy enough to be scrolled past, which would
mean promoting the underlying condition to a hard error instead.

## 2026-09-04 — GUESS: Finding.Endpoint is stamped by the caller
**Chose:** `Detect` leaves `Finding.Endpoint` zero; the analyser fills it in
after the call.
**Over:** Changing `Window` to carry the endpoint key.
**Because:** The given signature is `Detect(baseline, current Window, cfg
Config)`, and `Window` is `{From, To, Schema}` — so `Detect` is structurally
incapable of knowing which endpoint it was handed. Either the signature changes
or the caller stamps it, and the signature was specified. Documented in the
detect README and asserted in its tests, so nobody implementing against that
contract is surprised by a field they were never given the data to fill.
**Revisit if:** The signature is open to change, in which case putting the key
on `Window` is cleaner than stamping after the fact.

## 2026-09-04 — The YAML parser is used for both YAML and JSON specs
**Chose:** One loader, `yaml.Unmarshal` into `map[string]any`, for both
spellings of OpenAPI. `gopkg.in/yaml.v3` — the one dependency the project
budget allows.
**Over:** `encoding/json` for `.json` specs and yaml.v3 for `.yaml`.
**Because:** YAML 1.2 is a superset of JSON, so yaml.v3 parses both, and one
code path cannot disagree with itself about the same document. Two loaders
could: subtly different number handling or key coercion between them would
produce two different ground truths from one spec, which is a bug that would be
very hard to see and would silently corrupt every recall figure downstream.
There is a test that round-trips the YAML fixture through JSON and asserts the
two parses diff to nothing.
**Revisit if:** yaml.v3's maintenance status becomes a problem. The interface it
is used behind is one function.

## 2026-09-04 — The differ emits one change per affected path, not per edit
**Chose:** Removing an object emits `FieldRemoved` for the object *and* for
every path beneath it.
**Over:** One change for the conceptual edit, at the root of the removed subtree.
**Because:** apidrift observes `address`, `address.city` and `address.line1` as
three separate paths, and when the object goes it correctly reports all three.
If ground truth listed only `address`, the two correct findings about the
children would be scored as false positives — the evaluation would punish the
detector for being right, and the headline precision number would be wrong in
the tool's disfavour by a factor that grows with how nested the API is.
**Revisit if:** A "one alert per conceptual change" grouping is wanted for
*reporting*. That belongs in `internal/report`, not in the ground truth.

## 2026-09-04 — The differ stops descending at a type change
**Chose:** When a path's type differs between versions, emit `TypeChanged` and
do not compare the subtrees.
**Over:** Continuing to walk both sides.
**Because:** Once an object has become a string the two subtrees do not
correspond to each other, and walking them would report every field of the old
one as removed and every field of the new one as added — a dozen ground-truth
entries for what is one change. The detector will report one type change, and
would then be marked down for eleven misses it was right not to make.
**Revisit if:** Nothing; the alternative manufactures noise on both sides.

## 2026-09-04 — required → optional is labelled FieldRemoved
**Chose:** A field that stops being required is ground truth for
`FieldRemoved`, with the detail "became optional".
**Over:** A separate change kind, or no ground truth entry at all.
**Because:** apidrift cannot see a spec. It sees that a field's presence rate
fell from 100% to whatever the optional rate is, which is exactly the
observation it labels `FieldRemoved`. From outside, becoming optional and being
removed part of the time are the same event, and the tool is right to call them
the same thing. Emitting no ground truth would be worse: the detector's correct
finding would be counted as a false positive.
**Revisit if:** A distinct kind is added for partial removal, in which case both
sides change together.

## 2026-09-04 — Endpoints in only one spec version are excluded from the evaluation
**Chose:** `ComparableEndpoints` returns only endpoints both versions define;
everything else is out of both ground truth and the measurement.
**Over:** Counting a removed endpoint as a large set of `FieldRemoved` changes.
**Because:** An endpoint that appears or disappears has traffic in one window
and none in the other. `Detect` is given one empty window and correctly says
nothing — there is no comparison to make. Scoring those as missed detections
would mark the tool down for a job it correctly declined, and would make recall
depend on how much of the API was added or retired between versions rather than
on how good the detector is.
**Revisit if:** Endpoint appearance and disappearance become findings in their
own right, which they arguably should be — but that is a different detector,
watching the endpoint set rather than the schema.

## 2026-09-04 — The evaluation bypasses the templater
**Chose:** `Run` files responses under the endpoint key derived from the spec
path, rather than putting them through `endpoint.Templater`.
**Over:** Running the whole production pipeline, proxy and templater included.
**Because:** The evaluation measures the *detector*. Including the templater
would fold its warm-up into every detection-latency number, and a bad result
would be uninterpretable — no way to tell whether the detector is insensitive or
the templater was still deciding. The templater is tested on its own terms in
`internal/endpoint`, against the cases that actually stress it.
**Revisit if:** An end-to-end number is wanted. That is a second measurement to
report alongside this one, not a replacement: the two answer different questions.

## 2026-09-04 — The null run uses different seeds for its two windows
**Chose:** Four seeds: baseline, current, and two more for the null run's two
windows.
**Over:** Reusing one seed, which would be simpler.
**Because:** The null run's whole purpose is to ask "with nothing changed, how
often does the detector fire anyway?" — and the answer has to come from
*sampling* variation between two independent draws. Two windows generated from
the same seed would be byte-identical, every presence rate would match exactly,
and the measured false positive rate would be zero by construction. That number
would look excellent and mean nothing at all, which is the worst kind of
benchmark result.
**Revisit if:** Nothing; this is what makes the headline number real.

## 2026-09-04 — Detection latency is measured from constructed schemas, not generated traffic
**Chose:** `LatencySweep` builds windows directly from binomial draws, rather
than generating and merging response bodies.
**Over:** Running the full generate-extract-merge pipeline for each cell of the
sweep.
**Because:** The question is "how many responses does the detector need to see a
drop to rate p", which is a question about statistical power. Constructing the
counts directly measures exactly that, and nothing else — no dependence on how
the generator happens to lay out bodies, no confound from extraction. It is also
about a hundred times faster, which is what makes a grid of nine rates by nine
sample sizes by forty trials practical, and forty trials per cell is what stops
the answer being the luck of one seed.

A side benefit worth naming: the sweep needs only `detect.Detect`, not
`schema.Merge`, so the number it reports is a property of the detector alone and
cannot be contaminated by a fault elsewhere in the pipeline.
**Revisit if:** Extraction is suspected of distorting the counts, which would be
a bug in `Extract` and is better found by testing `Extract`.

## 2026-09-04 — A blocked evaluation prints no tables
**Chose:** `Result.Blocked` records why a run stopped; `WriteSummary` then
prints the reason and the ground truth, and suppresses the statistics.
**Over:** Printing the tables with zeros in them.
**Because:** A recall table full of zeros reads as "the detector found nothing".
What actually happened is that nothing ran. Those are opposite conclusions, and
the first is the one a reader will reach if the output looks like a normal
report. Today this fires on every run, because building a window needs
`schema.Merge`; the same machinery will matter later for any run that dies
partway through.
**Revisit if:** Nothing; the distinction between "measured zero" and "not
measured" is the sort of thing that quietly ruins a set of results.

## 2026-09-04 — The limitations section leads with the blind spot, unhedged
**Chose:** "If a field switches from cents to dollars, apidrift will not notice"
as the first limitation, stated flatly, with a list of the other
structure-preserving changes it is equally blind to and an explicit instruction
not to read silence as safety.
**Over:** Softening it into "shape-based detection has some limitations around
semantics", or burying it below the operational caveats.
**Because:** This is the failure mode most likely to hurt someone, precisely
because it is silent on every axis — no exception, no failing test, and now no
alert from the tool bought to catch exactly this class of problem. A hedged
sentence would let a reader conclude the gap is a rough edge rather than a
category the tool does not address at all. Stating that no threshold fixes it,
and that detecting it needs a different tool with different statistics, is the
only version that leaves the reader correctly calibrated.
**Revisit if:** Value-distribution monitoring is added, which would be a second
detector alongside this one rather than a change to it.

## 2026-09-04 — The README benchmarks against a bare reverse proxy, not against no proxy
**Chose:** Publish all four arms, and headline the apidrift-vs-bare-proxy delta.
**Over:** Headlining the direct-vs-apidrift number, which is ~137µs and looks far
more impressive to have "only" added.
**Because:** Direct-vs-proxied measures the cost of proxying at all, almost all
of it the second local HTTP round trip, and attributing that to apidrift would
be taking credit — or blame — for something it did not cause. The honest
question is what the analysis costs on top of a hop you are paying anyway, and
the answer is 0.5µs. Publishing all four arms lets a reader check that
themselves rather than trusting the framing.
**Revisit if:** Nothing; the four-arm table is the reproducible form.

## 2026-09-05 — Panic containment confirmed, and made loud enough to deserve it
**Chose:** Keep the `recover` on both analysis paths permanently, after raising
it for review. Strengthened the reporting: each *distinct* panic cause is logged
once, up to eight, rather than only the very first panic of the process.
**Over:** Reverting to a crash once `schema.Merge` lands, which was the open
question.

**The case against containment**, which is the real one and worth being able to
answer: a recovered panic is a bug that no longer announces itself. Go's default
is to crash precisely because a process in an unexpected state should stop
rather than continue producing output nobody can trust, and a monitoring tool
that quietly degrades is worse than one that visibly dies — at least the dead
one gets noticed.

**Why containment still wins here.** The consumer goroutine shares a process
with the proxy. Crashing does not stop apidrift, it stops *the traffic apidrift
sits in front of*: a bug in the analysis code becomes an outage in someone
else's service. That is a strictly worse failure than degraded monitoring, and
it is the exact failure the whole design is arranged to prevent — phase 1 spent
a benchmark proving analysis cannot add latency to a request, and one
unrecovered panic would undo that.

The asymmetry decides it: containment's worst case is losing observations, which
is a thing this design already accepts under load and already counts. Crashing's
worst case is an outage in a system apidrift was only supposed to watch.

**What the objection does buy** is the loudness requirement, and it exposed a
real hole. Reporting only the first panic of the process meant a second,
different bug arriving later would be a silent counter increment — a process
contentedly containing one failure for a week would swallow the arrival of the
next one. Now each distinct cause is announced once, capped at eight so a cause
that varies per sample cannot flood the log. Three timescales, three places: a
counter for the rate, a log line per distinct cause, a warning on the report
saying how many samples the figures are missing.
**Revisit if:** The panic counter is non-zero in steady state after all three
in steady state. That is a bug the counters exist to surface, and the answer is
to fix it, not to remove the net.

## 2026-09-05 — No language model goes anywhere near the detector
**Chose:** Keep `internal/detect` entirely deterministic. The model runs in a
separate layer, downstream, on things the statistics have already selected.
**Over:** Using a model to judge drift directly, which is the obvious way to
"add AI" and the one a reader might expect.
**Because:** The entire claim this tool makes is that its false positive rate is
a number you can measure and quote. That property depends on the detector being
deterministic: the same two windows must produce the same findings, and the null
experiment must be repeatable. A model in that path forfeits all of it — the
false positive rate stops being measurable, the evaluation harness stops meaning
anything, and the answer changes between runs.

The economics say the same thing from the other side. The detector tests every
path on every endpoint — thousands per window. A model on that path would be
roughly a thousand requests per window per endpoint at a latency and cost that
makes the tool unrunnable. Downstream, it runs on the handful of findings that
survived four gates.

So the division is: **statistics decide whether something changed; the model
decides what it means and what to do about it.** Each does the thing the other
is bad at.
**Revisit if:** Never for the detector. If a model ever appears there, the
evaluation harness and the false-positive number have to be redesigned first,
and there is no obvious way to do it.

## 2026-09-05 — The judgement layer targets the blind spot, not the strengths
**Chose:** Two model-backed judgements — did the values change *meaning*, and
what does a confirmed change *break* in the caller's code.
**Over:** Using a model to summarise findings, rank severity, or write nicer
alert text, all of which would have been easier.
**Because:** Summarising and ranking are things the deterministic code already
does adequately, and a model there adds cost and non-determinism for polish. The
two chosen jobs are things statistics genuinely cannot do:

- **Semantic drift** is the limitation the README states most bluntly: a field
  that switches from cents to dollars keeps its type and its presence rate, so
  nothing in a Schema moves. No threshold catches it. Reading the values and
  recognising that 2000 became 20 across the board is exactly what a language
  model is good at and a proportion test is structurally incapable of.
- **Impact** requires reading the caller's source and knowing which of forty
  grep hits is a real usage. That is a judgement about code, and there is no
  statistic for it.

**Revisit if:** A third job appears that statistics also cannot do. The test for
admitting one is that question, not whether a model could plausibly help.

## 2026-09-05 — A deterministic filter gates every model call
**Chose:** `valuesample.DetectShift` selects semantic candidates before anything
is sent: a median magnitude that moved by 2x or more, a changed dominant string
format, or a changed written number form. Everything else is discarded for free.
**Over:** Sending every sampled path to the model each window and letting it
decide.
**Because:** Cost and signal, and they point the same way. A mid-sized API has
thousands of paths; asking about all of them every window is a fortune spent
mostly on the answer "no, that looks normal", and the few real answers would be
buried in that pile. The filter costs microseconds and, in the smoke run,
reduced five sampled paths to one — it ignored `id`, `object`, `currency` and
`status`, and flagged `amount`.

It also makes the model's job easier rather than just cheaper. A candidate
arrives with a stated hypothesis — "the median magnitude moved by 1/130x" — so
the model is adjudicating a specific claim rather than hunting for one, which is
the difference between a judgement and a fishing expedition.
**Revisit if:** Real traffic produces candidates the filter misses. The 2x
threshold is the knob; lowering it costs money linearly.

## 2026-09-05 — The prompts are written against the model's pull to agree
**Chose:** Both system prompts state the base rate explicitly, list the mundane
explanations and require them to be ruled out, say that "no" is expected and
unpenalised, and demand a named mechanism before high confidence. The semantic
prompt additionally requires the model to say what would have changed its answer.
**Over:** Asking the question plainly and trusting the answer.
**Because:** A model handed two visibly different samples and an explicit
hypothesis is under strong pull to agree — concurring reads as helpful, and
there is always a story that fits. But a semantic detector with a high false
positive rate is precisely what this project exists to avoid; the statistical
side spends four gates suppressing noise, and a chatty layer bolted on the end
would undo all of it. The impact prompt has the mirror-image problem: handed
forty grep hits, the path of least resistance is to hand them back with a
plausible sentence each, which returns the reader's own noise with an implied
endorsement.

There is a test asserting these instructions are still present, because they are
behaviour, not decoration. Deleting them would not fail anything else.
**Revisit if:** Measured false positives on the semantic layer are high anyway.
The next lever is requiring a round-number ratio before `changed: true`, which
trades recall for precision.

## 2026-09-05 — Value samples live outside FieldStats
**Chose:** A separate `internal/valuesample` store with its own reservoirs, its
own bounds, and its own epoch rotation.
**Over:** Adding a value sample to `schema.FieldStats`, where the rest of a
field's observations already live.
**Because:** `FieldStats` is the type `Merge` is being written against, and its
contract is that every counter is additive and the merge is associative and
commutative. A reservoir sample is none of those things — merging two reservoirs
correctly needs their populations, not just their contents. Putting one in that
struct would have quietly broken the property the whole detector rests on, to
save a package.

The separation bought something unplanned too: because the sampler has no
dependency on the schema pipeline, semantic candidate selection works today,
independently of `schema.Merge`. A smoke run in which the merge path was failing
still correctly flagged a cents-to-dollars change.
**Revisit if:** Nothing. These are different kinds of data with different merge
semantics and they belong apart.

## 2026-09-05 — Values are sampled before the store write, not after
**Chose:** `analyzer.file` samples values immediately after the endpoint
resolves, ahead of `schema.Extract` and `store.Add`.
**Over:** Sampling after a successful store write, which is where it was first
written and reads more naturally as "record everything about a stored sample".
**Because:** It made the semantic layer fail whenever the structural pipeline
failed, for no reason — they share no data. The bug was live and invisible:
a fault in `store.Add` is contained by design, and value sampling would then
silently never run. The first end-to-end demo produced zero candidates
because of it.
**Revisit if:** Nothing; coupling two independent paths through a shared failure
was simply wrong.

## 2026-09-05 — assess is a separate command with a dry run, not part of report
**Chose:** `apidrift assess`, defaulting to nothing being sent until asked, with
`-dry-run` showing exactly what would be sent and `-max` capping items per run.
**Over:** Folding model judgements into `apidrift report`, so findings arrive
pre-explained.
**Because:** `report` is free, instant and deterministic; `assess` costs money,
takes seconds per item, and gives different answers on different runs. Silently
merging the two would mean a reader could not tell which parts of a report they
could rely on, and a habitual `report` in a monitoring loop would quietly bill
per finding. Keeping them separate keeps the free thing free and makes spending
an explicit act. `-dry-run` exists so the whole flow — candidate selection, code
search, prompt construction — is inspectable before anyone pays for it.
**Revisit if:** Assessments are wanted inline. The right shape then is a flag on
`report` that is off by default, not a merged default.

## 2026-09-05 — GUESS: refusals are surfaced as errors, not routed to a fallback model
**Chose:** Check `stop_reason` and return `ErrRefused`, rather than enabling
server-side fallbacks.
**Over:** The default guidance for Opus 5 code, which is to pass `fallbacks` and
let a substitute model answer a refused request.
**Because:** This layer classifies numeric units and reads call sites in the
user's own repository. A policy decline is not a failure mode it can plausibly
hit, and if one ever happened it would mean the prompt or the data was not what
was expected — which is worth surfacing rather than silently re-running
somewhere else. Enabling fallbacks would also move the call to the beta
endpoint, adding beta parameters to a non-critical enrichment path.
**Revisit if:** A refusal is ever actually observed, which would itself be
information worth having.

## 2026-09-05 — GUESS: the live API path is unverified
**Chose:** Ship `assess.Client` with its types verified against the installed
SDK (via `go doc`, so `OutputConfigParam`, `JSONOutputFormatParam` and the
adaptive thinking union are correct and it compiles), and every surrounding
behaviour tested through `assess.Fake`.
**Over:** Claiming it works end to end.
**Because:** There are no Anthropic credentials on this machine, so no request
has ever been made. The structured-output schemas, the decode path and the
error handling are unexercised against a real response. Everything around them
— candidate selection, code search, prompt rendering, the CLI, the reporting —
is tested and was demonstrated end to end with the Fake.
**Revisit if:** Credentials become available. The first real call is the test:
`apidrift assess -impact=false` against a proxy with a candidate, and the thing
most likely to be wrong is the structured-output schema being rejected at
request time.

## 2026-09-06 — Type shares are computed among non-null observations
**Chose:** `TypeChanged` compares each kind's share of `Present - ExplicitNull`,
excluding `KindNull` from the candidate kinds entirely.
**Over:** Sharing over `Present`, which is the obvious denominator.
**Because:** Nullability has its own change kind, and sharing over `Present`
makes every introduced null *also* a type change — the string share falls from
100% to 60% purely because 40% of the values became null. One upstream event
would be reported twice, and the less useful label would usually sort first. The
non-null denominator makes the two kinds independent: a field that became
nullable shows a null-rate change and no type change, and a field whose type
flipped shows a type change and no null-rate change.
**Revisit if:** A change that is genuinely both needs to report as both.

## 2026-09-06 — MinEffectSize is used as given; the others are defaulted
**Chose:** `Detect` fills `MinSamples` and `FDRAlpha` from the defaults when
zero, but takes `MinEffectSize` exactly as passed.
**Over:** Defaulting all of them uniformly.
**Because:** Zero is meaningless for a sample floor or an alpha, so a zero there
is plainly "unset". Zero is *meaningful* for an effect size — it means "do not
gate on effect at all" — so defaulting it would silently override a deliberate
choice, and there would be no way to express it.
**Revisit if:** The asymmetry surprises someone. It is documented at the
assignment, which is where a reader will be when the question occurs to them.

## 2026-09-06 — A field becoming *more* present is not reported
**Chose:** The presence hypothesis is only generated when the rate dropped.
**Over:** Reporting movement in either direction.
**Because:** A field that became more reliably present has not broken anybody.
Reporting it would make every upstream bug fix look like drift, and the fastest
way to get an alerting tool muted is for it to fire on good news.
**Revisit if:** A rising presence rate turns out to matter — a field that was
rare becoming common can signal a rollout worth knowing about. That is closer to
`FieldAdded` than to `FieldRemoved` and would want its own kind.

## 2026-09-06 — The evaluation found a bug in the evaluation
**Chose:** Give `GenConfig.NullRate` a non-zero default (0.3).
**Over:** Leaving it at zero, which is what it was.
**Because:** The first end-to-end run reported recall 0.000 for
`nullability_introduced`, and the detector was innocent. The generator's null
rate defaulted to zero, so a field the spec declared nullable was never actually
null in the generated traffic — ground truth claimed a change the traffic did
not express, and the detector correctly said nothing about it.

Worth recording because it is the failure mode an evaluation harness exists to
have: it is much easier to write a harness that quietly measures the wrong thing
than one that measures nothing, and a recall of zero on one kind is exactly what
that looks like from outside. The fix is one line; noticing was the work.
**Revisit if:** Another change kind reports suspiciously low recall. Check that
the generator expresses it before touching the detector.

## 2026-09-06 — MEASURED: detection latency is floored by MinSamples, not by power
**Chose:** Report the flat latency curve as the finding rather than re-tuning
the sweep until it looked like a power curve.
**Because:** Every presence rate whose effect clears `MinEffectSize` is detected
at exactly 200 responses — the first sample size that clears `MinSamples` — and
nothing is detected below 200 at any effect size, including a field that
vanished entirely. The statistics are not the binding constraint anywhere in the
detectable range; the two configured gates are.

That is worth stating plainly because it inverts the obvious tuning instinct. To
detect faster, lower `MinSamples`. Touching the significance machinery will do
nothing, because by the time a test is run at all the answer is already
overwhelming.

Two further results from the same sweep. The cliff at the effect gate is sharp:
a drop to 0.90 (effect exactly 0.10) is detected, a drop to 0.91 never is, at
any sample size up to 4,000. And — the result worth keeping — at a true effect
just below the gate, **more data makes the tool quieter**: detection probability
falls from 0.43 at n=200 to 0.00 at n=4,000, because with little data noise
sometimes pushes the observed effect over the gate and with more data it
converges on the truth. A significance test alone behaves the opposite way. That
is the effect-size gate earning its place.
**Revisit if:** `MinSamples` changes; the whole curve moves with it.

## 2026-09-06 — MEASURED: the headline numbers
**Chose:** Publish recall 1.000, precision 1.000, and zero false positives under
the null — with the caveat attached in the same breath.
**Because:** The numbers are real and reproducible (`apidrift eval` with the
committed fixtures), and they demonstrate the machinery is correct and
calibrated: 11 ground-truth changes across all five kinds, 11 found, none
invented, and nothing at all reported when v1 traffic is compared against itself
over 18 paths.

They are also easy to over-read, so the README says so in place rather than in a
footnote: the spec is small, the changes are large and clean, and the synthetic
traffic is independent by construction — which is precisely the assumption the
significance test wants and real traffic only approximates. A perfect score on a
benchmark you built yourself is evidence the implementation matches the
specification, not evidence about Stripe.
**Revisit if:** Run against real spec history, which is the honest next test and
where the interesting failures will be.

## 2026-09-07 — CORRECTION: the proxy overhead is below the noise floor
**Chose:** Report "no detectable p50 or p99 regression, overhead bounded by
roughly ±2.4µs" instead of the "+0.5µs p50 drained, +1.5µs saturated" this file
and the README previously quoted.
**Over:** Leaving the earlier figures, which were flattering and had already
been published.
**Because:** Re-running the same benchmark (`-benchtime 5000x -count 5`) after
the judgement layer landed produced p50 deltas of **−0.9µs drained and −0.4µs
saturated** — apidrift measuring *faster* than the bare proxy it wraps. That is
impossible, and the impossibility is the useful part: it proves the quantity
being measured is smaller than the measurement error. The bare-proxy arm varies
by 2.4µs across its own five runs, more than any delta in either direction.

The earlier numbers were not wrong so much as over-precise. They came from one
run of a quantity whose run-to-run variance exceeds it, and quoting them as
measurements implied a precision the method does not have. The README already
applied exactly this reasoning to p99 and should have applied it to p50 at the
same time.

Memory is unaffected by the correction and stays quoted precisely — +2.2 KB and
+15 allocations per request — because allocation counts are deterministic rather
than timed.
**Revisit if:** A benchmark that can actually resolve single microseconds is
wanted. That needs a different method — many more iterations, pinned CPUs, and a
quiet machine — and the honest answer is that it would be measuring something
nobody deploying a network proxy would notice.

## 2026-09-07 — Full verification completed
**Chose:** Record what has actually been run, now that the machine had enough
disk to run it.
**Because:** Several earlier sessions could only verify packages individually,
or not at all, and the distinction between "passed" and "could not be built"
matters when reading back a claim.

Verified, all green:

- `make lint` — `gofmt -l` clean, `go vet ./...` clean
- `make build` — binary produced
- `make test` — all 12 packages under `-race`
- `make test-impl` — 12 packages
- `make test-stub` — the 66 contract tests for `schema.Merge`, `detect.Detect`
  and `eval.Score`, counted exactly and all passing
- `apidrift eval` — recall 1.000, precision 1.000, 0 findings under the null,
  reproducing the published numbers exactly
- `make bench` — see the correction above

Still unverified, and stated in the README: the live Anthropic API path in
`internal/assess`. There are no credentials on this machine, so no request has
ever been made. Its types are checked against the installed SDK and every
surrounding behaviour is tested through `assess.Fake`.
**Revisit if:** Credentials become available; the first real call is the test.

## 2026-09-07 — The Go floor moved to 1.24, and CI moved with it
**Chose:** Raise both `go.mod` and the CI pin to 1.24.
**Over:** Holding the 1.22 floor recorded in the phase-0 entry above.
**Because:** Adding the Anthropic SDK raised the module's `go` directive to 1.24
as a transitive requirement. That silently broke the invariant the earlier entry
set out to protect — CI was still installing 1.22, which cannot build a module
declaring 1.24 at all, so the first push would have failed on every job. The
floor is a real constraint, not a preference, so the honest move is to state the
new one rather than pretend the old one still holds.

Worth noting as the cost of that dependency: it was justified for the judgement
layer, and this is the part of the bill that arrived later and quietly. A
project that must support 1.22 would have to drop the SDK and call the API over
plain HTTP.
**Revisit if:** The floor needs to come back down, in which case the SDK is the
thing to remove.

## 2026-09-07 — MEASURED: new enum values are 91% of the noise, so they are off by default
**Chose:** Report `FieldRemoved`, `TypeChanged` and `NullabilityIntroduced` by
default. `EnumValueAdded` needs `-enums`, and `FieldAdded` needs `-all`.
**Over:** Treating every non-additive change as breaking, which is what the
first version did and what the synthetic fixture made look reasonable.
**Because:** Running it against three months of real Stripe settled it. Between
`2026-05-27.dahlia` and `2026-08-26.dahlia`:

| kind | count |
|---|---:|
| field_added | 2110 |
| enum_value_added | 552 |
| field_removed | 57 |
| type_changed | 0 |
| nullability_introduced | 0 |

Enum additions were 91% of everything the old default reported. Providers add
payment methods, bank codes and card networks continuously, and almost none of
that breaks a caller. At 552 to 57 the real signal is buried, and a report
nobody can read is a report nobody keeps.

The 57 removals are genuine and are exactly what this tool is for: `iin`,
`issuer`, `description` and `three_d_secure.cryptogram` all disappeared from
card objects in that window, and any code reading them silently gets nothing.

Two things the numbers say beyond the filter. Stripe never changed a type or
introduced a null in three months, which suggests type stability is where
providers are disciplined and field removal is where they are not, so removal is
the case worth optimising for. And the synthetic fixture had one enum change out
of eleven, a ratio that made the old default look sensible; only real data
showed it was 91%.
**Revisit if:** A consumer genuinely needs enum coverage, which is a real case
for exhaustive switch statements. That is what `-enums` is for, and it should
probably become per-endpoint rather than global if anyone uses it.

## 2026-09-07 — VALIDATED: the loader handles a real spec
**Chose:** Record that it does, since every number before this came from a
90-line fixture.
**Because:** Stripe's published spec is 7.7 MB, 419 paths and 1454 component
schemas, with `$ref` cycles and `allOf` composition throughout. It parses to 593
response schemas in 0.37 seconds, and a full diff of two versions three months
apart takes 0.84 seconds. The recursion cutting and the `allOf` merge both hold
up, which were the two places a real spec was most likely to break them.
**Revisit if:** A provider ships something that does break it. The parser is
deliberately partial, and the honest expectation is that some spec somewhere
uses a construct it ignores.
