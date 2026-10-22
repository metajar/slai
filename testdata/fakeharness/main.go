// Command fakeharness is a stand-in for the `pi` and `opencode` binaries in slai's
// end-to-end tests (build it twice, as pi and as opencode; it picks its personality
// from argv[0]). Like fakeclaude it spends no tokens, but it goes further: it loads
// the REAL generated pi extension / opencode plugin in node and drives its handlers
// the way the real harness would, and it talks to the REAL `slai mcp` over stdio,
// taking the server command, arguments and environment ONLY from the run files:
//
//	pi        the MCP server the extension registers (registerMcpServer)
//	opencode  mcp.slai in the file named by $OPENCODE_CONFIG
//
// Its identity is whatever those say, never $SLAI_AGENT_ID (which the test leaves
// unset or wrong on purpose). Each input line is a turn: lifecycle events, then
// get_inbox, then ping->pong, ask->reply and assignment->get_task+report(done).
//
// Environment (all optional):
//
//	FAKEHARNESS_LOG              JSON-lines record of what happened
//	FAKEHARNESS_NODE             node binary (default: node)
//	FAKEHARNESS_HANG_<ID>=1      engineer: on its first assignment stop responding for good (no stop event)
//	FAKEHARNESS_SLOW_<ID>=ms     engineer: sleep this long on its first assignment before committing
//	FAKEHARNESS_DEAF_<ID>=1      first session never reads input
//	FAKEHARNESS_SWALLOW_<ID>=1   first typed line is dropped (nothing fires)
//	FAKEHARNESS_WRITE_<ID>=path  first turn: try a write tool call to path; the guard must refuse it
//
// A knob fires once per agent and leaves a marker under .slai/run/<id>/ so a
// restarted session behaves normally.
package main

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

//go:embed host.mjs
var hostJS string

type rec map[string]any

var (
	logf   *os.File
	logMu  sync.Mutex
	hostIn *bufio.Writer
)

var agentID string // set once the run files have told us who we are

func logr(r rec) {
	if logf == nil {
		return
	}
	if agentID != "" {
		r["slai_agent_id"] = agentID
	}
	r["time"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(r)
	logMu.Lock()
	defer logMu.Unlock()
	logf.Write(append(b, '\n'))
}

func fail(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	fmt.Println("fakeharness: " + msg)
	logr(rec{"event": "fatal", "error": msg})
	os.Exit(1)
}

type mcpCfg struct {
	Command string
	Args    []string
	Env     map[string]string
}

// ---- node host ----

type host struct {
	cmd    *exec.Cmd
	in     *bufio.Writer
	out    *bufio.Scanner
	nextID int
	hello  struct {
		Kind string
		MCP  *struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcp"`
		Handlers []string
	}
}

func startHost(kind, module, cwd string) *host {
	node := os.Getenv("FAKEHARNESS_NODE")
	if node == "" {
		node = "node"
	}
	dir, err := os.MkdirTemp("", "fakeharness-host-")
	if err != nil {
		fail("%v", err)
	}
	hp := filepath.Join(dir, "host.mjs")
	_ = os.WriteFile(hp, []byte(hostJS), 0o644)
	h := &host{}
	h.cmd = exec.Command(node, hp, kind, module, cwd)
	h.cmd.Stderr = os.Stderr
	in, _ := h.cmd.StdinPipe()
	outp, _ := h.cmd.StdoutPipe()
	h.in = bufio.NewWriter(in)
	h.out = bufio.NewScanner(outp)
	h.out.Buffer(make([]byte, 1<<20), 1<<20)
	if err := h.cmd.Start(); err != nil {
		fail("cannot start node (%s): %v", node, err)
	}
	if !h.out.Scan() {
		fail("node host died while loading %s", module)
	}
	var hello struct {
		Hello json.RawMessage `json:"hello"`
	}
	if err := json.Unmarshal(h.out.Bytes(), &hello); err != nil || hello.Hello == nil {
		fail("bad host hello %q", h.out.Text())
	}
	_ = json.Unmarshal(hello.Hello, &h.hello)
	return h
}

// fire calls a handler/hook of the loaded module and returns its result or error.
func (h *host) fire(name string, args ...any) (result json.RawMessage, errMsg string) {
	h.nextID++
	b, _ := json.Marshal(map[string]any{"id": h.nextID, "op": "fire", "name": name, "args": args})
	h.in.Write(append(b, '\n'))
	h.in.Flush()
	if !h.out.Scan() {
		return nil, "host gone"
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *string         `json:"error"`
	}
	_ = json.Unmarshal(h.out.Bytes(), &r)
	if r.Error != nil {
		errMsg = *r.Error
	}
	return r.Result, errMsg
}

func (h *host) quit() {
	h.in.WriteString("{\"op\":\"quit\"}\n")
	h.in.Flush()
	done := make(chan struct{})
	go func() { _ = h.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = h.cmd.Process.Kill()
	}
}

// ---- argument parsing ----

type launch struct {
	kind, model, session, agentFlag string
	extensions                      []string
	args                            []string
}

func parseArgs(kind string, args []string) launch {
	l := launch{kind: kind, args: args}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch a {
		case "--model", "-m":
			l.model = next()
		case "--session-id", "-s", "--session":
			l.session = next()
		case "--agent":
			l.agentFlag = next()
		case "-e", "--extension":
			l.extensions = append(l.extensions, next())
		case "--append-system-prompt":
			next()
		}
	}
	return l
}

