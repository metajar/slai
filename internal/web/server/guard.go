package server

import (
	"bytes"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/oorbt/slai/internal/web/assets"
	"github.com/oorbt/slai/internal/web/templates"
)

// csp is the Content-Security-Policy of every response: no inline script or style, nothing
// from another origin, no framing, no plugins.
const csp = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'"

func setSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", csp)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Opener-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
}

// chain is the handler every request goes through, in this order: security headers; Host
// allow-list; /static/; Origin; session (loopback creates one on first visit, otherwise
// the one-time token); then for writes the X-Slai-Request header, --read-only and the
// body cap; and only then the route. Each refusal is final: the route never runs.
func (s *Server) chain() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w)
		if !s.hostAllowed(r.Host) {
			s.denied(r, nil, "bad_host")
			s.refuse(w, r, http.StatusForbidden, "Host not allowed", "This server answers only to the address it is listening on. If you reached it through a name, restart it with --listen set to that name.")
			return
		}
		if strings.HasPrefix(r.URL.Path, "/static/") {
			s.static(w, r)
			return
		}
		sess := s.sessions.fromRequest(r)
		if !s.originOK(r) {
			s.denied(r, sess, "bad_origin")
			s.refuse(w, r, http.StatusForbidden, "Origin not allowed", "The request came from another origin.")
			return
		}
		write := r.Method != http.MethodGet && r.Method != http.MethodHead
		switch {
		case sess != nil:
			if !write && r.URL.Query().Has("token") { // the token is only ever read once, on the exchange
				s.stripToken(w, r)
				return
			}
		case write:
			s.denied(r, nil, "no_session")
			s.refuse(w, r, http.StatusForbidden, "No session", "Open the page in your browser first; a write needs the session cookie.")
			return
		default:
			if !s.startSession(w, r) {
				return
			}
		}
		if write {
			if r.Header.Get("X-Slai-Request") != "1" {
				s.denied(r, sess, "missing_csrf_header")
				s.refuse(w, r, http.StatusForbidden, "Missing X-Slai-Request", "Writes must carry the X-Slai-Request: 1 header the page sends itself.")
				return
			}
			if s.opts.ReadOnly {
				s.denied(r, sess, "read_only")
				s.refuse(w, r, http.StatusForbidden, "Read-only", "This server was started with --read-only: it refuses every write.")
				return
			}
			body, ok, err := readBody(r)
			if err != nil {
				s.refuse(w, r, http.StatusBadRequest, "Bad request", "The request body could not be read.")
				return
			}
			if !ok {
				s.refuse(w, r, http.StatusRequestEntityTooLarge, "Request too large", "A request body may be at most 64 KB.")
				return
			}
			replaceBody(r, body)
		}
		s.mux.ServeHTTP(w, r)
	})
}

// refuse answers with a small HTML error page.
func (s *Server) refuse(w http.ResponseWriter, r *http.Request, code int, heading, detail string) {
	tpl, err := templates.PageTemplate("layout")
	var b bytes.Buffer
	if err == nil {
		err = tpl.ExecuteTemplate(&b, "error", map[string]string{"Heading": heading, "Detail": detail, "Theme": themeOf(r)})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	if err != nil {
		w.Write([]byte(heading))
		return
	}
	w.Write(b.Bytes())
}

// isLoopbackHost: localhost or a loopback IP literal.
func isLoopbackHost(h string) bool {
	if strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

// hostAllowed is the DNS-rebinding defence: the Host header must be exactly an address the
// server is bound to (or a name the operator configured). On a loopback bind that is the
// loopback names with the bound port. On a wildcard bind any IP literal with the bound port
// is accepted (an IP literal cannot be rebound); a name must be listed with --allow-host.
func (s *Server) hostAllowed(host string) bool {
	host = strings.ToLower(host)
	bound := s.boundAddr()
	if bound == "" { // not listening yet (tests build the handler first): the configured listen address
		bound = s.opts.Listen
	}
	bhost, bport, err := net.SplitHostPort(bound)
	if err != nil {
		return false
	}
	for _, a := range s.opts.AllowHosts {
		if strings.EqualFold(a, host) {
			return true
		}
	}
	h, p, err := net.SplitHostPort(host)
	if err != nil || p != bport {
		return false
	}
	if s.loopback {
		return isLoopbackHost(h)
	}
	if strings.EqualFold(h, bhost) {
		return true
	}
	if ip := net.ParseIP(bhost); ip != nil && ip.IsUnspecified() {
		return net.ParseIP(strings.Trim(h, "[]")) != nil
	}
	return false
}

// originOK: when the request carries an Origin it must be this server's own origin. (A
// same-origin GET usually has none; a cross-site request always has one.)
func (s *Server) originOK(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Path != "" && u.Path != "/" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

// static serves the embedded assets. They are public (no session) but still behind the
// Host check; ETag + no-cache so an upgraded binary is picked up at once.
func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		s.refuse(w, r, http.StatusMethodNotAllowed, "Method not allowed", "")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/static/")
	if name == "" || strings.Contains(name, "/") || name == "VENDOR.md" {
		http.NotFound(w, r)
		return
	}
	b, err := assets.FS.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("ETag", assetETag(name, b))
	http.ServeContent(w, r, name, zeroTime, bytes.NewReader(b))
}

// maxDeniedEvents bounds the web_denied events one session can write, so a signed-in client
// cannot fill the events table with refusals; its counter keeps counting.
const maxDeniedEvents = 100

// denied accounts for a refused request. With a valid session the refusal is the signed-in
// user's (or their browser's) doing: it is counted for the footer and recorded as a
// web_denied event. Without one it only bumps an in-memory counter, so anyone who can reach
// the port cannot write to the events table.
func (s *Server) denied(r *http.Request, sess *Session, reason string) {
	if sess == nil {
		s.refusedUnauth.Add(1)
		return
	}
	n := sess.refused.Add(1)
	if s.opts.Store == nil || n > maxDeniedEvents {
		return
	}
	route := r.URL.Path
	if _, pattern := s.mux.Handler(r); pattern != "" {
		route = pattern
	}
	_ = s.opts.Store.AddEvent("user", "web_denied", jsonString(map[string]any{"actor": "user", "reason": reason, "route": route, "method": r.Method}))
}
