package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// errInsecureHTTP is the startup error for a non-loopback bind without --insecure-http.
var errInsecureHTTP = errors.New("bind 127.0.0.1 and put a TLS proxy in front, or pass --insecure-http on a trusted LAN")

// MaxBody is the largest request body any route accepts.
const MaxBody = 64 << 10

// Server is the HTTP side of `slai serve`.
type Server struct {
	opts     Options
	deps     *Deps
	mux      *http.ServeMux
	handler  http.Handler
	routes   []Route
	patterns map[string]bool

	badgeMu sync.Mutex
	badges  map[string]func() (int, string)

	refusedUnauth atomic.Int64 // refusals of requests with no valid session (never stored)

	token    string
	loopback bool
	sessions *sessions

	mu    sync.RWMutex
	bound string // host:port actually listening ("" until known)
}

// New builds a server (it does not listen). Call SetBound with the listener's address, or
// use Serve.
func New(opts Options) (*Server, error) {
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:0"
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	host, _, err := net.SplitHostPort(opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("--listen %q: %w", opts.Listen, err)
	}
	if !isLoopbackHost(host) && !opts.InsecureHTTP {
		return nil, errInsecureHTTP
	}
	s := &Server{opts: opts, mux: http.NewServeMux(), patterns: map[string]bool{},
		loopback: isLoopbackHost(host), sessions: newSessions(opts.Now), token: opts.Token}
	if s.token == "" {
		b := make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		s.token = hex.EncodeToString(b)
	}
	s.deps = &Deps{Dir: opts.Dir, Store: opts.Store, Git: opts.Git, Team: opts.Team, Actions: opts.Actions, ReadOnly: opts.ReadOnly, Now: opts.Now, Orphans: opts.Orphans, s: s}
	if err := s.register(); err != nil {
		return nil, err
	}
	s.handler = s.chain()
	return s, nil
}

// Handler is the full handler (all checks included), for httptest and http.Server.
func (s *Server) Handler() http.Handler { return s.handler }

// Token is the one-time token a non-loopback visitor exchanges for a session.
func (s *Server) Token() string { return s.token }

// Loopback reports whether the bind is a loopback address.
func (s *Server) Loopback() bool { return s.loopback }

// Routes lists the route table (the registered routes, fallbacks that lost excluded).
func (s *Server) Routes() []Route { return append([]Route(nil), s.routes...) }

// SetBound records the address actually listening (host:port); the Host allow-list is
// built from it.
func (s *Server) SetBound(addr string) {
	s.mu.Lock()
	s.bound = addr
	s.mu.Unlock()
}

func (s *Server) boundAddr() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bound
}

// register collects module routes and the shell's own, validates them, and fills the mux.
func (s *Server) register() error {
	var all, fallback []Route
	for _, m := range s.opts.Modules {
		all = append(all, m(s.deps)...)
	}
	for _, r := range builtinRoutes(s.deps) {
		r.Fallback = true
		fallback = append(fallback, r)
	}
	all = append(all, capabilitiesRoute(s.deps))
	for _, r := range append(all, fallback...) {
		if err := validRoute(r); err != nil {
			return err
		}
		key := r.Method + " " + r.Pattern
		if s.patterns[key] {
			if r.Fallback {
				continue
			}
			return fmt.Errorf("route %s registered twice", key)
		}
		s.patterns[key] = true
		s.routes = append(s.routes, r)
		s.mux.HandleFunc(key, s.wrap(r))
	}
	sort.SliceStable(s.routes, func(i, j int) bool { return s.routes[i].Pattern < s.routes[j].Pattern })
	return nil
}

func validRoute(r Route) error {
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodDelete:
	default:
		return fmt.Errorf("route %s %s: method must be GET, POST or DELETE", r.Method, r.Pattern)
	}
	if !strings.HasPrefix(r.Pattern, "/") || r.Handler == nil {
		return fmt.Errorf("route %s %s: needs a path pattern and a handler", r.Method, r.Pattern)
	}
	if r.Write() && r.Action == "" {
		return fmt.Errorf("route %s %s: a write needs an Action name for its web_action event", r.Method, r.Pattern)
	}
	if strings.HasPrefix(r.Pattern, "/static/") {
		return fmt.Errorf("route %s: /static/ belongs to the shell", r.Pattern)
	}
	return nil
}

