package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/avidnerd/apidrift/internal/assess"
	"github.com/avidnerd/apidrift/internal/codesearch"
	"github.com/avidnerd/apidrift/internal/detect"
)

// assessConfig is the parsed command line for "apidrift assess".
type assessConfig struct {
	provider  string
	maxTokens int64
	admin     string
	repo      string
	model     string
	timeout   time.Duration
	maxItems  int
	dryRun    bool
	asJSON    bool
	semantic  bool
	impact    bool
	backend   string
}

func parseAssessFlags(args []string, stderr io.Writer) (assessConfig, error) {
	var c assessConfig

	fs := flag.NewFlagSet("assess", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.admin, "admin", "http://127.0.0.1:9090", "admin address of a running apidrift serve")
	fs.StringVar(&c.repo, "repo", ".", "repository to search for affected call sites")
	fs.StringVar(&c.model, "model", "", "model to use (default: the provider's own default)")
	fs.Int64Var(&c.maxTokens, "max-tokens", assess.DefaultMaxTokens, "cap on the model's response length; lower it if your account has limited credit")
	fs.StringVar(&c.provider, "provider", "auto", "anthropic, openrouter, or auto (anthropic if ANTHROPIC_API_KEY is set, else openrouter)")
	fs.DurationVar(&c.timeout, "timeout", 5*time.Minute, "overall time limit")
	fs.IntVar(&c.maxItems, "max", 20, "most items to assess in one run, so a bad window cannot run up a bill")
	fs.BoolVar(&c.dryRun, "dry-run", false, "show what would be sent to the model, and send nothing")
	fs.BoolVar(&c.asJSON, "json", false, "emit JSON instead of the human-readable form")
	fs.BoolVar(&c.semantic, "semantic", true, "judge whether flagged values changed meaning")
	fs.BoolVar(&c.impact, "impact", true, "judge what confirmed findings break in -repo")

	if err := fs.Parse(args); err != nil {
		return c, parseError(err)
	}
	if c.maxItems < 1 {
		return c, fmt.Errorf("%w: -max must be at least 1", errUsage)
	}
	if !c.semantic && !c.impact {
		return c, fmt.Errorf("%w: -semantic and -impact are both off, so there is nothing to do", errUsage)
	}
	return c, nil
}

// Assessment is one run's output.
type Assessment struct {
	GeneratedAt time.Time            `json:"generated_at"`
	DryRun      bool                 `json:"dry_run"`
	Backend     string               `json:"backend"`
	Semantic    []SemanticAssessment `json:"semantic"`
	Impact      []ImpactAssessment   `json:"impact"`
}

// SemanticAssessment pairs a candidate with its verdict.
type SemanticAssessment struct {
	Request assess.SemanticRequest  `json:"request"`
	Verdict *assess.SemanticVerdict `json:"verdict,omitempty"`
}

// ImpactAssessment pairs a finding with its verdict.
type ImpactAssessment struct {
	Finding detect.Finding        `json:"finding"`
	Sites   int                   `json:"candidate_sites"`
	Verdict *assess.ImpactVerdict `json:"verdict,omitempty"`
}

// assessCmd runs the judgement layer over a proxy's current output.
func assessCmd(args []string, stdout, stderr io.Writer) error {
	cfg, err := parseAssessFlags(args, stderr)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()

	// A dry run uses the Fake, so the whole flow -- candidate selection, code
	// search, prompt construction -- is exercisable without credentials and
	// without spending anything.
	var assessor assess.Assessor = &assess.Fake{}
	if !cfg.dryRun {
		assessor, err = assess.NewAssessor(assess.Provider(cfg.provider), assess.Config{Model: cfg.model, MaxTokens: cfg.maxTokens})
		if err != nil {
			return err
		}
	}

	cfg.backend = assess.Describe(assessor)
	out := Assessment{GeneratedAt: time.Now().UTC(), DryRun: cfg.dryRun, Backend: cfg.backend}

	if cfg.semantic {
		out.Semantic, err = runSemantic(ctx, cfg, assessor, stderr)
		if err != nil {
			return err
		}
	}
	if cfg.impact {
		out.Impact, err = runImpact(ctx, cfg, assessor, stderr)
		if err != nil {
			return err
		}
	}

	if cfg.asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	return writeAssessment(stdout, cfg, out)
}

// runSemantic judges the candidates the proxy selected.
func runSemantic(ctx context.Context, cfg assessConfig, a assess.Assessor, stderr io.Writer) ([]SemanticAssessment, error) {
	candidates, err := fetchCandidates(ctx, http.DefaultClient, cfg.admin)
	if err != nil {
		return nil, err
	}
	if len(candidates) > cfg.maxItems {
		fmt.Fprintf(stderr, "apidrift: %d candidates, assessing the first %d (-max)\n", len(candidates), cfg.maxItems)
		candidates = candidates[:cfg.maxItems]
	}

	out := make([]SemanticAssessment, 0, len(candidates))
	for _, req := range candidates {
		item := SemanticAssessment{Request: req}
		if !cfg.dryRun {
			verdict, err := a.Semantic(ctx, req)
			if err != nil {
				return nil, fmt.Errorf("assessing %s %s: %w", req.Endpoint, req.Path, err)
			}
			item.Verdict = verdict
		}
		out = append(out, item)
	}
	return out, nil
}

