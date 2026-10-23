package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/agentrows"
	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/health"
	"github.com/oorbt/slai/internal/ratelimit"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/webtest"
)

var now = time.Date(2026, 10, 22, 12, 0, 0, 0, time.Local)

func teamOf(agents ...config.Agent) *config.Team {
	t := config.Preset("solo", "p", "d", "main")
	t.Agents = agents
	return t
}

func baseInput() Input {
	return Input{Now: now, Heartbeat: now.Unix(),
		Team:   teamOf(config.Agent{ID: "orch", Role: config.RoleOrchestrator, Model: "sonnet"}),
		Health: map[string]service.Health{}, Questions: map[string]string{}, SpendBy: map[string]float64{},
		Build: service.BuildState{Current: "aaaa", Supervisor: "aaaa"}}
}

func agent(id, status string) store.Agent {
	return store.Agent{ID: id, Role: "engineer", Status: status, StatusAt: now.Unix() - 120}
}

// P2 AC1: one chip per agent, carrying the classifier's value and reason, for every liveness.
func TestOneChipPerAgentForEveryLiveness(t *testing.T) {
	in := baseInput()
	for i, l := range health.All {
		id := fmt.Sprintf("eng-%d", i)
		in.Agents = append(in.Agents, agent(id, "working"))
		in.Health[id] = service.Health{Liveness: string(l), Reason: "why-" + string(l), LastActivity: now.Unix() - 90}
	}
	v := Build(in)
	cards := map[string]AgentCard{}
	for _, c := range append(append([]AgentCard{}, v.Active...), v.Idle...) {
		cards[c.ID] = c
	}
	if len(cards) != len(health.All) {
		t.Fatalf("%d cards for %d agents", len(cards), len(health.All))
	}
	for i, l := range health.All {
		c := cards[fmt.Sprintf("eng-%d", i)]
		if c.Chip.Label == "" || c.Chip.Icon == "" || c.Chip.Class == "" {
			t.Errorf("%s: chip %+v lacks a label, icon or class (colour must not be the only signal)", l, c.Chip)
		}
		if c.Chip.Reason != "why-"+string(l) {
			t.Errorf("%s: reason %q is not the classifier's", l, c.Chip.Reason)
		}
		if c.Chip.Gloss == "" {
			t.Errorf("%s: no glossary text on the chip", l)
		}
	}
}

// P2 AC2: banners appear for each seeded condition and are absent otherwise.
func TestBanners(t *testing.T) {
	has := func(v View, kind string) bool {
		for _, b := range v.Banners {
			if b.Kind == kind {
				return true
			}
		}
		return false
	}
	quiet := Build(baseInput())
	for _, k := range []string{"supervisor", "build", "hold", "paused", "orphans"} {
		if has(quiet, k) {
			t.Errorf("%s banner on a healthy team", k)
		}
	}
	in := baseInput()
	in.Heartbeat = now.Unix() - 120
	if v := Build(in); !has(v, "supervisor") {
		t.Error("no supervisor-down banner for a 2-minute-old heartbeat")
	}
	in = baseInput()
	in.Heartbeat = 0
	if v := Build(in); !has(v, "supervisor") {
		t.Error("no supervisor-down banner for a supervisor that never ran")
	}
	in = baseInput()
	in.Build.Supervisor = "bbbb"
	if v := Build(in); !has(v, "build") {
		t.Error("no old-build banner")
	}
	in = baseInput()
	in.Gate = service.WakeGate{Paused: true, TeamHold: true, HoldName: "nightly", HoldSince: now.Unix() - 600}
	v := Build(in)
	var hold Banner
	for _, b := range v.Banners {
		if b.Kind == "hold" {
			hold = b
		}
	}
	if !strings.Contains(hold.Title, "nightly") || !hold.Resume || has(v, "paused") {
		t.Errorf("hold banner = %+v", hold)
	}
	in = baseInput()
	in.Gate = service.WakeGate{Paused: true}
	if v := Build(in); !has(v, "paused") || has(v, "hold") {
		t.Error("plain pause banner")
	}
	in = baseInput()
	in.Orphans = []string{"slai_ab12_", "slai_cd34_"}
	if v := Build(in); !has(v, "orphans") || !strings.Contains(v.Banners[0].Text, "2 orphaned slai teams") {
		t.Errorf("orphan banner: %+v", v.Banners)
	}
}

