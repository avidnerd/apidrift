package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/avidnerd/apidrift/internal/assess"
	"github.com/avidnerd/apidrift/internal/codesearch"
	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/eval"
	"github.com/avidnerd/apidrift/internal/patch"
)

// maxSpecBytes bounds a downloaded spec. Stripe's is around 6 MB, so this has
// plenty of headroom while still refusing something pathological.
const maxSpecBytes = 64 << 20

// watch is the zero-infrastructure path, and the one that makes this usable in
// CI rather than only on a machine somebody is watching.
//
// serve/report/assess/fix all require a proxy carrying live traffic, which is a
// large thing to ask of somebody who just wants to know when an API they depend
// on breaks. watch asks for nothing: point it at a spec URL and a repository,
// run it on a schedule, and it opens a pull request when the spec changes in a
// way that affects the code.
//
// The trade against the traffic path is real and worth stating. A spec is what
// the provider says; traffic is what the provider does, and the two disagree
// often enough that the disagreement is frequently the bug. watch cannot see a
// field that is documented as required and is actually absent 6% of the time.
// It can see everything a provider publishes, on every repository, with no
// deployment at all.
type watchConfig struct {
	spec      string
	repo      string
	cacheDir  string
	provider  string
	model     string
	maxTokens int64
	maxFix    int
	timeout   time.Duration
	enums     bool
	all       bool
	apply     bool
	openPR    bool
	branch    string
}

func parseWatchFlags(args []string, stderr io.Writer) (watchConfig, error) {
	var c watchConfig

	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.spec, "spec", "", "URL or path of the OpenAPI spec to watch (required)")
	fs.StringVar(&c.repo, "repo", ".", "repository that uses the API")
	fs.StringVar(&c.cacheDir, "cache", ".apidrift", "directory holding the last seen version of the spec")
	fs.StringVar(&c.provider, "provider", "auto", "anthropic, openrouter, or auto")
	fs.StringVar(&c.model, "model", "", "model to use (default: the provider's own default)")
	fs.Int64Var(&c.maxTokens, "max-tokens", assess.DefaultMaxTokens, "cap on the model's response length")
	fs.IntVar(&c.maxFix, "max", 10, "most changes to work on in one run")
	fs.DurationVar(&c.timeout, "timeout", 10*time.Minute, "overall time limit")
	fs.BoolVar(&c.enums, "enums", false, "also report new enum values (noisy: they were 91% of Stripe's changes over three months)")
	fs.BoolVar(&c.all, "all", false, "report every change, including fields being added")
	fs.BoolVar(&c.apply, "apply", false, "write the changes on a new branch (default: show them and write nothing)")
	fs.BoolVar(&c.openPR, "pr", false, "push the branch and open a pull request (implies -apply)")
	fs.StringVar(&c.branch, "branch", "", "branch to commit on (default: apidrift/spec-<timestamp>)")

	if err := fs.Parse(args); err != nil {
		return c, parseError(err)
	}
	if c.spec == "" {
		return c, fmt.Errorf("%w: watch requires -spec", errUsage)
	}
	if c.openPR {
		c.apply = true
	}
	if c.maxFix < 1 {
		return c, fmt.Errorf("%w: -max must be at least 1", errUsage)
	}
	if c.branch == "" {
		c.branch = "apidrift/spec-" + time.Now().UTC().Format("20060102-1504")
	}
	return c, nil
}

// breakingKinds are the changes reported by default: the ones that make working
// code stop working.
//
// Two kinds are deliberately excluded, and the second only after seeing real
// data. A field being added breaks nobody, so -all is needed for those. A new
// enum value is more arguable, since a caller with an exhaustive switch can
// miss a case, but measuring three months of Stripe settled it: 552 new enum
// values against 57 removed fields, so enums were 91% of everything reported.
// Providers add payment methods and bank codes constantly. At that ratio the
// real signal is buried, and a tool nobody can read is a tool nobody keeps. The
// -enums flag brings them back.
var breakingKinds = map[detect.ChangeKind]bool{
	detect.FieldRemoved:          true,
	detect.TypeChanged:           true,
	detect.NullabilityIntroduced: true,
}

