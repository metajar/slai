package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/memory"
	"github.com/oorbt/slai/internal/store"
)

// Team memory tools: remember, recall, get_memory, and the curation hooks in
// request_review (facts[]) and approve / request_changes (fact_verdicts[]).

// Limits (spec decision 15).
const (
	MaxFactsPerPath   = 20
	MaxFactWritesHour = 10
	maxRecall         = 10
	similarFact       = 0.8 // word-overlap above which two facts on the same paths are one
)

// Errors a rejected fact returns; each names the rule that fired.
var (
	ErrFactTooLong     = errors.New("fact text is longer than 300 characters")
	ErrFactSecret      = errors.New("fact text looks like a secret or a raw hash; cite paths, not shas or credentials")
	ErrFactInstruction = errors.New("fact text reads as an instruction, not a fact")
	ErrFactPath        = errors.New("fact path must be a clean relative path inside the repository")
	ErrFactPathCap     = errors.New("too many facts already cite this path")
	ErrFactRate        = errors.New("too many facts written by this agent in the last hour")
	ErrFactCheckRole   = errors.New("only the principal may attach a check command; propose the fact without it")
)

type RememberArgs struct {
	Text     string   `json:"text" jsonschema:"the fact, one sentence, at most 300 characters; describe, do not instruct. Newlines and backticks are stripped"`
	Kind     string   `json:"kind" jsonschema:"architecture, convention, gotcha, command or decision-pointer"`
	Paths    []string `json:"paths,omitempty" jsonschema:"repository files the fact is about (relative paths that exist on the integration branch); a fact with no path can never be verified"`
	Decision string   `json:"decision,omitempty" jsonschema:"for kind decision-pointer: the heading in .slai/decisions.md"`
	Check    string   `json:"check,omitempty" jsonschema:"optional command that exits 0 when the fact holds; only a human runs it"`
}

type RecallArgs struct {
	Query        string   `json:"query,omitempty" jsonschema:"words that must all appear in the fact"`
	Paths        []string `json:"paths,omitempty" jsonschema:"facts citing these files (or files under these directories)"`
	TaskID       string   `json:"task_id,omitempty" jsonschema:"facts citing the files of this task"`
	IncludeStale bool     `json:"include_stale,omitempty" jsonschema:"also return stale facts (cited files changed); they are marked stale"`
}

type GetMemoryArgs struct {
	Query  string `json:"query,omitempty" jsonschema:"optional words to narrow the facts"`
	TaskID string `json:"task_id,omitempty" jsonschema:"rank facts for this task's files first (the same pack get_task returns as memory); without it you get the project map and the top verified facts"`
}

// FactVerdictArg is the principal's verdict on a fact an engineer proposed with request_review.
type FactVerdictArg struct {
	ID      string `json:"id" jsonschema:"fact id the engineer proposed"`
	Confirm bool   `json:"confirm" jsonschema:"true to verify the fact (approve only), false to reject and delete it"`
	Reason  string `json:"reason,omitempty" jsonschema:"why a fact is rejected (required when confirm is false)"`
}

type factOut struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Text     string   `json:"text"`
	Status   string   `json:"status"` // verified | proposed | stale
	Paths    []string `json:"paths,omitempty"`
	Author   string   `json:"author,omitempty"`
	Decision string   `json:"decision,omitempty"`
}

func (s *Service) memory() *memory.Memory {
	m := memory.New(filepath.Join(s.Dir, ".slai", "memory"), s.Store, s.Git, s.integration())
	m.Now = s.clock
	return m
}

// ---- write-time rules ----

var (
	wordRe = regexp.MustCompile(`[A-Za-z0-9']+`)

	secretRes = []*regexp.Regexp{
		regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
		regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`),
		regexp.MustCompile(`\bxox[abprs]-[A-Za-z0-9-]{10,}`),
		regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY`),
		regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}`),
		regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key|auth)\w*\s*[:=]\s*\S{6,}`),
		regexp.MustCompile(`\b[A-Fa-f0-9]{40,}\b`),
		regexp.MustCompile(`\b[A-Za-z0-9+/]{40,}={0,2}`),
	}

	instructVerbs   = setOf("ignore", "skip", "bypass", "disable", "override", "always", "never", "must", "should")
	instructTargets = setOf("lane", "guard", "hook", "permission", "review", "approve", "spec", "rule", "test", "principal", "orch")
	instructPhrases = []string{"you must", "you should", "do not", "don't", "sudo", "rm -rf", "curl ", "wget ", "--dangerously", "export "}
	imperatives     = setOf("run", "execute", "delete", "remove", "install", "push", "force", "merge")
)

