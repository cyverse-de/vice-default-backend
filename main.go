package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Sirupsen/logrus"
	"github.com/gorilla/mux"
	"github.com/machinebox/graphql"
)

var log = logrus.WithFields(logrus.Fields{
	"service": "cas-proxy",
	"art-id":  "cas-proxy",
	"group":   "org.cyverse",
})

func init() {
	logrus.SetFormatter(&logrus.JSONFormatter{})
}

// extractSubdomain returns the subdomain part of the URL.
func extractSubdomain(addr string) (string, error) {
	fields := strings.Split(addr, ".")
	if len(fields) < 2 {
		return "", nil
	}
	if len(fields) == 2 {
		if fields[0] == "www" {
			return "", nil
		}
		return fields[0], nil
	}
	return strings.Join(fields[:len(fields)-2], "."), nil
}

// App contains the http handlers for the application.
type App struct {
	graphqlBase              string
	viceDomain               string
	landingPageURL           string
	loadingPageURL           string
	notFoundPath             string
	disableCustomHeaderMatch bool
}

// FrontendAddress returns the appropriate host[:port] to use for various
// operations. If the --disable-custom-header-match flag is true, then the Host
// header in the request is returned. If it's false, the custom X-Frontend-Url
// header is returned.
func (a *App) FrontendAddress(r *http.Request) string {
	if a.disableCustomHeaderMatch {
		u := &url.URL{}
		if r.TLS != nil {
			u.Scheme = "https"
		} else {
			u.Scheme = "http"
		}
		u.Host = r.Host
		u.Path = r.URL.Path
		u.RawQuery = r.URL.RawQuery
		return u.String()
	}
	return r.Header.Get("X-Frontend-Url")
}

// AddressMatches returns true if the given URL is a subdomain of the configured
// VICE domain.
func (a *App) AddressMatches(addr string) (bool, error) {
	r := fmt.Sprintf("(a.*\\.)?\\Q%s\\E(:[0-9]+)?", a.viceDomain)
	matched, err := regexp.MatchString(r, addr)
	if err != nil {
		return false, err
	}
	return matched, nil
}

const subdomainLookupQuery = `
query Subdomain($subdomain: String) {
  jobs(where: {subdomain: {_eq: $subdomain}}) {
    id
  }
}
`

func (a *App) lookupSubdomain(subdomain string) (bool, error) {
	var (
		err error
		ok  bool
	)

	client := graphql.NewClient(a.graphqlBase)
	req := graphql.NewRequest(subdomainLookupQuery)
	req.Var("subdomain", subdomain)

	data := map[string][]map[string]string{}
	if err = client.Run(context.Background(), req, &data); err != nil {
		return false, err
	}

	if _, ok = data["jobs"]; !ok {
		return false, fmt.Errorf("missing jobs from graphql query for '%s' subdomain", subdomain)
	}

	if len(data["jobs"]) < 1 {
		return false, nil
	}

	return true, nil
}

// RouteRequest determines whether to redirect a request to the 404 handler,
// the landing page, or the loading page.
func (a *App) RouteRequest(w http.ResponseWriter, r *http.Request) {
	frontendURI := a.FrontendAddress(r)

	frontendURL, err := url.Parse(frontendURI)
	if err != nil {
		http.Error(w, fmt.Sprintf("error checking URL %s: %s", frontendURI, err.Error()), http.StatusInternalServerError)
		return
	}

	frontendHost := frontendURL.Host

	matches, err := a.AddressMatches(frontendHost)
	if err != nil {
		http.Error(w, fmt.Sprintf("error checking URL %s: %s", frontendHost, err.Error()), http.StatusInternalServerError)
		return
	}
	if !matches {
		http.Error(w, fmt.Sprintf("URL %s is not in the domain of %s", frontendHost, a.viceDomain), http.StatusBadRequest)
		return
	}

	subdomain, err := extractSubdomain(frontendHost)
	if err != nil {
		http.Error(w, fmt.Sprintf("error getting subdomain for URL %s", frontendHost), http.StatusInternalServerError)
		return
	}
	if subdomain == "" {
		http.Redirect(w, r, a.landingPageURL, http.StatusTemporaryRedirect)
		return
	}

	var exists bool
	exists, err = a.lookupSubdomain(subdomain)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if exists {
		loadingURL, err := url.Parse(a.loadingPageURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		q := loadingURL.Query()
		q.Set("url", frontendURI)
		loadingURL.RawQuery = q.Encode()
		http.Redirect(w, r, loadingURL.String(), http.StatusTemporaryRedirect)
		return
	}

	http.Redirect(w, r, a.notFoundPath, http.StatusTemporaryRedirect)
}

func main() {
	var (
		err                      error
		listenAddr               = flag.String("listen", "0.0.0.0:60000", "The listen address.")
		sslCert                  = flag.String("ssl-cert", "", "The path to the SSL .crt file.")
		sslKey                   = flag.String("ssl-key", "", "The path to the SSL .key file.")
		graphqlBase              = flag.String("graphql", "http://graphql-de/v1alpha1/graphql", "The base URL for the graphql provider.")
		viceDomain               = flag.String("vice-domain", "cyverse.run", "The domain and port for VICE apps.")
		landingPageURL           = flag.String("landing-page-url", "https://cyverse.run", "The URL for the landing page service.")
		loadingPageURL           = flag.String("loading-page-url", "https://loading.cyverse.run", "The URL for the loading page service.")
		staticFilePath           = flag.String("static-file-path", "./static", "Path to static file assets.")
		disableCustomHeaderMatch = flag.Bool("disable-custom-header-match", false, "Disables usage of the X-Frontend-Url header for subdomain matching. Use Host header instead. Useful during development.")
	)

	flag.Parse()

	useSSL := false
	if *sslCert != "" || *sslKey != "" {
		if *sslCert == "" {
			log.Fatal("--ssl-cert is required with --ssl-key.")
		}

		if *sslKey == "" {
			log.Fatal("--ssl-key is required with --ssl-cert.")
		}
		useSSL = true
	}

	log.Infof("listen address is %s", *listenAddr)
	log.Infof("VICE domain is %s", *viceDomain)
	log.Infof("graphql URL is %s", *graphqlBase)
	log.Infof("loading-page-url: %s", *loadingPageURL)
	log.Infof("landing-page-url: %s", *landingPageURL)
	log.Infof("disable-custom-header-match is %+v", *disableCustomHeaderMatch)

	app := App{
		graphqlBase:              *graphqlBase,
		disableCustomHeaderMatch: *disableCustomHeaderMatch,
		landingPageURL:           *landingPageURL,
		loadingPageURL:           *loadingPageURL,
		viceDomain:               *viceDomain,
		notFoundPath:             filepath.Join(*staticFilePath, "404.html"),
	}

	r := mux.NewRouter()

	r.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, app.notFoundPath)
	})

	r.PathPrefix("/healthz").HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "I'm healthy.")
	})

	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir(*staticFilePath))))

	r.PathPrefix("/").HandlerFunc(app.RouteRequest)

	server := &http.Server{
		Handler: r,
		Addr:    *listenAddr,
	}
	if useSSL {
		err = server.ListenAndServeTLS(*sslCert, *sslKey)
	} else {
		err = server.ListenAndServe()
	}
	log.Fatal(err)
}
