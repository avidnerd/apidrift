package codesearch_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/avidnerd/apidrift/internal/codesearch"
)

// repo writes a throwaway source tree and returns its root.
func repo(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", full, err)
		}
	}
	return root
}

func TestSpellings(t *testing.T) {
	tests := []struct {
		field string
		want  []string // every one of these must be produced
	}{
		{"legacy_id", []string{"legacy_id", "legacyId", "LegacyId", "LegacyID", "legacyID"}},
		{"amount", []string{"amount", "Amount"}},
		{"risk_score", []string{"risk_score", "riskScore", "RiskScore"}},
		{"payment_method_url", []string{"payment_method_url", "paymentMethodUrl", "PaymentMethodURL"}},
		{"camelCaseInput", []string{"camelCaseInput", "camel_case_input", "CamelCaseInput"}},
	}

	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			got := codesearch.Spellings(tt.field)
			have := make(map[string]bool, len(got))
			for _, g := range got {
				have[g] = true
			}
			for _, want := range tt.want {
				if !have[want] {
					t.Errorf("Spellings(%q) is missing %q; got %v", tt.field, want, got)
				}
			}
		})
	}
}

func TestSpellingsAreUnique(t *testing.T) {
	got := codesearch.Spellings("id")
	seen := map[string]bool{}
	for _, s := range got {
		if seen[s] {
			t.Errorf("Spellings returned %q twice: %v", s, got)
		}
		seen[s] = true
	}
}

// TestSearchFindsUsagesAcrossSpellings is the point of the package: the field
// arrives over the wire as snake_case and reaches the code as whatever the
// local convention is.
func TestSearchFindsUsagesAcrossSpellings(t *testing.T) {
	root := repo(t, map[string]string{
		"api/client.go":   "package api\n\ntype Charge struct {\n\tLegacyID string `json:\"legacy_id\"`\n}\n",
		"web/render.ts":   "export function show(c: Charge) {\n  return c.legacyId;\n}\n",
		"jobs/report.py":  "def run(charge):\n    return charge['legacy_id']\n",
		"docs/notes.md":   "the legacy_id field is deprecated\n",
		"unrelated/go.go": "package unrelated\n\nfunc noop() {}\n",
	})

	res, err := codesearch.Search(root, "data.legacy_id", codesearch.Config{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}

	if res.Field != "legacy_id" {
		t.Errorf("Field = %q, want legacy_id", res.Field)
	}

	files := map[string]bool{}
	for _, s := range res.Sites {
		files[s.File] = true
	}
	for _, want := range []string{"api/client.go", "web/render.ts", "jobs/report.py"} {
		if !files[filepath.FromSlash(want)] {
			t.Errorf("no hit in %s; got %v", want, res.Sites)
		}
	}
	// Markdown is not searched: the extension allow-list keeps prose out.
	if files[filepath.FromSlash("docs/notes.md")] {
		t.Error("searched a markdown file; only source extensions should be scanned")
	}
	if files[filepath.FromSlash("unrelated/go.go")] {
		t.Error("reported a file with no match")
	}
}

func TestSearchCapturesLineAndExcerpt(t *testing.T) {
	root := repo(t, map[string]string{
		"a.go": "package a\n\n// header\nfunc f() {\n\tx := charge.Amount\n\t_ = x\n}\n",
	})

	res, err := codesearch.Search(root, "amount", codesearch.Config{Context: 1})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Sites) != 1 {
		t.Fatalf("%d sites, want 1: %v", len(res.Sites), res.Sites)
	}

	site := res.Sites[0]
	if site.Line != 5 {
		t.Errorf("Line = %d, want 5", site.Line)
	}
	if site.Match != "Amount" {
		t.Errorf("Match = %q, want Amount", site.Match)
	}
	if len(site.Excerpt) != 3 {
		t.Errorf("Excerpt has %d lines, want 3 with a context of 1: %v", len(site.Excerpt), site.Excerpt)
	}
	if !strings.Contains(strings.Join(site.Excerpt, "\n"), "charge.Amount") {
		t.Errorf("Excerpt does not contain the matching line: %v", site.Excerpt)
	}
	if got := site.String(); got != filepath.FromSlash("a.go")+":5" {
		t.Errorf("String() = %q, want a clickable file:line", got)
	}
}

