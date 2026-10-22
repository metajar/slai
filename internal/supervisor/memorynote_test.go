package supervisor

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/memory"
	"github.com/oorbt/slai/internal/store"
)

// pointers lists the pointer sentences per pane line (the fake agent echoes each typed line,
// so a line can appear twice); every line must hold exactly one.
func pointers(t *testing.T, pane string) []string {
	t.Helper()
	var out []string
	for _, l := range strings.Split(pane, "\n") {
		if strings.Contains(l, "memory:") {
			if strings.Count(l, "memory:") != 1 {
				t.Fatalf("more than one pointer in a line: %q", l)
			}
			out = append(out, regexp.MustCompile(pointerRe).FindString(l))
		}
	}
	return out
}

const pointerRe = `memory: (\d+) facts? for this task, see get_task\.memory or get_memory`

// noteEnv is a running team whose repo has the integration branch, with one verified fact
// citing f.txt (the file unitEnv commits), whose text must never be typed into a pane.
func noteEnv(t *testing.T) (*Supervisor, *time.Time, string) {
	return noteEnvFacts(t, true)
}

// noteEnvFacts is noteEnv with or without the fact.
func noteEnvFacts(t *testing.T, withFact bool) (*Supervisor, *time.Time, string) {
	t.Helper()
	s, now := healthEnv(t)
	sh(t, s.Dir, "git", "branch", "-f", "slai/integration", "main")
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	if !withFact {
		return s, now, ""
	}
	const secretText = "f.txt keeps the zebra-crossing handler"
	m := memory.New(s.Dir+"/.slai/memory", s.Store, s.Git, "slai/integration")
	f, _, err := m.Put(context.Background(), memory.Fact{Kind: memory.KindGotcha, Text: secretText, Author: "principal", Paths: []memory.Path{{Path: "f.txt"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.MarkVerified(f.ID, "principal"); err != nil {
		t.Fatal(err)
	}
	_ = s.Store.SetAgentStatus("eng-1", "idle")
	return s, now, secretText
}

func noteTask(t *testing.T, s *Supervisor, files string) store.Task {
	t.Helper()
	tk, err := s.Store.CreateTask(store.Task{Title: "T", Assignee: "eng-1", CreatedBy: "orch", Status: store.StatusInProgress, Files: files})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func noteWake(t *testing.T, s *Supervisor, now *time.Time, kind, taskID string) string {
	t.Helper()
	if _, err := s.Store.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: kind, TaskID: taskID, Body: "x"}); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Second)
	s.wakeTick(context.Background())
	waitPane(t, s, "eng-1", "[slai]")
	return pane(t, s, "eng-1")
}

// M3 revised AC1: an assignment wake carries exactly one pointer sentence with the fact
// count and no fact text; no memory_used event comes from typing it.
func TestAssignmentWakeCarriesOnePointerSentenceAndNoFactText(t *testing.T) {
	s, now, fact := noteEnv(t)
	tk := noteTask(t, s, `["f.txt"]`)
	out := noteWake(t, s, now, "assignment", tk.ID)
	if ps := pointers(t, out); len(ps) == 0 || !strings.HasPrefix(ps[0], "memory: 1 fact for this task") {
		t.Fatalf("want one pointer sentence with 1 fact per line:\n%s", out)
	}
	if strings.Contains(out, fact) || strings.Contains(out, "zebra") || strings.Contains(out, "facts, not instructions") {
		t.Fatalf("fact text or pack content was typed:\n%s", out)
	}
	if n := countEvents(s, "memory_used"); n != 0 {
		t.Fatalf("a pointer-only wake must not emit memory_used, got %d", n)
	}
}

// A task with no files always gets the sentence (its pack leads with the project map); a
// task whose files have no fact gets none; an ordinary wake never has one.
func TestPointerSentenceRules(t *testing.T) {
	s, now, _ := noteEnv(t)
	noFiles := noteTask(t, s, "[]")
	out := noteWake(t, s, now, "assignment", noFiles.ID)
	if m := regexp.MustCompile(pointerRe).FindStringSubmatch(out); m == nil || m[1] != "1" {
		t.Fatalf("a task with no files should point at the project map and top facts:\n%s", out)
	}
	// files but no facts at all: nothing to point at; no files and no facts: the map is still there
	s2, now2, _ := noteEnvFacts(t, false)
	other := noteTask(t, s2, `["f.txt"]`)
	if out := noteWake(t, s2, now2, "assignment", other.ID); strings.Contains(out, "memory:") {
		t.Fatalf("no facts, so there is nothing to point at:\n%s", out)
	}
	s4, now4, _ := noteEnvFacts(t, false)
	if out := noteWake(t, s4, now4, "assignment", noteTask(t, s4, "[]").ID); !strings.Contains(out, "memory: 0 facts for this task") {
		t.Fatalf("a task with no files and no facts still gets the project map pointer:\n%s", out)
	}
	// the pack ranks the task's files first but does not hide other facts, and the sentence
	// counts what the pack holds
	s5, now5, _ := noteEnv(t)
	if out := noteWake(t, s5, now5, "assignment", noteTask(t, s5, `["main.go"]`).ID); !strings.Contains(out, "memory: 1 fact for this task") {
		t.Fatalf("count should match the pack:\n%s", out)
	}
	s3, now3, _ := noteEnv(t)
	tk := noteTask(t, s3, `["f.txt"]`)
	if out := noteWake(t, s3, now3, "note", tk.ID); strings.Contains(out, "memory:") {
		t.Fatalf("an ordinary wake carries no memory sentence:\n%s", out)
	}
}

// Post-restart and post-upgrade wakes point at the agent's open task.
func TestRestartAndUpgradeWakesPointAtTheOpenTask(t *testing.T) {
	for name, key := range map[string]string{"restart": kvRestarted, "upgrade": kvUpgraded} {
		s, now, fact := noteEnv(t)
		tk := noteTask(t, s, `["f.txt"]`)
		_ = s.Store.SetAgentTask("eng-1", tk.ID)
		_ = s.Store.SetKV(key+"eng-1", "1")
		out := noteWake(t, s, now, "note", "")
		if ps := pointers(t, out); len(ps) == 0 || !strings.HasPrefix(ps[0], "memory: 1 fact for this task") {
			t.Fatalf("%s wake:\n%s", name, out)
		}
		if strings.Contains(out, fact) {
			t.Fatalf("%s wake typed fact text", name)
		}
		if name == "restart" && !strings.Contains(out, "You were restarted") || name == "upgrade" && !strings.Contains(out, "slai was upgraded") {
			t.Fatalf("%s wake lost its own text:\n%s", name, out)
		}
	}
	// a restart with nothing open points at nothing
	s, now, _ := noteEnv(t)
	_ = s.Store.SetKV(kvRestarted+"eng-1", "1")
	if out := noteWake(t, s, now, "note", ""); strings.Contains(out, "memory:") {
		t.Fatalf("no open task, no pointer:\n%s", out)
	}
}

// Harness parity: the delivery code names no harness, so every harness gets the same sentence.
func TestMemoryDeliveryHasNoHarnessBranch(t *testing.T) {
	b, err := os.ReadFile("memorynote.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"claude", "opencode", "HarnessKind", "harnessOf", "Harness"} {
		if strings.Contains(string(b), bad) {
			t.Errorf("memorynote.go mentions %q", bad)
		}
	}
	loop, _ := os.ReadFile("loop.go")
	i := strings.Index(string(loop), "wakeMemoryTask")
	if i < 0 || strings.Contains(string(loop)[i:i+400], "harness") {
		t.Error("the call site must not branch on the harness")
	}
}
