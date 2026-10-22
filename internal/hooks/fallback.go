package hooks

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// paneRule maps a regex over the bottom of the pane to a status. Order matters:
// first match wins. Edit this table when Claude Code's UI changes.
type paneRule struct {
	re     *regexp.Regexp
	status string
}

// rateLimitRe matches Claude Code's usage-cap messages ("5-hour limit reached ∙
// resets 3pm", "Claude usage limit reached", "You've hit your limit").
var rateLimitRe = regexp.MustCompile(`(?i)(usage|rate|\d+-hour|weekly|session) limit (reached|exceeded)|you've (hit|reached) your (usage |rate |session )?limit|limit reached\s*[∙·|-]\s*resets`)

var paneRules = []paneRule{
	// Idle: the boxed ">" prompt and "? for shortcuts" of older builds, the bare "❯"
	// prompt (empty, or showing the placeholder suggestion) and the permission-mode
	// footer of 2.1.x (fixtures: testdata/panes/claude, captured from 2.1.306).
	{regexp.MustCompile(`(?m)^\s*[│|]\s*>(\s|$)|\? for shortcuts|^\s*❯(\s*$| Try ")|^\s*(⏵⏵|⏸) `), StatusIdle},
}

// workingLiveRe matches a turn in progress, and only a whole status line: agent prose that
// quotes these words (a summary saying "esc to interrupt", a quoted spinner) must never
// look like a live turn. Three shapes, each anchored to the line:
//   - the spinner of 2.1.307+, which no longer prints "esc to interrupt":
//     "✻ Cerebrating… (1m 21s · ↓ 408 tokens)" at column 0: one glyph (never a bullet,
//     prompt, dash, tool-result or digit), one space, a capitalised verb, an ellipsis, a
//     parenthesised group starting with the elapsed time and nothing after it;
//   - the older spinner whose parenthesised group ends in "esc to interrupt":
//     "✻ Thinking… (12s · esc to interrupt)";
//   - the "(ctrl+b ctrl+b (twice) to run in background)" hint under a running tool, as a
//     line of its own.
//
// The finished line "✻ Cogitated for 4m 40s · done 1:29 AM" has no ellipsis and matches none.
var workingLiveRe = regexp.MustCompile(`(?m)^[^\s●❯⎿\-\d] \p{Lu}[\p{L}'-]*…\s+\(\d[^\n]*\)\s*$` +
	`|^[^\s●❯⎿\-\d] \p{Lu}[\p{L}'-]*….*\([^()\n]*(?i:esc to interrupt)[^()\n]*\)\s*$` +
	`|^\s*\(ctrl\+b ctrl\+b \(twice\) to run in background\)\s*$`)

// workingLiveLines is how many trailing non-empty lines can hold a live spinner: it is
// drawn right above the input box (spinner, tip, rule, prompt, rule, footer), so an old
// spinner line in scrollback must not make an idle pane look busy.
const workingLiveLines = 10

// paneTailLines is how many trailing non-empty lines are inspected.
const paneTailLines = 25

// rateLimitTailLines is how many trailing non-empty lines can show a rate limit.
const rateLimitTailLines = 8

// DetectFromPane classifies Claude Code's UI from captured pane text.
func DetectFromPane(paneText string) (status string, ok bool) {
	return DetectWith(ClaudeRules(), paneText)
}

// DetectWith classifies a harness's TUI from captured pane text using that
// harness's rules. The logic is shared: a usage cap only counts in the last few
// lines, and a blocking prompt only while it is live.
func DetectWith(r Rules, paneText string) (status string, ok bool) {
	d := ExplainWith(r, paneText)
	return d.Status, d.OK
}

// Pane rule names reported by ExplainWith (the order is the precedence).
const (
	RuleRateLimit = "rate_limit"
	RuleModal     = "modal"
	RuleWorking   = "working"
	RuleIdle      = "idle"
)

// PaneDecision is what pane detection concluded and why: the status, the rule that
// matched and a short excerpt of the pane text it matched on (at most ExcerptMax
// characters, for logs and `slai doctor --panes`; never put it in a note an agent
// will see, it can contain the prompt text itself).
type PaneDecision struct {
	Status  string
	OK      bool
	Rule    string
	Excerpt string
}

// ExcerptMax bounds PaneDecision.Excerpt.
const ExcerptMax = 200

// ruleGlyphs are box-drawing characters: borders and dividers, noise in an excerpt.
var ruleGlyphs = regexp.MustCompile(`[─━╌╍═┄┅┈┉│┃╭╮╰╯]+`)

