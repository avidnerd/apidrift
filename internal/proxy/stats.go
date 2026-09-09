package proxy

import (
	"encoding/json"
	"net/http"

	"github.com/avidnerd/apidrift/internal/sample"
)

// Stats is a snapshot of the proxy's lifetime counters.
//
// Every response the proxy sees lands in exactly one of Sampled, Dropped, or
// one of the Skipped buckets, so the buckets account for the whole observed
// stream and each blind spot is separately visible. Requests can exceed their
// sum while responses are still in flight.
type Stats struct {
	// Requests is how many requests the proxy has forwarded.
	Requests uint64 `json:"requests"`
	// Sampled is how many response bodies reached the sink.
	Sampled uint64 `json:"sampled"`
	// Dropped is how many eligible bodies the sink refused, almost always
	// because the analysis pipeline was saturated. This is the number that
	// says observation quality is degrading.
	Dropped uint64 `json:"dropped"`
	// SkippedNonJSON counts responses whose Content-Type was not JSON.
	SkippedNonJSON uint64 `json:"skipped_non_json"`
	// SkippedEncoded counts JSON responses still under a content coding such
	// as gzip, which the proxy does not decode.
	SkippedEncoded uint64 `json:"skipped_encoded"`
	// SkippedOversize counts JSON responses larger than the body cap.
	SkippedOversize uint64 `json:"skipped_oversize"`
	// SkippedIncomplete counts bodies closed before EOF, typically because the
	// client hung up mid-response.
	SkippedIncomplete uint64 `json:"skipped_incomplete"`
	// UpstreamErrors counts requests the upstream failed to answer.
	UpstreamErrors uint64 `json:"upstream_errors"`
	// Queue is the sink's own snapshot, present when the sink reports one.
	Queue *sample.QueueStats `json:"queue,omitempty"`
}

// queueReporter is implemented by sinks that can describe their own state, such
// as sample.Queue. The proxy asks for it rather than requiring it, so any Sink
// remains usable.
type queueReporter interface {
	Stats() sample.QueueStats
}

// Stats returns a snapshot of the proxy's counters. Counters are read
// independently, so a snapshot taken under load may be very slightly
// inconsistent between fields; it is for monitoring, not accounting.
func (p *Proxy) Stats() Stats {
	s := Stats{
		Requests:          p.c.requests.Load(),
		Sampled:           p.c.sampled.Load(),
		Dropped:           p.c.dropped.Load(),
		SkippedNonJSON:    p.c.skippedNonJSON.Load(),
		SkippedEncoded:    p.c.skippedEncoded.Load(),
		SkippedOversize:   p.c.skippedOversize.Load(),
		SkippedIncomplete: p.c.skippedIncomplete.Load(),
		UpstreamErrors:    p.c.upstreamErrors.Load(),
	}
	if qr, ok := p.sink.(queueReporter); ok {
		qs := qr.Stats()
		s.Queue = &qs
	}
	return s
}

// StatsHandler serves Stats as JSON.
//
// It is deliberately a separate handler rather than a path the proxy
// intercepts: reserving /stats on the proxy itself would shadow an upstream
// route of the same name and silently change the proxied API. The CLI mounts
// this on its own admin listener.
func (p *Proxy) StatsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		// A write failure here means the monitoring client went away; there is
		// nothing useful to do about it and nothing to report to.
		_ = enc.Encode(p.Stats())
	})
}
