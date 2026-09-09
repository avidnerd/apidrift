package assess

import (
	"context"
	"sync"
)

// Fake is a scripted Assessor for tests and for a dry run of the CLI.
//
// It exists so that everything around the judgement layer -- candidate
// selection, prompt construction, reporting, the CLI -- is testable without a
// network call or an API key, and so a reviewer can exercise the whole flow
// before deciding to spend money on it.
type Fake struct {
	// SemanticFn and ImpactFn, when set, produce the verdicts. When nil, the
	// zero verdict is returned: nothing changed, nothing breaks.
	SemanticFn func(SemanticRequest) (*SemanticVerdict, error)
	ImpactFn   func(ImpactRequest) (*ImpactVerdict, error)
	PatchFn    func(PatchRequest) (*PatchVerdict, error)

	mu       sync.Mutex
	semantic []SemanticRequest
	impact   []ImpactRequest
	patch    []PatchRequest
}

// Fake implements Assessor.
var _ Assessor = (*Fake)(nil)

// Semantic records the request and returns the scripted verdict.
func (f *Fake) Semantic(ctx context.Context, req SemanticRequest) (*SemanticVerdict, error) {
	f.mu.Lock()
	f.semantic = append(f.semantic, req)
	f.mu.Unlock()

	if f.SemanticFn != nil {
		return f.SemanticFn(req)
	}
	return &SemanticVerdict{
		Kind:       SemanticNone,
		Confidence: ConfidenceHigh,
		Summary:    "values look like ordinary variation",
	}, nil
}

// Impact records the request and returns the scripted verdict.
func (f *Fake) Impact(ctx context.Context, req ImpactRequest) (*ImpactVerdict, error) {
	f.mu.Lock()
	f.impact = append(f.impact, req)
	f.mu.Unlock()

	if f.ImpactFn != nil {
		return f.ImpactFn(req)
	}
	return &ImpactVerdict{Summary: "nothing affected"}, nil
}

// SemanticCalls returns the semantic requests made so far.
func (f *Fake) SemanticCalls() []SemanticRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]SemanticRequest(nil), f.semantic...)
}

// Patch records the request and returns the scripted verdict.
func (f *Fake) Patch(ctx context.Context, req PatchRequest) (*PatchVerdict, error) {
	f.mu.Lock()
	f.patch = append(f.patch, req)
	f.mu.Unlock()

	if f.PatchFn != nil {
		return f.PatchFn(req)
	}
	return &PatchVerdict{Title: "no changes needed", Body: "Nothing in this repository uses the changed field."}, nil
}

// PatchCalls returns the patch requests made so far.
func (f *Fake) PatchCalls() []PatchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]PatchRequest(nil), f.patch...)
}

// ImpactCalls returns the impact requests made so far.
func (f *Fake) ImpactCalls() []ImpactRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ImpactRequest(nil), f.impact...)
}
