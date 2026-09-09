package store_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/schema"
	"github.com/avidnerd/apidrift/internal/store"
)

// Schemas here are hand-constructed rather than extracted, so the store's own
// logic -- bucket addressing, rotation, window selection, eviction -- is under
// test and nothing else is.

var base = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func key(template string) endpoint.Key {
	return endpoint.Key{Method: "GET", Template: template, StatusClass: 2}
}

// mkSchema builds a one-field schema whose Present count labels it, so tests
// can tell which bucket a window came from.
func mkSchema(label uint64) *schema.Schema {
	return &schema.Schema{
		Samples: label,
		Fields: map[string]*schema.FieldStats{
			"status": {
				Seen:         label,
				Present:      label,
				TypeCounts:   map[schema.Kind]uint64{schema.KindString: label},
				StringValues: map[string]uint64{"open": label},
			},
		},
	}
}

func hours(n int) time.Duration { return time.Duration(n) * time.Hour }

func TestAddAndWindowSingleBucket(t *testing.T) {
	s := store.New(store.Config{})
	k := key("/users/{id}")

	if !s.Add(k, base.Add(10*time.Minute), mkSchema(7)) {
		t.Fatal("Add() = false, want true")
	}

	tests := []struct {
		name   string
		from   time.Time
		to     time.Time
		wantOK bool
	}{
		{"range containing the bucket", base, base.Add(hours(1)), true},
		{"range starting exactly at the bucket start", base, base.Add(time.Minute), true},
		{"range ending exactly at the bucket start", base.Add(-hours(1)), base, false},
		{"range entirely before", base.Add(-hours(5)), base.Add(-hours(1)), false},
		{"range entirely after", base.Add(hours(1)), base.Add(hours(5)), false},
		{"wide range", base.Add(-hours(24)), base.Add(hours(24)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := s.Window(k, tt.from, tt.to)
			if ok != tt.wantOK {
				t.Fatalf("Window() ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if got.Samples != 7 {
				t.Errorf("Samples = %d, want 7", got.Samples)
			}
		})
	}
}

func TestWindowUnknownEndpoint(t *testing.T) {
	s := store.New(store.Config{})
	s.Add(key("/users/{id}"), base, mkSchema(1))

	if _, ok := s.Window(key("/orders/{id}"), base, base.Add(hours(1))); ok {
		t.Error("Window() for an unobserved endpoint = ok, want not ok")
	}
}

// TestBucketBoundaries checks that observations are filed by wall-clock bucket,
// not by arrival order.
func TestBucketBoundaries(t *testing.T) {
	s := store.New(store.Config{})
	k := key("/users/{id}")

	// Two observations an hour and a bit apart: different buckets.
	s.Add(k, base.Add(59*time.Minute), mkSchema(1))
	s.Add(k, base.Add(61*time.Minute), mkSchema(2))

	first, ok := s.Window(k, base, base.Add(hours(1)))
	if !ok {
		t.Fatal("first hour has no window")
	}
	if first.Samples != 1 {
		t.Errorf("first hour Samples = %d, want 1", first.Samples)
	}

	second, ok := s.Window(k, base.Add(hours(1)), base.Add(hours(2)))
	if !ok {
		t.Fatal("second hour has no window")
	}
	if second.Samples != 2 {
		t.Errorf("second hour Samples = %d, want 2", second.Samples)
	}

	if st := s.Stats(); st.LiveBuckets != 2 {
		t.Errorf("LiveBuckets = %d, want 2", st.LiveBuckets)
	}
}

// TestRingRotation is the point of a ring buffer: coming round to a slot
// recycles it rather than accumulating.
func TestRingRotation(t *testing.T) {
	const buckets = 4
	s := store.New(store.Config{Buckets: buckets})
	k := key("/users/{id}")

	// One observation per hour for three times the ring depth.
	for i := 0; i < buckets*3; i++ {
		if !s.Add(k, base.Add(hours(i)), mkSchema(uint64(i+1))) {
			t.Fatalf("Add() at hour %d = false, want true", i)
		}
	}

	st := s.Stats()
	if st.LiveBuckets != buckets {
		t.Errorf("LiveBuckets = %d, want %d -- the ring must not grow", st.LiveBuckets, buckets)
	}
	if st.Rotations == 0 {
		t.Error("Rotations = 0, want the ring to have recycled slots")
	}

	// The last `buckets` hours survive; anything older is gone.
	for i := buckets * 2; i < buckets*3; i++ {
		from := base.Add(hours(i))
		got, ok := s.Window(k, from, from.Add(hours(1)))
		if !ok {
			t.Errorf("hour %d has no window, want retained", i)
			continue
		}
		if want := uint64(i + 1); got.Samples != want {
			t.Errorf("hour %d Samples = %d, want %d", i, got.Samples, want)
		}
	}
	for i := 0; i < buckets*2; i++ {
		from := base.Add(hours(i))
		if _, ok := s.Window(k, from, from.Add(hours(1))); ok {
			t.Errorf("hour %d still has a window, want it aged out", i)
		}
	}
}

func TestStaleObservationsRejected(t *testing.T) {
	const buckets = 4

	// The retention horizon is a property of the store, not of an endpoint, so
	// the seed write and the write under test use different endpoints. That
	// keeps every case landing in an empty bucket, which is what lets this run
	// while schema.Merge is a stub.
	seed, k := key("/seed"), key("/users/{id}")

	tests := []struct {
		name       string
		at         time.Time
		wantAccept bool
	}{
		{"same bucket", base.Add(hours(10)), true},
		{"one bucket back", base.Add(hours(9)), true},
		{"at the retention edge", base.Add(hours(10) - hours(buckets)), true},
		{"past the retention edge", base.Add(hours(10) - hours(buckets) - time.Nanosecond), false},
		{"far in the past", base, false},
		{"in the future", base.Add(hours(20)), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A fresh store per case, so accepted writes do not move the
			// horizon for later ones.
			s := store.New(store.Config{Buckets: buckets})
			s.Add(seed, base.Add(hours(10)), mkSchema(1))

			if got := s.Add(k, tt.at, mkSchema(2)); got != tt.wantAccept {
				t.Errorf("Add(at=%v) = %v, want %v", tt.at.Sub(base), got, tt.wantAccept)
			}
			if !tt.wantAccept {
				if st := s.Stats(); st.StaleDrops != 1 {
					t.Errorf("StaleDrops = %d, want 1", st.StaleDrops)
				}
			}
		})
	}
}

