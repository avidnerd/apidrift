package endpoint_test

import (
	"fmt"
	"sync"
	"testing"

	"github.com/avidnerd/apidrift/internal/endpoint"
)

// observeAll feeds every path to the templater n times over, which is how real
// traffic arrives: repeatedly, and interleaved across routes.
func observeAll(tm endpoint.Templater, method string, n int, paths ...string) {
	for i := 0; i < n; i++ {
		for _, p := range paths {
			tm.Observe(method, p)
		}
	}
}

func mustResolve(t *testing.T, tm endpoint.Templater, method, path string, status int) endpoint.Key {
	t.Helper()

	k, ok := tm.Resolve(method, path, status)
	if !ok {
		t.Fatalf("Resolve(%s %s) = not ok, want a key", method, path)
	}
	return k
}

func TestKeyString(t *testing.T) {
	tests := []struct {
		name string
		key  endpoint.Key
		want string
	}{
		{"success", endpoint.Key{Method: "GET", Template: "/users/{id}/orders", StatusClass: 2}, "GET /users/{id}/orders 2xx"},
		{"client error", endpoint.Key{Method: "POST", Template: "/v1/charges", StatusClass: 4}, "POST /v1/charges 4xx"},
		{"root", endpoint.Key{Method: "GET", Template: "/", StatusClass: 2}, "GET / 2xx"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.key.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestVariableSegmentsCollapse covers the identifier shapes the templater must
// recognise: paths differing only in an identifier must share one template.
func TestVariableSegmentsCollapse(t *testing.T) {
	tests := []struct {
		name         string
		paths        []string
		resolve      string
		wantTemplate string
	}{
		{
			name:         "numeric ids",
			paths:        []string{"/users/8123/orders", "/users/9944/orders", "/users/17/orders"},
			resolve:      "/users/8123/orders",
			wantTemplate: "/users/{id}/orders",
		},
		{
			name: "uuids",
			paths: []string{
				"/v1/accounts/3f2504e0-4f89-11d3-9a0c-0305e82c3301",
				"/v1/accounts/886313e1-3b8a-5372-9b90-0c9aee199e5d",
				"/v1/accounts/9c5b94b1-35ad-49bb-b118-8e8fc24abf80",
			},
			resolve:      "/v1/accounts/3f2504e0-4f89-11d3-9a0c-0305e82c3301",
			wantTemplate: "/v1/accounts/{id}",
		},
		{
			name: "hex digests",
			paths: []string{
				"/objects/a94a8fe5ccb19ba61c4c0873d391e987982fbbd3",
				"/objects/da39a3ee5e6b4b0d3255bfef95601890afd80709",
				"/objects/356a192b7913b04c54574d18c28d46e6395428ab",
			},
			resolve:      "/objects/a94a8fe5ccb19ba61c4c0873d391e987982fbbd3",
			wantTemplate: "/objects/{id}",
		},
		{
			name: "prefixed opaque ids",
			paths: []string{
				"/v1/customers/cus_QxYzABCDEFgh/sources",
				"/v1/customers/cus_1a2b3c4d5e6f/sources",
				"/v1/customers/cus_ZZ99YY88XX77/sources",
			},
			resolve:      "/v1/customers/cus_QxYzABCDEFgh/sources",
			wantTemplate: "/v1/customers/{id}/sources",
		},
		{
			name: "two variable segments in one path",
			paths: []string{
				"/users/8123/orders/551",
				"/users/9944/orders/552",
				"/users/17/orders/553",
			},
			resolve:      "/users/8123/orders/551",
			wantTemplate: "/users/{id}/orders/{id}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := endpoint.NewTrieTemplater(endpoint.Config{})
			observeAll(tm, "GET", 100, tt.paths...)

			// Every observed path must resolve to the same template.
			for _, p := range tt.paths {
				k := mustResolve(t, tm, "GET", p, 200)
				if k.Template != tt.wantTemplate {
					t.Errorf("Resolve(%q).Template = %q, want %q", p, k.Template, tt.wantTemplate)
				}
			}

			k := mustResolve(t, tm, "GET", tt.resolve, 200)
			if k.Method != "GET" || k.StatusClass != 2 {
				t.Errorf("key = %+v, want Method GET and StatusClass 2", k)
			}
		})
	}
}

// TestStaticRoutesDoNotCollapse is the counterweight to the previous test: a
// position holding many distinct route names is static, however many names it
// holds, and each must keep its own endpoint.
func TestStaticRoutesDoNotCollapse(t *testing.T) {
	t.Run("a handful of documentation routes", func(t *testing.T) {
		tm := endpoint.NewTrieTemplater(endpoint.Config{})
		paths := []string{"/docs/webhooks", "/docs/pricing", "/docs/auth"}
		observeAll(tm, "GET", 100, paths...)

		for _, p := range paths {
			k := mustResolve(t, tm, "GET", p, 200)
			if k.Template != p {
				t.Errorf("Resolve(%q).Template = %q, want the literal path", p, k.Template)
			}
		}
	})

	t.Run("high cardinality that plateaus is still static", func(t *testing.T) {
		// Forty documentation pages under one prefix. High cardinality in that
		// position, but the set stops growing, and heavy traffic makes the
		// plateau visible.
		const pages = 40
		paths := make([]string, 0, pages)
		for i := 0; i < pages; i++ {
			paths = append(paths, fmt.Sprintf("/docs/guide-%d", i))
		}

		tm := endpoint.NewTrieTemplater(endpoint.Config{})
		observeAll(tm, "GET", 50, paths...) // 2000 observations over 40 routes

		seen := make(map[string]bool)
		for _, p := range paths {
			k := mustResolve(t, tm, "GET", p, 200)
			if k.Template != p {
				t.Errorf("Resolve(%q).Template = %q, want the literal path", p, k.Template)
			}
			seen[k.Template] = true
		}
		if len(seen) != pages {
			t.Errorf("%d distinct templates, want %d -- static routes were merged", len(seen), pages)
		}
	})
}

// TestPlateauVersusGrowth is the statistical tier, isolated: two positions with
// identical traffic volume and non-identifier-shaped values, separated only by
// whether their distinct-value count keeps up with traffic.
func TestPlateauVersusGrowth(t *testing.T) {
	const requests = 1200

	tests := []struct {
		name string
		// path returns the i'th request's path.
		path         func(i int) string
		probe        string
		wantTemplate string
	}{
		{
			name:         "a plateaued set stays static",
			path:         func(i int) string { return fmt.Sprintf("/blog/topic-%d", i%12) },
			probe:        "/blog/topic-3",
			wantTemplate: "/blog/topic-3",
		},
		{
			name:         "a set that keeps growing becomes a variable",
			path:         func(i int) string { return fmt.Sprintf("/blog/post-title-number-%d", i) },
			probe:        "/blog/post-title-number-7",
			wantTemplate: "/blog/{id}",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := endpoint.NewTrieTemplater(endpoint.Config{})
			for i := 0; i < requests; i++ {
				tm.Observe("GET", tt.path(i))
			}

			k := mustResolve(t, tm, "GET", tt.probe, 200)
			if k.Template != tt.wantTemplate {
				t.Errorf("Resolve(%q).Template = %q, want %q", tt.probe, k.Template, tt.wantTemplate)
			}
		})
	}
}

// TestResolveUndecidedBelowThreshold covers the promise that the templater
// declines to answer rather than answering wrongly.
func TestResolveUndecidedBelowThreshold(t *testing.T) {
	tests := []struct {
		name    string
		cfg     endpoint.Config
		observe int
		wantOK  bool
	}{
		{"one observation", endpoint.Config{}, 1, false},
		{"just under the threshold", endpoint.Config{MinSamples: 50}, 49, false},
		{"exactly at the threshold", endpoint.Config{MinSamples: 50}, 50, true},
		{"well past the threshold", endpoint.Config{MinSamples: 50}, 200, true},
		{"a raised threshold is respected", endpoint.Config{MinSamples: 300}, 299, false},
		{"a raised threshold is reached", endpoint.Config{MinSamples: 300}, 300, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := endpoint.NewTrieTemplater(tt.cfg)
			for i := 0; i < tt.observe; i++ {
				tm.Observe("GET", fmt.Sprintf("/users/%d/orders", i))
			}

			_, ok := tm.Resolve("GET", "/users/1/orders", 200)
			if ok != tt.wantOK {
				t.Errorf("Resolve() ok = %v after %d observations, want %v", ok, tt.observe, tt.wantOK)
			}
		})
	}
}

