package endpoint

import "strconv"

// Variable is the placeholder a learned variable path segment renders as.
// Every variable segment renders identically: the templater's job is grouping,
// and a name like "{uuid}" would assert something about the value's meaning
// that the templater has no basis for.
const Variable = "{id}"

// Overflow is the placeholder for everything past MaxSegments in a path deeper
// than the templater tracks. It is distinct from Variable so an unusually deep
// path is visibly truncated rather than silently mistaken for a variable.
const Overflow = "{...}"

// Key identifies a logical endpoint: one method, one path template, one status
// class. StatusClass is 2 for 2xx, 4 for 4xx, and so on.
//
// Status class is part of the identity because a 200 body and a 404 body are
// different documents. Merging them would make every field of each look
// intermittently absent, which reads as drift and is not.
type Key struct {
	Method      string `json:"method"`
	Template    string `json:"template"` // e.g. "/users/{id}/orders"
	StatusClass int    `json:"status_class"`
}

// String renders the key for logs and reports, e.g. "GET /users/{id}/orders 2xx".
func (k Key) String() string {
	return k.Method + " " + k.Template + " " + strconv.Itoa(k.StatusClass) + "xx"
}

// Templater learns which path segments are variables and groups raw paths into
// endpoints accordingly.
//
// Implementations must be safe for concurrent use.
type Templater interface {
	// Observe records a raw path so the templater can learn which segments vary.
	Observe(method, rawPath string)
	// Resolve returns the endpoint template for a raw path. ok is false if the
	// templater hasn't seen enough samples to decide yet.
	Resolve(method, rawPath string, status int) (Key, bool)
}
