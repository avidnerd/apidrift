package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/avidnerd/apidrift/internal/assess"
	"github.com/avidnerd/apidrift/internal/codesearch"
	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/patch"
)

// maxPatchFileBytes bounds how much source is put in front of the model for one
// finding. Whole files give it the context an excerpt cannot, but a generated
// or vendored file would swamp the request for no benefit.
const maxPatchFileBytes = 60 << 10

type fixConfig struct {
	provider  string
	maxTokens int64
	admin     string
	repo      string
	model     string
	branch    string
	timeout   time.Duration
	maxFix    int
	apply     bool
	openPR    bool
}

func parseFixFlags(args []string, stderr io.Writer) (fixConfig, error) {
	var c fixConfig

	fs := flag.NewFlagSet("fix", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.admin, "admin", "http://127.0.0.1:9090", "admin address of a running apidrift serve")
	fs.StringVar(&c.repo, "repo", ".", "repository to patch")
	fs.StringVar(&c.model, "model", "", "model to use (default: the provider's own default)")
	fs.Int64Var(&c.maxTokens, "max-tokens", assess.DefaultMaxTokens, "cap on the model's response length; lower it if your account has limited credit")
	fs.StringVar(&c.provider, "provider", "auto", "anthropic, openrouter, or auto (anthropic if ANTHROPIC_API_KEY is set, else openrouter)")
	fs.StringVar(&c.branch, "branch", "", "branch to commit on (default: apidrift/fix-<timestamp>)")
	fs.DurationVar(&c.timeout, "timeout", 10*time.Minute, "overall time limit")
	fs.IntVar(&c.maxFix, "max", 10, "most findings to work on in one run")
	fs.BoolVar(&c.apply, "apply", false, "write the changes to disk on a new branch (default: show them and write nothing)")
	fs.BoolVar(&c.openPR, "pr", false, "push the branch and open a pull request (implies -apply)")

	if err := fs.Parse(args); err != nil {
		return c, parseError(err)
	}
	if c.openPR {
		c.apply = true
	}
	if c.maxFix < 1 {
		return c, fmt.Errorf("%w: -max must be at least 1", errUsage)
	}
	if c.branch == "" {
		c.branch = "apidrift/fix-" + time.Now().UTC().Format("20060102-1504")
	}
	return c, nil
}

// fixCmd is the last step of the loop: take the findings, work out what they
// break in this repository, write the patch, and open a pull request.
//
// It writes nothing unless asked. The default prints what it would change,
// because these edits come from a language model and land in somebody's source
// tree, and seeing them first should be the easy path rather than the careful
// one.
func fixCmd(args []string, stdout, stderr io.Writer) error {
	cfg, err := parseFixFlags(args, stderr)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	rep, err := fetchReport(ctx, http.DefaultClient, cfg.admin)
	if err != nil {
		return err
	}

	var findings []detect.Finding
	for _, g := range rep.Groups {
		findings = append(findings, g.Findings...)
	}
	if len(findings) == 0 {
		fmt.Fprintln(stdout, "apidrift: no findings to fix")
		return nil
	}
	if len(findings) > cfg.maxFix {
		fmt.Fprintf(stderr, "apidrift: %d findings, working on the first %d (-max)\n", len(findings), cfg.maxFix)
		findings = findings[:cfg.maxFix]
	}

	if cfg.apply {
		if err := requireCleanTree(cfg.repo); err != nil {
			return err
		}
	}

	client, err := assess.NewAssessor(assess.Provider(cfg.provider), assess.Config{Model: cfg.model, MaxTokens: cfg.maxTokens})
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "apidrift fix: %s\n", assess.Describe(client))

	var (
		allEdits []assess.Edit
		titles   []string
		bodies   []string
		unfixed  []string
	)

	for _, f := range findings {
		search, err := codesearch.Search(cfg.repo, f.Path, codesearch.Config{})
		if err != nil {
			fmt.Fprintf(stderr, "apidrift: searching for %s: %v\n", f.Path, err)
			continue
		}
		if len(search.Sites) == 0 {
			continue
		}

		verdict, err := client.Patch(ctx, assess.PatchRequest{
			Endpoint: f.Endpoint,
			Path:     f.Path,
			Change:   describeFinding(f),
			Sites:    search.Sites,
			Files:    readSiteFiles(cfg.repo, search.Sites),
		})
		if err != nil {
			return fmt.Errorf("proposing a fix for %s: %w", f.Path, err)
		}

		allEdits = append(allEdits, verdict.Edits...)
		if verdict.Title != "" {
			titles = append(titles, verdict.Title)
		}
		if verdict.Body != "" {
			bodies = append(bodies, "## "+f.Path+"\n\n"+verdict.Body)
		}
		if verdict.Unfixable != "" {
			unfixed = append(unfixed, f.Path+": "+verdict.Unfixable)
		}
	}

	if len(allEdits) == 0 {
		fmt.Fprintln(stdout, "apidrift: nothing to change in this repository")
		for _, u := range unfixed {
			fmt.Fprintf(stdout, "  needs a person: %s\n", u)
		}
		return nil
	}

	outcome, err := patch.Apply(cfg.repo, allEdits, !cfg.apply)
	if err != nil {
		return err
	}
	printOutcome(stdout, outcome, unfixed, cfg.apply)

	if !cfg.apply || outcome.Applied == 0 {
		if !cfg.apply {
			fmt.Fprintln(stdout, "\nnothing was written. re-run with -apply to make these changes on a new branch.")
		}
		return nil
	}

	title := "Adapt to upstream API changes"
	if len(titles) == 1 {
		title = titles[0]
	}
	body := strings.Join(bodies, "\n\n")
	if len(unfixed) > 0 {
		body += "\n\n## Still needs a person\n\n"
		for _, u := range unfixed {
			body += "- " + u + "\n"
		}
	}
	body += "\n---\nFound and patched by apidrift from observed API traffic."

	if err := commitOnBranch(cfg.repo, cfg.branch, title, body, outcome.Files); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "\ncommitted on %s\n", cfg.branch)

	if !cfg.openPR {
		fmt.Fprintln(stdout, "re-run with -pr to push it and open a pull request.")
		return nil
	}
	return openPullRequest(cfg.repo, cfg.branch, title, body, stdout)
}

