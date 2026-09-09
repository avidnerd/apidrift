package eval_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/eval"
	"gopkg.in/yaml.v3"
)

func loadV1(t *testing.T) *eval.Spec {
	t.Helper()

	s, err := eval.LoadSpec("testdata/billing-v1.yaml")
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	return s
}

func loadV2(t *testing.T) *eval.Spec {
	t.Helper()

	s, err := eval.LoadSpec("testdata/billing-v2.yaml")
	if err != nil {
		t.Fatalf("LoadSpec: %v", err)
	}
	return s
}

func TestParseSpec(t *testing.T) {
	spec := loadV1(t)

	if spec.Title != "Billing API" || spec.Version != "1.0" {
		t.Errorf("info = %q %q, want \"Billing API\" \"1.0\"", spec.Title, spec.Version)
	}

	ids := spec.EndpointIDs()
	if len(ids) != 3 {
		t.Fatalf("%d endpoints, want 3: %v", len(ids), ids)
	}

	customer := eval.EndpointID{Method: "GET", Path: "/v1/customers/{customer}", Status: 200}
	node, ok := spec.Responses[customer]
	if !ok {
		t.Fatalf("no response for %s; have %v", customer, ids)
	}

	// The $ref was followed.
	if node.Type != "object" {
		t.Errorf("Type = %q, want object -- the $ref was not resolved", node.Type)
	}
	for _, want := range []string{"id", "object", "email", "legacy_id", "balance", "address"} {
		if _, ok := node.Properties[want]; !ok {
			t.Errorf("missing property %q; have %v", want, node.PropertyNames())
		}
	}
	if !node.Required["legacy_id"] {
		t.Error("legacy_id is not required, want required")
	}
	if node.Required["address"] {
		t.Error("address is required, want optional")
	}

	// Enum and nested object came through.
	if got := node.Properties["object"].Enum; len(got) != 1 || got[0] != "customer" {
		t.Errorf("object enum = %v, want [customer]", got)
	}
	if addr := node.Properties["address"]; addr == nil || addr.Properties["city"] == nil {
		t.Error("the nested address object did not survive parsing")
	}

	// The array endpoint's element schema resolved through its $ref.
	charges := eval.EndpointID{Method: "GET", Path: "/v1/charges", Status: 200}
	list := spec.Responses[charges]
	if list == nil || list.Properties["data"] == nil {
		t.Fatal("the charges list has no data property")
	}
	data := list.Properties["data"]
	if data.Type != "array" || data.Items == nil {
		t.Fatalf("data = %+v, want an array with items", data)
	}
	if data.Items.Properties["status"] == nil {
		t.Error("the array element schema did not resolve")
	}
}

// TestParseSpecAcceptsJSON: OpenAPI ships in both spellings, and both must
// produce the same spec -- one loader, so they cannot disagree.
func TestParseSpecAcceptsJSON(t *testing.T) {
	raw, err := os.ReadFile("testdata/billing-v1.yaml")
	if err != nil {
		t.Fatalf("reading spec: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("re-encoding: %v", err)
	}
	asJSON, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshalling to JSON: %v", err)
	}

	fromJSON, err := eval.ParseSpec(asJSON)
	if err != nil {
		t.Fatalf("ParseSpec(JSON): %v", err)
	}
	fromYAML := loadV1(t)

	if len(fromJSON.Responses) != len(fromYAML.Responses) {
		t.Fatalf("JSON gave %d responses, YAML gave %d", len(fromJSON.Responses), len(fromYAML.Responses))
	}
	if len(eval.Diff(fromJSON, fromYAML)) != 0 {
		t.Errorf("the same document parsed differently as JSON and YAML: %v", eval.Diff(fromJSON, fromYAML))
	}
}

