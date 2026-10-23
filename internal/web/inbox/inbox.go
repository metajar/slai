// Package inbox is the Inbox page of `slai serve`: the questions agents asked the user
// (ask_user), the escalations that need the user, supervisor notices, and the asks still
// waiting on another agent. The user answers a question here exactly as `slai send` would:
// the answer is a user message to the asking agent.
package inbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/templates"
)

// noticeWindow is how long a supervisor-raised ask_user (no asking agent: spend cap,
// message loop) counts as needing the user, the same window the TUI banner uses.
const noticeWindow = 15 * time.Minute

// waitingWindow bounds the "waiting on the team" list to asks from the last day.
const waitingWindow = 24 * time.Hour

const (
	kvDismissed = "web.dismissed." // + kind.agent: the escalation's Since it was dismissed at
)

type answeredRec = service.AnswerRec

// Module registers the inbox page and its writes, and the sidebar count.
func Module(d *server.Deps) []server.Route {
	m := &mod{d: d}
	d.SetBadge("inbox", func() (int, string) {
		v := m.load()
		return v.Count, v.Hint
	})
	return []server.Route{
		{Method: http.MethodGet, Pattern: "/inbox", Calls: "reads open ask_user questions, escalations and unanswered asks from the store", Handler: m.page},
		{Method: http.MethodPost, Pattern: "/inbox/{id}/answer", Action: "answer",
			Calls: "the supervisor's SendUser, the same call as `slai send`, then marks the question answered in the web UI", Handler: m.answer},
		{Method: http.MethodPost, Pattern: "/inbox/escalations/{kind}/{agent}/dismiss", Action: "dismiss_escalation", Target: escTarget,
			Calls: "hides the escalation in this UI only (a web.dismissed.* key); the escalation stays open in slai", Handler: m.dismiss},
		{Method: http.MethodPost, Pattern: "/inbox/escalations/{kind}/{agent}/restore", Action: "restore_escalation", Target: escTarget,
			Calls: "removes the UI-only dismissal; changes nothing else", Handler: m.restore},
	}
}

func escTarget(r *http.Request) string { return r.PathValue("kind") + "/" + r.PathValue("agent") }

type mod struct{ d *server.Deps }

type question struct {
	EventID    int64
	Agent      string
	Text       string
	Reason     string
	At         time.Time
	When       string // "11:31 (4m ago)"
	Answered   bool
	Answer     string
	AnswerAt   time.Time
	AnswerWhen string
}

type notice struct {
	Text string
	When string
}

type escalation struct {
	Agent, Kind, Text, Chip, Next string
	Since                         time.Time
	When                          string
	Dismissed                     bool
}

type waiting struct {
	From, To, Body, When string
}

// view is everything the page (and the sidebar badge) shows.
type view struct {
	Questions  []question // open, oldest first
	Answered   []question // answered here in the last day, newest first
	Notices    []notice
	Escalated  []escalation // active and not dismissed
	Dismissed  []escalation // dismissed here but still open in slai
	Waiting    []waiting
	Count      int
	Hint       string
	ReadOnly   bool
	Orchestrat string
}

func (m *mod) load() view {
	var v view
	now := m.d.Now()
	agents, _ := m.d.Store.GetAgents()
	byID := map[string]store.Agent{}
	for _, a := range agents {
		byID[a.ID] = a
	}
	// questions: each waiting agent's newest ask_user
	for _, a := range agents {
		if a.Status != "waiting_on_user" {
			continue
		}
		evs, err := m.d.Store.EventsByKind(a.ID, "ask_user", 1)
		if err != nil || len(evs) == 0 {
			continue
		}
		q := questionOf(now, evs[0])
		if rec, ok := m.answeredRec(evs[0].ID); ok {
			q.markAnswered(now, rec)
			v.Answered = append(v.Answered, q)
			continue
		}
		v.Questions = append(v.Questions, q)
	}
	sort.SliceStable(v.Questions, func(i, j int) bool { return v.Questions[i].At.Before(v.Questions[j].At) })
	// answered questions of agents that have since moved on stay visible for a day
	if evs, err := m.d.Store.RecentEvents(500); err == nil {
		seen := map[int64]bool{}
		for _, q := range v.Answered {
			seen[q.EventID] = true
		}
		for _, e := range evs {
			if e.Kind != "ask_user" || e.Agent == "" || seen[e.ID] || now.Sub(time.Unix(e.At, 0)) > 24*time.Hour {
				continue
			}
			if rec, ok := m.answeredRec(e.ID); ok {
				q := questionOf(now, e)
				q.markAnswered(now, rec)
				v.Answered = append(v.Answered, q)
			}
		}
		// supervisor notices: ask_user without an asking agent, fresh
		for _, e := range evs {
			if e.Kind != "ask_user" || e.Agent != "" || now.Sub(time.Unix(e.At, 0)) > noticeWindow {
				continue
			}
			q := questionOf(now, e)
			text := q.Text
			if q.Reason != "" && q.Reason != text {
				text = q.Reason + ": " + text
			}
			v.Notices = append(v.Notices, notice{Text: text, When: q.When})
		}
	}
	sort.SliceStable(v.Answered, func(i, j int) bool { return v.Answered[i].AnswerAt.After(v.Answered[j].AnswerAt) })
	// escalations the user was told about
	escs, _ := m.d.Store.OpenEscalations()
	for _, e := range escs {
		if !e.User {
			continue
		}
		row := escalation{Agent: e.Agent, Kind: e.Kind, Text: e.Text, Since: time.Unix(e.Since, 0), When: templates.Stamp(now, time.Unix(e.Since, 0)), Chip: chipOf(e.Kind), Next: nextOf(e.Kind)}
		if val, ok, _ := m.d.Store.GetKV(kvDismissed + e.Kind + "." + e.Agent); ok && val == strconv.FormatInt(e.Since, 10) {
			row.Dismissed = true
			v.Dismissed = append(v.Dismissed, row)
			continue
		}
		v.Escalated = append(v.Escalated, row)
	}
	// asks waiting on a teammate
	if asks, err := m.d.Store.OpenAsks(now.Add(-waitingWindow).Unix()); err == nil {
		for _, a := range asks {
			v.Waiting = append(v.Waiting, waiting{From: a.From, To: a.To, Body: a.Body, When: templates.Stamp(now, time.Unix(a.CreatedAt, 0))})
		}
	}
	v.Count = len(v.Questions) + len(v.Notices) + len(v.Escalated)
	v.Hint = fmt.Sprintf("%d need you: %d question(s), %d supervisor notice(s), %d escalation(s)", v.Count, len(v.Questions), len(v.Notices), len(v.Escalated))
	if n := len(v.Dismissed); n > 0 {
		v.Hint += fmt.Sprintf("; %d dismissed escalation(s) are still active in slai", n)
	}
	v.ReadOnly = m.d.ReadOnly
	if m.d.Team != nil {
		if t := m.d.Team(); t != nil {
			for _, a := range t.Agents {
				if a.Role == "orchestrator" {
					v.Orchestrat = a.ID
					break
				}
			}
		}
	}
	return v
}

