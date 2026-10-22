package sse

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/oorbt/slai/internal/web/server"
)

// Module serves GET /events. The hub is created from the server's store when the module is
// registered; onHub, if set, receives it (the pane push publishes "pane:<agent>" topics to
// it). There is one stream per browser tab, at most DefaultMaxPerSession per session.
func Module(onHub func(*Hub)) server.Module {
	return func(d *server.Deps) []server.Route {
		h := New(Config{Source: d.Store}) // real time: coalescing and heartbeats are wall-clock
		if onHub != nil {
			onHub(h)
		}
		return []server.Route{{Method: http.MethodGet, Pattern: "/events",
			Calls:   "streams change notices (topic names and ids, never page content) from the events table",
			Handler: func(w http.ResponseWriter, r *http.Request) { serve(h, d, w, r) }}}
	}
}

func serve(h *Hub, d *server.Deps, w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming is not supported here", http.StatusInternalServerError)
		return
	}
	release, ok := d.Session(r).AcquireStream(DefaultMaxPerSession)
	if !ok {
		http.Error(w, fmt.Sprintf("too many open streams for this session (%d): close another tab", DefaultMaxPerSession), http.StatusTooManyRequests)
		return
	}
	defer release()
	var last int64
	if v := r.Header.Get("Last-Event-ID"); v != "" {
		last, _ = strconv.ParseInt(v, 10, 64)
	}
	sub := h.Subscribe(last)
	defer sub.Close()
	sub.SetWatch(watchTopics(r.URL.Query().Get("watch")))
	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache, no-store")
	hd.Set("X-Accel-Buffering", "no") // a proxy must not buffer the stream
	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, "retry: 3000\n: connected\n\n")
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case f, open := <-sub.C:
			if !open {
				return
			}
			if _, err := w.Write(f.Bytes()); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// watchRe is the only kind of topic a page may ask to have watched: a pane, by agent id.
var watchRe = regexp.MustCompile(`^pane:[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// watchTopics parses ?watch=pane:eng-1,pane:pm: the topics the page is looking at. Anything
// that is not a well-formed pane topic is dropped, and at most MaxWatch are kept.
func watchTopics(q string) []string {
	var out []string
	for _, t := range strings.Split(q, ",") {
		if watchRe.MatchString(t) {
			out = append(out, t)
		}
		if len(out) == MaxWatch {
			break
		}
	}
	return out
}