func TestResolveUnobservedPaths(t *testing.T) {
	tm := endpoint.NewTrieTemplater(endpoint.Config{})
	observeAll(tm, "GET", 100, "/docs/pricing", "/docs/auth")

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{"an unseen method", "POST", "/docs/pricing"},
		{"an unseen static route", "GET", "/docs/webhooks"},
		{"an unseen prefix", "GET", "/blog/pricing"},
		{"a path deeper than anything observed", "GET", "/docs/pricing/details"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, ok := tm.Resolve(tt.method, tt.path, 200); ok {
				t.Errorf("Resolve(%s %s) = ok, want not ok", tt.method, tt.path)
			}
		})
	}
}

// TestTriePrefixSeparation is why the templater is a trie and not a table of
// path positions: the same index is a variable under one prefix and a route
// name under another.
func TestTriePrefixSeparation(t *testing.T) {
	tm := endpoint.NewTrieTemplater(endpoint.Config{})
	observeAll(tm, "GET", 100,
		"/users/8123", "/users/9944", "/users/17",
		"/docs/pricing", "/docs/auth", "/docs/webhooks",
	)

	tests := []struct {
		path string
		want string
	}{
		{"/users/8123", "/users/{id}"},
		{"/users/9944", "/users/{id}"},
		{"/docs/pricing", "/docs/pricing"},
		{"/docs/auth", "/docs/auth"},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if k := mustResolve(t, tm, "GET", tt.path, 200); k.Template != tt.want {
				t.Errorf("Resolve(%q).Template = %q, want %q", tt.path, k.Template, tt.want)
			}
		})
	}
}

