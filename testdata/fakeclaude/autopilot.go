package main

// Autopilot mode (FAKECLAUDE_AUTOPILOT=1): fakeclaude behaves like Claude Code
// as far as slai can observe. On every stdin line it runs the UserPromptSubmit
// hook, pulls its inbox through `slai tool`, does role-specific scripted work,
// then runs the Stop hook. See the per-role functions below.
//
// Environment knobs:
//
//	FAKECLAUDE_SLAI_BIN        path of the slai binary (default: parsed from --settings hook command)
//	FAKECLAUDE_ROLE_<ID>       role override for agent <ID> (upper-cased, '-' -> '_')
//	FAKECLAUDE_DELAY_MS        sleep inside each turn (between the two hooks); _<ID> suffix overrides per agent
//	FAKECLAUDE_NO_RESOLVE      1: engineers ignore merge-conflict notes
//	FAKECLAUDE_INTEGRATION     integration branch name (default slai/integration)
//
// Self-heal scripting (each fires once per agent, on its first assignment / first
// turn, and leaves a marker under .slai/run/<id>/ so a restarted session behaves):
//
//	FAKECLAUDE_SLOW_<ID>=ms    engineer: sleep this long on its first assignment before committing (a mid-task window)
//	FAKECLAUDE_HANG_<ID>=1     engineer: on its first assignment stop responding for good (turn stays "working")
//	FAKECLAUDE_MODAL_<ID>=1    engineer: on its first assignment show a permission prompt, fire the Notification hook and block
//	FAKECLAUDE_DEAF_<ID>=1     any role: the first session never reads input after startup
//	FAKECLAUDE_SWALLOW_<ID>=1  any role: the first typed line is dropped as if the pane swallowed it (no hooks fire)

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type inboxItem struct {
	ID     int64  `json:"id"`
	From   string `json:"from"`
	Kind   string `json:"kind"`
	TaskID string `json:"task_id"`
	Body   string `json:"body"`
}

type boardTask struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Assignee string `json:"assignee"`
	Status   string `json:"status"`
}

type pilot struct {
	lf         *os.File
	id, proj   string
	role       string
	bin        string
	cwd        string
	session    string
	transcript string
	turn       int
	prompt     string // the line that started the current turn
}

func autopilotEnabled() bool { return os.Getenv("FAKECLAUDE_AUTOPILOT") == "1" }

func envID(id string) string { return strings.ToUpper(strings.ReplaceAll(id, "-", "_")) }

// slaiBinFromSettings extracts <bin> from a "<bin> hook stop" command in a Claude settings.json.
func slaiBinFromSettings(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var s struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if json.Unmarshal(b, &s) != nil {
		return ""
	}
	for _, g := range s.Hooks["Stop"] {
		for _, h := range g.Hooks {
			if bin, ok := strings.CutSuffix(h.Command, " hook stop"); ok {
				return bin
			}
		}
	}
	return ""
}

type teamAgent struct{ ID, Role string }