// TestSearchSkipsNoiseDirectories: hits in vendored or generated trees are real
// matches and useless findings, because nobody fixes a bug by editing
// node_modules.
func TestSearchSkipsNoiseDirectories(t *testing.T) {
	root := repo(t, map[string]string{
		"src/app.js":                "const a = data.amount;\n",
		"node_modules/pkg/index.js": "const a = data.amount;\n",
		"vendor/lib/lib.go":         "x := data.Amount\n",
		".git/hooks/pre-commit.sh":  "amount\n",
		"testdata/fixture.json.go":  "const amount = 1\n",
		"dist/bundle.js":            "const a = data.amount;\n",
	})

	res, err := codesearch.Search(root, "amount", codesearch.Config{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Sites) != 1 {
		t.Fatalf("%d sites, want only the one in src: %v", len(res.Sites), res.Sites)
	}
	if res.Sites[0].File != filepath.FromSlash("src/app.js") {
		t.Errorf("hit in %s, want src/app.js", res.Sites[0].File)
	}
}

func TestSearchRespectsMaxHits(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 40; i++ {
		files[filepath.Join("pkg", "f"+string(rune('a'+i%26))+string(rune('a'+i/26))+".go")] =
			"package pkg\nvar amount = 1\nvar amount2 = 2\n"
	}
	root := repo(t, files)

	res, err := codesearch.Search(root, "amount", codesearch.Config{MaxHits: 5})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Sites) > 5 {
		t.Errorf("%d sites, want at most 5", len(res.Sites))
	}
	if !res.Truncated {
		t.Error("Truncated = false, want true when the cap was reached")
	}
}

func TestSearchIsOrdered(t *testing.T) {
	root := repo(t, map[string]string{
		"z.go": "package z\nvar amount = 1\n",
		"a.go": "package a\nvar amount = 1\nvar x = amount\n",
	})

	res, err := codesearch.Search(root, "amount", codesearch.Config{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(res.Sites) != 3 {
		t.Fatalf("%d sites, want 3: %v", len(res.Sites), res.Sites)
	}
	// Sorted by file, then line, so two runs produce the same report.
	want := []string{"a.go:2", "a.go:3", "z.go:2"}
	for i, w := range want {
		if got := res.Sites[i].String(); got != filepath.FromSlash(w) {
			t.Errorf("site %d = %q, want %q", i, got, w)
		}
	}
}

func TestSearchPathHandling(t *testing.T) {
	tests := []struct {
		name      string
		jsonPath  string
		wantField string
		wantErr   bool
	}{
		{"root field", "amount", "amount", false},
		{"nested", "data.legacy_id", "legacy_id", false},
		{"array element field", "data.items[].status", "status", false},
		{"the array itself", "data.items[]", "items", false},
		{"a map wildcard has no name to search", "data.{*}", "", true},
		{"empty", "", "", true},
	}

	root := repo(t, map[string]string{"a.go": "package a\n"})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := codesearch.Search(root, tt.jsonPath, codesearch.Config{})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Search(%q) = nil error, want one", tt.jsonPath)
				}
				return
			}
			if err != nil {
				t.Fatalf("Search(%q): %v", tt.jsonPath, err)
			}
			if res.Field != tt.wantField {
				t.Errorf("Field = %q, want %q", res.Field, tt.wantField)
			}
		})
	}
}

func TestSearchMissingRoot(t *testing.T) {
	if _, err := codesearch.Search(filepath.Join(t.TempDir(), "nope"), "amount", codesearch.Config{}); err == nil {
		t.Error("Search over a missing root = nil error, want an error")
	}
}
