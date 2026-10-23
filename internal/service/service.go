// Package service implements the slai team tools (PRD 6.1) on top of the
// store and git layers. The MCP server and `slai tool` are thin adapters.
package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/harness"
	"github.com/oorbt/slai/internal/roles"
	"github.com/oorbt/slai/internal/store"
)

const (
	MaxText         = 4000
	MaxDiffLines    = 2000
	MaxReviewRounds = 3
)

// Service holds everything a tool call needs.
type Service struct {
	Store *store.Store
	Git   *git.Repo
	Team  *config.Team
	Dir   string

	// Now is the clock the board reads (default time.Now); tests set it.
	Now func() time.Time

	live     atomic.Pointer[config.Team] // team.yaml as last reloaded; nil until it changes
	teamMu   sync.Mutex
	teamStat string // size+mtime of the team.yaml last read
}

func (s *Service) clock() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// team is the current team definition. A long-lived MCP server sees agents added
// to team.yaml while it runs (refreshTeam, called per tool call).
func (s *Service) team() *config.Team {
	if t := s.live.Load(); t != nil {
		return t
	}
	return s.Team
}

// refreshTeam reloads team.yaml when its size or mtime changed and it is valid;
// an invalid edit leaves the previous team in place.
func (s *Service) refreshTeam() {
	if s.Dir == "" {
		return
	}
	path := s.Dir + "/.slai/team.yaml"
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	sig := fmt.Sprintf("%d/%d", fi.Size(), fi.ModTime().UnixNano())
	s.teamMu.Lock()
	defer s.teamMu.Unlock()
	if s.teamStat == "" {
		s.teamStat = sig // the team this process loaded at startup
		return
	}
	if sig == s.teamStat {
		return
	}
	s.teamStat = sig
	if t, err := config.Load(path); err == nil {
		s.live.Store(t)
	}
}

// ---- tool argument types (shared with the MCP adapter) ----

type StatusFilter struct {
	Status string `json:"status,omitempty" jsonschema:"only tasks with this status"`
}
type TaskRef struct {
	TaskID string `json:"task_id" jsonschema:"task id such as T-0001"`
}

// GetTaskArgs is TaskRef with an optional id: empty means "my current task(s)".
type GetTaskArgs struct {
	TaskID string `json:"task_id,omitempty" jsonschema:"task id such as T-0001; omit to get your current open task(s)"`
}
type CreateTaskArgs struct {
	Title       string   `json:"title" jsonschema:"short task title"`
	Description string   `json:"description" jsonschema:"what to build and acceptance criteria; reference files for long specs"`
	Assignee    string   `json:"assignee" jsonschema:"agent id that will do the task"`
	ParentID    string   `json:"parent_id,omitempty" jsonschema:"parent task id"`
	SpecPath    string   `json:"spec_path,omitempty" jsonschema:"path to a spec file"`
	Files       []string `json:"files,omitempty" jsonschema:"files the task is expected to touch"`
	DependsOn   []string `json:"depends_on,omitempty" jsonschema:"task ids that must merge (or be cancelled) before this one can start; the assignee is told when they have"`
}
type FinishArgs struct {
	Summary string `json:"summary" jsonschema:"final summary for the user: what was built, where it lives, how to run it, what was not verified"`
	// AcceptUnmetACs finishes although a merged task has a failing or unanswered acceptance
	// criterion; the summary must say why. Use it only when the user has accepted the gap.
	AcceptUnmetACs bool `json:"accept_unmet_acs,omitempty" jsonschema:"set true only if the user accepted that a merged task has a failing or unanswered acceptance criterion; the summary must say so"`
}

type UpdateTaskArgs struct {
	TaskID      string   `json:"task_id" jsonschema:"task id"`
	Title       string   `json:"title,omitempty" jsonschema:"new title"`
	Description string   `json:"description,omitempty" jsonschema:"new description"`
	Assignee    string   `json:"assignee,omitempty" jsonschema:"new assignee agent id"`
	Files       []string `json:"files,omitempty" jsonschema:"new file list"`
	Status      string   `json:"status,omitempty" jsonschema:"only \"in_progress\": unblock a blocked task and tell its assignee"`
	DependsOn   []string `json:"depends_on,omitempty" jsonschema:"replace the task's dependencies (task ids that must merge or be cancelled first)"`
	SpecPath    string   `json:"spec_path,omitempty" jsonschema:"new spec file path (the spec is re-hashed from now on)"`
}
type CancelTaskArgs struct {
	TaskID string `json:"task_id" jsonschema:"task id"`
	Reason string `json:"reason" jsonschema:"why the task is cancelled"`
}
type ReportArgs struct {
	TaskID  string `json:"task_id,omitempty" jsonschema:"task id; omit when you are doing message-driven work with no board task"`
	Status  string `json:"status" jsonschema:"done or blocked"`
	Summary string `json:"summary" jsonschema:"short summary; reference files for detail"`
}

// ACClaimArg is the engineer's answer for one acceptance criterion of the task's spec.
type ACClaimArg struct {
	AC   string `json:"ac" jsonschema:"the criterion, as numbered in the spec (for example \"AC 3\" or \"3\")"`
	Met  bool   `json:"met" jsonschema:"true if your branch meets it"`
	Note string `json:"note,omitempty" jsonschema:"how: the test or file that shows it, or why it is not met"`
}

// ACVerdictArg is the reviewer's answer for one acceptance criterion.
type ACVerdictArg struct {
	AC   string `json:"ac" jsonschema:"the criterion, as the engineer's claim names it"`
	Pass bool   `json:"pass" jsonschema:"true if the branch meets it"`
	Note string `json:"note,omitempty" jsonschema:"why it fails, or what you checked"`
}

type RequestReviewArgs struct {
	TaskID   string       `json:"task_id" jsonschema:"task id"`
	Summary  string       `json:"summary" jsonschema:"what changed and how it was tested"`
	ACClaims []ACClaimArg `json:"ac_claims,omitempty" jsonschema:"one entry per acceptance criterion of the task's spec, each met or not met; required when the task has a spec"`
	Facts    []string     `json:"facts,omitempty" jsonschema:"ids of facts you proposed with remember during this task; the principal verifies or rejects them when reviewing"`
}
type ApproveArgs struct {
	TaskID     string         `json:"task_id" jsonschema:"task id"`
	Notes      string         `json:"notes,omitempty" jsonschema:"optional approval notes"`
	ACVerdicts []ACVerdictArg `json:"ac_verdicts,omitempty" jsonschema:"pass or fail for each acceptance criterion the engineer claimed (spec'd tasks); claims without a verdict are listed back to you"`
	// SpecRewriteOK approves although the spec file was rewritten (not just appended to) since assignment.
	FactVerdicts  []FactVerdictArg `json:"fact_verdicts,omitempty" jsonschema:"confirm or reject each fact the engineer proposed with request_review; only approve can verify a fact"`
	HeadMovedOK   bool             `json:"head_moved_ok,omitempty" jsonschema:"set true to approve the branch head although it differs from the commit your last review_diff showed; the approval then covers the current head. Prefer calling review_diff again"`
	SpecRewriteOK bool             `json:"spec_rewrite_ok,omitempty" jsonschema:"set true to approve although the task's spec file was edited or deleted (not just appended to) after assignment; the rewrite is recorded"`
}
type RequestChangesArgs struct {
	TaskID       string           `json:"task_id" jsonschema:"task id"`
	Comments     string           `json:"comments" jsonschema:"what must change"`
	ACVerdicts   []ACVerdictArg   `json:"ac_verdicts,omitempty" jsonschema:"pass or fail for each acceptance criterion the engineer claimed (spec'd tasks)"`
	FactVerdicts []FactVerdictArg `json:"fact_verdicts,omitempty" jsonschema:"reject proposed facts (confirm:false with a reason); facts can only be confirmed by approve"`
}
type SendMessageArgs struct {
	To   string `json:"to" jsonschema:"agent id or all"`
	Body string `json:"body" jsonschema:"message text, at most 4000 chars"`
}
type AskArgs struct {
	To       string `json:"to" jsonschema:"agent id"`
	Question string `json:"question" jsonschema:"question text, at most 4000 chars"`
}
type ReplyArgs struct {
	MessageID int64  `json:"message_id" jsonschema:"id of the ask message being answered"`
	Body      string `json:"body" jsonschema:"answer text, at most 4000 chars"`
}
type AskUserArgs struct {
	Question string `json:"question" jsonschema:"question for the human user"`
}

