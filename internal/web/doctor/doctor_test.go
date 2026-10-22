package doctor_test

import (
	"context"
	"errors"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/web/doctor"
	"github.com/oorbt/slai/internal/web/server"
	"github.com/oorbt/slai/internal/web/webtest"
)

// src is a fake doctor: the report text it returns is what `slai doctor` would print.
type src struct {
	server.Actions
	mu       sync.Mutex
	text     string
	fails    int
	err      error
	reports  int
	probes   []string
	probeOut map[string]string
	probeErr error
}

func (s *src) DoctorReport(context.Context) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reports++
	return s.text, s.fails, s.err
}

func (s *src) DoctorProbe(_ context.Context, which string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probes = append(s.probes, which)
	return s.probeOut[which], s.probeErr
}

const sample = `✓ tmux    tmux 3.4
✓ git     git version 2.43.0
✓ claude  2.1.307 (Claude Code)
! warn: integration branch slai/integration missing (created by ` + "`slai up`" + `)
✓ team.yaml valid (5 agents)
! warn: orphan tmux session slai-old-eng-1 (not in team.yaml)
team health:
  ⚠ supervisor is running an older slai build (pid 12, build abc, installed def); it re-execs itself on the next tick
  eng-1        alive        reason=- last-activity=3s ago escalation=none
hook/mcp errors:
  ✗ eng-2 mcp errors: 2 (within the last hour) [no slai tool call since], latest: timeout
      meaning: the agent's slai MCP server reported an error, so its slai tool calls may have failed
      next: check the agent (` + "`slai doctor --panes`" + `); restart it if its slai tools keep failing
  ! warn: eng-1 mcp errors: 1 (within the last hour) [slow, recovered: a later slai call succeeded], latest: slow
      meaning: the agent's slai MCP server reported an error
      next: ignore if it can still call slai tools
- no state.db yet
`

func newH(t *testing.T, s *src, opts ...webtest.Option) *webtest.Harness {
	if s == nil {
		s = &src{text: sample, fails: 1}
	}
	h := webtest.New(t, append([]webtest.Option{webtest.WithModules(doctor.Module),
		webtest.WithActionsFunc(func(*webtest.Harness) server.Actions { return s })}, opts...)...)
	h.Login()
	return h
}