// P2 AC3 and addendum AC9: caps read OFF when unset; 80% and 100% differ in class and in words.
func TestSpendCapsOffWarnAndCap(t *testing.T) {
	in := baseInput()
	in.SpendDay = 5
	v := Build(in)
	if v.Spend.Window != "today since 00:00 local, from recorded per-agent cost" {
		t.Errorf("spend window label = %q", v.Spend.Window)
	}
	for _, r := range v.Spend.Rows {
		if !r.OFF || r.Cap != "OFF" || !strings.Contains(r.Text, "OFF") || r.HasMeter {
			t.Errorf("%s with no cap: %+v", r.Label, r)
		}
	}
	in.Team.Defaults.Caps.DailyUSD = 100
	seen := map[string]SpendRow{}
	for spent, want := range map[float64]string{50: "ok", 80: "warn", 99.9: "warn", 100: "bad", 130: "bad"} {
		in.SpendDay = spent
		r := Build(in).Spend.Rows[0]
		if r.State != want || r.OFF || !r.HasMeter {
			t.Errorf("$%.1f of $100: %+v, want %s", spent, r, want)
		}
		seen[r.State] = r
	}
	if seen["warn"].Text == seen["bad"].Text || seen["ok"].Text == seen["warn"].Text {
		t.Error("states are not told apart in words")
	}
	if !strings.Contains(seen["warn"].Text, "warning") || !strings.Contains(seen["bad"].Text, "cap reached") {
		t.Errorf("texts: warn %q bad %q", seen["warn"].Text, seen["bad"].Text)
	}
	if seen["warn"].Icon == seen["bad"].Icon || seen["warn"].Icon == "" {
		t.Error("warn and cap share an icon")
	}
}

// Addendum AC8: an n/a gauge is neutral, says why, and never wears the healthy class.
func TestNAGaugeIsNeutral(t *testing.T) {
	in := baseInput()
	for _, g := range Build(in).Gauges {
		if !g.NA || g.Class != "chip-neutral" || g.Reason == "" || strings.Contains(g.Class, "ok") {
			t.Errorf("no-data gauge: %+v", g)
		}
	}
	in.Snap = &ratelimit.Snapshot{FiveHour: &ratelimit.Window{UsedPercent: 41, ResetsAt: now.Add(38 * time.Minute).Unix()}}
	g := Build(in).Gauges
	if g[0].NA || g[0].Class != "chip-ok" || !strings.Contains(g[0].Reason, "(in 38m)") || !strings.Contains(g[0].Reason, "resets 12:38") {
		t.Errorf("5h gauge: %+v", g[0])
	}
	if !g[1].NA || g[1].Class != "chip-neutral" {
		t.Errorf("weekly gauge with no data: %+v", g[1])
	}
}