func watchCmd(args []string, stdout, stderr io.Writer) error {
	cfg, err := parseWatchFlags(args, stderr)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	current, raw, err := loadSpecFrom(ctx, cfg.spec)
	if err != nil {
		return err
	}

	cachePath := filepath.Join(cfg.repo, cfg.cacheDir, specCacheName(cfg.spec))
	previousRaw, err := os.ReadFile(cachePath)
	if os.IsNotExist(err) {
		// First run. There is nothing to compare against, so record the spec
		// and say so plainly rather than reporting a clean bill of health.
		if err := writeCache(cachePath, raw); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "apidrift watch: first run\n\n")
		fmt.Fprintf(stdout, "Saved %s as the baseline (%d endpoints).\n", cfg.spec, len(current.Responses))
		fmt.Fprintf(stdout, "Nothing to compare against yet. Commit %s so the next run has it.\n", cachePath)
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the cached spec %s: %w", cachePath, err)
	}

	previous, err := eval.ParseSpec(previousRaw)
	if err != nil {
		return fmt.Errorf("parsing the cached spec %s: %w", cachePath, err)
	}

	changes := eval.Diff(previous, current)
	if !cfg.all {
		kept := changes[:0]
		for _, c := range changes {
			switch {
			case breakingKinds[c.Kind]:
				kept = append(kept, c)
			case cfg.enums && c.Kind == detect.EnumValueAdded:
				kept = append(kept, c)
			}
		}
		changes = kept
	}

	fmt.Fprintf(stdout, "apidrift watch: %s\n\n", cfg.spec)
	if len(changes) == 0 {
		fmt.Fprintf(stdout, "No breaking changes since the last run.\n")
		return writeCache(cachePath, raw)
	}
	fmt.Fprintf(stdout, "%d change(s) since the last run:\n", len(changes))
	for _, c := range changes {
		fmt.Fprintf(stdout, "  %s %s %s\n", c.Endpoint, c.Path, c.Kind)
	}

	// The search is local and costs nothing, so it runs over every change. The
	// cap belongs on model calls, which cost money, and applying it earlier
	// would let changes that do not touch this repository crowd out the ones
	// that do.
	type job struct {
		change eval.Change
		sites  []codesearch.Site
	}
	var jobs []job
	for _, c := range changes {
		found, err := codesearch.Search(cfg.repo, c.Path, codesearch.Config{})
		if err != nil {
			fmt.Fprintf(stderr, "apidrift: searching for %s: %v\n", c.Path, err)
			continue
		}
		if len(found.Sites) > 0 {
			jobs = append(jobs, job{change: c, sites: found.Sites})
		}
	}

	fmt.Fprintf(stdout, "\n%d of them touch this repository.\n", len(jobs))
	if len(jobs) == 0 {
		fmt.Fprintf(stdout, "Nothing to patch.\n")
		return writeCache(cachePath, raw)
	}
	if len(jobs) > cfg.maxFix {
		fmt.Fprintf(stderr, "apidrift: patching the first %d (-max)\n", cfg.maxFix)
		jobs = jobs[:cfg.maxFix]
	}

	if cfg.apply {
		if err := requireCleanTree(cfg.repo); err != nil {
			return err
		}
	}

	client, err := assess.NewAssessor(assess.Provider(cfg.provider),
		assess.Config{Model: cfg.model, MaxTokens: cfg.maxTokens})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "asking %s for a patch\n", assess.Describe(client))

	var (
		edits   []assess.Edit
		bodies  []string
		unfixed []string
	)
	for _, j := range jobs {
		verdict, err := client.Patch(ctx, assess.PatchRequest{
			Endpoint: j.change.Endpoint,
			Path:     j.change.Path,
			Change:   fmt.Sprintf("%s: %s (from the provider's OpenAPI spec)", j.change.Kind, j.change.Detail),
			Sites:    j.sites,
			Files:    readSiteFiles(cfg.repo, j.sites),
		})
		if err != nil {
			return fmt.Errorf("proposing a fix for %s: %w", j.change.Path, err)
		}
		edits = append(edits, verdict.Edits...)
		if verdict.Body != "" {
			bodies = append(bodies, "## "+j.change.Path+"\n\n"+verdict.Body)
		}
		if verdict.Unfixable != "" {
			unfixed = append(unfixed, j.change.Path+": "+verdict.Unfixable)
		}
	}

	if len(edits) == 0 {
		fmt.Fprintf(stdout, "\nNo code changes needed.\n")
		for _, u := range unfixed {
			fmt.Fprintf(stdout, "  needs a person: %s\n", u)
		}
		return writeCache(cachePath, raw)
	}

	outcome, err := patch.Apply(cfg.repo, edits, !cfg.apply)
	if err != nil {
		return err
	}
	printOutcome(stdout, outcome, unfixed, cfg.apply)

	if !cfg.apply {
		fmt.Fprintln(stdout, "\nnothing was written. re-run with -apply to make these changes on a new branch.")
		return nil
	}
	if outcome.Applied == 0 {
		return writeCache(cachePath, raw)
	}

	// The cache moves forward in the same commit as the patch, so the change is
	// only marked as handled if the patch itself lands.
	if err := writeCache(cachePath, raw); err != nil {
		return err
	}
	rel, _ := filepath.Rel(cfg.repo, cachePath)
	files := append(outcome.Files, rel)

	title := fmt.Sprintf("Adapt to %d upstream API change(s)", len(jobs))
	body := strings.Join(bodies, "\n\n")
	if len(unfixed) > 0 {
		body += "\n\n## Still needs a person\n\n"
		for _, u := range unfixed {
			body += "- " + u + "\n"
		}
	}
	body += "\n---\nOpened by apidrift from a change in " + cfg.spec

	if err := commitOnBranch(cfg.repo, cfg.branch, title, body, files); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\ncommitted on %s\n", cfg.branch)

	if !cfg.openPR {
		fmt.Fprintln(stdout, "re-run with -pr to push it and open a pull request.")
		return nil
	}
	return openPullRequest(cfg.repo, cfg.branch, title, body, stdout)
}

