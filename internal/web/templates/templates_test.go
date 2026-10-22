package templates

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestLayoutRendersNavAndEscapes(t *testing.T) {
	tpl, err := PageTemplate("dashboard")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	err = tpl.ExecuteTemplate(&b, "layout", Page{Title: "<b>x</b>", Active: "dashboard", Theme: "dark", Notice: "<script>alert(1)</script>",
		Nav:  []NavItem{{Label: "Dashboard", Href: "/", Key: "dashboard", Enabled: true}, {Label: "Board", Href: "/board", Key: "board"}},
		Data: map[string]any{"Agents": []int{}, "Tasks": []int{}, "Orchestrator": ""}})
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, bad := range []string{"<b>x</b>", "<script>alert"} {
		if strings.Contains(out, bad) {
			t.Errorf("unescaped %q in page", bad)
		}
	}
	for _, want := range []string{`data-bs-theme="dark"`, `aria-current="page"`, `aria-disabled="true"`, `X-Slai-Request`, `/static/htmx.min.js`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

// P1 AC (principal): htmx runs with allowEval=false and selfRequestsOnly=true, set by the
// meta tag of the base layout, so the CSP needs no unsafe-eval and htmx cannot call
// another origin.
func TestLayoutConfiguresHtmxSafely(t *testing.T) {
	tpl, err := PageTemplate("dashboard")
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := tpl.ExecuteTemplate(&b, "layout", Page{Title: "x", Theme: "light", Data: map[string]any{"Agents": []int{}, "Tasks": []int{}, "Orchestrator": ""}}); err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`<meta name="htmx-config" content='([^']*)'>`).FindStringSubmatch(b.String())
	if m == nil {
		t.Fatal("layout has no htmx-config meta tag")
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(m[1]), &cfg); err != nil {
		t.Fatalf("htmx-config is not JSON: %v", err)
	}
	if cfg["allowEval"] != false || cfg["selfRequestsOnly"] != true {
		t.Fatalf("htmx-config = %v, want allowEval=false and selfRequestsOnly=true", cfg)
	}
}