func TestParseSpecErrors(t *testing.T) {
	tests := []struct {
		name string
		doc  string
		want string
	}{
		{"empty", "", "empty"},
		{"not yaml", "\t\x00nonsense: [", "decoding"},
		{"no paths", "openapi: 3.0.3\ninfo:\n  title: x\n", "no paths"},
		{"no json responses", `
openapi: 3.0.3
paths:
  /x:
    get:
      responses:
        "200":
          content:
            text/html:
              schema: {type: string}
`, "no JSON response schemas"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := eval.ParseSpec([]byte(tt.doc))
			if err == nil {
				t.Fatalf("ParseSpec = nil error, want one containing %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// TestParseSpecHandlesRecursion guards the limit that makes real specs loadable:
// Stripe's is recursive through $ref, and following it naively does not
// terminate.
func TestParseSpecHandlesRecursion(t *testing.T) {
	doc := `
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
      required: [id]
      properties:
        id: {type: string}
        child: {$ref: '#/components/schemas/Node'}
`
	spec, err := eval.ParseSpec([]byte(doc))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}

	node := spec.Responses[eval.EndpointID{Method: "GET", Path: "/x", Status: 200}]
	if node == nil {
		t.Fatal("no response parsed")
	}
	child := node.Properties["child"]
	if child == nil {
		t.Fatal("the recursive property is missing entirely")
	}
	if !child.Truncated {
		t.Error("the recursive property is not marked Truncated; the cycle was followed rather than cut")
	}
}

func TestParseSpecComposition(t *testing.T) {
	doc := `
openapi: 3.0.3
paths:
  /x:
    get:
      responses:
        "200":
          content:
            application/json:
              schema:
                allOf:
                  - {type: object, required: [a], properties: {a: {type: string}}}
                  - {type: object, required: [b], properties: {b: {type: integer}}}
                properties:
                  c: {type: boolean}
  /y:
    get:
      responses:
        "200":
          content:
            application/json:
              schema:
                oneOf:
                  - {type: object, properties: {first: {type: string}}}
                  - {type: object, properties: {second: {type: string}}}
`
	spec, err := eval.ParseSpec([]byte(doc))
	if err != nil {
		t.Fatalf("ParseSpec: %v", err)
	}

	merged := spec.Responses[eval.EndpointID{Method: "GET", Path: "/x", Status: 200}]
	for _, want := range []string{"a", "b", "c"} {
		if merged.Properties[want] == nil {
			t.Errorf("allOf did not merge %q; have %v", want, merged.PropertyNames())
		}
	}
	if !merged.Required["a"] || !merged.Required["b"] {
		t.Errorf("allOf lost a required list: %v", merged.Required)
	}

	// oneOf takes the first branch, which is a choice rather than a truth.
	chosen := spec.Responses[eval.EndpointID{Method: "GET", Path: "/y", Status: 200}]
	if chosen.Properties["first"] == nil {
		t.Errorf("oneOf did not take the first branch; have %v", chosen.PropertyNames())
	}
}

func TestParseSpecNullableSpellings(t *testing.T) {
	tests := []struct {
		name   string
		schema string
	}{
		{"openapi 3.0", `{type: object, properties: {a: {type: string, nullable: true}}}`},
		{"openapi 3.1", `{type: object, properties: {a: {type: [string, "null"]}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := `
openapi: 3.0.3
paths:
  /x:
    get:
      responses:
        "200":
          content:
            application/json:
              schema: ` + tt.schema + "\n"

			spec, err := eval.ParseSpec([]byte(doc))
			if err != nil {
				t.Fatalf("ParseSpec: %v", err)
			}
			a := spec.Responses[eval.EndpointID{Method: "GET", Path: "/x", Status: 200}].Properties["a"]
			if a == nil {
				t.Fatal("property a is missing")
			}
			if !a.Nullable {
				t.Error("Nullable = false, want true")
			}
			if a.Type != "string" {
				t.Errorf("Type = %q, want string", a.Type)
			}
		})
	}
}

func TestEndpointIDKey(t *testing.T) {
	tests := []struct {
		id   eval.EndpointID
		want endpoint.Key
	}{
		{
			eval.EndpointID{Method: "get", Path: "/v1/customers/{customer}", Status: 200},
			endpoint.Key{Method: "GET", Template: "/v1/customers/{id}", StatusClass: 2},
		},
		{
			eval.EndpointID{Method: "POST", Path: "/v1/charges", Status: 201},
			endpoint.Key{Method: "POST", Template: "/v1/charges", StatusClass: 2},
		},
		{
			eval.EndpointID{Method: "GET", Path: "/v1/a/{x}/b/{y}", Status: 404},
			endpoint.Key{Method: "GET", Template: "/v1/a/{id}/b/{id}", StatusClass: 4},
		},
	}
	for _, tt := range tests {
		t.Run(tt.id.String(), func(t *testing.T) {
			if got := tt.id.Key(); got != tt.want {
				t.Errorf("Key() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
