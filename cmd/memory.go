package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/git"
	"github.com/oorbt/slai/internal/memory"
	"github.com/oorbt/slai/internal/store"
)

// Fact states shown by `slai memory ls`, doctor and the web page: one definition, in internal/memory.
const (
	factFresh      = memory.StateFresh
	factStale      = memory.StateStale
	factUnverified = memory.StateUnverified
	factInvalid    = memory.StateInvalid
)

// memoryGCDays: an unused stale fact may be collected after this long.
const memoryGCDays = 30

// memoryFileWarn: doctor and ls warn past this many files (a var so a test can lower it).
var memoryFileWarn = 500

// memEnv is one project's memory: the files, the derived index, and git.
type memEnv struct {
	dir  string // project (main checkout) directory
	mem  *memory.Memory
	repo *git.Repo
	rev  string
	test string // team.yaml project.test_command
	now  func() time.Time
	out  io.Writer
	in   io.Reader
	// tty: in is a terminal, so a prompt can be answered.
	tty bool
	// runCheck runs a fact's check command; nil means the shell (tests fake it).
	runCheck func(ctx context.Context, command string) ([]byte, error)
}

// openMemEnv opens the project at dir; the caller closes the returned store.
func openMemEnv(dir string, out io.Writer, in io.Reader) (*memEnv, *store.Store, error) {
	rev, test := config.DefaultIntegrationBranch, ""
	if team, err := config.Load(filepath.Join(dir, ".slai", "team.yaml")); err == nil {
		rev, test = team.Project.IntegrationBranch, team.Project.TestCommand
	}
	if err := os.MkdirAll(filepath.Join(dir, ".slai"), 0o755); err != nil {
		return nil, nil, err
	}
	st, err := store.Open(filepath.Join(dir, ".slai", "state.db"))
	if err != nil {
		return nil, nil, err
	}
	repo := &git.Repo{Dir: dir}
	return &memEnv{dir: dir, mem: memory.New(filepath.Join(dir, ".slai", "memory"), st, repo, rev), repo: repo, rev: rev,
		test: test, now: time.Now, out: out, in: in}, st, nil
}

// factView is a fact with its state computed now, against integration.
type factView struct {
	memory.Fact
	State  string
	Reason string // why it is stale
}

func (v factView) firstPath() string {
	if len(v.Paths) == 0 {
		return "-"
	}
	return v.Paths[0].Path
}

// stateOf: invalid files are handled by the caller. A stale fact is stale whatever
// its status; a fact that is not verified, or cites no path (so can never be grounded),
// is unverified.
func (e *memEnv) stateOf(ctx context.Context, f memory.Fact) (factView, error) {
	v, err := e.mem.View(ctx, f)
	return factView{Fact: v.Fact, State: v.State, Reason: v.Reason}, err
}

func (e *memEnv) views(ctx context.Context) ([]factView, []memory.Invalid, error) {
	facts, bad, err := e.mem.List()
	if err != nil {
		return nil, nil, err
	}
	var vs []factView
	for _, f := range facts {
		v, err := e.stateOf(ctx, f)
		if err != nil {
			return nil, nil, err
		}
		vs = append(vs, v)
	}
	return vs, bad, nil
}

func (e *memEnv) age(created string) string {
	t, err := time.Parse("2006-01-02", created)
	if err != nil {
		return "-"
	}
	d := int(e.now().UTC().Sub(t).Hours() / 24)
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%dd", d)
}

