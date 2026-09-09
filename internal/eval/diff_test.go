package eval_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/eval"
)

// TestDiffAgainstKnownSpecs pins the ground truth for the two test specs
// exactly. If the differ over- or under-reports, every recall and precision
// number downstream is wrong, so this is the load-bearing test of the harness.
func TestDiffAgainstKnownSpecs(t *testing.T) {
	got := eval.Diff(loadV1(t), loadV2(t))

	customer := endpoint.Key{Method: "GET", Template: "/v1/customers/{id}", StatusClass: 2}
	notFound := endpoint.Key{Method: "GET", Template: "/v1/customers/{id}", StatusClass: 4}
	charges := endpoint.Key{Method: "GET", Template: "/v1/charges", StatusClass: 2}

	want := []eval.ID{
		// A removed object costs one change per path beneath it, because
		// apidrift observes each of those paths separately.
		{Endpoint: customer, Path: "address", Kind: detect.FieldRemoved},
		{Endpoint: customer, Path: "address.city", Kind: detect.FieldRemoved},
		{Endpoint: customer, Path: "address.line1", Kind: detect.FieldRemoved},
		{Endpoint: customer, Path: "balance", Kind: detect.TypeChanged},
		{Endpoint: customer, Path: "email", Kind: detect.NullabilityIntroduced},
		{Endpoint: customer, Path: "legacy_id", Kind: detect.FieldRemoved},
		{Endpoint: customer, Path: "tax_id", Kind: detect.FieldAdded},
		{Endpoint: customer, Path: "tax_id.country", Kind: detect.FieldAdded},
		{Endpoint: customer, Path: "tax_id.value", Kind: detect.FieldAdded},
		// A field that stops being required starts going missing, which from
		// outside is a presence-rate drop.
		{Endpoint: notFound, Path: "error.code", Kind: detect.FieldRemoved},
		{Endpoint: charges, Path: "data[].status", Kind: detect.EnumValueAdded},
	}

	gotIDs := make([]eval.ID, 0, len(got))
	for _, c := range got {
		gotIDs = append(gotIDs, c.ID())
	}
	if !sameChangeSets(gotIDs, want) {
		t.Errorf("ground truth mismatch\n got: %s\nwant: %s", renderIDs(gotIDs), renderIDs(want))
	}

	// Every kind is exercised, so recall can be measured for each of them.
	kinds := map[detect.ChangeKind]bool{}
	for _, c := range got {
		kinds[c.Kind] = true
	}
	for _, k := range detect.AllChangeKinds {
		if !kinds[k] {
			t.Errorf("the test specs exercise no %v change; recall for it cannot be measured", k)
		}
	}
}

func TestDiffIsEmptyForIdenticalSpecs(t *testing.T) {
	v1 := loadV1(t)
	if got := eval.Diff(v1, loadV1(t)); len(got) != 0 {
		t.Errorf("a spec differs from itself: %v", got)
	}
	if got := eval.Diff(v1, v1); len(got) != 0 {
		t.Errorf("a spec differs from itself by identity: %v", got)
	}
}

func TestDiffIsDeterministic(t *testing.T) {
	v1, v2 := loadV1(t), loadV2(t)

	first := renderIDs(idsOf(eval.Diff(v1, v2)))
	for i := 0; i < 5; i++ {
		if got := renderIDs(idsOf(eval.Diff(v1, v2))); got != first {
			t.Fatalf("run %d differs:\n%s\n---\n%s", i, first, got)
		}
	}
}

// TestComparableEndpoints: an endpoint that only exists in one version has
// traffic in one window and none in the other, so there is nothing to compare
// and the detector is right to say nothing.
func TestComparableEndpoints(t *testing.T) {
	v1 := loadV1(t)
	v2 := loadV2(t)

	// Remove one endpoint from v2 and confirm it drops out of the comparison
	// and out of ground truth.
	gone := eval.EndpointID{Method: "GET", Path: "/v1/charges", Status: 200}
	delete(v2.Responses, gone)

	for _, id := range eval.ComparableEndpoints(v1, v2) {
		if id == gone {
			t.Fatalf("%s is comparable, but v2 does not define it", gone)
		}
	}
	for _, c := range eval.Diff(v1, v2) {
		if c.Endpoint == gone.Key() {
			t.Errorf("ground truth mentions %s, which cannot be evaluated: %v", gone, c)
		}
	}
}