func once(project, id, knob string) bool {
	env := "FAKEHARNESS_" + strings.ToUpper(knob) + "_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
	v := os.Getenv(env)
	if v == "" {
		return false
	}
	mark := filepath.Join(project, ".slai", "run", id, "fakeh-"+strings.ToLower(knob))
	if _, err := os.Stat(mark); err == nil {
		return false
	}
	_ = os.MkdirAll(filepath.Dir(mark), 0o755)
	_ = os.WriteFile(mark, nil, 0o644)
	return true
}

func knobValue(id, knob string) string {
	return os.Getenv("FAKEHARNESS_" + strings.ToUpper(knob) + "_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_")))
}

// ---- the agent ----

type agent struct {
	kind    string
	id      string
	project string
	cwd     string
	session string
	h       *host
	cs      *mcp.ClientSession
	prefix  string // tool-name prefix the harness gives MCP tools
	tools   []string
	turn    int
}

func main() {
	kind := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	if kind != "pi" && kind != "opencode" {
		kind = os.Getenv("FAKEHARNESS_KIND")
	}
	if kind != "pi" && kind != "opencode" {
		fail("run as pi or opencode (argv[0]=%q)", os.Args[0])
	}
	if p := os.Getenv("FAKEHARNESS_LOG"); p != "" {
		logf, _ = os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		defer logf.Close()
	}
	l := parseArgs(kind, os.Args[1:])
	cwd, _ := os.Getwd()

	// Where does the run-file configuration live?
	var module string
	var cfg mcpCfg
	switch kind {
	case "pi":
		for _, e := range l.extensions {
			if !strings.HasPrefix(e, "builtin:") {
				module = e
			}
		}
		if module == "" {
			fail("pi: no -e extension given (the slai extension is the only way the slai MCP server gets registered)")
		}
	case "opencode":
		path := os.Getenv("OPENCODE_CONFIG")
		if path == "" {
			fail("opencode: OPENCODE_CONFIG is not set")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			fail("opencode: %v", err)
		}
		var oc struct {
			MCP map[string]struct {
				Command     []string          `json:"command"`
				Environment map[string]string `json:"environment"`
			} `json:"mcp"`
			Plugin []string `json:"plugin"`
		}
		if err := json.Unmarshal(b, &oc); err != nil {
			fail("opencode: bad config %s: %v", path, err)
		}
		s, ok := oc.MCP["slai"]
		if !ok || len(s.Command) == 0 {
			fail("opencode: no mcp.slai in %s", path)
		}
		cfg = mcpCfg{Command: s.Command[0], Args: s.Command[1:], Env: s.Environment}
		if len(oc.Plugin) == 0 {
			fail("opencode: no plugin in %s", path)
		}
		module = oc.Plugin[0]
	}

	h := startHost(kind, module, cwd)
	defer h.quit()
	if kind == "pi" {
		if h.hello.MCP == nil {
			fail("pi: the extension registered no MCP server")
		}
		cfg = mcpCfg{Command: h.hello.MCP.Command, Args: h.hello.MCP.Args, Env: h.hello.MCP.Env}
	}
	id, project := flagValue(cfg.Args, "--agent"), flagValue(cfg.Args, "--project")
	if id == "" {
		id = cfg.Env["SLAI_AGENT_ID"]
	}
	if project == "" {
		project = cfg.Env["SLAI_PROJECT"]
	}
	if id == "" || project == "" {
		fail("%s: run files do not pin an identity (agent=%q project=%q)", kind, id, project)
	}
	agentID = id
	logr(rec{"event": "start", "kind": kind, "args": os.Args[1:], "cwd": cwd, "model": l.model, "session_flag": l.session,
		"env_agent": os.Getenv("SLAI_AGENT_ID"), "env_project": os.Getenv("SLAI_PROJECT"),
		"agent": id, "project": project, "mcp_command": cfg.Command, "mcp_args": cfg.Args, "handlers": h.hello.Handlers,
		"openCodeAgentFlag": l.agentFlag})

	a := &agent{kind: kind, id: id, project: project, cwd: cwd, h: h, session: l.session}
	a.prefix = "mcp__slai__"
	if kind == "opencode" {
		a.prefix = "slai_"
	}
	a.connectMCP(cfg)
	defer a.cs.Close()
	a.start(l)

	fmt.Printf("fake %s (agent %s)\n", kind, id)
	a.idle()
	if once(project, id, "deaf") {
		logr(rec{"event": "deaf"})
		time.Sleep(24 * time.Hour)
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		logr(rec{"event": "input", "line": line})
		if line == "exit" {
			fmt.Println("bye")
			break
		}
		if line == "" {
			continue
		}
		if once(project, id, "swallow") {
			logr(rec{"event": "swallowed", "line": line})
			continue
		}
		a.runTurn(line)
		a.idle()
	}
	logr(rec{"event": "exit"})
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (a *agent) connectMCP(cfg mcpCfg) {
	cmd := exec.Command(cfg.Command, cfg.Args...)
	cmd.Dir = a.cwd
	cmd.Env = os.Environ()
	for k, v := range cfg.Env { // the harness applies the entry's env over the inherited one
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.Stderr = nil
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "fake-" + a.kind, Version: "0"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		fail("connect to slai mcp (%s %v): %v", cfg.Command, cfg.Args, err)
	}
	a.cs = cs
	tl, err := cs.ListTools(ctx, nil)
	if err == nil {
		var names []string
		for _, t := range tl.Tools {
			names = append(names, t.Name)
		}
		a.tools = names
		logr(rec{"event": "tools", "tools": names})
	}
}

// start fires the session lifecycle the real harness would at launch.
func (a *agent) start(l launch) {
	switch a.kind {
	case "pi":
		if a.session == "" {
			a.session = fmt.Sprintf("fake-pi-%d", time.Now().UnixNano())
		}
		a.fire("session_start", map[string]any{"type": "session_start"}, a.ctx())
	case "opencode":
		if a.session != "" { // resumed with -s: opencode does not announce a new session
			a.fire("event", map[string]any{"event": map[string]any{"type": "session.updated", "properties": map[string]any{"sessionID": a.session}}})
			return
		}
		a.session = fmt.Sprintf("ses_fake%d", time.Now().UnixNano()%1e12)
		a.fire("event", map[string]any{"event": map[string]any{"type": "session.created", "properties": map[string]any{
			"sessionID": a.session, "info": map[string]any{"id": a.session, "directory": a.cwd}}}})
	}
}

func (a *agent) ctx() map[string]any {
	sid := a.session
	return map[string]any{"cwd": a.cwd, "sessionManager": map[string]any{}, "sessionId": sid}
}

func (a *agent) fire(name string, args ...any) (json.RawMessage, string) {
	r, e := a.h.fire(name, args...)
	logr(rec{"event": "fire", "name": name, "error": e})
	return r, e
}

func (a *agent) idle() {
	if a.kind == "pi" {
		fmt.Println("0.0%/1.0M (auto)")
	} else {
		fmt.Println("ctrl+p commands")
	}
}

func (a *agent) working() {
	if a.kind == "pi" {
		fmt.Println("── ⠧ Working ──────────")
	} else {
		fmt.Println("⬝⬝⬝⬝⬝⬝⬝⬝  esc interrupt")
	}
}

// guard fires the tool-call guard for a tool and reports whether it was blocked.
func (a *agent) guard(tool string, input map[string]any) (blocked bool, reason string) {
	switch a.kind {
	case "pi":
		r, e := a.fire("tool_call", map[string]any{"toolName": tool, "input": input}, a.ctx())
		if e != "" {
			return true, e
		}
		var res struct {
			Block  bool   `json:"block"`
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(r, &res)
		return res.Block, res.Reason
	default:
		_, e := a.fire("tool.execute.before", map[string]any{"tool": tool, "sessionID": a.session}, map[string]any{"args": input})
		return e != "", e
	}
}

func (a *agent) toolEnd(tool string) {
	if a.kind == "pi" {
		a.fire("tool_result", map[string]any{"toolName": tool}, a.ctx())
	} else {
		a.fire("tool.execute.after", map[string]any{"tool": tool, "sessionID": a.session})
	}
}

// call runs one slai MCP tool through the harness' guard, like a model tool call.
func (a *agent) call(name string, args map[string]any) (map[string]any, error) {
	full := a.prefix + name
	if blocked, why := a.guard(full, args); blocked {
		logr(rec{"event": "blocked", "tool": full, "reason": why})
		return nil, fmt.Errorf("blocked: %s", why)
	}
	if args == nil {
		args = map[string]any{}
	}
	res, err := a.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	a.toolEnd(full)
	if err != nil {
		logr(rec{"event": "tool", "name": name, "error": err.Error()})
		return nil, err
	}
	var text string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			text += t.Text
		}
	}
	logr(rec{"event": "tool", "name": name, "args": args, "result": text, "is_error": res.IsError})
	if res.IsError {
		return nil, fmt.Errorf("%s", text)
	}
	out := map[string]any{}
	_ = json.Unmarshal([]byte(text), &out)
	return out, nil
}