func clip(s string) string {
	s = strings.Join(strings.Fields(ruleGlyphs.ReplaceAllString(s, " ")), " ")
	if r := []rune(s); len(r) > ExcerptMax {
		return string(r[:ExcerptMax-1]) + "…"
	}
	return s
}

// matchedLine is the line of text that re matches (the whole line, not just the
// match: "⏵⏵" alone says little), or "" when it does not.
func matchedLine(re *regexp.Regexp, text string) string {
	loc := re.FindStringIndex(text)
	if loc == nil {
		return ""
	}
	start := strings.LastIndex(text[:loc[0]], "\n") + 1
	end := strings.Index(text[loc[0]:], "\n")
	if end < 0 {
		end = len(text)
	} else {
		end += loc[0]
	}
	return text[start:end]
}

// ExplainWith is DetectWith that also says which rule decided.
func ExplainWith(r Rules, paneText string) PaneDecision {
	var tail []string
	for _, l := range strings.Split(paneText, "\n") {
		if strings.TrimSpace(l) != "" {
			tail = append(tail, l)
		}
	}
	if len(tail) > paneTailLines {
		tail = tail[len(tail)-paneTailLines:]
	}
	text := strings.Join(tail, "\n")
	// Only the last few lines: older scrollback (a cat of a fixture, a quoted
	// error) must not mark a healthy agent rate limited.
	limitTail := strings.Join(tail[max(len(tail)-rateLimitTailLines, 0):], "\n")
	if r.RateLimit != nil && r.RateLimit.MatchString(limitTail) {
		return PaneDecision{StatusRateLimited, true, RuleRateLimit, clip(matchedLine(r.RateLimit, limitTail))}
	}
	// A permission/trust prompt only counts while it is live (see ModalExcerptWith):
	// the same words quoted in scrollback or in a slai note are not a prompt.
	if region := modalRegion(r, paneText); region != "" {
		return PaneDecision{StatusWaiting, true, RuleModal, clip(region)}
	}
	if r.WorkingLive != nil {
		live := strings.Join(tail[max(len(tail)-workingLiveLines, 0):], "\n")
		if r.WorkingLive.MatchString(live) {
			return PaneDecision{StatusWorking, true, RuleWorking, clip(matchedLine(r.WorkingLive, live))}
		}
	}
	if r.Working != nil && r.Working.MatchString(text) {
		return PaneDecision{StatusWorking, true, RuleWorking, clip(matchedLine(r.Working, text))}
	}
	if r.Idle != nil && r.Idle.MatchString(text) {
		return PaneDecision{StatusIdle, true, RuleIdle, clip(matchedLine(r.Idle, text))}
	}
	return PaneDecision{}
}

var resetRe = regexp.MustCompile(`(?i)resets?(?:\s+at)?\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)?`)

// ParseRateLimitReset extracts the reset time from a rate-limit pane ("resets
// 3pm", "resets at 15:30", "resets 3:30pm (America/New_York)"), interpreted in
// now's location and rolled to the next day if it already passed. ok is false
// when the pane names no reset time.
func ParseRateLimitReset(pane string, now time.Time) (time.Time, bool) {
	ms := resetRe.FindAllStringSubmatch(pane, -1)
	if len(ms) == 0 {
		return time.Time{}, false
	}
	m := ms[len(ms)-1] // the most recent message is last
	h, _ := strconv.Atoi(m[1])
	min := 0
	if m[2] != "" {
		min, _ = strconv.Atoi(m[2])
	}
	switch ap := strings.ToLower(m[3]); {
	case ap == "pm" && h < 12:
		h += 12
	case ap == "am" && h == 12:
		h = 0
	case ap == "" && m[2] == "":
		return time.Time{}, false // a bare number is not a clock time
	}
	if h > 23 || min > 59 {
		return time.Time{}, false
	}
	t := time.Date(now.Year(), now.Month(), now.Day(), h, min, 0, 0, now.Location())
	if !t.After(now) {
		t = t.AddDate(0, 0, 1)
	}
	return t, true
}

// modalRe matches the permission / trust / login prompts that block an agent
// until a human answers them.
var modalRe = regexp.MustCompile(`(?i)do you want to (proceed|make this edit|create|allow|run)|would you like to proceed|is this a project you created or one you trust|do you trust the files|❯\s*1\.\s*Yes|paste code here|press enter to (login|continue)|select login method`)

// liveModalLines is how many trailing non-empty lines can hold a live prompt: a
// permission/trust/login dialog is the bottom of the screen, not scrollback.
const liveModalLines = 14

