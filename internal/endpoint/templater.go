package endpoint

import (
	"strings"
	"sync"
)

// Config tunes the templating heuristic. The zero value is not usable; take
// DefaultConfig and adjust, or pass a Config with zero fields to
// NewTrieTemplater, which fills each zero field with its default.
type Config struct {
	// MinSamples is how many observations must pass through a position before
	// Resolve will commit to any answer about it. Below this, Resolve reports
	// ok=false rather than guessing.
	MinSamples uint64

	// StatMinSamples is the much higher bar the statistical tier must clear.
	// Distinguishing a variable from a large static route set requires enough
	// traffic for a static set to have visibly plateaued; the shape tier
	// handles the common cases long before this.
	StatMinSamples uint64

	// IDFraction is the share of observations at a position that must be
	// identifier-shaped for the position to be a variable.
	IDFraction float64

	// MinDistinctForVariable is the number of distinct literal values a
	// position must have taken before the statistical tier will consider it.
	MinDistinctForVariable int

	// GrowthRatio is the distinct-values-per-observation rate above which a
	// position counts as still growing rather than plateaued.
	GrowthRatio float64

	// MaxChildrenPerNode caps the literal children stored at one position.
	// Beyond it, new literals are counted but not stored: they feed the growth
	// statistic without consuming memory.
	MaxChildrenPerNode int

	// MaxSegments caps path depth. Deeper paths are templated to their first
	// MaxSegments segments followed by Overflow.
	MaxSegments int

	// MaxNodes caps the whole trie. Once reached, no new nodes are created and
	// paths needing one resolve to ok=false.
	MaxNodes int
}

// DefaultConfig returns the tuning the project ships with. The reasoning behind
// each number is in DECISIONS.md.
func DefaultConfig() Config {
	return Config{
		MinSamples:             50,
		StatMinSamples:         500,
		IDFraction:             0.8,
		MinDistinctForVariable: 8,
		GrowthRatio:            0.5,
		MaxChildrenPerNode:     256,
		MaxSegments:            16,
		MaxNodes:               100_000,
	}
}

// withDefaults fills zero fields from DefaultConfig, so callers can override
// one knob without restating the rest.
func (c Config) withDefaults() Config {
	d := DefaultConfig()
	if c.MinSamples == 0 {
		c.MinSamples = d.MinSamples
	}
	if c.StatMinSamples == 0 {
		c.StatMinSamples = d.StatMinSamples
	}
	if c.IDFraction == 0 {
		c.IDFraction = d.IDFraction
	}
	if c.MinDistinctForVariable == 0 {
		c.MinDistinctForVariable = d.MinDistinctForVariable
	}
	if c.GrowthRatio == 0 {
		c.GrowthRatio = d.GrowthRatio
	}
	if c.MaxChildrenPerNode == 0 {
		c.MaxChildrenPerNode = d.MaxChildrenPerNode
	}
	if c.MaxSegments == 0 {
		c.MaxSegments = d.MaxSegments
	}
	if c.MaxNodes == 0 {
		c.MaxNodes = d.MaxNodes
	}
	return c
}

// classification is what the templater has decided about one path position.
type classification uint8

const (
	// undecided means not enough traffic has passed through the position yet.
	undecided classification = iota
	// static means the position holds a bounded set of route names.
	static
	// variable means the position holds identifiers. This decision is sticky.
	variable
)

// node is one position in the path trie. Its counters describe the segment that
// comes next, not the segment that led here.
type node struct {
	// total is how many observations continued past this node.
	total uint64
	// idHits is how many of those had an identifier-shaped next segment.
	idHits uint64
	// named holds children for literal next segments, capped at
	// MaxChildrenPerNode.
	named map[string]*node
	// capMisses counts observations whose literal segment could not be stored
	// because named was full. It is an over-count of distinct unstored values,
	// since repeats of the same unstored value each count -- deliberately, as
	// the growth test wants an upper bound on how fast new values arrive.
	capMisses uint64
	// wild is the single child that all variable segments descend into.
	wild *node
	// kind caches the decision. Only the sticky variable verdict is cached;
	// static and undecided are recomputed, since more traffic can overturn them.
	kind classification
}