// readSiteFiles loads the files that have candidate sites, so the model can
// write an edit that fits the surrounding code rather than the excerpt.
func readSiteFiles(root string, sites []codesearch.Site) map[string]string {
	out := map[string]string{}
	for _, s := range sites {
		if _, done := out[s.File]; done {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, s.File))
		if err != nil || len(b) > maxPatchFileBytes {
			continue
		}
		out[s.File] = string(b)
	}
	return out
}

func printOutcome(w io.Writer, out *patch.Outcome, unfixed []string, applied bool) {
	verb := "would change"
	if applied {
		verb = "changed"
	}
	fmt.Fprintf(w, "\napidrift fix: %d edit(s) %s, %d rejected\n\n", out.Applied, verb, out.Rejected)

	for _, r := range out.Results {
		if !r.Applied {
			fmt.Fprintf(w, "  REJECTED  %s\n            %s\n", r.Edit.File, r.Reason)
			continue
		}
		fmt.Fprintf(w, "  %s\n", r.Edit.File)
		if r.Edit.Why != "" {
			fmt.Fprintf(w, "    %s\n", r.Edit.Why)
		}
		for _, line := range strings.Split(strings.TrimRight(r.Edit.Old, "\n"), "\n") {
			fmt.Fprintf(w, "    - %s\n", line)
		}
		for _, line := range strings.Split(strings.TrimRight(r.Edit.New, "\n"), "\n") {
			fmt.Fprintf(w, "    + %s\n", line)
		}
		fmt.Fprintln(w)
	}

	for _, u := range unfixed {
		fmt.Fprintf(w, "  needs a person: %s\n", u)
	}
}

// requireCleanTree refuses to write into a repository with uncommitted work, so
// a patch can always be undone with git checkout.
func requireCleanTree(repo string) error {
	out, err := git(repo, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("%s does not look like a git repository: %w", repo, err)
	}
	if strings.TrimSpace(out) != "" {
		return fmt.Errorf("%s has uncommitted changes; commit or stash them first so this patch can be reviewed on its own", repo)
	}
	return nil
}

func commitOnBranch(repo, branch, title, body string, files []string) error {
	if _, err := git(repo, "checkout", "-b", branch); err != nil {
		return fmt.Errorf("creating branch %s: %w", branch, err)
	}
	args := append([]string{"add", "--"}, files...)
	if _, err := git(repo, args...); err != nil {
		return fmt.Errorf("staging the patch: %w", err)
	}
	if _, err := git(repo, "commit", "-m", title, "-m", body); err != nil {
		return fmt.Errorf("committing the patch: %w", err)
	}
	return nil
}

func openPullRequest(repo, branch, title, body string, stdout io.Writer) error {
	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("the GitHub CLI (gh) is not installed, so the branch was committed but no pull request was opened")
	}
	if _, err := git(repo, "push", "-u", "origin", branch); err != nil {
		return fmt.Errorf("pushing %s: %w", branch, err)
	}

	cmd := exec.Command("gh", "pr", "create", "--title", title, "--body", body)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("opening the pull request: %w\n%s", err, out)
	}
	fmt.Fprintf(stdout, "%s", out)
	return nil
}

func git(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
