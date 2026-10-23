package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

func TestHookHealthGolden(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	agents := []store.Agent{{ID: "eng-1"}, {ID: "eng-2"}, {ID: "orch"}, {ID: "eng-3"}}
	// eng-1: recent + old hook.log lines; eng-2: old only; orch: none.
	write := func(agent, body string) {
		d := filepath.Join(dir, ".slai", "run", agent)
		_ = os.MkdirAll(d, 0o755)
		_ = os.WriteFile(filepath.Join(d, "hook.log"), []byte(body), 0o644)
	}
	write("eng-1", "2026-10-21T09:00:00Z stop old\n2026-10-21T11:30:00Z pretooluse boom   happened\n")
	write("eng-2", "2026-10-21T08:00:00Z stop ancient\n")
	_ = st.SetKV("mcp.err.eng-2", `{"count":3,"last":"dial failed","at":`+strconv.FormatInt(now.Add(-5*time.Minute).Unix(), 10)+`}`)
	_ = st.SetKV("lastseen.eng-2", strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)) // a later call succeeded: recovered
	// eng-3: later stamp, but the supervisor escalated a repeat failure: still a failure
	_ = st.SetKV("mcp.err.eng-3", `{"count":2,"last":"x","at":`+strconv.FormatInt(now.Add(-5*time.Minute).Unix(), 10)+`}`)
	_ = st.SetKV("lastseen.eng-3", strconv.FormatInt(now.Add(-time.Minute).Unix(), 10))
	_ = st.SetKV("esc.mcp_crash.eng-3", `{"agent":"eng-3","kind":"mcp_crash"}`)
	_ = st.SetKV("hook.err.orch", "plain text failure")
	_ = st.SetKV("mcp.err.orch", `{"count":2,"last":"no slai tool call stamp 1m0s after an mcp__slai__ call started","at":`+strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)+`}`)

	var buf bytes.Buffer
	if failed := hookHealth(&buf, dir, st, agents, now); failed != 2 {
		t.Fatalf("failed = %d, want 2 (orch: no later stamp; eng-3: escalated)", failed)
	}
	want := `hook/mcp errors:
  ! warn: eng-1 hook log: 2 (within the last hour), latest: 2026-10-21T11:30:00Z pretooluse boom happened
      meaning: the slai hook for this agent failed, so the board may show stale status for it
      next: see .slai/run/eng-1/hook.log, and run ` + "`slai doctor --panes`" + ` to check what the agent is doing
  ! warn: eng-2 hook log: 1 (older, last seen 4h0m0s ago), latest: 2026-10-21T08:00:00Z stop ancient
      meaning: the slai hook for this agent failed, so the board may show stale status for it
      next: see .slai/run/eng-2/hook.log, and run ` + "`slai doctor --panes`" + ` to check what the agent is doing
  ! warn: eng-2 mcp errors: 3 (within the last hour) [slow, recovered: a later slai call succeeded], latest: dial failed
      meaning: the agent's slai MCP server reported an error, so its slai tool calls may have failed
      next: check the agent (` + "`slai doctor --panes`" + `); restart it if its slai tools keep failing
  ! warn: orch hook errors: 1 (age unknown), latest: plain text failure
      meaning: the slai hook for this agent failed, so the board may show stale status for it
      next: run ` + "`slai doctor --panes`" + ` to check what the agent is doing (there is no hook.log: nothing has written a hook error to a file)
  ✗ orch mcp errors: 2 (within the last hour) [no slai tool call since], latest: no slai tool call stamp 1m0s after an mcp__slai__ call started
      meaning: an slai tool call started but no completion was recorded; usually the call was interrupted or the agent was busy, sometimes its MCP server is not answering
      next: check the agent (` + "`slai doctor --panes`" + `); if it can still call slai tools, ignore this. slai restarts it once on its own
  ✗ eng-3 mcp errors: 2 (within the last hour) [MCP keeps failing: escalated], latest: x
      meaning: the agent's slai MCP server reported an error, so its slai tool calls may have failed
      next: check the agent (` + "`slai doctor --panes`" + `); restart it if its slai tools keep failing
`
	if buf.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", buf.String(), want)
	}
	buf.Reset()
	hookHealth(&buf, t.TempDir(), st, []store.Agent{{ID: "ghost"}}, now)
	if buf.Len() != 0 {
		t.Fatalf("absent sources must be silent: %q", buf.String())
	}
}
