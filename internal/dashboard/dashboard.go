// Package dashboard serves the web view of a running apidrift.
//
// The page is embedded in the binary rather than read from disk, so a deployed
// apidrift is one file with no asset directory to lose. It has no dependencies
// and loads nothing from the network: a monitoring tool that needs a CDN to
// render is a monitoring tool that goes blank during the outage you deployed it
// for.
package dashboard

import (
	"bytes"
	_ "embed"
	"net/http"
	"time"
)

//go:embed index.html
var indexHTML []byte

// buildTime gives the embedded page a stable modification time so conditional
// requests work and a browser is not re-fetching it every five seconds.
var buildTime = time.Now()

// Handler serves the dashboard. It answers only GET and HEAD, and only at the
// root path, so a request for anything else falls through to the mux rather
// than being swallowed here.
func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "index.html", buildTime, bytes.NewReader(indexHTML))
	})
}