// P9 AC1: the page shows the checks `slai doctor` printed, each warning and failure with
// its hint; the exit state is a banner naming every one of them.
func TestDoctorPageShowsChecksHintsAndSummary(t *testing.T) {
	s := &src{text: sample, fails: 1}
	h := newH(t, s)
	r := h.Get("/doctor")
	h.CheckPage(r)
	b := r.Body
	for _, want := range []string{
		"tmux 3.4", "valid (5 agents)", // ok rows
		"integration branch slai/integration missing", // warn
		"orphan tmux session", "older slai build",
		"meaning: the agent&#39;s slai MCP server reported an error, so its slai tool calls may have failed",
		"next: check the agent (`slai doctor --panes`)",
		"no state.db yet", "team health:",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// the banner names every failure and warning (focus-group AC 25)
	m := regexp.MustCompile(`<strong>([^<]*)</strong>`).FindAllStringSubmatch(b, -1)
	var banner string
	for _, x := range m {
		if strings.Contains(x[1], "failure") || strings.Contains(x[1], "warning") || strings.Contains(x[1], "All checks") {
			banner = x[1]
		}
	}
	for _, want := range []string{"1 failure: mcp", "4 warnings: integration branch, orphans, build, mcp"} {
		if !strings.Contains(banner, want) {
			t.Errorf("summary %q lacks %q", banner, want)
		}
	}
	// MCP timeout lines follow the T-0051 grading: unrecovered is a fail chip, recovered is a warning
	if !regexp.MustCompile(`chip chip-bad">fail</span>\s*<div><span>eng-2 mcp errors: 2`).MatchString(b) {
		t.Error("an MCP error with no later call must be a failure")
	}
	if !regexp.MustCompile(`chip chip-warn">warn</span>\s*<div><span>eng-1 mcp errors: 1[^<]*slow, recovered`).MatchString(b) {
		t.Error("a recovered MCP error must be a warning")
	}
	if !strings.Contains(b, "banner-bad") {
		t.Error("a failure makes the banner red")
	}
	// the exit state is a banner, not an exit: the page is a 200
	if r.Code != 200 {
		t.Fatalf("status %d", r.Code)
	}
}

func TestAllClearAndWarningsOnlyBanners(t *testing.T) {
	ok := newH(t, &src{text: "✓ tmux  tmux 3.4\n✓ git repository\n"})
	if b := ok.Get("/doctor").Body; !strings.Contains(b, "All checks passed.") || !strings.Contains(b, "banner-info") {
		t.Errorf("all clear:\n%s", b)
	}
	warn := newH(t, &src{text: "✓ tmux  tmux 3.4\n! warn: orphan tmux session x (not in team.yaml)\n"})
	b := warn.Get("/doctor").Body
	if !strings.Contains(b, "1 warning: orphans") || !strings.Contains(b, "banner-hold") || strings.Contains(b, "banner-bad") {
		t.Errorf("warnings only:\n%s", b)
	}
	empty := newH(t, &src{text: ""})
	if !strings.Contains(empty.Get("/doctor").Body, "The doctor printed nothing.") {
		t.Error("empty report")
	}
}

func TestReportIsCachedAndRerunBypassesIt(t *testing.T) {
	s := &src{text: sample, fails: 1}
	h := newH(t, s)
	h.Get("/doctor")
	h.Get("/doctor")
	if s.reports != 1 {
		t.Fatalf("report computed %d times within the cache window, want 1", s.reports)
	}
	h.Get("/doctor?rerun=1")
	if s.reports != 2 {
		t.Fatalf("re-run did not recompute: %d", s.reports)
	}
	h.Advance(doctor.CacheFor + time.Second)
	h.Get("/doctor")
	if s.reports != 3 {
		t.Fatalf("an expired cache did not recompute: %d", s.reports)
	}
	s.err = errors.New("git exploded")
	h.Get("/doctor?rerun=1")
	if !strings.Contains(h.Get("/doctor").Body, "git exploded") {
		t.Error("an error from the checks must be shown")
	}
}

// P9 AC2: --lane and --panes are POSTs that run the probes and show their results.
func TestProbesRunOnPostAndShowResults(t *testing.T) {
	s := &src{text: sample, probeOut: map[string]string{
		"lane":  "✓ eng-1      claude   engineer           write outside the worktree: deny\n",
		"panes": "pane decisions:\n  eng-2        claude    pane: working       rule: claude.working_live\n",
	}}
	h := newH(t, s)
	if b := h.Get("/doctor").Body; strings.Count(b, "Not run from the web yet.") != 2 || !strings.Contains(b, "slai doctor --lane") || !strings.Contains(b, "slai doctor --panes") {
		t.Fatalf("before running:\n%s", b)
	}
	for _, which := range []string{"lane", "panes"} {
		r := h.Post("/doctor/run", url.Values{"which": {which}})
		if r.Code != 303 || r.Header.Get("Location") != "/doctor" {
			t.Fatalf("run %s = %d", which, r.Code)
		}
	}
	if strings.Join(s.probes, ",") != "lane,panes" {
		t.Fatalf("probes run: %v", s.probes)
	}
	b := h.Get("/doctor").Body
	for _, want := range []string{"write outside the worktree: deny", "rule: claude.working_live", "slai doctor --lane · last run", "(just now)"} {
		if !strings.Contains(b, want) {
			t.Errorf("results lack %q", want)
		}
	}
	// the htmx form reloads the page; unknown probes and probe errors are shown, nothing stored
	req := h.Browser("POST", "/doctor/run", url.Values{"which": {"lane"}})
	req.Header.Set("HX-Request", "true")
	if r := h.Do(req); r.Code != 204 || r.Header.Get("HX-Redirect") != "/doctor" {
		t.Fatalf("htmx run = %d %v", r.Code, r.Header)
	}
	if r := h.Post("/doctor/run", url.Values{"which": {"rm -rf"}}); r.Code != 422 || !strings.Contains(r.Body, "unknown probe") {
		t.Fatalf("unknown probe = %d", r.Code)
	}
	s.probeErr = errors.New("no valid .slai/team.yaml here")
	if r := h.Post("/doctor/run", url.Values{"which": {"panes"}}); r.Code != 422 || !strings.Contains(r.Body, "no valid .slai/team.yaml here") {
		t.Fatalf("probe error = %d %s", r.Code, r.Body)
	}
}

// Probe output is data: escaped, and each run is one web_action.
func TestProbeOutputIsEscapedAndLogged(t *testing.T) {
	s := &src{text: sample, probeOut: map[string]string{"panes": `matched: "<script>alert(1)</script>"`}}
	h := newH(t, s)
	h.Post("/doctor/run", url.Values{"which": {"panes"}})
	b := h.Get("/doctor").Body
	if strings.Contains(b, "<script>alert(1)") || !strings.Contains(b, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("probe output not escaped")
	}
	acts := h.WebActions()
	if len(acts) != 1 || acts[0]["action"] != "doctor_probe" || acts[0]["target"] != "panes" {
		t.Fatalf("web_action events: %v", acts)
	}
}

// Read-only: no Run button, the command to copy instead, and the POST is refused.
func TestReadOnlyDoctorShowsCommandsNotButtons(t *testing.T) {
	s := &src{text: sample}
	h := newH(t, s, webtest.ReadOnly())
	b := h.Get("/doctor").Body
	if strings.Contains(b, "hx-post") || strings.Count(b, "Read-only mode: run it from a terminal") != 2 {
		t.Errorf("read-only page:\n%s", b)
	}
	if r := h.Post("/doctor/run", url.Values{"which": {"lane"}}); r.Code != 403 || len(s.probes) != 0 {
		t.Fatalf("read-only run = %d probes=%v", r.Code, s.probes)
	}
}

// Without the CLI's doctor behind it the page says so rather than failing.
func TestNoSourceMessage(t *testing.T) {
	h := webtest.New(t, webtest.WithModules(doctor.Module))
	h.Login()
	r := h.Get("/doctor")
	if r.Code != 200 || !strings.Contains(r.Body, "no connection to the doctor checks") {
		t.Fatalf("%d %s", r.Code, r.Body)
	}
}

func TestParseLabels(t *testing.T) {
	rep := doctor.Parse("! warn: stale supervisor pidfile /x (no lock holder)\n! warn: worktree for eng-1 missing\n! warn: something unusual happened here today\n✗ state.db: unable to open\n")
	if got := strings.Join(rep.Warnings, ","); got != "supervisor pidfile,worktrees,something unusual happened" {
		t.Errorf("warnings %q", got)
	}
	if got := strings.Join(rep.Failures, ","); got != "state.db" {
		t.Errorf("failures %q", got)
	}
}
