package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/hooks"
)

// The pane detectors decide whether slai may type into an agent's window, so a UI
// change in Claude Code, OpenCode or Pi must break THIS test, not an agent. Every
// fixture is a capture from the real TUI (claude 2.1.306 and 2.1.307, opencode 1.18.16, pi 1.3.0
// in a 200x50 tmux pane), except the ones marked synthetic. Negative cases are the
// ones that matter most: text that LOOKS like a prompt but is not live.

type paneCase struct {
	kind   Kind
	file   string // under internal/hooks/testdata/panes (or ../supervisor/testdata/panes for "sup:")
	status string // "" = nothing detected
	rule   string
	neg    string // non-empty: a negative case, and what it is
}

var paneTable = []paneCase{
	// ---- Claude Code ----
	{Claude, "claude/trust-dialog.txt", hooks.StatusWaiting, hooks.RuleModal, ""},
	{Claude, "claude/permission-dialog.txt", hooks.StatusWaiting, hooks.RuleModal, ""},
	{Claude, "claude/working-spinner.txt", hooks.StatusWorking, hooks.RuleWorking, ""},
	{Claude, "claude/working-spinner-2.1.307.txt", hooks.StatusWorking, hooks.RuleWorking, ""},
	{Claude, "claude/working-bash-2.1.307.txt", hooks.StatusWorking, hooks.RuleWorking, ""},
	{Claude, "claude/working-queued-message-2.1.307.txt", hooks.StatusWorking, hooks.RuleWorking, ""},
	{Claude, "claude/idle-done-2.1.307.txt", hooks.StatusIdle, hooks.RuleIdle, "the finished line \"Cogitated for 4m 40s · done\" is past tense, no ellipsis"},
	{Claude, "claude/idle-wake-typed-2.1.307.txt", hooks.StatusIdle, hooks.RuleIdle, "a wake line typed at the prompt of an idle pane"},
	{Claude, "claude/idle-reply-quotes-esc-to-interrupt-synthetic.txt", hooks.StatusIdle, hooks.RuleIdle, "a reply just above the prompt saying 2.1.307 no longer prints esc to interrupt (synthetic: the idle capture with that reply)"},
	{Claude, "claude/idle-reply-quotes-spinner-synthetic.txt", hooks.StatusIdle, hooks.RuleIdle, "a reply quoting a spinner line as a bullet, in backticks, indented, after a bullet glyph and as a tool result (synthetic)"},
	{Claude, "claude/idle-old-spinner-in-scrollback-synthetic.txt", hooks.StatusIdle, hooks.RuleIdle, "a live-looking spinner line far up in scrollback (synthetic: the idle capture with a spinner prepended)"},
	{Claude, "claude/idle-prompt.txt", hooks.StatusIdle, hooks.RuleIdle, ""},
	{Claude, "claude/idle-after-turn.txt", hooks.StatusIdle, hooks.RuleIdle, ""},
	{Claude, "claude/idle-manual-mode.txt", hooks.StatusIdle, hooks.RuleIdle, ""},
	{Claude, "claude/idle-after-rejected-dialog.txt", hooks.StatusIdle, hooks.RuleIdle, "a dismissed dialog's text above the prompt"},
	{Claude, "claude/bg-shell-footer.txt", hooks.StatusIdle, hooks.RuleIdle, "footer \"1 shell\" replaces \"? for shortcuts\""},
	{Claude, "claude/bg-tasks-footer.txt", hooks.StatusIdle, hooks.RuleIdle, "footer \"1 shell, 1 monitor\""},
	{Claude, "claude/bg-tasks-footer-plural-synthetic.txt", hooks.StatusIdle, hooks.RuleIdle, "footer \"2 shells, 3 monitors\" (synthetic: the real capture with the counts changed)"},
	{Claude, "claude/subagents-working-turn-2.1.307.txt", hooks.StatusWorking, hooks.RuleWorking, "subagents running while the main turn works (real capture)"},
	{Claude, "claude/subagents-idle-turn-2.1.307.txt", hooks.StatusIdle, hooks.RuleIdle, "subagents still running after the main turn ended: the pane reads idle, BackgroundWith says why it is not (real capture)"},
	{Claude, "claude/quoted-dialog-in-scrollback.txt", hooks.StatusIdle, hooks.RuleIdle, "the dialog's words quoted in a reply (real capture)"},
	{Claude, "sup:ratelimit_reset.txt", hooks.StatusRateLimited, hooks.RuleRateLimit, "synthetic"},
	{Claude, "sup:ratelimit_noreset.txt", hooks.StatusRateLimited, hooks.RuleRateLimit, "synthetic"},
	{Claude, "sup:orch_quoted_modal_note.txt", hooks.StatusIdle, hooks.RuleIdle, "slai's own note quoting a prompt (synthetic)"},
	{Claude, "sup:orch_quoted_modal_note_working.txt", hooks.StatusWorking, hooks.RuleWorking, "the same note while working (synthetic)"},
	// ---- OpenCode ----
	{OpenCode, "opencode/idle-home.txt", hooks.StatusIdle, hooks.RuleIdle, ""},
	{OpenCode, "opencode/idle-after-turn.txt", hooks.StatusIdle, hooks.RuleIdle, ""},
	{OpenCode, "opencode/working.txt", hooks.StatusWorking, hooks.RuleWorking, ""},
	{OpenCode, "opencode/permission-dialog.txt", hooks.StatusWaiting, hooks.RuleModal, ""},
	{OpenCode, "opencode/permission-dialog-external-dir.txt", hooks.StatusWaiting, hooks.RuleModal, ""},
	{OpenCode, "opencode/provider-error-retrying.txt", hooks.StatusRateLimited, hooks.RuleRateLimit, ""},
	{OpenCode, "opencode/quoted-dialog-in-scrollback.txt", hooks.StatusIdle, hooks.RuleIdle, "the dialog's words quoted in a reply"},
	{OpenCode, "opencode/dialog-then-idle.txt", hooks.StatusIdle, hooks.RuleIdle, "a real dialog capture followed by a real idle capture: the dialog scrolled into history (synthetic composite)"},
	// ---- Pi ----
	{Pi, "pi/idle-startup-extkeys-on.txt", hooks.StatusIdle, hooks.RuleIdle, ""},
	{Pi, "pi/idle-startup-extkeys-off.txt", hooks.StatusIdle, hooks.RuleIdle, ""},
	{Pi, "pi/idle-after-turn.txt", hooks.StatusIdle, hooks.RuleIdle, ""},
	{Pi, "pi/working.txt", hooks.StatusWorking, hooks.RuleWorking, ""},
	{Pi, "pi/trust-prompt.txt", hooks.StatusWaiting, hooks.RuleModal, ""},
	{Pi, "pi/quoted-dialog-in-scrollback.txt", hooks.StatusIdle, hooks.RuleIdle, "the dialog's words quoted in a reply"},
	{Pi, "pi/provider-error-badkey.txt", hooks.StatusIdle, hooks.RuleIdle, "a 401 is an auth failure, not a rate limit"},
	{Pi, "pi/lane-denied-tool-result.txt", hooks.StatusIdle, hooks.RuleIdle, "a denied tool result is not a prompt"},
}

