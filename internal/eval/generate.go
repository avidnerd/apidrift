package eval

import (
	"encoding/json"
	"fmt"
	"math/rand"

	"github.com/avidnerd/apidrift/internal/schema"
)

// GenConfig controls the synthetic traffic.
//
// Presence rates are the knobs that matter: apidrift detects changes in how
// often a field appears, so the harness has to be able to set that directly and
// sweep it. Everything else here exists to keep the generated bodies realistic
// enough that the schema extractor is doing its real job.
type GenConfig struct {
	// Seed makes a run reproducible.
	Seed int64
	// RequiredPresence is the probability a required field appears. Below 1.0
	// it models an upstream that does not honour its own spec.
	RequiredPresence float64
	// OptionalPresence is the probability an optional field appears.
	OptionalPresence float64
	// PresenceByPath overrides both of the above for specific JSON paths. This
	// is what the detection-latency sweep drives.
	PresenceByPath map[string]float64
	// NullRate is the probability a nullable field is null when present.
	NullRate float64
	// NullRateByPath overrides NullRate for specific paths.
	NullRateByPath map[string]float64
	// ArrayLen is how many elements arrays get. It does not affect presence
	// statistics -- the schema package counts responses, not occurrences -- but
	// it does affect body size.
	ArrayLen int
	// StringCardinality bounds the distinct values generated for a string with
	// no enum. Zero means unbounded, which is what an identifier field looks
	// like and what makes schema.StringOverflow fire.
	StringCardinality int
}

// DefaultGenConfig returns a configuration that produces plausible traffic:
// required fields always present, optional fields most of the time.
func DefaultGenConfig() GenConfig {
	return GenConfig{
		Seed:             1,
		RequiredPresence: 1.0,
		OptionalPresence: 0.7,
		// A nullable field that is never actually null is not a faithful
		// sample of a spec that declares it nullable -- the traffic would
		// silently fail to express a change the ground truth claims, and
		// recall for nullability would read as zero when nothing was wrong
		// with the detector. This is what the evaluation caught the first
		// time it ran end to end.
		NullRate:          0.3,
		ArrayLen:          3,
		StringCardinality: 8,
	}
}

// withDefaults fills the fields that have no sensible zero.
func (c GenConfig) withDefaults() GenConfig {
	if c.RequiredPresence == 0 {
		c.RequiredPresence = 1.0
	}
	if c.OptionalPresence == 0 {
		c.OptionalPresence = DefaultGenConfig().OptionalPresence
	}
	if c.NullRate == 0 {
		c.NullRate = DefaultGenConfig().NullRate
	}
	if c.ArrayLen == 0 {
		c.ArrayLen = DefaultGenConfig().ArrayLen
	}
	return c
}

// Generator produces response bodies conforming to a spec.
//
// It is deterministic given its seed and not safe for concurrent use: the
// harness drives it from one goroutine, and a shared RNG would make runs
// unreproducible, which would defeat the point of measuring anything.
type Generator struct {
	spec *Spec
	cfg  GenConfig
	rng  *rand.Rand
	// counter backs unique string values, so successive responses differ.
	counter int
}

// NewGenerator returns a Generator for spec.
func NewGenerator(spec *Spec, cfg GenConfig) *Generator {
	cfg = cfg.withDefaults()
	return &Generator{spec: spec, cfg: cfg, rng: rand.New(rand.NewSource(cfg.Seed))}
}

// Body generates one response body for the given endpoint.
func (g *Generator) Body(id EndpointID) ([]byte, error) {
	node, ok := g.spec.Responses[id]
	if !ok {
		return nil, fmt.Errorf("eval: spec has no response for %s", id)
	}
	return json.Marshal(g.value("", node, 0))
}

// value produces one value for a node at path.
func (g *Generator) value(path string, n *Node, depth int) any {
	if n == nil || depth >= MaxSchemaDepth {
		return nil
	}
	if n.Nullable && g.rng.Float64() < g.nullRate(path) {
		return nil
	}

	switch n.Type {
	case "object":
		return g.object(path, n, depth)
	case "array":
		return g.array(path, n, depth)
	case "boolean":
		return g.rng.Intn(2) == 0
	case "integer":
		return g.rng.Intn(100000)
	case "number":
		// One decimal place, so the value is unambiguously a float in JSON --
		// schema.Extract types numbers by how they are written, and a whole
		// number would be indistinguishable from an integer.
		return float64(g.rng.Intn(100000)) + 0.5
	default:
		return g.stringValue(n)
	}
}

// object produces an object, including each property with its presence rate.
func (g *Generator) object(path string, n *Node, depth int) map[string]any {
	out := make(map[string]any, len(n.Properties))
	for _, name := range n.PropertyNames() {
		child := joinPath(path, name)
		if g.rng.Float64() >= g.presence(child, n.Required[name]) {
			continue
		}
		out[name] = g.value(child, n.Properties[name], depth+1)
	}
	return out
}

// array produces an array of ArrayLen elements.
func (g *Generator) array(path string, n *Node, depth int) []any {
	elemPath := path + schema.ArrayElem
	out := make([]any, 0, g.cfg.ArrayLen)
	for i := 0; i < g.cfg.ArrayLen; i++ {
		out = append(out, g.value(elemPath, n.Items, depth+1))
	}
	return out
}

// stringValue produces a string: an enum member when the schema constrains it,
// otherwise a value from a bounded pool, or a unique one when unbounded.
func (g *Generator) stringValue(n *Node) string {
	if len(n.Enum) > 0 {
		return n.Enum[g.rng.Intn(len(n.Enum))]
	}
	g.counter++
	if g.cfg.StringCardinality > 0 {
		return fmt.Sprintf("v%d", g.rng.Intn(g.cfg.StringCardinality))
	}
	return fmt.Sprintf("v%d", g.counter)
}

// presence returns the probability that the field at path appears.
func (g *Generator) presence(path string, required bool) float64 {
	if p, ok := g.cfg.PresenceByPath[path]; ok {
		return p
	}
	if required {
		return g.cfg.RequiredPresence
	}
	return g.cfg.OptionalPresence
}

// nullRate returns the probability that a nullable field at path is null.
func (g *Generator) nullRate(path string) float64 {
	if r, ok := g.cfg.NullRateByPath[path]; ok {
		return r
	}
	return g.cfg.NullRate
}
