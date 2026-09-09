package sample

import "time"

// MaxBodyBytes is the default cap on how much of a response body apidrift
// retains for analysis.
//
// Bodies larger than this are not sampled at all rather than sampled in part:
// a JSON document cut off mid-object does not parse, so a truncated sample
// would contribute nothing but a parse error. The proxy counts these as
// oversize skips so the blind spot stays visible in /stats.
const MaxBodyBytes = 256 << 10 // 256 KiB

// Sample is one observed HTTP response, carried from the proxy to the analysis
// pipeline. It is immutable once offered to a Sink; nothing downstream may
// modify Body.
type Sample struct {
	// Method is the HTTP method of the request that produced the response.
	Method string
	// RawPath is the un-templated request path as the client sent it, e.g.
	// "/users/8123/orders". Templating happens later, in package endpoint.
	RawPath string
	// Status is the HTTP status code of the response.
	Status int
	// Body is the complete response body. Its length never exceeds the
	// MaxBodyBytes limit in force when the sample was taken.
	Body []byte
	// ObservedAt is when the response body finished being read.
	ObservedAt time.Time
}

// StatusClass returns the leading digit of the status code: 2 for 2xx, 4 for
// 4xx, and so on. Responses in different classes carry different shapes and are
// tracked as different endpoints.
func (s Sample) StatusClass() int { return s.Status / 100 }
