package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/agentrows"
	"github.com/oorbt/slai/internal/hooks"
	"github.com/oorbt/slai/internal/store"
)

func groundProject(t *testing.T) (string, *store.Store) {
	dir := project(t, true)
	st, err := store.Open(filepath.Join(dir, ".slai", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return dir, st
}

func runGround(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	c := groundCmd()
	c.SetOut(&out)
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

func TestGroundCommandQueuesIdentityAndRecordsAnEvent(t *testing.T) {
	_, st := groundProject(t)
	root := newRootCmd()
	if c, _, err := root.Find([]string{"ground"}); err != nil || c.Name() != "ground" {
		t.Fatalf("`slai ground` is not a command: %v", err)
	}
	out, err := runGround(t, "eng-1")
	if err != nil || !strings.Contains(out, "queued a re-ground for eng-1") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	evs, _ := st.EventsByKind("eng-1", EventIdentityGround, 5)
	msgs, _ := st.UndeliveredFor("eng-1")
	if len(evs) != 1 || len(msgs) != 1 || msgs[0].From != "slai" || !strings.Contains(msgs[0].Body, "You are eng-1, the engineer") || !strings.Contains(msgs[0].Body, "whoami") {
		t.Fatalf("events=%v messages=%+v", evs, msgs)
	}
	if v, ok, _ := st.GetKV(hooks.KVIdentityPending + "eng-1"); !ok || v == "" {
		t.Fatal("the wake line is not marked to open with the identity sentence")
	}
	if _, err := runGround(t, "nobody"); err == nil {
		t.Fatal("unknown agent accepted")
	}
}

func TestGroundIsIdempotent(t *testing.T) {
	_, st := groundProject(t)
	if _, err := runGround(t, "eng-1"); err != nil {
		t.Fatal(err)
	}
	out, err := runGround(t, "eng-1")
	if err != nil || !strings.Contains(out, "already waiting") {
		t.Fatalf("out=%q err=%v", out, err)
	}
	evs, _ := st.EventsByKind("eng-1", EventIdentityGround, 5)
	msgs, _ := st.UndeliveredFor("eng-1")
	if len(evs) != 1 || len(msgs) != 1 {
		t.Fatalf("a repeat added work: %d events, %d messages", len(evs), len(msgs))
	}
	// once the wake has typed it (the pending mark is cleared), a new request queues again
	_ = st.DeleteKV(hooks.KVIdentityPending + "eng-1")
	if out, _ := runGround(t, "eng-1"); !strings.Contains(out, "queued a re-ground") {
		t.Fatalf("a later request was swallowed: %q", out)
	}
}

func TestStatusJSONCarriesLastGroundAtAndCorrection(t *testing.T) {
	dir, st := groundProject(t)
	read := func() []statusAgent {
		c := statusCmd()
		c.SetArgs([]string{"--json"})
		out := captureStdout(t, func() {
			if err := c.Execute(); err != nil {
				t.Fatal(err)
			}
		})
		var s struct{ Agents []statusAgent }
		if err := json.Unmarshal([]byte(out), &s); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return s.Agents
	}
	_ = dir
	for _, a := range read() {
		if a.LastGroundAt != 0 || a.IdentityCorrection != "" {
			t.Fatalf("never-grounded agent %s shows %+v", a.ID, a)
		}
	}
	if _, err := runGround(t, "eng-1"); err != nil {
		t.Fatal(err)
	}
	_ = st.AddEvent("eng-1", "identity_correction", `{"reason":"sent a message signed as orch"}`)
	var got statusAgent
	for _, a := range read() {
		if a.ID == "eng-1" {
			got = a
		}
	}
	if got.LastGroundAt == 0 || got.IdentityCorrection != "sent a message signed as orch" {
		t.Fatalf("eng-1 = %+v", got)
	}
	if n := identityNote(got, time.Now()); !strings.Contains(n, "re-grounded") || !strings.Contains(n, "identity-correction: sent a message signed as orch") {
		t.Fatalf("status text note = %q", n)
	}
	// a re-ground closes the correction
	_ = st.DeleteKV(hooks.KVIdentityPending + "eng-1")
	_, _ = runGround(t, "eng-1")
	if id := agentrows.IdentityOf(st, "eng-1"); id.Correction != "" {
		t.Fatalf("correction still open after a re-ground: %+v", id)
	}
}

// captureStdout runs f and returns what it printed to os.Stdout (status prints there directly).
func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	return string(b)
}