func (e *memEnv) ls(ctx context.Context, onlyStale bool) error {
	vs, bad, err := e.views(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tKIND\tSTATUS\tPATH\tAGE")
	n := 0
	for _, v := range vs {
		if onlyStale && v.State != factStale {
			continue
		}
		n++
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", v.ID, v.Kind, v.State, v.firstPath(), e.age(v.Created))
	}
	if !onlyStale {
		for _, b := range bad {
			n++
			fmt.Fprintf(tw, "%s\t-\t%s\t-\t-\n", b.ID, factInvalid)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if n == 0 {
		if onlyStale {
			fmt.Fprintln(e.out, "No stale facts.")
		} else {
			fmt.Fprintln(e.out, "No facts yet.")
		}
	}
	if !onlyStale {
		for _, b := range bad {
			fmt.Fprintf(e.out, "! %s is invalid, kept out of packs and left on disk: %s\n", b.ID, b.Reason)
		}
		if total := len(vs) + len(bad); total > memoryFileWarn {
			fmt.Fprintf(e.out, "! %d memory files (over %d): run `slai memory gc` or prune by hand\n", total, memoryFileWarn)
		}
	}
	return nil
}

// jsonFact is one row of `slai memory ls --json`.
type jsonFact struct {
	ID      string        `json:"id"`
	Kind    string        `json:"kind"`
	Status  string        `json:"status"` // the state: fresh, stale, unverified or invalid
	Reason  string        `json:"reason,omitempty"`
	Path    string        `json:"path"` // the first cited path, "" when none
	Paths   []memory.Path `json:"paths"`
	Age     string        `json:"age"`
	Created string        `json:"created"`
	Author  string        `json:"author,omitempty"`
}

// lsJSON is `slai memory ls --json`: the same facts as the table, invalid files included
// unless --stale narrows it.
func (e *memEnv) lsJSON(ctx context.Context, onlyStale bool) error {
	vs, bad, err := e.views(ctx)
	if err != nil {
		return err
	}
	out := struct {
		Facts []jsonFact `json:"facts"`
	}{Facts: []jsonFact{}}
	for _, v := range vs {
		if onlyStale && v.State != factStale {
			continue
		}
		p := v.firstPath()
		if p == "-" {
			p = ""
		}
		paths := v.Paths
		if paths == nil {
			paths = []memory.Path{}
		}
		out.Facts = append(out.Facts, jsonFact{ID: v.ID, Kind: v.Kind, Status: v.State, Reason: v.Reason, Path: p,
			Paths: paths, Age: e.age(v.Created), Created: v.Created, Author: v.Author})
	}
	if !onlyStale {
		for _, b := range bad {
			out.Facts = append(out.Facts, jsonFact{ID: b.ID, Status: factInvalid, Reason: b.Reason, Paths: []memory.Path{}, Age: "-"})
		}
	}
	return writeJSON(e.out, out)
}

// showJSON is `slai memory show <id> --json`: the fact, its recorded vs current blobs and its state.
func (e *memEnv) showJSON(ctx context.Context, id string) error {
	raw, err := os.ReadFile(filepath.Join(e.mem.Dir, id+".md"))
	if err != nil {
		return fmt.Errorf("no memory file for %q (try `slai memory ls`)", id)
	}
	type grounding struct {
		Path     string `json:"path"`
		Recorded string `json:"recorded"`
		Current  string `json:"current"` // "" when the file is gone
		State    string `json:"state"`   // ok, CHANGED or MISSING
	}
	out := struct {
		ID        string      `json:"id"`
		File      string      `json:"file"`
		Status    string      `json:"status"` // fresh, stale, unverified or invalid
		Reason    string      `json:"reason,omitempty"`
		Kind      string      `json:"kind,omitempty"`
		Author    string      `json:"author,omitempty"`
		Verified  string      `json:"verified_by,omitempty"`
		Created   string      `json:"created,omitempty"`
		Text      string      `json:"text,omitempty"`
		Check     string      `json:"check,omitempty"`
		Against   string      `json:"against"`
		Grounding []grounding `json:"grounding"`
	}{ID: id, File: string(raw), Against: e.rev, Grounding: []grounding{}}
	f, err := e.mem.Get(id)
	if err != nil {
		out.Status, out.Reason = factInvalid, err.Error()
		return writeJSON(e.out, out)
	}
	names := make([]string, len(f.Paths))
	for i, p := range f.Paths {
		names[i] = p.Path
	}
	cur, err := e.repo.LsTree(ctx, e.rev, names)
	if err != nil {
		return err
	}
	v, err := e.stateOf(ctx, f)
	if err != nil {
		return err
	}
	for _, p := range f.Paths {
		now, ok := cur[p.Path]
		g := grounding{Path: p.Path, Recorded: p.Blob, Current: now, State: "ok"}
		switch {
		case !ok:
			g.State = "MISSING"
		case now != p.Blob:
			g.State = "CHANGED"
		}
		out.Grounding = append(out.Grounding, g)
	}
	out.Status, out.Reason, out.Kind, out.Author, out.Verified, out.Created, out.Text, out.Check =
		v.State, v.Reason, f.Kind, f.Author, f.VerifiedBy, f.Created, f.Text, f.Check
	return writeJSON(e.out, out)
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func (e *memEnv) show(ctx context.Context, id string) error {
	path := filepath.Join(e.mem.Dir, id+".md")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("no memory file for %q (try `slai memory ls`)", id)
	}
	e.out.Write(raw)
	if !strings.HasSuffix(string(raw), "\n") {
		fmt.Fprintln(e.out)
	}
	f, err := e.mem.Get(id)
	if err != nil {
		fmt.Fprintf(e.out, "\nstatus: %s (%v)\n", factInvalid, err)
		return nil
	}
	names := make([]string, len(f.Paths))
	for i, p := range f.Paths {
		names[i] = p.Path
	}
	cur, err := e.repo.LsTree(ctx, e.rev, names)
	if err != nil {
		return err
	}
	v, err := e.stateOf(ctx, f)
	if err != nil {
		return err
	}
	fmt.Fprintf(e.out, "\ngrounding against %s:\n", e.rev)
	tw := tabwriter.NewWriter(e.out, 0, 0, 2, ' ', 0)
	for _, p := range f.Paths {
		now, ok := cur[p.Path]
		switch {
		case !ok:
			fmt.Fprintf(tw, "  %s\trecorded %s\tcurrent -\tMISSING\n", p.Path, short(p.Blob))
		case now != p.Blob:
			fmt.Fprintf(tw, "  %s\trecorded %s\tcurrent %s\tCHANGED\n", p.Path, short(p.Blob), short(now))
		default:
			fmt.Fprintf(tw, "  %s\trecorded %s\tcurrent %s\tok\n", p.Path, short(p.Blob), short(now))
		}
	}
	if len(f.Paths) == 0 {
		fmt.Fprintln(tw, "  (cites no path: cannot be grounded)")
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "status: %s\n", v.State)
	return nil
}

// verifyTimeout bounds a fact's check command.
var verifyTimeout = 2 * time.Minute

// verifyOpts are the flags of `slai memory verify`.
type verifyOpts struct {
	Yes             bool // run without asking (scripts)
	AllowUnverified bool // run the check of a proposed or stale fact
}

// shellCheck runs a fact's check command in the project directory.
func (e *memEnv) shellCheck(ctx context.Context, command string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()
	c := exec.CommandContext(cctx, "sh", "-c", command)
	c.Dir = e.dir
	return c.CombinedOutput()
}

// verify shows the fact's check command, asks before running it, runs it (a human ran
// this command; agents and the supervisor never do) and, on exit 0, records
// verified_by: user. A check comes from a fact file anyone can edit, so it runs only
// after a y answer (or --yes), never without a terminal to ask on, and a proposed or
// stale fact's check also needs --allow-unverified.
func (e *memEnv) verify(ctx context.Context, id string, o verifyOpts) error {
	f, err := e.mem.Get(id)
	if err != nil {
		return fmt.Errorf("fact %s: %w", id, err)
	}
	if strings.TrimSpace(f.Check) == "" {
		return fmt.Errorf("fact %s has no check command to run", id)
	}
	v, err := e.stateOf(ctx, f)
	if err != nil {
		return err
	}
	if !o.AllowUnverified && (f.Status != memory.StatusVerified || v.State == factStale) {
		why := "proposed"
		if v.State == factStale {
			why = "stale (" + v.Reason + ")"
		}
		return fmt.Errorf("not running the check of %s: the fact is %s, and its check is unreviewed text. Read it with `slai memory show %s`, then pass --allow-unverified to run it", id, why, id)
	}
	who := f.Author
	if f.VerifiedBy != "" {
		who += ", verified by " + f.VerifiedBy
	}
	fmt.Fprintf(e.out, "fact %s (%s)\ncheck command:\n  %s\n", f.ID, who, f.Check)
	if !o.Yes {
		if !e.tty {
			return fmt.Errorf("not running the check: there is no terminal to ask you on. Pass --yes to run it without asking")
		}
		fmt.Fprint(e.out, "Run this check? [y/N] ")
		ans, _ := bufio.NewReader(e.in).ReadString('\n')
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(ans)), "y") {
			fmt.Fprintln(e.out, "cancelled; nothing ran")
			return nil
		}
	}
	run := e.runCheck
	if run == nil {
		run = e.shellCheck
	}
	out, err := run(ctx, f.Check)
	e.out.Write(out)
	if err != nil {
		return fmt.Errorf("check failed (%v): %s stays %s", err, id, f.Status)
	}
	if _, err := e.mem.MarkVerified(id, "user"); err != nil {
		return err
	}
	fmt.Fprintf(e.out, "%s verified (verified_by: user)\n", id)
	if v.State == factStale {
		fmt.Fprintf(e.out, "note: it still cites changed paths (%s), so it stays stale until it is rewritten against the current code\n", v.Reason)
	}
	return nil
}

// gcCandidates are stale facts nobody has used for memoryGCDays (never in a pack, or
// last in one that long ago; a fact never used counts from its creation).
func (e *memEnv) gcCandidates(ctx context.Context) ([]factView, error) {
	vs, _, err := e.views(ctx)
	if err != nil {
		return nil, err
	}
	rows := map[string]store.FactRow{}
	if rs, err := e.mem.DB.ListFacts(); err == nil {
		for _, r := range rs {
			rows[r.ID] = r
		}
	}
	cutoff := e.now().AddDate(0, 0, -memoryGCDays)
	var out []factView
	for _, v := range vs {
		if v.State != factStale {
			continue
		}
		last, _ := time.Parse("2006-01-02", v.Created)
		if r, ok := rows[v.ID]; ok && r.LastUsedAt > 0 {
			last = time.Unix(r.LastUsedAt, 0)
		}
		if last.IsZero() || last.After(cutoff) {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// gc lists the collectable facts and deletes them after confirmation (yes skips the
// question). Only this command deletes memory files; no tick does.
func (e *memEnv) gc(ctx context.Context, yes bool) error {
	cands, err := e.gcCandidates(ctx)
	if err != nil {
		return err
	}
	if len(cands) == 0 {
		fmt.Fprintf(e.out, "Nothing to collect: no stale fact has been unused for %d days.\n", memoryGCDays)
		return nil
	}
	fmt.Fprintf(e.out, "Stale facts unused for %d+ days:\n", memoryGCDays)
	for _, v := range cands {
		fmt.Fprintf(e.out, "  %s  %s  %s (%s)\n", v.ID, v.Kind, v.firstPath(), v.Reason)
	}
	if !yes {
		fmt.Fprintf(e.out, "Delete %d fact(s)? [y/N] ", len(cands))
		ans, _ := bufio.NewReader(e.in).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
			fmt.Fprintln(e.out, "Nothing deleted.")
			return nil
		}
	}
	for _, v := range cands {
		if err := e.mem.Delete(v.ID); err != nil {
			return err
		}
	}
	fmt.Fprintf(e.out, "Deleted %d fact(s).\n", len(cands))
	return nil
}

func (e *memEnv) projectMap(ctx context.Context) error {
	vs, _, err := e.views(ctx)
	if err != nil {
		return err
	}
	opt := memory.MapOptions{TestCommand: e.test}
	for _, v := range vs {
		if v.Kind == memory.KindCommand && v.State != factStale {
			opt.Commands = append(opt.Commands, v.Text)
		}
	}
	s, err := memory.ProjectMap(e.dir, opt)
	if err != nil {
		return err
	}
	_, err = io.WriteString(e.out, s)
	return err
}

func memoryCmd() *cobra.Command {
	c := &cobra.Command{Use: "memory", Short: "Inspect the team's shared fact memory (.slai/memory)"}
	run := func(f func(context.Context, *memEnv) error) func(*cobra.Command, []string) error {
		return func(c *cobra.Command, _ []string) error {
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			e, st, err := openMemEnv(dir, c.OutOrStdout(), c.InOrStdin())
			if err != nil {
				return err
			}
			defer st.Close()
			return f(c.Context(), e)
		}
	}
	var onlyStale, yes, vYes, allowUnverified, lsJSON, showJSON bool
	ls := &cobra.Command{Use: "ls", Short: "List facts with their state (fresh, stale, unverified, invalid)", Args: cobra.NoArgs}
	ls.RunE = run(func(ctx context.Context, e *memEnv) error {
		if lsJSON {
			return e.lsJSON(ctx, onlyStale)
		}
		return e.ls(ctx, onlyStale)
	})
	ls.Flags().BoolVar(&lsJSON, "json", false, "machine-readable output")
	ls.Flags().BoolVar(&onlyStale, "stale", false, "only stale facts")
	show := &cobra.Command{Use: "show <id>", Short: "Print a fact file and its recorded vs current blobs", Args: cobra.ExactArgs(1)}
	show.RunE = func(c *cobra.Command, args []string) error {
		return run(func(ctx context.Context, e *memEnv) error {
			if showJSON {
				return e.showJSON(ctx, args[0])
			}
			return e.show(ctx, args[0])
		})(c, args)
	}
	show.Flags().BoolVar(&showJSON, "json", false, "machine-readable output")
	verify := &cobra.Command{Use: "verify <id>", Short: "Show the fact's check command, ask, run it; on exit 0 mark it verified by you", Args: cobra.ExactArgs(1)}
	verify.RunE = func(c *cobra.Command, args []string) error {
		return run(func(ctx context.Context, e *memEnv) error {
			if fi, err := os.Stdin.Stat(); err == nil && c.InOrStdin() == os.Stdin {
				e.tty = fi.Mode()&os.ModeCharDevice != 0
			}
			return e.verify(ctx, args[0], verifyOpts{Yes: vYes, AllowUnverified: allowUnverified})
		})(c, args)
	}
	verify.Flags().BoolVar(&vYes, "yes", false, "run the check without asking (for scripts)")
	verify.Flags().BoolVar(&allowUnverified, "allow-unverified", false, "also run the check of a proposed or stale fact")
	gc := &cobra.Command{Use: "gc", Short: "Delete stale facts unused for 30 days (asks first)", Args: cobra.NoArgs}
	gc.RunE = run(func(ctx context.Context, e *memEnv) error { return e.gc(ctx, yes) })
	gc.Flags().BoolVar(&yes, "yes", false, "delete without asking")
	mp := &cobra.Command{Use: "map", Short: "Print the generated project map", Args: cobra.NoArgs}
	mp.RunE = run(func(ctx context.Context, e *memEnv) error { return e.projectMap(ctx) })
	c.AddCommand(ls, show, verify, gc, mp)
	return c
}

// doctorMemory prints the "team memory" section and returns the number of problems
// (warnings do not count). Nothing is printed for a project with no memory yet.
func doctorMemory(ctx context.Context, dir string, w io.Writer) int {
	if _, err := os.Stat(filepath.Join(dir, ".slai", "memory")); err != nil {
		return 0
	}
	e, st, err := openMemEnv(dir, w, nil)
	if err != nil {
		fmt.Fprintf(w, "✗ team memory: %v\n", err)
		return 1
	}
	defer st.Close()
	vs, bad, err := e.views(ctx)
	if err != nil {
		fmt.Fprintf(w, "✗ team memory: %v\n", err)
		return 1
	}
	count := map[string]int{}
	var staleVerified []string
	for _, v := range vs {
		count[v.State]++
		if v.State == factStale && v.Status == memory.StatusVerified {
			staleVerified = append(staleVerified, v.ID)
		}
	}
	sort.Strings(staleVerified)
	fmt.Fprintf(w, "✓ team memory: %d fact(s): %d fresh, %d stale, %d unverified, %d invalid\n",
		len(vs)+len(bad), count[factFresh], count[factStale], count[factUnverified], len(bad))
	for _, b := range bad {
		fmt.Fprintf(w, "! warn: memory file %s is invalid and kept out of packs: %s\n", b.ID, b.Reason)
	}
	for _, id := range staleVerified {
		fmt.Fprintf(w, "! warn: verified fact %s is stale (`slai memory show %s`)\n", id, id)
	}
	if total := len(vs) + len(bad); total > memoryFileWarn {
		fmt.Fprintf(w, "! warn: %d memory files (over %d): run `slai memory gc` or prune by hand\n", total, memoryFileWarn)
	}
	return 0
}
