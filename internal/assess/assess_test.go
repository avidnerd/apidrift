package assess_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/avidnerd/apidrift/internal/assess"
	"github.com/avidnerd/apidrift/internal/codesearch"
	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/schema"
	"github.com/avidnerd/apidrift/internal/valuesample"
)

// Every test here runs against the Fake. Nothing in this package's test suite
// makes a network call or needs an API key, which is what keeps the layer
// around the model -- candidate selection, prompt construction, reporting --
// under test in CI.

var key = endpoint.Key{Method: "GET", Template: "/v1/charges/{id}", StatusClass: 2}

func vals(kind schema.Kind, raws ...string) []valuesample.Value {
	out := make([]valuesample.Value, 0, len(raws))
	for _, r := range raws {
		out = append(out, valuesample.Value{Kind: kind, Raw: r})
	}
	return out
}

func TestFakeRecordsRequests(t *testing.T) {
	f := &assess.Fake{}

	req := assess.SemanticRequest{
		Endpoint: key,
		Path:     "amount",
		Shift:    valuesample.Shift{Reason: "magnitude moved 1/100x", MagnitudeRatio: 0.01},
		Baseline: vals(schema.KindInt, "2000", "1500"),
		Current:  vals(schema.KindInt, "20", "15"),
	}
	got, err := f.Semantic(context.Background(), req)
	if err != nil {
		t.Fatalf("Semantic: %v", err)
	}
	if got.Changed {
		t.Error("the zero Fake verdict reports a change; it should default to nothing changed")
	}
	if calls := f.SemanticCalls(); len(calls) != 1 || calls[0].Path != "amount" {
		t.Errorf("SemanticCalls() = %+v, want the one request", calls)
	}
}

func TestFakeScripting(t *testing.T) {
	f := &assess.Fake{
		SemanticFn: func(req assess.SemanticRequest) (*assess.SemanticVerdict, error) {
			return &assess.SemanticVerdict{
				Changed:    true,
				Kind:       assess.SemanticUnit,
				Confidence: assess.ConfidenceHigh,
				Summary:    "amount moved from cents to dollars",
			}, nil
		},
		ImpactFn: func(req assess.ImpactRequest) (*assess.ImpactVerdict, error) {
			return &assess.ImpactVerdict{
				Breaks:  true,
				Summary: "two call sites do arithmetic on the raw value",
				Affected: []assess.AffectedSite{
					{File: "billing/total.go", Line: 42, Why: "sums cents", Fix: "divide by 100"},
				},
				Remediation: "convert at the boundary",
			}, nil
		},
	}

	sv, err := f.Semantic(context.Background(), assess.SemanticRequest{Path: "amount"})
	if err != nil {
		t.Fatalf("Semantic: %v", err)
	}
	if sv.Kind != assess.SemanticUnit {
		t.Errorf("Kind = %q, want %q", sv.Kind, assess.SemanticUnit)
	}

	iv, err := f.Impact(context.Background(), assess.ImpactRequest{Path: "amount"})
	if err != nil {
		t.Fatalf("Impact: %v", err)
	}
	if len(iv.Affected) != 1 || iv.Affected[0].Line != 42 {
		t.Errorf("Affected = %+v, want one site at line 42", iv.Affected)
	}
	if len(f.ImpactCalls()) != 1 {
		t.Error("the impact call was not recorded")
	}
}

