package proxy

import (
	"io"
	"sync"
)

// teeOutcome describes how a response body ended, and is what decides whether
// the captured bytes are worth analysing.
type teeOutcome struct {
	// Body is the captured body, valid only when Complete is true and
	// Oversize is false. It is owned by the callback once handed over.
	Body []byte
	// Complete reports that the body was read through to io.EOF. A body that
	// was closed early -- a client that hung up mid-response -- is truncated
	// and therefore useless for schema extraction.
	Complete bool
	// Oversize reports that the body exceeded the byte cap. Captured bytes are
	// discarded in that case, since a JSON document cut off mid-object does
	// not parse.
	Oversize bool
}

// teeBody wraps a response body, copying what passes through it into a capped
// buffer and invoking onDone exactly once when the body is finished with.
//
// It sits on the response path, so it does the cheapest possible thing: an
// append into a preallocated buffer. It never blocks, never allocates beyond
// the cap, and never changes what the client receives -- Read returns the
// underlying reader's bytes and errors verbatim.
type teeBody struct {
	src    io.ReadCloser
	max    int
	onDone func(teeOutcome)

	// mu guards the capture state. Read and Close are normally called from the
	// same goroutine, but a transport may close a response body from another
	// on cancellation, so the state is locked rather than assumed serial.
	mu       sync.Mutex
	buf      []byte
	oversize bool
	complete bool
	done     bool
}

// newTeeBody wraps src, capturing at most max bytes. sizeHint, when positive
// and within the cap, preallocates the capture buffer. onDone is called exactly
// once, from whichever of Read or Close finishes the body.
func newTeeBody(src io.ReadCloser, max int, sizeHint int64, onDone func(teeOutcome)) *teeBody {
	t := &teeBody{src: src, max: max, onDone: onDone}
	if sizeHint > 0 && sizeHint <= int64(max) {
		t.buf = make([]byte, 0, sizeHint)
	}
	return t
}

// Read passes through to the underlying body, capturing a copy of what it
// returns. The client sees exactly the bytes and errors the upstream produced.
func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.src.Read(p)

	t.mu.Lock()
	if n > 0 && !t.oversize {
		if len(t.buf)+n > t.max {
			// Past the cap: stop capturing and release what we have. Partial
			// JSON is not worth the memory it occupies.
			t.oversize = true
			t.buf = nil
		} else {
			t.buf = append(t.buf, p[:n]...)
		}
	}
	if err == io.EOF {
		t.complete = true
	}
	finished := err != nil
	t.mu.Unlock()

	if finished {
		t.finish()
	}
	return n, err
}

// Close finishes the capture and closes the underlying body. A Close that
// arrives before io.EOF marks the capture incomplete.
func (t *teeBody) Close() error {
	t.finish()
	return t.src.Close()
}

// finish invokes onDone at most once, handing over ownership of the buffer.
func (t *teeBody) finish() {
	t.mu.Lock()
	if t.done {
		t.mu.Unlock()
		return
	}
	t.done = true
	out := teeOutcome{Complete: t.complete, Oversize: t.oversize}
	// The buffer is handed over only when it holds a whole document. A partial
	// capture is released here rather than travelling on as an unusable field.
	if t.complete && !t.oversize {
		out.Body = t.buf
	}
	t.buf = nil
	t.mu.Unlock()

	t.onDone(out)
}
