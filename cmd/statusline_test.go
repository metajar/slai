package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/ratelimit"
	"github.com/oorbt/slai/internal/store"
)

func TestStatuslineCmd(t *testing.T) {
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, ".slai"), 0o755)
	t.Setenv("SLAI_AGENT_ID", "e1")
	t.Setenv("SLAI_PROJECT", proj)
	run := func(in string) string {
		c := statuslineCmd()
		var out bytes.Buffer
		c.SetIn(strings.NewReader(in))
		c.SetOut(&out)
		if err := c.RunE(c, nil); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String())
	}
	got := run(`{"rate_limits":{"five_hour":{"used_percentage":42,"resets_at":1},"seven_day":{"used_percentage":17,"resets_at":2}}}`)
	if got != "5h 42% · wk 17% · $0.00 today" {
		t.Fatalf("line %q", got)
	}
	// Empty and garbage input print a line and keep the saved snapshot.
	for _, in := range []string{"garbage", "", `{"model":"x"}`} {
		if run(in) == "" {
			t.Fatalf("no output for %q", in)
		}
	}
	st, _ := store.Open(filepath.Join(proj, ".slai", "state.db"))
	defer st.Close()
	// Unchanged limits do not rewrite the row; changed ones do.
	before, _, _ := ratelimit.Load(st)
	run(`{"rate_limits":{"five_hour":{"used_percentage":42,"resets_at":1},"seven_day":{"used_percentage":17,"resets_at":2}}}`)
	if again, _, _ := ratelimit.Load(st); again.At != before.At {
		t.Fatalf("unchanged snapshot rewritten")
	}
	snap, ok, _ := ratelimit.Load(st)
	if !ok || snap.FiveHour == nil || snap.FiveHour.UsedPercent != 42 || snap.Agent != "e1" || snap.At == 0 {
		t.Fatalf("snapshot %+v %v", snap, ok)
	}
}

// Unchanged limits still refresh At once a minute, so the TUI does not dim fresh data.
func TestStatuslineRefreshesAtOncePerMinute(t *testing.T) {
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, ".slai"), 0o755)
	env := map[string]string{"SLAI_AGENT_ID": "e1", "SLAI_PROJECT": proj}
	get := func(k string) string { return env[k] }
	in := `{"rate_limits":{"five_hour":{"used_percentage":42,"resets_at":1}}}`
	t0 := time.Unix(1_000_000, 0)
	runStatusline(strings.NewReader(in), get, func() time.Time { return t0 })
	load := func() ratelimit.Snapshot {
		st, _ := store.Open(filepath.Join(proj, ".slai", "state.db"))
		defer st.Close()
		snap, _, _ := ratelimit.Load(st)
		return snap
	}
	runStatusline(strings.NewReader(in), get, func() time.Time { return t0.Add(30 * time.Second) })
	if at := load().At; at != t0.Unix() {
		t.Fatalf("rewritten after 30s: At=%d", at)
	}
	runStatusline(strings.NewReader(in), get, func() time.Time { return t0.Add(61 * time.Second) })
	if s := load(); s.At != t0.Unix()+61 || s.FiveHour == nil || s.FiveHour.UsedPercent != 42 {
		t.Fatalf("At not refreshed after 61s: %+v", s)
	}
}
