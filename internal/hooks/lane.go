package hooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/oorbt/slai/internal/config"
)

// Decision is a PreToolUse permission decision. Marshal it with HookJSON to
// get the Claude Code hook stdout (hookSpecificOutput.permissionDecision).
type Decision struct {
	Deny   bool
	Reason string
}

// HookJSON renders the Claude Code PreToolUse decision output.
func (d Decision) HookJSON() []byte {
	dec := "allow"
	if d.Deny {
		dec = "deny"
	}
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName":            "PreToolUse",
		"permissionDecision":       dec,
		"permissionDecisionReason": d.Reason,
	}})
	return b
}

// Lane describes where an agent may write.
type Lane struct {
	Role       string
	ProjectDir string
	Worktree   string // engineers only
	Cwd        string // working directory of the tool call

	// vars holds NAME=value assignments seen so far in the Bash command being
	// checked, so `S=/tmp/x; mkdir -p "$S"` can be resolved statically.
	vars map[string]string
}

var editToolNames = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

// CheckLane returns a deny Decision when the tool call falls outside the
// agent's lane (PRD 8), or nil to leave the decision to normal permissions.
func CheckLane(l Lane, tool string, input json.RawMessage) *Decision {
	switch {
	case editToolNames[tool]:
		var in struct {
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
		}
		_ = json.Unmarshal(input, &in)
		p := in.FilePath
		if p == "" {
			p = in.NotebookPath
		}
		if p == "" {
			return nil
		}
		if !l.writable(p) {
			return deny("%s may not write %s: %s", l.Role, p, l.describe())
		}
	case tool == "EnterWorktree":
		return deny("%s may not create its own worktree: slai manages worktrees, and anything written in a copy of the repo never reaches the real project. %s", l.Role, l.describe())
	case tool == "Bash":
		var in struct {
			Command string `json:"command"`
		}
		_ = json.Unmarshal(input, &in)
		if r := l.checkBash(in.Command); r != "" {
			return deny("%s", r)
		}
	}
	return nil
}

func deny(format string, a ...any) *Decision {
	return &Decision{Deny: true, Reason: "slai lane guard: " + fmt.Sprintf(format, a...)}
}

func (l Lane) describe() string {
	switch l.Role {
	case config.RoleEngineer:
		return "writes are limited to your worktree " + l.Worktree
	case config.RolePM:
		return "writes are limited to " + l.inProject(".slai/specs/**") + " — write the spec there, in the real project directory, not in another worktree or copy"
	case config.RolePrincipal:
		return "writes are limited to " + l.inProject(".slai/decisions.md")
	case config.RoleOrchestrator:
		return "this role is read-only: you are the orchestrator, you never write code or edit files, you own getting tasks done. Delegate this with create_task (assign it to an engineer), then follow it on get_board"
	}
	return "this role is read-only"
}

// inProject renders a project-relative path as absolute when the project dir is known.
func (l Lane) inProject(rel string) string {
	if l.ProjectDir == "" {
		return rel
	}
	return filepath.Join(l.ProjectDir, rel)
}

var varName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var varRef = regexp.MustCompile(`\$(?:\{([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*))`)

// expandVars substitutes $VAR and ${VAR} from assignments earlier in the same
// command, then from the hook's own environment. It returns ok=false when any
// variable is unknown (or a `$(...)`-style expansion remains), in which case the
// path cannot be checked ahead of time and must not be treated as a literal path.
func (l Lane) expandVars(p string) (string, bool) {
	ok := true
	out := varRef.ReplaceAllStringFunc(p, func(m string) string {
		sub := varRef.FindStringSubmatch(m)
		name := sub[1]
		if name == "" {
			name = sub[2]
		}
		if v, found := l.vars[name]; found {
			return v
		}
		if v := os.Getenv(name); v != "" {
			return v
		}
		ok = false
		return m
	})
	return out, ok && !strings.Contains(out, "$")
}

func (l Lane) abs(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return "" // cannot resolve statically
	}
	if strings.Contains(p, "$") {
		var ok bool
		if p, ok = l.expandVars(p); !ok {
			return ""
		}
	}
	if !filepath.IsAbs(p) {
		base := l.Cwd
		if base == "" {
			base = l.ProjectDir
		}
		p = filepath.Join(base, p)
	}
	p = filepath.Clean(p)
	// Resolve symlinks on the deepest existing ancestor so links cannot escape.
	rest := ""
	cur := p
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