// Stats describes the templater's state and the limits it has run into.
type Stats struct {
	// Nodes is how many trie nodes are currently allocated.
	Nodes int `json:"nodes"`
	// Observations is how many paths have been observed.
	Observations uint64 `json:"observations"`
	// Collapses counts positions that flipped to variable, discarding the
	// literal children learned beneath them.
	Collapses uint64 `json:"collapses"`
	// ChildLimitHits counts observations whose literal segment could not be
	// stored because a position was at MaxChildrenPerNode.
	ChildLimitHits uint64 `json:"child_limit_hits"`
	// NodeLimitHits counts observations that needed a node the trie could not
	// allocate because it was at MaxNodes.
	NodeLimitHits uint64 `json:"node_limit_hits"`
	// TruncatedPaths counts paths deeper than MaxSegments.
	TruncatedPaths uint64 `json:"truncated_paths"`
}

// TrieTemplater learns path templates from observed traffic.
//
// Positions are judged in a prefix trie rather than by their index in the path,
// because the same index means different things under different prefixes: the
// second segment of /users/8123 is an identifier while the second segment of
// /docs/pricing is a route name. Only a structure that keeps the prefix can
// tell them apart.
//
// Memory is bounded three ways: at most MaxNodes nodes overall, at most
// MaxChildrenPerNode literal children at any position, and at most MaxSegments
// levels deep. Each limit has a counter in Stats, so exhausting one is visible
// rather than silent.
//
// TrieTemplater is safe for concurrent use.
type TrieTemplater struct {
	cfg Config

	// mu guards everything below. It is a plain Mutex rather than an RWMutex
	// because Resolve is not read-only: reaching a verdict can cache a sticky
	// decision and discard the literal children beneath it.
	mu    sync.Mutex
	roots map[string]*node
	nodes int
	stats Stats
}

// TrieTemplater implements Templater.
var _ Templater = (*TrieTemplater)(nil)

// NewTrieTemplater returns a templater using cfg, with zero fields defaulted.
func NewTrieTemplater(cfg Config) *TrieTemplater {
	return &TrieTemplater{
		cfg:   cfg.withDefaults(),
		roots: make(map[string]*node),
	}
}

// Observe records a raw path so the templater can learn which segments vary.
func (t *TrieTemplater) Observe(method, rawPath string) {
	segs, truncated := t.split(rawPath)

	t.mu.Lock()
	defer t.mu.Unlock()

	t.stats.Observations++
	if truncated {
		t.stats.TruncatedPaths++
	}

	n := t.roots[method]
	if n == nil {
		n = t.newNodeLocked()
		if n == nil {
			return
		}
		t.roots[method] = n
	}
	for _, seg := range segs {
		next := t.stepObserveLocked(n, seg)
		if next == nil {
			return // a limit was hit; the prefix learned so far still counts
		}
		n = next
	}
}

// Resolve returns the endpoint template for a raw path. ok is false if the
// templater hasn't seen enough samples to decide yet.
//
// A path resolves only when every position along it has been decided. A single
// undecided position fails the whole path rather than being guessed at: an
// endpoint key that changes meaning once more traffic arrives would silently
// split one endpoint's history in two.
func (t *TrieTemplater) Resolve(method, rawPath string, status int) (Key, bool) {
	segs, truncated := t.split(rawPath)

	t.mu.Lock()
	defer t.mu.Unlock()

	n := t.roots[method]
	if n == nil {
		return Key{}, false
	}

	parts := make([]string, 0, len(segs)+1)
	for _, seg := range segs {
		if t.classifyLocked(n) == undecided {
			return Key{}, false
		}
		next, part := t.stepResolveLocked(n, seg)
		if next == nil {
			return Key{}, false
		}
		parts = append(parts, part)
		n = next
	}
	if truncated {
		parts = append(parts, Overflow)
	}

	return Key{
		Method:      method,
		Template:    buildTemplate(parts),
		StatusClass: status / 100,
	}, true
}

// Stats returns a snapshot of the templater's state.
func (t *TrieTemplater) Stats() Stats {
	t.mu.Lock()
	defer t.mu.Unlock()

	s := t.stats
	s.Nodes = t.nodes
	return s
}

