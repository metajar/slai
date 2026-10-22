// Package memory is the Team memory page (P10) of `slai serve`: browse and search the facts
// under .slai/memory/, see each one's state against integration, the files it cites (recorded
// blob against the current one) and which tasks were given it.
//
// It only reads, through internal/memory (the same state rules as `slai memory ls` and
// doctor). Editing, re-verifying and deleting facts stay with `slai memory` and the TUI; the
// web layer has no route that writes and never touches a memory file itself.
package memory

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/git"
	mem "github.com/oorbt/slai/internal/memory"
	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/templates"
)

type handler struct {
	d *server.Deps
}

// Module serves the memory list and fact detail pages.
func Module(d *server.Deps) []server.Route {
	h := &handler{d: d}
	const reads = "reads .slai/memory/*.md through internal/memory and compares each cited blob with slai/integration (one git ls-tree); changes nothing"
	return []server.Route{
		{Method: "GET", Pattern: "/memory", Calls: reads, Handler: h.list},
		{Method: "GET", Pattern: "/memory/{id}", Calls: reads + "; also reads the memory_used events", Handler: h.detail},
	}
}

func (h *handler) memory() *mem.Memory {
	rev := ""
	if t := h.d.Team(); t != nil {
		rev = t.Project.IntegrationBranch
	}
	return mem.New(filepath.Join(h.d.Dir, ".slai", "memory"), h.d.Store, &git.Repo{Dir: h.d.Dir}, rev)
}

// states in the order the filter and the summary list them.
var states = []string{mem.StateFresh, mem.StateStale, mem.StateUnverified, mem.StateInvalid}

// Explain is the one-line meaning of each state, shown as a legend.
var explain = map[string]string{
	mem.StateFresh:      "verified, and every cited file is unchanged",
	mem.StateStale:      "a cited file changed or is gone since the fact was recorded: do not trust it until it is rechecked",
	mem.StateUnverified: "proposed but not yet verified by the principal, or cites no file so it cannot be grounded",
	mem.StateInvalid:    "the file does not parse: kept on disk, kept out of packs",
}

// Row is one fact (or one invalid file) in the list.
type Row struct {
	ID, Kind, Text, State, Reason, FirstPath, Age string
	MorePaths                                     int
	UsedBy                                        []string
	Class, Glyph                                  string
	Invalid                                       bool
}

// Filter is the list's filter; it lives in the URL.
type Filter struct{ Status, Kind, Q string }

type listData struct {
	Rows     []Row
	Total    int
	Counts   map[string]int
	Summary  string
	Filter   Filter
	Kinds    []string
	States   []StateInfo
	Explain  map[string]string
	Warn     string
	Hrefs    map[string]string // state -> list URL filtered to it
	AllHref  string
	Reindex  string
	Unusable string
}

// StateInfo is one state with its chip.
type StateInfo struct{ Name, Class, Glyph, Meaning string }

func chip(state string) (class, glyph string) {
	switch state {
	case mem.StateFresh:
		return "ok", "✓"
	case mem.StateStale:
		return "waiting", "⚠"
	case mem.StateInvalid:
		return "bad", "✗"
	}
	return "idle", "?"
}

func age(created string, now time.Time) string {
	t, err := time.Parse("2006-01-02", created)
	if err != nil {
		return "-"
	}
	d := int(now.UTC().Sub(t).Hours() / 24)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%dd", d)
}

// usedBy reads the memory_used events: fact id -> the tasks that were given it, and per task
// the deliveries (agent, tokens).
type use struct {
	Task, Agent string
	At          string
	Tokens      int
}

func (h *handler) uses() map[string][]use {
	out := map[string][]use{}
	evs, err := h.d.Store.EventsAfter(0, "memory_used", 5000)
	if err != nil {
		return out
	}
	for i := len(evs) - 1; i >= 0; i-- { // newest first
		var m struct {
			Task    string   `json:"task"`
			Agent   string   `json:"agent"`
			FactIDs []string `json:"fact_ids"`
			Tokens  int      `json:"tokens"`
		}
		if json.Unmarshal([]byte(evs[i].Data), &m) != nil {
			continue
		}
		for _, id := range m.FactIDs {
			out[id] = append(out[id], use{Task: m.Task, Agent: m.Agent, Tokens: m.Tokens, At: time.Unix(evs[i].At, 0).Format("2006-01-02 15:04")})
		}
	}
	return out
}

