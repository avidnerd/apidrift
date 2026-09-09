// Package proxy implements the reverse proxy that sits in front of an upstream
// service and tees response bodies into the analysis pipeline.
//
// Analysis is strictly off the request path: a saturated pipeline degrades
// observation quality, never client latency or correctness.
//
// Implemented in phase 1.
package proxy
