// Package sample defines the unit of observed traffic that flows from the proxy
// into the analysis pipeline, plus the bounded queue that carries it.
//
// The queue is deliberately lossy: the proxy must never block on analysis, so a
// full queue drops the sample and increments a counter rather than applying
// backpressure to the client request.
//
// Implemented in phase 1.
package sample