// Addendum AC5: the status sentence comes from a fixed rule table.
func TestStatusSentenceTable(t *testing.T) {
	cases := []struct {
		name string
		s    SentenceState
		want string
	}{
		{"hold", SentenceState{Hold: &HoldView{}, Working: 3}, "Team is on hold. Nothing runs until you resume."},
		{"named hold", SentenceState{Hold: &HoldView{}, HoldName: "night"}, "Team is on hold (night). Nothing runs until you resume."},
		{"supervisor", SentenceState{SupervisorDown: true}, "The supervisor is not running, so nobody is being woken. Start it with `slai up`."},
		{"one need", SentenceState{NeedsYou: []string{"eng-1 blocked"}}, "1 thing needs you: eng-1 blocked."},
		{"three needs", SentenceState{NeedsYou: []string{"a", "b", "c"}}, "3 things need you: a; b; c."},
		{"five needs", SentenceState{NeedsYou: []string{"a", "b", "c", "d", "e"}}, "5 things need you: a; b; c, and 2 more."},
		{"paused", SentenceState{Paused: true, Tasks: 2}, "Wake-ups are paused. Resume to let the team continue."},
		{"first run", SentenceState{Orchestrator: "orch"}, "Nothing has been asked yet. Tell orch what you want built."},
		{"working", SentenceState{Tasks: 2, Working: 1, Orchestrator: "orch"}, "The team is working. Nothing needs you."},
		{"idle", SentenceState{Tasks: 2, Orchestrator: "orch"}, "The team is idle. Nothing needs you."},
		{"hold beats needs", SentenceState{Hold: &HoldView{}, NeedsYou: []string{"x"}}, "Team is on hold. Nothing runs until you resume."},
		{"needs beat paused", SentenceState{Paused: true, NeedsYou: []string{"x"}}, "1 thing needs you: x."},
	}
	for _, c := range cases {
		if got := Sentence(c.s); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	in := baseInput()
	in.Agents = []store.Agent{agent("eng-1", "waiting_on_user")}
	in.Questions["eng-1"] = "Which database?"
	in.Tasks = []store.Task{{ID: "T-1", Status: store.StatusInProgress}}
	v := Build(in)
	if v.NeedsYou != 1 || !strings.HasPrefix(v.Sentence, "1 thing needs you: eng-1 waiting on you") || v.Needs[0].Text != "Which database?" {
		t.Errorf("view: sentence %q needs %+v", v.Sentence, v.Needs)
	}
}

// Addendum AC6: every liveness value and the five everyday words have a glossary line.
func TestEveryLivenessHasAGlossaryEntry(t *testing.T) {
	for _, l := range health.All {
		if strings.TrimSpace(Glossary[string(l)]) == "" {
			t.Errorf("no glossary entry for %s", l)
		}
	}
	for _, w := range glossaryWords {
		if strings.TrimSpace(Glossary[w]) == "" {
			t.Errorf("no glossary entry for %q", w)
		}
	}
	in := baseInput()
	in.Agents = []store.Agent{agent("eng-1", "working"), agent("eng-2", "working")}
	in.Health["eng-1"] = service.Health{Liveness: "blocked", Reason: "permission prompt"}
	in.Health["eng-2"] = service.Health{Liveness: "gated", Reason: "paused"}
	for _, n := range Build(in).Needs {
		if n.Gloss == "" {
			t.Errorf("Needs-attention row %s/%s has no explanation", n.Who, n.Label)
		}
	}
}

// Addendum AC7: healthy idle agents collapse into one line; every other agent is a full card.
func TestIdleAgentsCollapseOthersDoNot(t *testing.T) {
	in := baseInput()
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("idle-%d", i)
		in.Agents = append(in.Agents, agent(id, "idle"))
		in.Health[id] = service.Health{Liveness: "idle"}
	}
	for _, l := range []string{"alive", "busy", "blocked", "stale", "dead", "rate_limited", "gated"} {
		in.Agents = append(in.Agents, agent("x-"+l, "working"))
		in.Health["x-"+l] = service.Health{Liveness: l, Reason: "r"}
	}
	v := Build(in)
	if len(v.Idle) != 5 || len(v.Active) != 7 {
		t.Fatalf("idle %d active %d", len(v.Idle), len(v.Active))
	}
	for _, c := range v.Active {
		if c.Chip.Label == "idle" {
			t.Errorf("idle agent %s shown as a full card", c.ID)
		}
	}
}

func TestSentenceSeenInPageAndNoDemoControls(t *testing.T) {
	entries, _ := os.ReadDir(filepath.Join("..", "templates", "dashboard"))
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join("..", "templates", "dashboard", e.Name()))
		if strings.Contains(string(b), "Demo") {
			t.Errorf("%s carries a prototype-only control", e.Name())
		}
	}
}