func TestDiffKinds(t *testing.T) {
	tests := []struct {
		name     string
		v1, v2   string
		wantPath string
		wantKind detect.ChangeKind
		wantNone bool
	}{
		{
			name:     "a scalar type change",
			v1:       `{type: object, properties: {a: {type: integer}}}`,
			v2:       `{type: object, properties: {a: {type: string}}}`,
			wantPath: "a", wantKind: detect.TypeChanged,
		},
		{
			name:     "an object becoming an array",
			v1:       `{type: object, properties: {a: {type: object, properties: {b: {type: string}}}}}`,
			v2:       `{type: object, properties: {a: {type: array, items: {type: string}}}}`,
			wantPath: "a", wantKind: detect.TypeChanged,
		},
		{
			name:     "nullability introduced",
			v1:       `{type: object, properties: {a: {type: string}}}`,
			v2:       `{type: object, properties: {a: {type: string, nullable: true}}}`,
			wantPath: "a", wantKind: detect.NullabilityIntroduced,
		},
		{
			name:     "a change inside an array element",
			v1:       `{type: object, properties: {a: {type: array, items: {type: object, properties: {b: {type: string}}}}}}`,
			v2:       `{type: object, properties: {a: {type: array, items: {type: object, properties: {}}}}}`,
			wantPath: "a[].b", wantKind: detect.FieldRemoved,
		},
		{
			name:     "an enum value added",
			v1:       `{type: object, properties: {a: {type: string, enum: [x, y]}}}`,
			v2:       `{type: object, properties: {a: {type: string, enum: [x, y, z]}}}`,
			wantPath: "a", wantKind: detect.EnumValueAdded,
		},
		{
			name:     "an enum value removed is not an addition",
			v1:       `{type: object, properties: {a: {type: string, enum: [x, y, z]}}}`,
			v2:       `{type: object, properties: {a: {type: string, enum: [x, y]}}}`,
			wantNone: true,
		},
		{
			name:     "an enum appearing where there was none is not a value addition",
			v1:       `{type: object, properties: {a: {type: string}}}`,
			v2:       `{type: object, properties: {a: {type: string, enum: [x, y]}}}`,
			wantNone: true,
		},
		{
			name:     "nullability removed is not a change worth reporting",
			v1:       `{type: object, properties: {a: {type: string, nullable: true}}}`,
			v2:       `{type: object, properties: {a: {type: string}}}`,
			wantNone: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := eval.Diff(specWith(t, tt.v1), specWith(t, tt.v2))

			if tt.wantNone {
				if len(got) != 0 {
					t.Fatalf("got %v, want no changes", got)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("got %d changes, want 1: %v", len(got), got)
			}
			if got[0].Path != tt.wantPath || got[0].Kind != tt.wantKind {
				t.Errorf("change = %s %v, want %s %v", got[0].Path, got[0].Kind, tt.wantPath, tt.wantKind)
			}
		})
	}
}

// TestDiffStopsAtATypeChange: once two subtrees have different types they no
// longer correspond, and descending would report every field of one as removed
// and every field of the other as added -- a dozen findings for one change.
func TestDiffStopsAtATypeChange(t *testing.T) {
	v1 := specWith(t, `{type: object, properties: {a: {type: object, properties: {x: {type: string}, y: {type: string}}}}}`)
	v2 := specWith(t, `{type: object, properties: {a: {type: string}}}`)

	got := eval.Diff(v1, v2)
	if len(got) != 1 {
		t.Fatalf("got %d changes, want exactly 1: %v", len(got), got)
	}
	if got[0].Kind != detect.TypeChanged || got[0].Path != "a" {
		t.Errorf("change = %s %v, want a type_changed", got[0].Path, got[0].Kind)
	}
}

// TestDiffIgnoresTruncatedSubtrees: a subtree the loader declined to read is not
// evidence of anything, and reporting differences under it would invent ground
// truth out of a parser limit.
func TestDiffIgnoresTruncatedSubtrees(t *testing.T) {
	recursive := `
openapi: 3.0.3
paths:
  /x:
    get:
      responses:
        "200":
          content:
            application/json:
              schema: {$ref: '#/components/schemas/Node'}
components:
  schemas:
    Node:
      type: object
      properties:
        id: {type: string}
        child: {$ref: '#/components/schemas/Node'}
`
	v1, err := eval.ParseSpec([]byte(recursive))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}
	v2, err := eval.ParseSpec([]byte(strings.Replace(recursive, "id: {type: string}", "id: {type: integer}", 1)))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}

	got := eval.Diff(v1, v2)
	if len(got) != 1 || got[0].Path != "id" {
		t.Errorf("got %v, want exactly one change at \"id\" -- the truncated child must not contribute", got)
	}
}

// -- helpers ------------------------------------------------------------------

// specWith wraps a schema fragment in a minimal document.
func specWith(t *testing.T, schema string) *eval.Spec {
	t.Helper()

	doc := `
openapi: 3.0.3
paths:
  /x:
    get:
      responses:
        "200":
          content:
            application/json:
              schema: ` + schema + "\n"

	s, err := eval.ParseSpec([]byte(doc))
	if err != nil {
		t.Fatalf("ParseSpec(%s): %v", schema, err)
	}
	return s
}

func idsOf(changes []eval.Change) []eval.ID {
	out := make([]eval.ID, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.ID())
	}
	return out
}

func renderIDs(ids []eval.ID) string {
	lines := make([]string, 0, len(ids))
	for _, id := range ids {
		lines = append(lines, "\n  "+id.Endpoint.String()+" "+id.Path+" "+id.Kind.String())
	}
	sort.Strings(lines)
	return strings.Join(lines, "")
}

func sameChangeSets(a, b []eval.ID) bool {
	return renderIDs(a) == renderIDs(b)
}
