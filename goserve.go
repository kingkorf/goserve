package main

import (
	"gopkg.in/v1/yaml"

	"compress/gzip"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// Headers represents a simplified HTTP header dict
type Headers map[string]string

// ServerConfig represents a server configuration.
type ServerConfig struct {
	Listeners []Listener `yaml:"listeners"`
	Serves    []Serve    `yaml:"serves"`
	Errors    []Error    `yaml:"errors"`
	Redirects []Redirect `yaml:"redirects"`
}

func (c ServerConfig) sanitise() {
	for _, l := range c.Listeners {
		l.sanitise()
	}
	for _, s := range c.Serves {
		s.sanitise()
	}
	for _, r := range c.Redirects {
		r.sanitise()
	}
	for _, e := range c.Errors {
		e.sanitise()
	}
}

func (c ServerConfig) check() (ok bool) {
	ok = true
	if len(c.Listeners) == 0 {
		log.Printf("No listeners defined!")
		ok = false
	}
	for i, l := range c.Listeners {
		ok = l.check(fmt.Sprintf("Listener #%d:", i)) && ok
	}
	if len(c.Serves) == 0 {
		log.Printf("No serves defined!")
		ok = false
	}
	for i, s := range c.Serves {
		ok = s.check(fmt.Sprintf("Serve #%d:", i)) && ok
	}
	for i, r := range c.Redirects {
		ok = r.check(fmt.Sprintf("Redirect #%d:", i)) && ok
	}
	return
}

// Listener describes how connections are accepted and the protocol used.
type Listener struct {
	Protocol string  `yaml:"protocol"`
	Addr     string  `yaml:"addr"`
	CertFile string  `yaml:"cert"`
	KeyFile  string  `yaml:"key"`
	Headers  Headers `yaml:"headers"` // custom headers
	Gzip     bool    `yaml:"gzip"`
}

func (l *Listener) sanitise() {
	if l.Protocol == "" {
		l.Protocol = "http"
	}
	if l.Addr == "" {
		l.Addr = ":http"
	}
}

func (l *Listener) check(label string) (ok bool) {
	ok = true
	if l.Protocol == "http" {
		if l.CertFile != "" || l.KeyFile != "" {
			log.Printf(label + ": certificate supplied for non-HTTPS listener")
			ok = false
		}
	} else if l.Protocol == "https" {
		if _, err := os.Stat(l.CertFile); os.IsNotExist(err) {
			log.Printf(label+": cert file `%s` does not exist", l.CertFile)
			ok = false
		}
		if _, err := os.Stat(l.KeyFile); os.IsNotExist(err) {
			log.Printf(label+": key file `%s` does not exist", l.KeyFile)
			ok = false
		}
	} else {
		log.Printf(label+": invalid protocol `%s`", l.Protocol)
		ok = false
	}
	return
}

// Serve represents a path that will be served.
type Serve struct {
	Target         string  `yaml:"target"`          // where files are stored on the file system
	Path           string  `yaml:"path"`            // HTTP path to serve files under
	Error          int     `yaml:"error"`           // HTTP error to return (0=disabled)
	PreventListing bool    `yaml:"prevent-listing"` // prevent file listing
	Headers        Headers `yaml:"headers"`         // custom headers
}

func (s *Serve) sanitise() {
	if s.Path == "" {
		s.Path = "/"
	}
}

func (s Serve) check(label string) (ok bool) {
	ok = true
	if s.Path == "" {
		log.Println(label + ": no path specified")
		ok = false
	}
	if s.Error == 0 && s.Target == "" {
		log.Println(label + ": no target path specified")
		ok = false
	}
	if s.Error != 0 && s.Target != "" {
		log.Println(label + ": error specified with target path")
		ok = false
	}
	return
}

func (s Serve) handler() http.Handler {
	var h http.Handler
	if s.Error > 0 {
		errStatus := s.Error
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, http.StatusText(errStatus), errStatus)
		})
	} else if s.PreventListing {
		// Prevent listing of directories lacking an index.html file
		target := s.Target
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			d := &PreventListingDir{http.Dir(target)}
			h := http.FileServer(d)
			defer func() {
				if p := recover(); p != nil {
					if p == d {
						http.Error(w, http.StatusText(http.StatusForbidden), http.StatusForbidden)
						return
					}
					panic(p)
				}
			}()
			h.ServeHTTP(w, r)
		})
	} else {
		h = http.FileServer(http.Dir(s.Target))
	}
	if len(s.Headers) > 0 {
		h = CustomHeadersHandler(h, s.Headers)
	}
	return http.StripPrefix(s.Path, h)
}

