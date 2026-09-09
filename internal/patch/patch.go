// Package patch applies the edits the assess layer proposes, and refuses the
// ones it cannot apply safely.
//
// Every edit is an exact text replacement, and every one is checked before
// anything is written: the file has to exist, be inside the repository, and
// contain the old text exactly once. An edit failing any of those is rejected
// with a reason rather than applied approximately. Nothing is written unless
// the whole set verifies, because a half-landed patch is worse than one that
// never landed.
package patch

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/avidnerd/apidrift/internal/assess"
)

// Result describes what happened to one edit.
type Result struct {
	Edit assess.Edit
	// Applied reports whether it was written.
	Applied bool
	// Reason explains a rejection.
	Reason string
}

// Outcome is the result of applying a set of edits.
type Outcome struct {
	Results []Result
	// Files lists the files that changed, sorted.
	Files []string
	// Applied and Rejected are counts, for a summary line.
	Applied  int
	Rejected int
}

// Apply writes the edits into root, or reports why it cannot.
//
// dryRun verifies everything and writes nothing, which is what the CLI does by
// default.
func Apply(root string, edits []assess.Edit, dryRun bool) (*Outcome, error) {
	out := &Outcome{Results: make([]Result, 0, len(edits))}

	// Verify first and build the new contents in memory, keyed by absolute
	// path so two edits to the same file compose rather than clobber.
	pending := map[string]string{}
	changed := map[string]bool{}

	for _, e := range edits {
		abs, err := safeJoin(root, e.File)
		if err != nil {
			out.Results = append(out.Results, Result{Edit: e, Reason: err.Error()})
			continue
		}

		body, held := pending[abs]
		if !held {
			b, err := os.ReadFile(abs)
			if err != nil {
				out.Results = append(out.Results, Result{Edit: e, Reason: "cannot read the file: " + err.Error()})
				continue
			}
			body = string(b)
		}

		switch n := strings.Count(body, e.Old); {
		case n == 0:
			out.Results = append(out.Results, Result{Edit: e,
				Reason: "the text to replace is not in the file, so applying it would mean guessing"})
			continue
		case n > 1:
			out.Results = append(out.Results, Result{Edit: e,
				Reason: fmt.Sprintf("the text to replace appears %d times, so the edit is ambiguous", n)})
			continue
		}

		pending[abs] = strings.Replace(body, e.Old, e.New, 1)
		changed[e.File] = true
		out.Results = append(out.Results, Result{Edit: e, Applied: true})
	}

	for _, r := range out.Results {
		if r.Applied {
			out.Applied++
			continue
		}
		out.Rejected++
	}
	for f := range changed {
		out.Files = append(out.Files, f)
	}
	sort.Strings(out.Files)

	if dryRun || out.Applied == 0 {
		return out, nil
	}

	for abs, body := range pending {
		info, err := os.Stat(abs)
		if err != nil {
			return out, fmt.Errorf("patch: stat %s: %w", abs, err)
		}
		if err := os.WriteFile(abs, []byte(body), info.Mode().Perm()); err != nil {
			return out, fmt.Errorf("patch: writing %s: %w", abs, err)
		}
	}
	return out, nil
}

// safeJoin resolves name inside root and refuses anything that escapes it.
//
// These paths come from a language model, so a path like "../../.ssh/config"
// is a shape worth refusing outright rather than trusting never to appear.
func safeJoin(root, name string) (string, error) {
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("refusing an absolute path: %s", name)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	joined := filepath.Clean(filepath.Join(absRoot, name))

	rel, err := filepath.Rel(absRoot, joined)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("refusing a path outside the repository: %s", name)
	}
	return joined, nil
}
