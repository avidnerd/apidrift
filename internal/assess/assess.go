// Package assess is the judgement layer: the part of apidrift that reasons
// about what a change *means*, using a language model, on top of what the
// statistics have already established.
//
// The division is the whole design, and it runs in one direction:
//
//   - The detector decides *whether* something changed. It is deterministic,
//     cheap, and measurable -- a two-proportion test with an effect-size gate
//     and a false-discovery-rate correction, run over every path on every
//     endpoint, thousands per window. Its false positive rate is a number you
//     can put in a table.
//   - This package decides *what a change means and what to do about it*. It is
//     none of those things: non-deterministic, comparatively expensive, and not
//     susceptible to a false-positive rate you can quote. So it runs only on
//     the handful of findings that survived the statistics, never on the
//     thousands of paths that did not.
//
// Putting a model inside the detector would have been the obvious way to "add
// AI", and it would have destroyed the one property that makes the tool
// credible. Putting it here buys the two things statistics genuinely cannot do:
// recognise that a field's *meaning* changed while its shape did not, and read
// the calling code to say what will actually break.
package assess

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/avidnerd/apidrift/internal/codesearch"
	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/valuesample"
)

// DefaultModel is the model used unless configured otherwise.
const DefaultModel = "claude-opus-5"

// DefaultMaxTokens is generous enough that a verdict is never truncated
// mid-sentence, and small enough to bound the cost of a bad prompt.
const DefaultMaxTokens = 8000

// ErrRefused reports that the model declined the request on policy grounds.
//
// Handled explicitly rather than through server-side fallbacks: this layer
// classifies numeric units and reads call sites, so a policy decline is not a
// failure mode it can realistically hit, and a refusal here should surface as a
// plain error rather than silently re-run on a substitute model.
var ErrRefused = errors.New("assess: the model declined the request")

// SemanticKind names what sort of meaning change was found.
type SemanticKind string

const (
	// SemanticNone means the values still mean the same thing.
	SemanticNone SemanticKind = "none"
	// SemanticUnit is a change of unit: cents to dollars, bytes to kilobytes.
	SemanticUnit SemanticKind = "unit_change"
	// SemanticScale is a change of scale or precision: seconds to milliseconds.
	SemanticScale SemanticKind = "scale_change"
	// SemanticEncoding is a change of representation: an epoch integer becoming
	// an RFC 3339 string, an ID space being renumbered.
	SemanticEncoding SemanticKind = "encoding_change"
	// SemanticRedefinition is a change in what the field counts: a total
	// becoming a subtotal, a status code being reused for something else.
	SemanticRedefinition SemanticKind = "redefinition"
)

// Confidence is how much weight to put on a verdict.
type Confidence string

const (
	ConfidenceLow    Confidence = "low"
	ConfidenceMedium Confidence = "medium"
	ConfidenceHigh   Confidence = "high"
)

// SemanticRequest asks whether the values at one path changed meaning.
//
// It is JSON-serialisable because it doubles as the wire format: the proxy
// serves the candidates it has selected, and the assess command consumes them.
// Having one type for "what the proxy found worth asking about" and "what the
// model is asked" removes a translation step that could drift.
type SemanticRequest struct {
	Endpoint endpoint.Key `json:"endpoint"`
	Path     string       `json:"path"`
	// Shift is the deterministic pre-filter's reason for asking. The model is
	// told what moved so it is judging a specific hypothesis rather than
	// hunting for one.
	Shift valuesample.Shift `json:"shift"`
	// Baseline and Current are value samples from the two windows.
	Baseline []valuesample.Value `json:"baseline"`
	Current  []valuesample.Value `json:"current"`
}

// SemanticVerdict is the answer.
type SemanticVerdict struct {
	// Changed reports whether the meaning changed. False is the expected
	// answer and carries no penalty; see the prompt.
	Changed bool `json:"changed"`
	// Kind classifies the change.
	Kind SemanticKind `json:"kind"`
	// Confidence is how sure the model is.
	Confidence Confidence `json:"confidence"`
	// Summary is one line for a report.
	Summary string `json:"summary"`
	// Reasoning is the argument for the verdict, including what would have
	// changed the answer.
	Reasoning string `json:"reasoning"`
	// Consequence is what breaks in a consumer if this is real.
	Consequence string `json:"consequence"`
}

// Finding converts a positive verdict into a detect.Finding, so semantic
// results flow through the same reporting path as statistical ones.
//
// The kind is TypeChanged and the evidence says plainly that this came from a
// model, because a reader has to be able to tell which findings rest on a
// measured rate and which rest on a judgement.
func (v *SemanticVerdict) Finding(req SemanticRequest) detect.Finding {
	severity := detect.SeverityHigh
	if v.Confidence == ConfidenceHigh {
		severity = detect.SeverityCritical
	}
	return detect.Finding{
		Endpoint: req.Endpoint,
		Path:     req.Path,
		Kind:     detect.TypeChanged,
		Severity: severity,
		Evidence: fmt.Sprintf("semantic (model-assessed, %s confidence): %s — %s",
			v.Confidence, v.Summary, v.Consequence),
	}
}

