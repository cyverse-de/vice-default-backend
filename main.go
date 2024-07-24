package main

import (
	"database/sql"
	"flag"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"

	"github.com/cyverse-de/app-exposer/common"
	"github.com/cyverse-de/configurate"
	"github.com/gorilla/mux"
	_ "github.com/lib/pq"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

var log = common.Log

func init() {
	logrus.SetFormatter(&logrus.JSONFormatter{})
}

// App contains the http handlers for the application.
type App struct {
	db                       *sql.DB
	viceBaseURL              string
	loadingPageBaseURL       *url.URL
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

// TemplateURL is used for interpolating the URL into the template passed
// in for the loading page URL.
type TemplateURL struct {
	URL string
}

// RouteRequest determines whether to redirect a request to the 404 handler,
// the landing page, or the loading page.
func (a *App) RouteRequest(w http.ResponseWriter, r *http.Request) {
	appURL, err := a.AppURL(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	log.Infof("app url: %s", appURL)
	loadingURL := a.loadingPageBaseURL.JoinPath(template.URLQueryEscaper(appURL))
	http.Redirect(w, r, loadingURL.String(), http.StatusTemporaryRedirect)
}

func main() {
	log.Logger.SetReportCaller(true)

	var (
		err                      error
		cfg                      *viper.Viper
		dbURI                    string
		viceBaseURL              string
		loadingPageURL           string
		loadingPageBaseURL       *url.URL
		configPath               = flag.String("config", "/etc/iplant/de/jobservices.yml", "Path to the config file")
		listenAddr               = flag.String("listen", "0.0.0.0:60000", "The listen address.")
		sslCert                  = flag.String("ssl-cert", "", "The path to the SSL .crt file.")
		sslKey                   = flag.String("ssl-key", "", "The path to the SSL .key file.")
		staticFilePath           = flag.String("static-file-path", "./static", "Path to static file assets.")
		disableCustomHeaderMatch = flag.Bool("disable-custom-header-match", false, "Disables usage of the X-Frontend-Url header for subdomain matching. Use Host header instead. Useful during development.")
		logLevel                 = flag.String("log-level", "info", "One of trace, debug, info, warn, error, fatal, or panic.")
	)

	flag.Parse()

	var levelSetting logrus.Level

	switch *logLevel {
	case "trace":
		levelSetting = logrus.TraceLevel
	case "debug":
		levelSetting = logrus.DebugLevel
	case "info":
		levelSetting = logrus.InfoLevel
	case "warn":
		levelSetting = logrus.WarnLevel
	case "error":
		levelSetting = logrus.ErrorLevel
	case "fatal":
		levelSetting = logrus.FatalLevel
	case "panic":
		levelSetting = logrus.PanicLevel
	default:
		log.Fatal("incorrect log level")
	}

	log.Logger.SetLevel(levelSetting)

	log.Infof("Reading config from %s", *configPath)
	if _, err = os.Open(*configPath); err != nil {
		log.Fatal(*configPath)
	}

	cfg, err = configurate.Init(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	log.Infof("Done reading config from %s", *configPath)

	// Make sure the db.uri URL is parseable
	dbURI = cfg.GetString("vice.db.uri")
	if _, err = url.Parse(dbURI); err != nil {
		log.Fatal(errors.Wrap(err, "Can't parse db.uri in the config file"))
	}

	// Make sure the base URL is parseable
	viceBaseURL = cfg.GetString("vice.default_backend.base_url")
	if _, err = url.Parse(viceBaseURL); err != nil {
		log.Fatal(errors.Wrap(err, "Cannot parse vice.default_backend.base_url from the configuration file"))
	}

	// Make sure the loading page URL is parseable
	loadingPageURL = cfg.GetString("vice.default_backend.loading_page_url")
	if loadingPageBaseURL, err = url.Parse(loadingPageURL); err != nil {
		log.Fatal(errors.Wrap(err, "Cannot parse vice.default_backend.loading_page_url"))
	}

	// Test database connection
	db, err := sql.Open("postgres", dbURI)
	if err != nil {
		log.Fatal(errors.Wrapf(err, "error connecting to database %s", dbURI))
	}

	if err = db.Ping(); err != nil {
		log.Fatal(errors.Wrapf(err, "error pinging database %s", dbURI))
	}

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
	log.Infof("VICE base is %s", viceBaseURL)
	log.Infof("loading-page-url: %s", loadingPageURL)
	log.Infof("disable-custom-header-match is %+v", *disableCustomHeaderMatch)

	app := App{
		db:                       db,
		disableCustomHeaderMatch: *disableCustomHeaderMatch,
		loadingPageBaseURL:       loadingPageBaseURL,
		viceBaseURL:              viceBaseURL,
		notFoundPath:             filepath.Join(*staticFilePath, "404.html"),
	}

	r := mux.NewRouter()

	r.NotFoundHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, app.notFoundPath)
	})

	r.PathPrefix("/healthz").HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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
