package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/store"
)

// hookErrRecent is how recent an error must be for doctor to WARN about it.
const hookErrRecent = time.Hour

// errSummary is one source of errors for an agent: a count, the latest line and
// when it happened (zero when unknown).
type errSummary struct {
	count int
	last  string
	at    time.Time
}

// hookLogSummary reads .slai/run/<agent>/hook.log; every line is an error
// ("<RFC3339> <event> <message>"). A missing log is an empty summary.
func hookLogSummary(dir, agent string) errSummary {
	b, err := os.ReadFile(filepath.Join(dir, ".slai", "run", agent, "hook.log"))
	if err != nil {
		return errSummary{}
	}
	var s errSummary
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		s.count++
		s.last, s.at = l, time.Time{}
		if ts, _, ok := strings.Cut(l, " "); ok {
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				s.at = t
			}
		}
	}
	return s
}

// kvErrSummary reads KV <prefix>.<agent>. The value is JSON
// {"count":N,"last":"...","at":unix} or, failing that, plain text (one error).
// An absent key is an empty summary.
func kvErrSummary(st *store.Store, key string) errSummary {
	v, ok, err := st.GetKV(key)
	if err != nil || !ok || strings.TrimSpace(v) == "" {
		return errSummary{}
	}
	var j struct {
		Count int    `json:"count"`
		N     int    `json:"n"`
		Last  string `json:"last"`
		Line  string `json:"line"`
		Error string `json:"error"`
		At    int64  `json:"at"`
	}
	if json.Unmarshal([]byte(v), &j) != nil {
		return errSummary{count: 1, last: strings.TrimSpace(v)}
	}
	s := errSummary{count: max(j.Count, j.N), last: firstNonEmpty(j.Last, j.Line, j.Error)}
	if s.count == 0 {
		s.count = 1
	}
	if j.At > 0 {
		s.at = time.Unix(j.At, 0)
	}
	return s
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// hookHealth prints per-agent hook/MCP error counts with the latest error line.
// A nonzero count is always a warning, never a check mark: a green mark beside
// "errors: 2" reads as "healthy" when it is not. Age keeps recent errors louder than
// stale ones ("within the last hour" vs "older, last seen Xm ago"). Each line says
// what the error means and one next step. Absent or zero sources are silent.
//
// MCP errors are graded (spec review-loop-audit.md, Doctor decision): "slow,
// recovered" is a warning when the agent's lastseen stamp is later than the error;
// it is a failure when no stamp is later, or when an mcp_crash escalation is open
// (a repeat inside MCPRepeatWindow), whatever the later stamps say. Hook errors are
// warnings only. Returns the number of failures.
func hookHealth(w io.Writer, dir string, st *store.Store, agents []store.Agent, now time.Time) (failed int) {
	printed := false
	for _, a := range agents {
		for _, src := range []struct {
			name, kind string
			s          errSummary
		}{
			{"hook log", "hook", hookLogSummary(dir, a.ID)},
			{"hook errors", "hook", kvErrSummary(st, "hook.err."+a.ID)},
			{"mcp errors", "mcp", kvErrSummary(st, "mcp.err."+a.ID)},
		} {
			if src.s.count == 0 {
				continue
			}
			if !printed {
				fmt.Fprintln(w, "hook/mcp errors:")
				printed = true
			}
			mark, note := "! warn:", ""
			if src.kind == "mcp" {
				switch {
				case escOpen(st, "mcp_crash", a.ID):
					mark, note = "✗", " [MCP keeps failing: escalated]"
					failed++
				case !src.s.at.IsZero() && lastSeen(st, a.ID) <= src.s.at.Unix():
					mark, note = "✗", " [no slai tool call since]"
					failed++
				case !src.s.at.IsZero():
					note = " [slow, recovered: a later slai call succeeded]"
				}
			}
			fmt.Fprintf(w, "  %s %s %s: %d%s%s, latest: %s\n", mark, a.ID, src.name, src.s.count, errAge(src.s.at, now), note, truncLine(src.s.last, 160))
			fmt.Fprintf(w, "      meaning: %s\n      next: %s\n", errMeaning(src.kind, src.s.last), errNext(dir, src.kind, a.ID, src.s.last))
		}
	}
	return failed
}

func escOpen(st *store.Store, kind, agent string) bool {
	_, ok, _ := st.GetKV("esc." + kind + "." + agent)
	return ok
}

func lastSeen(st *store.Store, agent string) int64 {
	v, _, _ := st.GetKV("lastseen." + agent)
	n, _ := strconv.ParseInt(v, 10, 64)
	return n
}

// errAge renders when the latest error happened.
func errAge(at, now time.Time) string {
	switch {
	case at.IsZero():
		return " (age unknown)"
	case now.Sub(at) <= hookErrRecent:
		return " (within the last hour)"
	default:
		return fmt.Sprintf(" (older, last seen %s ago)", now.Sub(at).Round(time.Minute))
	}
}

// mcpNoStamp is the text the supervisor records when an slai tool call started but
// never reached the MCP server (internal/supervisor.mcpAgent).
const mcpNoStamp = "no slai tool call stamp"

func errMeaning(kind, last string) string {
	switch {
	case kind == "mcp" && strings.Contains(last, mcpNoStamp):
		return "an slai tool call started but no completion was recorded; usually the call was interrupted or the agent was busy, sometimes its MCP server is not answering"
	case kind == "mcp":
		return "the agent's slai MCP server reported an error, so its slai tool calls may have failed"
	default:
		return "the slai hook for this agent failed, so the board may show stale status for it"
	}
}

func errNext(dir, kind, agent, last string) string {
	switch {
	case kind == "mcp" && strings.Contains(last, mcpNoStamp):
		return "check the agent (`slai doctor --panes`); if it can still call slai tools, ignore this. slai restarts it once on its own"
	case kind == "mcp":
		return "check the agent (`slai doctor --panes`); restart it if its slai tools keep failing"
	default:
		// hook.log is written only when a hook errors: name it only when it is there
		if _, err := os.Stat(filepath.Join(dir, ".slai", "run", agent, "hook.log")); err == nil {
			return "see .slai/run/" + agent + "/hook.log, and run `slai doctor --panes` to check what the agent is doing"
		}
		return "run `slai doctor --panes` to check what the agent is doing (there is no hook.log: nothing has written a hook error to a file)"
	}
}

func truncLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
