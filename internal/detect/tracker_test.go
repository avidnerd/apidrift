package detect_test

import (
	"testing"

	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/endpoint"
)

// Tracker is implemented, so these tests pass. They exercise it against
// hand-built findings, without going near the Detect stub.

func mkFinding(path string, kind detect.ChangeKind) detect.Finding {
	return detect.Finding{
		Endpoint: endpoint.Key{Method: "GET", Template: "/users/{id}", StatusClass: 2},
		Path:     path,
		Kind:     kind,
		Severity: detect.SeverityHigh,
	}
}

func paths(findings []detect.Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Path)
	}
	return out
}

func equalPaths(got []detect.Finding, want ...string) bool {
	g := paths(got)
	if len(g) != len(want) {
		return false
	}
	for i := range g {
		if g[i] != want[i] {
			return false
		}
	}
	return true
}

// TestTrackerSuppressesUntilPersistent is the core of the wrapper: a finding
// that appears once and goes is noise; one that keeps appearing is a change.
func TestTrackerSuppressesUntilPersistent(t *testing.T) {
	tests := []struct {
		name string
		// windows is the sequence of findings, one entry per window.
		windows [][]detect.Finding
		persist int
		// wantEmitted is the paths emitted per window.
		wantEmitted [][]string
	}{
		{
			name:    "a one-window flicker is suppressed",
			persist: 2,
			windows: [][]detect.Finding{
				{mkFinding("a", detect.FieldRemoved)},
				{},
			},
			wantEmitted: [][]string{{}, {}},
		},
		{
			name:    "two consecutive windows qualify",
			persist: 2,
			windows: [][]detect.Finding{
				{mkFinding("a", detect.FieldRemoved)},
				{mkFinding("a", detect.FieldRemoved)},
			},
			wantEmitted: [][]string{{}, {"a"}},
		},
		{
			name:    "a qualified finding keeps being reported while it persists",
			persist: 2,
			windows: [][]detect.Finding{
				{mkFinding("a", detect.FieldRemoved)},
				{mkFinding("a", detect.FieldRemoved)},
				{mkFinding("a", detect.FieldRemoved)},
				{mkFinding("a", detect.FieldRemoved)},
			},
			wantEmitted: [][]string{{}, {"a"}, {"a"}, {"a"}},
		},
		{
			name:    "a gap restarts the streak",
			persist: 3,
			windows: [][]detect.Finding{
				{mkFinding("a", detect.FieldRemoved)},
				{mkFinding("a", detect.FieldRemoved)},
				{}, // gap
				{mkFinding("a", detect.FieldRemoved)},
				{mkFinding("a", detect.FieldRemoved)},
				{mkFinding("a", detect.FieldRemoved)},
			},
			wantEmitted: [][]string{{}, {}, {}, {}, {}, {"a"}},
		},
		{
			name:    "persist of one emits immediately",
			persist: 1,
			windows: [][]detect.Finding{
				{mkFinding("a", detect.FieldRemoved)},
			},
			wantEmitted: [][]string{{"a"}},
		},
		{
			name:    "findings are tracked independently",
			persist: 2,
			windows: [][]detect.Finding{
				{mkFinding("a", detect.FieldRemoved), mkFinding("b", detect.FieldRemoved)},
				{mkFinding("a", detect.FieldRemoved)},
				{mkFinding("a", detect.FieldRemoved), mkFinding("b", detect.FieldRemoved)},
				{mkFinding("a", detect.FieldRemoved), mkFinding("b", detect.FieldRemoved)},
			},
			wantEmitted: [][]string{{}, {"a"}, {"a"}, {"a", "b"}},
		},
		{
			name:    "the same path with a different kind is a different finding",
			persist: 2,
			windows: [][]detect.Finding{
				{mkFinding("a", detect.FieldRemoved)},
				{mkFinding("a", detect.TypeChanged)},
				{mkFinding("a", detect.TypeChanged)},
			},
			wantEmitted: [][]string{{}, {}, {"a"}},
		},
		{
			name:    "input order is preserved",
			persist: 1,
			windows: [][]detect.Finding{
				{mkFinding("z", detect.FieldRemoved), mkFinding("a", detect.FieldRemoved), mkFinding("m", detect.FieldRemoved)},
			},
			wantEmitted: [][]string{{"z", "a", "m"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := detect.NewTracker(detect.Config{PersistWindows: tt.persist})

			for i, w := range tt.windows {
				got := tr.Observe(w)
				if !equalPaths(got, tt.wantEmitted[i]...) {
					t.Errorf("window %d emitted %v, want %v", i, paths(got), tt.wantEmitted[i])
				}
			}
		})
	}
}

// TestTrackerDuplicatesWithinAWindow guards that a duplicate cannot qualify a
// finding a window early.
func TestTrackerDuplicatesWithinAWindow(t *testing.T) {
	tr := detect.NewTracker(detect.Config{PersistWindows: 2})

	f := mkFinding("a", detect.FieldRemoved)
	if got := tr.Observe([]detect.Finding{f, f, f}); len(got) != 0 {
		t.Errorf("window 0 emitted %v, want nothing: three copies in one window are still one window", paths(got))
	}
	if got := tr.Observe([]detect.Finding{f}); !equalPaths(got, "a") {
		t.Errorf("window 1 emitted %v, want [a]", paths(got))
	}
}

// TestTrackerForgetsDepartedFindings guards the memory bound: the map holds the
// current window's findings, not every finding ever seen.
func TestTrackerForgetsDepartedFindings(t *testing.T) {
	tr := detect.NewTracker(detect.Config{PersistWindows: 2})

	var many []detect.Finding
	for i := 0; i < 500; i++ {
		many = append(many, mkFinding(string(rune('a'+i%26))+string(rune('a'+i/26)), detect.FieldRemoved))
	}
	tr.Observe(many)
	if got := tr.Stats().Tracked; got != len(many) {
		t.Errorf("Tracked = %d after a full window, want %d", got, len(many))
	}

	tr.Observe([]detect.Finding{mkFinding("survivor", detect.FieldRemoved)})
	if got := tr.Stats().Tracked; got != 1 {
		t.Errorf("Tracked = %d after the findings departed, want 1", got)
	}
}

func TestTrackerStats(t *testing.T) {
	tr := detect.NewTracker(detect.Config{PersistWindows: 2})

	a, b := mkFinding("a", detect.FieldRemoved), mkFinding("b", detect.FieldRemoved)
	tr.Observe([]detect.Finding{a, b}) // both suppressed
	tr.Observe([]detect.Finding{a})    // a emitted, b forgotten

	st := tr.Stats()
	if st.Windows != 2 {
		t.Errorf("Windows = %d, want 2", st.Windows)
	}
	if st.Suppressed != 2 {
		t.Errorf("Suppressed = %d, want 2", st.Suppressed)
	}
	if st.Emitted != 1 {
		t.Errorf("Emitted = %d, want 1", st.Emitted)
	}
	if st.Tracked != 1 {
		t.Errorf("Tracked = %d, want 1", st.Tracked)
	}
}

func TestTrackerEmptyWindows(t *testing.T) {
	tr := detect.NewTracker(detect.Config{PersistWindows: 2})

	for i := 0; i < 3; i++ {
		if got := tr.Observe(nil); len(got) != 0 {
			t.Errorf("window %d emitted %v from no findings, want nothing", i, paths(got))
		}
	}
	if st := tr.Stats(); st.Windows != 3 {
		t.Errorf("Windows = %d, want 3 -- an empty window is still a window", st.Windows)
	}
}

func TestChangeKindAndSeverityRoundTrip(t *testing.T) {
	for _, k := range detect.AllChangeKinds {
		t.Run("kind/"+k.String(), func(t *testing.T) {
			text, err := k.MarshalText()
			if err != nil {
				t.Fatalf("MarshalText: %v", err)
			}
			var got detect.ChangeKind
			if err := got.UnmarshalText(text); err != nil {
				t.Fatalf("UnmarshalText(%q): %v", text, err)
			}
			if got != k {
				t.Errorf("round trip gave %v, want %v", got, k)
			}
		})
	}

	for _, s := range []detect.Severity{detect.SeverityInfo, detect.SeverityHigh, detect.SeverityCritical} {
		t.Run("severity/"+s.String(), func(t *testing.T) {
			text, err := s.MarshalText()
			if err != nil {
				t.Fatalf("MarshalText: %v", err)
			}
			var got detect.Severity
			if err := got.UnmarshalText(text); err != nil {
				t.Fatalf("UnmarshalText(%q): %v", text, err)
			}
			if got != s {
				t.Errorf("round trip gave %v, want %v", got, s)
			}
		})
	}

	t.Run("unknown names are rejected", func(t *testing.T) {
		var k detect.ChangeKind
		if err := k.UnmarshalText([]byte("nonsense")); err == nil {
			t.Error("UnmarshalText(nonsense) = nil error, want an error")
		}
		var s detect.Severity
		if err := s.UnmarshalText([]byte("nonsense")); err == nil {
			t.Error("UnmarshalText(nonsense) = nil error, want an error")
		}
	})
}