func setOf(w ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range w {
		m[x] = true
	}
	return m
}

// SanitizeFact strips newlines and backticks and collapses whitespace.
func SanitizeFact(text string) string {
	text = strings.ReplaceAll(text, "`", "")
	return strings.Join(strings.Fields(text), " ")
}

// CheckFactText applies the write-time rules to already sanitised text. The deny-list
// (spec addendum Q4) is a tripwire, not the defence: the defence is the fixed template,
// the non-instruction prefix and verification at approve.
func CheckFactText(text string) error {
	if n := len([]rune(text)); n == 0 {
		return memory.ErrBadText
	} else if n > memory.MaxTextLen {
		return fmt.Errorf("%w (%d)", ErrFactTooLong, n)
	}
	for _, re := range secretRes {
		if re.MatchString(text) {
			return ErrFactSecret
		}
	}
	lower := strings.ToLower(text)
	words := wordRe.FindAllString(lower, -1)
	for i, w := range words {
		if !instructVerbs[w] {
			continue
		}
		for j := i + 1; j < len(words) && j <= i+6; j++ {
			if instructTargets[words[j]] {
				return fmt.Errorf("%w: %q before %q", ErrFactInstruction, w, words[j])
			}
		}
	}
	for _, p := range instructPhrases {
		if strings.Contains(lower, p) {
			return fmt.Errorf("%w: contains %q", ErrFactInstruction, strings.TrimSpace(p))
		}
	}
	if len(words) > 0 && imperatives[words[0]] {
		return fmt.Errorf("%w: starts with the imperative %q", ErrFactInstruction, words[0])
	}
	return nil
}

func cleanFactPath(p string) (string, error) {
	c := path.Clean(filepath.ToSlash(strings.TrimSpace(p)))
	if c == "." || strings.HasPrefix(c, "/") || c == ".." || strings.HasPrefix(c, "../") || strings.ContainsAny(c, "\n\x00") {
		return "", fmt.Errorf("%w: %q", ErrFactPath, p)
	}
	return c, nil
}

// ---- remember ----

const kvMemWrites = "memory.writes."

