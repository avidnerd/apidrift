package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// watchRepo builds a throwaway git repository that consumes the billing API.
func watchRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	src := "package billing\n\ntype Charge struct {\n\tLegacyID string `json:\"legacy_id\"`\n}\n"
	if err := os.WriteFile(filepath.Join(root, "billing.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"}, {"add", "-A"},
		{"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return root
}

// TestWatchFirstRunRecordsABaseline covers the state that would otherwise look
// like a clean bill of health when it is really "I have nothing to compare to".
func TestWatchFirstRunRecordsABaseline(t *testing.T) {
	repo := watchRepo(t)

	var out, errOut bytes.Buffer
	if code := run([]string{"watch", "-spec", "../../internal/eval/testdata/billing-v1.yaml", "-repo", repo}, &out, &errOut); code != exitOK {
		t.Fatalf("run() = %d\n%s\n%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "first run") {
		t.Errorf("a first run must say so rather than report no changes:\n%s", out.String())
	}

	entries, err := os.ReadDir(filepath.Join(repo, ".apidrift"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("the baseline spec was not cached: %v", err)
	}
}

// TestWatchDetectsBreakingChanges is the product in one test: a spec changed
// under the repository, and watch says which changes matter and which of them
// touch this code.
func TestWatchDetectsBreakingChanges(t *testing.T) {
	repo := watchRepo(t)
	spec := filepath.Join(t.TempDir(), "provider.yaml")

	copyFile(t, "../../internal/eval/testdata/billing-v1.yaml", spec)
	var out, errOut bytes.Buffer
	run([]string{"watch", "-spec", spec, "-repo", repo}, &out, &errOut)

	// The provider ships a breaking change.
	copyFile(t, "../../internal/eval/testdata/billing-v2.yaml", spec)
	out.Reset()
	errOut.Reset()
	// -max 0 is invalid, so this stops before any model call by pointing at a
	// provider with no credentials rather than by capping the work.
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENROUTER_API_KEY", "")
	run([]string{"watch", "-spec", spec, "-repo", repo}, &out, &errOut)

	got := out.String()
	for _, want := range []string{
		"legacy_id field_removed",
		"balance type_changed",
		"email nullability_introduced",
		"touch this repository",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output is missing %q:\n%s", want, got)
		}
	}
	// A field being added breaks nobody, so it stays out unless -all is given.
	if strings.Contains(got, "field_added") {
		t.Errorf("additive changes should be excluded by default:\n%s", got)
	}
}

func TestWatchRequiresASpec(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"watch"}, &out, &errOut); code != exitUsage {
		t.Errorf("run() = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(errOut.String(), "requires -spec") {
		t.Errorf("error does not say what is missing:\n%s", errOut.String())
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	b, err := os.ReadFile(from)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(to, b, 0o644); err != nil {
		t.Fatal(err)
	}
}