func readPane(t *testing.T, file string) string {
	t.Helper()
	path := filepath.Join("..", "hooks", "testdata", "panes", file)
	if rest, ok := strings.CutPrefix(file, "sup:"); ok {
		path = filepath.Join("..", "supervisor", "testdata", "panes", rest)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestPaneFixturesDecideAsExpected(t *testing.T) {
	negatives := map[Kind]int{}
	for _, c := range paneTable {
		h, err := For(c.kind)
		if err != nil {
			t.Fatal(err)
		}
		rules := h.PaneRules()
		pane := readPane(t, c.file)
		d := hooks.ExplainWith(rules, pane)
		if d.Status != c.status || d.Rule != c.rule {
			t.Errorf("%s/%s: decided %q by rule %q (excerpt %q), want %q by %q", c.kind, c.file, d.Status, d.Rule, d.Excerpt, c.status, c.rule)
		}
		if len([]rune(d.Excerpt)) > hooks.ExcerptMax {
			t.Errorf("%s/%s: excerpt is %d chars, max %d", c.kind, c.file, len([]rune(d.Excerpt)), hooks.ExcerptMax)
		}
		if d.Rule != "" && d.Excerpt == "" {
			t.Errorf("%s/%s: rule %s matched with an empty excerpt", c.kind, c.file, d.Rule)
		}
		// the shared helpers agree with ExplainWith
		if st, ok := hooks.DetectWith(rules, pane); st != d.Status || ok != d.OK {
			t.Errorf("%s/%s: DetectWith = %q,%v disagrees with ExplainWith", c.kind, c.file, st, ok)
		}
		isModal := hooks.ModalExcerptWith(rules, pane) != ""
		if isModal != (c.status == hooks.StatusWaiting) {
			t.Errorf("%s/%s: ModalExcerptWith non-empty = %v, status %q", c.kind, c.file, isModal, c.status)
		}
		if c.neg != "" {
			negatives[c.kind]++
		}
	}
	for _, k := range Kinds() {
		if negatives[k] < 2 {
			t.Errorf("%s has %d negative cases, want at least 2", k, negatives[k])
		}
	}
	if len(paneTable) < 8 {
		t.Errorf("only %d fixtures", len(paneTable))
	}
}

// A live dialog followed (in the capture) by an idle screen is history: whatever the
// harness, once its input box / footer is drawn below, the dialog is not a prompt.
func TestDialogScrolledAboveALiveInputBoxIsHistory(t *testing.T) {
	for _, c := range []struct {
		kind         Kind
		dialog, idle string
	}{
		{Claude, "claude/permission-dialog.txt", "claude/idle-prompt.txt"},
		{Claude, "claude/trust-dialog.txt", "claude/idle-after-turn.txt"},
		{OpenCode, "opencode/permission-dialog.txt", "opencode/idle-after-turn.txt"},
		{OpenCode, "opencode/permission-dialog-external-dir.txt", "opencode/idle-home.txt"},
		{Pi, "pi/trust-prompt.txt", "pi/idle-after-turn.txt"},
	} {
		h, _ := For(c.kind)
		rules := h.PaneRules()
		dialog, idle := readPane(t, c.dialog), readPane(t, c.idle)
		if hooks.ModalExcerptWith(rules, dialog) == "" {
			t.Errorf("%s: the dialog alone must be a live modal (%s)", c.kind, c.dialog)
		}
		both := hooks.ExplainWith(rules, dialog+"\n"+idle)
		if both.Status == hooks.StatusWaiting || both.Rule == hooks.RuleModal {
			t.Errorf("%s: %s followed by %s still reads as a live prompt: %+v", c.kind, c.dialog, c.idle, both)
		}
	}
}

// Detection must not turn a pane's quoted words into a refusal to type: the
// excerpt of a refusal names the live prompt and stays within 200 characters.
func TestModalDecisionCarriesABoundedExcerpt(t *testing.T) {
	for _, c := range paneTable {
		if c.status != hooks.StatusWaiting {
			continue
		}
		h, _ := For(c.kind)
		d := hooks.ExplainWith(h.PaneRules(), readPane(t, c.file))
		if d.Rule != hooks.RuleModal || d.Excerpt == "" || len([]rune(d.Excerpt)) > hooks.ExcerptMax {
			t.Errorf("%s/%s: %+v", c.kind, c.file, d)
		}
	}
}

// Claude Code 2.1.307 dropped "esc to interrupt": the working rule must name what it
// actually matched, the spinner or the background hint, and it must be the bottom one.
func TestClaude2_1_307WorkingRuleMatchesTheSpinnerAndBackgroundHint(t *testing.T) {
	h, _ := For(Claude)
	for file, want := range map[string]string{
		"claude/working-spinner-2.1.307.txt":        "Moonwalking… (9s",
		"claude/working-bash-2.1.307.txt":           "Moonwalking… (31s",
		"claude/working-queued-message-2.1.307.txt": "Cerebrating… (2m 31s",
	} {
		pane := readPane(t, file)
		if strings.Contains(strings.ToLower(pane), "esc to interrupt") {
			t.Fatalf("%s still has the old marker; it proves nothing about 2.1.307", file)
		}
		if d := hooks.ExplainWith(h.PaneRules(), pane); d.Status != hooks.StatusWorking {
			t.Errorf("%s: %+v", file, d)
		}
		// without the background hint the spinner line alone decides
		var kept []string
		for _, l := range strings.Split(pane, "\n") {
			if !strings.Contains(l, "ctrl+b") {
				kept = append(kept, l)
			}
		}
		d := hooks.ExplainWith(h.PaneRules(), strings.Join(kept, "\n"))
		if d.Status != hooks.StatusWorking || !strings.Contains(d.Excerpt, want) {
			t.Errorf("%s: %q by %q, excerpt %q, want working on %q", file, d.Status, d.Rule, d.Excerpt, want)
		}
	}
	// the hint alone (a tool running between two spinner frames) counts too
	d := hooks.ExplainWith(h.PaneRules(), "  Bash(sleep 60)\n  (ctrl+b ctrl+b (twice) to run in background)\n────\n❯ \n────\n  ⏵⏵ auto mode on\n")
	if d.Status != hooks.StatusWorking || !strings.Contains(d.Excerpt, "ctrl+b") {
		t.Errorf("hint only: %+v", d)
	}
	// the old marker still works for older builds
	if d := hooks.ExplainWith(h.PaneRules(), readPane(t, "claude/working-spinner.txt")); d.Status != hooks.StatusWorking {
		t.Errorf("2.1.306 fixture: %+v", d)
	}
}

// The spinner glyph rotates (✻ ✽ ✶ · and a plain *), and a Tip line can sit under it.
func TestClaudeSpinnerGlyphsAllCountAsWorking(t *testing.T) {
	h, _ := For(Claude)
	for _, g := range []string{"✻", "✽", "✶", "✢", "·", "*"} {
		pane := "some output\n" + g + " Gusting… (23s · ↓ 2.3k tokens)\n  ⎿  Tip: a tip\n────\n❯ \n────\n  ⏵⏵ auto mode on\n"
		if d := hooks.ExplainWith(h.PaneRules(), pane); d.Status != hooks.StatusWorking {
			t.Errorf("glyph %q: %+v", g, d)
		}
	}
}

// The working rule matches a whole status line, never prose that mentions one.
func TestClaudeWorkingRuleIsAnchoredToTheWholeLine(t *testing.T) {
	h, _ := For(Claude)
	tail := "\n────\n❯ \n────\n  ⏵⏵ auto mode on (shift+tab to cycle)\n"
	for line, working := range map[string]bool{
		"✻ Cerebrating… (1m 21s · ↓ 408 tokens)":                           true,
		"* Gusting… (23s · ↓ 2.3k tokens)":                                 true,
		"· Moonwalking… (3m 10s · ↓ 15.1k tokens · thought for 2s)":        true,
		"✻ Thinking… (12s · esc to interrupt)":                             true,
		"  (ctrl+b ctrl+b (twice) to run in background)":                   true,
		"✻ Cogitated for 4m 40s · done 1:29 AM":                            false,
		"  ✻ Cerebrating… (1m 21s · ↓ 408 tokens)":                         false, // indented: quoted
		"- ✻ Cerebrating… (1m 21s · ↓ 408 tokens)":                         false,
		"● ✻ Cerebrating… (1m 21s · ↓ 408 tokens)":                         false,
		"❯ ✻ Cerebrating… (1m 21s · ↓ 408 tokens)":                         false,
		"  ⎿  ✻ Cerebrating… (1m 21s · ↓ 408 tokens)":                      false,
		"1 ✻ Cerebrating… (1m 21s · ↓ 408 tokens)":                         false,
		"✻ Cerebrating… (1m 21s · ↓ 408 tokens) is what 2.1.307 draws":     false, // text after the paren
		"  It no longer prints esc to interrupt, so I matched the spinner": false,
		"  Press esc to interrupt (or ctrl+c)":                             false,
		"  I saw (ctrl+b ctrl+b (twice) to run in background) once":        false,
	} {
		d := hooks.ExplainWith(h.PaneRules(), line+tail)
		if got := d.Status == hooks.StatusWorking; got != working {
			t.Errorf("%q: working = %v, want %v (%+v)", line, got, working, d)
		}
	}
}