func within(root, p string) bool {
	if root == "" {
		return false
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	rel, err := filepath.Rel(filepath.Clean(root), p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (l Lane) worktree() string {
	return l.Worktree
}

// writable reports whether path p (absolute or cwd-relative) is in the lane.
// scratchRoots are throwaway locations engineers may use for temp files, test
// fixtures and tool output (outside the project, so they can never be merged).
func scratchRoots() []string {
	roots := []string{"/tmp", os.TempDir()}
	if t := os.Getenv("TMPDIR"); t != "" {
		roots = append(roots, t)
	}
	return roots
}

// claudeScratchRoots are the per-user temp dirs Claude Code itself uses
// (/tmp/claude-<uid>/<project>/<session>/scratchpad/...). Every role may write
// there so reviewers can stage checkouts and probe scripts; it is outside the
// project, so nothing written there can be merged.
func claudeScratchRoots() []string {
	name := fmt.Sprintf("claude-%d", os.Getuid())
	var roots []string
	for _, r := range scratchRoots() {
		roots = append(roots, filepath.Join(r, name))
	}
	return roots
}

func (l Lane) writable(p string) bool {
	a := l.abs(p)
	if a == "" {
		// A path built from $VAR cannot be checked ahead of time. Deny it for the
		// narrowly scoped roles, but don't make engineers thrash on `> $S/x` (a scratch
		// dir held in a variable). The home directory is never a scratch dir, and rm -r
		// (below) stays strict.
		// Safe default: an unresolvable $VAR is allowed for engineers only; every other
		// role is denied rather than guessed at (use an absolute path instead).
		return l.Role == config.RoleEngineer && !strings.HasPrefix(p, "~") && !strings.Contains(p, "$HOME") && !strings.Contains(p, "${HOME}")
	}
	for _, r := range claudeScratchRoots() {
		if within(r, a) && !within(l.ProjectDir, a) {
			return true
		}
	}
	switch l.Role {
	case config.RoleEngineer:
		if within(l.worktree(), a) {
			return true
		}
		for _, r := range scratchRoots() {
			if within(r, a) && !within(l.ProjectDir, a) {
				return true
			}
		}
		return false
	case config.RolePM:
		return within(filepath.Join(l.ProjectDir, ".slai", "specs"), a)
	case config.RolePrincipal:
		d := filepath.Join(l.ProjectDir, ".slai", "decisions.md")
		if r, err := filepath.EvalSymlinks(filepath.Dir(d)); err == nil {
			d = filepath.Join(r, filepath.Base(d))
		}
		return a == d
	}
	return false
}

// shellWords splits a command line into words honouring simple quotes.
func shellWords(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	has := false
	flush := func() {
		if has {
			out = append(out, cur.String())
			cur.Reset()
			has = false
		}
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, has = r, true
		case r == ' ' || r == '\t':
			flush()
		case r == ';' || r == '|' || r == '&' || r == '\n' || r == '(' || r == ')':
			flush()
			out = append(out, ";") // segment separator
		case r == '>':
			flush()
			out = append(out, ">")
		default:
			cur.WriteRune(r)
			has = true
		}
	}
	flush()
	return out
}

var writeCmds = map[string]bool{"mv": true, "cp": true, "touch": true, "tee": true, "mkdir": true, "rmdir": true,
	"truncate": true, "install": true, "ln": true, "chmod": true, "chown": true, "rm": true, "dd": true}

// stripHeredocs removes heredoc bodies and here-string operands from a command so
// they are never tokenised as shell: `<<`, `<<-` and `<<<` are input, not output
// redirects, and a body such as `n=2` is data. Operators are replaced by a space;
// anything after the operator on the same line (`| tee x`, `> y`) is kept and
// still checked. An unterminated heredoc swallows the rest of the command, which
// is what the shell does too.
func stripHeredocs(cmd string) string {
	type pend struct {
		delim string
		tabs  bool
	}
	var out strings.Builder
	var pending []pend
	rs := []rune(cmd)
	var quote rune
	// word reads one (possibly quoted) shell word starting at i.
	word := func(i int) (string, int) {
		var w strings.Builder
		var q rune
		for ; i < len(rs); i++ {
			r := rs[i]
			switch {
			case q != 0:
				if r == q {
					q = 0
				} else {
					w.WriteRune(r)
				}
			case r == '\'' || r == '"':
				q = r
			case r == ' ' || r == '\t' || r == '\n' || r == ';' || r == '|' || r == '&' || r == '<' || r == '>' || r == ')':
				return w.String(), i
			default:
				w.WriteRune(r)
			}
		}
		return w.String(), i
	}
	skipBlanks := func(i int) int {
		for i < len(rs) && (rs[i] == ' ' || rs[i] == '\t') {
			i++
		}
		return i
	}
	for i := 0; i < len(rs); {
		r := rs[i]
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
			out.WriteRune(r)
			i++
		case r == '\'' || r == '"':
			quote = r
			out.WriteRune(r)
			i++
		case r == '<' && i+2 < len(rs) && rs[i+1] == '<' && rs[i+2] == '<':
			_, i = word(skipBlanks(i + 3))
			out.WriteByte(' ')
		case r == '<' && i+1 < len(rs) && rs[i+1] == '<':
			i += 2
			p := pend{}
			if i < len(rs) && rs[i] == '-' {
				p.tabs = true
				i++
			}
			p.delim, i = word(skipBlanks(i))
			pending = append(pending, p)
			out.WriteByte(' ')
		case r == '\n' && len(pending) > 0:
			out.WriteRune(r)
			i++
			for _, p := range pending {
				for i < len(rs) {
					j := i
					for j < len(rs) && rs[j] != '\n' {
						j++
					}
					line := string(rs[i:j])
					if p.tabs {
						line = strings.TrimLeft(line, "\t")
					}
					i = j
					if i < len(rs) {
						i++ // the newline ending this body line
					}
					if line == p.delim {
						break
					}
				}
			}
			pending = nil
		default:
			out.WriteRune(r)
			i++
		}
	}
	return out.String()
}

// checkBash denies obviously destructive or out-of-lane shell commands.
// It is a best-effort static check, not a sandbox.
func (l Lane) checkBash(cmd string) string {
	l.vars = map[string]string{}
	words := shellWords(stripHeredocs(cmd))
	var seg []string
	flush := func() string {
		defer func() { seg = nil }()
		return l.checkSegment(seg)
	}
	for _, w := range words {
		if w == ";" {
			if r := flush(); r != "" {
				return r
			}
			continue
		}
		seg = append(seg, w)
	}
	return flush()
}

func (l Lane) checkSegment(seg []string) string {
	// drop leading VAR=val assignments and sudo/command/env wrappers
	for len(seg) > 0 && (strings.Contains(seg[0], "=") && !strings.HasPrefix(seg[0], "-") || seg[0] == "sudo" || seg[0] == "command" || seg[0] == "env") {
		if name, val, ok := strings.Cut(seg[0], "="); ok && varName.MatchString(name) {
			if v, ok := l.expandVars(val); ok {
				l.vars[name] = v
			}
		}
		seg = seg[1:]
	}
	if len(seg) == 0 {
		return ""
	}
	// redirections: "> target" (also ">> target": the second '>' arrives as its own word)
	for i, w := range seg {
		if w == ">" {
			j := i + 1
			for j < len(seg) && seg[j] == ">" {
				j++
			}
			if j < len(seg) && seg[j] != "&" && !strings.HasPrefix(seg[j], "&") && seg[j] != "/dev/null" && !l.writable(seg[j]) {
				return fmt.Sprintf("%s may not redirect output to %s: %s", l.Role, seg[j], l.describe())
			}
		}
	}
	name := filepath.Base(seg[0])
	args := seg[1:]
	var targets []string
	recursive := false
	for _, a := range args {
		switch {
		case a == ">":
		case strings.HasPrefix(a, "--"):
			if a == "--recursive" {
				recursive = true
			}
		case strings.HasPrefix(a, "-") && len(a) > 1:
			if strings.ContainsAny(a, "rR") {
				recursive = true
			}
		default:
			targets = append(targets, a)
		}
	}
	if name == "rm" && recursive {
		if l.Role != config.RoleEngineer {
			return fmt.Sprintf("%s may not run rm -r/-rf", l.Role)
		}
		for _, t := range targets {
			a := l.abs(t)
			if a == "" || !within(l.worktree(), a) || a == l.abs(l.worktree()) {
				return fmt.Sprintf("rm -rf target %q is not inside your worktree %s", t, l.Worktree)
			}
		}
		return ""
	}
	if l.Role == config.RoleEngineer {
		// engineers: write commands must stay in the worktree (flags like -i are skipped above)
		if writeCmds[name] {
			for _, t := range targets {
				if !looksLikePath(t) {
					continue
				}
				if !l.writable(t) {
					return fmt.Sprintf("%s target %q is outside your worktree %s (temp files under /tmp are fine)", name, t, l.Worktree)
				}
			}
		}
		return ""
	}
	if writeCmds[name] || (name == "sed" && hasFlag(args, "-i")) {
		for _, t := range targets {
			if name == "sed" && !looksLikePath(t) {
				continue
			}
			if !l.writable(t) {
				return fmt.Sprintf("%s may not run %s on %s: %s", l.Role, name, t, l.describe())
			}
		}
	}
	return ""
}

func looksLikePath(s string) bool {
	return strings.ContainsAny(s, "/.") && !strings.ContainsAny(s, "{}$")
}

func hasFlag(args []string, f string) bool {
	for _, a := range args {
		if a == f || strings.HasPrefix(a, f) && !strings.HasPrefix(a, "--") {
			return true
		}
	}
	return false
}

// LaneFor builds the Lane for an agent row.
func LaneFor(role, agentID, worktree, projectDir, cwd string) Lane {
	if role == config.RoleEngineer && worktree == "" {
		worktree = filepath.Join(projectDir, ".slai", "worktrees", agentID)
	}
	if projectDir != "" {
		if r, err := filepath.EvalSymlinks(projectDir); err == nil {
			projectDir = r
		}
	}
	return Lane{Role: role, ProjectDir: projectDir, Worktree: worktree, Cwd: cwd}
}