func harness(t *testing.T, opts ...webtest.Option) *webtest.Harness {
	t.Helper()
	h := webtest.New(t, append([]webtest.Option{webtest.WithModules(Module(Config{BuildID: "aaaa"}))}, opts...)...)
	h.Login()
	return h
}

// P2 AC4: with zero tasks the first-run hint names the orchestrator and has a send box for it.
func TestFirstRunHintNamesOrchestratorWithSendBox(t *testing.T) {
	h := harness(t, webtest.Setup(func(st *store.Store, _ *config.Team) { _ = st.SetSupervisorHeartbeat(1_800_000_000) }))
	r := h.Get("/")
	h.CheckPage(r)
	for _, want := range []string{"Start here", "Tell <strong>orch</strong>", `hx-post="/agents/orch/send"`, `name="body"`, "Nothing has been asked yet. Tell orch"} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("first-run page lacks %q", want)
		}
	}
	// with a task, the hint goes away
	if _, err := h.Store.CreateTask(store.Task{Title: "x", Assignee: "eng-1", CreatedBy: "orch"}); err != nil {
		t.Fatal(err)
	}
	if r := h.Get("/"); strings.Contains(r.Body, "Start here") {
		t.Error("first-run hint with a task on the board")
	}
	// read-only: no send box, the CLI is named instead
	ro := harness(t, webtest.ReadOnly())
	if r := ro.Get("/"); strings.Contains(r.Body, `hx-post="/agents/orch/send"`) || !strings.Contains(r.Body, "read-only") {
		t.Error("read-only server offers a send box")
	}
}