// teamAgents scans .slai/team.yaml (stdlib only) for "- id:" / "role:" pairs.
func teamAgents(proj string) []teamAgent {
	f, err := os.Open(filepath.Join(proj, ".slai", "team.yaml"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []teamAgent
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if v, ok := strings.CutPrefix(l, "- id:"); ok {
			out = append(out, teamAgent{ID: strings.TrimSpace(v)})
		} else if v, ok := strings.CutPrefix(l, "role:"); ok && len(out) > 0 {
			out[len(out)-1].Role = strings.TrimSpace(v)
		}
	}
	return out
}

func runAutopilot(lf *os.File, flags map[string]string) {
	p := &pilot{lf: lf, id: os.Getenv("SLAI_AGENT_ID"), proj: os.Getenv("SLAI_PROJECT")}
	p.cwd, _ = os.Getwd()
	p.bin = os.Getenv("FAKECLAUDE_SLAI_BIN")
	if p.bin == "" {
		p.bin = slaiBinFromSettings(flags["--settings"])
	}
	if p.bin == "" {
		p.bin = "slai"
	}
	p.role = os.Getenv("FAKECLAUDE_ROLE_" + envID(p.id))
	if p.role == "" {
		for _, a := range teamAgents(p.proj) {
			if a.ID == p.id {
				p.role = a.Role
			}
		}
	}
	p.session = fmt.Sprintf("fake-%s-%d", p.id, time.Now().UnixNano())
	p.transcript = filepath.Join(p.proj, ".slai", "run", p.id, "transcript-"+p.session+".jsonl")
	_ = os.MkdirAll(filepath.Dir(p.transcript), 0o755)

	fmt.Printf("fakeclaude autopilot: agent=%s role=%s\n", p.id, p.role)
	startMCP(p.bin) // FAKECLAUDE_MCP=1: a long-lived `slai mcp` like the one real claude spawns
	source := "startup"
	if flags["--continue"] != "" || flags["--resume"] != "" {
		source = "resume"
	}
	p.hook("sessionstart", map[string]any{"source": source})
	p.idlePrompt()
	if p.once("deaf") {
		logRec(lf, record{Event: "deaf"})
		time.Sleep(24 * time.Hour) // never reads stdin: typed lines pile up unread
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		logRec(lf, record{Event: "input", Line: line, Agent: p.id})
		if strings.TrimSpace(line) == "exit" {
			fmt.Println("bye")
			return
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		if p.once("swallow") {
			logRec(lf, record{Event: "swallowed", Line: line, Agent: p.id})
			continue
		}
		p.runTurn(line)
		p.idlePrompt()
	}
}

// once reports whether the named self-heal knob is set for this agent and has not
// fired yet; it records that it fired so a restarted session behaves normally.
func (p *pilot) once(knob string) bool {
	if os.Getenv("FAKECLAUDE_"+strings.ToUpper(knob)+"_"+envID(p.id)) == "" {
		return false
	}
	mark := filepath.Join(p.proj, ".slai", "run", p.id, "fake-"+knob)
	if _, err := os.Stat(mark); err == nil {
		return false
	}
	_ = os.MkdirAll(filepath.Dir(mark), 0o755)
	_ = os.WriteFile(mark, nil, 0o644)
	return true
}

func (p *pilot) idlePrompt() { fmt.Println("? for shortcuts") }

func (p *pilot) delay() time.Duration {
	v := os.Getenv("FAKECLAUDE_DELAY_MS_" + envID(p.id))
	if v == "" {
		v = os.Getenv("FAKECLAUDE_DELAY_MS")
	}
	n, _ := strconv.Atoi(v)
	return time.Duration(n) * time.Millisecond
}

func (p *pilot) runTurn(prompt string) {
	p.turn++
	p.prompt = prompt
	fmt.Println("esc to interrupt")
	p.hook("userpromptsubmit", map[string]any{"prompt": prompt})
	p.appendUsage()
	switch p.role {
	case "orchestrator":
		p.orchestrator()
	case "engineer":
		p.engineer()
	case "principal_engineer":
		p.principal()
	default:
		p.inbox() // other roles just drain their inbox
	}
	time.Sleep(p.delay()) // the turn stays "working" after the inbox was read
	p.appendUsage()
	p.hook("stop", nil)
}

// appendUsage adds an assistant line with usage to the transcript.
func (p *pilot) appendUsage() {
	l := map[string]any{"type": "assistant", "message": map[string]any{
		"id": fmt.Sprintf("msg_%s_%d_%d", p.id, p.turn, time.Now().UnixNano()), "model": "claude-sonnet-4",
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 50, "cache_read_input_tokens": 1000, "cache_creation_input_tokens": 200},
	}}
	b, _ := json.Marshal(l)
	f, err := os.OpenFile(p.transcript, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(b, '\n'))
}

func (p *pilot) hook(event string, extra map[string]any) {
	payload := map[string]any{"session_id": p.session, "transcript_path": p.transcript, "cwd": p.cwd,
		"hook_event_name": event}
	for k, v := range extra {
		payload[k] = v
	}
	b, _ := json.Marshal(payload)
	cmd := exec.Command(p.bin, "hook", event)
	cmd.Stdin = bytes.NewReader(b)
	cmd.Dir = p.cwd
	if out, err := cmd.CombinedOutput(); err != nil {
		logRec(p.lf, record{Event: "hook_error", Line: fmt.Sprintf("%s: %v: %s", event, err, out)})
	}
	logRec(p.lf, record{Event: "hook", Line: event})
}

// tool runs `slai tool <name> <json>` as this agent and returns stdout.
func (p *pilot) tool(name string, args any) ([]byte, error) {
	b, _ := json.Marshal(args)
	cmd := exec.Command(p.bin, "tool", name, string(b))
	cmd.Dir = p.cwd
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	res := strings.TrimSpace(out.String())
	if err != nil {
		res = strings.TrimSpace(errb.String())
	}
	logRec(p.lf, record{Event: "tool", Line: fmt.Sprintf("%s %s => %v %s", name, b, err, res)})
	fmt.Printf("tool %s -> %s\n", name, res)
	return out.Bytes(), err
}

func (p *pilot) inbox() []inboxItem {
	out, err := p.tool("get_inbox", map[string]any{})
	if err != nil {
		return nil
	}
	var r struct {
		Items []inboxItem `json:"items"`
	}
	_ = json.Unmarshal(out, &r)
	return r.Items
}

func (p *pilot) board() []boardTask {
	out, err := p.tool("get_board", map[string]any{})
	if err != nil {
		return nil
	}
	var r struct {
		Tasks []boardTask `json:"tasks"`
	}
	_ = json.Unmarshal(out, &r)
	return r.Tasks
}

// git runs git in dir with a fixed identity.
func (p *pilot) git(dir string, args ...string) (string, error) {
	a := append([]string{"-c", "user.name=fakeclaude", "-c", "user.email=fake@example.com", "-C", dir}, args...)
	out, err := exec.Command("git", a...).CombinedOutput()
	logRec(p.lf, record{Event: "git", Line: fmt.Sprintf("%v => %v %s", args, err, strings.TrimSpace(string(out)))})
	return string(out), err
}

func (p *pilot) integration() string {
	if v := os.Getenv("FAKECLAUDE_INTEGRATION"); v != "" {
		return v
	}
	return "slai/integration"
}

// ---- orchestrator ----

func (p *pilot) orchestrator() {
	items := p.inbox()
	sawNote := false
	for _, it := range items {
		switch it.Kind {
		case "user":
			if strings.Contains(strings.ToLower(it.Body), "build") {
				p.createTasks(it.Body)
			}
		case "note":
			sawNote = true
		}
	}
	if !sawNote {
		return
	}
	tasks := p.board()
	if len(tasks) == 0 {
		return
	}
	var ids []string
	for _, t := range tasks {
		if t.Status != "merged" {
			return
		}
		ids = append(ids, t.ID)
	}
	path := filepath.Join(p.proj, ".slai", "run", "orch-summary.txt")
	_ = os.WriteFile(path, []byte(fmt.Sprintf("all %d tasks merged: %s\n", len(ids), strings.Join(ids, ","))), 0o644)
}

func (p *pilot) createTasks(msg string) {
	var engs []string
	for _, a := range teamAgents(p.proj) {
		if a.Role == "engineer" {
			engs = append(engs, a.ID)
		}
	}
	if len(engs) > 2 {
		engs = engs[:2]
	}
	conflict := strings.Contains(msg, "CONFLICT")
	for i, e := range engs {
		title := fmt.Sprintf("Greeting part %d", i+1)
		if i == 0 && strings.Contains(msg, "REWORK") {
			title += " REWORK"
		}
		desc := fmt.Sprintf("Implement part %d of: %s", i+1, msg)
		file := fmt.Sprintf("feature-%d.txt", i+1)
		if conflict {
			desc += " CONFLICT"
			file = "shared.txt"
		}
		p.tool("create_task", map[string]any{"title": title, "description": desc, "assignee": e, "files": []string{file}})
	}
}

// ---- engineer ----

type taskInfo struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Files       []string `json:"files"`
}

