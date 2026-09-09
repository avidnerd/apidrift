package proxy

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// scriptedBody returns the given chunks one per Read, then the terminal error.
type scriptedBody struct {
	chunks   [][]byte
	i        int
	endErr   error // returned once chunks are exhausted; io.EOF unless overridden
	closed   int
	closeErr error
}

// Read serves at most one chunk per call, and never more than fits in p, so a
// small read buffer shortens reads instead of silently dropping bytes.
func (s *scriptedBody) Read(p []byte) (int, error) {
	if s.i >= len(s.chunks) {
		if s.endErr != nil {
			return 0, s.endErr
		}
		return 0, io.EOF
	}
	n := copy(p, s.chunks[s.i])
	s.chunks[s.i] = s.chunks[s.i][n:]
	if len(s.chunks[s.i]) == 0 {
		s.i++
	}
	return n, nil
}

func (s *scriptedBody) Close() error {
	s.closed++
	return s.closeErr
}

func chunks(ss ...string) [][]byte {
	out := make([][]byte, 0, len(ss))
	for _, s := range ss {
		out = append(out, []byte(s))
	}
	return out
}

func TestTeeBodyCapture(t *testing.T) {
	tests := []struct {
		name         string
		chunks       [][]byte
		endErr       error
		max          int
		sizeHint     int64
		closeEarly   bool // Close before reading to EOF
		wantPassed   string
		wantBody     string
		wantComplete bool
		wantOversize bool
	}{
		{
			name:         "complete body under the cap is captured",
			chunks:       chunks(`{"a":1,`, `"b":2}`),
			max:          64,
			wantPassed:   `{"a":1,"b":2}`,
			wantBody:     `{"a":1,"b":2}`,
			wantComplete: true,
		},
		{
			name:         "body exactly at the cap is captured",
			chunks:       chunks("0123456789"),
			max:          10,
			wantPassed:   "0123456789",
			wantBody:     "0123456789",
			wantComplete: true,
		},
		{
			name:         "body one byte over the cap is discarded",
			chunks:       chunks("0123456789", "X"),
			max:          10,
			wantPassed:   "0123456789X",
			wantBody:     "",
			wantComplete: true,
			wantOversize: true,
		},
		{
			name:         "oversize is detected mid-stream and the rest still passes through",
			chunks:       chunks("aaaa", "bbbb", "cccc"),
			max:          6,
			wantPassed:   "aaaabbbbcccc",
			wantBody:     "",
			wantComplete: true,
			wantOversize: true,
		},
		{
			name:         "size hint preallocates without changing the result",
			chunks:       chunks(`{"ok":true}`),
			max:          64,
			sizeHint:     11,
			wantPassed:   `{"ok":true}`,
			wantBody:     `{"ok":true}`,
			wantComplete: true,
		},
		{
			name:         "an oversize hint is ignored rather than preallocating past the cap",
			chunks:       chunks("ab"),
			max:          4,
			sizeHint:     1 << 30,
			wantPassed:   "ab",
			wantBody:     "ab",
			wantComplete: true,
		},
		{
			name:       "close before EOF marks the capture incomplete",
			chunks:     chunks("{\"a\":", "1}"),
			max:        64,
			closeEarly: true,
			wantPassed: `{"a":`,
			wantBody:   "",
		},
		{
			name:       "a non-EOF read error marks the capture incomplete",
			chunks:     chunks(`{"a":`),
			endErr:     errors.New("connection reset"),
			max:        64,
			wantPassed: `{"a":`,
			wantBody:   "",
		},
		{
			name:         "an empty body is complete and empty",
			chunks:       nil,
			max:          64,
			wantPassed:   "",
			wantBody:     "",
			wantComplete: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := &scriptedBody{chunks: tt.chunks, endErr: tt.endErr}

			var got teeOutcome
			calls := 0
			tee := newTeeBody(src, tt.max, tt.sizeHint, func(o teeOutcome) {
				calls++
				got = o
			})

			var passed bytes.Buffer
			buf := make([]byte, 8)
			for i := 0; ; i++ {
				n, err := tee.Read(buf)
				passed.Write(buf[:n])
				if err != nil {
					break
				}
				if tt.closeEarly && i == 0 {
					break
				}
			}
			if err := tee.Close(); err != nil {
				t.Fatalf("Close() = %v, want nil", err)
			}

			if calls != 1 {
				t.Errorf("onDone called %d times, want exactly 1", calls)
			}
			if passed.String() != tt.wantPassed {
				t.Errorf("bytes passed to the client = %q, want %q", passed.String(), tt.wantPassed)
			}
			if string(got.Body) != tt.wantBody {
				t.Errorf("captured body = %q, want %q", got.Body, tt.wantBody)
			}
			if got.Complete != tt.wantComplete {
				t.Errorf("Complete = %v, want %v", got.Complete, tt.wantComplete)
			}
			if got.Oversize != tt.wantOversize {
				t.Errorf("Oversize = %v, want %v", got.Oversize, tt.wantOversize)
			}
			if src.closed != 1 {
				t.Errorf("underlying body closed %d times, want exactly 1", src.closed)
			}
		})
	}
}

// TestTeeBodyReadErrorIsVerbatim guards the promise that the tee never alters
// what the caller sees: the error from the underlying body is passed through
// unchanged, not swallowed or wrapped.
func TestTeeBodyReadErrorIsVerbatim(t *testing.T) {
	want := errors.New("upstream exploded")
	src := &scriptedBody{chunks: chunks("partial"), endErr: want}
	tee := newTeeBody(src, 64, 0, func(teeOutcome) {})

	buf := make([]byte, 32)
	if _, err := tee.Read(buf); err != nil {
		t.Fatalf("first Read() = %v, want nil", err)
	}
	if _, err := tee.Read(buf); !errors.Is(err, want) {
		t.Errorf("second Read() = %v, want %v", err, want)
	}
}

// TestTeeBodyCloseReturnsUnderlyingError guards that Close does not hide a
// failure from the underlying body.
func TestTeeBodyCloseReturnsUnderlyingError(t *testing.T) {
	want := errors.New("close failed")
	src := &scriptedBody{closeErr: want}
	tee := newTeeBody(src, 64, 0, func(teeOutcome) {})

	if err := tee.Close(); !errors.Is(err, want) {
		t.Errorf("Close() = %v, want %v", err, want)
	}
}