var (
	// belowModalRe matches what only appears under a prompt that is NOT live: the
	// empty input box, the shortcuts hint or the working spinner. A real dialog
	// replaces all of them, so text above one of these is history (for example a
	// slai note quoted in the pane), not a prompt waiting for a human.
	//
	// 2.1.x draws a bare "❯" prompt (empty or with the placeholder; a selector line such
	// as "❯ No, exit" or "❯ 1. Yes" is a dialog option, not an input box) and a footer
	// that starts with the ⏵⏵ / ⏸ mode glyph.
	belowModalRe = regexp.MustCompile(`(?i)^[│|]?\s*>(\s|[│|]|$)|\? for shortcuts|esc to interrupt|^❯(\s*$| Try ")|^(⏵⏵|⏸) `)
	optionRe     = regexp.MustCompile(`^[❯>]?\s*\d+\.\s`)
	glyphRe      = regexp.MustCompile(`[│╭╮╰╯─❯⏺✻·|┃▌▐△←]`)
)

// ModalExcerpt reports whether the pane's live bottom region shows a blocking
// prompt of Claude Code's and, if so, returns a short summary of it ("" if none).
func ModalExcerpt(paneText string) string { return ModalExcerptWith(ClaudeRules(), paneText) }

// ModalExcerptWith is ModalExcerpt for any harness's rules. The summary is safe to
// put in notes and events: it names the kind of prompt and the command or file it is
// about, but never repeats the question, the selector glyph or the option lines, so
// it cannot itself look like a prompt to this detector when an agent's pane displays it.
func ModalExcerptWith(r Rules, paneText string) string {
	tail, at, ok := liveModal(r, paneText)
	if !ok {
		return ""
	}
	return summarizeModal(r, tail, at)
}

// ModalDecision is the pane decision behind a refusal to type: the live prompt
// found by the harness' rules, named "modal", with a bounded excerpt of the text
// around it. It is the zero value when no prompt is live.
func ModalDecision(r Rules, paneText string) PaneDecision {
	region := modalRegion(r, paneText)
	if region == "" {
		return PaneDecision{}
	}
	return PaneDecision{Status: StatusWaiting, OK: true, Rule: RuleModal, Excerpt: clip(region)}
}

// modalRegion is the raw pane text around a live prompt ("" when none): what a
// pane_decision event records. It is not safe to repeat to an agent.
func modalRegion(r Rules, paneText string) string {
	tail, at, ok := liveModal(r, paneText)
	if !ok {
		return ""
	}
	return strings.Join(tail[max(at-2, 0):min(at+5, len(tail))], " | ")
}

// liveModal finds the last prompt line in the pane's live bottom region, unless
// something drawn below it (an input box, a footer, the spinner) says it is history.
func liveModal(r Rules, paneText string) (tail []string, at int, ok bool) {
	if r.Modal == nil {
		return nil, 0, false
	}
	for _, l := range strings.Split(paneText, "\n") {
		if strings.TrimSpace(l) != "" {
			tail = append(tail, strings.TrimSpace(l))
		}
	}
	if len(tail) > liveModalLines {
		tail = tail[len(tail)-liveModalLines:]
	}
	last := -1
	for i, l := range tail {
		if r.Modal.MatchString(l) {
			last = i
		}
	}
	if last < 0 {
		return nil, 0, false
	}
	if r.Below != nil {
		for _, l := range tail[last+1:] {
			if r.Below.MatchString(l) {
				return nil, 0, false // the prompt scrolled past: an input box or spinner is drawn below it
			}
		}
	}
	return tail, last, true
}

func summarizeModal(r Rules, tail []string, at int) string {
	whole := strings.ToLower(strings.Join(tail[max(at-3, 0):min(at+4, len(tail))], " "))
	kind := "permission prompt"
	switch {
	case strings.Contains(whole, "trust"):
		kind = "trust prompt"
	case strings.Contains(whole, "login") || strings.Contains(whole, "paste code"):
		kind = "login prompt"
	}
	// What the prompt is about: above the question for Claude, below it for
	// harnesses that print the command or path after the title.
	lo, hi := max(at-6, 0), at
	if r.ContextAfter {
		lo, hi = at+1, min(at+7, len(tail))
	}
	var ctx []string
	for _, l := range tail[lo:hi] {
		l = strings.TrimSpace(glyphRe.ReplaceAllString(l, " "))
		if r.MainColumn { // drop a sidebar drawn to the right of the main column
			if i := strings.Index(l, "   "); i > 0 {
				l = l[:i]
			}
		}
		l = strings.Join(strings.Fields(l), " ")
		if l == "" || r.Modal.MatchString(l) || (r.Option != nil && r.Option.MatchString(l)) {
			continue
		}
		ctx = append(ctx, l)
	}
	out := kind
	if len(ctx) > 0 {
		if len(ctx) > 2 {
			if r.ContextAfter {
				ctx = ctx[:2]
			} else {
				ctx = ctx[len(ctx)-2:]
			}
		}
		out += ": " + strings.Join(ctx, " ")
	}
	out = strings.TrimSpace(r.Modal.ReplaceAllString(out, ""))
	if len(out) > 100 {
		out = out[:100] + "…"
	}
	return out
}

