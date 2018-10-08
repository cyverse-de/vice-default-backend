package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"

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

// App contains the http handlers for the application.
type App struct {
	graphqlBase              string
	viceBaseURL              string
	landingPageURL           string
	loadingPageURL           string
	notFoundPath             string
	disableCustomHeaderMatch bool
}

// AppURL returns the fully-formed app URL based on the request passed in. Uses
// the Host header and the configured VICE base URL to construct the app URL.
func (a *App) AppURL(r *http.Request) (string, error) {
	fmt.Printf("%+v\n", r)
	parsed, err := url.Parse(a.viceBaseURL)
	if err != nil {
		return "", err
	}
	parsed.Host = fmt.Sprintf("%s.%s", r.Host, parsed.Host)
	parsed.RawPath = r.URL.RawPath
	parsed.RawQuery = r.URL.RawQuery
	return parsed.String(), nil
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
	var err error

	subdomain := r.Host

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
		appURL, err := a.AppURL(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		q.Set("url", appURL)
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
		viceBaseURL              = flag.String("vice-base-url", "https://cyverse.run", "The base URL for VICE apps.")
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
	log.Infof("VICE base is %s", *viceBaseURL)
	log.Infof("graphql URL is %s", *graphqlBase)
	log.Infof("loading-page-url: %s", *loadingPageURL)
	log.Infof("landing-page-url: %s", *landingPageURL)
	log.Infof("disable-custom-header-match is %+v", *disableCustomHeaderMatch)

	app := App{
		graphqlBase:              *graphqlBase,
		disableCustomHeaderMatch: *disableCustomHeaderMatch,
		landingPageURL:           *landingPageURL,
		loadingPageURL:           *loadingPageURL,
		viceBaseURL:              *viceBaseURL,
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
