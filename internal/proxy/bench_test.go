package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sort"
	"testing"
	"time"

	"github.com/avidnerd/apidrift/internal/proxy"
	"github.com/avidnerd/apidrift/internal/sample"
)

// benchBody is a Stripe-shaped response of roughly two kilobytes: representative
// of the payload sizes apidrift actually sees, and large enough that the tee's
// copy is not lost in the noise.
const benchBody = `{
  "id": "ch_3PxYzABCDEFghijk0LmNoPqR",
  "object": "charge",
  "amount": 2000,
  "amount_captured": 2000,
  "amount_refunded": 0,
  "balance_transaction": "txn_3PxYzABCDEFghijk0LmNoPqR",
  "billing_details": {
    "address": {"city": "Cambridge", "country": "US", "line1": "1 Main St", "line2": null, "postal_code": "02139", "state": "MA"},
    "email": "jenny.rosen@example.com",
    "name": "Jenny Rosen",
    "phone": null
  },
  "captured": true,
  "created": 1724457600,
  "currency": "usd",
  "customer": "cus_QxYzABCDEFghij",
  "description": "Subscription creation",
  "disputed": false,
  "livemode": false,
  "metadata": {"order_id": "6735", "channel": "web"},
  "outcome": {"network_status": "approved_by_network", "reason": null, "risk_level": "normal", "risk_score": 12, "seller_message": "Payment complete.", "type": "authorized"},
  "paid": true,
  "payment_method": "pm_1PxYzABCDEFghijk0LmNoPqR",
  "payment_method_details": {
    "card": {"brand": "visa", "checks": {"address_line1_check": "pass", "address_postal_code_check": "pass", "cvc_check": "pass"}, "country": "US", "exp_month": 8, "exp_year": 2027, "fingerprint": "Xt5EWLLDS7FJjR1c", "funding": "credit", "last4": "4242", "network": "visa", "three_d_secure": null, "wallet": null},
    "type": "card"
  },
  "receipt_url": "https://pay.stripe.com/receipts/payment/example",
  "refunded": false,
  "status": "succeeded"
}`

// BenchmarkProxyLatency measures the latency apidrift adds to a client request.
//
// The upstream is local, so the numbers isolate the proxy's own cost -- the tee
// copy, the hand-off, the extra hop -- rather than being swamped by network
// time. The saturated case is the one that matters: it is the shape of a
// production incident, where analysis cannot keep up and the question is
// whether client requests suffer for it.
func BenchmarkProxyLatency(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, benchBody)
	}))
	defer upstream.Close()

	target, err := url.Parse(upstream.URL)
	if err != nil {
		b.Fatalf("parsing upstream URL: %v", err)
	}

	// Baseline: the client talking to the upstream with no proxy in between.
	b.Run("direct", func(b *testing.B) {
		benchLatency(b, upstream.URL)
	})

	// A bare httputil.ReverseProxy with no capture at all. Deploying apidrift
	// means accepting a proxy hop whatever it does with the bytes, so this --
	// not "direct" -- is the baseline that isolates the cost of the analysis.
	b.Run("reverse_proxy_only", func(b *testing.B) {
		bare := httptest.NewServer(&httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) { pr.SetURL(target) },
		})
		defer bare.Close()
		benchLatency(b, bare.URL)
	})

	// Drained: the analysis pipeline is keeping up, so every response is
	// captured and handed off.
	b.Run("proxied_drained", func(b *testing.B) {
		q := sample.NewQueue(1024)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for range q.C() {
			}
		}()
		defer func() {
			q.Close()
			<-done
		}()

		front := frontFor(b, target, q)
		defer front.Close()
		benchLatency(b, front.URL)

		if st := q.Stats(); st.Enqueued == 0 {
			b.Fatalf("drained run enqueued nothing (stats %+v); the benchmark is not measuring capture", st)
		}
	})

	// Saturated: nothing is draining the queue, so it is full and every sample
	// is dropped. This is the number the phase-1 promise rests on.
	b.Run("proxied_saturated", func(b *testing.B) {
		q := sample.NewQueue(1) // fills on the first response, drops forever after
		front := frontFor(b, target, q)
		defer front.Close()
		benchLatency(b, front.URL)

		if st := q.Stats(); st.Dropped == 0 {
			b.Fatalf("saturated run dropped nothing (stats %+v); the queue was not saturated", st)
		}
	})
}

// frontFor stands a proxy in front of target, feeding sink.
func frontFor(b *testing.B, target *url.URL, sink sample.Sink) *httptest.Server {
	b.Helper()

	p, err := proxy.New(proxy.Options{Upstream: target, Sink: sink})
	if err != nil {
		b.Fatalf("proxy.New: %v", err)
	}
	return httptest.NewServer(p)
}

// benchLatency issues b.N sequential requests against addr and reports the
// median and 99th percentile round trip alongside the usual ns/op.
//
// Requests are sequential on purpose: percentiles from a saturated parallel
// client would measure queueing in the load generator, not latency added by the
// proxy.
func benchLatency(b *testing.B, addr string) {
	b.Helper()

	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 2}}
	defer client.CloseIdleConnections()

	// Warm the connection so TCP setup is not counted in the first sample.
	if err := doOnce(client, addr); err != nil {
		b.Fatalf("warmup request: %v", err)
	}

	durations := make([]time.Duration, b.N)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		if err := doOnce(client, addr); err != nil {
			b.Fatalf("request %d: %v", i, err)
		}
		durations[i] = time.Since(start)
	}
	b.StopTimer()

	reportPercentiles(b, durations)
}

// doOnce performs one request and reads the body to completion, as a real
// client does -- the tee only finishes when the body does.
func doOnce(client *http.Client, addr string) error {
	resp, err := client.Get(addr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &url.Error{Op: "GET", URL: addr, Err: http.ErrMissingBoundary}
	}
	return nil
}

// reportPercentiles adds p50 and p99 in microseconds to the benchmark output.
func reportPercentiles(b *testing.B, d []time.Duration) {
	b.Helper()

	if len(d) == 0 {
		return
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })

	pick := func(q float64) float64 {
		i := int(float64(len(d)) * q)
		if i >= len(d) {
			i = len(d) - 1
		}
		return float64(d[i].Nanoseconds()) / 1000
	}
	b.ReportMetric(pick(0.50), "p50-us")
	b.ReportMetric(pick(0.99), "p99-us")
}