// Redirect represents a redirect from one path to another.
type Redirect struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
	With int    `yaml:"status"`
}

func (r *Redirect) sanitise() {
	if r.With == 0 {
		r.With = 301
		log.Printf("Defaulting status code %d for redirect %s\n", r.With, r.From)
	}
}

func (r Redirect) check(label string) (ok bool) {
	if r.From == "" {
		log.Printf(label + ": no `from` path")
		ok = false
	}
	if r.To == "" {
		log.Printf(label + ": no `to` path")
		ok = false
	}
	return true
}

func (r Redirect) handler() http.Handler {
	return http.RedirectHandler(r.To, r.With)
}

// Error represents what to do when a particular HTTP status is encountered.
type Error struct {
	Status int    `yaml:"status"`
	Target string `yaml:"target"`
}

func (e *Error) sanitise() {
}

func (e Error) check() (ok bool) {
	return true
}

func (e Error) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Clear content-type as set by `http.Error` to force re-detection
		w.Header().Del("Content-Type")

		// Serve error page with a specific status code
		http.ServeFile(w, r, e.Target)
	})
}

// StaticServeMux wraps ServeMux but allows for the interception of errors.
type StaticServeMux struct {
	*http.ServeMux
	errors map[int]http.Handler
}

// NewStaticServeMux allocates and returns a new StaticServeMux
func NewStaticServeMux() *StaticServeMux {
	return &StaticServeMux{
		ServeMux: http.NewServeMux(),
		errors:   make(map[int]http.Handler),
	}
}

// HandleError registers a handler for the given response code.
func (s *StaticServeMux) HandleError(status int, handler http.Handler) {
	if s.errors[status] != nil {
		panic("Handler for error already registered")
	}
	s.errors[status] = handler
}

func (s StaticServeMux) intercept(status int, w http.ResponseWriter, req *http.Request) bool {
	// Get error handler if there is one
	if h, f := s.errors[status]; f {
		h.ServeHTTP(statusResponseWriter{w, status}, req)
		return true
	}
	// Ignore non-error status codes
	if status < 400 {
		return false
	}
	http.Error(w, http.StatusText(status), status)
	return true
}

func (s *StaticServeMux) interceptHandler(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		irw := &interceptResponseWriter{
			ResponseWriter: w,
			r:              r,
			m:              s,
		}

		// If intercept occurred, originating call would have been panic'd.
		// Recover here once error has been dealt with.
		defer func() {
			if p := recover(); p != nil {
				if p == irw {
					return
				}
				panic(p)
			}
		}()

		handler.ServeHTTP(irw, r)
	})
}

func (s *StaticServeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.RequestURI == "*" {
		if r.ProtoAtLeast(1, 1) {
			w.Header().Set("Connection", "close")
		}
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	h, _ := s.Handler(r)
	h = s.interceptHandler(h)
	h.ServeHTTP(w, r)
}

type interceptResponseWriter struct {
	http.ResponseWriter
	r *http.Request
	m *StaticServeMux
}

func (h *interceptResponseWriter) WriteHeader(status int) {
	if h.m.intercept(status, h.ResponseWriter, h.r) {
		panic(h)
	} else {
		h.ResponseWriter.WriteHeader(status)
	}
}

type statusResponseWriter struct {
	http.ResponseWriter
	Status int
}

func (h statusResponseWriter) WriteHeader(status int) {
	if h.Status < 0 {
		return
	}
	if h.Status > 0 {
		h.ResponseWriter.WriteHeader(h.Status)
		return
	}
	h.ResponseWriter.WriteHeader(status)
}

// PreventListingDir panics whenever a file open fails, allowing index
// requests to be intercepted.
type PreventListingDir struct {
	http.Dir
}

// Open panics whenever opening a file fails.
func (dir *PreventListingDir) Open(name string) (f http.File, err error) {
	f, err = dir.Dir.Open(name)
	if f == nil {
		panic(dir)
	}
	return
}

// CustomHeadersHandler creates a new handler that includes the provided
// headers in each response.
func CustomHeadersHandler(h http.Handler, headers Headers) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wh := w.Header()
		for k, v := range headers {
			if wh.Get(k) == "" {
				wh.Set(k, v)
			}
		}
		h.ServeHTTP(w, r)
	})
}

