package tui

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// osc52Max is the largest payload (bytes of base64) we emit; many terminals
// drop longer OSC 52 sequences.
const osc52Max = 74994

// osc52 builds the OSC 52 "set clipboard" sequence for text. With inTmux it is
// wrapped in tmux's DCS passthrough (ESC doubled) so it reaches the outer
// terminal (needs `set -g allow-passthrough on` in tmux >= 3.3).
func osc52(text string, inTmux bool) string {
	seq := "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\x07"
	if inTmux {
		return "\x1bPtmux;" + strings.ReplaceAll(seq, "\x1b", "\x1b\x1b") + "\x1b\\"
	}
	return seq
}

// clipHelper is a native clipboard command.
type clipHelper struct {
	name string
	args []string
	env  string // env var that must be non-empty for the helper to be usable ("" = always)
}

var clipHelpers = []clipHelper{
	{"pbcopy", nil, ""},
	{"wl-copy", nil, "WAYLAND_DISPLAY"},
	{"xclip", []string{"-selection", "clipboard"}, "DISPLAY"},
	{"xsel", []string{"--clipboard", "--input"}, "DISPLAY"},
}

// Clipboard copies text via a native helper (when one is present and usable)
// and, additionally, an OSC 52 sequence written to the terminal so it also
// works over SSH. Every dependency is injectable for tests.
type Clipboard struct {
	LookPath func(string) (string, error)
	Run      func(ctx context.Context, name string, args []string, stdin string) error
	Getenv   func(string) string
	Out      io.Writer // terminal; nil disables OSC 52
	GOOS     string
}

// NewClipboard returns a Clipboard wired to the real system and terminal.
func NewClipboard() *Clipboard {
	return &Clipboard{LookPath: exec.LookPath, Run: runHelper, Getenv: os.Getenv, Out: os.Stdout, GOOS: runtime.GOOS}
}

func runHelper(ctx context.Context, name string, args []string, stdin string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = strings.NewReader(stdin) // stdout/stderr -> /dev/null so xclip can daemonize
	return cmd.Run()
}

// Copy puts text on the clipboard. It returns what was used, e.g. "xclip+osc52".
// An error is returned only when nothing worked.
func (c *Clipboard) Copy(text string) (string, error) {
	var used []string
	var errs []string
	for _, h := range clipHelpers {
		if h.env != "" && c.Getenv(h.env) == "" {
			continue
		}
		if h.name == "pbcopy" && c.GOOS != "darwin" {
			continue
		}
		if _, err := c.LookPath(h.name); err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := c.Run(ctx, h.name, h.args, text)
		cancel()
		if err == nil {
			used = append(used, h.name)
			break
		}
		errs = append(errs, h.name+": "+err.Error())
	}
	if c.Out != nil && len(base64.StdEncoding.EncodeToString([]byte(text))) <= osc52Max {
		if _, err := io.WriteString(c.Out, osc52(text, c.Getenv("TMUX") != "")); err == nil {
			used = append(used, "osc52")
		} else {
			errs = append(errs, "osc52: "+err.Error())
		}
	}
	if len(used) == 0 {
		if len(errs) == 0 {
			errs = append(errs, "no clipboard helper and text too long for OSC 52")
		}
		return "", fmt.Errorf("copy failed: %s", strings.Join(errs, "; "))
	}
	return strings.Join(used, "+"), nil
}
