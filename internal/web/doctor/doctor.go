// Package doctor is the Doctor page of `slai serve`. The checks are the ones `slai doctor`
// runs (the page asks the same function for its text and reads the result; it implements
// no check of its own), shown as rows with their hints, under a summary that names every
// warning and failure. The page's exit state is a banner, never a process exit. The
// --lane and --panes probes can be run from here: both are read-only.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/templates"
)

// Source is what the page needs from the CLI side. The real one is cmd's web actions; the
// server's Actions value is asserted to have these methods, and without them the page says
// the checks are unavailable.
type Source interface {
	// DoctorReport is the text `slai doctor` prints and its failure count.
	DoctorReport(ctx context.Context) (text string, failures int, err error)
	// DoctorProbe runs `slai doctor --lane` or `--panes` ("lane" or "panes").
	DoctorProbe(ctx context.Context, which string) (string, error)
}

// CacheFor is how long a report is reused; "Re-run checks" bypasses it.
var CacheFor = 30 * time.Second

// Module registers /doctor and the probe runner.
func Module(d *server.Deps) []server.Route {
	m := &mod{d: d}
	return []server.Route{
		{Method: http.MethodGet, Pattern: "/doctor", Calls: "the same checks as `slai doctor` (read-only), via the CLI's runDoctorChecks; the last --lane / --panes results from the store", Handler: m.page},
		{Method: http.MethodGet, Pattern: "/f/doctor", Calls: "the same read-only checks (cached for 30 s) and stored probe results, as the page's refetch fragment", Handler: m.fragment},
		{Method: http.MethodPost, Pattern: "/doctor/run", Action: "doctor_probe", Target: func(r *http.Request) string { return r.PostFormValue("which") },
			Calls: "`slai doctor --lane` or `--panes`: read-only probes (synthetic guard payloads; pane capture); the result is stored for the page", Handler: m.run},
	}
}

type mod struct {
	d *server.Deps

	mu     sync.Mutex
	cached *report
}

// Level of a row.
const (
	OK   = "ok"
	Warn = "warn"
	Fail = "fail"
	Info = "info"
	Text = "text"
)

type Row struct {
	Level, Chip, Label, Body string
	Hints                    []string
}

type report struct {
	At       time.Time
	Rows     []Row
	Warnings []string // names, deduplicated, in order
	Failures []string
	Checked  int
	Err      string
}

// Summary is the banner text: it names every warning and failure.
func (r report) Summary() string {
	var parts []string
	if n := len(r.Failures); n > 0 {
		parts = append(parts, plural(n, "failure")+": "+strings.Join(r.Failures, ", "))
	}
	if n := len(r.Warnings); n > 0 {
		parts = append(parts, plural(n, "warning")+": "+strings.Join(r.Warnings, ", "))
	}
	if len(parts) == 0 {
		return "All checks passed."
	}
	return strings.Join(parts, "; ")
}

// Class is the banner colour: bad when anything failed, hold-amber for warnings.
func (r report) Class() string {
	switch {
	case len(r.Failures) > 0:
		return "banner-bad"
	case len(r.Warnings) > 0:
		return "banner-hold"
	}
	return "banner-info"
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return itoa(n) + " " + word + "s"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// labels name a warning or failure in the summary: the first keyword found in the line.
var labels = []struct{ key, name string }{
	{"older slai build", "build"}, {"old build", "build"}, {"build", "build"},
	{"orphan", "orphans"}, {"worktree", "worktrees"}, {"pidfile", "supervisor pidfile"},
	{"integration branch", "integration branch"}, {"mcp", "mcp"}, {"hook", "hooks"},
	{"permission mode", "claude modes"}, {"claude", "claude"}, {"tmux", "tmux"}, {"git", "git"},
	{"memory", "memory"}, {"fact", "memory"}, {"state.db", "state.db"}, {"team.yaml", "team.yaml"},
	{"lane", "lane guard"}, {"harness", "harnesses"}, {"supervisor", "supervisor"}, {"identity", "identity"},
	{"escalation", "escalations"},
}

func labelOf(line string) string {
	low := strings.ToLower(line)
	for _, l := range labels {
		if strings.Contains(low, l.key) {
			return l.name
		}
	}
	f := strings.Fields(line)
	if len(f) > 3 {
		f = f[:3]
	}
	return strings.Join(f, " ")
}

// Parse turns the text `slai doctor` prints into rows. A line starting with a check mark,
// a cross, "! warn:" or a warning sign is a result; the indented meaning:/next: lines under
// it are its hints; everything else is shown as plain text. Unknown lines are never dropped.
func Parse(text string) report {
	var r report
	seenW, seenF := map[string]bool{}, map[string]bool{}
	for _, raw := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if (strings.HasPrefix(line, "meaning:") || strings.HasPrefix(line, "next:")) && len(r.Rows) > 0 {
			last := &r.Rows[len(r.Rows)-1]
			last.Hints = append(last.Hints, line)
			continue
		}
		row := Row{Level: Text, Body: line}
		switch {
		case strings.HasPrefix(line, "✓"):
			row.Level, row.Chip, row.Body = OK, "chip-ok", strings.TrimSpace(strings.TrimPrefix(line, "✓"))
		case strings.HasPrefix(line, "✗"):
			row.Level, row.Chip, row.Body = Fail, "chip-bad", strings.TrimSpace(strings.TrimPrefix(line, "✗"))
		case strings.HasPrefix(line, "! warn:"):
			row.Level, row.Chip, row.Body = Warn, "chip-warn", strings.TrimSpace(strings.TrimPrefix(line, "! warn:"))
		case strings.HasPrefix(line, "⚠"):
			row.Level, row.Chip, row.Body = Warn, "chip-warn", strings.TrimSpace(strings.TrimPrefix(line, "⚠"))
		case strings.HasPrefix(line, "- "):
			row.Level, row.Chip, row.Body = Info, "chip-idle", strings.TrimSpace(strings.TrimPrefix(line, "- "))
		}
		switch row.Level {
		case OK:
			r.Checked++
		case Fail:
			r.Checked++
			if n := labelOf(row.Body); !seenF[n] {
				seenF[n] = true
				r.Failures = append(r.Failures, n)
			}
		case Warn:
			r.Checked++
			if n := labelOf(row.Body); !seenW[n] {
				seenW[n] = true
				r.Warnings = append(r.Warnings, n)
			}
		}
		row.Label = map[string]string{OK: "ok", Warn: "warn", Fail: "fail", Info: "info"}[row.Level]
		r.Rows = append(r.Rows, row)
	}
	return r
}