// GzipResponseWriter gzips content written to it
type GzipResponseWriter struct {
	io.Writer
	http.ResponseWriter
	gotContentType bool
}

func (w *GzipResponseWriter) Write(b []byte) (int, error) {
	if !w.gotContentType {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", http.DetectContentType(b))
		}
		w.gotContentType = true
	}
	return w.Writer.Write(b)
}

// GzipHandler gzips the HTTP response if supported by the client. Based on
// the implementation of `go.httpgzip`
func GzipHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Serve normally to clients that don't express gzip support
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			h.ServeHTTP(w, r)
			return
		}

		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		h.ServeHTTP(&GzipResponseWriter{Writer: gz, ResponseWriter: w}, r)
	})
}

var configPath string
var checkConfig bool
var defaultAddr string

func init() {
	flag.StringVar(&configPath, "config", "", "Path to configuration")
	flag.BoolVar(&checkConfig, "check", false, "Only check config")
	flag.StringVar(&defaultAddr, "addr", ":8080", "Default listen address")

	flag.Parse()
}

func readServerConfig(filename string) (cfg ServerConfig, err error) {
	data, err := ioutil.ReadFile(filename)
	if err != nil {
		return
	}
	err = yaml.Unmarshal(data, &cfg)
	return
}

func defaultServerConfig() ServerConfig {
	c := ServerConfig{}
	c.Listeners = []Listener{
		Listener{Protocol: "http", Addr: defaultAddr},
	}
	target := flag.Arg(0)
	if target == "" {
		target = "."
	}
	c.Serves = []Serve{
		Serve{Path: "/", Target: target},
	}
	return c
}

func main() {
	cfg := defaultServerConfig()
	if configPath != "" {
		var err error
		cfg, err = readServerConfig(configPath)
		if err != nil {
			log.Fatalln("Couldn't load config:", err)
		}
	}
	cfg.sanitise()

	if !cfg.check() {
		log.Fatalln("Invalid config.")
	}
	if checkConfig {
		log.Println("Config check passed")
		os.Exit(0)
	}

	// Setup handlers
	mux := NewStaticServeMux()
	for _, e := range cfg.Errors {
		mux.HandleError(e.Status, e.handler())
	}
	for _, serve := range cfg.Serves {
		mux.Handle(serve.Path, serve.handler())
	}
	for _, redirect := range cfg.Redirects {
		mux.Handle(redirect.From, redirect.handler())
	}

	// Start listeners
	for _, listener := range cfg.Listeners {
		var h http.Handler = mux
		if len(listener.Headers) > 0 {
			h = CustomHeadersHandler(h, listener.Headers)
		}
		if listener.Gzip {
			h = GzipHandler(h)
		}
		if listener.Protocol == "http" {
			go func() {
				err := http.ListenAndServe(listener.Addr, h)
				if err != nil {
					log.Fatalln(err)
				}
			}()
		} else if listener.Protocol == "https" {
			go func() {
				err := http.ListenAndServeTLS(listener.Addr, listener.CertFile, listener.KeyFile, h)
				if err != nil {
					log.Fatalln(err)
				}
			}()
		} else {
			log.Printf("Unsupported protocol %s\n", listener.Protocol)
		}
		log.Printf("listening on %s (%s)\n", listener.Addr, listener.Protocol)
	}

	// Since all the listeners are running in separate gorotines, we have to
	// wait here for a termination signal.
	exit := make(chan os.Signal, 1)
	signal.Notify(exit, os.Interrupt, syscall.SIGTERM)
	<-exit
	os.Exit(0)
}
