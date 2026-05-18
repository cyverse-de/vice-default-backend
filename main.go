// Package main implements vice-default-backend, the catch-all backend for
// VICE analysis subdomains. When a request lands on a `*.<vice-domain>`
// subdomain whose analysis-specific HTTPRoute is not present on this cluster,
// the default backend looks the subdomain up via app-exposer to discover which
// operator (cluster) owns the analysis. If the owner is a different cluster,
// the browser is 302'd to the matching base URL there so the owning operator's
// loading page can take over. Otherwise (or on any lookup failure) the
// existing waiting page is served, which periodically reloads until the
// analysis-specific HTTPRoute takes over.
package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cyverse-de/app-exposer/common"
	"github.com/sirupsen/logrus"
)

var log = common.Log

//go:embed templates/waiting.html
var waitingTemplateFS embed.FS

// waitingTemplate is the parsed waiting page template.
var waitingTemplate = template.Must(template.ParseFS(waitingTemplateFS, "templates/waiting.html"))

// waitingPageData holds the template data for the waiting page.
type waitingPageData struct {
	// RefreshSeconds is the interval between page reloads.
	RefreshSeconds int
}

// canonicalURLResponse mirrors the JSON body returned by app-exposer's
// GET /vice/admin/{host}/canonical-url endpoint.
type canonicalURLResponse struct {
	URL string `json:"url"`
}

// canonicalLookup discovers the canonical user-facing URL for a VICE
// subdomain by calling app-exposer's admin canonical-url endpoint.
type canonicalLookup struct {
	baseURL *url.URL
	client  *http.Client
}

func (l *canonicalLookup) Lookup(ctx context.Context, subdomain string) (string, error) {
	reqURL := l.baseURL.JoinPath("vice", "admin", subdomain, "canonical-url").String()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close

	if resp.StatusCode == http.StatusNotFound {
		return "", errNotFound
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("canonical-url returned %d", resp.StatusCode)
	}

	var body canonicalURLResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", fmt.Errorf("decoding canonical-url response: %w", err)
	}
	if body.URL == "" {
		return "", fmt.Errorf("canonical-url returned empty url")
	}
	return body.URL, nil
}

// errNotFound is returned by canonicalLookup.Lookup when app-exposer responds
// 404 (no operator owns the subdomain, or owner has no base_url). Callers
// cache this outcome so the reload loop doesn't re-query for every reload.
var errNotFound = errors.New("subdomain has no owning operator")

// cacheEntry stores the outcome of a canonical-url lookup. A non-empty url
// triggers a redirect; an empty url means "fall through to the waiting page"
// (either no match, or the owner is on this cluster).
type cacheEntry struct {
	url string
	exp time.Time
}

// lookupCache memoizes canonical-url outcomes per subdomain so the waiting
// page's tight reload loop doesn't hammer app-exposer.
type lookupCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry
	ttl     time.Duration
}

func newLookupCache(ttl time.Duration) *lookupCache {
	return &lookupCache{
		entries: make(map[string]cacheEntry),
		ttl:     ttl,
	}
}

// Get returns the cached redirect URL (which may be empty for "no redirect")
// and ok=true when a non-expired entry is present.
func (c *lookupCache) Get(subdomain string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[subdomain]
	if !ok || time.Now().After(entry.exp) {
		return "", false
	}
	return entry.url, true
}

// Put records a lookup outcome under subdomain. An empty url means "no
// redirect — fall through to the waiting page".
func (c *lookupCache) Put(subdomain, redirectURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[subdomain] = cacheEntry{
		url: redirectURL,
		exp: time.Now().Add(c.ttl),
	}
}

// App contains the HTTP handlers for the default backend.
type App struct {
	refreshSeconds int

	// viceDomainSuffix is "."+vice-domain, used as the Host-header suffix
	// check. Empty disables cross-cluster lookup.
	viceDomainSuffix string

	lookup *canonicalLookup
	cache  *lookupCache
}

