// Package tmux wraps the tmux CLI via os/exec.
package tmux

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Client runs tmux commands, optionally against a private server socket (-L).
type Client struct {
	// Socket is the optional socket name passed as -L. Empty uses the default server.
	Socket string
	// Bin is the tmux binary; defaults to "tmux".
	Bin string
}

// New returns a Client using the given socket name (may be empty).
func New(socket string) *Client { return &Client{Socket: socket} }

func (c *Client) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "tmux"
}

func (c *Client) args(a ...string) []string {
	if c.Socket != "" {
		return append([]string{"-L", c.Socket}, a...)
	}
	return a
}

func (c *Client) run(ctx context.Context, a ...string) (string, error) {
	cmd := exec.CommandContext(ctx, c.bin(), c.args(a...)...)
	cmd.Env = EnvWithout(os.Environ(), "TMUX")
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("tmux %s: %w: %s", strings.Join(a, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// SessionName returns slai_<short project hash>_<agentID>.
func SessionName(projectPath, agentID string) string {
	sum := sha1.Sum([]byte(projectPath))
	return "slai_" + hex.EncodeToString(sum[:])[:6] + "_" + agentID
}

// NewSession creates a detached session running cmd (shell command; empty = default shell).
func (c *Client) NewSession(ctx context.Context, name, cwd string, env map[string]string, cmd string) error {
	a := []string{"new-session", "-d", "-s", name, "-x", "200", "-y", "50"}
	if cwd != "" {
		a = append(a, "-c", cwd)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a = append(a, "-e", k+"="+env[k])
	}
	if cmd != "" {
		a = append(a, cmd)
	}
	if _, err := c.run(ctx, a...); err != nil {
		return err
	}
	// Claude Code nags "tmux focus-events off" otherwise; best effort, server-wide.
	_, _ = c.run(ctx, "set-option", "-g", "focus-events", "on")
	return nil
}

// HasSession reports whether the session exists.
func (c *Client) HasSession(ctx context.Context, name string) (bool, error) {
	cmd := exec.CommandContext(ctx, c.bin(), c.args("has-session", "-t", "="+name)...)
	cmd.Env = EnvWithout(os.Environ(), "TMUX")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return false, nil // no such session, or no server running
	}
	return false, fmt.Errorf("tmux has-session: %w: %s", err, strings.TrimSpace(errb.String()))
}

// KillSession kills a session. Missing sessions are not an error.
func (c *Client) KillSession(ctx context.Context, name string) error {
	ok, err := c.HasSession(ctx, name)
	if err != nil || !ok {
		return err
	}
	_, err = c.run(ctx, "kill-session", "-t", "="+name)
	return err
}

// ListSessions returns session names starting with prefix (all if empty).
func (c *Client) ListSessions(ctx context.Context, prefix string) ([]string, error) {
	out, err := c.run(ctx, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		if strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "No such file or directory") || strings.Contains(err.Error(), "error connecting") {
			return nil, nil
		}
		return nil, err
	}
	var res []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && strings.HasPrefix(l, prefix) {
			res = append(res, l)
		}
	}
	return res, nil
}

// SessionInfo describes one tmux session.
type SessionInfo struct {
	Name     string
	Path     string    // the session's working directory
	Activity time.Time // last activity tmux saw in the session
}

// ListSessionInfo returns the sessions whose name starts with prefix (all if empty).
func (c *Client) ListSessionInfo(ctx context.Context, prefix string) ([]SessionInfo, error) {
	out, err := c.run(ctx, "list-sessions", "-F", "#{session_name}\t#{session_activity}\t#{session_path}")
	if err != nil {
		if strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "No such file or directory") || strings.Contains(err.Error(), "error connecting") {
			return nil, nil
		}
		return nil, err
	}
	var res []SessionInfo
	for _, l := range strings.Split(out, "\n") {
		f := strings.SplitN(strings.TrimRight(l, "\r"), "\t", 3)
		if len(f) < 3 || !strings.HasPrefix(f[0], prefix) {
			continue
		}
		si := SessionInfo{Name: f[0], Path: f[2]}
		if n, err := strconv.ParseInt(f[1], 10, 64); err == nil && n > 0 {
			si.Activity = time.Unix(n, 0)
		}
		res = append(res, si)
	}
	return res, nil
}

// SessionEnv returns the value of key in the session's environment ("" if unset).
func (c *Client) SessionEnv(ctx context.Context, session, key string) string {
	out, err := c.run(ctx, "show-environment", "-t", "="+session, key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), key+"="))
}

// EnterDelay is the pause between typed text and its Enter. Claude Code's input
// box treats text followed immediately by a newline as a paste and keeps the
// newline in the box instead of submitting, so the text sat there unsent.
var EnterDelay = 400 * time.Millisecond

// SendKeys types text literally (-l) and optionally presses Enter.
func (c *Client) SendKeys(ctx context.Context, session, text string, enter bool) error {
	return c.SendKeysDelay(ctx, session, text, enter, EnterDelay)
}