func (p *pilot) getTask(id string) (taskInfo, bool) {
	out, err := p.tool("get_task", map[string]any{"task_id": id})
	var t taskInfo
	if err != nil || json.Unmarshal(out, &t) != nil {
		return t, false
	}
	return t, true
}

func (t taskInfo) file() string {
	if len(t.Files) > 0 {
		return t.Files[0]
	}
	return t.ID + ".txt"
}

func (p *pilot) commit(t taskInfo, msg string) {
	p.git(p.cwd, "add", "-A")
	p.git(p.cwd, "commit", "-m", fmt.Sprintf("[%s] %s", t.ID, msg))
}

// selfHeal plays the scripted failure for this agent's first assignment.
func (p *pilot) selfHeal(t taskInfo) {
	switch {
	case p.once("hang"):
		logRec(p.lf, record{Event: "hang", Line: t.ID})
		time.Sleep(time.Hour) // no output, no hooks: the turn stays "working"
	case p.once("modal"):
		logRec(p.lf, record{Event: "modal", Line: t.ID})
		fmt.Println("Do you want to proceed?")
		fmt.Println("❯ 1. Yes")
		fmt.Println("  2. No")
		p.hook("notification", map[string]any{"notification_type": "permission_prompt", "message": "Claude needs your permission to run a command"})
		time.Sleep(time.Hour)
	case p.once("slow"):
		ms, _ := strconv.Atoi(os.Getenv("FAKECLAUDE_SLOW_" + envID(p.id)))
		logRec(p.lf, record{Event: "slow", Line: t.ID})
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
}

// resumeAfterRestart carries on an in-progress task after slai restarted the
// session (the assignment was already claimed by the previous incarnation).
func (p *pilot) resumeAfterRestart() {
	out, err := p.tool("get_task", map[string]any{})
	var t taskInfo
	if err != nil || json.Unmarshal(out, &t) != nil || t.ID == "" {
		return
	}
	content := fmt.Sprintf("greeting feature for %s by %s (resumed)\n", t.ID, p.id)
	_ = os.WriteFile(filepath.Join(p.cwd, t.file()), []byte(content), 0o644)
	p.commit(t, t.Title)
	p.tool("request_review", map[string]any{"task_id": t.ID, "summary": "implemented " + t.file() + " after restart"})
}

func (p *pilot) engineer() {
	if strings.Contains(p.prompt, "restarted") {
		defer p.resumeAfterRestart()
	}
	for _, it := range p.inbox() {
		switch {
		case it.Kind == "assignment" && it.TaskID != "":
			t, ok := p.getTask(it.TaskID)
			if !ok {
				continue
			}
			p.selfHeal(t)
			content := fmt.Sprintf("greeting feature for %s by %s\n", t.ID, p.id)
			if strings.Contains(t.Description, "CONFLICT") {
				content = fmt.Sprintf("line from %s for %s\n", p.id, t.ID)
			}
			_ = os.WriteFile(filepath.Join(p.cwd, t.file()), []byte(content), 0o644)
			p.commit(t, t.Title)
			p.tool("request_review", map[string]any{"task_id": t.ID, "summary": "implemented " + t.file()})
		case it.Kind == "review" && strings.Contains(it.Body, "hanges") && it.TaskID != "":
			t, ok := p.getTask(it.TaskID)
			if !ok {
				continue
			}
			f, _ := os.OpenFile(filepath.Join(p.cwd, t.file()), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
			if f != nil {
				fmt.Fprintf(f, "reworked after review by %s\n", p.id)
				f.Close()
			}
			p.commit(t, "address review comments")
			p.tool("request_review", map[string]any{"task_id": t.ID, "summary": "addressed review comments"})
		case it.Kind == "note" && strings.Contains(it.Body, "conflicted") && it.TaskID != "":
			if os.Getenv("FAKECLAUDE_NO_RESOLVE") == "1" {
				continue // simulate an engineer that cannot resolve the conflict
			}
			t, ok := p.getTask(it.TaskID)
			if !ok {
				continue
			}
			p.resolveConflict(t)
		case it.Kind == "note" && strings.Contains(it.Body, "was merged") && it.TaskID != "":
			p.tool("report", map[string]any{"task_id": it.TaskID, "status": "done", "summary": "merged, nothing further"})
		}
	}
}

// resolveConflict merges the integration branch into the task branch, keeping
// both sides of every conflicted file, commits, and requests review again.
func (p *pilot) resolveConflict(t taskInfo) {
	if _, err := p.git(p.cwd, "merge", p.integration()); err != nil {
		out, _ := p.git(p.cwd, "diff", "--name-only", "--diff-filter=U")
		for _, f := range strings.Fields(out) {
			path := filepath.Join(p.cwd, f)
			b, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var keep []string
			for _, l := range strings.Split(string(b), "\n") {
				if strings.HasPrefix(l, "<<<<<<<") || strings.HasPrefix(l, "=======") || strings.HasPrefix(l, ">>>>>>>") {
					continue
				}
				keep = append(keep, l)
			}
			_ = os.WriteFile(path, []byte(strings.Join(keep, "\n")), 0o644)
		}
		p.git(p.cwd, "add", "-A")
		p.git(p.cwd, "commit", "--no-edit")
	}
	p.tool("request_review", map[string]any{"task_id": t.ID, "summary": "resolved merge conflict"})
}

// ---- principal ----

func (p *pilot) principal() {
	for _, it := range p.inbox() {
		if it.Kind != "review" || it.TaskID == "" {
			continue
		}
		p.tool("review_diff", map[string]any{"task_id": it.TaskID})
		title := ""
		for _, t := range p.board() {
			if t.ID == it.TaskID {
				title = t.Title
			}
		}
		mark := filepath.Join(p.proj, ".slai", "run", p.id, "reworked-"+it.TaskID)
		if _, err := os.Stat(mark); err != nil && strings.Contains(title, "REWORK") {
			_ = os.WriteFile(mark, nil, 0o644)
			p.tool("request_changes", map[string]any{"task_id": it.TaskID, "comments": "please make some changes: add a rework note"})
			continue
		}
		p.tool("approve", map[string]any{"task_id": it.TaskID, "notes": "lgtm"})
	}
}

// startMCP runs `slai mcp` as a child with an open stdin, as claude does: it
// records its build in the store at startup and runs the binary that was on disk
// when this session started, until the session (and so the pipe) goes away.
func startMCP(bin string) {
	if os.Getenv("FAKECLAUDE_MCP") == "" {
		return
	}
	cmd := exec.Command(bin, "mcp")
	cmd.Stdout, cmd.Stderr = nil, nil
	if _, err := cmd.StdinPipe(); err != nil {
		return
	}
	_ = cmd.Start()
}