func (s *Service) Remember(ctx context.Context, caller string, a RememberArgs) (any, error) {
	if err := s.begin(caller, "remember", a); err != nil {
		return nil, err
	}
	role, _ := s.role(caller)
	text := SanitizeFact(a.Text)
	if err := CheckFactText(text); err != nil {
		return nil, err
	}
	if err := checkTexts("decision", a.Decision, "check", a.Check); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Check) != "" && role != config.RolePrincipal {
		return nil, ErrFactCheckRole // `slai memory verify` runs it on the user's machine
	}
	var paths []memory.Path
	seen := map[string]bool{}
	for _, p := range a.Paths {
		c, err := cleanFactPath(p)
		if err != nil {
			return nil, err
		}
		if !seen[c] {
			seen[c] = true
			paths = append(paths, memory.Path{Path: c})
		}
	}
	f := memory.Fact{Kind: a.Kind, Text: text, Author: caller, Status: memory.StatusProposed, Paths: paths,
		Decision: strings.TrimSpace(a.Decision), Check: strings.TrimSpace(a.Check)}
	if role == config.RolePrincipal { // the verifier's own facts are verified directly
		f.Status, f.VerifiedBy = memory.StatusVerified, "principal"
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	m := s.memory()
	facts, _, err := m.List()
	if err != nil {
		return nil, err
	}
	sorted := append([]memory.Path(nil), paths...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	id := memory.ID(f.Kind, f.Text, sorted)
	for _, e := range facts { // dedupe: same id, or same kind and paths with near-identical text
		if e.ID == id || (len(paths) > 0 && e.Kind == f.Kind && samePaths(e.Paths, sorted) && similar(e.Text, f.Text) >= similarFact) {
			return map[string]any{"id": e.ID, "status": factStatus(s, e), "existing": true}, nil
		}
	}
	for _, p := range paths {
		var ids []string
		for _, e := range facts {
			for _, ep := range e.Paths {
				if ep.Path == p.Path {
					ids = append(ids, e.ID)
				}
			}
		}
		if len(ids) >= MaxFactsPerPath {
			return nil, fmt.Errorf("%w: %s already has %d facts (%s); reuse or reject one instead", ErrFactPathCap, p.Path, len(ids), strings.Join(ids, ", "))
		}
	}
	now := s.clock()
	recent := s.recentFactWrites(caller, now)
	if len(recent) >= MaxFactWritesHour {
		return nil, fmt.Errorf("%w (limit %d per hour)", ErrFactRate, MaxFactWritesHour)
	}
	put, existed, err := m.Put(ctx, f)
	if err != nil {
		return nil, err
	}
	if !existed {
		b, _ := json.Marshal(append(recent, now.Unix()))
		_ = s.Store.SetKV(kvMemWrites+caller, string(b))
	}
	return map[string]any{"id": put.ID, "status": put.Status, "existing": existed}, nil
}

func (s *Service) recentFactWrites(agent string, now time.Time) []int64 {
	v, _, _ := s.Store.GetKV(kvMemWrites + agent)
	var all, keep []int64
	_ = json.Unmarshal([]byte(v), &all)
	for _, t := range all {
		if now.Sub(time.Unix(t, 0)) < time.Hour {
			keep = append(keep, t)
		}
	}
	return keep
}

func samePaths(a []memory.Path, b []memory.Path) bool {
	if len(a) != len(b) {
		return false
	}
	x := map[string]bool{}
	for _, p := range a {
		x[p.Path] = true
	}
	for _, p := range b {
		if !x[p.Path] {
			return false
		}
	}
	return true
}

// similar is the Jaccard overlap of the two texts' word sets.
func similar(a, b string) float64 {
	wa, wb := setOf(wordRe.FindAllString(strings.ToLower(a), -1)...), setOf(wordRe.FindAllString(strings.ToLower(b), -1)...)
	inter := 0
	for w := range wa {
		if wb[w] {
			inter++
		}
	}
	if union := len(wa) + len(wb) - inter; union > 0 {
		return float64(inter) / float64(union)
	}
	return 0
}

// factStatus is verified, proposed or stale (stale wins), from the index.
func factStatus(s *Service, f memory.Fact) string {
	if r, err := s.Store.GetFact(f.ID); err == nil && r.Stale {
		return "stale"
	}
	return f.Status
}

// ---- recall / get_memory ----

func (s *Service) selectFacts(ctx context.Context, query string, paths []string, includeStale bool) ([]factOut, error) {
	facts, _, err := s.memory().List()
	if err != nil {
		return nil, err
	}
	words := setOf(wordRe.FindAllString(strings.ToLower(query), -1)...)
	var out []factOut
	for _, f := range facts {
		st := factStatus(s, f)
		if st == "stale" && !includeStale {
			continue
		}
		if len(words) > 0 {
			have := setOf(wordRe.FindAllString(strings.ToLower(f.Text), -1)...)
			ok := true
			for w := range words {
				if !have[w] {
					ok = false
				}
			}
			if !ok {
				continue
			}
		}
		if len(paths) > 0 && !citesAny(f, paths) {
			continue
		}
		o := factOut{ID: f.ID, Kind: f.Kind, Text: f.Text, Status: st, Author: f.Author, Decision: f.Decision}
		for _, p := range f.Paths {
			o.Paths = append(o.Paths, p.Path)
		}
		out = append(out, o)
	}
	rank := map[string]int{"verified": 0, "proposed": 1, "stale": 2}
	sort.SliceStable(out, func(i, j int) bool {
		if rank[out[i].Status] != rank[out[j].Status] {
			return rank[out[i].Status] < rank[out[j].Status]
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func citesAny(f memory.Fact, paths []string) bool {
	for _, want := range paths {
		want = strings.TrimSuffix(path.Clean(filepath.ToSlash(want)), "/")
		for _, p := range f.Paths {
			if p.Path == want || strings.HasPrefix(p.Path, want+"/") {
				return true
			}
		}
	}
	return false
}

func (s *Service) Recall(ctx context.Context, caller string, a RecallArgs) (any, error) {
	if err := s.begin(caller, "recall", a); err != nil {
		return nil, err
	}
	paths := a.Paths
	if a.TaskID != "" {
		t, err := s.task(a.TaskID)
		if err != nil {
			return nil, err
		}
		var files []string
		_ = json.Unmarshal([]byte(t.Files), &files)
		paths = append(paths, files...)
		if len(paths) == 0 {
			return map[string]any{"facts": []factOut{}, "note": "task " + t.ID + " lists no files"}, nil
		}
	}
	out, err := s.selectFacts(ctx, a.Query, paths, a.IncludeStale)
	if err != nil {
		return nil, err
	}
	if len(out) > maxRecall {
		out = out[:maxRecall]
	}
	if out == nil {
		out = []factOut{}
	}
	return map[string]any{"facts": out}, nil
}

// PackHeading opens every memory pack (see memory.BuildPack).
const PackHeading = memory.PackHeading

// packFor builds the memory pack for t (nil: no task, so the project-wide pack) and
// records the memory_used event: it is called exactly when a pack is handed to an agent
// (get_task's "memory" field, get_memory), never for the pointer sentence a wake types.
func (s *Service) packFor(ctx context.Context, caller string, t *store.Task, query string) (memory.Pack, error) {
	team := s.team()
	var files []string
	if t != nil {
		_ = json.Unmarshal([]byte(t.Files), &files)
	}
	p, err := s.memory().BuildPack(ctx, memory.TeamPackOptions(team, s.Dir, files, query))
	if err != nil {
		return memory.Pack{}, err
	}
	data := map[string]any{"agent": caller, "fact_ids": append([]string{}, p.FactIDs...), "tokens": p.Tokens}
	if t != nil {
		data["task"], data["task_id"] = t.ID, t.ID
	}
	b, _ := json.Marshal(data)
	_ = s.Store.AddEvent(caller, "memory_used", string(b))
	_ = s.Store.TouchFacts(p.FactIDs, s.clock().Unix())
	return p, nil
}

// GetMemory returns the pack get_task would put in its "memory" field: with task_id, the
// pack for that task's files (byte for byte what get_task(task_id).memory is); without,
// the project map and the top verified facts.
func (s *Service) GetMemory(ctx context.Context, caller string, a GetMemoryArgs) (any, error) {
	if err := s.begin(caller, "get_memory", a); err != nil {
		return nil, err
	}
	var t *store.Task
	if a.TaskID != "" {
		tk, err := s.task(a.TaskID)
		if err != nil {
			return nil, err
		}
		t = &tk
	}
	p, err := s.packFor(ctx, caller, t, a.Query)
	if err != nil {
		return nil, err
	}
	return map[string]any{"memory": p.Text}, nil
}

// ---- curation: propose at request_review, verify at approve ----

const kvReviewFacts = "review.facts." // + task id: JSON list of fact ids the engineer proposed

// proposeFacts validates the ids an engineer attaches to request_review (their own,
// still-proposed facts) and remembers them for the reviewer. It replaces any earlier list.
func (s *Service) proposeFacts(caller, taskID string, ids []string) ([]string, error) {
	var keep []string
	seen := map[string]bool{}
	m := s.memory()
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		f, err := m.Get(id)
		if err != nil {
			return nil, fmt.Errorf("facts: %s is not a fact (%v)", id, err)
		}
		if f.Author != caller || f.Status != memory.StatusProposed {
			return nil, fmt.Errorf("facts: %s is not a proposed fact of yours (author %s, status %s)", id, f.Author, f.Status)
		}
		keep = append(keep, id)
	}
	if ids != nil {
		b, _ := json.Marshal(keep)
		return keep, s.Store.SetKV(kvReviewFacts+taskID, string(b))
	}
	return nil, nil
}

func (s *Service) proposedFacts(taskID string) []string {
	v, _, _ := s.Store.GetKV(kvReviewFacts + taskID)
	var ids []string
	_ = json.Unmarshal([]byte(v), &ids)
	return ids
}

type factPlan struct {
	task     string
	confirm  []string
	reject   map[string]string // id -> reason
	pending  []string          // proposed ids with no verdict
	allowYes bool
}

// planFactVerdicts checks verdicts against the task's proposed facts without changing
// anything. Confirming is allowed only from approve; a stale fact cannot be confirmed.
func (s *Service) planFactVerdicts(ctx context.Context, t store.Task, vs []FactVerdictArg, allowConfirm bool) (*factPlan, error) {
	p := &factPlan{task: t.ID, reject: map[string]string{}}
	proposed := setOf(s.proposedFacts(t.ID)...)
	m := s.memory()
	done := map[string]bool{}
	for _, v := range vs {
		if !proposed[v.ID] {
			return nil, fmt.Errorf("fact_verdicts: %s was not proposed with request_review on task %s", v.ID, t.ID)
		}
		if done[v.ID] {
			return nil, fmt.Errorf("fact_verdicts: two verdicts for %s", v.ID)
		}
		done[v.ID] = true
		if v.Confirm {
			if !allowConfirm {
				return nil, fmt.Errorf("fact_verdicts: %s can only be confirmed by approve; request_changes may only reject facts", v.ID)
			}
			f, err := m.Get(v.ID)
			if err != nil {
				return nil, fmt.Errorf("fact_verdicts: %s: %w", v.ID, err)
			}
			if r, err := m.Check(ctx, f); err == nil && r.Stale {
				return nil, fmt.Errorf("fact_verdicts: %s is stale (%s) and cannot be confirmed", v.ID, r.Reason)
			}
			p.confirm = append(p.confirm, v.ID)
			continue
		}
		if strings.TrimSpace(v.Reason) == "" {
			return nil, fmt.Errorf("fact_verdicts: rejecting %s needs a reason", v.ID)
		}
		if err := checkText("reason", v.Reason); err != nil {
			return nil, err
		}
		p.reject[v.ID] = v.Reason
	}
	for id := range proposed {
		if !done[id] {
			p.pending = append(p.pending, id)
		}
	}
	sort.Strings(p.pending)
	return p, nil
}

// apply records the verdicts: confirmed facts become verified (by the principal), rejected
// ones are deleted. Problems come back as warnings, since the review itself has succeeded.
func (p *factPlan) apply(ctx context.Context, s *Service, caller string) (warnings []string) {
	m := s.memory()
	for _, id := range p.confirm {
		if _, err := m.Verify(ctx, id, "principal"); err != nil {
			warnings = append(warnings, fmt.Sprintf("fact %s was not verified: %v", id, err))
			continue
		}
		s.factEvent(caller, "memory_confirmed", map[string]any{"fact": id, "task_id": p.task})
	}
	for id, why := range p.reject {
		if err := m.Delete(id); err != nil {
			warnings = append(warnings, fmt.Sprintf("fact %s was not removed: %v", id, err))
			continue
		}
		s.factEvent(caller, "memory_rejected", map[string]any{"fact": id, "task_id": p.task, "reason": why})
	}
	return warnings
}

func (s *Service) factEvent(agent, kind string, data any) {
	b, _ := json.Marshal(data)
	_ = s.Store.AddEvent(agent, kind, string(b))
}

func (p *factPlan) addTo(out map[string]any, warnings []string) {
	if len(p.confirm) > 0 {
		out["facts_confirmed"] = p.confirm
	}
	if len(p.reject) > 0 {
		ids := make([]string, 0, len(p.reject))
		for id := range p.reject {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		out["facts_rejected"] = ids
	}
	if len(p.pending) > 0 {
		out["unanswered_facts"] = p.pending
		warnings = append(warnings, "proposed facts with no verdict (they stay unverified): "+strings.Join(p.pending, ", "))
	}
	if len(warnings) > 0 {
		prev, _ := out["warnings"].([]string)
		out["warnings"] = append(prev, warnings...)
	}
}
