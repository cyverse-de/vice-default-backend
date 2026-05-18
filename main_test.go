package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestApp builds an *App wired against an httptest.Server-backed lookup so
// each test can control the canonical-url response without touching production
// flag parsing. Returns the app, the lookup server, and a counter the test
// can use to assert lookup-count expectations.
func newTestApp(t *testing.T, handler http.HandlerFunc) (*App, *httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	parsed, err := url.Parse(srv.URL)
	require.NoError(t, err)

	app := &App{
		refreshSeconds:   5,
		viceDomainSuffix: ".cyverse.run",
		lookup: &canonicalLookup{
			baseURL: parsed,
			client:  &http.Client{Timeout: 2 * time.Second},
		},
		cache: newLookupCache(15 * time.Second),
	}
	return app, srv, &calls
}

func canonicalHandler(t *testing.T, urlByPath map[string]string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		// Path is /vice/admin/<subdomain>/canonical-url.
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) != 4 || parts[0] != "vice" || parts[1] != "admin" || parts[3] != "canonical-url" {
			http.NotFound(w, r)
			return
		}
		subdomain := parts[2]
		if u, ok := urlByPath[subdomain]; ok {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(canonicalURLResponse{URL: u})
			return
		}
		http.NotFound(w, r)
	}
}

// doRequest issues a request with the given Host header through HandleWaiting.
func doRequest(app *App, host, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	app.HandleWaiting(rec, req)
	return rec
}

func TestHandleWaiting_RedirectOnDifferentCluster(t *testing.T) {
	app, _, calls := newTestApp(t, canonicalHandler(t, map[string]string{
		"a38c27842": "https://a38c27842.sandbox.cyverse.rocks",
	}))

	rec := doRequest(app, "a38c27842.cyverse.run", "/some/path?key=val")

	require.Equal(t, http.StatusFound, rec.Code)
	assert.Equal(t, "https://a38c27842.sandbox.cyverse.rocks/some/path?key=val", rec.Header().Get("Location"))
	assert.Equal(t, int32(1), calls.Load())
}

func TestHandleWaiting_NoRedirectOnSameHost(t *testing.T) {
	app, _, _ := newTestApp(t, canonicalHandler(t, map[string]string{
		"a1": "https://a1.cyverse.run",
	}))

	rec := doRequest(app, "a1.cyverse.run", "/")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "true", rec.Header().Get("X-Vice-Default-Backend"))
}

func TestHandleWaiting_NoRedirectOnNonViceHost(t *testing.T) {
	app, _, calls := newTestApp(t, canonicalHandler(t, nil))

	rec := doRequest(app, "example.com", "/")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "true", rec.Header().Get("X-Vice-Default-Backend"))
	assert.Equal(t, int32(0), calls.Load(), "lookup must be skipped for non-VICE hosts")
}

func TestHandleWaiting_FallsThroughOn404(t *testing.T) {
	app, _, calls := newTestApp(t, canonicalHandler(t, nil))

	rec := doRequest(app, "aunknown.cyverse.run", "/")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int32(1), calls.Load())
	assert.Equal(t, "true", rec.Header().Get("X-Vice-Default-Backend"))
}

func TestHandleWaiting_FallsThroughOn500(t *testing.T) {
	app, _, calls := newTestApp(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})

	rec := doRequest(app, "aerror.cyverse.run", "/")

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int32(1), calls.Load())
}

func TestHandleWaiting_FallsThroughOnTimeout(t *testing.T) {
	// Backend sleeps longer than the client timeout to force a timeout error.
	app, _, _ := newTestApp(t, func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	app.lookup.client.Timeout = 50 * time.Millisecond

	rec := doRequest(app, "aslow.cyverse.run", "/")
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestHandleWaiting_CacheHitSkipsLookup(t *testing.T) {
	app, _, calls := newTestApp(t, canonicalHandler(t, map[string]string{
		"a1": "https://a1.sandbox.cyverse.rocks",
	}))

	for i := 0; i < 5; i++ {
		rec := doRequest(app, "a1.cyverse.run", "/")
		require.Equal(t, http.StatusFound, rec.Code)
	}
	assert.Equal(t, int32(1), calls.Load(), "second-through-fifth requests must hit the cache")
}

func TestHandleWaiting_CacheCaches404(t *testing.T) {
	app, _, calls := newTestApp(t, canonicalHandler(t, nil))

	for i := 0; i < 5; i++ {
		rec := doRequest(app, "amiss.cyverse.run", "/")
		require.Equal(t, http.StatusOK, rec.Code)
	}
	assert.Equal(t, int32(1), calls.Load(), "negative outcomes must also be cached to avoid hammering app-exposer")
}

func TestHandleWaiting_NoLookupWhenDisabled(t *testing.T) {
	// Construct an app with no lookup wired — represents a deploy where
	// --vice-domain/--app-exposer-url were left unset.
	app := &App{refreshSeconds: 5}
	rec := doRequest(app, "a1.cyverse.run", "/")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "true", rec.Header().Get("X-Vice-Default-Backend"))
}

func TestStripPort(t *testing.T) {
	tests := map[string]string{
		"a.b.cyverse.run":      "a.b.cyverse.run",
		"a.b.cyverse.run:4343": "a.b.cyverse.run",
		"127.0.0.1:8080":       "127.0.0.1",
	}
	for in, want := range tests {
		t.Run(in, func(t *testing.T) {
			assert.Equal(t, want, stripPort(in))
		})
	}
}

func TestWithPathAndQuery(t *testing.T) {
	tests := []struct {
		base, path, query, want string
	}{
		{"https://a.example.com", "/foo", "k=v", "https://a.example.com/foo?k=v"},
		{"https://a.example.com:4343", "/", "", "https://a.example.com:4343/"},
		{"https://a.example.com", "", "", "https://a.example.com"},
		{"https://a.example.com/", "/lab/tree/notebook.ipynb", "token=abc", "https://a.example.com/lab/tree/notebook.ipynb?token=abc"},
	}
	for _, tt := range tests {
		t.Run(tt.base+"|"+tt.path, func(t *testing.T) {
			got, err := withPathAndQuery(tt.base, tt.path, tt.query)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRedirectsToSameHost(t *testing.T) {
	assert.True(t, redirectsToSameHost("https://a1.cyverse.run/", "a1.cyverse.run"))
	assert.True(t, redirectsToSameHost("https://A1.CYVERSE.RUN/", "a1.cyverse.run"))
	assert.True(t, redirectsToSameHost("https://a1.cyverse.run:4343/", "a1.cyverse.run:4343"))
	assert.False(t, redirectsToSameHost("https://a1.sandbox.cyverse.rocks/", "a1.cyverse.run"))
	assert.False(t, redirectsToSameHost(":not-a-url:", "a1.cyverse.run"))
}

func TestLookupCacheExpires(t *testing.T) {
	c := newLookupCache(10 * time.Millisecond)
	c.Put("a1", "https://x")
	got, ok := c.Get("a1")
	require.True(t, ok)
	assert.Equal(t, "https://x", got)

	time.Sleep(20 * time.Millisecond)
	_, ok = c.Get("a1")
	assert.False(t, ok, "expired entries must report miss")
}
