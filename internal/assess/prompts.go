package assess

import (
	"fmt"
	"sort"
	"strings"

	"github.com/avidnerd/apidrift/internal/valuesample"
)

// The prompts below are written against one specific hazard, and it is worth
// naming because it shapes every line of them.
//
// A model asked "did the meaning of this field change?" is under enormous pull
// to answer yes. It has been handed two samples that visibly differ and an
// explicit hypothesis; agreeing is the path of least resistance, and it reads
// as helpful. But a semantic detector with a high false positive rate is
// exactly what this whole project exists to avoid -- the statistical side
// spends four separate gates suppressing noise, and a chatty model bolted on
// the end would undo all of it.
//
// So both prompts do the same four things: state the base rate, name the
// mundane explanation and require it to be ruled out, make "no" an expected
// and unpenalised answer, and demand a specific mechanism before high
// confidence. Asking for the reasoning is not decoration either -- requiring
// the model to say what would have changed its mind is what stops "changed:
// true" being a reflex.

const semanticSystem = `You are the semantic layer of apidrift, a tool that watches the JSON responses of third-party APIs and reports when they change.

apidrift's statistical detector already handles everything structural: fields appearing and disappearing, types flipping, nulls arriving, new enum values. It measures those, and it is good at them.

You handle the one thing it is blind to. A field that switches from cents to dollars keeps its type and its presence rate. Nothing structural moves. The statistics cannot see it, and no amount of tuning will make them. That is your job and only your job.

## What you are given

Two samples of the actual values seen at one JSON path: one from a baseline window, one from a current window. Plus the reason a cheap deterministic filter thought they were worth a second look.

## The base rate

Most of the time, nothing has changed meaning. Values move for ordinary reasons all the time:

- Traffic mix shifted. A different set of customers, regions, or plan tiers was sampled.
- The samples are small. Two random draws from the same distribution differ.
- A seasonal or business effect. Order values rise in December.
- The reservoir happened to catch outliers on one side.

Every one of these produces exactly the signal you are looking at. Rule them out before you conclude anything.

## The bar

Answer changed: true only when you can name a specific mechanism -- a unit, a scale, an encoding, a redefinition -- that explains the difference better than "the traffic moved". A ratio near a suspicious round number (100 for cents to dollars, 1000 for seconds to milliseconds or bytes to kilobytes) is real evidence. A ratio of 2.3 is almost certainly business variation, not a unit change.

Reserve high confidence for cases where the mechanism is close to unmistakable: the ratio sits on a round number, the sample is not tiny, and no mundane explanation fits.

## Answering "no"

"No, this looks like ordinary variation" is the expected answer and the correct one most of the time. It is not a failure to find something. A false alarm here costs an engineer an hour and costs the tool its credibility; a miss costs one window, because the next window asks again.

In your reasoning, say explicitly what would have changed your answer. If you cannot name that, you have not reasoned about it.`

const impactSystem = `You are the impact layer of apidrift, a tool that watches the JSON responses of third-party APIs and reports when they change.

A change has been confirmed on a specific JSON path. You are given candidate call sites found by a plain text search of the user's repository, and your job is to say which of them actually break, and how to fix them.

## The candidates are noisy on purpose

The search matched a field name in several spellings across every source file. It has no idea what any of those matches mean. Expect the majority to be irrelevant:

- comments and documentation
- a local variable that happens to share the name
- test fixtures and mock data
- the deserialiser or model definition, which usually needs no change at all
- an unrelated field of the same name on a different object

Discard those without ceremony. A short, correct list is the deliverable. Listing everything the grep found is worse than useless -- it hands the reader back the same noise they already had, with an implied endorsement.

## What counts as affected

Code that reads this field and would now behave differently: arithmetic on a value whose units changed, a null dereference where a field became nullable, a lookup of a key that is gone, a switch that does not handle a new enum value.

Code that merely mentions the name is not affected.

## The fix

For each genuinely affected site, say concretely what to change -- enough that a reviewer could apply it without re-deriving your reasoning. Then write an overall remediation suitable as a pull request description: what changed upstream, what breaks, what the patch does.

If nothing genuinely breaks, say so. breaks: false with an empty list is a good answer when it is the true one, and far more useful than a speculative list.`