func TestMethodsAreIndependent(t *testing.T) {
	tm := endpoint.NewTrieTemplater(endpoint.Config{})
	observeAll(tm, "GET", 100, "/v1/charges/ch_1a2b3c4d5e6f")
	observeAll(tm, "POST", 100, "/v1/charges")

	if k := mustResolve(t, tm, "GET", "/v1/charges/ch_1a2b3c4d5e6f", 200); k.Method != "GET" {
		t.Errorf("GET key method = %q, want GET", k.Method)
	}
	if k := mustResolve(t, tm, "POST", "/v1/charges", 201); k.Method != "POST" || k.Template != "/v1/charges" {
		t.Errorf("POST key = %+v, want POST /v1/charges", k)
	}
	// POST traffic never went to the identifier route, so it cannot resolve it.
	if _, ok := tm.Resolve("POST", "/v1/charges/ch_1a2b3c4d5e6f", 200); ok {
		t.Error("Resolve(POST /v1/charges/{id}) = ok, want not ok: no POST traffic went there")
	}
}

func TestStatusClass(t *testing.T) {
	tm := endpoint.NewTrieTemplater(endpoint.Config{})
	observeAll(tm, "GET", 100, "/v1/charges/ch_1a2b3c4d5e6f")

	tests := []struct {
		status int
		want   int
	}{
		{200, 2}, {201, 2}, {301, 3}, {404, 4}, {429, 4}, {500, 5}, {503, 5},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprint(tt.status), func(t *testing.T) {
			k := mustResolve(t, tm, "GET", "/v1/charges/ch_1a2b3c4d5e6f", tt.status)
			if k.StatusClass != tt.want {
				t.Errorf("StatusClass for %d = %d, want %d", tt.status, k.StatusClass, tt.want)
			}
		})
	}
}

func TestPathEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		observe string
		resolve string
		want    string
	}{
		{"root", "/", "/", "/"},
		{"empty path is treated as root", "", "", "/"},
		{"trailing slash keeps its empty segment", "/docs/", "/docs/", "/docs/"},
		{"a dotted filename stays literal", "/openapi.json", "/openapi.json", "/openapi.json"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := endpoint.NewTrieTemplater(endpoint.Config{})
			observeAll(tm, "GET", 100, tt.observe)

			k := mustResolve(t, tm, "GET", tt.resolve, 200)
			if k.Template != tt.want {
				t.Errorf("Resolve(%q).Template = %q, want %q", tt.resolve, k.Template, tt.want)
			}
		})
	}
}

// TestDeepPathsAreTruncated covers the depth bound: a path deeper than
// MaxSegments is visibly cut short rather than tracked without limit.
func TestDeepPathsAreTruncated(t *testing.T) {
	tm := endpoint.NewTrieTemplater(endpoint.Config{MaxSegments: 3})
	observeAll(tm, "GET", 100, "/a/b/c/d/e/f")

	k := mustResolve(t, tm, "GET", "/a/b/c/d/e/f", 200)
	if want := "/a/b/c/{...}"; k.Template != want {
		t.Errorf("Template = %q, want %q", k.Template, want)
	}
	if st := tm.Stats(); st.TruncatedPaths != 100 {
		t.Errorf("Stats().TruncatedPaths = %d, want 100", st.TruncatedPaths)
	}
}