// SendKeysDelay is SendKeys with the pause before Enter given by the harness whose
// TUI is being typed into (Claude Code needs EnterDelay; others submit at once).
func (c *Client) SendKeysDelay(ctx context.Context, session, text string, enter bool, delay time.Duration) error {
	if text != "" {
		if _, err := c.run(ctx, "send-keys", "-t", session, "-l", "--", text); err != nil {
			return err
		}
		if enter && delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}
	}
	if enter {
		_, err := c.run(ctx, "send-keys", "-t", session, "Enter")
		return err
	}
	return nil
}

// CapturePane returns pane contents. lines > 0 captures that many lines of scrollback
// above the visible area as well; otherwise only the visible pane.
func (c *Client) CapturePane(ctx context.Context, session string, withANSI bool, lines int) (string, error) {
	a := []string{"capture-pane", "-p", "-t", session}
	if withANSI {
		a = append(a, "-e")
	}
	if lines > 0 {
		a = append(a, "-S", "-"+strconv.Itoa(lines))
	}
	return c.run(ctx, a...)
}

// KillServer stops the tmux server (no error if none is running).
func (c *Client) KillServer(ctx context.Context) error {
	_, err := c.run(ctx, "kill-server")
	if c.Socket != "" {
		RemoveSocket(c.Socket) // tmux can leave the socket file behind
	}
	if err != nil && (strings.Contains(err.Error(), "no server running") || strings.Contains(err.Error(), "error connecting")) {
		return nil
	}
	return err
}

// RemoveSocket deletes the socket file of a named (-L) tmux server that is no
// longer running. It never touches a live server's socket.
func RemoveSocket(name string) {
	if name == "" || strings.ContainsAny(name, "/\\") {
		return
	}
	if exec.Command("tmux", "-L", name, "has-session").Run() == nil {
		return // a live server is still using it
	}
	base := os.Getenv("TMUX_TMPDIR")
	if base == "" {
		base = "/tmp"
	}
	_ = os.Remove(filepath.Join(base, fmt.Sprintf("tmux-%d", os.Getuid()), name))
}

// Attach attaches the current terminal to the session interactively and blocks until detach.
func (c *Client) Attach(ctx context.Context, session string) error {
	cmd := exec.CommandContext(ctx, c.bin(), c.args("attach-session", "-t", "="+session)...)
	cmd.Env = EnvWithout(os.Environ(), "TMUX")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("tmux attach %s: %w", session, err)
	}
	return nil
}

// AttachCmd returns the command for attaching (for tea.ExecProcess).
func (c *Client) AttachCmd(session string) *exec.Cmd {
	cmd := exec.Command(c.bin(), c.args("attach-session", "-t", "="+session)...)
	// tmux refuses to nest inside another tmux client unless $TMUX is unset.
	cmd.Env = EnvWithout(os.Environ(), "TMUX")
	return cmd
}

// EnvWithout returns env minus the variable name.
func EnvWithout(env []string, name string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, name+"=") {
			out = append(out, e)
		}
	}
	return out
}

// ClientCount returns how many clients (e.g. the user's terminal) are attached to the session.
func (c *Client) ClientCount(ctx context.Context, session string) (int, error) {
	out, err := c.run(ctx, "list-clients", "-t", session, "-F", "#{client_name}")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) != "" {
			n++
		}
	}
	return n, nil
}

// Key is one element of a key sequence for SendSeq: literal text (typed with
// send-keys -l) or a named tmux key such as Enter, C-c, M-x, BTab, F5.
type Key struct {
	Literal bool
	S       string
}

// Lit and Named build Keys.
func Lit(s string) Key   { return Key{Literal: true, S: s} }
func Named(s string) Key { return Key{S: s} }

// target addresses a session's active pane exactly (no prefix matching).
func target(session string) string { return "=" + session + ":" }

// SendSeq sends a key sequence to the session's pane in order. Consecutive
// literals are merged; named keys are batched into one send-keys call.
func (c *Client) SendSeq(ctx context.Context, session string, keys []Key) error {
	for i := 0; i < len(keys); {
		j := i
		if keys[i].Literal {
			var sb strings.Builder
			for j < len(keys) && keys[j].Literal {
				sb.WriteString(keys[j].S)
				j++
			}
			if sb.Len() > 0 {
				if _, err := c.run(ctx, "send-keys", "-t", target(session), "-l", "--", sb.String()); err != nil {
					return err
				}
			}
		} else {
			a := []string{"send-keys", "-t", target(session)}
			for j < len(keys) && !keys[j].Literal {
				a = append(a, keys[j].S)
				j++
			}
			if _, err := c.run(ctx, a...); err != nil {
				return err
			}
		}
		i = j
	}
	return nil
}

// Paste pastes text into the pane as one bracketed paste (tmux set-buffer +
// paste-buffer -p), so multi-line text arrives intact instead of as Enters.
func (c *Client) Paste(ctx context.Context, session, text string) error {
	if text == "" {
		return nil
	}
	buf := "slai-paste"
	if _, err := c.run(ctx, "set-buffer", "-b", buf, "--", text); err != nil {
		return err
	}
	_, err := c.run(ctx, "paste-buffer", "-p", "-d", "-b", buf, "-t", target(session))
	return err
}