// runImpact judges what the confirmed findings break in the repository.
func runImpact(ctx context.Context, cfg assessConfig, a assess.Assessor, stderr io.Writer) ([]ImpactAssessment, error) {
	rep, err := fetchReport(ctx, http.DefaultClient, cfg.admin)
	if err != nil {
		return nil, err
	}

	var findings []detect.Finding
	for _, g := range rep.Groups {
		findings = append(findings, g.Findings...)
	}
	if len(findings) > cfg.maxItems {
		fmt.Fprintf(stderr, "apidrift: %d findings, assessing the first %d (-max)\n", len(findings), cfg.maxItems)
		findings = findings[:cfg.maxItems]
	}

	out := make([]ImpactAssessment, 0, len(findings))
	for _, f := range findings {
		// The repository search is deterministic and cheap, so it runs even in
		// a dry run -- seeing the candidate sites is most of the value of one.
		search, err := codesearch.Search(cfg.repo, f.Path, codesearch.Config{})
		if err != nil {
			fmt.Fprintf(stderr, "apidrift: searching for %s: %v\n", f.Path, err)
			continue
		}

		item := ImpactAssessment{Finding: f, Sites: len(search.Sites)}
		if !cfg.dryRun {
			verdict, err := a.Impact(ctx, assess.ImpactRequest{
				Endpoint: f.Endpoint,
				Path:     f.Path,
				Change:   describeFinding(f),
				Sites:    search.Sites,
			})
			if err != nil {
				return nil, fmt.Errorf("assessing %s %s: %w", f.Endpoint, f.Path, err)
			}
			item.Verdict = verdict
		}
		out = append(out, item)
	}
	return out, nil
}

// describeFinding renders a finding as the sentence the model is given.
func describeFinding(f detect.Finding) string {
	s := fmt.Sprintf("%s at %s (baseline %.1f%%, current %.1f%%)",
		f.Kind, f.Path, f.BaselineRate*100, f.CurrentRate*100)
	if f.Evidence != "" {
		s += ". " + f.Evidence
	}
	return s
}

// fetchCandidates retrieves the semantic candidates from a running proxy.
func fetchCandidates(ctx context.Context, client *http.Client, admin string) ([]assess.SemanticRequest, error) {
	endpoint := strings.TrimSuffix(admin, "/") + "/candidates"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: building request for %s: %v", errUsage, endpoint, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, unreachable(endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxReportBytes))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %s: %s", endpoint, resp.Status, describeError(body))
	}

	var out []assess.SemanticRequest
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decoding candidates from %s: %w", endpoint, err)
	}
	return out, nil
}

// writeAssessment renders the human-readable form.
func writeAssessment(w io.Writer, cfg assessConfig, a Assessment) error {
	if a.DryRun {
		fmt.Fprintf(w, "apidrift assess: DRY RUN, nothing was sent to a model\n\n")
	} else {
		fmt.Fprintf(w, "apidrift assess: %s\n\n", cfg.backend)
	}

	if cfg.semantic {
		fmt.Fprintf(w, "semantic candidates (%d)\n", len(a.Semantic))
		fmt.Fprintf(w, "  paths whose values moved in a way the statistical detector cannot see\n\n")
		if len(a.Semantic) == 0 {
			fmt.Fprintf(w, "  none — no field's values shifted enough to be worth asking about\n\n")
		}
		for _, item := range a.Semantic {
			fmt.Fprintf(w, "  %s  %s\n", item.Request.Endpoint, item.Request.Path)
			fmt.Fprintf(w, "    flagged: %s\n", item.Request.Shift.Reason)
			fmt.Fprintf(w, "    samples: %d baseline, %d current\n", len(item.Request.Baseline), len(item.Request.Current))
			if item.Verdict == nil {
				fmt.Fprintf(w, "    (dry run — would ask the model)\n\n")
				continue
			}
			v := item.Verdict
			if !v.Changed {
				fmt.Fprintf(w, "    verdict: NO CHANGE (%s confidence) — %s\n\n", v.Confidence, v.Summary)
				continue
			}
			fmt.Fprintf(w, "    verdict: %s (%s confidence)\n", strings.ToUpper(string(v.Kind)), v.Confidence)
			fmt.Fprintf(w, "    %s\n", v.Summary)
			fmt.Fprintf(w, "    consequence: %s\n", v.Consequence)
			fmt.Fprintf(w, "    reasoning: %s\n\n", v.Reasoning)
		}
	}

	if cfg.impact {
		fmt.Fprintf(w, "impact of confirmed findings (%d)\n", len(a.Impact))
		fmt.Fprintf(w, "  what each statistically confirmed change breaks in %s\n\n", cfg.repo)
		if len(a.Impact) == 0 {
			fmt.Fprintf(w, "  none — the proxy reported no findings to assess\n\n")
		}
		for _, item := range a.Impact {
			fmt.Fprintf(w, "  %s  %s  %s\n", item.Finding.Endpoint, item.Finding.Path, item.Finding.Kind)
			fmt.Fprintf(w, "    candidate call sites: %d\n", item.Sites)
			if item.Verdict == nil {
				fmt.Fprintf(w, "    (dry run — would ask the model)\n\n")
				continue
			}
			v := item.Verdict
			if !v.Breaks {
				fmt.Fprintf(w, "    verdict: NOTHING BREAKS — %s\n\n", v.Summary)
				continue
			}
			fmt.Fprintf(w, "    verdict: BREAKS — %s\n", v.Summary)
			for _, site := range v.Affected {
				fmt.Fprintf(w, "      %s:%d\n", site.File, site.Line)
				fmt.Fprintf(w, "        why: %s\n", site.Why)
				fmt.Fprintf(w, "        fix: %s\n", site.Fix)
			}
			fmt.Fprintf(w, "\n    remediation:\n")
			for _, line := range strings.Split(v.Remediation, "\n") {
				fmt.Fprintf(w, "      %s\n", line)
			}
			fmt.Fprintln(w)
		}
	}
	return nil
}