func (m *mod) source() (Source, bool) {
	s, ok := m.d.Actions.(Source)
	return s, ok
}

func (m *mod) report(r *http.Request) (*report, error) {
	src, ok := m.source()
	if !ok {
		return nil, errors.New("this server has no connection to the doctor checks")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.d.Now()
	if m.cached != nil && r.URL.Query().Get("rerun") != "1" && now.Sub(m.cached.At) < CacheFor {
		return m.cached, nil
	}
	text, _, err := src.DoctorReport(r.Context())
	rep := Parse(text)
	rep.At = now
	if err != nil {
		rep.Err = err.Error()
	}
	m.cached = &rep
	return m.cached, nil
}

type probe struct {
	Which, Title, Command, About string
	Ran                          bool
	When, Text                   string
}

type view struct {
	Report   *report
	Missing  string
	At       string
	Probes   []probe
	ReadOnly bool
}

const (
	kvProbe = "web.doctor."
)

type stored struct {
	At   int64  `json:"at"`
	Text string `json:"text"`
}

func (m *mod) probes() []probe {
	out := []probe{
		{Which: "lane", Title: "slai doctor --lane", Command: "slai doctor --lane",
			About: "Fires known-bad writes at each agent's lane guard offline, with synthetic payloads (nothing is executed or written), and checks each was denied."},
		{Which: "panes", Title: "slai doctor --panes", Command: "slai doctor --panes",
			About: "Classifies every live pane with the same rules the supervisor uses and shows which rule matched. It only reads pane text."},
	}
	for i := range out {
		if v, ok, _ := m.d.Store.GetKV(kvProbe + out[i].Which); ok {
			var s stored
			if json.Unmarshal([]byte(v), &s) == nil {
				out[i].Ran, out[i].Text = true, s.Text
				out[i].When = templates.Stamp(m.d.Now(), time.Unix(s.At, 0))
			}
		}
	}
	return out
}

func (m *mod) view(r *http.Request) view {
	v := view{Probes: m.probes(), ReadOnly: m.d.ReadOnly}
	rep, err := m.report(r)
	if err != nil {
		v.Missing = err.Error()
	} else {
		v.Report = rep
		v.At = templates.Stamp(m.d.Now(), rep.At)
	}
	return v
}

func (m *mod) page(w http.ResponseWriter, r *http.Request) {
	tpl, err := m.d.Template("doctor")
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.d.Render(w, r, tpl, templates.Page{Title: "Doctor", Active: "doctor", Data: m.view(r)})
}

// fragment is the live region alone (the page's refetch): it never re-runs the checks ahead of
// the 30 s cache, whatever the query says.
func (m *mod) fragment(w http.ResponseWriter, r *http.Request) {
	r.URL.RawQuery = ""
	tpl, err := m.d.Template("doctor")
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.d.Fragment(w, r, tpl, "doctor-live", m.view(r))
}

// run executes one read-only probe and stores its result for the page.
func (m *mod) run(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		m.d.Fail(w, r, err)
		return
	}
	which := r.PostForm.Get("which")
	if which != "lane" && which != "panes" {
		m.d.Fail(w, r, errors.New("unknown probe: choose lane or panes"))
		return
	}
	src, ok := m.source()
	if !ok {
		m.d.Fail(w, r, errors.New("this server has no connection to the doctor checks"))
		return
	}
	text, err := src.DoctorProbe(r.Context(), which)
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	b, _ := json.Marshal(stored{At: m.d.Now().Unix(), Text: text})
	_ = m.d.Store.SetKV(kvProbe+which, string(b))
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/doctor")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/doctor", http.StatusSeeOther)
}