func (q *question) markAnswered(now time.Time, rec answeredRec) {
	q.Answered, q.Answer, q.AnswerAt = true, rec.Answer, time.Unix(rec.At, 0)
	q.AnswerWhen = templates.Stamp(now, q.AnswerAt)
}

func questionOf(now time.Time, e store.Event) question {
	var d struct {
		Question string `json:"question"`
		Reason   string `json:"reason"`
	}
	_ = json.Unmarshal([]byte(e.Data), &d)
	if d.Question == "" {
		d.Question = d.Reason
		d.Reason = ""
	}
	at := time.Unix(e.At, 0)
	return question{EventID: e.ID, Agent: e.Agent, Text: d.Question, Reason: d.Reason, At: at, When: templates.Stamp(now, at)}
}

func (m *mod) answeredRec(eventID int64) (answeredRec, bool) {
	return service.AskAnswer(m.d.Store, eventID)
}

// chipOf picks the chip colour for an escalation kind: a rate limit lifts by itself, the
// rest need someone to act.
func chipOf(kind string) string {
	if strings.HasPrefix(kind, "rate_limited") {
		return "chip-warn"
	}
	return "chip-bad"
}

var next = map[string]string{
	"modal_stuck":    "A permission dialog is open and the supervisor will not type over it. Open the agent in the TUI and answer it once.",
	"rate_limited":   "Wakes are held until the limit resets. Nothing is lost.",
	"revive_gave_up": "The session is gone and revive gave up. Press v on it in the TUI, or run slai up.",
	"orch_down":      "The orchestrator is not running. Run slai up.",
	"stalled":        "The orchestrator did not act on a stalled agent. Message it, or reassign the task.",
}

func nextOf(kind string) string {
	if n, ok := next[kind]; ok {
		return n
	}
	return "Run slai doctor for details."
}

func (m *mod) page(w http.ResponseWriter, r *http.Request) {
	tpl, err := m.d.Template("inbox")
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.d.Render(w, r, tpl, templates.Page{Title: "Inbox", Active: "inbox", Data: m.load()})
}

func (m *mod) answer(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		m.d.Fail(w, r, err)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		m.d.Fail(w, r, errors.New("unknown question"))
		return
	}
	if m.d.Actions == nil {
		m.d.Fail(w, r, errors.New("this server has no supervisor connection, so it cannot send the answer"))
		return
	}
	// One code path with `slai reply`: service.AnswerAsk checks the ask is open, sends and records it.
	agent, err := service.AnswerAsk(m.d.Store, m.d.Now(), id, r.PostForm.Get("answer"), m.d.Actions.SendUser)
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.done(w, r, "Sent to "+agent+".")
}

// done answers a successful inbox write: htmx reloads the page, a plain form is redirected.
func (m *mod) done(w http.ResponseWriter, r *http.Request, msg string) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/inbox")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	m.d.OK(w, r, msg, "/inbox")
}

// openEsc finds the open, user-facing escalation named by the route.
func (m *mod) openEsc(r *http.Request) (store.Escalation, error) {
	kind, agent := r.PathValue("kind"), r.PathValue("agent")
	escs, err := m.d.Store.OpenEscalations()
	if err != nil {
		return store.Escalation{}, err
	}
	for _, e := range escs {
		if e.Kind == kind && e.Agent == agent && e.User {
			return e, nil
		}
	}
	return store.Escalation{}, fmt.Errorf("no open escalation %s for %s: it may have cleared already", kind, agent)
}

func (m *mod) dismiss(w http.ResponseWriter, r *http.Request) {
	e, err := m.openEsc(r)
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	// Only a UI marker: the escalation itself is not touched, so slai still counts it.
	if err := m.d.Store.SetKV(kvDismissed+e.Kind+"."+e.Agent, strconv.FormatInt(e.Since, 10)); err != nil {
		m.d.Fail(w, r, err)
		return
	}
	m.done(w, r, "Hidden here. It is still active in slai.")
}

func (m *mod) restore(w http.ResponseWriter, r *http.Request) {
	e, err := m.openEsc(r)
	if err != nil {
		m.d.Fail(w, r, err)
		return
	}
	_ = m.d.Store.DeleteKV(kvDismissed + e.Kind + "." + e.Agent)
	m.done(w, r, "Shown again.")
}
