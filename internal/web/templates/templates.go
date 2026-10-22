// Package templates holds the HTML templates of `slai serve`, embedded in the binary.
//
// layout/ is the base page (nav, banners, scripts) and is owned by the server shell; every
// other page lives in its own sub-directory (dashboard/, board/, ...) and only defines the
// blocks it fills: "title", "content" and optionally "scripts". Adding a directory of
// *.html files is enough to embed it. Templates use html/template, so every value is
// escaped for its context; never mark agent-supplied text as trusted markup (lint_test.go enforces it).
package templates

import (
	"embed"
	"fmt"
	"html/template"
	"path"
	"sync"
	"time"
)

//go:embed */*.html
var FS embed.FS

// NavItem is one sidebar entry. Enabled is false while no route serves its page yet.
type NavItem struct {
	Label, Href, Key string
	Enabled          bool
	Badge            int    // a count shown beside the label (e.g. items that need you); 0 hides it
	Hint             string // tooltip for the badge
}

// Page is what every template is executed with. Data is the page's own value.
type Page struct {
	Title    string // the <title> and heading
	Active   string // the Key of the NavItem to mark current
	Nav      []NavItem
	ReadOnly bool   // the server was started with --read-only: hide write controls
	Theme    string // "light" or "dark"
	Notice   string // a banner for the whole app, e.g. the supervisor is not running
	// Refused counts the requests refused for this session; RefusedUnauth those refused
	// with no session since the server started. Both are shown in the footer.
	Refused, RefusedUnauth int64
	// Needs is the number of items that need the user (the inbox badge); the tab title is
	// prefixed with it.
	Needs int
	Data  any
}

var (
	mu    sync.Mutex
	cache = map[string]*template.Template{}
)

// PageTemplate returns the layout plus every template file in dir (for example "board"), ready to
// execute as "layout". It is parsed once and cached.
func PageTemplate(dir string) (*template.Template, error) {
	mu.Lock()
	defer mu.Unlock()
	if t, ok := cache[dir]; ok {
		return t, nil
	}
	t, err := template.New("layout").Funcs(template.FuncMap{"dict": dict}).ParseFS(FS, "layout/*.html")
	if err != nil {
		return nil, fmt.Errorf("layout: %w", err)
	}
	if dir != "layout" {
		if t, err = t.ParseFS(FS, path.Join(dir, "*.html")); err != nil {
			return nil, fmt.Errorf("%s: %w", dir, err)
		}
	}
	cache[dir] = t
	return t, nil
}

// dict builds a map from key, value pairs, to pass several values to a sub-template.
func dict(kv ...any) (map[string]any, error) {
	if len(kv)%2 != 0 {
		return nil, fmt.Errorf("dict: odd number of arguments")
	}
	m := make(map[string]any, len(kv)/2)
	for i := 0; i < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			return nil, fmt.Errorf("dict: key %v is not a string", kv[i])
		}
		m[k] = kv[i+1]
	}
	return m, nil
}

// Age renders a duration the way the web UI shows ages: "just now", "5m", "2h10m", "3d".
func Age(d time.Duration) string {
	switch {
	case d < 45*time.Second:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute)/time.Minute))
	case d < 48*time.Hour:
		h := int(d / time.Hour)
		return fmt.Sprintf("%dh%02dm", h, int(d/time.Minute)%60)
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// Stamp is a local clock time and the age since, "11:31 (38m ago)", relative to now.
func Stamp(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	a := Age(now.Sub(t))
	if a == "just now" {
		return t.Local().Format("15:04") + " (just now)"
	}
	return t.Local().Format("15:04") + " (" + a + " ago)"
}
