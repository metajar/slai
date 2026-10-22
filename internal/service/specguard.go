package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/oorbt/slai/internal/store"
)

// The spec is the acceptance criteria's source of truth (review-loop-audit.md B). When
// a task gets a spec_path, the file's sha256, length and mtime are recorded (KV
// spec.hash.<task>). At review_diff / approve / request_changes the file is read again:
//   - same bytes: nothing;
//   - the old bytes are a strict prefix of the new: a spec addendum, allowed and reported;
//   - anything else (edited, truncated, deleted): the spec was rewritten, and approve
//     refuses unless the call says spec_rewrite_ok.
// Only the hash and length are kept, not a copy: "old is a prefix of new" is checked by
// hashing the first <length> bytes of the new file.

const (
	kvSpecHash     = "spec.hash."     // + task: specRec JSON
	kvSpecAddendum = "spec.addendum." // + task: sha256 of the last addendum already reported
)

type specRec struct {
	Path  string `json:"path"` // as stored on the task
	SHA   string `json:"sha256"`
	Len   int64  `json:"len"`
	MTime int64  `json:"mtime"` // unix
}

type specState string

const (
	specNone      specState = ""          // no spec_path
	specUnchanged specState = "unchanged" //
	specAddendum  specState = "addendum"
	specRewritten specState = "rewritten"
)

type specCheck struct {
	State  specState
	Reason string // for rewritten: what was found
}

func sha256Hex(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// recordSpec stores the baseline for a task's current spec_path (or forgets it when the
// task has none). A missing file records nothing: the first check then counts as a
// rewrite only if a baseline existed.
func (s *Service) recordSpec(taskID, specPath string) {
	if specPath == "" {
		_ = s.Store.DeleteKV(kvSpecHash + taskID)
		_ = s.Store.DeleteKV(kvSpecAddendum + taskID)
		return
	}
	b, err := os.ReadFile(s.absPath(specPath))
	if err != nil {
		return
	}
	var mt int64
	if fi, err := os.Stat(s.absPath(specPath)); err == nil {
		mt = fi.ModTime().Unix()
	}
	rec, _ := json.Marshal(specRec{Path: specPath, SHA: sha256Hex(b), Len: int64(len(b)), MTime: mt})
	_ = s.Store.SetKV(kvSpecHash+taskID, string(rec))
	_ = s.Store.DeleteKV(kvSpecAddendum + taskID)
}

// checkSpec compares the spec file with the baseline recorded for the task.
func (s *Service) checkSpec(t store.Task) specCheck {
	if t.SpecPath == "" {
		return specCheck{State: specNone}
	}
	v, ok, _ := s.Store.GetKV(kvSpecHash + t.ID)
	var rec specRec
	if !ok || json.Unmarshal([]byte(v), &rec) != nil || rec.Path != t.SpecPath {
		s.recordSpec(t.ID, t.SpecPath) // a task from before this check: its spec as it is now is the baseline
		return specCheck{State: specUnchanged}
	}
	b, err := os.ReadFile(s.absPath(t.SpecPath))
	if err != nil {
		return specCheck{State: specRewritten, Reason: "the spec file " + t.SpecPath + " can no longer be read (" + err.Error() + ")"}
	}
	if sha256Hex(b) == rec.SHA {
		return specCheck{State: specUnchanged}
	}
	if int64(len(b)) > rec.Len && sha256Hex(b[:rec.Len]) == rec.SHA {
		return specCheck{State: specAddendum}
	}
	return specCheck{State: specRewritten, Reason: fmt.Sprintf("the spec file %s was changed or truncated since task %s was assigned (it is not just longer)", t.SpecPath, t.ID)}
}

// specNote records the check's events once and returns the result fields to add to
// the tool's answer. caller is the reviewer. override is spec_rewrite_ok.
func (s *Service) specNote(caller string, t store.Task, c specCheck, override bool) map[string]any {
	out := map[string]any{}
	switch c.State {
	case specAddendum:
		out["spec_addendum"] = "spec addendum since assignment: text was appended to " + t.SpecPath + " after " + t.ID + " was assigned (appending is allowed; earlier text is unchanged)"
		cur, _ := os.ReadFile(s.absPath(t.SpecPath))
		sum := sha256Hex(cur)
		if last, _, _ := s.Store.GetKV(kvSpecAddendum + t.ID); last != sum { // one event per distinct addendum
			_ = s.Store.SetKV(kvSpecAddendum+t.ID, sum)
			b, _ := json.Marshal(map[string]any{"task_id": t.ID, "by": caller, "spec_path": t.SpecPath})
			_ = s.Store.AddEvent(caller, "spec_addendum", string(b))
		}
	case specRewritten:
		out["spec_rewritten"] = c.Reason
	}
	return out
}