// TestSemanticVerdictBecomesAFinding covers the join back into the normal
// reporting path, and the promise that a reader can always tell a
// model-assessed finding from a measured one.
func TestSemanticVerdictBecomesAFinding(t *testing.T) {
	req := assess.SemanticRequest{Endpoint: key, Path: "amount"}

	tests := []struct {
		name         string
		verdict      assess.SemanticVerdict
		wantSeverity detect.Severity
	}{
		{
			name: "high confidence is critical",
			verdict: assess.SemanticVerdict{
				Changed: true, Kind: assess.SemanticUnit, Confidence: assess.ConfidenceHigh,
				Summary: "cents to dollars", Consequence: "totals are 100x too small",
			},
			wantSeverity: detect.SeverityCritical,
		},
		{
			name: "medium confidence is high, not critical",
			verdict: assess.SemanticVerdict{
				Changed: true, Kind: assess.SemanticScale, Confidence: assess.ConfidenceMedium,
				Summary: "possible scale change", Consequence: "durations may be wrong",
			},
			wantSeverity: detect.SeverityHigh,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.verdict.Finding(req)

			if got.Endpoint != key || got.Path != "amount" {
				t.Errorf("finding identity = %v %q, want the request's", got.Endpoint, got.Path)
			}
			if got.Severity != tt.wantSeverity {
				t.Errorf("Severity = %v, want %v", got.Severity, tt.wantSeverity)
			}
			if !strings.Contains(got.Evidence, "model-assessed") {
				t.Errorf("Evidence = %q; it must say the verdict came from a model, not a measured rate", got.Evidence)
			}
			if !strings.Contains(got.Evidence, string(tt.verdict.Confidence)) {
				t.Errorf("Evidence = %q, want it to carry the confidence", got.Evidence)
			}
			if !strings.Contains(got.Evidence, tt.verdict.Consequence) {
				t.Errorf("Evidence = %q, want it to say what breaks", got.Evidence)
			}
		})
	}
}

// TestSchemasAreValidJSONSchema guards the structured-output contract: a
// malformed schema is rejected by the API at request time, which is a long way
// from where the mistake was made.
func TestSchemasAreValidJSONSchema(t *testing.T) {
	for name, verdict := range map[string]any{
		"semantic": assess.SemanticVerdict{},
		"impact":   assess.ImpactVerdict{},
	} {
		t.Run(name, func(t *testing.T) {
			// Round-tripping the Go type through JSON proves the field names
			// the model is asked for are the ones the decoder reads.
			raw, err := json.Marshal(verdict)
			if err != nil {
				t.Fatalf("marshalling %s verdict: %v", name, err)
			}
			var back map[string]any
			if err := json.Unmarshal(raw, &back); err != nil {
				t.Fatalf("unmarshalling %s verdict: %v", name, err)
			}
			if len(back) == 0 {
				t.Errorf("%s verdict has no JSON fields", name)
			}
		})
	}
}

// TestVerdictJSONTagsMatchTheSchema is the specific failure this catches: a
// renamed Go field that silently stops being populated, because the model is
// still asked for the old name and the decoder quietly finds nothing.
func TestVerdictJSONTagsMatchTheSchema(t *testing.T) {
	semantic := `{"changed":true,"kind":"unit_change","confidence":"high",
	              "summary":"s","reasoning":"r","consequence":"c"}`
	var sv assess.SemanticVerdict
	if err := json.Unmarshal([]byte(semantic), &sv); err != nil {
		t.Fatalf("decoding a semantic verdict: %v", err)
	}
	if !sv.Changed || sv.Kind != assess.SemanticUnit || sv.Confidence != assess.ConfidenceHigh {
		t.Errorf("decoded verdict = %+v, want every field populated", sv)
	}
	if sv.Summary != "s" || sv.Reasoning != "r" || sv.Consequence != "c" {
		t.Errorf("decoded verdict = %+v, want every text field populated", sv)
	}

	impact := `{"breaks":true,"summary":"s","remediation":"r",
	            "affected":[{"file":"a.go","line":7,"why":"w","fix":"f"}]}`
	var iv assess.ImpactVerdict
	if err := json.Unmarshal([]byte(impact), &iv); err != nil {
		t.Fatalf("decoding an impact verdict: %v", err)
	}
	if !iv.Breaks || iv.Summary != "s" || iv.Remediation != "r" {
		t.Errorf("decoded verdict = %+v, want every field populated", iv)
	}
	if len(iv.Affected) != 1 || iv.Affected[0].File != "a.go" || iv.Affected[0].Line != 7 {
		t.Errorf("Affected = %+v, want the one site decoded", iv.Affected)
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	// Constructing a client must not require credentials; only calling it does.
	if c := assess.New(assess.Config{APIKey: "test-key-not-used"}); c == nil {
		t.Fatal("New returned nil")
	}
	if c := assess.New(assess.Config{}); c == nil {
		t.Fatal("New with an empty config returned nil; credentials resolve at call time")
	}
}

// TestPromptsStateTheBaseRate is a test of the prompt itself, which is as much
// a part of this package's behaviour as its code.
//
// The hazard it guards is specific: a model handed two visibly different
// samples and an explicit hypothesis is under strong pull to agree. If these
// instructions are ever edited away, the semantic layer becomes a false-positive
// generator and the statistical work upstream is wasted.
func TestPromptsStateTheBaseRate(t *testing.T) {
	f := &assess.Fake{}
	_, _ = f.Semantic(context.Background(), assess.SemanticRequest{Path: "amount"})

	prompt := assess.SemanticSystemPrompt()
	for _, want := range []string{
		"Most of the time, nothing has changed meaning",
		"Traffic mix shifted",
		"expected answer",
		"what would have changed your answer",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the semantic prompt no longer contains %q; without it the layer will over-report", want)
		}
	}

	impact := assess.ImpactSystemPrompt()
	for _, want := range []string{
		"noisy on purpose",
		"Discard those without ceremony",
		"breaks: false with an empty list is a good answer",
	} {
		if !strings.Contains(impact, want) {
			t.Errorf("the impact prompt no longer contains %q; without it the layer will echo the grep back", want)
		}
	}
}

