package detect

import "sync"

// MaxTrackedFindings caps how many distinct findings a Tracker will follow at
// once. Beyond it, new findings are not tracked and are counted in
// TrackerStats.Dropped rather than silently ignored.
//
// The map is pruned to the current window's findings on every Observe, so in
// practice its size is the number of findings in one window. The cap is a
// backstop against a pathological window -- a schema change that lights up
// every field of every endpoint at once -- not an expected limit.
const MaxTrackedFindings = 100_000

// TrackerStats describes what the Tracker has seen.
type TrackerStats struct {
	// Windows is how many windows have been observed.
	Windows uint64 `json:"windows"`
	// Tracked is how many findings are currently being followed.
	Tracked int `json:"tracked"`
	// Emitted is the lifetime count of findings returned by Observe.
	Emitted uint64 `json:"emitted"`
	// Suppressed is the lifetime count of findings withheld for not yet having
	// survived PersistWindows.
	Suppressed uint64 `json:"suppressed"`
	// Dropped counts findings that could not be tracked because the tracker
	// was at MaxTrackedFindings.
	Dropped uint64 `json:"dropped"`
}

// streak is how many consecutive windows one finding has appeared in.
type streak struct {
	count int
	// lastWindow is the window number the finding was last seen in. A gap
	// between it and the current window is what breaks the streak.
	lastWindow uint64
}

// Tracker suppresses findings until they have survived PersistWindows
// consecutive windows.
//
// This is the answer to a specific failure mode. A significance test run every
// window against thousands of fields will, by construction, produce some
// findings that are pure sampling luck -- that is what a p-value threshold
// means. Those are uncorrelated between windows, so they vanish on the next
// one. A real upstream change does not vanish: it is there in every window
// after the deploy. Requiring persistence therefore costs one window of
// detection latency and removes an entire class of false positive, which is a
// good trade for a tool whose credibility depends on its alerts being worth
// reading.
//
// It is bookkeeping over Detect's verdicts, not a second opinion on them: it
// never turns a non-finding into a finding.
//
// Tracker is safe for concurrent use.
type Tracker struct {
	cfg Config

	mu      sync.Mutex
	streaks map[ID]*streak
	window  uint64
	stats   TrackerStats
}

// NewTracker returns a Tracker using cfg, with zero fields defaulted.
func NewTracker(cfg Config) *Tracker {
	return &Tracker{
		cfg:     cfg.withDefaults(),
		streaks: make(map[ID]*streak),
	}
}

// Observe records one window's findings and returns those that have now
// appeared in PersistWindows consecutive windows, in the order given.
//
// A finding that survives keeps being returned for as long as it keeps
// appearing: it is a condition that is still true, not an event that already
// fired, and a report showing only newly-qualified findings would go blank
// while the upstream was still broken.
//
// Findings absent from this window are forgotten entirely. A change that comes
// back later starts its streak again, which is the honest reading -- an
// intermittent finding is exactly what sampling noise looks like.
func (t *Tracker) Observe(findings []Finding) []Finding {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.window++
	t.stats.Windows++

	out := make([]Finding, 0, len(findings))
	for _, f := range findings {
		id := f.ID()

		s, ok := t.streaks[id]
		switch {
		case ok && s.lastWindow == t.window:
			// A duplicate within this window. Detect should not produce these,
			// but counting one change twice would let a duplicate qualify a
			// finding a window early, so it is guarded rather than assumed.
		case ok && s.lastWindow == t.window-1:
			s.count++
			s.lastWindow = t.window
		case ok:
			// A gap: the streak broke and this is the start of a new one.
			s.count = 1
			s.lastWindow = t.window
		default:
			if len(t.streaks) >= MaxTrackedFindings {
				t.stats.Dropped++
				continue
			}
			s = &streak{count: 1, lastWindow: t.window}
			t.streaks[id] = s
		}

		if s.count >= t.cfg.PersistWindows {
			t.stats.Emitted++
			out = append(out, f)
		} else {
			t.stats.Suppressed++
		}
	}

	t.pruneLocked()
	return out
}

// pruneLocked forgets findings that did not appear in the current window.
// Without it the map would accumulate every finding ever seen, and a streak
// broken long ago would still be occupying space.
func (t *Tracker) pruneLocked() {
	for id, s := range t.streaks {
		if s.lastWindow != t.window {
			delete(t.streaks, id)
		}
	}
}

// Stats returns a snapshot of the tracker's state.
func (t *Tracker) Stats() TrackerStats {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.stats
	s.Tracked = len(t.streaks)
	return s
}