func tasksOf(us []use) []string {
	seen := map[string]bool{}
	var out []string
	for _, u := range us {
		if u.Task != "" && !seen[u.Task] {
			seen[u.Task] = true
			out = append(out, u.Task)
		}
	}
	sort.Strings(out)
	return out
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := Filter{Status: q.Get("status"), Kind: q.Get("kind"), Q: strings.TrimSpace(q.Get("q"))}
	vs, bad, err := h.memory().Views(r.Context())
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	uses := h.uses()
	now := h.d.Now()
	d := listData{Filter: f, Counts: map[string]int{}, Explain: explain}
	kinds := map[string]bool{}
	var rows []Row
	for _, v := range vs {
		d.Counts[v.State]++
		kinds[v.Kind] = true
		row := Row{ID: v.ID, Kind: v.Kind, Text: clip(v.Text, 140), State: v.State, Reason: v.Reason, Age: age(v.Created, now), UsedBy: tasksOf(uses[v.ID])}
		row.Class, row.Glyph = chip(v.State)
		if len(v.Paths) > 0 {
			row.FirstPath, row.MorePaths = v.Paths[0].Path, len(v.Paths)-1
		}
		rows = append(rows, row)
	}
	for _, b := range bad {
		d.Counts[mem.StateInvalid]++
		row := Row{ID: b.ID, Kind: "-", Text: clip(b.Reason, 140), State: mem.StateInvalid, Age: "-", Invalid: true}
		row.Class, row.Glyph = chip(mem.StateInvalid)
		rows = append(rows, row)
	}
	d.Total = len(rows)
	for _, row := range rows {
		if f.Status != "" && row.State != f.Status {
			continue
		}
		if f.Kind != "" && row.Kind != f.Kind {
			continue
		}
		if f.Q != "" && !strings.Contains(strings.ToLower(row.ID+" "+row.Text+" "+row.FirstPath+" "+row.Kind), strings.ToLower(f.Q)) {
			continue
		}
		d.Rows = append(d.Rows, row)
	}
	for k := range kinds {
		d.Kinds = append(d.Kinds, k)
	}
	sort.Strings(d.Kinds)
	var parts []string
	d.Hrefs = map[string]string{}
	for _, s := range states {
		c, g := chip(s)
		d.States = append(d.States, StateInfo{Name: s, Class: c, Glyph: g, Meaning: explain[s]})
		parts = append(parts, fmt.Sprintf("%d %s", d.Counts[s], s))
		d.Hrefs[s] = "/memory?" + url.Values{"status": {s}}.Encode()
	}
	d.AllHref = "/memory"
	d.Summary = fmt.Sprintf("%d facts: %s", d.Total, strings.Join(parts, ", "))
	if d.Total > 500 {
		d.Warn = fmt.Sprintf("%d memory files (over 500): run `slai memory gc` or prune by hand.", d.Total)
	}
	tpl, err := h.d.Template("memory")
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	h.d.Render(w, r, tpl, templates.Page{Title: "Team memory", Active: "memory", Data: d})
}

// PathRow compares one cited path's recorded blob with the one on integration now.
type PathRow struct {
	Path, Recorded, Current, Result, Note string
	Class                                 string
}

// Detail is one fact's page.
type Detail struct {
	Row
	Author, VerifiedBy, Created, SHA, Check, Decision, Stored string
	Paths                                                     []PathRow
	Changes                                                   []string // what changed, for a stale or invalid fact
	File                                                      string   // the file as it is on disk
	Uses                                                      []use
	Total                                                     int
	Meaning                                                   string
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	if s == "" {
		return "-"
	}
	return s
}

func (h *handler) detail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	m := h.memory()
	// only ids that are files of the memory directory are served, so the id never becomes a path
	facts, bad, err := m.List()
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	var fact *mem.Fact
	for i := range facts {
		if facts[i].ID == id {
			fact = &facts[i]
		}
	}
	var inv *mem.Invalid
	for i := range bad {
		if bad[i].ID == id {
			inv = &bad[i]
		}
	}
	if fact == nil && inv == nil {
		h.d.Fail(w, r, errors.New("no memory fact "+id+": run `slai memory ls` to see them"))
		return
	}
	d := Detail{Uses: h.uses()[id]}
	d.ID = id
	if raw, err := os.ReadFile(filepath.Join(m.Dir, id+".md")); err == nil {
		d.File = string(raw)
	}
	if inv != nil {
		d.State, d.Kind, d.Text = mem.StateInvalid, "-", "This file does not parse, so it is kept out of packs and left on disk."
		d.Class, d.Glyph = chip(d.State)
		d.Changes = []string{"Parse error: " + inv.Reason}
		d.Meaning = explain[d.State]
	} else {
		v, err := m.View(r.Context(), *fact)
		if err != nil {
			h.d.Fail(w, r, err)
			return
		}
		cur, err := m.CurrentBlobs(r.Context(), *fact)
		if err != nil {
			h.d.Fail(w, r, err)
			return
		}
		d.State, d.Reason, d.Kind, d.Text = v.State, v.Reason, fact.Kind, fact.Text
		d.Class, d.Glyph = chip(d.State)
		d.Meaning = explain[d.State]
		d.Author, d.VerifiedBy, d.Created, d.SHA, d.Check, d.Decision, d.Stored = fact.Author, fact.VerifiedBy, fact.Created, fact.IntegrationSHA, fact.Check, fact.Decision, fact.Status
		d.Age = age(fact.Created, h.d.Now())
		for _, p := range fact.Paths {
			row := PathRow{Path: p.Path, Recorded: short(p.Blob), Current: short(cur[p.Path])}
			switch now := cur[p.Path]; {
			case now == "":
				row.Result, row.Class, row.Note, row.Current = "gone", "bad", "path no longer exists on integration", "-"
				d.Changes = append(d.Changes, p.Path+": path no longer exists (recorded "+short(p.Blob)+")")
			case now != p.Blob:
				row.Result, row.Class, row.Note = "changed", "waiting", "the file changed since the fact was recorded"
				d.Changes = append(d.Changes, p.Path+": recorded "+short(p.Blob)+", now "+short(now))
			default:
				row.Result, row.Class = "same", "ok"
			}
			d.Paths = append(d.Paths, row)
		}
		if d.State != mem.StateStale {
			d.Changes = nil
		}
	}
	d.Total = len(d.Uses)
	d.UsedBy = tasksOf(d.Uses)
	tpl, err := h.d.Template("memoryfact")
	if err != nil {
		h.d.Fail(w, r, err)
		return
	}
	h.d.Render(w, r, tpl, templates.Page{Title: "Fact " + id, Active: "memory", Data: d})
}
