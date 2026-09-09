package eval

import (
	"fmt"
	"sort"
	"strings"

	"github.com/avidnerd/apidrift/internal/detect"
	"github.com/avidnerd/apidrift/internal/endpoint"
	"github.com/avidnerd/apidrift/internal/schema"
)

// Change is one ground-truth difference between two spec versions.
type Change struct {
	// Endpoint is the endpoint the change occurs on.
	Endpoint endpoint.Key `json:"endpoint"`
	// Path is the JSON path, in the same dotted-with-[] notation the schema
	// package produces, so findings and truth are directly comparable.
	Path string `json:"path"`
	// Kind is what changed.
	Kind detect.ChangeKind `json:"kind"`
	// Detail is a human-readable note for the report.
	Detail string `json:"detail"`
}

// ID identifies a change for matching against findings. Detail is descriptive
// and deliberately excluded.
type ID struct {
	Endpoint endpoint.Key
	Path     string
	Kind     detect.ChangeKind
}

// ID returns the change's identity.
func (c Change) ID() ID {
	return ID{Endpoint: c.Endpoint, Path: c.Path, Kind: c.Kind}
}

// String renders the change for reports.
func (c Change) String() string {
	return fmt.Sprintf("%s %s %s (%s)", c.Endpoint, c.Path, c.Kind, c.Detail)
}

// ComparableEndpoints returns the endpoints defined in both specs, which are
// the only ones the harness can evaluate.
//
// An endpoint added or removed between versions has traffic in one window and
// none in the other, so there is nothing to compare -- and the detector is
// right to say nothing about it. Counting those as missed detections would
// score the tool for a job it correctly declined.
func ComparableEndpoints(v1, v2 *Spec) []EndpointID {
	var out []EndpointID
	for _, id := range v1.EndpointIDs() {
		if _, ok := v2.Responses[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// Diff produces the ground-truth changelist between two spec versions.
//
// The paths it emits use the schema package's notation, and it emits one change
// per affected path rather than one per conceptual edit. Removing an object
// removes every path beneath it, and apidrift sees each of those separately --
// so ground truth has to list each of them, or the detector's correct findings
// about the children would be scored as false positives.
func Diff(v1, v2 *Spec) []Change {
	var changes []Change
	for _, id := range ComparableEndpoints(v1, v2) {
		d := &differ{key: id.Key()}
		d.walk("", v1.Responses[id], v2.Responses[id])
		changes = append(changes, d.changes...)
	}

	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Endpoint != changes[j].Endpoint {
			return changes[i].Endpoint.String() < changes[j].Endpoint.String()
		}
		if changes[i].Path != changes[j].Path {
			return changes[i].Path < changes[j].Path
		}
		return changes[i].Kind < changes[j].Kind
	})
	return changes
}

// differ accumulates one endpoint's changes.
type differ struct {
	key     endpoint.Key
	changes []Change
}

func (d *differ) add(path string, kind detect.ChangeKind, detail string) {
	d.changes = append(d.changes, Change{
		Endpoint: d.key,
		Path:     path,
		Kind:     kind,
		Detail:   detail,
	})
}

// walk compares two nodes at the same path.
func (d *differ) walk(path string, a, b *Node) {
	if a == nil || b == nil {
		return
	}
	// A truncated node is one the loader declined to read fully, usually
	// because the spec is recursive. Reporting differences under it would be
	// reporting on data neither side actually has.
	if a.Truncated || b.Truncated {
		return
	}

	if a.Type != b.Type {
		d.add(path, detect.TypeChanged, fmt.Sprintf("%s became %s", a.Type, b.Type))
		// The shapes no longer correspond, so descending would compare
		// unrelated subtrees and manufacture changes that are really one.
		return
	}
	if !a.Nullable && b.Nullable {
		d.add(path, detect.NullabilityIntroduced, "became nullable")
	}
	if added := addedEnumValues(a.Enum, b.Enum); len(added) > 0 {
		d.add(path, detect.EnumValueAdded, "added "+strings.Join(added, ", "))
	}

	switch a.Type {
	case "object":
		d.walkObject(path, a, b)
	case "array":
		d.walk(path+schema.ArrayElem, a.Items, b.Items)
	}
}

// walkObject compares the properties of two objects.
func (d *differ) walkObject(path string, a, b *Node) {
	for _, name := range a.PropertyNames() {
		child := joinPath(path, name)
		bChild, stillThere := b.Properties[name]
		if !stillThere {
			d.addSubtree(child, a.Properties[name], detect.FieldRemoved, "removed")
			continue
		}
		// A field that stops being required starts going missing, which is a
		// presence-rate drop -- the same observation apidrift labels
		// FieldRemoved, because from the outside it is the same thing.
		if a.Required[name] && !b.Required[name] {
			d.add(child, detect.FieldRemoved, "became optional")
		}
		d.walk(child, a.Properties[name], bChild)
	}

	for _, name := range b.PropertyNames() {
		if _, existed := a.Properties[name]; existed {
			continue
		}
		d.addSubtree(joinPath(path, name), b.Properties[name], detect.FieldAdded, "added")
	}
}

// addSubtree records the same kind for a path and everything beneath it, since
// apidrift observes each of those paths separately.
func (d *differ) addSubtree(path string, n *Node, kind detect.ChangeKind, detail string) {
	d.add(path, kind, detail)
	if n == nil || n.Truncated {
		return
	}
	switch n.Type {
	case "object":
		for _, name := range n.PropertyNames() {
			d.addSubtree(joinPath(path, name), n.Properties[name], kind, detail)
		}
	case "array":
		if n.Items != nil {
			d.addSubtree(path+schema.ArrayElem, n.Items, kind, detail)
		}
	}
}

// addedEnumValues returns the values in b that are not in a.
func addedEnumValues(a, b []string) []string {
	if len(a) == 0 || len(b) == 0 {
		// An enum that appears or disappears entirely is a constraint change,
		// not a new value: with no baseline enum every value is "new", which
		// would be a meaningless finding.
		return nil
	}
	have := make(map[string]bool, len(a))
	for _, v := range a {
		have[v] = true
	}
	var added []string
	for _, v := range b {
		if !have[v] {
			added = append(added, v)
		}
	}
	sort.Strings(added)
	return added
}

// joinPath appends a property name, handling the root case.
func joinPath(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}