func TestAddRejectsNilSchema(t *testing.T) {
	s := store.New(store.Config{})
	if s.Add(key("/x"), base, nil) {
		t.Error("Add(nil) = true, want false")
	}
	if st := s.Stats(); st.Endpoints != 0 {
		t.Errorf("Endpoints = %d after a nil add, want 0", st.Endpoints)
	}
}

// TestStoreCopiesSchemas guards that the store neither retains nor exposes the
// caller's schema: mutating either side must not affect the other.
func TestStoreCopiesSchemas(t *testing.T) {
	s := store.New(store.Config{})
	k := key("/users/{id}")

	in := mkSchema(5)
	s.Add(k, base, in)

	// Mutating the input after adding must not change what was stored.
	in.Fields["status"].Present = 999
	in.Samples = 999

	got, ok := s.Window(k, base, base.Add(hours(1)))
	if !ok {
		t.Fatal("no window")
	}
	if got.Samples != 5 || got.Fields["status"].Present != 5 {
		t.Errorf("stored schema followed the caller's mutation: Samples=%d Present=%d", got.Samples, got.Fields["status"].Present)
	}

	// And mutating the returned window must not change what is stored.
	got.Fields["status"].Present = 111
	again, _ := s.Window(k, base, base.Add(hours(1)))
	if again.Fields["status"].Present != 5 {
		t.Errorf("returned window aliases the store: Present = %d, want 5", again.Fields["status"].Present)
	}
}

func TestKeys(t *testing.T) {
	s := store.New(store.Config{})
	want := []endpoint.Key{
		{Method: "GET", Template: "/users/{id}", StatusClass: 2},
		{Method: "GET", Template: "/users/{id}", StatusClass: 4},
		{Method: "POST", Template: "/users", StatusClass: 2},
	}
	for _, k := range want {
		s.Add(k, base, mkSchema(1))
	}

	got := s.Keys()
	if len(got) != len(want) {
		t.Fatalf("Keys() returned %d keys, want %d", len(got), len(want))
	}
	seen := make(map[endpoint.Key]bool, len(got))
	for _, k := range got {
		seen[k] = true
	}
	for _, k := range want {
		if !seen[k] {
			t.Errorf("Keys() is missing %v", k)
		}
	}
}

// TestEndpointEviction covers the cardinality bound and that the coldest
// endpoint is the one shed.
func TestEndpointEviction(t *testing.T) {
	const maxEndpoints = 4
	s := store.New(store.Config{MaxEndpoints: maxEndpoints})

	// Each endpoint written at a distinct time, oldest first.
	for i := 0; i < maxEndpoints; i++ {
		s.Add(key(fmt.Sprintf("/route-%d", i)), base.Add(hours(i)), mkSchema(1))
	}
	// Keep the second one warm, so the first is unambiguously coldest.
	s.Add(key("/route-1"), base.Add(hours(maxEndpoints)), mkSchema(1))

	s.Add(key("/route-new"), base.Add(hours(maxEndpoints+1)), mkSchema(1))

	st := s.Stats()
	if st.Endpoints > maxEndpoints {
		t.Errorf("Endpoints = %d, want at most %d", st.Endpoints, maxEndpoints)
	}
	if st.Evictions == 0 {
		t.Error("Evictions = 0, want at least one")
	}
	if _, ok := s.Window(key("/route-0"), base, base.Add(hours(1))); ok {
		t.Error("the coldest endpoint survived eviction")
	}
	if _, ok := s.Window(key("/route-new"), base.Add(hours(maxEndpoints+1)), base.Add(hours(maxEndpoints+2))); !ok {
		t.Error("the newest endpoint was not stored")
	}
}