// stepObserveLocked accounts for seg at n and returns the child to continue
// from, or nil if a limit prevented it.
func (t *TrieTemplater) stepObserveLocked(n *node, seg string) *node {
	n.total++
	if isIDShaped(seg) {
		n.idHits++
	}

	if t.routesToWildLocked(n, seg) {
		if n.wild == nil {
			n.wild = t.newNodeLocked()
		}
		return n.wild
	}

	if c, ok := n.named[seg]; ok {
		return c
	}
	if len(n.named) >= t.cfg.MaxChildrenPerNode {
		// The position is full. Count the miss -- it is evidence of growth --
		// but allocate nothing.
		n.capMisses++
		t.stats.ChildLimitHits++
		return nil
	}
	c := t.newNodeLocked()
	if c == nil {
		return nil
	}
	if n.named == nil {
		n.named = make(map[string]*node)
	}
	n.named[seg] = c
	return c
}

// stepResolveLocked walks the same way stepObserveLocked did, returning the
// child and the segment's rendering. Observe and Resolve must agree on the
// route taken, or a path would resolve to a template no traffic was recorded
// under.
func (t *TrieTemplater) stepResolveLocked(n *node, seg string) (*node, string) {
	if t.routesToWildLocked(n, seg) {
		return n.wild, Variable // nil wild means the position was never observed
	}
	return n.named[seg], seg
}

// routesToWildLocked reports whether seg is treated as a variable at n.
//
// Two independent grounds: the segment looks like an identifier, or the
// position as a whole has been judged to hold identifiers. The first is
// per-segment evidence and applies even at a static position, so an occasional
// identifier appearing among route names does not create a new endpoint per
// value.
func (t *TrieTemplater) routesToWildLocked(n *node, seg string) bool {
	return isIDShaped(seg) || t.classifyLocked(n) == variable
}

// classifyLocked decides what n's next position holds, caching only the sticky
// variable verdict.
func (t *TrieTemplater) classifyLocked(n *node) classification {
	if n.kind == variable {
		return variable
	}
	if n.total < t.cfg.MinSamples {
		return undecided
	}

	// Shape tier: the position is mostly identifiers.
	if float64(n.idHits)/float64(n.total) >= t.cfg.IDFraction {
		t.markVariableLocked(n)
		return variable
	}

	// Growth tier: distinct values are still arriving in proportion to
	// traffic, so the set is not a bounded list of route names. This needs far
	// more traffic than the shape tier, because early on a large static route
	// set and a variable are genuinely indistinguishable -- fifty requests
	// spread over forty-five documentation pages look exactly like forty-five
	// identifiers.
	if n.total >= t.cfg.StatMinSamples &&
		len(n.named) >= t.cfg.MinDistinctForVariable &&
		float64(len(n.named)+int(n.capMisses))/float64(n.total) >= t.cfg.GrowthRatio {
		t.markVariableLocked(n)
		return variable
	}

	return static
}

// markVariableLocked records the sticky verdict and discards the literal
// children learned beneath the position.
//
// The subtrees are discarded rather than merged into the wildcard child. Merging
// would preserve the structure learned under each identifier, but it is exactly
// the structure that is about to be re-learned in the next few hundred requests,
// and merging n subtrees correctly is a great deal of machinery to save that.
// Discarding keeps the memory bound trivially true.
func (t *TrieTemplater) markVariableLocked(n *node) {
	n.kind = variable
	if n.named == nil {
		return
	}
	freed := 0
	for _, c := range n.named {
		freed += countNodes(c)
	}
	n.named = nil
	t.nodes -= freed
	t.stats.Collapses++
}

// newNodeLocked allocates a node, or returns nil if the trie is at MaxNodes.
func (t *TrieTemplater) newNodeLocked() *node {
	if t.nodes >= t.cfg.MaxNodes {
		t.stats.NodeLimitHits++
		return nil
	}
	t.nodes++
	return &node{}
}

// countNodes returns the size of the subtree rooted at n, including n.
func countNodes(n *node) int {
	if n == nil {
		return 0
	}
	total := 1
	for _, c := range n.named {
		total += countNodes(c)
	}
	total += countNodes(n.wild)
	return total
}

// split breaks a path into segments, reporting whether it was truncated at
// MaxSegments.
func (t *TrieTemplater) split(rawPath string) ([]string, bool) {
	rawPath = strings.TrimPrefix(rawPath, "/")
	if rawPath == "" {
		return nil, false
	}
	segs := strings.Split(rawPath, "/")
	if len(segs) > t.cfg.MaxSegments {
		return segs[:t.cfg.MaxSegments], true
	}
	return segs, false
}

// buildTemplate joins rendered segments back into a path.
func buildTemplate(parts []string) string {
	if len(parts) == 0 {
		return "/"
	}
	return "/" + strings.Join(parts, "/")
}
