// Package store holds observed schemas in a fixed-size ring of hourly buckets
// per endpoint, and answers window queries by merging the buckets in range.
//
// Memory is bounded by endpoints x fields x buckets; both the cap and the
// eviction policy for cold endpoints are documented in DECISIONS.md.
//
// Implemented in phase 2.
package store
