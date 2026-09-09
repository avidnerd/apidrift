package eval

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/avidnerd/apidrift/internal/endpoint"
	"gopkg.in/yaml.v3"
)

// Limits on what the loader will follow, so a hostile or merely recursive spec
// cannot exhaust memory or stack. Stripe's spec is deeply recursive through
// $ref, so these are load-bearing rather than theoretical.
const (
	// MaxSchemaDepth bounds nesting. Below it, a node keeps its type but not
	// its contents.
	MaxSchemaDepth = 24
	// MaxRefDepth bounds a chain of $ref indirections.
	MaxRefDepth = 64
)

// EndpointID identifies one response body in a spec: a method, a path template,
// and a status code.
type EndpointID struct {
	Method string
	Path   string
	Status int
}

// Key converts the id to the endpoint key the detector uses.
//
// Spec paths carry named parameters -- /v1/customers/{customer} -- while the
// templater emits a single anonymous placeholder. Normalising here is what lets
// ground truth and findings be compared by key.
func (e EndpointID) Key() endpoint.Key {
	return endpoint.Key{
		Method:      strings.ToUpper(e.Method),
		Template:    normalizeTemplate(e.Path),
		StatusClass: e.Status / 100,
	}
}

// String renders the id for reports and errors.
func (e EndpointID) String() string {
	return fmt.Sprintf("%s %s %d", strings.ToUpper(e.Method), e.Path, e.Status)
}

// normalizeTemplate rewrites every {named} parameter to the anonymous
// placeholder the templater produces.
func normalizeTemplate(path string) string {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		if len(s) >= 2 && s[0] == '{' && s[len(s)-1] == '}' {
			segs[i] = endpoint.Variable
		}
	}
	return strings.Join(segs, "/")
}

// Node is the subset of JSON Schema the harness understands: enough to describe
// a response body's shape, and nothing more.
//
// Deliberately not a general JSON Schema implementation. The harness needs to
// know what fields exist, what types they hold, whether they are required, and
// what values an enum admits. Constraints like minLength or pattern do not
// change a response's *shape*, which is the only thing apidrift can see.
type Node struct {
	// Type is the JSON type: object, array, string, integer, number, boolean.
	Type string
	// Nullable reports that the value may be null.
	Nullable bool
	// Enum lists the permitted string values, if the schema constrains them.
	Enum []string
	// Properties are an object's fields.
	Properties map[string]*Node
	// Required names the properties that must be present.
	Required map[string]bool
	// Items is an array's element schema.
	Items *Node
	// Truncated reports that the loader stopped here, at a depth or recursion
	// limit. Diff skips truncated subtrees rather than reporting the absence of
	// fields it declined to read.
	Truncated bool
}

// PropertyNames returns the object's field names in sorted order, so every walk
// over a spec is deterministic.
func (n *Node) PropertyNames() []string {
	if n == nil {
		return nil
	}
	out := make([]string, 0, len(n.Properties))
	for k := range n.Properties {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Spec is a loaded OpenAPI document, reduced to the response bodies it defines.
type Spec struct {
	// Title and Version come from the document's info block, for reports.
	Title   string
	Version string
	// Responses maps each documented response to its body schema.
	Responses map[EndpointID]*Node
}

// EndpointIDs returns the spec's endpoints in a stable order.
func (s *Spec) EndpointIDs() []EndpointID {
	out := make([]EndpointID, 0, len(s.Responses))
	for id := range s.Responses {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		if out[i].Method != out[j].Method {
			return out[i].Method < out[j].Method
		}
		return out[i].Status < out[j].Status
	})
	return out
}

// LoadSpec reads and parses an OpenAPI document from disk.
func LoadSpec(path string) (*Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("eval: reading spec %s: %w", path, err)
	}
	s, err := ParseSpec(data)
	if err != nil {
		return nil, fmt.Errorf("eval: parsing spec %s: %w", path, err)
	}
	return s, nil
}

