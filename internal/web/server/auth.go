package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	cookieName  = "slai_session"
	sessionTTL  = 12 * time.Hour
	maxSessions = 256
)

var zeroTime time.Time

func assetETag(name string, b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:8]) + `"`
}

// Session is one browser's login. Page packages reach it with Deps.Session.
type Session struct {
	id      string
	created time.Time

	refused atomic.Int64 // writes and requests refused for this session

	mu      sync.Mutex
	streams int
}

// AcquireStream reserves one of the session's long-lived streams (SSE); ok is false when
// max are already open. Call release when the stream ends.
func (s *Session) AcquireStream(max int) (release func(), ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.streams >= max {
		return nil, false
	}
	s.streams++
	var once sync.Once
	return func() { once.Do(func() { s.mu.Lock(); s.streams--; s.mu.Unlock() }) }, true
}

type sessions struct {
	now func() time.Time
	mu  sync.Mutex
	m   map[string]*Session
}

func newSessions(now func() time.Time) *sessions {
	return &sessions{now: now, m: map[string]*Session{}}
}

func (ss *sessions) fromRequest(r *http.Request) *Session {
	c, err := r.Cookie(cookieName)
	if err != nil || c.Value == "" {
		return nil
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s := ss.m[c.Value]
	if s == nil {
		return nil
	}
	if ss.now().Sub(s.created) > sessionTTL {
		delete(ss.m, c.Value)
		return nil
	}
	return s
}

func (ss *sessions) create() (*Session, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	s := &Session{id: hex.EncodeToString(b), created: ss.now()}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if len(ss.m) >= maxSessions { // drop the oldest
		var oldest string
		for id, o := range ss.m {
			if oldest == "" || o.created.Before(ss.m[oldest].created) {
				oldest = id
			}
		}
		delete(ss.m, oldest)
	}
	ss.m[s.id] = s
	return s, nil
}

// secureRequest reports whether the browser reached us over HTTPS (directly, or through a
// TLS-terminating proxy that says so). The Secure cookie attribute follows the request,
// not the bind: browsers drop a Secure cookie delivered over plain http:// to anything but
// localhost, which would lock a plain-HTTP non-loopback visitor out after the token exchange.
func secureRequest(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func (s *Server) setCookie(w http.ResponseWriter, r *http.Request, sess *Session) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: sess.id, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: secureRequest(r), MaxAge: int(sessionTTL / time.Second)})
}

// forwardingHeaders mark a request that came through a proxy. Any of them means the peer
// is not the person at this machine, whatever the Host says.
var forwardingHeaders = []string{"X-Forwarded-For", "Forwarded", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-IP"}

// needsToken reports whether a request without a session must do the token exchange. A
// non-loopback bind always does. A loopback bind signs in the first visitor automatically
// only when the request's own Host is a loopback name and it carries no forwarding header:
// a request that arrived through a reverse proxy on the same host (an --allow-host name, or
// X-Forwarded-For and friends) could be anyone who can reach the proxy.
func (s *Server) needsToken(r *http.Request) bool {
	if !s.loopback {
		return true
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if !isLoopbackHost(host) {
		return true
	}
	for _, h := range forwardingHeaders {
		if r.Header.Get(h) != "" {
			return true
		}
	}
	return false
}

// startSession handles a request that has no valid session. A plain loopback visit creates
// one (the visit itself is the proof: Host and Origin are already checked). Otherwise the request
// must be the token exchange: GET / with ?token=; the session is created and the browser is
// redirected to / so the token leaves the address bar. It reports whether the request
// should continue to the route.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request) bool {
	if s.needsToken(r) {
		got := r.URL.Query().Get("token")
		if r.URL.Path != "/" || got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) != 1 {
			s.denied(r, nil, "no_valid_token")
			s.refuse(w, r, http.StatusUnauthorized, "Sign in", signInDetail)
			return false
		}
	}
	sess, err := s.sessions.create()
	if err != nil {
		s.refuse(w, r, http.StatusInternalServerError, "Server error", "Could not start a session.")
		return false
	}
	s.setCookie(w, r, sess)
	if r.URL.Query().Has("token") {
		s.stripToken(w, r)
		return false
	}
	// A new cookie is not on this request: let the route see a session.
	r.AddCookie(&http.Cookie{Name: cookieName, Value: sess.id})
	return true
}

// stripToken redirects to the same URL without the token parameter.
func (s *Server) stripToken(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	q.Del("token")
	u := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
	if u.Path == "" {
		u.Path = "/"
	}
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

// signInDetail is the text of the sign-in page. It says where the token may be used and how
// to rotate it.
const signInDetail = "Open the URL that `slai serve` printed; it carries a token that starts your session, and it is only accepted on the first page load. " +
	"The token is a password sent over plain HTTP: use it on a trusted LAN only, or put a TLS proxy in front. " +
	"A new token is generated every time `slai serve` starts, so restarting it rotates the token and signs everyone out."
