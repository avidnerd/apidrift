package patch_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avidnerd/apidrift/internal/assess"
	"github.com/avidnerd/apidrift/internal/patch"
)

func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func read(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestApply(t *testing.T) {
	root := repo(t, map[string]string{
		"billing/total.go": "package billing\n\nfunc total(c Charge) int {\n\treturn c.Amount\n}\n",
	})

	out, err := patch.Apply(root, []assess.Edit{{
		File: "billing/total.go",
		Old:  "return c.Amount",
		New:  "return c.Amount / 100",
		Why:  "amount is now in dollars",
	}}, false)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if out.Applied != 1 || out.Rejected != 0 {
		t.Fatalf("applied/rejected = %d/%d, want 1/0", out.Applied, out.Rejected)
	}
	if got := read(t, root, "billing/total.go"); !strings.Contains(got, "c.Amount / 100") {
		t.Errorf("the edit was not written:\n%s", got)
	}
}

func TestApplyDryRunWritesNothing(t *testing.T) {
	before := "package a\n\nvar x = c.Amount\n"
	root := repo(t, map[string]string{"a.go": before})

	out, err := patch.Apply(root, []assess.Edit{{File: "a.go", Old: "c.Amount", New: "c.Amount / 100"}}, true)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Applied != 1 {
		t.Errorf("Applied = %d, want 1: a dry run still reports what would land", out.Applied)
	}
	if got := read(t, root, "a.go"); got != before {
		t.Errorf("a dry run modified the file:\n%s", got)
	}
}

// TestApplyRefusesUnsafeEdits is the guard that matters, since these edits come
// from a language model and are written straight into somebody's repository.
func TestApplyRefusesUnsafeEdits(t *testing.T) {
	root := repo(t, map[string]string{
		"a.go":    "package a\n\nvar x = 1\nvar y = 1\n",
		"only.go": "package a\n\nvar unique = 1\n",
	})

	tests := []struct {
		name       string
		edit       assess.Edit
		wantReason string
	}{
		{
			name:       "text that is not there",
			edit:       assess.Edit{File: "only.go", Old: "var missing = 2", New: "x"},
			wantReason: "not in the file",
		},
		{
			name:       "text that appears twice",
			edit:       assess.Edit{File: "a.go", Old: "= 1", New: "= 2"},
			wantReason: "appears 2 times",
		},
		{
			name:       "a file that does not exist",
			edit:       assess.Edit{File: "nope.go", Old: "x", New: "y"},
			wantReason: "cannot read the file",
		},
		{
			name:       "a path escaping the repository",
			edit:       assess.Edit{File: "../../.ssh/config", Old: "x", New: "y"},
			wantReason: "outside the repository",
		},
		{
			name:       "an absolute path",
			edit:       assess.Edit{File: "/etc/hosts", Old: "x", New: "y"},
			wantReason: "absolute path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := patch.Apply(root, []assess.Edit{tt.edit}, false)
			if err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if out.Applied != 0 || out.Rejected != 1 {
				t.Fatalf("applied/rejected = %d/%d, want 0/1", out.Applied, out.Rejected)
			}
			if got := out.Results[0].Reason; !strings.Contains(got, tt.wantReason) {
				t.Errorf("reason = %q, want it to mention %q", got, tt.wantReason)
			}
		})
	}

	// Nothing in the repository changed.
	if got := read(t, root, "a.go"); !strings.Contains(got, "var x = 1\nvar y = 1") {
		t.Errorf("a rejected edit still modified the file:\n%s", got)
	}
}

// TestApplyComposesTwoEditsToOneFile guards that a second edit sees the result
// of the first, rather than reading the file fresh and clobbering it.
func TestApplyComposesTwoEditsToOneFile(t *testing.T) {
	root := repo(t, map[string]string{
		"a.go": "package a\n\nvar first = c.Amount\nvar second = c.Legacy\n",
	})

	out, err := patch.Apply(root, []assess.Edit{
		{File: "a.go", Old: "c.Amount", New: "c.Amount / 100"},
		{File: "a.go", Old: "c.Legacy", New: `""`},
	}, false)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Applied != 2 {
		t.Fatalf("Applied = %d, want 2", out.Applied)
	}

	got := read(t, root, "a.go")
	if !strings.Contains(got, "c.Amount / 100") || !strings.Contains(got, `var second = ""`) {
		t.Errorf("both edits should be present:\n%s", got)
	}
	if len(out.Files) != 1 {
		t.Errorf("Files = %v, want the one file listed once", out.Files)
	}
}

// TestApplyIsAllOrNothingPerFile: a rejected edit must not stop a valid one in
// a different file, but must not leave the rejected file half-written either.
func TestApplyMixedEdits(t *testing.T) {
	root := repo(t, map[string]string{
		"good.go": "package a\n\nvar x = c.Amount\n",
		"bad.go":  "package a\n\nvar y = 1\n",
	})

	out, err := patch.Apply(root, []assess.Edit{
		{File: "good.go", Old: "c.Amount", New: "c.Amount / 100"},
		{File: "bad.go", Old: "not present anywhere", New: "z"},
	}, false)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if out.Applied != 1 || out.Rejected != 1 {
		t.Fatalf("applied/rejected = %d/%d, want 1/1", out.Applied, out.Rejected)
	}
	if !strings.Contains(read(t, root, "good.go"), "/ 100") {
		t.Error("the valid edit did not land")
	}
	if read(t, root, "bad.go") != "package a\n\nvar y = 1\n" {
		t.Error("the rejected edit's file was modified")
	}
}
