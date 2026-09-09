package store

import (
	"sync"
	"time"

	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/schema"
)

// Defaults for Config. One hour of resolution over one week of history is the
// shape a drift report wants: fine enough to say "this started on Tuesday
// afternoon", coarse enough that a week of it is a few hundred buckets.
const (
	DefaultBucketDuration = time.Hour
	DefaultBuckets        = 24 * 7
	DefaultMaxEndpoints   = 4096
	DefaultMaxLiveBuckets = 20000
)

// Config tunes the store. Zero fields take their defaults.
type Config struct {
	// BucketDuration is the width of one bucket.
	BucketDuration time.Duration
	// Buckets is the ring depth. Retention is Buckets * BucketDuration.
	Buckets int
	// MaxEndpoints caps how many endpoints are tracked at once. Reaching it
	// evicts the coldest endpoint.
	MaxEndpoints int
	// MaxLiveBuckets caps how many non-empty buckets exist across all
	// endpoints. This is the real memory bound; the arithmetic is worked
	// through in DECISIONS.md.
	MaxLiveBuckets int
}

func (c Config) withDefaults() Config {
	if c.BucketDuration <= 0 {
		c.BucketDuration = DefaultBucketDuration
	}
	if c.Buckets <= 0 {
		c.Buckets = DefaultBuckets
	}
	if c.MaxEndpoints <= 0 {
		c.MaxEndpoints = DefaultMaxEndpoints
	}
	if c.MaxLiveBuckets <= 0 {
		c.MaxLiveBuckets = DefaultMaxLiveBuckets
	}
	return c
}

// Stats describes what the store holds and what it has thrown away.
type Stats struct {
	// Endpoints is how many endpoints are currently tracked.
	Endpoints int `json:"endpoints"`
	// LiveBuckets is how many non-empty buckets exist across all endpoints.
	// This is the quantity MaxLiveBuckets bounds.
	LiveBuckets int `json:"live_buckets"`
	// Added is the lifetime count of accepted schemas.
	Added uint64 `json:"added"`
	// StaleDrops counts schemas rejected for being older than retention.
	StaleDrops uint64 `json:"stale_drops"`
	// Evictions counts endpoints dropped to stay within the limits.
	Evictions uint64 `json:"evictions"`
	// Rotations counts buckets recycled because the ring came round to them.
	Rotations uint64 `json:"rotations"`
	// Newest is the most recent observation time the store has accepted.
	Newest time.Time `json:"newest"`
}

// ring is one endpoint's history: a fixed array of buckets addressed by
// absolute time, so no bookkeeping is needed to advance it. The bucket for a
// time lives at a slot derived from that time, and starts records which bucket
// each slot currently holds. A slot whose start disagrees with the time being
// written has come round again and is recycled.
type ring struct {
	// buckets[i] is the schema for the bucket beginning at starts[i], or nil.
	buckets []*schema.Schema
	// starts[i] is the start of the bucket in slot i. The zero time means the
	// slot is empty.
	starts []time.Time
	// lastWrite is the newest observation time written here, and is what
	// "coldest" is measured by when evicting.
	lastWrite time.Time
	// live is how many of buckets are non-nil.
	live int
}

// Store keeps a ring of schema buckets per endpoint and answers window queries
// over them.
//
// It is safe for concurrent use.
type Store struct {
	cfg Config

	mu        sync.Mutex
	endpoints map[endpoint.Key]*ring
	live      int
	newest    time.Time
	stats     Stats
}

// New returns a Store using cfg, with zero fields defaulted.
func New(cfg Config) *Store {
	return &Store{
		cfg:       cfg.withDefaults(),
		endpoints: make(map[endpoint.Key]*ring),
	}
}

// Retention is the span of history the store keeps.
func (s *Store) Retention() time.Duration {
	return time.Duration(s.cfg.Buckets) * s.cfg.BucketDuration
}

