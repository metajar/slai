package tmux

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func newTestClient(t *testing.T) *Client {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	c := New(fmt.Sprintf("slaitest-%d", time.Now().UnixNano()))
	t.Cleanup(func() { _ = c.KillServer(context.Background()) })
	return c
}

func waitFor(t *testing.T, c *Client, s, want string) string {
	t.Helper()
	var out string
	for i := 0; i < 50; i++ {
		out, _ = c.CapturePane(context.Background(), s, false, 100)
		if strings.Contains(out, want) {
			return out
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %q; pane:\n%s", want, out)
	return out
}

func TestSessionName(t *testing.T) {
	a, b := SessionName("/a", "eng-1"), SessionName("/b", "eng-1")
	if a == b || !strings.HasPrefix(a, "slai_") || !strings.HasSuffix(a, "_eng-1") {
		t.Fatalf("bad names %q %q", a, b)
	}
	if a != SessionName("/a", "eng-1") {
		t.Fatal("not deterministic")
	}
}

func TestLifecycle(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	if l, err := c.ListSessions(ctx, ""); err != nil || len(l) != 0 {
		t.Fatalf("empty list: %v %v", l, err)
	}
	if ok, err := c.HasSession(ctx, "nope"); ok || err != nil {
		t.Fatalf("has: %v %v", ok, err)
	}
	name := SessionName(dir, "eng-1")
	if err := c.NewSession(ctx, name, dir, map[string]string{"SLAI_AGENT_ID": "eng-1"}, "cd "+dir+" && echo ID=$SLAI_AGENT_ID PWD=$(pwd); cat"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.HasSession(ctx, name); !ok {
		t.Fatal("expected session")
	}
	if err := c.NewSession(ctx, name, dir, nil, "cat"); err == nil {
		t.Fatal("duplicate should error")
	}
	waitFor(t, c, name, "ID=eng-1")
	if err := c.SendKeys(ctx, name, "hello $world; 'q'", true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, c, name, "hello $world; 'q'")
	if out, err := c.CapturePane(ctx, name, true, 0); err != nil || out == "" {
		t.Fatalf("ansi capture: %q %v", out, err)
	}
	l, _ := c.ListSessions(ctx, "slai_")
	if len(l) != 1 || l[0] != name {
		t.Fatalf("list: %v", l)
	}
	if l, _ := c.ListSessions(ctx, "zzz"); len(l) != 0 {
		t.Fatalf("prefix filter: %v", l)
	}
	if err := c.KillSession(ctx, name); err != nil {
		t.Fatal(err)
	}
	if ok, _ := c.HasSession(ctx, name); ok {
		t.Fatal("still exists")
	}
	if err := c.KillSession(ctx, name); err != nil {
		t.Fatalf("kill missing: %v", err)
	}
}

func TestInteractiveHelpers(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	dir := t.TempDir()
	// two sessions where one name prefixes the other: targets must be exact
	if err := c.NewSession(ctx, "ab", dir, nil, "cat"); err != nil {
		t.Fatal(err)
	}
	if err := c.NewSession(ctx, "abc", dir, nil, "cat"); err != nil {
		t.Fatal(err)
	}
	if err := c.SendSeq(ctx, "ab", []Key{Lit("he"), Lit("llo; world"), Named("Enter")}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, c, "ab", "hello; world")
	if out, _ := c.CapturePane(ctx, "abc", false, 0); strings.Contains(out, "hello") {
		t.Fatal("keys leaked into prefix-matching session")
	}
	// multi-line paste arrives as text; cat echoes it back
	if err := c.Paste(ctx, "ab", "p1\np2\n"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, c, "ab", "p2")
	// resize + snapshot geometry and cursor
	if err := c.ResizeWindow(ctx, "ab", 77, 21); err != nil {
		t.Fatal(err)
	}
	snap, err := c.Snapshot(ctx, "ab")
	if err != nil {
		t.Fatal(err)
	}
	if snap.W != 77 || snap.H != 21 || !strings.Contains(snap.Body, "hello; world") || !snap.CursorVisible {
		t.Fatalf("snapshot: %+v", snap)
	}
	if err := c.WindowSizeLatest(ctx, "ab"); err != nil {
		t.Fatal(err)
	}
	if err := c.BindDetachKey(ctx, "C-q"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Snapshot(ctx, "nope"); err == nil {
		t.Fatal("snapshot of missing session should fail")
	}
	// $TMUX must never reach tmux children
	if got := EnvWithout([]string{"A=1", "TMUX=/x,1,0", "TMUXX=2"}, "TMUX"); len(got) != 2 {
		t.Fatalf("EnvWithout: %v", got)
	}
	if env := c.AttachCmd("ab").Env; len(env) == 0 {
		t.Fatal("attach env not set")
	} else {
		for _, e := range env {
			if strings.HasPrefix(e, "TMUX=") {
				t.Fatal("TMUX leaked into attach env")
			}
		}
	}
}

func TestScrollRange(t *testing.T) {
	cases := []struct {
		off, h int
		s, e   string
	}{{3, 5, "-3", "1"}, {1, 24, "-1", "22"}, {26, 5, "-26", "-22"}}
	for _, c := range cases {
		if s, e := ScrollRange(c.off, c.h); s != c.s || e != c.e {
			t.Errorf("ScrollRange(%d,%d) = %s,%s want %s,%s", c.off, c.h, s, e, c.s, c.e)
		}
	}
}

func TestSnapshotAtScrollback(t *testing.T) {
	c := newTestClient(t)
	ctx := context.Background()
	if err := c.NewSession(ctx, "sb", t.TempDir(), nil, "seq 1 60; cat"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, c, "sb", "60")
	if err := c.ResizeWindow(ctx, "sb", 40, 10); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	live, err := c.SnapshotAt(ctx, "sb", 0)
	if err != nil || live.Offset != 0 || live.HistorySize < 50 || !strings.Contains(live.Body, "60") {
		t.Fatalf("live: %+v %v", live, err)
	}
	lines := func(s string) []string { return strings.Split(strings.TrimRight(s, "\n"), "\n") }
	topLive := strings.TrimSpace(lines(live.Body)[0])
	var n int
	fmt.Sscan(topLive, &n)
	// 4 lines back: the window starts 4 lines earlier and has the full height
	back, err := c.SnapshotAt(ctx, "sb", 4)
	if err != nil || back.Offset != 4 || len(lines(back.Body)) != live.H {
		t.Fatalf("back: %+v %v", back, err)
	}
	if got := strings.TrimSpace(lines(back.Body)[0]); got != fmt.Sprint(n-4) {
		t.Fatalf("first row 4 back = %q, want %d", got, n-4)
	}
	// far past the history clamps to the oldest window instead of garbage
	top, err := c.SnapshotAt(ctx, "sb", 100000)
	if err != nil || top.Offset != top.HistorySize || strings.TrimSpace(lines(top.Body)[0]) != "1" {
		t.Fatalf("top: off=%d hist=%d body=%q err=%v", top.Offset, top.HistorySize, top.Body, err)
	}
}