// HandleWaiting attempts to redirect cross-cluster requests; otherwise serves
// the waiting page. The page periodically reloads itself; once the analysis-
// specific HTTPRoute is active, the reload lands on the vice-operator loading
// page or the running analysis instead of here.
func (a *App) HandleWaiting(w http.ResponseWriter, r *http.Request) {
	if a.tryRedirect(w, r) {
		return
	}
	a.renderWaitingPage(w, r)
}

// tryRedirect attempts to 302 the browser to the cross-cluster canonical URL.
// Returns true when it wrote a redirect response; false when the caller should
// render the waiting page (including all failure modes: non-VICE host, empty
// subdomain, no-match, owner-on-this-cluster, app-exposer error).
func (a *App) tryRedirect(w http.ResponseWriter, r *http.Request) bool {
	if a.lookup == nil || a.viceDomainSuffix == "" {
		return false
	}

	host := stripPort(r.Host)
	if !strings.HasSuffix(host, a.viceDomainSuffix) {
		return false
	}
	subdomain, _, ok := strings.Cut(host, ".")
	if !ok || subdomain == "" {
		return false
	}

	redirectURL, ok := a.cache.Get(subdomain)
	if !ok {
		redirectURL = a.resolve(r.Context(), subdomain)
		a.cache.Put(subdomain, redirectURL)
	}
	if redirectURL == "" {
		return false
	}

	// Defense in depth: if the resolved URL points back at this same Host,
	// never redirect — we'd loop. The cache should already store "" in
	// that case, but recompute here as a guard.
	if redirectsToSameHost(redirectURL, r.Host) {
		return false
	}

	target, err := withPathAndQuery(redirectURL, r.URL.Path, r.URL.RawQuery)
	if err != nil {
		log.Warnf("canonical-url for subdomain %s returned unparseable url %q: %v", subdomain, redirectURL, err)
		return false
	}

	log.Debugf("redirecting %s%s to %s", r.Host, r.URL.RequestURI(), target)
	http.Redirect(w, r, target, http.StatusFound)
	return true
}

// resolve calls the canonical-url lookup and returns the redirect URL string
// to cache. Returns "" for any outcome that should result in the waiting page
// being served (no match, owner-on-this-cluster, lookup failure).
func (a *App) resolve(ctx context.Context, subdomain string) string {
	canonical, err := a.lookup.Lookup(ctx, subdomain)
	if err != nil {
		if errors.Is(err, errNotFound) {
			log.Infof("no operator owns subdomain %s; falling through to waiting page", subdomain)
		} else {
			log.Warnf("canonical-url lookup for subdomain %s failed: %v", subdomain, err)
		}
		return ""
	}
	return canonical
}

// renderWaitingPage serves the existing self-refreshing waiting template.
func (a *App) renderWaitingPage(w http.ResponseWriter, _ *http.Request) {
	data := waitingPageData{RefreshSeconds: a.refreshSeconds}

	var buf strings.Builder
	if err := waitingTemplate.Execute(&buf, data); err != nil {
		log.Errorf("rendering waiting page: %v", err)
		http.Error(w, "failed to render waiting page", http.StatusInternalServerError)
		return
	}

	// Set a custom header so the client-side JS can detect when the response
	// is no longer coming from the default backend.
	w.Header().Set("X-Vice-Default-Backend", "true")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprint(w, buf.String())
}

// stripPort returns the host portion of a Host header, dropping any ":port".
// Delegates to net.SplitHostPort, which understands the bracketed IPv6 form;
// if no port is present, returns the input unchanged.
func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// redirectsToSameHost reports whether the canonical URL's host (with port)
// matches the request's Host header. Defends against a misconfigured operator
// row whose base_url points back at the local vice domain.
func redirectsToSameHost(redirectURL, requestHost string) bool {
	u, err := url.Parse(redirectURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, requestHost)
}