// ResizeWindow resizes the session's window to exactly w x h cells.
func (c *Client) ResizeWindow(ctx context.Context, session string, w, h int) error {
	if w < 1 || h < 1 {
		return nil
	}
	_, err := c.run(ctx, "resize-window", "-t", target(session), "-x", strconv.Itoa(w), "-y", strconv.Itoa(h))
	return err
}

// WindowSizeLatest returns the window to tmux's default sizing (follow the
// most recently active client), undoing the manual size resize-window sets.
func (c *Client) WindowSizeLatest(ctx context.Context, session string) error {
	_, err := c.run(ctx, "set-window-option", "-t", target(session), "window-size", "latest")
	return err
}

// BindDetachKey binds key (e.g. "C-q") without prefix to detach-client.
func (c *Client) BindDetachKey(ctx context.Context, key string) error {
	_, err := c.run(ctx, "bind-key", "-n", key, "detach-client")
	return err
}

// Snapshot is a pane capture plus geometry and cursor.
type Snapshot struct {
	Body          string // capture-pane -p -e output
	W, H          int    // pane size in cells
	CursorX       int
	CursorY       int
	CursorVisible bool
	HistorySize   int // lines of scrollback above the visible area
	Offset        int // how many lines back from the bottom Body is (0 = live view)

	// Pane mode: what the app inside has negotiated with the terminal.
	AltScreen     bool // alternate screen: no scrollback exists
	MouseAny      bool // the app wants mouse reports (any mouse mode on)
	MouseSGR      bool // ... in SGR (1006) encoding
	MouseStandard bool // ... click reporting (1000)
	MouseButton   bool // ... drag reporting (1002)
	MouseUTF8     bool // ... in UTF-8 (1005) encoding
}

const snapMarker = "SLAISNAP "

const snapFormat = snapMarker + "#{pane_width} #{pane_height} #{cursor_x} #{cursor_y} #{cursor_flag} #{history_size} #{alternate_on} #{mouse_any_flag} #{mouse_sgr_flag} #{mouse_standard_flag} #{mouse_button_flag} #{mouse_utf8_flag}"

func parseSnapHeader(head string) (Snapshot, error) {
	var s Snapshot
	var flag, alt, any, sgr, std, btn, utf int
	if _, err := fmt.Sscanf(strings.TrimPrefix(head, snapMarker), "%d %d %d %d %d %d %d %d %d %d %d %d",
		&s.W, &s.H, &s.CursorX, &s.CursorY, &flag, &s.HistorySize, &alt, &any, &sgr, &std, &btn, &utf); err != nil {
		return Snapshot{}, fmt.Errorf("tmux snapshot: bad header %q", head)
	}
	s.CursorVisible = flag == 1
	s.AltScreen, s.MouseAny, s.MouseSGR = alt == 1, any == 1, sgr == 1
	s.MouseStandard, s.MouseButton, s.MouseUTF8 = std == 1, btn == 1, utf == 1
	return s, nil
}

// Snapshot captures the visible pane (with ANSI) and its cursor in one tmux call.
func (c *Client) Snapshot(ctx context.Context, session string) (Snapshot, error) {
	return c.SnapshotAt(ctx, session, 0)
}

// ScrollRange returns the capture-pane -S/-E arguments that show the pane
// rows [-offset .. h-1-offset] (negative = scrollback, 0 = first visible row).
func ScrollRange(offset, h int) (start, end string) {
	return strconv.Itoa(-offset), strconv.Itoa(h - 1 - offset)
}

// SnapshotAt is Snapshot, but showing the window `offset` lines back in the
// scrollback (offset is clamped to the available history; Snapshot.Offset is
// the value actually used). offset <= 0 is the live view and costs one tmux call.
func (c *Client) SnapshotAt(ctx context.Context, session string, offset int) (Snapshot, error) {
	t := target(session)
	if offset <= 0 {
		out, err := c.run(ctx, "display-message", "-p", "-t", t, snapFormat, ";", "capture-pane", "-p", "-e", "-t", t)
		if err != nil {
			return Snapshot{}, err
		}
		head, body, _ := strings.Cut(out, "\n")
		s, err := parseSnapHeader(head)
		if err != nil {
			return Snapshot{}, err
		}
		s.Body = body
		return s, nil
	}
	// The capture range depends on the pane height and the history size, so
	// ask for those first.
	out, err := c.run(ctx, "display-message", "-p", "-t", t, snapFormat)
	if err != nil {
		return Snapshot{}, err
	}
	s, err := parseSnapHeader(strings.TrimSpace(out))
	if err != nil {
		return Snapshot{}, err
	}
	k := min(offset, s.HistorySize)
	if k <= 0 {
		return c.SnapshotAt(ctx, session, 0)
	}
	st, en := ScrollRange(k, s.H)
	body, err := c.run(ctx, "capture-pane", "-p", "-e", "-t", t, "-S", st, "-E", en)
	if err != nil {
		return Snapshot{}, err
	}
	s.Body, s.Offset = body, k
	return s, nil
}