// hasPage reports whether a GET route serves exactly path (used to enable nav entries).
func (s *Server) hasPage(path string) bool {
	if path == "/" {
		return s.patterns["GET /{$}"] || s.patterns["GET /"]
	}
	return s.patterns["GET "+path]
}

// statusWriter records the response status for the web_action event.
type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int) {
	if w.status == 0 {
		w.status = c
	}
	w.ResponseWriter.WriteHeader(c)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// wrap adds the per-route behaviour: a write records exactly one web_action event after
// the handler returns. The checks that decide whether the handler runs at all are in the
// chain (guard.go), so a refused request never reaches here and records nothing.
func (s *Server) wrap(r Route) http.HandlerFunc {
	if !r.Write() {
		return r.Handler
	}
	return func(w http.ResponseWriter, req *http.Request) {
		sw := &statusWriter{ResponseWriter: w}
		r.Handler(sw, req)
		target := req.PathValue("id")
		if r.Target != nil {
			target = r.Target(req)
		}
		s.recordAction(r.Action, target, sw.status)
	}
}

// recordAction writes the web_action event of a mutating request.
func (s *Server) recordAction(action, target string, status int) {
	if s.opts.Store == nil {
		return
	}
	if status == 0 {
		status = http.StatusOK
	}
	_ = s.opts.Store.AddEvent("user", "web_action", jsonString(map[string]any{"actor": "user", "action": action, "target": target, "status": status}))
}

// Serve listens on opts.Listen and serves until ctx is cancelled.
func Serve(ctx context.Context, opts Options) error {
	s, err := New(opts)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", s.opts.Listen)
	if err != nil {
		return err
	}
	s.SetBound(ln.Addr().String())
	if w := s.opts.Out; w != nil {
		fmt.Fprintln(w, s.startMessage(ln.Addr().String()))
	}
	srv := &http.Server{Handler: s.handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 32 << 10}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		<-done
		return nil
	}
}

// startMessage is what `slai serve` prints: the URL to open, with the one-time token when
// the bind is not loopback.
func (s *Server) startMessage(addr string) string {
	if s.loopback {
		_, port, _ := net.SplitHostPort(addr)
		host, _, _ := net.SplitHostPort(s.opts.Listen)
		if host == "" || host == "localhost" {
			host = "127.0.0.1"
		}
		local := fmt.Sprintf("slai serve: open http://%s/", net.JoinHostPort(host, port))
		if len(s.opts.AllowHosts) == 0 {
			return local
		}
		// Behind a reverse proxy the automatic sign-in does not apply: a proxied visit does
		// the token exchange, so the token URL is printed for each allowed name.
		var b strings.Builder
		b.WriteString(local + "\n")
		for _, h := range s.opts.AllowHosts {
			fmt.Fprintf(&b, "  via the proxy (%s): open https://%s/?token=%s  (http:// if the proxy does not terminate TLS)\n", h, h, s.token)
		}
		b.WriteString("  The token is a password: anyone holding it can see every agent's terminal output and use every write this page offers.")
		return b.String()
	}
	host, port, _ := net.SplitHostPort(addr)
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "<this machine's address>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "slai serve: open http://%s/?token=%s\n", net.JoinHostPort(host, port), s.token)
	b.WriteString("  The token is a password: anyone holding it, on this network, can see every agent's terminal output\n")
	b.WriteString("  and use every write this page offers. It is sent over plain HTTP; use it only on a network you trust.")
	return b.String()
}

// readBody reads at most MaxBody+1 bytes; ok is false when the body is too large.
func readBody(r *http.Request) (body []byte, ok bool, err error) {
	if r.ContentLength > MaxBody {
		return nil, false, nil
	}
	if r.Body == nil {
		return nil, true, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, MaxBody+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > MaxBody {
		return nil, false, nil
	}
	return b, true, nil
}

func replaceBody(r *http.Request, b []byte) {
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
}