// loadSpecFrom reads a spec from a URL or a path, returning the parsed spec and
// the bytes it came from. The bytes are what gets cached, so the next run
// compares against exactly what this run saw.
func loadSpecFrom(ctx context.Context, src string) (*eval.Spec, []byte, error) {
	var raw []byte

	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %v", errUsage, err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, nil, fmt.Errorf("fetching %s: %w", src, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return nil, nil, fmt.Errorf("fetching %s: %s", src, resp.Status)
		}
		raw, err = io.ReadAll(io.LimitReader(resp.Body, maxSpecBytes))
		if err != nil {
			return nil, nil, fmt.Errorf("reading %s: %w", src, err)
		}
	} else {
		var err error
		if raw, err = os.ReadFile(src); err != nil {
			return nil, nil, fmt.Errorf("reading %s: %w", src, err)
		}
	}

	spec, err := eval.ParseSpec(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing %s: %w", src, err)
	}
	return spec, raw, nil
}

// specCacheName derives a stable filename for a spec source, so one repository
// can watch several APIs without them colliding.
func specCacheName(src string) string {
	sum := sha256.Sum256([]byte(src))
	base := filepath.Base(strings.SplitN(src, "?", 2)[0])
	if base == "" || base == "." || base == "/" {
		base = "spec"
	}
	return hex.EncodeToString(sum[:6]) + "-" + base
}

func writeCache(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}
