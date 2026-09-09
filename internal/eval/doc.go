// Package eval measures how well the detector works: it diffs two OpenAPI specs
// into a ground-truth changelist, generates synthetic traffic conforming to each
// version, runs it through the pipeline, and scores the findings.
//
// Ownership note: Score is intentionally left as a stub for the repository
// owner to implement. Everything else in this package is implemented here.
// See README.md in this package.
package eval