// ---- helpers ----

func (s *Service) role(caller string) (string, bool) {
	for _, a := range s.team().Agents {
		if a.ID == caller {
			return a.Role, true
		}
	}
	return "", false
}

func (s *Service) agentExists(id string) bool {
	_, ok := s.role(id)
	return ok
}

// checkAssignee: tasks are for agents that can act on them. Engineers build and go
// through review; the PM writes specs. The orchestrator, principal and focus group
// have no way to start or close a task, so a task assigned to them would hang forever.
func (s *Service) checkAssignee(id string) error {
	role, ok := s.role(id)
	if !ok {
		return fmt.Errorf("assignee %q is not an agent in this team", id)
	}
	if role != config.RoleEngineer && role != config.RolePM {
		return fmt.Errorf("assignee %q is a %s and cannot work a task: assign tasks to an engineer, or to the product manager for a spec. (To get a review, the engineer calls request_review and the principal is notified automatically; to get feedback from a persona, ask the product manager.)", id, role)
	}
	return nil
}

func (s *Service) agentsWithRole(role string) []string {
	var out []string
	for _, a := range s.team().Agents {
		if a.Role == role {
			out = append(out, a.ID)
		}
	}
	return out
}

// UserCaller is the identity of the person at the keyboard (the web UI, like the TUI and CLI,
// acts as "user"). It is not an agent: it may call only UserTools, the orchestrator's task
// tools, and never approves, requests changes, merges or reviews.
const UserCaller = "user"

// UserTools are the tools UserCaller may use: creating, editing (the web only unblocks) and
// cancelling tasks.
var UserTools = []string{"create_task", "update_task", "cancel_task"}

// Authorize checks that caller exists and its role may use tool.
func (s *Service) Authorize(caller, tool string) error {
	if caller == UserCaller {
		for _, t := range UserTools {
			if t == tool {
				return nil
			}
		}
		return fmt.Errorf("tool %q is not available to the user: only %s are", tool, strings.Join(UserTools, ", "))
	}
	role, ok := s.role(caller)
	if !ok {
		return fmt.Errorf("unknown agent %q", caller)
	}
	for _, t := range roles.ToolNames(role) {
		if t == tool {
			return nil
		}
	}
	return fmt.Errorf("tool %q is not allowed for role %s", tool, role)
}

// summarize renders args as compact JSON, truncating long strings (except
// keep-listed keys, which hold review comments that history must retain).
func summarize(args any) string {
	b, err := json.Marshal(args)
	if err != nil {
		return "{}"
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return string(b)
	}
	for k, v := range m {
		if str, ok := v.(string); ok && k != "comments" && utf8.RuneCountInString(str) > 300 {
			m[k] = string([]rune(str)[:300]) + "..."
		}
	}
	out, _ := json.Marshal(m)
	return string(out)
}

// begin authorizes the call and records its events row.
func (s *Service) begin(caller, tool string, args any) error {
	s.refreshTeam()
	if err := s.Authorize(caller, tool); err != nil {
		if _, known := s.role(caller); known {
			s.flagIdentity(caller, "role_denial", "called "+tool+", which its role may not use")
		}
		return err
	}
	s.touch(caller)
	return s.Store.AddEvent(caller, tool, summarize(args))
}

func checkText(field, v string) error {
	if n := utf8.RuneCountInString(v); n > MaxText {
		return fmt.Errorf("%s is too long (%d > %d chars): write it to a file in the repo and reference the path instead", field, n, MaxText)
	}
	return nil
}

// CheckText holds a text field to MaxText runes; the error says how to shorten it. The web
// layer calls it so it refuses what the MCP tools would.
func CheckText(field, v string) error { return checkText(field, v) }