// ImpactRequest asks what a confirmed change breaks in a codebase.
type ImpactRequest struct {
	Endpoint endpoint.Key `json:"endpoint"`
	Path     string       `json:"path"`
	// Change is a human-readable description of what the detector found.
	Change string `json:"change"`
	// Sites are candidate usages from package codesearch.
	Sites []codesearch.Site `json:"sites"`
}

// AffectedSite is one call site the model judged genuinely affected.
type AffectedSite struct {
	File string `json:"file"`
	Line int    `json:"line"`
	// Why explains what this code does with the field and how it breaks.
	Why string `json:"why"`
	// Fix is the concrete change to make here.
	Fix string `json:"fix"`
}

// ImpactVerdict is the answer.
type ImpactVerdict struct {
	// Breaks reports whether any real usage is affected.
	Breaks bool `json:"breaks"`
	// Summary is one line for a report.
	Summary string `json:"summary"`
	// Affected lists the sites that genuinely matter, which is usually far
	// fewer than the candidates handed in.
	Affected []AffectedSite `json:"affected"`
	// Remediation describes the overall fix, suitable for a pull request body.
	Remediation string `json:"remediation"`
}

// Assessor is the judgement layer's interface.
//
// An interface rather than a concrete client so the CLI, the tests and any
// future offline mode all depend on the behaviour rather than on the SDK. The
// tests use Fake and never make a network call.
type Assessor interface {
	// Semantic judges whether values changed meaning.
	Semantic(ctx context.Context, req SemanticRequest) (*SemanticVerdict, error)
	// Impact judges what a change breaks in a codebase.
	Impact(ctx context.Context, req ImpactRequest) (*ImpactVerdict, error)
	// Patch proposes concrete edits that adapt the code to a change.
	Patch(ctx context.Context, req PatchRequest) (*PatchVerdict, error)
}

// Config configures a Client.
type Config struct {
	// APIKey is the Anthropic API key. Empty means the SDK resolves
	// credentials itself, from ANTHROPIC_API_KEY or a logged-in profile.
	APIKey string
	// Model is the model id. Empty means DefaultModel.
	Model string
	// Effort tunes how hard the model works. Empty means high: these are
	// judgement calls on a handful of findings, and the cheap thing to do is
	// not run them at all rather than run them badly.
	Effort anthropic.OutputConfigEffort
	// MaxTokens caps one response. Zero means DefaultMaxTokens.
	MaxTokens int64
}

// Client is the real Assessor, backed by the Anthropic API.
type Client struct {
	api       anthropic.Client
	model     string
	effort    anthropic.OutputConfigEffort
	maxTokens int64
}

// Client implements Assessor.
var _ Assessor = (*Client)(nil)

// New returns a Client.
func New(cfg Config) *Client {
	var opts []option.RequestOption
	if cfg.APIKey != "" {
		opts = append(opts, option.WithAPIKey(cfg.APIKey))
	}

	c := &Client{
		api:       anthropic.NewClient(opts...),
		model:     cfg.Model,
		effort:    cfg.Effort,
		maxTokens: cfg.MaxTokens,
	}
	if c.model == "" {
		c.model = DefaultModel
	}
	if c.effort == "" {
		c.effort = anthropic.OutputConfigEffortHigh
	}
	if c.maxTokens == 0 {
		c.maxTokens = DefaultMaxTokens
	}
	return c
}

// Semantic judges whether the values at a path changed meaning.
func (c *Client) Semantic(ctx context.Context, req SemanticRequest) (*SemanticVerdict, error) {
	var out SemanticVerdict
	err := c.ask(ctx, semanticSystem, renderSemantic(req), semanticSchema, &out)
	if err != nil {
		return nil, err
	}
	if !out.Changed {
		out.Kind = SemanticNone
	}
	return &out, nil
}

// Impact judges what a confirmed change breaks.
func (c *Client) Impact(ctx context.Context, req ImpactRequest) (*ImpactVerdict, error) {
	var out ImpactVerdict
	if err := c.ask(ctx, impactSystem, renderImpact(req), impactSchema, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ask makes one structured-output request and decodes it into out.
func (c *Client) ask(ctx context.Context, system, user string, schema map[string]any, out any) error {
	adaptive := anthropic.ThinkingConfigAdaptiveParam{}

	resp, err := c.api.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: c.maxTokens,
		System:    []anthropic.TextBlockParam{{Text: system}},
		Thinking:  anthropic.ThinkingConfigParamUnion{OfAdaptive: &adaptive},
		OutputConfig: anthropic.OutputConfigParam{
			Effort: c.effort,
			Format: anthropic.JSONOutputFormatParam{Schema: schema},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(user)),
		},
	})
	if err != nil {
		return fmt.Errorf("assess: calling the model: %w", err)
	}
	if resp.StopReason == anthropic.StopReasonRefusal {
		return fmt.Errorf("%w: %s", ErrRefused, resp.StopDetails.Explanation)
	}

	var text strings.Builder
	for _, block := range resp.Content {
		if tb, ok := block.AsAny().(anthropic.TextBlock); ok {
			text.WriteString(tb.Text)
		}
	}
	if text.Len() == 0 {
		return errors.New("assess: the model returned no content")
	}
	if err := json.Unmarshal([]byte(text.String()), out); err != nil {
		return fmt.Errorf("assess: decoding the verdict: %w", err)
	}
	return nil
}

