package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const specV1 = "# Spec\n\n1. AC 1: it works\n2. AC 2: it is fast\n"

// specInReview writes a spec file, creates a spec'd task for it and has the engineer
// request review. It returns the task id and the spec's absolute path.
func specInReview(t *testing.T, s *Service, name string) (string, string) {
	t.Helper()
	// absolute, outside the repo: the test helpers commit everything under s.Dir onto the
	// task branch, which would take an in-repo spec with it when they switch branches
	abs := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(abs, []byte(specV1), 0o644); err != nil {
		t.Fatal(err)
	}
	id := specTask(t, s, abs)
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done", ACClaims: threeClaims[:2]}))
	return id, abs
}

func eventCount(s *Service, kind string) (n int) {
	for _, e := range must(s.Store.RecentEvents(500)) {
		if e.Kind == kind {
			n++
		}
	}
	return
}

// AC 1: setting spec_path records hash + mtime; changing it records the new file.
func TestSpecPathRecordsHashAndRerecordsOnChange(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	id, abs := specInReview(t, s, "a.md")
	read := func() specRec {
		v, ok, _ := s.Store.GetKV(kvSpecHash + id)
		var r specRec
		if !ok || json.Unmarshal([]byte(v), &r) != nil {
			t.Fatalf("no spec.hash record: %q %v", v, ok)
		}
		return r
	}
	r := read()
	if r.SHA != sha256Hex([]byte(specV1)) || r.Len != int64(len(specV1)) || r.MTime == 0 || r.Path != abs {
		t.Fatalf("record = %+v", r)
	}
	other := filepath.Join(filepath.Dir(abs), "b.md")
	os.WriteFile(other, []byte("# Other spec\n"), 0o644)
	must(s.UpdateTask(ctx, "orch", UpdateTaskArgs{TaskID: id, SpecPath: other}))
	if r2 := read(); r2.Path != other || r2.SHA != sha256Hex([]byte("# Other spec\n")) {
		t.Fatalf("not re-recorded: %+v", r2)
	}
}

// AC 2: appending leaves approve working, says so in the result, and logs one event.
func TestSpecAppendIsAnAddendumNotARewrite(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	id, abs := specInReview(t, s, "a.md")
	f, _ := os.OpenFile(abs, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("3. AC 3: added later\n")
	f.Close()
	must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id}))
	ap := j(must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id})))
	if ap["status"] != "merge_queued" || ap["spec_addendum"] == nil || ap["spec_rewritten"] != nil {
		t.Fatalf("approve = %v", ap)
	}
	if n := eventCount(s, "spec_addendum"); n != 1 {
		t.Fatalf("spec_addendum events = %d, want exactly one for one addendum", n)
	}
}

// AC 3: edited or truncated earlier content refuses approve unless spec_rewrite_ok.
func TestSpecRewriteRefusesApproveUnlessOverridden(t *testing.T) {
	curT = t
	for _, tc := range []struct {
		name  string
		write func(abs string)
	}{
		{"edited", func(abs string) {
			os.WriteFile(abs, []byte(strings.Replace(specV1, "it works", "anything goes", 1)), 0o644)
		}},
		{"truncated", func(abs string) { os.WriteFile(abs, []byte("# Spec\n"), 0o644) }},
		{"edited and extended", func(abs string) {
			os.WriteFile(abs, []byte(strings.Replace(specV1, "fast", "slow", 1)+"more\n"), 0o644)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			curT = t
			s := newSvc(t, true)
			id, abs := specInReview(t, s, "a.md")
			tc.write(abs)
			must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id}))
			_, err := s.Approve(ctx, "prin", ApproveArgs{TaskID: id})
			wantErr(t, err, id)
			wantErr(t, err, "spec_rewrite_ok")
			if st := must(s.Store.GetTask(id)).Status; st != "in_review" {
				t.Fatalf("a refused approve changed the status: %s", st)
			}
			if eventCount(s, "spec_rewritten") != 1 {
				t.Fatal("want a spec_rewritten event for the refusal")
			}
			ap := j(must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id, SpecRewriteOK: true})))
			if ap["status"] != "merge_queued" {
				t.Fatalf("override did not approve: %v", ap)
			}
			var ev map[string]any
			for _, e := range must(s.Store.RecentEvents(50)) { // newest first
				if e.Kind == "spec_rewritten" {
					_ = json.Unmarshal([]byte(e.Data), &ev)
					break
				}
			}
			if ev["task_id"] != id || ev["approver"] != "prin" || ev["override"] != true {
				t.Fatalf("event should record task, approver and the override: %v", ev)
			}
		})
	}
}

// AC 4: a spec that vanishes counts as rewritten; a task with no spec is unaffected.
func TestSpecMissingCountsAsRewrittenAndNoSpecIsUnaffected(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	id, abs := specInReview(t, s, "a.md")
	os.Remove(abs)
	_, err := s.Approve(ctx, "prin", ApproveArgs{TaskID: id})
	wantErr(t, err, "spec_rewrite_ok")

	id2 := specTask(t, s, "")
	must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id2, Summary: "done"}))
	ap := j(must(s.Approve(ctx, "prin", ApproveArgs{TaskID: id2})))
	if ap["status"] != "merge_queued" || ap["spec_addendum"] != nil || ap["spec_rewritten"] != nil {
		t.Fatalf("a task without a spec changed behaviour: %v", ap)
	}
	if _, ok, _ := s.Store.GetKV(kvSpecHash + id2); ok {
		t.Fatal("recorded a hash for a task without spec_path")
	}
}

// request_changes and review_diff report a rewrite without refusing.
func TestSpecRewriteIsReportedByReviewDiffAndRequestChanges(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	id, abs := specInReview(t, s, "a.md")
	os.WriteFile(abs, []byte("# Spec\n"), 0o644)
	d := j(must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id})))
	if d["spec_rewritten"] == nil {
		t.Fatalf("review_diff does not flag the rewrite: %v", d["spec_rewritten"])
	}
	rc := j(must(s.RequestChanges(ctx, "prin", RequestChangesArgs{TaskID: id, Comments: "fix it"})))
	if rc["status"] != "changes_requested" || rc["spec_rewritten"] == nil {
		t.Fatalf("request_changes = %v", rc)
	}
}
