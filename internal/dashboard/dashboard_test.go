package dashboard_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/avidnerd/apidrift/internal/dashboard"
)

func TestHandler(t *testing.T) {
	h := dashboard.Handler()

	t.Run("serves the page at the root", func(t *testing.T) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Errorf("Content-Type = %q, want text/html", ct)
		}
		body := rec.Body.String()
		for _, want := range []string{"<title>apidrift</title>", "/findings", "/stats", "/candidates"} {
			if !strings.Contains(body, want) {
				t.Errorf("page is missing %q", want)
			}
		}
	})

	// The page has to work with no network, because a monitoring tool that
	// needs a CDN goes blank during the outage you deployed it for.
	t.Run("loads nothing from the network", func(t *testing.T) {
		body := rec(h, "/").Body.String()
		for _, bad := range []string{"https://", "http://cdn", "//cdn.", "<script src"} {
			if strings.Contains(body, bad) {
				t.Errorf("page references an external resource (%q); it must be self-contained", bad)
			}
		}
	})

	t.Run("does not swallow other paths", func(t *testing.T) {
		if got := rec(h, "/stats").Code; got != http.StatusNotFound {
			t.Errorf("GET /stats through the dashboard handler = %d, want 404 so the mux can route it", got)
		}
	})

	t.Run("rejects writes", func(t *testing.T) {
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest(http.MethodPost, "/", nil))
		if r.Code != http.StatusMethodNotAllowed {
			t.Errorf("POST / = %d, want 405", r.Code)
		}
	})
}

func rec(h http.Handler, path string) *httptest.ResponseRecorder {
	r := httptest.NewRecorder()
	h.ServeHTTP(r, httptest.NewRequest(http.MethodGet, path, nil))
	return r
}