type taskInfo struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Branch      string   `json:"branch"`
	Files       []string `json:"files"`
}

func (t taskInfo) file() string {
	if len(t.Files) > 0 {
		return t.Files[0]
	}
	return t.ID + ".txt"
}

func (a *agent) getTask(id string) (taskInfo, bool) {
	args := map[string]any{}
	if id != "" {
		args["task_id"] = id
	}
	out, err := a.call("get_task", args)
	var t taskInfo
	if err != nil {
		return t, false
	}
	b, _ := json.Marshal(out)
	_ = json.Unmarshal(b, &t)
	return t, t.ID != ""
}

func (a *agent) git(args ...string) {
	out, err := exec.Command("git", append([]string{"-c", "user.name=fakeharness", "-c", "user.email=fake@example.com", "-C", a.cwd}, args...)...).CombinedOutput()
	logr(rec{"event": "git", "args": args, "error": fmt.Sprint(err), "out": strings.TrimSpace(string(out))})
}

func (a *agent) commit(t taskInfo, msg string) {
	a.git("add", "-A")
	a.git("commit", "-q", "-m", fmt.Sprintf("[%s] %s", t.ID, msg))
}

// selfHeal plays the scripted failure for this agent's first assignment.
func (a *agent) selfHeal(t taskInfo) {
	switch {
	case once(a.project, a.id, "hang"):
		logr(rec{"event": "hang", "line": t.ID})
		time.Sleep(time.Hour) // no stop event: the turn stays "working"
	case once(a.project, a.id, "slow"):
		ms, _ := strconv.Atoi(knobValue(a.id, "slow"))
		logr(rec{"event": "slow", "line": t.ID})
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
}

func (a *agent) has(tool string) bool {
	for _, t := range a.tools {
		if t == tool {
			return true
		}
	}
	return false
}

func (a *agent) runTurn(prompt string) {
	a.turn++
	a.working()
	if a.kind == "pi" {
		a.fire("before_agent_start", map[string]any{"prompt": prompt}, a.ctx())
	} else {
		a.fire("event", map[string]any{"event": map[string]any{"type": "session.status", "properties": map[string]any{"sessionID": a.session, "status": map[string]any{"type": "busy"}}}})
		a.fire("chat.message", map[string]any{"sessionID": a.session}, map[string]any{})
	}
	engineer := a.has("request_review")
	if engineer && strings.Contains(prompt, "restarted") {
		defer a.resumeAfterRestart()
	}

	inbox, err := a.call("get_inbox", nil)
	var items []map[string]any
	if err == nil {
		if l, ok := inbox["items"].([]any); ok {
			for _, it := range l {
				if m, ok := it.(map[string]any); ok {
					items = append(items, m)
				}
			}
		}
	}

	if path := knobValue(a.id, "write"); path != "" && once(a.project, a.id, "write") {
		blocked, why := a.guard("write", map[string]any{"path": path, "filePath": path, "content": "x"})
		logr(rec{"event": "write_attempt", "path": path, "blocked": blocked, "reason": why})
		if !blocked {
			_ = os.WriteFile(path, []byte("written by fakeharness\n"), 0o644)
		}
	}

	for _, it := range items {
		id := int64(it["id"].(float64))
		kind, _ := it["kind"].(string)
		tid, _ := it["task_id"].(string)
		body, _ := it["body"].(string)
		switch {
		case kind == "ping":
			_, _ = a.call("pong", map[string]any{"message_id": id, "agent_id": a.id})
		case kind == "ask":
			_, _ = a.call("reply", map[string]any{"message_id": id, "body": "fake " + a.kind + " answer from " + a.id})
		case engineer && kind == "assignment" && tid != "":
			t, ok := a.getTask(tid)
			if !ok {
				continue
			}
			if t.Branch != "" { // the supervisor has the worktree on the task branch
				a.git("checkout", "-q", "-B", t.Branch)
			}
			a.selfHeal(t)
			_ = os.WriteFile(filepath.Join(a.cwd, t.file()), []byte(fmt.Sprintf("greeting feature for %s by %s\n", t.ID, a.id)), 0o644)
			a.commit(t, t.Title)
			_, _ = a.call("request_review", map[string]any{"task_id": t.ID, "summary": "implemented " + t.file()})
		case engineer && kind == "review" && strings.Contains(body, "hanges") && tid != "":
			if t, ok := a.getTask(tid); ok {
				f, _ := os.OpenFile(filepath.Join(a.cwd, t.file()), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
				if f != nil {
					fmt.Fprintf(f, "reworked after review by %s\n", a.id)
					f.Close()
				}
				a.commit(t, "address review comments")
				_, _ = a.call("request_review", map[string]any{"task_id": t.ID, "summary": "addressed review comments"})
			}
		case engineer && kind == "note" && strings.Contains(body, "was merged") && tid != "":
			_, _ = a.call("report", map[string]any{"task_id": tid, "status": "done", "summary": "merged, nothing further"})
		case !engineer && kind == "assignment" && tid != "":
			_, _ = a.getTask(tid)
			_, _ = a.call("report", map[string]any{"task_id": tid, "status": "blocked", "summary": "fake " + a.kind + " has no role script"})
		}
	}

	if !engineer && once(a.project, a.id, "hang") {
		logr(rec{"event": "hang"})
		time.Sleep(24 * time.Hour)
	}
	a.stop()
}

// resumeAfterRestart carries on an in-progress task after slai restarted the session.
func (a *agent) resumeAfterRestart() {
	t, ok := a.getTask("")
	if !ok {
		return
	}
	_ = os.WriteFile(filepath.Join(a.cwd, t.file()), []byte(fmt.Sprintf("greeting feature for %s by %s (resumed)\n", t.ID, a.id)), 0o644)
	a.commit(t, t.Title)
	_, _ = a.call("request_review", map[string]any{"task_id": t.ID, "summary": "implemented " + t.file() + " after restart"})
}

func (a *agent) stop() {
	if a.kind == "pi" {
		a.fire("message_end", map[string]any{"message": map[string]any{"role": "assistant", "usage": map[string]any{
			"input": 100, "output": 10, "cacheRead": 50, "cost": map[string]any{"total": 0.001}}}}, a.ctx())
		a.fire("agent_settled", map[string]any{}, a.ctx())
		return
	}
	a.fire("event", map[string]any{"event": map[string]any{"type": "session.status", "properties": map[string]any{"sessionID": a.session, "status": map[string]any{"type": "idle"}}}})
	a.fire("event", map[string]any{"event": map[string]any{"type": "session.idle", "properties": map[string]any{"sessionID": a.session}}})
}
