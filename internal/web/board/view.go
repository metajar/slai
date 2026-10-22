package board

import (
	"sort"
	"strings"

	"github.com/oorbt/slai/internal/store"
)

// Card is one task on the board.
type Card struct {
	ID, Title, Assignee, Branch, SpecPath string
	Status, Col                           string
	Priority                              int
	Prio                                  string // p0, p2 ...
	PrioHigh                              bool
	Round                                 int // review rounds so far
	Waiting                               []string
	DependsOn                             []string
	DotClass, DotText                     string // the assignee's liveness: colour class + words
	ChangesRequested                      bool
	DoneNonCode                           bool
	StuckProblem                          bool // blocked by a problem, not by dependencies
	NeedsMe                               bool
	CanCancel, CanUnblock                 bool
	Prios                                 []PrioChoice // the card menu's priority buttons
}

// PrioChoice is one priority button of the card menu.
type PrioChoice struct {
	Label   string
	Current bool
}

// ColView is a column with its cards.
type ColView struct {
	Column
	Cards    []Card
	Hidden   int    // finished cards left out (the oldest), see finishedShown
	MoreHref string // where to see them all (or fewer again)
	Final    bool
}

// finishedShown is how many cards a finished column (merged, cancelled, reverted) shows before
// the rest are folded behind a link: after a long run the merged column would otherwise be
// hundreds of cards tall.
const finishedShown = 12

// BoardView is what the columns template renders.
type BoardView struct {
	Cols     []ColView
	Total    int
	Shown    int
	Filter   Filter
	View     string // the raw query string of the page, carried through writes
	Refresh  string // hx-trigger of the safety refetch
	ReadOnly bool
}

// Filter is the board's filter state; it lives in the URL so a view can be bookmarked.
type Filter struct {
	Assignee string
	Text     string
	NeedsMe  bool
	All      bool // show every finished card
}

// Active reports whether any filter is set.
func (f Filter) Active() bool { return f.Assignee != "" || f.Text != "" || f.NeedsMe }

func (f Filter) match(c Card) bool {
	if f.Assignee != "" && c.Assignee != f.Assignee {
		return false
	}
	if f.NeedsMe && !c.NeedsMe {
		return false
	}
	if t := strings.ToLower(strings.TrimSpace(f.Text)); t != "" {
		hay := strings.ToLower(c.ID + " " + c.Title + " " + c.Branch + " " + c.Assignee)
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
}

// dot maps an agent's stored status to a colour class and plain words.
func dot(status string) (class, text string) {
	switch status {
	case "working":
		return "ok", "working"
	case "waiting_on_user":
		return "user", "waiting for you"
	case "dead", "errored":
		return "bad", status
	case "":
		return "idle", "unknown"
	}
	return "idle", strings.ReplaceAll(status, "_", " ")
}

// buildBoard turns the store's tasks into columns. It reads what the page already has (tasks,
// agents, dependency state); WaitingOn is asked only for blocked tasks.
func buildBoard(st *store.Store, f Filter) (BoardView, error) {
	tasks, err := st.ListTasks("")
	if err != nil {
		return BoardView{}, err
	}
	agents, err := st.GetAgents()
	if err != nil {
		return BoardView{}, err
	}
	byAgent := map[string]store.Agent{}
	for _, a := range agents {
		byAgent[a.ID] = a
	}
	v := BoardView{Filter: f, Total: len(tasks)}
	idx := map[string]int{}
	for i, c := range Columns {
		v.Cols = append(v.Cols, ColView{Column: c})
		idx[c.Key] = i
	}
	for _, t := range tasks {
		col := ColumnOf(t.Status)
		i, ok := idx[col]
		if !ok {
			continue
		}
		c := Card{ID: t.ID, Title: t.Title, Assignee: t.Assignee, Branch: t.Branch, SpecPath: t.SpecPath,
			Status: t.Status, Col: col, Priority: t.Priority, Prio: PrioLabel(t.Priority), PrioHigh: t.Priority >= 2,
			Round: t.ReviewRounds, DependsOn: t.DependsOn,
			ChangesRequested: t.Status == store.StatusChangesRequested, DoneNonCode: t.Status == store.StatusDone}
		a := byAgent[t.Assignee]
		c.DotClass, c.DotText = dot(a.Status)
		if t.Status == store.StatusBlocked {
			c.Waiting, _ = st.WaitingOn(t.ID)
			c.StuckProblem = len(c.Waiting) == 0
		}
		// "needs you" follows the task's own state, not the assignee's: an open task that is
		// blocked by a problem, or the one an agent waiting on you is working on. A finished
		// task (merged, cancelled, reverted) and an agent's other tasks never carry it.
		c.NeedsMe = !isFinal(col) && (c.StuckProblem || (a.Status == "waiting_on_user" && a.CurrentTask == t.ID))
		c.CanCancel = !isFinal(col)
		c.CanUnblock = t.Status == store.StatusBlocked && len(c.Waiting) == 0
		for p := 0; p <= 3; p++ {
			c.Prios = append(c.Prios, PrioChoice{Label: PrioLabel(p), Current: t.Priority == p})
		}
		if !f.match(c) {
			continue
		}
		v.Shown++
		v.Cols[i].Cards = append(v.Cols[i].Cards, c)
	}
	updated := map[string]int64{}
	for _, t := range tasks {
		updated[t.ID] = t.UpdatedAt
	}
	for i := range v.Cols {
		cards := v.Cols[i].Cards
		v.Cols[i].Final = isFinal(v.Cols[i].Key)
		if v.Cols[i].Final {
			// finished work: newest first, and only the newest few unless asked for all
			sort.SliceStable(cards, func(a, b int) bool {
				if updated[cards[a].ID] != updated[cards[b].ID] {
					return updated[cards[a].ID] > updated[cards[b].ID]
				}
				return cards[a].ID > cards[b].ID
			})
			if len(cards) > finishedShown && !f.All {
				v.Cols[i].Hidden = len(cards) - finishedShown
				v.Cols[i].Cards = cards[:finishedShown]
			}
			if v.Cols[i].Hidden > 0 || (f.All && len(cards) > finishedShown) {
				g := f
				g.All = !f.All
				v.Cols[i].MoreHref = g.Href()
			}
			continue
		}
		sort.SliceStable(cards, func(a, b int) bool {
			if cards[a].Priority != cards[b].Priority {
				return cards[a].Priority > cards[b].Priority
			}
			return cards[a].ID < cards[b].ID
		})
	}
	return v, nil
}
