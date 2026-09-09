package sample_test

import (
	"sync"
	"testing"
	"time"

	"github.com/avidnerd/apidrift/internal/sample"
)

func mkSample(path string) sample.Sample {
	return sample.Sample{
		Method:     "GET",
		RawPath:    path,
		Status:     200,
		Body:       []byte(`{"ok":true}`),
		ObservedAt: time.Unix(0, 0),
	}
}

func TestSampleStatusClass(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   int
	}{
		{"ok", 200, 2},
		{"created", 201, 2},
		{"moved", 301, 3},
		{"not found", 404, 4},
		{"server error", 503, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (sample.Sample{Status: tt.status}).StatusClass(); got != tt.want {
				t.Errorf("StatusClass() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestQueueOfferBounded(t *testing.T) {
	tests := []struct {
		name        string
		capacity    int
		offers      int
		wantAccept  uint64
		wantDropped uint64
	}{
		{"under capacity", 4, 3, 3, 0},
		{"exactly capacity", 4, 4, 4, 0},
		{"over capacity drops the excess", 4, 10, 4, 6},
		{"capacity of one", 1, 3, 1, 2},
		{"zero capacity is raised to one", 0, 3, 1, 2},
		{"negative capacity is raised to one", -5, 2, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := sample.NewQueue(tt.capacity)

			var accepted uint64
			for i := 0; i < tt.offers; i++ {
				if q.Offer(mkSample("/x")) {
					accepted++
				}
			}
			if accepted != tt.wantAccept {
				t.Errorf("accepted = %d, want %d", accepted, tt.wantAccept)
			}

			got := q.Stats()
			if got.Enqueued != tt.wantAccept {
				t.Errorf("Stats().Enqueued = %d, want %d", got.Enqueued, tt.wantAccept)
			}
			if got.Dropped != tt.wantDropped {
				t.Errorf("Stats().Dropped = %d, want %d", got.Dropped, tt.wantDropped)
			}
			if want := int(tt.wantAccept); got.Len != want {
				t.Errorf("Stats().Len = %d, want %d", got.Len, want)
			}
			if got.Capacity < 1 {
				t.Errorf("Stats().Capacity = %d, want at least 1", got.Capacity)
			}
		})
	}
}

func TestQueueDrainingMakesRoom(t *testing.T) {
	q := sample.NewQueue(2)

	for _, p := range []string{"/a", "/b"} {
		if !q.Offer(mkSample(p)) {
			t.Fatalf("Offer(%s) = false, want true", p)
		}
	}
	if q.Offer(mkSample("/c")) {
		t.Fatal("Offer on a full queue = true, want false")
	}

	if got := <-q.C(); got.RawPath != "/a" {
		t.Errorf("first sample = %q, want /a (FIFO)", got.RawPath)
	}
	if !q.Offer(mkSample("/c")) {
		t.Error("Offer after draining = false, want true")
	}
}

func TestQueueCloseIsIdempotentAndOfferStaysSafe(t *testing.T) {
	q := sample.NewQueue(4)
	if !q.Offer(mkSample("/a")) {
		t.Fatal("Offer before Close = false, want true")
	}

	q.Close()
	q.Close() // must not panic

	// Buffered samples are still drainable, and the channel then closes.
	if got := <-q.C(); got.RawPath != "/a" {
		t.Errorf("buffered sample = %q, want /a", got.RawPath)
	}
	if _, open := <-q.C(); open {
		t.Error("channel still open after Close and drain, want closed")
	}

	// Offering after Close drops rather than panicking on a closed channel.
	if q.Offer(mkSample("/b")) {
		t.Error("Offer after Close = true, want false")
	}
	if st := q.Stats(); !st.Closed || st.Dropped != 1 {
		t.Errorf("Stats() = %+v, want Closed=true Dropped=1", st)
	}
}

func TestQueueRangeTerminatesOnClose(t *testing.T) {
	q := sample.NewQueue(8)
	for i := 0; i < 3; i++ {
		q.Offer(mkSample("/a"))
	}
	q.Close()

	n := 0
	for range q.C() {
		n++
	}
	if n != 3 {
		t.Errorf("drained %d samples, want 3", n)
	}
}

// TestQueueConcurrentProducers is the race-detector case: every offer must be
// accounted for as exactly one enqueue or one drop, with no lost updates.
func TestQueueConcurrentProducers(t *testing.T) {
	const (
		producers    = 8
		perProducer  = 500
		totalOffers  = producers * perProducer
		queueDepth   = 16
		consumerStop = time.Second
	)

	q := sample.NewQueue(queueDepth)

	// A consumer that drains slowly enough that drops definitely happen.
	consumed := make(chan int)
	go func() {
		n := 0
		for range q.C() {
			n++
		}
		consumed <- n
	}()

	var wg sync.WaitGroup
	for i := 0; i < producers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perProducer; j++ {
				q.Offer(mkSample("/concurrent"))
			}
		}()
	}
	wg.Wait()
	q.Close()

	select {
	case <-consumed:
	case <-time.After(consumerStop):
		t.Fatal("consumer did not finish after Close")
	}

	st := q.Stats()
	if st.Enqueued+st.Dropped != totalOffers {
		t.Errorf("Enqueued(%d) + Dropped(%d) = %d, want %d",
			st.Enqueued, st.Dropped, st.Enqueued+st.Dropped, totalOffers)
	}
}
