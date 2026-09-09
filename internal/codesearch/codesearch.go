// Package codesearch finds the places in a repository that read a given JSON
// path, so a finding about an upstream field can be tied to the code that will
// break because of it.
//
// It is deliberately dumb and deterministic: it produces *candidates* by
// matching the field's name in its common spellings, and makes no claim that a
// hit is a real usage. Deciding which candidates matter is a judgement, and
// judgement is what the assess package spends a language model on. Doing the
// mechanical part here keeps that spend proportional to the number of real
// findings rather than the size of the repository.
package codesearch

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// Limits, so a search over an unfamiliar repository cannot run away.
const (
	DefaultMaxFileBytes = 1 << 20 // 1 MiB; larger files are generated or vendored
	DefaultMaxHits      = 60
	DefaultContext      = 3
)

// skipDirs are never descended into. Vendored and generated trees produce hits
// that are real matches and useless findings: nobody fixes a bug by editing
// node_modules.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, "target": true, ".venv": true, "venv": true,
	"__pycache__": true, ".next": true, ".idea": true, "testdata": true,
}

// codeExts are the extensions searched. An allow-list rather than a deny-list,
// because the failure mode of guessing wrong is scanning a binary.
var codeExts = map[string]bool{
	".go": true, ".py": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true,
	".rb": true, ".java": true, ".kt": true, ".rs": true, ".php": true, ".cs": true,
	".swift": true, ".scala": true, ".c": true, ".h": true, ".cpp": true, ".hpp": true,
	".sql": true, ".graphql": true,
}

// Config tunes a search. Zero fields take their defaults.
type Config struct {
	MaxFileBytes int64
	MaxHits      int
	// Context is how many lines either side of a hit are captured.
	Context int
}

func (c Config) withDefaults() Config {
	if c.MaxFileBytes <= 0 {
		c.MaxFileBytes = DefaultMaxFileBytes
	}
	if c.MaxHits <= 0 {
		c.MaxHits = DefaultMaxHits
	}
	if c.Context <= 0 {
		c.Context = DefaultContext
	}
	return c
}

// Site is one candidate usage.
type Site struct {
	// File is the path relative to the search root.
	File string `json:"file"`
	// Line is the 1-indexed line the match is on.
	Line int `json:"line"`
	// Match is the spelling that matched, e.g. "legacy_id" or "legacyId".
	Match string `json:"match"`
	// Excerpt is the matching line with a few either side, for a reader or a
	// model to judge whether the hit is real.
	Excerpt []string `json:"excerpt"`
}

// String renders a site as an editor-clickable reference.
func (s Site) String() string { return fmt.Sprintf("%s:%d", s.File, s.Line) }

// Result is one search.
type Result struct {
	// Field is the leaf name that was searched for.
	Field string `json:"field"`
	// Spellings are the forms that were tried.
	Spellings []string `json:"spellings"`
	// Sites are the candidate usages, ordered by file then line.
	Sites []Site `json:"sites"`
	// FilesScanned is how many files were read.
	FilesScanned int `json:"files_scanned"`
	// Truncated reports that MaxHits was reached and there may be more.
	Truncated bool `json:"truncated"`
}