// Addendum AC10/11: the "N need you" chip links to Inbox; the live region refreshes by htmx
// every 60 s and names the SSE topics it depends on; the fragment is the same markup.
func TestPageLinksInboxAndRefreshes(t *testing.T) {
	h := harness(t, webtest.Setup(func(st *store.Store, team *config.Team) {
		_ = st.SetSupervisorHeartbeat(1_800_000_000)
		_ = st.UpsertAgent(store.Agent{ID: "eng-1", Role: "engineer"})
		_ = st.SetAgentStatus("eng-1", "waiting_on_user")
		_ = st.AddEvent("eng-1", "ask_user", `{"question":"Postgres or SQLite?"}`)
	}))
	r := h.Get("/")
	h.CheckPage(r)
	for _, want := range []string{`href="/inbox"`, "1 need you", `hx-trigger="every 60s"`, `hx-get="/f/dashboard"`, `data-live="agents tasks alerts spend inbox"`,
		"Postgres or SQLite?", "1 thing needs you: eng-1 waiting on you"} {
		if !strings.Contains(r.Body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	f := h.Get("/f/dashboard")
	if f.Code != 200 || strings.Contains(f.Body, "<html") || !strings.Contains(f.Body, `id="dash"`) || !strings.Contains(f.Body, "Postgres or SQLite?") {
		t.Errorf("fragment: %d %.200s", f.Code, f.Body)
	}
	h.CheckHeaders(f)
	// agent text is escaped
	_ = h.Store.AddEvent("eng-1", "ask_user", `{"question":"<script>alert(1)</script>"}`)
	if r := h.Get("/"); strings.Contains(r.Body, "<script>alert") {
		t.Error("agent-supplied question rendered unescaped")
	}
}

// ---- hold / pause / resume (P12 AC1) ----

type fakeCtl struct {
	st    *store.Store
	calls []string
}

func (f *fakeCtl) SendUser(string, string) (int64, error)                { return 1, nil }
func (f *fakeCtl) CommitPreview(context.Context) (string, error)         { return "", nil }
func (f *fakeCtl) RevertPreview(context.Context, string) (string, error) { return "", nil }
func (f *fakeCtl) Pause() error                                          { f.calls = append(f.calls, "pause"); return f.st.SetKV("paused", "1") }
func (f *fakeCtl) Resume() error {
	f.calls = append(f.calls, "resume")
	_ = f.st.DeleteKV("team.hold")
	return f.st.SetKV("paused", "0")
}
func (f *fakeCtl) HoldTeam(name string) error {
	f.calls = append(f.calls, "hold:"+name)
	b, _ := json.Marshal(map[string]any{"name": name, "since": now.Unix()})
	_ = f.st.SetKV("team.hold", string(b))
	return f.st.SetKV("paused", "1")
}

type browser struct {
	t      *testing.T
	srv    *server.Server
	cookie *http.Cookie
}

func (b *browser) do(method, path string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	b.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	req.Host = webtest.Addr
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if b.cookie != nil {
		req.AddCookie(b.cookie)
	}
	if method != "GET" {
		req.Header.Set("X-Slai-Request", "1")
		req.Header.Set("Origin", "http://"+webtest.Addr)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	b.srv.Handler().ServeHTTP(rec, req)
	for _, c := range rec.Result().Cookies() {
		if c.Name == "slai_session" {
			b.cookie = c
		}
	}
	return rec
}

func ctlServer(t *testing.T, readOnly bool) (*browser, *fakeCtl, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	team := teamOf(config.Agent{ID: "orch", Role: config.RoleOrchestrator, Model: "sonnet"}, config.Agent{ID: "eng-1", Role: config.RoleEngineer, Model: "sonnet"})
	ctl := &fakeCtl{st: st}
	srv, err := server.New(server.Options{Dir: t.TempDir(), Store: st, Team: func() *config.Team { return team }, Actions: ctl,
		Listen: webtest.Addr, ReadOnly: readOnly, Now: func() time.Time { return now }, Modules: []server.Module{Module(Config{BuildID: "aaaa"})}})
	if err != nil {
		t.Fatal(err)
	}
	srv.SetBound(webtest.Addr)
	b := &browser{t: t, srv: srv}
	b.do("GET", "/", nil, nil)
	if b.cookie == nil {
		t.Fatal("no session")
	}
	return b, ctl, st
}

// P12 AC1: hold / pause / resume use the supervisor's own calls, record a web_action each,
// and the banner shows the hold by name.
func TestHoldPauseResumeFromTheHeader(t *testing.T) {
	b, ctl, st := ctlServer(t, false)
	if r := b.do("POST", "/team/hold", url.Values{"name": {"nightly"}}, nil); r.Code != 200 && r.Code != 303 {
		t.Fatalf("hold: %d %s", r.Code, r.Body.String())
	}
	page := b.do("GET", "/", nil, nil).Body.String()
	for _, want := range []string{"Team on hold: nightly", "Team is on hold (nightly)", `hx-post="/team/resume"`} {
		if !strings.Contains(page, want) {
			t.Errorf("held page lacks %q", want)
		}
	}
	b.do("POST", "/team/resume", nil, nil)
	if p := b.do("GET", "/", nil, nil).Body.String(); strings.Contains(p, "Team on hold") {
		t.Error("hold banner after resume")
	}
	b.do("POST", "/team/pause", nil, nil)
	if p := b.do("GET", "/", nil, nil).Body.String(); !strings.Contains(p, "Wake-ups are paused") {
		t.Error("no paused banner")
	}
	if got := strings.Join(ctl.calls, ","); got != "hold:nightly,resume,pause" {
		t.Errorf("controller calls = %s", got)
	}
	evs, _ := st.RecentEvents(100)
	var actions []string
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Kind == "web_action" {
			var d map[string]any
			_ = json.Unmarshal([]byte(evs[i].Data), &d)
			actions = append(actions, fmt.Sprint(d["action"], ":", d["target"]))
		}
	}
	if got := strings.Join(actions, ","); got != "hold:team,resume:team,pause:team" {
		t.Errorf("web_action events = %s", got)
	}
	// a name longer than 80 characters is refused with a message and nothing is held
	n := len(ctl.calls)
	if r := b.do("POST", "/team/hold", url.Values{"name": {strings.Repeat("x", 81)}}, nil); r.Code != 422 || !strings.Contains(r.Body.String(), "80 characters") || len(ctl.calls) != n {
		t.Errorf("long name: %d %s", r.Code, r.Body.String())
	}
}

func TestReadOnlyRefusesHoldAndHidesControls(t *testing.T) {
	b, ctl, _ := ctlServer(t, true)
	for _, p := range []string{"/team/hold", "/team/pause", "/team/resume"} {
		if r := b.do("POST", p, nil, nil); r.Code != 403 {
			t.Errorf("%s read-only: %d", p, r.Code)
		}
	}
	if len(ctl.calls) != 0 {
		t.Errorf("controller called: %v", ctl.calls)
	}
	if p := b.do("GET", "/", nil, nil).Body.String(); strings.Contains(p, "Hold the team") {
		t.Error("read-only page offers the hold button")
	}
	// without a supervisor connection the buttons are hidden and a POST says why
	h := harness(t)
	if p := h.Get("/").Body; strings.Contains(p, "Hold the team") {
		t.Error("controls shown with no supervisor connection")
	}
	if r := h.Post("/team/hold", nil); r.Code != 422 || !strings.Contains(r.Body, "no supervisor connection") {
		t.Errorf("hold with no controller: %d %s", r.Code, r.Body)
	}
}

func TestStylesheetServed(t *testing.T) {
	h := harness(t)
	r := h.Get("/dashboard.css")
	if r.Code != 200 || !strings.HasPrefix(r.Header.Get("Content-Type"), "text/css") || !strings.Contains(r.Body, ".chip-ok") {
		t.Fatalf("css: %d %q", r.Code, r.Header.Get("Content-Type"))
	}
	if strings.Contains(r.Body, "http://") || strings.Contains(r.Body, "https://") || strings.Contains(r.Body, "@import") {
		t.Error("stylesheet reaches out to the network")
	}
}

// Forms keep what the user typed when the region refreshes (live update or the 60 s tick).
func TestFormsSurviveRefresh(t *testing.T) {
	b, _, _ := ctlServer(t, false)
	page := b.do("GET", "/f/dashboard", nil, nil).Body.String()
	for _, id := range []string{"first-run-form", "hold-form"} {
		if !strings.Contains(page, `id="`+id+`" hx-preserve="true"`) {
			t.Errorf("form %s is not preserved across swaps", id)
		}
	}
}

// slai.js (the live-region glue) is on every page, once, after htmx.
func TestPageLoadsSlaiJSOnce(t *testing.T) {
	h := harness(t)
	body := h.Get("/").Body
	if n := strings.Count(body, `src="/static/slai.js"`); n != 1 {
		t.Fatalf("slai.js included %d times", n)
	}
	if strings.Index(body, "/static/htmx.min.js") > strings.Index(body, "/static/slai.js") {
		t.Error("slai.js loads before htmx")
	}
}

// ---- T-0115: focus-group round 2 ----

// Item 1: the headline carries health and spend after the status, and says "Nothing needs you".
func TestHeadlineSaysHealthAndSpend(t *testing.T) {
	cases := []struct {
		name string
		s    SentenceState
		want string
	}{
		{"working with a cap", SentenceState{Tasks: 3, Working: 4, Orchestrator: "orch", AgentsTotal: 9, SpendToday: 91.45, DailyCap: 120},
			"The team is working. Nothing needs you. 4 of 9 agents working; $91.45 of $120.00 spent today (76% of the daily cap)."},
		{"idle, no cap", SentenceState{Tasks: 3, Orchestrator: "orch", AgentsTotal: 3, SpendToday: 0},
			"The team is idle. Nothing needs you. 0 of 3 agents working; $0.00 spent today (no daily cap is set)."},
		{"needs you", SentenceState{NeedsYou: []string{"eng-3 waiting on you"}, Working: 2, AgentsTotal: 9, SpendToday: 10, DailyCap: 100},
			"1 thing needs you: eng-3 waiting on you. 2 of 9 agents working; $10.00 of $100.00 spent today (10% of the daily cap)."},
		{"first run has no tail", SentenceState{Orchestrator: "orch", AgentsTotal: 9}, "Nothing has been asked yet. Tell orch what you want built."},
	}
	for _, c := range cases {
		if got := Sentence(c.s); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
	in := baseInput()
	in.Team.Defaults.Caps.DailyUSD = 120
	in.Agents = []store.Agent{agent("eng-1", "working")}
	in.Health["eng-1"] = service.Health{Liveness: "busy"}
	in.Tasks = []store.Task{{ID: "T-1", Status: store.StatusInProgress}}
	in.SpendDay = 60
	if got := Build(in).Sentence; got != "The team is working. Nothing needs you. 1 of 1 agents working; $60.00 of $120.00 spent today (50% of the daily cap)." {
		t.Errorf("built headline: %q", got)
	}
}

// Item 3: one line says which caps protect and which do not.
func TestProtectionLine(t *testing.T) {
	for _, c := range []struct {
		caps config.SpendCaps
		want string
	}{
		{config.SpendCaps{DailyUSD: 120, PerAgentUSD: 40}, "Protected by: daily cap $120, per-agent $40. Not protected: per task, per run."},
		{config.SpendCaps{}, "Protected by: nothing (no spend cap is set). Not protected: daily, per-agent, per task, per run."},
		{config.SpendCaps{DailyUSD: 99.5, PerAgentUSD: 10, PerTaskUSD: 5, PerRunUSD: 200}, "Protected by: daily cap $99.5, per-agent $10, per task $5, per run $200. Not protected: nothing."},
	} {
		if got := protectionLine(c.caps); got != c.want {
			t.Errorf("%+v:\n got %q\nwant %q", c.caps, got, c.want)
		}
	}
	in := baseInput()
	in.Team.Defaults.Caps = config.SpendCaps{DailyUSD: 120, PerAgentUSD: 40}
	if got := Build(in).Spend.Protection; !strings.HasPrefix(got, "Protected by: daily cap $120, per-agent $40.") {
		t.Errorf("view: %q", got)
	}
}

// Item 8 (principal's answer: one row per agent, highest severity, the rest on an "also" line):
// stalled and stale for one agent are two detectors seeing one problem.
func TestNeedsAttentionOneRowPerAgent(t *testing.T) {
	in := baseInput()
	in.Agents = []store.Agent{agent("eng-2", "working"), agent("eng-3", "waiting_on_user"), agent("eng-4", "working")}
	in.Health["eng-2"] = service.Health{Liveness: "stale", Reason: "silent 26m"}
	in.Health["eng-3"] = service.Health{Liveness: "blocked", Reason: "waiting on user"}
	in.Health["eng-4"] = service.Health{Liveness: "rate_limited", Reason: "resets 15:10"}
	in.Questions["eng-3"] = "Which database?"
	in.Esc = []store.Escalation{{Agent: "eng-2", Kind: "stalled", Text: "no progress after 3 reminders", Since: now.Unix() - 960, User: true},
		{Agent: "eng-4", Kind: "rate_limited", Text: "waits for the reset", Since: now.Unix() - 600}}
	v := Build(in)
	seen := map[string]int{}
	for _, n := range v.Needs {
		seen[n.Who]++
	}
	for who, n := range seen {
		if n != 1 {
			t.Errorf("%s has %d rows, want 1: %+v", who, n, v.Needs)
		}
	}
	if len(v.Needs) != 3 {
		t.Fatalf("rows = %+v", v.Needs)
	}
	if v.Needs[0].Who != "eng-3" || v.Needs[0].Label != "waiting on you" || v.Needs[0].Also != "" {
		t.Errorf("first row (waiting on you; its blocked chip is the same thing): %+v", v.Needs[0])
	}
	if !strings.HasPrefix(v.Needs[1].Gloss, "An alarm: slai has already reminded") {
		t.Errorf("the stalled row explains itself as %q", v.Needs[1].Gloss)
	}
	if v.Needs[1].Who != "eng-2" || v.Needs[1].Label != "stalled" || v.Needs[1].Also != "stale" || !v.Needs[1].ActionYou {
		t.Errorf("eng-2 row (stalled beats stale, keeps the user action): %+v", v.Needs[1])
	}
	if v.Needs[2].Who != "eng-4" || v.Needs[2].Label != "rate limited" || v.Needs[2].ActionYou {
		t.Errorf("eng-4 row: %+v", v.Needs[2])
	}
	if v.NeedsYou != 2 {
		t.Errorf("NeedsYou = %d, want 2 rows", v.NeedsYou)
	}
}

// Item 2: the orphan banner says what the command touches (the principal's wording), and
// drops to a quiet style when nothing needs the user.
func TestOrphanBannerSaysWhatItTouches(t *testing.T) {
	in := baseInput()
	in.Orphans = []string{"slai_ab12_"}
	var b Banner
	for _, x := range Build(in).Banners {
		if x.Kind == "orphans" {
			b = x
		}
	}
	if b.Class != "note" || !strings.Contains(b.Text, "Stops only that team's leftover terminal sessions and their processes; never your branches, worktrees or files.") {
		t.Errorf("quiet banner: %+v", b)
	}
	in.Agents = []store.Agent{agent("eng-3", "waiting_on_user")}
	for _, x := range Build(in).Banners {
		if x.Kind == "orphans" && x.Class != "info" {
			t.Errorf("with something needing the user the banner is %q, want info", x.Class)
		}
	}
}

// Item 7: the panel explains orphaned, stale vs stalled vs stuck, n/a and hold vs pause.
func TestGlossaryPanelCoversRound2Words(t *testing.T) {
	h := harness(t, webtest.Setup(func(st *store.Store, _ *config.Team) { _ = st.SetSupervisorHeartbeat(1_800_000_000) }))
	body := h.Get("/").Body
	for _, w := range []string{"orphaned", "stale", "stalled", "stuck", "n/a", "hold", "pause"} {
		if Glossary[w] == "" {
			t.Errorf("no glossary entry for %q", w)
		}
		if !strings.Contains(body, "<dt>"+w+"</dt>") {
			t.Errorf("the glossary panel lacks %q", w)
		}
	}
	for _, want := range []string{"A state, not an alarm", "An alarm: slai has already reminded", "No data, not zero", "unsaved buffers are lost"} {
		found := false
		for _, g := range Glossary {
			found = found || strings.Contains(g, want)
		}
		if !found {
			t.Errorf("glossary lacks %q", want)
		}
	}
	// the weekly n/a reason is inline, next to the gauge
	if !strings.Contains(body, "Weekly window") || !strings.Contains(body, "no data yet") {
		t.Error("the weekly n/a reason is not shown inline")
	}
	if !strings.Contains(body, `id="spend-protection"`) || !strings.Contains(body, "Protected by:") {
		t.Error("the caps panel has no protection line")
	}
}

// C8: each agent card says when the agent was last re-grounded and shows an open identity correction.
func TestAgentCardsShowIdentityGrounding(t *testing.T) {
	in := baseInput()
	in.Agents = []store.Agent{agent("eng-1", "working"), agent("eng-2", "working")}
	in.Health["eng-1"] = service.Health{Liveness: "busy", LastActivity: now.Unix() - 30}
	in.Health["eng-2"] = service.Health{Liveness: "busy", LastActivity: now.Unix() - 30}
	in.Identity = map[string]agentrows.Identity{"eng-1": {GroundAt: now.Unix() - 180, Source: "user", Correction: "sent a message signed as orch", CorrectionAt: now.Unix() - 10}}
	cards := map[string]AgentCard{}
	for _, c := range Build(in).Active {
		cards[c.ID] = c
	}
	if c := cards["eng-1"]; c.Grounded != "re-grounded 3m ago" || c.Correction != "sent a message signed as orch" {
		t.Errorf("eng-1 card = %+v", c)
	}
	if c := cards["eng-2"]; c.Grounded != "never re-grounded" || c.Correction != "" {
		t.Errorf("eng-2 card = %+v", c)
	}
}