func checkTexts(kv ...string) error {
	for i := 0; i+1 < len(kv); i += 2 {
		if err := checkText(kv[i], kv[i+1]); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) task(id string) (store.Task, error) {
	if id == "" {
		return store.Task{}, errors.New("task_id is required")
	}
	t, err := s.Store.GetTask(id)
	if errors.Is(err, store.ErrNotFound) {
		return t, fmt.Errorf("task %s not found", id)
	}
	return t, err
}

func (s *Service) notify(from, to, kind, taskID, body string) error {
	_, err := s.Store.AddMessage(store.Message{From: from, To: to, Kind: kind, TaskID: taskID, Body: body})
	return err
}

func (s *Service) notifyRole(from, role, kind, taskID, body string) error {
	for _, id := range s.agentsWithRole(role) {
		if id == from {
			continue
		}
		if err := s.notify(from, id, kind, taskID, body); err != nil {
			return err
		}
	}
	return nil
}

func branchFor(assignee, taskID string) string { return "slai/" + assignee + "/" + taskID }

type taskView struct {
	ID        string   `json:"id"`
	ParentID  string   `json:"parent_id,omitempty"`
	Title     string   `json:"title"`
	Assignee  string   `json:"assignee"`
	Status    string   `json:"status"`
	Branch    string   `json:"branch,omitempty"`
	DependsOn []string `json:"depends_on,omitempty"`
	WaitingOn []string `json:"waiting_on,omitempty"` // dependencies not yet merged or cancelled
	Priority  int      `json:"priority"`             // higher first; 0 is the default
}

func view(t store.Task) taskView {
	return taskView{ID: t.ID, ParentID: t.ParentID, Title: t.Title, Assignee: t.Assignee, Status: t.Status, Branch: t.Branch, DependsOn: t.DependsOn, Priority: t.Priority}
}

// viewWithDeps is view plus waiting_on, read from the store.
func (s *Service) viewWithDeps(t store.Task) taskView {
	v := view(t)
	if len(t.DependsOn) > 0 {
		v.WaitingOn, _ = s.Store.WaitingOn(t.ID)
	}
	return v
}

// flagIdentity records that caller may be confused about who it is and queues its identity line
// (store.RecordIdentityCorrection). Best effort: it never fails the call that tripped it.
func (s *Service) flagIdentity(caller, signal, detail string) {
	if caller == UserCaller || caller == "" {
		return
	}
	_, _ = s.Store.RecordIdentityCorrection(caller, signal, detail, roles.IdentityLineFor(s.team(), caller), s.clock())
}

// speaksAsOther returns the id of another team member (not the recipient) that body opens with
// as a signature ("eng-2: ...", "[eng-2] ...", "**eng-2** - ..."), or "". Opening with a
// teammate's id suggests the sender has adopted that agent's voice. Weak signal: it only
// flags, it never refuses.
func (s *Service) speaksAsOther(caller, to, body string) string {
	for _, ag := range s.team().Agents {
		if ag.ID == caller || ag.ID == to {
			continue
		}
		re := regexp.MustCompile(`^[\s\[@*_` + "`" + `]*` + regexp.QuoteMeta(ag.ID) + `[\s*_` + "`" + `]*(?::|\]|-|\x{2014}|\x{2013})`)
		if re.MatchString(body) {
			return ag.ID
		}
	}
	return ""
}

func (s *Service) requireAssignee(caller string, t store.Task) error {
	if t.Assignee != caller {
		s.flagIdentity(caller, "foreign_task", fmt.Sprintf("acted on %s, which is assigned to %s", t.ID, t.Assignee))
		return fmt.Errorf("task %s is assigned to %s, not %s", t.ID, t.Assignee, caller)
	}
	return nil
}

// ---- tools ----

type InboxItem struct {
	ID      int64  `json:"id"`
	From    string `json:"from"`
	Kind    string `json:"kind"`
	TaskID  string `json:"task_id,omitempty"`
	ReplyTo int64  `json:"reply_to,omitempty"`
	Body    string `json:"body"`
	At      int64  `json:"at"`
}

func (s *Service) GetInbox(ctx context.Context, caller string) (any, error) {
	if err := s.begin(caller, "get_inbox", struct{}{}); err != nil {
		return nil, err
	}
	msgs, err := s.Store.ClaimInbox(caller)
	if err != nil {
		return nil, err
	}
	// Items about higher-priority tasks come first (stable: with no priorities set the
	// order is unchanged, and items with no task count as priority 0).
	prio := s.Store.TaskPriorities()
	sort.SliceStable(msgs, func(i, j int) bool { return prio[msgs[i].TaskID] > prio[msgs[j].TaskID] })
	// an identity correction is read before anything else
	sort.SliceStable(msgs, func(i, j int) bool {
		return msgs[i].Kind == store.IdentityMessageKind && msgs[j].Kind != store.IdentityMessageKind
	})
	items := make([]InboxItem, 0, len(msgs))
	for _, m := range msgs {
		items = append(items, InboxItem{m.ID, m.From, m.Kind, m.TaskID, m.ReplyTo, m.Body, m.CreatedAt})
	}
	return map[string]any{"items": items}, nil
}

func (s *Service) GetBoard(ctx context.Context, caller string, a StatusFilter) (any, error) {
	if err := s.begin(caller, "get_board", a); err != nil {
		return nil, err
	}
	ts, err := s.Store.ListTasks(a.Status)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].Priority > ts[j].Priority }) // highest first; ties keep id order
	out := make([]taskView, 0, len(ts))
	for _, t := range ts {
		out = append(out, s.viewWithDeps(t))
	}
	agents := []agentView{}
	var ids []string
	build := CurrentBuild(s.Store)
	now := s.clock()
	gates := NewGateSource(s.Store, GateConfigOf(s.team()), now) // once per board, not once per agent
	for _, ta := range s.team().Agents {
		ids = append(ids, ta.ID)
		v := s.livenessWith(ta.ID, now, gates)
		v.Role = ta.Role
		v.StaleBuild = StaleBuild(s.Store, ta.ID, build)
		setHarness(&v, ta)
		agents = append(agents, v)
	}
	return map[string]any{"tasks": out, "agents": agents, "supervisor": s.supervisorView(time.Now()), "build": build,
		"wake_gate": gates.Wake(ids)}, nil
}

// SupervisorDownAfter is how stale the supervisor heartbeat may be before it
// reads as down. A tick that runs a git merge or polls many panes can take
// several seconds, so this is generous.
const SupervisorDownAfter = 15 * time.Second

type supervisorView struct {
	Running        bool  `json:"running"`
	HeartbeatAgeS  int64 `json:"heartbeat_age_s"`
	HeartbeatKnown bool  `json:"heartbeat_known"`
}

func (s *Service) supervisorView(now time.Time) supervisorView {
	at, ok, _ := s.Store.SupervisorHeartbeat()
	if !ok {
		return supervisorView{}
	}
	age := max(now.Unix()-at, 0)
	return supervisorView{Running: time.Duration(age)*time.Second <= SupervisorDownAfter, HeartbeatAgeS: age, HeartbeatKnown: true}
}

type HistoryItem struct {
	At    int64           `json:"at"`
	Agent string          `json:"agent"`
	Kind  string          `json:"kind"`
	Data  json.RawMessage `json:"data"`
}

func (s *Service) GetTask(ctx context.Context, caller string, a GetTaskArgs) (any, error) {
	if err := s.begin(caller, "get_task", a); err != nil {
		return nil, err
	}
	if a.TaskID == "" {
		open, err := s.openTasksOf(caller)
		if err != nil {
			return nil, err
		}
		switch len(open) {
		case 0:
			return map[string]any{"task": nil, "message": "no task assigned to you; you are doing message-driven work, so report() without a task_id when finished"}, nil
		case 1:
			a.TaskID = open[0].ID
		default:
			list := []map[string]any{}
			for _, t := range open {
				list = append(list, map[string]any{"id": t.ID, "title": t.Title, "status": t.Status, "branch": t.Branch})
			}
			return map[string]any{"tasks": list, "message": "several open tasks; call get_task with one task_id"}, nil
		}
	}
	t, err := s.task(a.TaskID)
	if err != nil {
		return nil, err
	}
	if err := s.requireAssignee(caller, t); err != nil {
		return nil, err
	}
	// First read (or re-read after review feedback) starts/resumes work.
	if t.Status == store.StatusAssigned || t.Status == store.StatusChangesRequested {
		if err := s.Store.UpdateTaskStatus(t.ID, store.StatusInProgress); err != nil {
			return nil, err
		}
		t.Status = store.StatusInProgress
	}
	var files []string
	_ = json.Unmarshal([]byte(t.Files), &files)
	evs, err := s.Store.TaskEvents(t.ID, 50)
	if err != nil {
		return nil, err
	}
	history := []HistoryItem{}
	comments := []map[string]any{}
	for _, e := range evs {
		if e.Kind == "get_task" || e.Kind == "get_board" {
			continue
		}
		data := json.RawMessage(e.Data)
		history = append(history, HistoryItem{e.At, e.Agent, e.Kind, data})
		if e.Kind == "request_changes" {
			var d struct {
				Comments string `json:"comments"`
			}
			_ = json.Unmarshal(data, &d)
			comments = append(comments, map[string]any{"at": e.At, "by": e.Agent, "comments": d.Comments})
		}
	}
	out := map[string]any{
		"id": t.ID, "parent_id": t.ParentID, "title": t.Title, "description": t.Description,
		"assignee": t.Assignee, "created_by": t.CreatedBy, "status": t.Status,
		"spec_path": s.absPath(t.SpecPath), "branch": t.Branch, "files": files,
		"review_rounds": t.ReviewRounds, "review_comments": comments, "history": history,
	}
	// The team's memory for this task rides along in the response (never typed into the
	// pane). A memory problem must not stop an agent from reading its task.
	if p, err := s.packFor(ctx, caller, &t, ""); err == nil {
		out["memory"] = p.Text
	}
	return out, nil
}

