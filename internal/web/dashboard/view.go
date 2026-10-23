package dashboard

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/agentrows"
	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/ratelimit"
	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

// Input is everything the dashboard shows, read once per render. Build turns it into a View
// without touching the store, so every rule is testable with a literal.
type Input struct {
	Now    time.Time
	Team   *config.Team
	Agents []store.Agent
	Health map[string]service.Health
	// Identity is each agent's identity grounding (agentrows.IdentityOf); a missing entry reads as never re-grounded.
	Identity map[string]agentrows.Identity
	Tasks    []store.Task
	Esc      []store.Escalation
	// Questions maps an agent waiting on the user to its latest ask_user text.
	Questions map[string]string
	Heartbeat int64 // supervisor heartbeat (unix), 0 = never
	Build     service.BuildState
	Gate      service.WakeGate
	SpendDay  float64
	SpendBy   map[string]float64
	Snap      *ratelimit.Snapshot
	Orphans   []string // prefixes of orphaned teams
	ReadOnly  bool
}

// View is the rendered model.
type View struct {
	Sentence string
	Banners  []Banner
	Cards    []Card
	Needs    []Need
	Active   []AgentCard // everything that is not a healthy idle agent
	Idle     []AgentCard // healthy idle agents, collapsed into one line
	Spend    Spend
	Gauges   []Gauge
	Hold     *HoldView
	FirstRun *FirstRun
	// NeedsYou counts the rows that need a person; the "N need you" chip links to Inbox.
	NeedsYou int
	Glossary []GlossRow
}

// Banner is a full-width notice. Kind is one of: supervisor, build, hold, paused, orphans.
type Banner struct {
	Kind, Class, Title, Text string
	Resume                   bool // offer the resume button
}

// Card is one metric.
type Card struct{ Label, Value, Sub, Href string }

// Need is a row of Needs attention.
type Need struct {
	Who, Label, Text, Since, Gloss, Class string
	ActionYou                             bool // a person has to act (counted in NeedsYou)
	Href                                  string
	Also                                  string // the other reasons for the same agent, merged into this row
}

// Chip is a state marker: an icon and a word, never colour alone.
type Chip struct{ Class, Icon, Label, Reason, Gloss string }

// AgentCard is one agent on the dashboard.
type AgentCard struct {
	ID, Role, Harness, Task, Status, Last string
	Chip                                  Chip
	Grounded                              string // "re-grounded 3m ago" / "never re-grounded"
	Correction                            string // the open identity correction, "" when none
}

// Spend is the spend card.
type Spend struct {
	Window string
	Rows   []SpendRow
	// Protection is one line saying which caps guard the spend and which do not, built from
	// the configured caps ("Protected by: daily cap $120, per-agent $40. Not protected: ...").
	Protection string
}

// SpendRow is one cap. OFF is true when the cap is not set; State is ok|warn|bad|off and is
// also spelled out in Text.
type SpendRow struct {
	Label, Spent, Cap, State, Text, Icon string
	OFF                                  bool
	Percent                              int
	HasMeter                             bool
}

// Gauge is a rate-limit window. NA gauges are neutral and say why.
type Gauge struct {
	Label, Text, Reason, Class string
	NA                         bool
	Percent                    int
}

// HoldView is the team hold, when one is in force.
type HoldView struct{ Name, Since string }

// FirstRun is the hint shown when there are no tasks yet.
type FirstRun struct{ Orchestrator string }

// GlossRow is one line of the glossary footer.
type GlossRow struct{ Word, Text string }