// TestRenderedPromptCarriesTheEvidence checks the model is given what it needs
// to reason: both samples, the written form of each value, and the ratio.
func TestRenderedPromptCarriesTheEvidence(t *testing.T) {
	req := assess.SemanticRequest{
		Endpoint: key,
		Path:     "amount",
		Shift:    valuesample.Shift{Reason: "the median magnitude moved 1/100x", MagnitudeRatio: 0.01},
		Baseline: vals(schema.KindInt, "2000", "1500", "3000"),
		Current:  vals(schema.KindInt, "20", "15", "30"),
	}

	got := assess.RenderSemantic(req)
	for _, want := range []string{
		"GET /v1/charges/{id} 2xx", "amount",
		"the median magnitude moved 1/100x", "0.01",
		"2000", "1500", "20", "15",
		"Baseline window", "Current window",
		"uniform random samples",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the rendered prompt is missing %q:\n%s", want, got)
		}
	}
}

func TestRenderedImpactPromptHandlesNoCandidates(t *testing.T) {
	got := assess.RenderImpact(assess.ImpactRequest{
		Endpoint: key, Path: "legacy_id", Change: "field removed",
	})
	if !strings.Contains(got, "no candidate usages") {
		t.Errorf("with no call sites the prompt must say so plainly:\n%s", got)
	}

	withSites := assess.RenderImpact(assess.ImpactRequest{
		Endpoint: key, Path: "legacy_id", Change: "field removed",
		Sites: []codesearch.Site{
			{File: "billing/total.go", Line: 42, Match: "legacyID", Excerpt: []string{"x := c.legacyID"}},
		},
	})
	for _, want := range []string{"billing/total.go:42", "legacyID", "x := c.legacyID", "probably irrelevant"} {
		if !strings.Contains(withSites, want) {
			t.Errorf("the rendered impact prompt is missing %q:\n%s", want, withSites)
		}
	}
}

// TestNewAssessorPicksAProvider covers the selection rule, including the
// fallback that lets one exported key be enough either way.
func TestNewAssessorPicksAProvider(t *testing.T) {
	tests := []struct {
		name      string
		provider  assess.Provider
		anthropic string
		openroute string
		want      string
		wantErr   bool
	}{
		{name: "explicit anthropic", provider: assess.ProviderAnthropic, want: "anthropic / "},
		{name: "explicit openrouter", provider: assess.ProviderOpenRouter, openroute: "or-key", want: "openrouter / "},
		{name: "openrouter without a key is an error", provider: assess.ProviderOpenRouter, wantErr: true},
		{name: "auto prefers anthropic", provider: assess.ProviderAuto, anthropic: "sk-key", openroute: "or-key", want: "anthropic / "},
		{name: "auto falls back to openrouter", provider: assess.ProviderAuto, openroute: "or-key", want: "openrouter / "},
		{name: "empty provider means auto", provider: "", openroute: "or-key", want: "openrouter / "},
		{name: "an unknown provider is an error", provider: "gemini", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ANTHROPIC_API_KEY", tt.anthropic)
			t.Setenv("OPENROUTER_API_KEY", tt.openroute)

			got, err := assess.NewAssessor(tt.provider, assess.Config{})
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if d := assess.Describe(got); !strings.HasPrefix(d, tt.want) {
				t.Errorf("Describe() = %q, want it to start with %q", d, tt.want)
			}
		})
	}
}