// TestLiveBucketBudget covers the memory bound proper: total non-empty buckets
// across all endpoints, which is what actually costs memory.
func TestLiveBucketBudget(t *testing.T) {
	const budget = 10
	s := store.New(store.Config{MaxLiveBuckets: budget, Buckets: 24})

	// Twenty endpoints, three buckets each: far past the budget.
	for e := 0; e < 20; e++ {
		for h := 0; h < 3; h++ {
			s.Add(key(fmt.Sprintf("/route-%d", e)), base.Add(hours(h)), mkSchema(1))
		}
	}

	st := s.Stats()
	if st.LiveBuckets > budget {
		t.Errorf("LiveBuckets = %d, want at most %d", st.LiveBuckets, budget)
	}
	if st.Evictions == 0 {
		t.Error("Evictions = 0, want the budget to have forced some")
	}
}

// TestEvictionPrefersAgedOutEndpoints checks the cheap case is taken first: an
// endpoint whose data is all past retention can hold nothing useful, so it goes
// before any endpoint with live data.
func TestEvictionPrefersAgedOutEndpoints(t *testing.T) {
	const buckets = 4
	s := store.New(store.Config{Buckets: buckets, MaxEndpoints: 3})

	s.Add(key("/ancient"), base, mkSchema(1))
	s.Add(key("/warm-a"), base.Add(hours(10)), mkSchema(1))
	s.Add(key("/warm-b"), base.Add(hours(11)), mkSchema(1))

	// /ancient is now well past the four-hour horizon.
	s.Add(key("/newcomer"), base.Add(hours(12)), mkSchema(1))

	if _, ok := s.Window(key("/ancient"), base, base.Add(hours(1))); ok {
		t.Error("the aged-out endpoint survived, want it evicted first")
	}
	for _, k := range []string{"/warm-a", "/warm-b", "/newcomer"} {
		found := false
		for _, have := range s.Keys() {
			if have.Template == k {
				found = true
			}
		}
		if !found {
			t.Errorf("%s was evicted, want it kept over the aged-out endpoint", k)
		}
	}
}

func TestRetention(t *testing.T) {
	tests := []struct {
		name string
		cfg  store.Config
		want time.Duration
	}{
		{"defaults are one week of hours", store.Config{}, 7 * 24 * time.Hour},
		{"custom depth", store.Config{Buckets: 12}, 12 * time.Hour},
		{"custom width", store.Config{Buckets: 10, BucketDuration: time.Minute}, 10 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := store.New(tt.cfg).Retention(); got != tt.want {
				t.Errorf("Retention() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestConcurrentUse is the race-detector case: writers and readers against one
// store.
func TestConcurrentUse(t *testing.T) {
	s := store.New(store.Config{Buckets: 24})

	// Each worker owns an endpoint and writes strictly increasing hours, so
	// every write lands in an empty or recycled bucket. That keeps this off
	// schema.Merge while still putting real contention on the store's lock.
	const workers = 8
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			k := key(fmt.Sprintf("/route-%d", w))
			for i := 0; i < 200; i++ {
				s.Add(k, base.Add(hours(i)), mkSchema(1))
				// A single-bucket window, for the same reason.
				s.Window(k, base.Add(hours(i)), base.Add(hours(i+1)))
				s.Keys()
				s.Stats()
			}
		}(w)
	}
	wg.Wait()

	if st := s.Stats(); st.Endpoints != workers {
		t.Errorf("Endpoints = %d, want %d", st.Endpoints, workers)
	}
}

// -- formerly blocked on schema.Merge ----------------------------------------

// TestWindowMergesMultipleBuckets is the whole point of windowing, and cannot
// run yet.
func TestWindowMergesMultipleBuckets(t *testing.T) {
	s := store.New(store.Config{})
	k := key("/users/{id}")
	for i := 0; i < 3; i++ {
		s.Add(k, base.Add(hours(i)), mkSchema(10))
	}

	got, ok := s.Window(k, base, base.Add(hours(3)))
	if !ok {
		t.Fatal("no window")
	}
	if got.Samples != 30 {
		t.Errorf("Samples = %d, want 30", got.Samples)
	}
}

// TestAddMergesWithinABucket covers two observations landing in the same
// bucket, which is the common case in production and needs Merge.
func TestAddMergesWithinABucket(t *testing.T) {
	s := store.New(store.Config{})
	k := key("/users/{id}")
	s.Add(k, base.Add(time.Minute), mkSchema(3))
	s.Add(k, base.Add(2*time.Minute), mkSchema(4))

	got, ok := s.Window(k, base, base.Add(hours(1)))
	if !ok {
		t.Fatal("no window")
	}
	if got.Samples != 7 {
		t.Errorf("Samples = %d, want 7", got.Samples)
	}
	if st := s.Stats(); st.LiveBuckets != 1 {
		t.Errorf("LiveBuckets = %d, want 1 -- both observations belong to one bucket", st.LiveBuckets)
	}
}