// Edit is one concrete change to one file, expressed as an exact replacement.
//
// A replacement rather than a line number or a diff, deliberately. Line numbers
// go stale the moment an earlier edit lands, and a model-authored unified diff
// has to be parsed and applied with fuzz. An exact old-for-new swap can be
// verified before it is written: if Old does not appear in the file exactly
// once, the edit is refused rather than guessed at.
type Edit struct {
	File string `json:"file"`
	// Old is the text to replace. It must appear exactly once in the file, so
	// it needs enough surrounding context to be unique.
	Old string `json:"old"`
	// New is what replaces it.
	New string `json:"new"`
	// Why explains the change, for the pull request body.
	Why string `json:"why"`
}

// PatchRequest asks for the edits that fix one confirmed change.
type PatchRequest struct {
	Endpoint endpoint.Key      `json:"endpoint"`
	Path     string            `json:"path"`
	Change   string            `json:"change"`
	Sites    []codesearch.Site `json:"sites"`
	// Files is the full text of each file that has a candidate site, keyed by
	// path. Excerpts are enough to judge whether a site matters; writing a
	// correct edit needs the surrounding code.
	Files map[string]string `json:"-"`
}

// PatchVerdict is the answer.
type PatchVerdict struct {
	// Edits are the changes to make. Empty is a valid answer.
	Edits []Edit `json:"edits"`
	// Title is a one-line pull request title.
	Title string `json:"title"`
	// Body explains the upstream change and the fix, for the pull request.
	Body string `json:"body"`
	// Unfixable describes anything that needs a human, so a green pull request
	// does not imply the whole problem is handled.
	Unfixable string `json:"unfixable"`
}

// Patch asks for concrete edits that adapt the caller's code to a change.
func (c *Client) Patch(ctx context.Context, req PatchRequest) (*PatchVerdict, error) {
	var out PatchVerdict
	if err := c.ask(ctx, patchSystem, renderPatch(req), patchSchema, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Provider names a backend for the judgement layer.
type Provider string

const (
	// ProviderAuto picks Anthropic when ANTHROPIC_API_KEY is set, otherwise
	// OpenRouter when OPENROUTER_API_KEY is set.
	ProviderAuto Provider = "auto"
	// ProviderAnthropic uses the Anthropic API directly.
	ProviderAnthropic Provider = "anthropic"
	// ProviderOpenRouter routes through OpenRouter.
	ProviderOpenRouter Provider = "openrouter"
)

// ErrNoCredentials reports that neither provider has a key configured.
var ErrNoCredentials = errors.New(
	"assess: no credentials found; set ANTHROPIC_API_KEY, or OPENROUTER_API_KEY to route through OpenRouter")

// NewAssessor returns the Assessor for a provider.
//
// ProviderAuto prefers Anthropic, because that is what the prompts were written
// and tuned against. It falls back to OpenRouter rather than failing, so a
// single exported key is enough to make the layer work either way.
func NewAssessor(p Provider, cfg Config) (Assessor, error) {
	switch p {
	case ProviderAnthropic:
		return New(cfg), nil
	case ProviderOpenRouter:
		o := NewOpenRouter(cfg)
		if o.key == "" {
			return nil, ErrNoOpenRouterKey
		}
		return o, nil
	case ProviderAuto, "":
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			return New(cfg), nil
		}
		if os.Getenv("OPENROUTER_API_KEY") != "" {
			return NewOpenRouter(cfg), nil
		}
		// Neither key is exported. The Anthropic SDK can still resolve a
		// logged-in profile from disk, so it gets the benefit of the doubt and
		// reports its own, more specific error if it cannot.
		return New(cfg), nil
	}
	return nil, fmt.Errorf("assess: unknown provider %q, want anthropic or openrouter", p)
}

// Describe names the provider and model an Assessor will use, for the line the
// CLI prints before spending anything.
func Describe(a Assessor) string {
	switch v := a.(type) {
	case *Client:
		return "anthropic / " + v.model
	case *OpenRouter:
		return "openrouter / " + v.model
	case *Fake:
		return "dry run (no model)"
	}
	return "unknown"
}
