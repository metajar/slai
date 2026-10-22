package agents

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/oorbt/slai/internal/web/pane"
	"github.com/oorbt/slai/internal/web/server"
)

// topicPrefix is the SSE topic of an agent's pane: "pane:<agent id>".
const topicPrefix = "pane:"

// hub is the part of the SSE hub the pane push uses (internal/web/sse.Hub implements it).
type hub interface {
	Watched(prefix string) []string
	Publish(topic string, ids ...string)
	OnWatch(f func())
}

// Push keeps the agent pages' terminal view live without anyone polling. A page that shows
// an agent's pane opens its event stream with ?watch=pane:<agent>; while at least one open
// stream watches an agent, Push captures that pane once a second and, only when its content
// hash changed, publishes the topic "pane:<agent>". The page then refetches the pane
// fragment. When nobody watches (a tab closed or hidden, which closes its stream) nothing
// is captured and the loop ends.
type Push struct {
	// Interval between captures of a watched pane (default one second).
	Interval time.Duration
	// W remembers the last capture of each agent; the pane handlers record what they
	// rendered here, so the first capture after a page load is compared with what the
	// user is looking at.
	W pane.Watcher

	mu      sync.Mutex
	hub     hub
	src     PaneSource
	running bool
}

// Attach connects Push to the SSE hub; the hub wakes the loop when a stream starts watching.
func (p *Push) Attach(h hub) {
	p.mu.Lock()
	p.hub = h
	p.mu.Unlock()
	h.OnWatch(p.wake)
}

func (p *Push) setSource(src PaneSource) {
	p.mu.Lock()
	p.src = src
	p.mu.Unlock()
}

func (p *Push) interval() time.Duration {
	if p.Interval > 0 {
		return p.Interval
	}
	return time.Second
}

// wake starts the loop unless it is running.
func (p *Push) wake() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running || p.hub == nil || p.src == nil {
		return
	}
	p.running = true
	go p.loop()
}

func (p *Push) loop() {
	t := time.NewTicker(p.interval())
	defer t.Stop()
	for range t.C {
		if p.Tick(context.Background()) > 0 {
			continue
		}
		p.mu.Lock()
		if len(p.hub.Watched(topicPrefix)) == 0 { // re-checked under the lock: a stream may have just arrived
			p.running = false
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
	}
}

// Running reports whether the capture loop is alive (tests).
func (p *Push) Running() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running
}

// Tick makes one pass: it captures every watched agent's pane and publishes the topic of
// each whose content changed. It returns how many agents are watched. An unwatched agent is
// never captured.
func (p *Push) Tick(ctx context.Context) int {
	p.mu.Lock()
	h, src := p.hub, p.src
	p.mu.Unlock()
	if h == nil || src == nil {
		return 0
	}
	topics := h.Watched(topicPrefix)
	for _, topic := range topics {
		id := strings.TrimPrefix(topic, topicPrefix)
		raw, err := src.CapturePane(ctx, id, true)
		if err != nil {
			continue // a dead or unknown agent has no pane to push
		}
		if p.W.Changed(id, raw) {
			h.Publish(topic, id)
		}
	}
	return len(topics)
}

// ModuleWithPush is Module whose pane view is live: it records what it renders in push.W
// and takes its pane source from the server's Actions.
func ModuleWithPush(push *Push) server.Module {
	return func(d *server.Deps) []server.Route {
		if src, ok := d.Actions.(PaneSource); ok && push != nil {
			push.setSource(src)
		}
		return newModule(d, push).routes()
	}
}