// withPathAndQuery joins the canonical base URL with the request's path and
// raw query, preserving the original request shape so a redirected reload
// lands at the same resource on the other cluster.
func withPathAndQuery(baseURL, path, rawQuery string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	if path != "" {
		u.Path = path
	}
	u.RawQuery = rawQuery
	return u.String(), nil
}

// Environment variable names for cross-cluster redirect configuration.
// Kept in env (not flags) so per-cluster deploys can set them from a
// ConfigMap/Secret without changing container args.
const (
	envViceDomain     = "VICE_DOMAIN"
	envAppExposerURL  = "APP_EXPOSER_URL"
	envLookupTimeout  = "LOOKUP_TIMEOUT"
	envLookupCacheTTL = "LOOKUP_CACHE_TTL"
)

// defaultLookupTimeout bounds each canonical-url lookup. Short — this runs
// in the user's request path.
const defaultLookupTimeout = 2 * time.Second

// defaultLookupCacheTTL is how long canonical-url outcomes are memoized per
// subdomain so the waiting page's reload loop doesn't hammer app-exposer.
const defaultLookupCacheTTL = 15 * time.Second

// envDuration reads a time.Duration from env, falling back to fallback when
// the variable is unset. An unparseable value is fatal — silently using the
// default would mask a misconfiguration that only matters at request time.
func envDuration(name string, fallback time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Fatalf("invalid %s %q: %v", name, raw, err)
	}
	return d
}

func main() {
	log.Logger.SetReportCaller(true)

	var (
		listenAddr     = flag.String("listen", "0.0.0.0:60000", "The listen address.")
		refreshSeconds = flag.Int("refresh-seconds", 5, "Seconds between page reloads while waiting for the analysis route.")
		logLevel       = flag.String("log-level", "info", "One of trace, debug, info, warn, error, fatal, or panic.")
	)

	flag.Parse()

	level, err := logrus.ParseLevel(*logLevel)
	if err != nil {
		log.Fatalf("invalid log level %q: %v", *logLevel, err)
	}
	logrus.SetLevel(level)
	logrus.SetFormatter(&logrus.JSONFormatter{})

	viceDomain := os.Getenv(envViceDomain)
	appExposerURL := os.Getenv(envAppExposerURL)
	lookupTimeout := envDuration(envLookupTimeout, defaultLookupTimeout)
	lookupCacheTTL := envDuration(envLookupCacheTTL, defaultLookupCacheTTL)

	app := &App{
		refreshSeconds: *refreshSeconds,
	}

	switch {
	case viceDomain != "" && appExposerURL != "":
		parsed, perr := url.Parse(appExposerURL)
		if perr != nil || parsed.Host == "" {
			log.Fatalf("invalid %s %q: %v", envAppExposerURL, appExposerURL, perr)
		}
		app.viceDomainSuffix = "." + strings.TrimPrefix(viceDomain, ".")
		app.lookup = &canonicalLookup{
			baseURL: parsed,
			client:  &http.Client{Timeout: lookupTimeout},
		}
		app.cache = newLookupCache(lookupCacheTTL)
		log.Infof("cross-cluster redirect enabled (%s=%s, %s=%s, %s=%s, %s=%s)",
			envViceDomain, viceDomain,
			envAppExposerURL, parsed.String(),
			envLookupTimeout, lookupTimeout,
			envLookupCacheTTL, lookupCacheTTL)
	case viceDomain == "" && appExposerURL == "":
		log.Warnf("cross-cluster redirect disabled: %s and %s are both unset; serving waiting page for all requests", envViceDomain, envAppExposerURL)
	default:
		log.Fatalf("%s and %s must be set together (or both omitted to disable cross-cluster redirect)", envViceDomain, envAppExposerURL)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, "healthy")
	})
	mux.HandleFunc("/", app.HandleWaiting)

	log.Infof("vice-default-backend listening on %s (refresh-seconds=%d)", *listenAddr, *refreshSeconds)

	server := &http.Server{
		Handler: mux,
		Addr:    *listenAddr,
	}
	log.Fatal(server.ListenAndServe())
}