// ParseSpec parses an OpenAPI document.
//
// YAML and JSON go through the same path, because YAML is a superset of JSON
// and yaml.v3 accepts both. That is the entire reason for the dependency: the
// alternative is two loaders that can disagree about the same document.
func ParseSpec(data []byte) (*Spec, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decoding document: %w", err)
	}
	if doc == nil {
		return nil, fmt.Errorf("document is empty")
	}

	spec := &Spec{Responses: make(map[EndpointID]*Node)}
	if info, ok := mapAt(doc, "info"); ok {
		spec.Title, _ = stringAt(info, "title")
		spec.Version, _ = stringAt(info, "version")
	}

	paths, ok := mapAt(doc, "paths")
	if !ok {
		return nil, fmt.Errorf("document has no paths section; is it an OpenAPI spec?")
	}

	l := &loader{doc: doc}
	for path, rawItem := range paths {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		for method, rawOp := range item {
			if !isHTTPMethod(method) {
				continue
			}
			op, ok := rawOp.(map[string]any)
			if !ok {
				continue
			}
			responses, ok := mapAt(op, "responses")
			if !ok {
				continue
			}
			for statusText, rawResp := range responses {
				status, err := strconv.Atoi(statusText)
				if err != nil {
					// "default" and range forms like "4XX" describe no single
					// status, so there is no window to file them under.
					continue
				}
				schema, ok := l.responseSchema(rawResp)
				if !ok {
					continue
				}
				spec.Responses[EndpointID{Method: strings.ToUpper(method), Path: path, Status: status}] =
					l.node(schema, 0, nil)
			}
		}
	}

	if len(spec.Responses) == 0 {
		return nil, fmt.Errorf("no JSON response schemas found")
	}
	return spec, nil
}

// loader carries the whole document, so $ref can be resolved against it.
type loader struct {
	doc map[string]any
}

// responseSchema digs the JSON body schema out of one response object.
func (l *loader) responseSchema(rawResp any) (map[string]any, bool) {
	resp, ok := rawResp.(map[string]any)
	if !ok {
		return nil, false
	}
	if ref, isRef := stringAt(resp, "$ref"); isRef {
		resolved, ok := l.resolve(ref, 0)
		if !ok {
			return nil, false
		}
		resp = resolved
	}
	content, ok := mapAt(resp, "content")
	if !ok {
		return nil, false
	}
	for mediaType, rawMedia := range content {
		if !isJSONMediaType(mediaType) {
			continue
		}
		media, ok := rawMedia.(map[string]any)
		if !ok {
			continue
		}
		if schema, ok := mapAt(media, "schema"); ok {
			return schema, true
		}
	}
	return nil, false
}

// node converts a JSON Schema object into a Node.
//
// refs is the chain of $ref targets currently being resolved; a target already
// on the chain is a cycle, and is truncated rather than followed. Stripe's spec
// is full of these -- a Customer holds a Subscription which holds a Customer.
func (l *loader) node(schema map[string]any, depth int, refs []string) *Node {
	if depth >= MaxSchemaDepth || len(refs) >= MaxRefDepth {
		return &Node{Type: "object", Truncated: true}
	}

	if ref, ok := stringAt(schema, "$ref"); ok {
		for _, seen := range refs {
			if seen == ref {
				return &Node{Type: "object", Truncated: true}
			}
		}
		resolved, ok := l.resolve(ref, len(refs))
		if !ok {
			return &Node{Type: "object", Truncated: true}
		}
		return l.node(resolved, depth, append(refs, ref))
	}

	// allOf is composition: merge the branches into one node. anyOf and oneOf
	// are alternatives, and a generator has to pick one -- the first, which
	// keeps generation deterministic at the cost of never exercising the rest.
	if branches, ok := sliceAt(schema, "allOf"); ok {
		return l.merged(branches, schema, depth, refs)
	}
	for _, key := range []string{"oneOf", "anyOf"} {
		if branches, ok := sliceAt(schema, key); ok && len(branches) > 0 {
			if first, ok := branches[0].(map[string]any); ok {
				return l.node(first, depth, refs)
			}
		}
	}

	n := &Node{Type: schemaType(schema), Nullable: schemaNullable(schema)}

	if values, ok := sliceAt(schema, "enum"); ok {
		for _, v := range values {
			if s, ok := v.(string); ok {
				n.Enum = append(n.Enum, s)
			}
		}
		sort.Strings(n.Enum)
	}

	if props, ok := mapAt(schema, "properties"); ok {
		n.Type = "object"
		n.Properties = make(map[string]*Node, len(props))
		for name, rawProp := range props {
			prop, ok := rawProp.(map[string]any)
			if !ok {
				continue
			}
			n.Properties[name] = l.node(prop, depth+1, refs)
		}
	}
	if required, ok := sliceAt(schema, "required"); ok {
		n.Required = make(map[string]bool, len(required))
		for _, r := range required {
			if s, ok := r.(string); ok {
				n.Required[s] = true
			}
		}
	}
	if items, ok := mapAt(schema, "items"); ok {
		n.Type = "array"
		n.Items = l.node(items, depth+1, refs)
	}

	if n.Type == "" {
		// A schema with properties but no declared type is an object; one with
		// neither is unconstrained, and a string is the least surprising thing
		// to generate for it.
		if n.Properties != nil {
			n.Type = "object"
		} else {
			n.Type = "string"
		}
	}
	return n
}

