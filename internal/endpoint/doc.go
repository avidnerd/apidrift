// Package endpoint groups raw request paths into logical endpoints by learning
// which path segments are variables (identifiers) and which are static routes.
//
// Grouping is a prerequisite for drift detection: /users/8123/orders and
// /users/9944/orders must share a schema, while /docs/pricing and /docs/auth
// must not, even though both positions have high cardinality.
//
// Implemented in phase 2, alongside the storage that is keyed by Key.
package endpoint