// Search looks for usages of a JSON path under root.
//
// Only the leaf name is searched. A path like "data.items[].legacy_id" reaches
// application code as whatever the deserialiser called it, and the containers
// along the way are usually invisible by then -- so the leaf is the part with
// any chance of appearing verbatim.
func Search(root, jsonPath string, cfg Config) (*Result, error) {
	cfg = cfg.withDefaults()

	// An unreadable subdirectory is skipped below, because one bad permission
	// should not abandon a whole search. A missing root is different: it is a
	// misconfiguration, and silently returning zero results would read as "your
	// code does not use this field".
	if info, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("codesearch: reading %s: %w", root, err)
	} else if !info.IsDir() {
		return nil, fmt.Errorf("codesearch: %s is not a directory", root)
	}

	field := leafOf(jsonPath)
	if field == "" {
		return nil, fmt.Errorf("codesearch: %q has no field name to search for", jsonPath)
	}
	spellings := Spellings(field)

	res := &Result{Field: field, Spellings: spellings}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable directory is not a reason to abandon the search.
			return nil //nolint:nilerr // deliberate: skip and continue
		}
		if d.IsDir() {
			if path != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(res.Sites) >= cfg.MaxHits {
			res.Truncated = true
			return filepath.SkipAll
		}
		if !codeExts[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil || info.Size() > cfg.MaxFileBytes {
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		res.FilesScanned++
		sites, scanErr := scanFile(path, rel, spellings, cfg)
		if scanErr != nil {
			return nil
		}
		res.Sites = append(res.Sites, sites...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("codesearch: walking %s: %w", root, err)
	}

	if len(res.Sites) > cfg.MaxHits {
		res.Sites = res.Sites[:cfg.MaxHits]
		res.Truncated = true
	}
	sort.Slice(res.Sites, func(i, j int) bool {
		if res.Sites[i].File != res.Sites[j].File {
			return res.Sites[i].File < res.Sites[j].File
		}
		return res.Sites[i].Line < res.Sites[j].Line
	})
	return res, nil
}

// scanFile returns every matching line in one file, with context.
func scanFile(path, rel string, spellings []string, cfg Config) ([]Site, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	var sites []Site
	for i, line := range lines {
		for _, spelling := range spellings {
			if !strings.Contains(line, spelling) {
				continue
			}
			sites = append(sites, Site{
				File:    rel,
				Line:    i + 1,
				Match:   spelling,
				Excerpt: excerpt(lines, i, cfg.Context),
			})
			break // one site per line, whichever spelling hit first
		}
	}
	return sites, nil
}

// excerpt returns the line at i with n lines either side.
func excerpt(lines []string, i, n int) []string {
	lo, hi := i-n, i+n+1
	if lo < 0 {
		lo = 0
	}
	if hi > len(lines) {
		hi = len(lines)
	}
	return append([]string(nil), lines[lo:hi]...)
}

// leafOf returns the final field name of a JSON path, stripping the array and
// wildcard markers the schema package uses.
func leafOf(jsonPath string) string {
	p := strings.TrimSuffix(jsonPath, "[]")
	if i := strings.LastIndex(p, "."); i >= 0 {
		p = p[i+1:]
	}
	p = strings.TrimSuffix(p, "[]")
	if p == "{*}" || p == "" {
		return ""
	}
	return p
}

// Spellings returns the forms a JSON field name plausibly takes in source code.
//
// A field arrives over the wire as snake_case and reaches application code as
// whatever the local convention is -- camelCase in TypeScript, PascalCase on a
// Go struct, often with an ID suffix uppercased. Searching only the wire
// spelling finds the deserialiser and misses every call site.
func Spellings(field string) []string {
	words := splitWords(field)
	if len(words) == 0 {
		return nil
	}

	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}

	add(field)
	add(strings.Join(words, "_"))
	add(camel(words, false))
	add(camel(words, true))
	add(camelInitialisms(words, false))
	add(camelInitialisms(words, true))
	return out
}

// splitWords breaks a field name into lowercase words, accepting snake_case,
// kebab-case and camelCase input.
func splitWords(field string) []string {
	var words []string
	var cur strings.Builder

	flush := func() {
		if cur.Len() > 0 {
			words = append(words, strings.ToLower(cur.String()))
			cur.Reset()
		}
	}
	for i, r := range field {
		switch {
		case r == '_' || r == '-' || r == ' ':
			flush()
		case unicode.IsUpper(r) && i > 0:
			flush()
			cur.WriteRune(r)
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return words
}

// camel joins words, capitalising the first when upperFirst.
func camel(words []string, upperFirst bool) string {
	var b strings.Builder
	for i, w := range words {
		if i == 0 && !upperFirst {
			b.WriteString(w)
			continue
		}
		b.WriteString(title(w))
	}
	return b.String()
}

// initialisms are the words Go and several other conventions uppercase whole.
var initialisms = map[string]string{
	"id": "ID", "url": "URL", "uri": "URI", "api": "API", "http": "HTTP",
	"json": "JSON", "html": "HTML", "sql": "SQL", "uuid": "UUID", "ip": "IP",
}

// camelInitialisms is camel with Go's initialism convention applied, so
// "legacy_id" also yields "LegacyID" and not just "LegacyId".
func camelInitialisms(words []string, upperFirst bool) string {
	var b strings.Builder
	for i, w := range words {
		up, isInit := initialisms[w]
		switch {
		case i == 0 && !upperFirst:
			b.WriteString(w)
		case isInit:
			b.WriteString(up)
		default:
			b.WriteString(title(w))
		}
	}
	return b.String()
}

func title(w string) string {
	if w == "" {
		return w
	}
	return strings.ToUpper(w[:1]) + w[1:]
}