// TestOpenRouterModelDefaulting: OpenRouter needs a vendor-prefixed name, so a
// bare Anthropic-style id would 404 rather than resolve.
func TestOpenRouterModelDefaulting(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", assess.DefaultOpenRouterModel},
		{"claude-opus-5", assess.DefaultOpenRouterModel},
		{"anthropic/claude-opus-4.5", "anthropic/claude-opus-4.5"},
		{"openai/gpt-5", "openai/gpt-5"},
		{"google/gemini-2.5-pro", "google/gemini-2.5-pro"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := assess.Describe(assess.NewOpenRouter(assess.Config{APIKey: "k", Model: tt.in}))
			if want := "openrouter / " + tt.want; got != want {
				t.Errorf("Describe() = %q, want %q", got, want)
			}
		})
	}
}

// TestOpenRouterSendsTheOpenAIShape checks the wire format against a stub, since
// OpenRouter speaks OpenAI's chat API rather than the Anthropic one and that
// difference is the whole reason this client exists.
func TestOpenRouterSendsTheOpenAIShape(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if auth := r.Header.Get("Authorization"); auth != "Bearer test-key" {
			t.Errorf("Authorization = %q, want a bearer token", auth)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"{\"changed\":false,\"kind\":\"none\",\"confidence\":\"high\",\"summary\":\"s\",\"reasoning\":\"r\",\"consequence\":\"\"}"}}]}`)
	}))
	defer srv.Close()

	o := assess.NewOpenRouterAt(srv.URL, assess.Config{APIKey: "test-key"})
	v, err := o.Semantic(context.Background(), assess.SemanticRequest{Path: "amount"})
	if err != nil {
		t.Fatalf("Semantic: %v", err)
	}
	if v.Changed {
		t.Error("Changed = true, want the stubbed false")
	}

	if _, ok := got["messages"]; !ok {
		t.Error("request has no messages array; this is the OpenAI shape, not Anthropic's")
	}
	rf, ok := got["response_format"].(map[string]any)
	if !ok {
		t.Fatal("request has no response_format; structured output would not be enforced")
	}
	if rf["type"] != "json_schema" {
		t.Errorf("response_format.type = %v, want json_schema", rf["type"])
	}
	if _, ok := got["output_config"]; ok {
		t.Error("request carries output_config, which is Anthropic-only and OpenRouter will reject")
	}
}

func TestOpenRouterSurfacesErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantErr string
	}{
		{"error in a 200 body", 200, `{"error":{"message":"no credits"}}`, "no credits"},
		{"http error", 401, `{"error":{"message":"invalid key"}}`, "invalid key"},
		{"html error page", 502, `<html>bad gateway</html>`, "not JSON"},
		{"no choices", 200, `{"choices":[]}`, "no choices"},
		{"content is not the schema", 200, `{"choices":[{"message":{"content":"sorry!"}}]}`, "decoding the verdict"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer srv.Close()

			o := assess.NewOpenRouterAt(srv.URL, assess.Config{APIKey: "k"})
			_, err := o.Semantic(context.Background(), assess.SemanticRequest{Path: "a"})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestOpenRouterWithoutAKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	o := assess.NewOpenRouter(assess.Config{})
	if _, err := o.Semantic(context.Background(), assess.SemanticRequest{Path: "a"}); !errors.Is(err, assess.ErrNoOpenRouterKey) {
		t.Errorf("error = %v, want ErrNoOpenRouterKey", err)
	}
}