// TestChildLimitIsBounded covers the per-position memory bound, and that
// hitting it is counted rather than silent.
func TestChildLimitIsBounded(t *testing.T) {
	const cap = 16
	tm := endpoint.NewTrieTemplater(endpoint.Config{MaxChildrenPerNode: cap})

	// Non-identifier literals, so nothing routes to the wildcard child and
	// every distinct value would otherwise want its own node.
	for i := 0; i < 200; i++ {
		tm.Observe("GET", fmt.Sprintf("/docs/guide-%d", i))
	}

	st := tm.Stats()
	if st.ChildLimitHits == 0 {
		t.Error("Stats().ChildLimitHits = 0, want the cap to have been hit")
	}
	// One root, one "docs" node, and at most cap children beneath it.
	if maxNodes := 2 + cap; st.Nodes > maxNodes {
		t.Errorf("Stats().Nodes = %d, want at most %d", st.Nodes, maxNodes)
	}
}

// TestNodeLimitIsBounded covers the whole-trie memory bound.
func TestNodeLimitIsBounded(t *testing.T) {
	const limit = 10
	tm := endpoint.NewTrieTemplater(endpoint.Config{MaxNodes: limit})

	for i := 0; i < 500; i++ {
		tm.Observe("GET", fmt.Sprintf("/docs/guide-%d", i))
	}

	st := tm.Stats()
	if st.Nodes > limit {
		t.Errorf("Stats().Nodes = %d, want at most %d", st.Nodes, limit)
	}
	if st.NodeLimitHits == 0 {
		t.Error("Stats().NodeLimitHits = 0, want the limit to have been hit")
	}
}

// TestCollapseReclaimsNodes covers what happens when a position that looked
// static turns out to be a variable: the literal children are discarded, and
// the trie gets smaller rather than larger.
func TestCollapseReclaimsNodes(t *testing.T) {
	tm := endpoint.NewTrieTemplater(endpoint.Config{})

	// Below the statistical threshold, growing slugs still look like routes.
	for i := 0; i < 400; i++ {
		tm.Observe("GET", fmt.Sprintf("/blog/post-title-number-%d/comments", i))
	}
	before := tm.Stats()
	if before.Collapses != 0 {
		t.Fatalf("Stats().Collapses = %d before the threshold, want 0", before.Collapses)
	}

	// Past it, the position flips and everything learned beneath it is dropped.
	for i := 400; i < 1200; i++ {
		tm.Observe("GET", fmt.Sprintf("/blog/post-title-number-%d/comments", i))
	}
	after := tm.Stats()

	if after.Collapses == 0 {
		t.Error("Stats().Collapses = 0 after the threshold, want at least 1")
	}
	if after.Nodes >= before.Nodes {
		t.Errorf("Stats().Nodes went %d -> %d, want the collapse to reclaim nodes", before.Nodes, after.Nodes)
	}
	if k := mustResolve(t, tm, "GET", "/blog/post-title-number-9/comments", 200); k.Template != "/blog/{id}/comments" {
		t.Errorf("Template = %q, want /blog/{id}/comments", k.Template)
	}
}

// TestVariableVerdictIsSticky guards that a position judged to hold identifiers
// keeps that judgement, so an endpoint key does not change meaning under it.
func TestVariableVerdictIsSticky(t *testing.T) {
	tm := endpoint.NewTrieTemplater(endpoint.Config{})
	observeAll(tm, "GET", 100, "/users/8123", "/users/9944")

	if k := mustResolve(t, tm, "GET", "/users/8123", 200); k.Template != "/users/{id}" {
		t.Fatalf("Template = %q, want /users/{id}", k.Template)
	}

	// A literal now arrives at that position. It must be templated as a
	// variable too, rather than splitting off a new endpoint.
	for i := 0; i < 1000; i++ {
		tm.Observe("GET", "/users/me")
	}
	if k := mustResolve(t, tm, "GET", "/users/me", 200); k.Template != "/users/{id}" {
		t.Errorf("Resolve(/users/me).Template = %q, want /users/{id}: the verdict must be sticky", k.Template)
	}
}

// TestConcurrentUse is the race-detector case.
func TestConcurrentUse(t *testing.T) {
	tm := endpoint.NewTrieTemplater(endpoint.Config{})

	const workers = 8
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 300; i++ {
				tm.Observe("GET", fmt.Sprintf("/users/%d/orders", w*1000+i))
				tm.Observe("GET", "/docs/pricing")
				tm.Resolve("GET", "/users/1/orders", 200)
				tm.Resolve("GET", "/docs/pricing", 200)
			}
		}(w)
	}
	wg.Wait()

	if k := mustResolve(t, tm, "GET", "/users/1/orders", 200); k.Template != "/users/{id}/orders" {
		t.Errorf("Template = %q, want /users/{id}/orders", k.Template)
	}
	if st := tm.Stats(); st.Observations != workers*300*2 {
		t.Errorf("Stats().Observations = %d, want %d", st.Observations, workers*300*2)
	}
}
