package pages_test

import (
	"os"
	"path/filepath"

	"github.com/oorbt/slai/internal/config"
	"regexp"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/web/pages"
	"github.com/oorbt/slai/internal/web/webtest"
)

// forbidden are the capabilities the web layer must never have (binding decision 1 and the
// addenda): approving, requesting changes, merging, applying a commit, reverting, typing
// raw keystrokes into a pane, and reviving, restarting or reassigning an agent.
// New Agent is allowed (user decision, second addendum).
var forbidden = regexp.MustCompile(`(?i)approve|request[-_]?changes|merge|commit|revert|keystroke|keys|raw|type|revive|restart|reassign|assign|shell|exec`)

// The route table of the full server holds no forbidden capability. Every page module is
// in pages.All, so this test covers a route the moment it is registered.
func TestNoForbiddenRoutes(t *testing.T) {
	h := webtest.New(t, webtest.WithModules(pages.All()...))
	for _, rt := range h.Srv.Routes() {
		if forbidden.MatchString(rt.Pattern) || forbidden.MatchString(rt.Action) {
			// the read-only previews of commit and revert are GET pages and allowed
			if !rt.Write() && strings.Contains(strings.ToLower(rt.Pattern), "preview") {
				continue
			}
			t.Errorf("%s %s (action %q) is a capability the web UI must not have", rt.Method, rt.Pattern, rt.Action)
		}
		if rt.Write() && rt.Action == "" {
			t.Errorf("%s %s has no action name for web_action", rt.Method, rt.Pattern)
		}
		// /capabilities prints what each write calls; an undescribed write makes that claim empty.
		if rt.Write() && rt.Calls == "" {
			t.Errorf("%s %s has no Calls text for /capabilities (name the service/supervisor function it uses)", rt.Method, rt.Pattern)
		}
	}
}

// Every page that takes no path parameter renders for a signed-in user with the page checks
// (200, security headers, no external URL, no inline script).
func TestAllStaticPagesRender(t *testing.T) {
	h := webtest.New(t, webtest.WithModules(pages.All()...), webtest.WithGit())
	// pages that read team.yaml (the new-agent form) need one in the project directory
	if err := os.MkdirAll(filepath.Join(h.Dir, ".slai"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.Write(filepath.Join(h.Dir, ".slai", "team.yaml"), h.Team); err != nil {
		t.Fatal(err)
	}
	h.Login()
	n := 0
	for _, rt := range h.Srv.Routes() {
		if rt.Write() || strings.Contains(rt.Pattern, "{") && rt.Pattern != "/{$}" || rt.Pattern == "/theme" || rt.Pattern == "/events" || rt.Pattern == "/dashboard.css" {
			continue
		}
		n++
		t.Run(rt.Pattern, func(t *testing.T) { h.T = t; h.CheckPage(h.Get(webtest.ConcretePath(rt.Pattern))) })
	}
	if n < 4 {
		t.Fatalf("expected the shell, inbox, reviews and commit-preview pages, rendered %d", n)
	}
}
