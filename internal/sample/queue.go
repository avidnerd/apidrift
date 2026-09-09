package sample

import (
	"sync"
	"sync/atomic"
)

// Sink is the narrow interface the proxy needs in order to hand off a sample.
// Offer must never block: the proxy calls it on the response path, and analysis
// is never allowed to add latency to a client request.
type Sink interface {
	// Offer submits a sample for analysis, reporting whether it was accepted.
	// A false return means the sample was dropped, not that anything failed.
	Offer(Sample) bool
}

// QueueStats is a point-in-time snapshot of a Queue.
type QueueStats struct {
	// Capacity is the queue's fixed depth, in samples.
	Capacity int `json:"capacity"`
	// Len is how many samples are currently buffered.
	Len int `json:"len"`
	// Enqueued is the lifetime count of accepted samples.
	Enqueued uint64 `json:"enqueued"`
	// Dropped is the lifetime count of samples refused because the queue was
	// full or closed.
	Dropped uint64 `json:"dropped"`
	// Closed reports whether the queue has been closed to new samples.
	Closed bool `json:"closed"`
}

// Queue is a bounded, deliberately lossy hand-off between the proxy and the
// analysis pipeline. It is safe for concurrent use by many producers and many
// consumers.
//
// Bounded memory: a Queue holds at most Capacity samples, each at most
// MaxBodyBytes of body, so its footprint is capped at roughly
// Capacity * MaxBodyBytes. When it is full, Offer drops the sample and
// increments the drop counter instead of blocking. Losing observations degrades
// the statistics; blocking would degrade the service being proxied. Dropping is
// the correct trade.
type Queue struct {
	ch chan Sample

	// mu guards closed and makes the send in Offer safe against a concurrent
	// Close. It is held for reading by producers, so they do not contend with
	// each other, and for writing only by Close.
	mu     sync.RWMutex
	closed bool

	enqueued atomic.Uint64
	dropped  atomic.Uint64
}

// NewQueue returns a Queue with the given depth in samples. A capacity below 1
// is raised to 1, since a zero-capacity channel would make Offer drop every
// sample that did not have a consumer already parked on it.
func NewQueue(capacity int) *Queue {
	if capacity < 1 {
		capacity = 1
	}
	return &Queue{ch: make(chan Sample, capacity)}
}

// Offer submits s without blocking, reporting whether it was accepted. It
// returns false, and counts a drop, when the queue is full or closed.
func (q *Queue) Offer(s Sample) bool {
	q.mu.RLock()
	defer q.mu.RUnlock()

	if q.closed {
		q.dropped.Add(1)
		return false
	}
	select {
	case q.ch <- s:
		q.enqueued.Add(1)
		return true
	default:
		q.dropped.Add(1)
		return false
	}
}

// C returns the channel consumers range over. It is closed by Close, so a
// range loop over it terminates once the queue is closed and drained.
func (q *Queue) C() <-chan Sample { return q.ch }

// Close stops the queue accepting samples and closes the consumer channel once
// buffered samples have been drained. It is idempotent. Offer remains safe to
// call after Close; it simply drops.
func (q *Queue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return
	}
	q.closed = true
	close(q.ch)
}

// Stats returns a snapshot of the queue's counters. The fields are read
// independently, so a snapshot taken during heavy traffic may be very slightly
// inconsistent between fields; it is intended for monitoring, not accounting.
func (q *Queue) Stats() QueueStats {
	q.mu.RLock()
	closed := q.closed
	q.mu.RUnlock()

	return QueueStats{
		Capacity: cap(q.ch),
		Len:      len(q.ch),
		Enqueued: q.enqueued.Load(),
		Dropped:  q.dropped.Load(),
		Closed:   closed,
	}
}
