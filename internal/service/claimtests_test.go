package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func commitFile(t *testing.T, s *Service, branch, file, content string) {
	t.Helper()
	gitRun(t, s.Dir, "checkout", branch)
	os.WriteFile(filepath.Join(s.Dir, file), []byte(content), 0o644)
	gitRun(t, s.Dir, "add", "-A")
	gitRun(t, s.Dir, "commit", "-m", "add "+file)
	gitRun(t, s.Dir, "checkout", "slai/integration")
}

func warningsOf(m map[string]any) string {
	ws, _ := m["warnings"].([]any)
	var b []string
	for _, w := range ws {
		b = append(b, w.(string))
	}
	return strings.Join(b, "\n")
}

// AC 1: an existing test produces no warning; a missing one is listed by name.
// AC 2: the warning is in both the request_review result and review_diff.
// AC 3: it never blocks the request.
func TestClaimNotesNamingMissingTestsWarn(t *testing.T) {
	curT = t
	s := newSvc(t, true)
	id := specTask(t, s, "")
	branch := "slai/eng-1/" + id
	commitFile(t, s, branch, "real_test.go", "package x\n\nfunc TestReal(t *testing.T) {}\n")

	ok := []ACClaimArg{{AC: "AC 1", Met: true, Note: "TestReal covers it"}, {AC: "AC 2", Met: true, Note: "see the Tests below, and testing notes"}}
	r := j(must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "done", ACClaims: ok})))
	if r["status"] != "in_review" || r["warnings"] != nil {
		t.Fatalf("existing test (and ordinary words) should not warn: %v", r)
	}
	if d := j(must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id}))); d["warnings"] != nil {
		t.Fatalf("review_diff warned about an existing test: %v", d["warnings"])
	}

	bad := []ACClaimArg{{AC: "AC 1", Met: true, Note: "TestReal covers it"}, {AC: "AC 2", Met: true, Note: "TestGhost_Case and TestNope"}}
	r = j(must(s.RequestReview(ctx, "eng-1", RequestReviewArgs{TaskID: id, Summary: "again", ACClaims: bad})))
	w := warningsOf(r)
	if r["status"] != "in_review" { // AC 3: still accepted
		t.Fatalf("a missing test blocked the request: %v", r)
	}
	if !strings.Contains(w, "TestGhost_Case") || !strings.Contains(w, "TestNope") || strings.Contains(w, "TestReal") || !strings.Contains(w, "AC 2") {
		t.Fatalf("request_review warning = %q", w)
	}
	d := j(must(s.ReviewDiff(ctx, "prin", TaskRef{TaskID: id})))
	if dw := warningsOf(d); !strings.Contains(dw, "TestGhost_Case") || !strings.Contains(dw, "TestNope") {
		t.Fatalf("review_diff warning = %q", dw)
	}
}