// openTasksOf returns tasks assigned to agent that are not yet finished.
func (s *Service) openTasksOf(agent string) ([]store.Task, error) {
	all, err := s.Store.ListTasks("")
	if err != nil {
		return nil, err
	}
	var out []store.Task
	for _, t := range all {
		if t.Assignee != agent {
			continue
		}
		switch t.Status {
		case store.StatusMerged, store.StatusDone, store.StatusCancelled, store.StatusReverted:
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// absPath makes a project-relative path absolute (specs are untracked, so they
// only exist under the project root, not in engineer worktrees).
func (s *Service) absPath(p string) string {
	if p == "" || filepath.IsAbs(p) || s.Dir == "" {
		return p
	}
	return filepath.Join(s.Dir, p)
}

func (s *Service) CreateTask(ctx context.Context, caller string, a CreateTaskArgs) (any, error) {
	if err := s.begin(caller, "create_task", a); err != nil {
		return nil, err
	}
	if a.Title == "" {
		return nil, errors.New("title is required")
	}
	if err := checkTexts("title", a.Title, "description", a.Description); err != nil {
		return nil, err
	}
	if err := s.checkAssignee(a.Assignee); err != nil {
		return nil, err
	}
	if a.ParentID != "" {
		if _, err := s.task(a.ParentID); err != nil {
			return nil, fmt.Errorf("parent_id: %w", err)
		}
	}
	files, _ := json.Marshal(append([]string{}, a.Files...))
	t, err := s.Store.CreateTask(store.Task{
		ParentID: a.ParentID, Title: a.Title, Description: a.Description, Assignee: a.Assignee,
		CreatedBy: caller, SpecPath: a.SpecPath, Files: string(files),
	})
	if err != nil {
		return nil, err
	}
	s.recordSpec(t.ID, t.SpecPath)
	if len(a.DependsOn) > 0 {
		if err := s.Store.SetTaskDeps(t.ID, a.DependsOn); err != nil {
			_ = s.Store.UpdateTaskStatus(t.ID, store.StatusCancelled) // do not leave a half-made task
			return nil, err
		}
	}
	if role, _ := s.role(a.Assignee); role == config.RoleEngineer { // only code tasks have a branch
		t.Branch = branchFor(a.Assignee, t.ID)
		if err := s.Store.UpdateTaskFields(t.ID, store.Task{Branch: t.Branch}); err != nil {
			return nil, err
		}
	}
	_ = s.Store.DeleteKV("run.done") // new work reopens a finished run
	body := fmt.Sprintf("New task %s: %s. Call get_task for details.", t.ID, t.Title)
	waiting, _ := s.Store.WaitingOn(t.ID)
	if len(waiting) > 0 {
		// blocked, not assigned: assigned counts as owed work and would be nagged
		if err := s.Store.UpdateTaskStatus(t.ID, store.StatusBlocked); err != nil {
			return nil, err
		}
		t.Status = store.StatusBlocked
		body += fmt.Sprintf(" It is blocked on %s: do not start until you get a note that they merged.", strings.Join(waiting, ", "))
	}
	if err := s.notify(caller, a.Assignee, "assignment", t.ID, body); err != nil {
		return nil, err
	}
	return map[string]any{"task_id": t.ID, "status": t.Status, "branch": t.Branch, "depends_on": a.DependsOn, "waiting_on": waiting}, nil
}

func (s *Service) UpdateTask(ctx context.Context, caller string, a UpdateTaskArgs) (any, error) {
	if err := s.begin(caller, "update_task", a); err != nil {
		return nil, err
	}
	if err := checkTexts("title", a.Title, "description", a.Description); err != nil {
		return nil, err
	}
	t, err := s.task(a.TaskID)
	if err != nil {
		return nil, err
	}
	if t.Status == store.StatusMerged || t.Status == store.StatusCancelled || t.Status == store.StatusReverted {
		return nil, fmt.Errorf("task %s is %s and cannot be edited", t.ID, t.Status)
	}
	if a.Assignee != "" {
		if err := s.checkAssignee(a.Assignee); err != nil {
			return nil, err
		}
	}
	f := store.Task{Title: a.Title, Description: a.Description, Assignee: a.Assignee}
	if a.Assignee != "" && a.Assignee != t.Assignee {
		// the branch belongs to the assignee's worktree; a new owner needs its own
		if role, _ := s.role(a.Assignee); role == config.RoleEngineer {
			f.Branch = branchFor(a.Assignee, t.ID)
		}
	}
	if a.Status != "" {
		if a.Status != store.StatusInProgress || t.Status != store.StatusBlocked {
			return nil, fmt.Errorf("status can only be set to %q on a blocked task (task %s is %s); use cancel_task to cancel", store.StatusInProgress, t.ID, t.Status)
		}
		if err := s.Store.UpdateTaskStatus(t.ID, store.StatusInProgress); err != nil {
			return nil, err
		}
		who := t.Assignee
		if a.Assignee != "" {
			who = a.Assignee
		}
		_ = s.notify(caller, who, "note", t.ID, fmt.Sprintf("Task %s is unblocked and back in progress. %s Continue now.", t.ID, a.Description))
	}
	if a.DependsOn != nil {
		if err := s.Store.SetTaskDeps(t.ID, a.DependsOn); err != nil {
			return nil, err
		}
		if waiting, _ := s.Store.WaitingOn(t.ID); len(waiting) > 0 && t.Status != store.StatusBlocked {
			if err := s.Store.UpdateTaskStatus(t.ID, store.StatusBlocked); err != nil {
				return nil, err
			}
			_ = s.notify(caller, t.Assignee, "note", t.ID, fmt.Sprintf("Task %s is blocked on %s: stop and wait for a note that they merged.", t.ID, strings.Join(waiting, ", ")))
		}
	}
	if a.Files != nil {
		b, _ := json.Marshal(a.Files)
		f.Files = string(b)
	}
	f.SpecPath = a.SpecPath
	if err := s.Store.UpdateTaskFields(t.ID, f); err != nil {
		return nil, err
	}
	if a.SpecPath != "" && a.SpecPath != t.SpecPath {
		s.recordSpec(t.ID, a.SpecPath) // a new spec: hash it afresh
	}
	if a.Assignee != "" && a.Assignee != t.Assignee {
		_ = s.notify(caller, a.Assignee, "assignment", t.ID, fmt.Sprintf("Task %s (%s) was assigned to you. Call get_task for details.", t.ID, t.Title))
		_ = s.notify(caller, t.Assignee, "note", t.ID, fmt.Sprintf("Task %s was reassigned to %s.", t.ID, a.Assignee))
	}
	nt, _ := s.Store.GetTask(t.ID)
	return s.viewWithDeps(nt), nil
}

func (s *Service) CancelTask(ctx context.Context, caller string, a CancelTaskArgs) (any, error) {
	if err := s.begin(caller, "cancel_task", a); err != nil {
		return nil, err
	}
	if err := checkText("reason", a.Reason); err != nil {
		return nil, err
	}
	t, err := s.task(a.TaskID)
	if err != nil {
		return nil, err
	}
	if err := s.Store.UpdateTaskStatus(t.ID, store.StatusCancelled); err != nil {
		return nil, err
	}
	if t.Assignee != "" && t.Assignee != caller {
		_ = s.notify(caller, t.Assignee, "note", t.ID, fmt.Sprintf("Task %s cancelled: %s", t.ID, a.Reason))
	}
	return map[string]any{"task_id": t.ID, "status": store.StatusCancelled}, nil
}

func (s *Service) Report(ctx context.Context, caller string, a ReportArgs) (any, error) {
	if err := s.begin(caller, "report", a); err != nil {
		return nil, err
	}
	if a.Status != "done" && a.Status != "blocked" {
		return nil, errors.New(`status must be "done" or "blocked"`)
	}
	if err := checkText("summary", a.Summary); err != nil {
		return nil, err
	}
	if a.TaskID == "" {
		// Resolve like get_task(): an agent with an open board task reports on it,
		// so the engineer review guard and status transitions still apply.
		open, err := s.openTasksOf(caller)
		if err != nil {
			return nil, err
		}
		switch len(open) {
		case 0:
		case 1:
			a.TaskID = open[0].ID
		default:
			ids := make([]string, len(open))
			for i, t := range open {
				ids[i] = t.ID
			}
			return nil, fmt.Errorf("you have several open tasks (%s); pass task_id", strings.Join(ids, ", "))
		}
	}
	if a.TaskID == "" {
		// message-driven work: no board task, just tell the orchestrator
		body := fmt.Sprintf("%s reports %s: %s", caller, a.Status, a.Summary)
		if err := s.notifyRole(caller, config.RoleOrchestrator, "note", "", body); err != nil {
			return nil, err
		}
		return map[string]any{"reported": a.Status, "task_id": ""}, nil
	}
	t, err := s.task(a.TaskID)
	if err != nil {
		return nil, err
	}
	if err := s.requireAssignee(caller, t); err != nil {
		return nil, err
	}
	role, _ := s.role(caller)
	switch {
	case a.Status == "blocked" && t.Status != store.StatusBlocked:
		if err := s.Store.UpdateTaskStatus(t.ID, store.StatusBlocked); err != nil {
			return nil, err
		}
		t.Status = store.StatusBlocked
	case a.Status == "done" && role == config.RoleEngineer:
		// Code is done only once it is reviewed and merged. report(done) on its own
		// would leave the task open forever while the orchestrator believes it is finished.
		if t.Status != store.StatusInReview && t.Status != store.StatusMerged {
			return nil, fmt.Errorf("task %s is %s, not in review: commit your work on %s and call request_review first (report(done) does not send work to review)", t.ID, t.Status, t.Branch)
		}
	case a.Status == "done":
		// non-code work (a PM spec) is finished by reporting it
		if t.Status == store.StatusAssigned {
			if err := s.Store.UpdateTaskStatus(t.ID, store.StatusInProgress); err != nil {
				return nil, err
			}
			t.Status = store.StatusInProgress
		}
		if t.Status == store.StatusInProgress {
			if err := s.Store.UpdateTaskStatus(t.ID, store.StatusDone); err != nil {
				return nil, err
			}
			t.Status = store.StatusDone
		}
	}
	body := fmt.Sprintf("%s reports %s %s: %s", caller, t.ID, a.Status, a.Summary)
	if err := s.notifyRole(caller, config.RoleOrchestrator, "note", t.ID, body); err != nil {
		return nil, err
	}
	return map[string]any{"task_id": t.ID, "reported": a.Status, "status": t.Status}, nil
}

func (s *Service) integration() string { return s.team().Project.IntegrationBranch }

func (s *Service) RequestReview(ctx context.Context, caller string, a RequestReviewArgs) (any, error) {
	if err := s.begin(caller, "request_review", a); err != nil {
		return nil, err
	}
	if err := checkText("summary", a.Summary); err != nil {
		return nil, err
	}
	t, err := s.task(a.TaskID)
	if err != nil {
		return nil, err
	}
	if err := s.requireAssignee(caller, t); err != nil {
		return nil, err
	}
	if t.Status != store.StatusInProgress && t.Status != store.StatusBlocked && t.Status != store.StatusInReview {
		return nil, fmt.Errorf("task %s is %s; it must be in_progress (or blocked) to request review (call get_task first if it is assigned)", t.ID, t.Status)
	}
	if waiting, _ := s.Store.WaitingOn(t.ID); len(waiting) > 0 {
		return nil, fmt.Errorf("task %s waits on %s; request review after it merges and you have rebased", t.ID, strings.Join(waiting, ", "))
	}
	claims, err := acClaims(a.ACClaims)
	if err != nil {
		return nil, err
	}
	if t.SpecPath != "" && len(claims) == 0 {
		return nil, fmt.Errorf("task %s has a spec (%s): list its acceptance criteria in ac_claims, one entry each, met or not met, for example [{\"ac\":\"AC 1\",\"met\":true,\"note\":\"TestFoo\"},{\"ac\":\"AC 2\",\"met\":false,\"note\":\"why\"}]. The reviewer answers pass/fail per criterion", t.ID, t.SpecPath)
	}
	if a.Facts != nil {
		if _, err := s.proposeFacts(caller, t.ID, a.Facts); err != nil {
			return nil, err
		}
	}
	has, err := s.Git.HasCommits(ctx, s.integration(), t.Branch)
	if err != nil {
		return nil, fmt.Errorf("cannot inspect branch %s: %w", t.Branch, err)
	}
	if !has {
		return nil, fmt.Errorf("branch %s has no commits beyond %s: commit your work before requesting review", t.Branch, s.integration())
	}
	if t.Status == store.StatusBlocked {
		// the work is done: no orchestrator unblock hop needed
		if err := s.Store.UpdateTaskStatus(t.ID, store.StatusInProgress); err != nil {
			return nil, err
		}
	}
	if len(claims) > 0 { // before the status change: a refusal below must leave no claims behind
		if _, err := s.Store.SetACClaims(t.ID, claims); err != nil {
			return nil, err
		}
	}
	if t.Status != store.StatusInReview { // a re-request (new commits while in review) keeps the status
		if err := s.Store.UpdateTaskStatus(t.ID, store.StatusInReview); err != nil {
			return nil, err
		}
	}
	// The review is of exactly this commit: approval pins it, and the merge takes that
	// commit and nothing the branch gained afterwards.
	head, err := s.Git.RevParse(ctx, t.Branch)
	if err != nil {
		return nil, fmt.Errorf("cannot read the head of %s: %w", t.Branch, err)
	}
	_ = s.Store.SetKV(kvReviewHead+t.ID, head)
	_ = s.Store.DeleteKV(kvReviewSeen + t.ID)
	if !s.team().Defaults.RequireReview {
		if err := s.mergeRequested(caller, t.ID, "review not required: "+a.Summary, head); err != nil {
			return nil, err
		}
		out := map[string]any{"task_id": t.ID, "status": "merge_queued", "head_sha": head}
		if w := s.missingTestWarning(ctx, head, claims); w != nil {
			out["warnings"] = w
		}
		return out, nil
	}
	body := fmt.Sprintf("%s requests review of %s (%s) at commit %s: %s", caller, t.ID, t.Title, shortSHA(head), a.Summary)
	if len(claims) > 0 {
		body += "\n" + claimSummary(claims)
	}
	if len(a.Facts) > 0 {
		body += "\nProposed facts (verify with fact_verdicts on approve, or reject): " + strings.Join(a.Facts, ", ")
	}
	if err := s.notifyRole(caller, config.RolePrincipal, "review", t.ID, body); err != nil {
		return nil, err
	}
	out := map[string]any{"task_id": t.ID, "status": store.StatusInReview, "head_sha": head}
	if w := s.missingTestWarning(ctx, head, claims); w != nil {
		out["warnings"] = w
	}
	return out, nil
}

// KV keys pinning what was reviewed (+ task id).
const (
	kvReviewHead = "review.head." // the branch head when the engineer called request_review
	kvReviewSeen = "review.seen." // the branch head when the reviewer last called review_diff
	// kvReviewDiffed is like kvReviewSeen but survives a new request_review, so approve can
	// tell "I reviewed an older commit than the one the engineer re-requested".
	kvReviewDiffed = "review.diffed."
)

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func (s *Service) mergeRequested(agent, taskID, notes, sha string) error {
	b, _ := json.Marshal(map[string]string{"task_id": taskID, "notes": notes, "sha": sha})
	return s.Store.AddEvent(agent, "merge_requested", string(b))
}

func (s *Service) ReviewDiff(ctx context.Context, caller string, a TaskRef) (any, error) {
	if err := s.begin(caller, "review_diff", a); err != nil {
		return nil, err
	}
	t, err := s.task(a.TaskID)
	if err != nil {
		return nil, err
	}
	if t.Branch == "" {
		return nil, fmt.Errorf("task %s has no branch", t.ID)
	}
	d, err := s.Git.Diff(ctx, s.integration(), t.Branch, MaxDiffLines)
	if err != nil {
		return nil, err
	}
	files := d.Files
	if files == nil {
		files = []string{}
	}
	if len(files) == 0 && t.Status != store.StatusInReview {
		return nil, fmt.Errorf("task %s is %s and %s has no commits yet: nothing to review. The engineer calls request_review when ready, and you will get a review item in your inbox then", t.ID, t.Status, t.Branch)
	}
	// Name what the diff is against so reviewers don't compare with main by mistake.
	base, _ := s.Git.MergeBase(ctx, s.integration(), t.Branch)
	tip, _ := s.Git.RevParse(ctx, s.integration())
	reviewNo, claims, err := s.Store.LatestACClaims(t.ID)
	if err != nil {
		return nil, err
	}
	if claims == nil {
		claims = []store.ACClaim{}
	}
	// approve pins the commit this diff was of; the merge takes exactly that one.
	head, _ := s.Git.RevParse(ctx, t.Branch)
	if head != "" {
		_ = s.Store.SetKV(kvReviewSeen+t.ID, head)
		_ = s.Store.SetKV(kvReviewDiffed+t.ID, head)
	}
	// The reviewer's tokens from here until approve/request_changes count toward this task
	// (per_task_usd). Never take over a task the reviewer is itself assigned.
	// A charge that only waits for the Stop hook (an earlier verdict this turn) is taken over:
	// the whole turn then lands on the newest reviewed task.
	cur := s.Store.AgentTask(caller)
	if pend, _, _ := s.Store.GetKV(store.KVReviewEndCharge + caller); cur == "" || (pend != "" && cur == pend) {
		_ = s.Store.ChargeAgentTo(caller, t.ID)
		_ = s.Store.DeleteKV(store.KVReviewEndCharge + caller)
	}
	// the reviewed commit, checked out where the reviewer may run its tests
	reviewDir, reviewErr := "", error(nil)
	if head != "" {
		reviewDir, reviewErr = s.reviewCheckout(ctx, head)
	}
	out := map[string]any{"task_id": t.ID, "branch": t.Branch, "integration_branch": s.integration(), "base_sha": base,
		"integration_sha": tip, "reviewed_sha": head, "files": files, "truncated": d.Truncated, "patch": d.Patch,
		"spec_path": t.SpecPath, "review_no": reviewNo, "ac_claims": claims}
	if reviewDir != "" {
		out["review_checkout"], out["review_checkout_sha"] = reviewDir, head
		out["review_checkout_note"] = "run go test / go vet / go build here, in this directory: it is the exact commit under review, detached and recreated by every review_diff; do not use the engineer's worktree"
	} else if reviewErr != nil {
		out["review_checkout_error"] = reviewErr.Error()
	}
	for k, v := range s.specNote(caller, t, s.checkSpec(t), false) {
		out[k] = v
	}
	if w := s.missingTestWarning(ctx, head, claims); w != nil {
		out["warnings"] = w
	}
	return out, nil
}

// endReviewSpend ends the charge of the reviewer's tokens to taskID (set by review_diff), but
// not yet: usage rows are written when the turn ends, so the current task stays set until the
// Stop hook (or the supervisor's idle fallback) has recorded this turn's usage.
func (s *Service) endReviewSpend(reviewer, taskID string) {
	if s.Store.AgentTask(reviewer) == taskID {
		_ = s.Store.SetKV(store.KVReviewEndCharge+reviewer, taskID)
	}
}

func (s *Service) Approve(ctx context.Context, caller string, a ApproveArgs) (any, error) {
	if err := s.begin(caller, "approve", a); err != nil {
		return nil, err
	}
	if err := checkText("notes", a.Notes); err != nil {
		return nil, err
	}
	t, err := s.task(a.TaskID)
	if err != nil {
		return nil, err
	}
	if t.Status != store.StatusInReview {
		return nil, fmt.Errorf("task %s is %s, not in_review", t.ID, t.Status)
	}
	// What is approved is a commit: the one review_diff showed (else the one the
	// engineer asked to have reviewed). If the branch has moved since, the reviewer has
	// not seen what would merge, so refuse now rather than at merge time. This comes
	// before the verdicts are stored: a refused approve records nothing.
	pinned, _, _ := s.Store.GetKV(kvReviewSeen + t.ID)
	if pinned == "" {
		pinned, _, _ = s.Store.GetKV(kvReviewHead + t.ID)
	}
	head, _ := s.Git.RevParse(ctx, t.Branch)
	if a.HeadMovedOK && head != "" {
		pinned = head
	}
	if pinned != "" && head != "" && head != pinned {
		return nil, fmt.Errorf("%s has new commits since the review you saw (reviewed %s, branch is now at %s): call review_diff again and approve what it shows", t.Branch, shortSHA(pinned), shortSHA(head))
	}
	if diffed, _, _ := s.Store.GetKV(kvReviewDiffed + t.ID); diffed != "" && head != "" && head != diffed && !a.HeadMovedOK {
		return nil, fmt.Errorf("%s is at %s but the last review_diff you ran showed %s: you have not reviewed what would merge. Call review_diff again and approve what it shows, or approve with head_moved_ok: true to accept the current head", t.Branch, shortSHA(head), shortSHA(diffed))
	}
	if pinned == "" {
		pinned = head // nothing recorded (a task from before pinning): pin what is there now
	}
	// The criteria are in the spec file: if it was rewritten (not just appended to) since
	// the task was assigned, the engineer may have been judged against different words.
	spec := s.checkSpec(t)
	if spec.State == specRewritten {
		b, _ := json.Marshal(map[string]any{"task_id": t.ID, "approver": caller, "reason": spec.Reason, "override": a.SpecRewriteOK})
		_ = s.Store.AddEvent(caller, "spec_rewritten", string(b))
		if !a.SpecRewriteOK {
			return nil, fmt.Errorf("task %s: %s. Acceptance criteria may have been changed to fit the build. Read the spec, and if the change is fine approve again with spec_rewrite_ok: true (the override is recorded)", t.ID, spec.Reason)
		}
		s.recordSpec(t.ID, t.SpecPath) // accepted: this is the baseline from now on
	}
	plan, err := s.planFactVerdicts(ctx, t, a.FactVerdicts, true)
	if err != nil {
		return nil, err
	}
	res, err := s.recordVerdicts(t, caller, a.ACVerdicts)
	if err != nil {
		return nil, err
	}
	if failed := failedACs(res.verdicts); len(failed) > 0 {
		res.warnings = append(res.warnings, "approved although these criteria were marked fail: "+strings.Join(failed, ", "))
	}
	if err := s.mergeRequested(caller, t.ID, a.Notes, pinned); err != nil {
		return nil, err
	}
	s.endReviewSpend(caller, t.ID)
	out := map[string]any{"status": "merge_queued", "approved_sha": pinned}
	for k, v := range s.specNote(caller, t, spec, a.SpecRewriteOK) {
		out[k] = v
	}
	res.addTo(out)
	plan.addTo(out, plan.apply(ctx, s, caller))
	return out, nil
}

func (s *Service) RequestChanges(ctx context.Context, caller string, a RequestChangesArgs) (any, error) {
	if err := s.begin(caller, "request_changes", a); err != nil {
		return nil, err
	}
	if a.Comments == "" {
		return nil, errors.New("comments are required")
	}
	if err := checkText("comments", a.Comments); err != nil {
		return nil, err
	}
	t, err := s.task(a.TaskID)
	if err != nil {
		return nil, err
	}
	if t.Status != store.StatusInReview {
		return nil, fmt.Errorf("task %s is %s, not in_review", t.ID, t.Status)
	}
	plan, err := s.planFactVerdicts(ctx, t, a.FactVerdicts, false)
	if err != nil {
		return nil, err
	}
	verd, err := s.recordVerdicts(t, caller, a.ACVerdicts)
	if err != nil {
		return nil, err
	}
	if err := s.Store.UpdateTaskStatus(t.ID, store.StatusChangesRequested); err != nil {
		return nil, err
	}
	s.endReviewSpend(caller, t.ID)
	nt, err := s.task(t.ID)
	if err != nil {
		return nil, err
	}
	if nt.ReviewRounds >= MaxReviewRounds {
		if err := s.Store.UpdateTaskStatus(t.ID, store.StatusBlocked); err != nil {
			return nil, err
		}
		q := fmt.Sprintf("Task %s (%s) hit %d review rounds without approval and is blocked. Latest comments from %s: %s",
			t.ID, t.Title, nt.ReviewRounds, caller, a.Comments)
		if err := s.notifyRole(caller, config.RoleOrchestrator, "note", t.ID, q); err != nil {
			return nil, err
		}
		_ = s.notify(caller, t.Assignee, "note", t.ID, fmt.Sprintf("Task %s is blocked after %d review rounds. Stop and wait for the orchestrator to unblock or reassign it; don't keep resubmitting.", t.ID, nt.ReviewRounds))
		b, _ := json.Marshal(map[string]string{"question": q, "task_id": t.ID})
		if err := s.Store.AddEvent(caller, "ask_user", string(b)); err != nil {
			return nil, err
		}
		out := map[string]any{"task_id": t.ID, "status": store.StatusBlocked, "review_rounds": nt.ReviewRounds}
		verd.addTo(out)
		return out, nil
	}
	body := fmt.Sprintf("Changes requested on %s (round %d): %s", t.ID, nt.ReviewRounds, a.Comments)
	if line := verdictSummary(verd.verdicts); line != "" {
		body += "\n" + line
	}
	if err := s.notify(caller, t.Assignee, "review", t.ID, body); err != nil {
		return nil, err
	}
	out := map[string]any{"task_id": t.ID, "status": store.StatusChangesRequested, "review_rounds": nt.ReviewRounds}
	for k, v := range s.specNote(caller, t, s.checkSpec(t), false) {
		out[k] = v
	}
	verd.addTo(out)
	rejected := &factPlan{task: plan.task, reject: plan.reject}
	rejected.addTo(out, rejected.apply(ctx, s, caller))
	return out, nil
}

func (s *Service) SendMessage(ctx context.Context, caller string, a SendMessageArgs) (any, error) {
	if err := s.begin(caller, "send_message", a); err != nil {
		return nil, err
	}
	if a.Body == "" {
		return nil, errors.New("body is required")
	}
	if err := checkText("body", a.Body); err != nil {
		return nil, err
	}
	if other := s.speaksAsOther(caller, a.To, a.Body); other != "" {
		s.flagIdentity(caller, "speaks_as_other", "a message to "+a.To+" opens with "+other)
	}
	var to []string
	if a.To == "all" {
		for _, ag := range s.team().Agents {
			if ag.ID != caller && ag.Role != config.RoleFocusGroup { // personas answer asks only
				to = append(to, ag.ID)
			}
		}
	} else {
		if !s.agentExists(a.To) {
			return nil, fmt.Errorf("unknown recipient %q", a.To)
		}
		to = []string{a.To}
	}
	for _, id := range to {
		if err := s.notify(caller, id, "note", "", a.Body); err != nil {
			return nil, err
		}
	}
	return map[string]any{"sent": len(to)}, nil
}

func (s *Service) Ask(ctx context.Context, caller string, a AskArgs) (any, error) {
	if err := s.begin(caller, "ask", a); err != nil {
		return nil, err
	}
	if a.Question == "" {
		return nil, errors.New("question is required")
	}
	if err := checkText("question", a.Question); err != nil {
		return nil, err
	}
	if other := s.speaksAsOther(caller, a.To, a.Question); other != "" {
		s.flagIdentity(caller, "speaks_as_other", "a question to "+a.To+" opens with "+other)
	}
	if !s.agentExists(a.To) || a.To == caller {
		return nil, fmt.Errorf("unknown recipient %q", a.To)
	}
	id, err := s.Store.AddMessage(store.Message{From: caller, To: a.To, Kind: "ask", Body: a.Question})
	if err != nil {
		return nil, err
	}
	return map[string]any{"message_id": id}, nil
}

func (s *Service) Reply(ctx context.Context, caller string, a ReplyArgs) (any, error) {
	if err := s.begin(caller, "reply", a); err != nil {
		return nil, err
	}
	if a.Body == "" {
		return nil, errors.New("body is required")
	}
	if err := checkText("body", a.Body); err != nil {
		return nil, err
	}
	m, err := s.Store.GetMessage(a.MessageID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fmt.Errorf("message %d not found", a.MessageID)
	}
	if err != nil {
		return nil, err
	}
	if m.To != caller {
		return nil, fmt.Errorf("message %d was not addressed to %s", m.ID, caller)
	}
	if other := s.speaksAsOther(caller, m.From, a.Body); other != "" {
		s.flagIdentity(caller, "speaks_as_other", "a reply to "+m.From+" opens with "+other)
	}
	id, err := s.Store.AddMessage(store.Message{From: caller, To: m.From, Kind: "reply", ReplyTo: m.ID, TaskID: m.TaskID, Body: a.Body})
	if err != nil {
		return nil, err
	}
	return map[string]any{"message_id": id, "delivered_to": m.From, "in_reply_to": m.ID}, nil
}

// Finish is how the orchestrator declares the user's request complete. It refuses
// while tasks are still open, records a run_done event (shown in the dashboard and
// `slai status`), and stops the driver's completion reminders until new work arrives.
func (s *Service) Finish(ctx context.Context, caller string, a FinishArgs) (any, error) {
	if err := s.begin(caller, "finish", a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.Summary) == "" {
		return nil, errors.New("summary is required: what was built, where it lives, how to run it, what was not verified")
	}
	if err := checkText("summary", a.Summary); err != nil {
		return nil, err
	}
	tasks, err := s.Store.ListTasks("")
	if err != nil {
		return nil, err
	}
	var open []string
	for _, t := range tasks {
		if t.Status != store.StatusMerged && t.Status != store.StatusCancelled && t.Status != store.StatusDone && t.Status != store.StatusReverted {
			open = append(open, fmt.Sprintf("%s [%s] %s", t.ID, t.Status, t.Title))
		}
	}
	if len(open) > 0 {
		return nil, fmt.Errorf("%d task(s) are still open, so the request is not finished: %s. Finish, unblock or cancel them first", len(open), strings.Join(open, "; "))
	}
	// Done is checked against what the reviewers recorded, not against the engineers'
	// and orchestrator's say-so: every merged task that has a spec must have all of its
	// latest criteria answered, and answered pass.
	var unmet []string
	merged, withoutSpec := 0, 0
	for _, t := range tasks {
		if t.Status != store.StatusMerged {
			continue
		}
		merged++
		if t.SpecPath == "" {
			withoutSpec++
			continue
		}
		vs, err := s.Store.LatestACVerdicts(t.ID)
		if err != nil {
			return nil, err
		}
		for _, v := range vs {
			if !v.Pass {
				unmet = append(unmet, fmt.Sprintf("%s: %s marked fail", t.ID, v.AC))
			}
		}
		un, err := s.Store.UnansweredClaims(t.ID)
		if err != nil {
			return nil, err
		}
		for _, ac := range un {
			unmet = append(unmet, fmt.Sprintf("%s: %s claimed but never answered by a reviewer", t.ID, ac))
		}
	}
	if len(unmet) > 0 && !a.AcceptUnmetACs {
		return nil, fmt.Errorf("%d acceptance criteria of merged tasks are not met: %s. Have the criterion fixed under a follow-up task and re-reviewed, or, only if the user accepts the gap, call finish again with accept_unmet_acs: true and say so in the summary", len(unmet), strings.Join(unmet, "; "))
	}
	if len(unmet) > 0 {
		b, _ := json.Marshal(map[string]any{"unmet": unmet})
		_ = s.Store.AddEvent(caller, "finish_accepted_unmet", string(b))
	}
	_ = s.Store.SetKV("run.done", a.Summary)
	_ = s.Store.SetKV(KVRunStart, strconv.FormatInt(s.clock().Unix(), 10)) // the next run's spend starts here
	return map[string]any{"status": "finished", "tasks": len(tasks), "merged": merged, "merged_without_spec": withoutSpec}, nil
}

func (s *Service) AskUser(ctx context.Context, caller string, a AskUserArgs) (any, error) {
	if err := s.begin(caller, "ask_user", a); err != nil { // events row kind=ask_user {question}
		return nil, err
	}
	if a.Question == "" {
		return nil, errors.New("question is required")
	}
	if err := checkText("question", a.Question); err != nil {
		return nil, err
	}
	if err := s.Store.SetAgentStatus(caller, "waiting_on_user"); err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	return map[string]any{"status": "waiting_on_user"}, nil
}

// Tools lists every tool name this package implements.
var Tools = []string{"get_inbox", "get_board", "get_task", "create_task", "update_task", "cancel_task", "report",
	"request_review", "review_diff", "approve", "request_changes", "send_message", "ask", "reply", "ask_user", "finish", "ping", "pong",
	"remember", "recall", "get_memory", "whoami", "clear_context"}

// Call dispatches a tool by name with raw JSON arguments (used by `slai tool`).
func (s *Service) Call(ctx context.Context, caller, tool string, raw json.RawMessage) (any, error) {
	if err := s.Authorize(caller, tool); err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = json.RawMessage("{}")
	}
	dec := func(v any) error {
		d := json.NewDecoder(strings.NewReader(string(raw)))
		d.DisallowUnknownFields()
		if err := d.Decode(v); err != nil {
			return fmt.Errorf("invalid arguments for %s: %w", tool, err)
		}
		return nil
	}
	switch tool {
	case "whoami":
		return s.Whoami(ctx, caller) // takes no arguments: whatever was passed is ignored
	case "clear_context":
		var a ClearContextArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.ClearContext(ctx, caller, a)
	case "get_inbox":
		return s.GetInbox(ctx, caller)
	case "get_board":
		var a StatusFilter
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.GetBoard(ctx, caller, a)
	case "get_task":
		var a GetTaskArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.GetTask(ctx, caller, a)
	case "create_task":
		var a CreateTaskArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.CreateTask(ctx, caller, a)
	case "update_task":
		var a UpdateTaskArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.UpdateTask(ctx, caller, a)
	case "cancel_task":
		var a CancelTaskArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.CancelTask(ctx, caller, a)
	case "report":
		var a ReportArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.Report(ctx, caller, a)
	case "request_review":
		var a RequestReviewArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.RequestReview(ctx, caller, a)
	case "review_diff":
		var a TaskRef
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.ReviewDiff(ctx, caller, a)
	case "approve":
		var a ApproveArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.Approve(ctx, caller, a)
	case "request_changes":
		var a RequestChangesArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.RequestChanges(ctx, caller, a)
	case "send_message":
		var a SendMessageArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.SendMessage(ctx, caller, a)
	case "ping":
		var a PingArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.Ping(ctx, caller, a)
	case "pong":
		var a PongArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.Pong(ctx, caller, a)
	case "ask":
		var a AskArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.Ask(ctx, caller, a)
	case "reply":
		var a ReplyArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.Reply(ctx, caller, a)
	case "finish":
		var a FinishArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.Finish(ctx, caller, a)
	case "remember":
		var a RememberArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.Remember(ctx, caller, a)
	case "recall":
		var a RecallArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.Recall(ctx, caller, a)
	case "get_memory":
		var a GetMemoryArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.GetMemory(ctx, caller, a)
	case "ask_user":
		var a AskUserArgs
		if err := dec(&a); err != nil {
			return nil, err
		}
		return s.AskUser(ctx, caller, a)
	}
	return nil, fmt.Errorf("unknown tool %q", tool)
}

// Open loads the project at dir (team.yaml + state.db) into a Service.
func Open(dir string) (*Service, error) {
	team, err := config.Load(dir + "/.slai/team.yaml")
	if err != nil {
		return nil, err
	}
	stat := ""
	if fi, err := os.Stat(dir + "/.slai/team.yaml"); err == nil {
		stat = fmt.Sprintf("%d/%d", fi.Size(), fi.ModTime().UnixNano())
	}
	st, err := store.Open(dir + "/.slai/state.db")
	if err != nil {
		return nil, err
	}
	return &Service{Store: st, Git: &git.Repo{Dir: dir}, Team: team, Dir: dir, teamStat: stat}, nil
}

// setHarness fills the harness fields of a board entry from the agent's config.
func setHarness(v *agentView, a config.Agent) {
	kind := harness.Kind(a.HarnessKind())
	v.Harness = string(kind)
	h, err := harness.For(kind)
	if err != nil {
		return
	}
	f := h.Features()
	v.Degraded = f.Degraded()
	if f.Statusline {
		v.Gauges = "ok"
	} else {
		v.Gauges, v.GaugesReason = "n/a", "5h/weekly gauges are not supported by "+string(kind)
	}
}