// semanticSchema constrains the semantic verdict.
var semanticSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"changed", "kind", "confidence", "summary", "reasoning", "consequence"},
	"properties": map[string]any{
		"changed": map[string]any{
			"type":        "boolean",
			"description": "True only if the values changed meaning, not merely magnitude.",
		},
		"kind": map[string]any{
			"type": "string",
			"enum": []string{
				string(SemanticNone), string(SemanticUnit), string(SemanticScale),
				string(SemanticEncoding), string(SemanticRedefinition),
			},
		},
		"confidence": map[string]any{
			"type": "string",
			"enum": []string{string(ConfidenceLow), string(ConfidenceMedium), string(ConfidenceHigh)},
		},
		"summary": map[string]any{
			"type":        "string",
			"description": "One line for a report. If nothing changed, say what the values look like instead.",
		},
		"reasoning": map[string]any{
			"type":        "string",
			"description": "The argument, including which mundane explanations were ruled out and what would have changed the answer.",
		},
		"consequence": map[string]any{
			"type":        "string",
			"description": "What breaks in a consumer if this is real. Empty when nothing changed.",
		},
	},
}

// impactSchema constrains the impact verdict.
var impactSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"breaks", "summary", "affected", "remediation"},
	"properties": map[string]any{
		"breaks": map[string]any{"type": "boolean"},
		"summary": map[string]any{
			"type":        "string",
			"description": "One line: what breaks, or that nothing does.",
		},
		"affected": map[string]any{
			"type":        "array",
			"description": "Only call sites that genuinely break. Empty is a valid and often correct answer.",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"file", "line", "why", "fix"},
				"properties": map[string]any{
					"file": map[string]any{"type": "string"},
					"line": map[string]any{"type": "integer"},
					"why":  map[string]any{"type": "string"},
					"fix":  map[string]any{"type": "string"},
				},
			},
		},
		"remediation": map[string]any{
			"type":        "string",
			"description": "An overall description of the fix, suitable as a pull request body.",
		},
	},
}

// renderSemantic builds the user message for a semantic judgement.
func renderSemantic(req SemanticRequest) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Endpoint: %s\n", req.Endpoint)
	fmt.Fprintf(&b, "JSON path: %s\n\n", req.Path)
	fmt.Fprintf(&b, "Why this was flagged for review: %s\n", req.Shift.Reason)
	if req.Shift.MagnitudeRatio != 0 {
		fmt.Fprintf(&b, "Median magnitude ratio (current / baseline): %.4g\n", req.Shift.MagnitudeRatio)
	}

	fmt.Fprintf(&b, "\nBaseline window — %d sampled values:\n", len(req.Baseline))
	writeValues(&b, req.Baseline)
	fmt.Fprintf(&b, "\nCurrent window — %d sampled values:\n", len(req.Current))
	writeValues(&b, req.Current)

	b.WriteString("\nThese are uniform random samples of every value seen at this path in each window, so they are representative but small.\n")
	b.WriteString("\nDid the values at this path change meaning?")
	return b.String()
}

// writeValues lists a sample compactly, with its JSON kind, so the written form
// of a number stays visible -- 1000 and 10.00 are the same quantity and
// different serialisations, and the difference is exactly what may have moved.
func writeValues(b *strings.Builder, vs []valuesample.Value) {
	for _, v := range vs {
		fmt.Fprintf(b, "  %-6s %s\n", v.Kind, v.Raw)
	}
}

// renderImpact builds the user message for an impact judgement.
func renderImpact(req ImpactRequest) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Endpoint: %s\n", req.Endpoint)
	fmt.Fprintf(&b, "JSON path: %s\n", req.Path)
	fmt.Fprintf(&b, "Confirmed change: %s\n\n", req.Change)

	if len(req.Sites) == 0 {
		b.WriteString("The repository search found no candidate usages at all.\n")
		b.WriteString("\nDoes anything break?")
		return b.String()
	}

	fmt.Fprintf(&b, "Candidate call sites (%d), from a plain text search — most are probably irrelevant:\n\n", len(req.Sites))
	for _, s := range req.Sites {
		fmt.Fprintf(&b, "--- %s:%d (matched %q)\n", s.File, s.Line, s.Match)
		for _, line := range s.Excerpt {
			fmt.Fprintf(&b, "    %s\n", line)
		}
		b.WriteString("\n")
	}

	b.WriteString("Which of these genuinely break, and what is the fix?")
	return b.String()
}

// The four helpers below expose the prompts and their rendering to this
// package's tests. The prompts are behaviour, not decoration -- the base-rate
// and discard-the-noise instructions are the only thing standing between this
// layer and a false-positive generator -- so they are tested like behaviour.

// SemanticSystemPrompt returns the system prompt used for semantic judgements.
func SemanticSystemPrompt() string { return semanticSystem }

// ImpactSystemPrompt returns the system prompt used for impact judgements.
func ImpactSystemPrompt() string { return impactSystem }

