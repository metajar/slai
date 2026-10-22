package store

import (
	"path/filepath"
	"reflect"
	"testing"
)

func acStore(t *testing.T) (*Store, string) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	tk, err := s.CreateTask(Task{Title: "t", Assignee: "eng-1", CreatedBy: "orch", SpecPath: "specs/x.md"})
	if err != nil {
		t.Fatal(err)
	}
	return s, tk.ID
}

func TestACClaimsRoundTripAndKeepHistory(t *testing.T) {
	s, id := acStore(t)
	n, err := s.SetACClaims(id, []ACClaim{{"AC 1", true, "TestA"}, {"AC 2", false, "not done"}, {"AC 3", true, ""}})
	if err != nil || n != 1 {
		t.Fatalf("first request: %d %v", n, err)
	}
	no, got, _ := s.LatestACClaims(id)
	want := []ACClaim{{"AC 1", true, "TestA"}, {"AC 2", false, "not done"}, {"AC 3", true, ""}}
	if no != 1 || !reflect.DeepEqual(got, want) {
		t.Fatalf("claims = %d %+v", no, got)
	}
	// a second review request is a new set; the first stays as history
	if n, _ = s.SetACClaims(id, []ACClaim{{"AC 1", true, ""}, {"AC 2", true, "fixed"}}); n != 2 {
		t.Fatalf("second request number = %d", n)
	}
	no, got, _ = s.LatestACClaims(id)
	if no != 2 || len(got) != 2 || !got[1].Met {
		t.Fatalf("latest claims = %d %+v", no, got)
	}
	var rows int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM task_ac_claims WHERE task_id=?`, id).Scan(&rows)
	if rows != 5 {
		t.Fatalf("history rows = %d, want 5", rows)
	}
	if _, err := s.SetACClaims(id, nil); err == nil {
		t.Error("an empty claim list must be refused")
	}
}

func TestACVerdictsAttachToTheLatestRequestAndListUnanswered(t *testing.T) {
	s, id := acStore(t)
	_, _ = s.SetACClaims(id, []ACClaim{{"AC 1", true, ""}, {"AC 2", true, ""}, {"AC 3", true, ""}})
	if err := s.SaveACVerdicts(id, "principal", []ACVerdict{{AC: "ac  1", Pass: true}, {AC: "AC 2", Pass: false, Note: "no test"}}); err != nil {
		t.Fatal(err)
	}
	un, _ := s.UnansweredClaims(id)
	if !reflect.DeepEqual(un, []string{"AC 3"}) {
		t.Fatalf("unanswered = %v (matching ignores case and spacing)", un)
	}
	vs, _ := s.LatestACVerdicts(id)
	if len(vs) != 2 || vs[0].By != "principal" || vs[1].Pass || vs[1].Note != "no test" || vs[0].At == 0 {
		t.Fatalf("verdicts = %+v", vs)
	}
	// a later verdict for the same AC replaces the earlier one
	_ = s.SaveACVerdicts(id, "principal", []ACVerdict{{AC: "AC 2", Pass: true}})
	if vs, _ = s.LatestACVerdicts(id); len(vs) != 2 || !vs[1].Pass {
		t.Fatalf("after replacing AC 2: %+v", vs)
	}
	// a new review request starts a clean slate of verdicts
	_, _ = s.SetACClaims(id, []ACClaim{{"AC 1", true, ""}})
	if vs, _ = s.LatestACVerdicts(id); len(vs) != 0 {
		t.Fatalf("verdicts of the previous request leaked: %+v", vs)
	}
	// GetTask carries both
	_ = s.SaveACVerdicts(id, "principal", []ACVerdict{{AC: "AC 1", Pass: true}})
	tk, err := s.GetTask(id)
	if err != nil || len(tk.ACClaims) != 1 || len(tk.ACVerdicts) != 1 {
		t.Fatalf("GetTask: %+v %v", tk, err)
	}
}