// merged combines the branches of an allOf, plus any sibling keywords, into one
// node.
func (l *loader) merged(branches []any, parent map[string]any, depth int, refs []string) *Node {
	out := &Node{Type: "object", Properties: map[string]*Node{}, Required: map[string]bool{}}

	add := func(n *Node) {
		if n == nil {
			return
		}
		if n.Truncated {
			out.Truncated = true
		}
		if n.Type != "" && n.Type != "object" {
			out.Type = n.Type
		}
		out.Nullable = out.Nullable || n.Nullable
		out.Enum = append(out.Enum, n.Enum...)
		if n.Items != nil {
			out.Items = n.Items
		}
		for k, v := range n.Properties {
			out.Properties[k] = v
		}
		for k := range n.Required {
			out.Required[k] = true
		}
	}

	for _, raw := range branches {
		branch, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		add(l.node(branch, depth+1, refs))
	}

	// Keywords sitting alongside the allOf apply too.
	sibling := make(map[string]any, len(parent))
	for k, v := range parent {
		if k != "allOf" {
			sibling[k] = v
		}
	}
	if len(sibling) > 0 {
		add(l.node(sibling, depth+1, refs))
	}

	if len(out.Properties) == 0 {
		out.Properties = nil
	}
	if len(out.Required) == 0 {
		out.Required = nil
	}
	sort.Strings(out.Enum)
	return out
}

// resolve follows a local $ref of the form "#/components/schemas/Name".
func (l *loader) resolve(ref string, depth int) (map[string]any, bool) {
	if depth >= MaxRefDepth || !strings.HasPrefix(ref, "#/") {
		// External refs would need a fetcher and a trust model; the harness
		// reads self-contained documents.
		return nil, false
	}
	cur := any(l.doc)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[unescapeRefToken(part)]
		if !ok {
			return nil, false
		}
	}
	out, ok := cur.(map[string]any)
	return out, ok
}

// unescapeRefToken decodes the JSON Pointer escapes ~1 for "/" and ~0 for "~".
func unescapeRefToken(s string) string {
	s = strings.ReplaceAll(s, "~1", "/")
	return strings.ReplaceAll(s, "~0", "~")
}

// schemaType reads the declared type, accepting the OpenAPI 3.1 form where it
// is a list that may include "null".
func schemaType(schema map[string]any) string {
	switch t := schema["type"].(type) {
	case string:
		return t
	case []any:
		for _, v := range t {
			if s, ok := v.(string); ok && s != "null" {
				return s
			}
		}
	}
	return ""
}

// schemaNullable accepts both the 3.0 spelling (nullable: true) and the 3.1
// spelling (type includes "null").
func schemaNullable(schema map[string]any) bool {
	if b, ok := schema["nullable"].(bool); ok && b {
		return true
	}
	if list, ok := schema["type"].([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok && s == "null" {
				return true
			}
		}
	}
	return false
}

func isHTTPMethod(s string) bool {
	switch strings.ToLower(s) {
	case "get", "put", "post", "delete", "options", "head", "patch", "trace":
		return true
	}
	return false
}

func isJSONMediaType(s string) bool {
	s = strings.ToLower(strings.TrimSpace(strings.SplitN(s, ";", 2)[0]))
	return s == "application/json" || s == "text/json" || strings.HasSuffix(s, "+json")
}

func mapAt(m map[string]any, key string) (map[string]any, bool) {
	v, ok := m[key].(map[string]any)
	return v, ok
}

func sliceAt(m map[string]any, key string) ([]any, bool) {
	v, ok := m[key].([]any)
	return v, ok
}

func stringAt(m map[string]any, key string) (string, bool) {
	v, ok := m[key].(string)
	return v, ok
}