// Build computes the view.
func Build(in Input) View {
	v := View{}
	byID := map[string]store.Agent{}
	for _, a := range in.Agents {
		byID[a.ID] = a
	}
	team := in.Team
	if team == nil {
		team = &config.Team{}
	}
	cfg := map[string]config.Agent{}
	orch := ""
	for _, a := range team.Agents {
		cfg[a.ID] = a
		if a.Role == config.RoleOrchestrator && orch == "" {
			orch = a.ID
		}
	}

	agents := append([]store.Agent(nil), in.Agents...)
	sort.SliceStable(agents, func(i, j int) bool { return agents[i].ID < agents[j].ID })

	counts := map[string]int{}
	working := 0
	for _, a := range agents {
		h := in.Health[a.ID]
		live := h.Liveness
		if live == "" {
			live = "idle"
		}
		counts[live]++
		card := AgentCard{ID: a.ID, Role: a.Role, Task: a.CurrentTask, Status: a.Status, Chip: chipOf(live, h.Reason)}
		id := in.Identity[a.ID]
		card.Grounded, card.Correction = id.GroundText(in.Now), id.Correction
		if c, ok := cfg[a.ID]; ok {
			card.Harness = c.HarnessKind() + " · " + c.Model
		}
		if h.LastActivity > 0 {
			card.Last = Ago(in.Now, h.LastActivity)
		} else {
			card.Last = "no activity yet"
		}
		if live == "idle" {
			v.Idle = append(v.Idle, card)
		} else {
			v.Active = append(v.Active, card)
		}
		if live == "alive" || live == "busy" {
			working++
		}
	}

	// ---- needs attention ----
	seen := map[string]bool{}
	addNeed := func(n Need) {
		key := n.Who + "|" + n.Label
		if seen[key] {
			return
		}
		seen[key] = true
		if n.Href == "" {
			n.Href = "/inbox"
		}
		v.Needs = append(v.Needs, n)
		if n.ActionYou {
			v.NeedsYou++
		}
	}
	waiting := map[string]bool{} // agents already listed as waiting on the user: not also "blocked"
	for _, a := range agents {
		if a.Status != "waiting_on_user" {
			continue
		}
		waiting[a.ID] = true
		text := in.Questions[a.ID]
		if text == "" {
			text = "is waiting for your answer"
		}
		addNeed(Need{Who: a.ID, Label: "waiting on you", Text: text, Since: Ago(in.Now, a.StatusAt), Class: "bad", ActionYou: true})
	}
	for _, e := range in.Esc {
		n := Need{Who: e.Agent, Label: escLabel(e.Kind), Text: e.Text, Since: Ago(in.Now, e.Since), Class: "warn", ActionYou: e.User,
			Gloss: escGloss(e.Kind)}
		if n.Who == "" {
			n.Who = "supervisor"
		}
		addNeed(n)
	}
	for _, a := range agents {
		h := in.Health[a.ID]
		switch h.Liveness {
		case "blocked":
			if waiting[a.ID] {
				continue
			}
			addNeed(Need{Who: a.ID, Label: "blocked", Text: h.Reason, Gloss: gloss("blocked"), Class: "bad", ActionYou: true})
		case "stale":
			addNeed(Need{Who: a.ID, Label: "stale", Text: h.Reason, Gloss: gloss("stale"), Class: "warn"})
		case "dead":
			if a.Status == "dead" {
				addNeed(Need{Who: a.ID, Label: "dead", Text: h.Reason, Gloss: gloss("dead"), Class: "bad"})
			}
		case "rate_limited":
			addNeed(Need{Who: a.ID, Label: "rate limited", Text: h.Reason, Gloss: gloss("rate limit"), Class: "warn"})
		case "gated":
			addNeed(Need{Who: a.ID, Label: "gated", Text: h.Reason, Gloss: gloss("gated"), Class: "info"})
		}
	}

	v.Needs, v.NeedsYou = mergeNeeds(v.Needs)

	// ---- banners ----
	if in.Heartbeat == 0 || in.Now.Sub(time.Unix(in.Heartbeat, 0)) > service.SupervisorDownAfter {
		t := "The supervisor is not running, so agents are not being woken or revived. Run `slai up`. You can still read the team and queue messages."
		if in.Heartbeat != 0 {
			t = "The supervisor stopped " + Ago(in.Now, in.Heartbeat) + ", so agents are not being woken or revived. Run `slai up`."
		}
		v.Banners = append(v.Banners, Banner{Kind: "supervisor", Class: "bad", Title: "Supervisor down", Text: t})
	}
	if in.Build.SupervisorOld() {
		v.Banners = append(v.Banners, Banner{Kind: "build", Class: "bad", Title: "Running an old build",
			Text: fmt.Sprintf("The supervisor was started from build %s; this server is build %s. New fixes are not active until you run `slai up --restart`.", short(in.Build.Supervisor), short(in.Build.Current))})
	}
	if in.Gate.TeamHold {
		name := in.Gate.HoldName
		hv := &HoldView{Name: name, Since: Ago(in.Now, in.Gate.HoldSince)}
		v.Hold = hv
		t := "Nothing is woken, nudged or restarted until you resume. Sessions are still up."
		title := "Team on hold"
		if name != "" {
			title += ": " + name
		}
		v.Banners = append(v.Banners, Banner{Kind: "hold", Class: "hold", Title: title, Text: t + " Held " + hv.Since + ".", Resume: !in.ReadOnly})
	} else if in.Gate.Paused {
		v.Banners = append(v.Banners, Banner{Kind: "paused", Class: "hold", Title: "Wake-ups are paused",
			Text: "Agents are not being woken until you resume.", Resume: !in.ReadOnly})
	}
	if n := len(in.Orphans); n > 0 {
		class := "info"
		if v.NeedsYou == 0 {
			class = "note" // nothing needs you: a tidy-up reminder, not a warning
		}
		v.Banners = append(v.Banners, Banner{Kind: "orphans", Class: class, Title: "Orphaned slai teams",
			Text: fmt.Sprintf("%d orphaned slai %s (%s) still running with no supervisor. Run `slai down --orphans` to clean up. %s", n, plural(n, "team", "teams"), strings.Join(in.Orphans, ", "), OrphanClaim)})
	}

	// ---- cards ----
	open, review := 0, 0
	for _, t := range in.Tasks {
		switch t.Status {
		case store.StatusMerged, store.StatusCancelled, store.StatusDone, store.StatusReverted:
		default:
			open++
		}
		if t.Status == store.StatusInReview {
			review++
		}
	}
	idleN := counts["idle"]
	v.Cards = []Card{
		{Label: "Agents", Value: fmt.Sprint(len(agents)), Sub: fmt.Sprintf("%d working, %d idle", working, idleN), Href: "/agents"},
		{Label: "Open tasks", Value: fmt.Sprint(open), Sub: fmt.Sprintf("%d total", len(in.Tasks)), Href: "/board"},
		{Label: "In review", Value: fmt.Sprint(review), Sub: "waiting on the principal", Href: "/reviews"},
		{Label: "Need you", Value: fmt.Sprint(v.NeedsYou), Sub: "answer in Inbox", Href: "/inbox"},
	}

	v.Spend = spendView(in, team)
	v.Gauges = gaugesOf(in)
	if len(in.Tasks) == 0 && orch != "" {
		v.FirstRun = &FirstRun{Orchestrator: orch}
	}
	var reasons []string
	for _, n := range v.Needs {
		if n.ActionYou {
			reasons = append(reasons, n.Who+" "+n.Label)
		}
	}
	v.Sentence = Sentence(SentenceState{Hold: v.Hold, HoldName: in.Gate.HoldName, SupervisorDown: len(v.Banners) > 0 && v.Banners[0].Kind == "supervisor",
		Paused: in.Gate.Paused, NeedsYou: reasons, Tasks: len(in.Tasks), Working: working, Orchestrator: orch,
		AgentsTotal: len(agents), SpendToday: in.SpendDay, DailyCap: team.Defaults.Caps.DailyUSD})
	words := append([]string(nil), glossaryWords...)
	sort.Strings(words)
	for _, w := range words {
		v.Glossary = append(v.Glossary, GlossRow{Word: w, Text: Glossary[w]})
	}
	return v
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

var chipTable = map[string]struct{ class, icon, label string }{
	"alive":        {"chip-ok", "●", "alive"},
	"busy":         {"chip-ok", "◐", "busy"},
	"idle":         {"chip-idle", "○", "idle"},
	"blocked":      {"chip-bad", "▲", "blocked"},
	"stale":        {"chip-warn", "▲", "stale"},
	"dead":         {"chip-bad", "✕", "dead"},
	"rate_limited": {"chip-warn", "◑", "rate limited"},
	"gated":        {"chip-info", "⏸", "gated"},
}

func chipOf(live, reason string) Chip {
	t, ok := chipTable[live]
	if !ok {
		return Chip{Class: "chip-neutral", Icon: "?", Label: live, Reason: reason}
	}
	return Chip{Class: t.class, Icon: t.icon, Label: t.label, Reason: reason, Gloss: gloss(live)}
}

// Ago renders a unix time relative to now: "just now", "5m ago", "3h ago", "2d ago".
func Ago(now time.Time, unix int64) string {
	if unix <= 0 {
		return "never"
	}
	d := now.Sub(time.Unix(unix, 0))
	switch {
	case d < 0:
		return "just now"
	case d < 45*time.Second:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", max(int(d.Minutes()), 1))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}

// In renders a future unix time: "in 38m".
func In(now time.Time, unix int64) string {
	d := time.Unix(unix, 0).Sub(now)
	switch {
	case d <= 0:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("in %dm", max(int(d.Minutes()), 1))
	case d < 48*time.Hour:
		return fmt.Sprintf("in %dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("in %dd", int(d.Hours()/24))
}

func escLabel(kind string) string {
	switch kind {
	case "rate_limited":
		return "rate limited"
	case "rate_limited_all":
		return "all agents rate limited"
	case "modal_stuck":
		return "stuck on a prompt"
	case "revive_gave_up":
		return "revive gave up"
	case "orch_down":
		return "orchestrator down"
	case "supervisor_down":
		return "supervisor down"
	case "unread_stuck":
		return "messages unread"
	case "stuck_gave_up":
		return "stuck, restart failed"
	case "stalled":
		return "stalled"
	}
	return strings.ReplaceAll(kind, "_", " ")
}

func escGloss(kind string) string {
	switch kind {
	case "rate_limited", "rate_limited_all":
		return gloss("rate limit")
	case "stalled":
		return gloss("stalled")
	case "modal_stuck", "stuck_gave_up", "unread_stuck":
		return gloss("stuck")
	case "revive_gave_up", "orch_down":
		return gloss("dead")
	}
	return ""
}

func spendView(in Input, team *config.Team) Spend {
	caps := team.Defaults.Caps
	top, topAgent := 0.0, ""
	for id, s := range in.SpendBy {
		if s > top || (s == top && id < topAgent) {
			top, topAgent = s, id
		}
	}
	rows := []SpendRow{
		meter("Daily cap", in.SpendDay, caps.DailyUSD, "all agents today"),
		meter("Per-agent cap", top, caps.PerAgentUSD, "highest agent today"+ifs(topAgent != "", ": "+topAgent)),
		{Label: "Per-task cap", Cap: capText(caps.PerTaskUSD), OFF: caps.PerTaskUSD <= 0, State: stateOf(caps.PerTaskUSD <= 0), Text: pick(caps.PerTaskUSD <= 0, "OFF: no per-task cap is set", "limit per task; see Stats for each task's spend")},
		{Label: "Per-run cap", Cap: capText(caps.PerRunUSD), OFF: caps.PerRunUSD <= 0, State: stateOf(caps.PerRunUSD <= 0), Text: pick(caps.PerRunUSD <= 0, "OFF: no per-run cap is set", "limit since the last finish or `slai up`")},
	}
	return Spend{Window: "today since 00:00 local, from recorded per-agent cost", Rows: rows, Protection: protectionLine(caps)}
}

// protectionLine says in one sentence which spend caps are set and which are not, so a cap
// that reads OFF next to one that is on is not mistaken for "unprotected" or "protected".
func protectionLine(c config.SpendCaps) string {
	type cap struct {
		name  string
		value float64
	}
	var on, off []string
	for _, k := range []cap{{"daily cap", c.DailyUSD}, {"per-agent", c.PerAgentUSD}, {"per task", c.PerTaskUSD}, {"per run", c.PerRunUSD}} {
		if k.value > 0 {
			on = append(on, fmt.Sprintf("%s $%s", k.name, trimUSD(k.value)))
		} else {
			off = append(off, strings.TrimSuffix(k.name, " cap"))
		}
	}
	protected, not := "nothing (no spend cap is set)", "nothing"
	if len(on) > 0 {
		protected = strings.Join(on, ", ")
	}
	if len(off) > 0 {
		not = strings.Join(off, ", ")
	}
	return "Protected by: " + protected + ". Not protected: " + not + "."
}

func trimUSD(v float64) string {
	s := fmt.Sprintf("%.2f", v)
	return strings.TrimSuffix(strings.TrimSuffix(s, "0"), ".0")
}

func ifs(c bool, s string) string {
	if c {
		return s
	}
	return ""
}

func pick(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

func stateOf(off bool) string {
	if off {
		return "off"
	}
	return "ok"
}

func capText(c float64) string {
	if c <= 0 {
		return "OFF"
	}
	return fmt.Sprintf("$%.2f", c)
}

// meter is a cap row with spend against it. No cap shows OFF, never a bar; 80% is a warning
// and 100% is the cap, each spelled out so colour is not the only signal.
func meter(label string, spent, limit float64, what string) SpendRow {
	r := SpendRow{Label: label, Spent: fmt.Sprintf("$%.2f", spent), Cap: capText(limit)}
	if limit <= 0 {
		r.OFF, r.State, r.Text = true, "off", "OFF: no cap is set ("+what+")"
		return r
	}
	pct := spent / limit * 100
	r.HasMeter, r.Percent = true, min(int(pct), 100)
	switch {
	case pct >= 100:
		r.State, r.Icon, r.Text = "bad", "✕", fmt.Sprintf("cap reached (%.0f%% of the cap): wake-ups stop", pct)
	case pct >= 80:
		r.State, r.Icon, r.Text = "warn", "▲", fmt.Sprintf("%.0f%% of the cap: over the warning line", pct)
	default:
		r.State, r.Icon, r.Text = "ok", "●", fmt.Sprintf("%.0f%% of the cap (%s)", pct, what)
	}
	return r
}

func gaugesOf(in Input) []Gauge {
	mk := func(label string, w *ratelimit.Window) Gauge {
		if w == nil {
			return Gauge{Label: label, NA: true, Class: "chip-neutral", Text: "n/a",
				Reason: "no data yet: the agent's harness has not reported this limit"}
		}
		g := Gauge{Label: label, Percent: min(int(w.UsedPercent), 100), Text: fmt.Sprintf("%.0f%%", w.UsedPercent)}
		switch {
		case w.UsedPercent >= 100:
			g.Class = "chip-bad"
		case w.UsedPercent >= 80:
			g.Class = "chip-warn"
		default:
			g.Class = "chip-ok"
		}
		if w.ResetsAt > 0 {
			g.Reason = fmt.Sprintf("resets %s (%s)", time.Unix(w.ResetsAt, 0).In(in.Now.Location()).Format("15:04"), In(in.Now, w.ResetsAt))
		}
		return g
	}
	var five, seven *ratelimit.Window
	if in.Snap != nil {
		five, seven = in.Snap.FiveHour, in.Snap.SevenDay
	}
	return []Gauge{mk("5-hour window", five), mk("Weekly window", seven)}
}

// ---- the status sentence ----

// SentenceState is what the status sentence is chosen from.
type SentenceState struct {
	Hold           *HoldView
	HoldName       string
	SupervisorDown bool
	Paused         bool
	NeedsYou       []string // one short reason per thing that needs a person
	Tasks          int
	Working        int
	Orchestrator   string
	AgentsTotal    int     // all agents on the team
	SpendToday     float64 // recorded spend today
	DailyCap       float64 // 0: no daily cap
}

type sentenceRule struct {
	Name string
	When func(SentenceState) bool
	Say  func(SentenceState) string
}

// sentenceRules is the fixed rule table: the first rule that matches speaks. Order is
// priority.
var sentenceRules = []sentenceRule{
	{"hold", func(s SentenceState) bool { return s.Hold != nil }, func(s SentenceState) string {
		if s.HoldName != "" {
			return "Team is on hold (" + s.HoldName + "). Nothing runs until you resume."
		}
		return "Team is on hold. Nothing runs until you resume."
	}},
	{"supervisor", func(s SentenceState) bool { return s.SupervisorDown }, func(SentenceState) string {
		return "The supervisor is not running, so nobody is being woken. Start it with `slai up`."
	}},
	{"needs-you", func(s SentenceState) bool { return len(s.NeedsYou) > 0 }, func(s SentenceState) string {
		n := len(s.NeedsYou)
		shown := s.NeedsYou
		if len(shown) > 3 {
			shown = shown[:3]
		}
		more := ""
		if n > len(shown) {
			more = fmt.Sprintf(", and %d more", n-len(shown))
		}
		if n == 1 {
			return "1 thing needs you: " + shown[0] + "."
		}
		return fmt.Sprintf("%d things need you: %s%s.", n, strings.Join(shown, "; "), more)
	}},
	{"paused", func(s SentenceState) bool { return s.Paused }, func(SentenceState) string {
		return "Wake-ups are paused. Resume to let the team continue."
	}},
	{"first-run", func(s SentenceState) bool { return s.Tasks == 0 && s.Orchestrator != "" }, func(s SentenceState) string {
		return "Nothing has been asked yet. Tell " + s.Orchestrator + " what you want built."
	}},
	{"working", func(s SentenceState) bool { return s.Working > 0 }, func(SentenceState) string {
		return "The team is working. Nothing needs you."
	}},
	{"idle", func(SentenceState) bool { return true }, func(SentenceState) string {
		return "The team is idle. Nothing needs you."
	}},
}

// Sentence picks the plain-English headline: the first matching rule says what the team is
// doing and whether anything needs the user, and a fixed tail adds health and spend, so the
// first line of the page answers "is it healthy, what is it costing, do I have to do anything".
func Sentence(s SentenceState) string {
	for _, r := range sentenceRules {
		if r.When(s) {
			out := r.Say(s)
			if t := headlineTail(s); t != "" && r.Name != "first-run" {
				out += " " + t
			}
			return out
		}
	}
	return ""
}

// headlineTail is "4 of 9 agents are working; $91.45 of $120.00 spent today (76% of the daily cap)."
func headlineTail(s SentenceState) string {
	if s.AgentsTotal == 0 {
		return ""
	}
	t := fmt.Sprintf("%d of %d agents working; ", s.Working, s.AgentsTotal)
	if s.DailyCap > 0 {
		return t + fmt.Sprintf("$%.2f of $%.2f spent today (%.0f%% of the daily cap).", s.SpendToday, s.DailyCap, s.SpendToday/s.DailyCap*100)
	}
	return t + fmt.Sprintf("$%.2f spent today (no daily cap is set).", s.SpendToday)
}

// OrphanClaim says what `slai down --orphans` touches. Confirmed by the principal against
// internal/orphans.Finder.Kill (tmux sessions, then processes tagged with that team's project;
// no git, no worktree, no file writes): use it verbatim.
const OrphanClaim = "Stops only that team's leftover terminal sessions and their processes; never your branches, worktrees or files."

var severityOrder = map[string]int{"waiting on you": 0, "blocked": 1, "dead": 2, "revive gave up": 2, "orchestrator down": 2, "supervisor down": 2,
	"stalled": 3, "stuck on a prompt": 3, "stuck, restart failed": 3, "messages unread": 3, "all agents rate limited": 4, "stale": 5,
	"rate limited": 6, "gated": 7}

func severity(label string) int {
	if n, ok := severityOrder[label]; ok {
		return n
	}
	return 5
}

func joinAlso(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ", ")
}

// mergeNeeds keeps one row per agent: the highest-severity reason, with the others named in
// its Also line. Rows come out in severity order (then agent), and the second result counts
// the rows a person has to act on.
func mergeNeeds(in []Need) ([]Need, int) {
	best := map[string]int{} // agent -> index in out
	var out []Need
	for _, n := range in {
		i, seen := best[n.Who]
		if !seen {
			best[n.Who] = len(out)
			out = append(out, n)
			continue
		}
		cur := out[i]
		keep, other := cur, n
		if severity(n.Label) < severity(cur.Label) {
			keep, other = n, cur
		}
		keep.ActionYou = cur.ActionYou || n.ActionYou
		keep.Also = joinAlso(keep.Also, other.Label, other.Also)
		out[i] = keep
	}
	sort.SliceStable(out, func(i, j int) bool { return severity(out[i].Label) < severity(out[j].Label) })
	n := 0
	for i := range out {
		if out[i].ActionYou {
			n++
		}
	}
	return out, n
}

// GaugeLine is the plan limits as one line, for pages that show them once instead of per
// row: "5-hour window 41% (resets 15:10 (in 40m)) · Weekly window n/a (no data yet: ...)".
func GaugeLine(now time.Time, snap *ratelimit.Snapshot) string {
	var parts []string
	for _, g := range gaugesOf(Input{Now: now, Snap: snap}) {
		t := g.Text
		if g.Reason != "" {
			t += " (" + g.Reason + ")"
		}
		parts = append(parts, g.Label+" "+t)
	}
	return strings.Join(parts, " · ")
}