// Rules is the data-only description of one harness's TUI that pane detection is
// fed with: which text means working or idle, a blocking prompt, an option line,
// a usage cap and its reset time. The logic that applies them (live bottom region
// only, rejected when an input box is drawn below) is shared and stays here.
type Rules struct {
	Working, Idle, Modal, Option, RateLimit, Reset *regexp.Regexp
	// WorkingLive matches a turn in progress in the pane's last workingLiveLines lines only
	// (nil: the harness has none). Working, by contrast, may match anywhere in the tail.
	WorkingLive *regexp.Regexp
	// Below matches what is only drawn when no prompt is live (the input box, a
	// footer hint, the working spinner): a prompt-looking line above it is history.
	Below *regexp.Regexp
	// ContextAfter: the command/path a prompt is about is printed below its title.
	ContextAfter bool
	// MainColumn: the pane has a sidebar to the right of the main column; summaries
	// keep only the main column.
	MainColumn bool
	// Background matches the counts of background tasks the footer shows while the
	// harness keeps working after its turn ended (nil: the harness has no such footer).
	// Group 1 is the count, group 2 the kind. See BackgroundWith.
	Background *regexp.Regexp
	// Subagents matches the line the harness draws while subagents run for the session;
	// group 1 is the count (nil: the harness has none). See SubagentsWith.
	Subagents *regexp.Regexp
}

// ClaudeRules are the rules DetectFromPane and ModalExcerpt apply.
func ClaudeRules() Rules {
	return Rules{
		WorkingLive: workingLiveRe,
		Idle:        paneRules[0].re,
		Modal:       modalRe,
		Option:      optionRe,
		RateLimit:   rateLimitRe,
		Reset:       resetRe,
		Below:       belowModalRe,
		Background:  backgroundRe,
		Subagents:   subagentsRe,
	}
}

var (
	// spinnerLineRe is a whole live spinner line, either shape (see workingLiveRe).
	spinnerLineRe = regexp.MustCompile(`^[^\s●❯⎿\-\d] \p{Lu}[\p{L}'-]*….*\(\d[^\n]*\)\s*$`)
	toolRunningRe = regexp.MustCompile(`(?m)^\s*\(ctrl\+b ctrl\+b \(twice\) to run in background\)\s*$`)
	ruleLineRe    = regexp.MustCompile(`^\s*[─━-]{10,}\s*$`)
	usageFigureRe = regexp.MustCompile(`\d+(?:\.\d+)?%|\$\s*\d`)
)

// ToolRunning reports whether the pane's live region shows the background hint drawn
// under a tool call in flight: no hook fires until that call returns.
func ToolRunning(paneText string) bool {
	var tail []string
	for _, l := range strings.Split(paneText, "\n") {
		if strings.TrimSpace(l) != "" {
			tail = append(tail, l)
		}
	}
	live := strings.Join(tail[max(len(tail)-workingLiveLines, 0):], "\n")
	return toolRunningRe.MatchString(live)
}

// StablePane returns the pane without what moves on its own while nothing happens:
// spinner lines (glyph, elapsed time, token counts), the ctrl+b hint, and the footer
// below the input box (statusline usage figures). Only real content changes remain.
func StablePane(paneText string) string {
	lines := strings.Split(paneText, "\n")
	lastRule := -1
	for i, l := range lines {
		if ruleLineRe.MatchString(l) {
			lastRule = i
		}
	}
	if lastRule >= 0 {
		lines = lines[:lastRule+1]
	}
	out := lines[:0:0]
	for _, l := range lines {
		if spinnerLineRe.MatchString(l) || toolRunningRe.MatchString(l) || subagentsRe.MatchString(l) || usageFigureRe.MatchString(l) {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}