// RenderSemantic returns the user message for a semantic judgement.
func RenderSemantic(req SemanticRequest) string { return renderSemantic(req) }

// RenderImpact returns the user message for an impact judgement.
func RenderImpact(req ImpactRequest) string { return renderImpact(req) }

const patchSystem = `You are the remediation layer of apidrift. A third-party API has changed, apidrift has confirmed it statistically, and your job is to write the code changes that adapt this repository to it.

## What you produce

Exact text replacements. For each edit, give the file, the text to replace, and what to replace it with. The old text must appear exactly once in that file, so include enough surrounding lines to be unambiguous. An edit whose old text is missing or appears twice will be refused rather than applied, and a refused edit is worse than one you never proposed.

Match the file's existing indentation and style exactly. The replacement is written into the file verbatim.

## The bar for editing at all

Most candidate sites do not need changing. The search that found them matched a field name in several spellings across every source file, so expect comments, test fixtures, unrelated variables of the same name, and the deserialiser itself. Only edit code whose behaviour is actually wrong now.

Prefer the smallest change that makes the code correct. Do not reformat, rename, refactor surrounding code, or add error handling that was not there before. Somebody has to review this, and a diff full of unrelated changes gets rejected whether or not the fix inside it was right.

## When you cannot fix it

Return no edits and explain why in the "unfixable" field. Cases where that is the right answer:

- The correct behaviour depends on a product decision. If a field is gone, whether to fall back to a default, drop the record, or fail loudly is not yours to decide.
- The fix belongs somewhere the search did not reach, like a database schema or another service.
- The change needs a data migration as well as a code change.

An honest "this needs a person, here is why" is a good outcome. Inventing a plausible-looking edit for something you do not have the context to decide is not.

## The pull request text

Write the title and body for somebody who has not seen the alert. Say what the upstream changed, what breaks because of it, and what the patch does. Keep it short. If anything is left unfixed, say so in the body rather than only in the field, because that is what a reviewer reads.`

// patchSchema constrains the remediation verdict.
var patchSchema = map[string]any{
	"type":                 "object",
	"additionalProperties": false,
	"required":             []string{"edits", "title", "body", "unfixable"},
	"properties": map[string]any{
		"edits": map[string]any{
			"type":        "array",
			"description": "Exact text replacements. Empty is valid when nothing needs changing or a person is required.",
			"items": map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"file", "old", "new", "why"},
				"properties": map[string]any{
					"file": map[string]any{"type": "string"},
					"old": map[string]any{
						"type":        "string",
						"description": "Text that appears exactly once in the file, with enough context to be unique.",
					},
					"new": map[string]any{"type": "string"},
					"why": map[string]any{"type": "string"},
				},
			},
		},
		"title":     map[string]any{"type": "string"},
		"body":      map[string]any{"type": "string"},
		"unfixable": map[string]any{"type": "string", "description": "What still needs a human, or empty."},
	},
}

// renderPatch builds the user message for a remediation request.
func renderPatch(req PatchRequest) string {
	var b strings.Builder

	fmt.Fprintf(&b, "Endpoint: %s\n", req.Endpoint)
	fmt.Fprintf(&b, "JSON path: %s\n", req.Path)
	fmt.Fprintf(&b, "Confirmed change: %s\n\n", req.Change)

	if len(req.Sites) == 0 {
		b.WriteString("The repository search found no candidate usages.\n\nIs there anything to change?")
		return b.String()
	}

	fmt.Fprintf(&b, "Candidate sites (%d), from a plain text search. Most are probably irrelevant:\n\n", len(req.Sites))
	for _, s := range req.Sites {
		fmt.Fprintf(&b, "  %s:%d (matched %q)\n", s.File, s.Line, s.Match)
	}

	if len(req.Files) > 0 {
		b.WriteString("\nFull text of the files those sites are in:\n")
		for _, name := range sortedFileNames(req.Files) {
			fmt.Fprintf(&b, "\n===== %s =====\n%s\n", name, req.Files[name])
		}
	}

	b.WriteString("\nWhat needs to change?")
	return b.String()
}

// sortedFileNames keeps the prompt stable between runs, so the same input does
// not produce a different cache key each time.
func sortedFileNames(files map[string]string) []string {
	out := make([]string, 0, len(files))
	for k := range files {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// PatchSystemPrompt returns the system prompt used for remediation.
func PatchSystemPrompt() string { return patchSystem }

// RenderPatch returns the user message for a remediation request.
func RenderPatch(req PatchRequest) string { return renderPatch(req) }
