package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/store"
	"github.com/oorbt/slai/internal/supervisor"
)

func TestHookCmdEndToEnd(t *testing.T) {
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, ".slai"), 0o755)
	db := filepath.Join(proj, ".slai", "state.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	st.UpsertAgent(store.Agent{ID: "e1", Role: "engineer"})
	st.Close()
	t.Setenv("SLAI_AGENT_ID", "e1")
	t.Setenv("SLAI_PROJECT", proj)

	run := func(event, stdin string) string {
		c := hookCmd()
		var out, errb bytes.Buffer
		c.SetIn(strings.NewReader(stdin))
		c.SetOut(&out)
		c.SetErr(&errb)
		c.SetArgs([]string{event})
		if err := c.Execute(); err != nil {
			t.Fatalf("hook must not fail: %v", err)
		}
		return out.String() + errb.String()
	}
	status := func() string {
		st, _ := store.Open(db)
		defer st.Close()
		a, _ := st.AgentByID("e1")
		return a.Status
	}
	if out := run("UserPromptSubmit", `{"session_id":"s","prompt":"go"}`); out != "" {
		t.Fatalf("stdout not empty: %q", out)
	}
	if s := status(); s != "working" {
		t.Fatalf("got %s", s)
	}
	run("notification", `{"notification_type":"permission_prompt"}`)
	if s := status(); s != "waiting" {
		t.Fatalf("got %s", s)
	}
	run("STOP", `{}`)
	if s := status(); s != "idle" {
		t.Fatalf("got %s", s)
	}
	// garbage input and unknown agent: still exit 0, logged
	run("stop", `not json`)
	t.Setenv("SLAI_AGENT_ID", "ghost")
	run("stop", `{}`)
	b, err := os.ReadFile(filepath.Join(proj, ".slai", "run", "ghost", "hook.log"))
	if err != nil || !strings.Contains(string(b), "handle:") {
		t.Fatalf("log: %q %v", b, err)
	}
	// missing env: no-op
	t.Setenv("SLAI_AGENT_ID", "")
	run("stop", `{}`)
}

func TestStopHookRespawnsSupervisorOnlyWhenNotStoppedOnPurpose(t *testing.T) {
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, ".slai", "run"), 0o755)
	st, err := store.Open(filepath.Join(proj, ".slai", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	st.UpsertAgent(store.Agent{ID: "e1", Role: "engineer"})
	st.Close()
	getenv := func(k string) string { return map[string]string{"SLAI_AGENT_ID": "e1", "SLAI_PROJECT": proj}[k] }
	spawned := 0
	old := spawnSupervisor
	spawnSupervisor = func(string) error { spawned++; return nil }
	t.Cleanup(func() { spawnSupervisor = old })
	stop := func() { runHook("stop", strings.NewReader(`{}`), getenv) }

	stop() // no pidfile: the supervisor never ran here
	if spawned != 0 {
		t.Fatal("respawned although no supervisor ever ran")
	}
	os.WriteFile(supervisor.PidPath(proj), []byte("123\n"), 0o644) // pidfile, lock free
	supervisor.MarkStopped(proj)
	stop()
	if spawned != 0 {
		t.Fatal("respawned after `slai down`")
	}
	os.Remove(supervisor.StoppedPath(proj))
	runHook("userpromptsubmit", strings.NewReader(`{}`), getenv)
	if spawned != 0 {
		t.Fatal("respawned on a non-Stop hook")
	}
	stop()
	if spawned != 1 {
		t.Fatalf("spawned = %d, want 1", spawned)
	}
	// a live supervisor (lock held) is left alone
	lock, err := supervisor.Acquire(proj)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	stop()
	if spawned != 1 {
		t.Fatal("respawned over a running supervisor")
	}
}

func TestConcurrentStopHooksRespawnOnce(t *testing.T) {
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, ".slai", "run"), 0o755)
	st, err := store.Open(filepath.Join(proj, ".slai", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		st.UpsertAgent(store.Agent{ID: "e" + string(rune('1'+i)), Role: "engineer"})
	}
	st.Close()
	os.WriteFile(supervisor.PidPath(proj), []byte("123\n"), 0o644) // crashed: pidfile, lock free

	var mu sync.Mutex
	spawned := 0
	var daemon *supervisor.Lock
	old := spawnSupervisor
	spawnSupervisor = func(dir string) error {
		mu.Lock()
		spawned++
		mu.Unlock()
		time.Sleep(150 * time.Millisecond) // a daemon takes a moment to come up
		l, err := supervisor.Acquire(dir)  // ...then holds the lock like the real one
		daemon = l
		return err
	}
	t.Cleanup(func() { spawnSupervisor = old; daemon.Release() })

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		id := "e" + string(rune('1'+i))
		wg.Add(1)
		go func() {
			defer wg.Done()
			getenv := func(k string) string { return map[string]string{"SLAI_AGENT_ID": id, "SLAI_PROJECT": proj}[k] }
			if out := runHook("stop", strings.NewReader(`{}`), getenv); len(out) != 0 {
				t.Errorf("hook stdout must stay empty, got %q", out)
			}
		}()
	}
	wg.Wait()
	if spawned != 1 {
		t.Fatalf("respawned %d times, want 1", spawned)
	}
}

func TestCleanSupervisorExitLeavesNothingToRespawn(t *testing.T) {
	proj := t.TempDir()
	lock, err := supervisor.Acquire(proj) // e.g. the TUI-owned supervisor
	if err != nil {
		t.Fatal(err)
	}
	if supervisor.ShouldRespawn(proj) {
		t.Fatal("respawn while a TUI-owned supervisor holds the lock")
	}
	lock.Release() // user quits the TUI
	if supervisor.ShouldRespawn(proj) {
		t.Fatal("a clean exit must not look like a crash")
	}
}

func TestFailingHookLeavesTraceForTheSupervisor(t *testing.T) {
	proj := t.TempDir()
	os.MkdirAll(filepath.Join(proj, ".slai"), 0o755)
	st, err := store.Open(filepath.Join(proj, ".slai", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// the agent is unknown to the store, so Handle fails
	getenv := func(k string) string { return map[string]string{"SLAI_AGENT_ID": "ghost", "SLAI_PROJECT": proj}[k] }
	runHook("pretooluse", strings.NewReader(`{}`), getenv)
	runHook("pretooluse", strings.NewReader(`{}`), getenv)
	r, ok := hooks.LoadErr(st, "hook.err.ghost")
	if !ok || r.Count != 2 || r.Last == "" || r.At == 0 {
		t.Fatalf("hook.err.ghost = %+v, %v; want JSON with cumulative count 2", r, ok)
	}
}
