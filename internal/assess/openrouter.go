package assess

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// OpenRouter is an Assessor backed by OpenRouter rather than the Anthropic API.
//
// It exists because OpenRouter speaks the OpenAI chat format and nothing else:
// the endpoint is /api/v1/chat/completions, not /v1/messages, and the bodies
// differ, so pointing the Anthropic SDK at it with a base URL override does not
// work. The wire format is the only thing that changes, though. The prompts and
// the JSON schemas are shared with the Anthropic client, which is what keeps
// the two from drifting apart into different behaviour.
//
// It is written against net/http rather than pulling in a second SDK, since the
// whole surface used here is one POST with a JSON body.
type OpenRouter struct {
	http      *http.Client
	key       string
	model     string
	baseURL   string
	maxTokens int64
}

// OpenRouter implements Assessor.
var _ Assessor = (*OpenRouter)(nil)

// DefaultOpenRouterModel is used when no model is configured. OpenRouter names
// models as "vendor/model", and any model it serves that supports structured
// outputs will work here.
const DefaultOpenRouterModel = "anthropic/claude-opus-4.5"

// defaultOpenRouterURL is the chat completions endpoint.
const defaultOpenRouterURL = "https://openrouter.ai/api/v1/chat/completions"

// ErrNoOpenRouterKey reports that no credential was found.
var ErrNoOpenRouterKey = errors.New("assess: OPENROUTER_API_KEY is not set")

// NewOpenRouter returns an OpenRouter-backed Assessor. An empty APIKey falls
// back to the OPENROUTER_API_KEY environment variable.
func NewOpenRouter(cfg Config) *OpenRouter {
	key := cfg.APIKey
	if key == "" {
		key = os.Getenv("OPENROUTER_API_KEY")
	}
	model := cfg.Model
	if model == "" || !strings.Contains(model, "/") {
		// A bare model id is an Anthropic-style name and will not resolve on
		// OpenRouter, which requires the vendor prefix.
		model = DefaultOpenRouterModel
	}
	maxTokens := cfg.MaxTokens
	if maxTokens == 0 {
		maxTokens = DefaultMaxTokens
	}
	return &OpenRouter{
		http:      &http.Client{Timeout: 5 * time.Minute},
		key:       key,
		model:     model,
		baseURL:   defaultOpenRouterURL,
		maxTokens: maxTokens,
	}
}

// NewOpenRouterAt is NewOpenRouter pointed at a different endpoint. It exists so
// the wire format can be tested against a stub server rather than by spending
// money, and so a self-hosted OpenRouter-compatible gateway can be used.
func NewOpenRouterAt(baseURL string, cfg Config) *OpenRouter {
	o := NewOpenRouter(cfg)
	o.baseURL = baseURL
	return o
}

// Semantic judges whether the values at a path changed meaning.
func (o *OpenRouter) Semantic(ctx context.Context, req SemanticRequest) (*SemanticVerdict, error) {
	var out SemanticVerdict
	if err := o.ask(ctx, "semantic_verdict", semanticSystem, renderSemantic(req), semanticSchema, &out); err != nil {
		return nil, err
	}
	if !out.Changed {
		out.Kind = SemanticNone
	}
	return &out, nil
}

// Impact judges what a confirmed change breaks.
func (o *OpenRouter) Impact(ctx context.Context, req ImpactRequest) (*ImpactVerdict, error) {
	var out ImpactVerdict
	if err := o.ask(ctx, "impact_verdict", impactSystem, renderImpact(req), impactSchema, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Patch proposes concrete edits.
func (o *OpenRouter) Patch(ctx context.Context, req PatchRequest) (*PatchVerdict, error) {
	var out PatchVerdict
	if err := o.ask(ctx, "patch_verdict", patchSystem, renderPatch(req), patchSchema, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// chatRequest is the OpenAI-shaped body OpenRouter expects.
type chatRequest struct {
	Model          string     `json:"model"`
	Messages       []chatMsg  `json:"messages"`
	ResponseFormat respFormat `json:"response_format"`
	MaxTokens      int64      `json:"max_tokens,omitempty"`
}

type chatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type respFormat struct {
	Type       string     `json:"type"`
	JSONSchema jsonSchema `json:"json_schema"`
}

type jsonSchema struct {
	Name   string         `json:"name"`
	Strict bool           `json:"strict"`
	Schema map[string]any `json:"schema"`
}

// chatResponse is the subset of the reply this needs.
type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// ask makes one structured-output request and decodes it into out.
func (o *OpenRouter) ask(ctx context.Context, name, system, user string, schema map[string]any, out any) error {
	if o.key == "" {
		return ErrNoOpenRouterKey
	}

	body, err := json.Marshal(chatRequest{
		Model: o.model,
		Messages: []chatMsg{
			{Role: "system", Content: system},
			{Role: "user", Content: user},
		},
		ResponseFormat: respFormat{
			Type:       "json_schema",
			JSONSchema: jsonSchema{Name: name, Strict: true, Schema: schema},
		},
		MaxTokens: o.maxTokens,
	})
	if err != nil {
		return fmt.Errorf("assess: building the request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("assess: building the request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+o.key)
	httpReq.Header.Set("Content-Type", "application/json")
	// OpenRouter uses these for attribution on its dashboard. They are optional
	// and carry nothing about the code being analysed.
	httpReq.Header.Set("HTTP-Referer", "https://github.com/avidnerd/apidrift")
	httpReq.Header.Set("X-Title", "apidrift")

	resp, err := o.http.Do(httpReq)
	if err != nil {
		return fmt.Errorf("assess: calling OpenRouter: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("assess: reading the response: %w", err)
	}

	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return fmt.Errorf("assess: OpenRouter returned %s with a body that is not JSON: %s",
			resp.Status, snippet(raw))
	}
	// An error can arrive with a 200, so the body is checked before the status.
	if parsed.Error != nil {
		return fmt.Errorf("assess: OpenRouter: %s", parsed.Error.Message)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("assess: OpenRouter returned %s: %s", resp.Status, snippet(raw))
	}
	if len(parsed.Choices) == 0 {
		return errors.New("assess: OpenRouter returned no choices")
	}

	content := parsed.Choices[0].Message.Content
	if content == "" {
		return fmt.Errorf("assess: OpenRouter returned empty content (finish_reason %q)",
			parsed.Choices[0].FinishReason)
	}
	if err := json.Unmarshal([]byte(content), out); err != nil {
		return fmt.Errorf("assess: decoding the verdict: %w\n%s", err, snippet([]byte(content)))
	}
	return nil
}

// snippet trims a body for an error message, so a huge HTML error page does not
// end up in the terminal.
func snippet(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "..."
	}
	return s
}