// Add records sc against k in the bucket containing at, reporting whether it
// was accepted. It is rejected only when at is older than retention, which
// means there is no bucket left for it to belong to.
//
// sc is not retained: the store keeps a copy, so the caller may reuse it.
func (s *Store) Add(k endpoint.Key, at time.Time, sc *schema.Schema) bool {
	if sc == nil {
		return false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Retention is measured against the newest observation the store has, not
	// against wall-clock now. That keeps the store deterministic -- replaying
	// a captured trace gives the same answer today as next week -- and removes
	// a clock dependency from every test.
	if !s.newest.IsZero() && at.Before(s.newest.Add(-s.Retention())) {
		s.stats.StaleDrops++
		return false
	}
	if at.After(s.newest) {
		s.newest = at
	}

	r := s.endpoints[k]
	if r == nil {
		s.evictForRoomLocked()
		r = &ring{
			buckets: make([]*schema.Schema, s.cfg.Buckets),
			starts:  make([]time.Time, s.cfg.Buckets),
		}
		s.endpoints[k] = r
	}

	start := s.bucketStart(at)
	slot := s.slotFor(start)

	if r.buckets[slot] != nil && !r.starts[slot].Equal(start) {
		// The ring has come round to this slot; the schema in it belongs to a
		// bucket that has aged out.
		r.buckets[slot] = nil
		r.live--
		s.live--
		s.stats.Rotations++
	}

	if r.buckets[slot] == nil {
		// Cloning rather than merging here is not just an optimisation: it is
		// what lets the ring's indexing, rotation and eviction be tested while
		// schema.Merge is still a stub.
		r.buckets[slot] = sc.Clone()
		r.starts[slot] = start
		r.live++
		s.live++
	} else {
		r.buckets[slot] = schema.Merge(r.buckets[slot], sc)
	}

	if at.After(r.lastWrite) {
		r.lastWrite = at
	}
	s.stats.Added++
	s.evictForBudgetLocked()
	return true
}

// Window returns the merged schema for k covering [from, to), and whether any
// data was found. A bucket is included when its start falls in the range.
//
// The result is freshly built and owned by the caller.
func (s *Store) Window(k endpoint.Key, from, to time.Time) (*schema.Schema, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	r := s.endpoints[k]
	if r == nil {
		return nil, false
	}

	// Collect in slot order, then sort by bucket start. Merge is required to be
	// associative and commutative, so order should not matter -- but a window
	// query that returns different answers on different runs would be a
	// miserable thing to debug, and depending on that requirement to get a
	// deterministic answer is depending on someone else's correctness.
	idx := make([]int, 0, len(r.buckets))
	for i, sc := range r.buckets {
		if sc == nil {
			continue
		}
		st := r.starts[i]
		if st.Before(from) || !st.Before(to) {
			continue
		}
		idx = append(idx, i)
	}
	if len(idx) == 0 {
		return nil, false
	}
	sortByStart(idx, r.starts)

	if len(idx) == 1 {
		// One bucket needs no merging, which keeps single-bucket windows
		// working while schema.Merge is a stub.
		return r.buckets[idx[0]].Clone(), true
	}

	out := r.buckets[idx[0]].Clone()
	for _, i := range idx[1:] {
		out = schema.Merge(out, r.buckets[i])
	}
	return out, true
}

// Keys returns every tracked endpoint, in no particular order.
func (s *Store) Keys() []endpoint.Key {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]endpoint.Key, 0, len(s.endpoints))
	for k := range s.endpoints {
		out = append(out, k)
	}
	return out
}

// Stats returns a snapshot of the store's state.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.stats
	st.Endpoints = len(s.endpoints)
	st.LiveBuckets = s.live
	st.Newest = s.newest
	return st
}

// bucketStart returns the start of the bucket containing t.
func (s *Store) bucketStart(t time.Time) time.Time {
	return t.Truncate(s.cfg.BucketDuration).UTC()
}

// slotFor maps a bucket start to its slot in the ring. Absolute-time addressing
// means slots need no rotation bookkeeping: the same instant always lands in
// the same slot, and a slot is stale exactly when its recorded start disagrees.
func (s *Store) slotFor(start time.Time) int {
	n := start.UnixNano() / int64(s.cfg.BucketDuration)
	slot := int(n % int64(s.cfg.Buckets))
	if slot < 0 {
		slot += s.cfg.Buckets // times before the epoch
	}
	return slot
}

// evictForRoomLocked makes room for one more endpoint.
func (s *Store) evictForRoomLocked() {
	for len(s.endpoints) >= s.cfg.MaxEndpoints {
		if !s.evictColdestLocked() {
			return
		}
	}
}

// evictForBudgetLocked drops endpoints until the live-bucket budget is met.
func (s *Store) evictForBudgetLocked() {
	for s.live > s.cfg.MaxLiveBuckets {
		if !s.evictColdestLocked() {
			return
		}
	}
}

// evictColdestLocked removes the endpoint with the oldest last write,
// preferring any whose data has aged out entirely. It reports whether anything
// was removed.
//
// Endpoints go cold for good reasons -- a route is retired, a client stops
// calling it -- and a store that never forgets them would grow without bound
// across a long-lived process. Oldest-write is the right thing to shed: an
// endpoint with no recent traffic has no recent window to compare against, so
// it can produce no findings anyway.
//
// The scan is linear in the number of endpoints. Eviction happens only at the
// limits, so the cost is paid rarely, and a heap kept permanently correct would
// be more machinery than the saving is worth.
func (s *Store) evictColdestLocked() bool {
	var (
		victim  endpoint.Key
		oldest  time.Time
		found   bool
		horizon = s.newest.Add(-s.Retention())
	)
	for k, r := range s.endpoints {
		// An endpoint whose newest write is past the horizon holds nothing a
		// window query could return. Take it first, whatever else is here.
		if !s.newest.IsZero() && r.lastWrite.Before(horizon) {
			victim, found = k, true
			break
		}
		if !found || r.lastWrite.Before(oldest) {
			victim, oldest, found = k, r.lastWrite, true
		}
	}
	if !found {
		return false
	}

	s.live -= s.endpoints[victim].live
	delete(s.endpoints, victim)
	s.stats.Evictions++
	return true
}

// sortByStart orders slot indices by their bucket start. Insertion sort: the
// slice is at most Buckets long and usually nearly ordered already, since slots
// advance with time.
func sortByStart(idx []int, starts []time.Time) {
	for i := 1; i < len(idx); i++ {
		for j := i; j > 0 && starts[idx[j]].Before(starts[idx[j-1]]); j-- {
			idx[j], idx[j-1] = idx[j-1], idx[j]
		}
	}
}
