// Package schema turns JSON response bodies into a flat, mergeable description
// of their structure: for each JSON path, how often the key was present, how
// often it was explicitly null, which types it held, and which string values it
// took.
//
// Ownership note: Extract is implemented here. Merge is intentionally left as a
// stub for the repository owner to implement; see README.md in this package.
package schema
