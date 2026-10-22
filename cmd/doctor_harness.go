package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/harness"
)

// doctorHarnesses prints the "Harnesses" section: for every non-Claude harness a
// team uses, whether its binary and the provider auth of its models are in place,
// whether the generated plugin/extension files exist, and what is degraded for it.
// It returns the number of problems (a missing binary or missing auth).
func doctorHarnesses(ctx context.Context, dir string, w io.Writer) int {
	team, err := config.Load(filepath.Join(dir, ".slai", "team.yaml"))
	if err != nil {
		return 0
	}
	byKind := map[string][]config.Agent{}
	for _, a := range team.Agents {
		if a.HarnessKind() != config.HarnessClaude {
			byKind[a.HarnessKind()] = append(byKind[a.HarnessKind()], a)
		}
	}
	if len(byKind) == 0 {
		return 0
	}
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	fmt.Fprintln(w, "Harnesses:")
	bad := 0
	for _, kind := range kinds {
		agents := byKind[kind]
		path, err := exec.LookPath(kind)
		if err != nil {
			fmt.Fprintf(w, "  ✗ %-9s binary not found on PATH (needed by %s)\n", kind, agentIDs(agents))
			bad++
			continue
		}
		fmt.Fprintf(w, "  ✓ %-9s %s\n", kind, firstOutputLine(runQuiet(ctx, path, nil, "--version")))
		for _, prov := range providersOf(agents) {
			if ok, why := providerAuth(ctx, kind, path, prov); ok {
				fmt.Fprintf(w, "    ✓ auth for provider %s\n", prov)
			} else {
				fmt.Fprintf(w, "    ✗ no auth for provider %s (%s)\n", prov, why)
				bad++
			}
		}
		h, herr := harness.For(harness.Kind(kind))
		if herr != nil {
			continue
		}
		if fl, ok := h.(interface{ RunFileNames() []string }); ok {
			for _, a := range agents {
				var missing []string
				for _, f := range fl.RunFileNames() {
					if _, err := os.Stat(filepath.Join(dir, ".slai", "run", a.ID, f)); err != nil {
						missing = append(missing, f)
					}
				}
				if len(missing) > 0 {
					fmt.Fprintf(w, "    ! warn: %s: run files not generated yet (%s); `slai up` writes them\n", a.ID, strings.Join(missing, ", "))
				}
			}
		}
		for _, a := range agents {
			if d := h.Features().Degraded(); len(d) > 0 {
				var parts []string
				for _, f := range d {
					parts = append(parts, f+" (not supported by "+kind+")")
				}
				fmt.Fprintf(w, "    - %s: degraded: %s\n", a.ID, strings.Join(parts, ", "))
			}
		}
		switch kind {
		case config.HarnessPi:
			fmt.Fprintln(w, "    - note: pi's `-e builtin:mcp` also connects every MCP server in your ~/.pi/agent/mcp.json, not only slai's")
		case config.HarnessOpenCode:
			// A broken user config makes every opencode command fail; slai isolates itself
			// from it, so this is a warning for the user's own opencode, never a failure.
			if _, err := exec.CommandContext(ctx, path, "debug", "config").CombinedOutput(); err != nil {
				fmt.Fprintln(w, "    ! warn: your opencode config is invalid (`opencode debug config` fails); slai agents are isolated from it, but your own opencode is affected")
			}
		}
	}
	return bad
}

func agentIDs(as []config.Agent) string {
	ids := make([]string, len(as))
	for i, a := range as {
		ids[i] = a.ID
	}
	return strings.Join(ids, ", ")
}

// providersOf lists the distinct providers named by the agents' provider/model strings.
func providersOf(as []config.Agent) []string {
	seen := map[string]bool{}
	var out []string
	for _, a := range as {
		if p, _, ok := strings.Cut(a.Model, "/"); ok && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// providerAuth asks the harness's own CLI whether it has credentials for provider.
func providerAuth(ctx context.Context, kind, bin, provider string) (ok bool, why string) {
	switch kind {
	case config.HarnessPi:
		out, err := exec.CommandContext(ctx, bin, "auth", "check", "--provider", provider, "--json").Output()
		if err == nil && strings.Contains(strings.ReplaceAll(string(out), " ", ""), `"status":"ready"`) {
			return true, ""
		}
		return false, "`pi auth check --provider " + provider + "` is not ready"
	case config.HarnessOpenCode:
		// An isolated, empty config dir: the user's own opencode config may be invalid,
		// and auth lives in the data dir, which stays reachable.
		cfgDir, err := os.MkdirTemp("", "slai-doctor-oc-")
		if err != nil {
			return false, err.Error()
		}
		defer os.RemoveAll(cfgDir)
		out := runQuiet(ctx, bin, []string{"XDG_CONFIG_HOME=" + cfgDir}, "providers", "list")
		if strings.Contains(strings.ToLower(out), strings.ToLower(provider)) {
			return true, ""
		}
		return false, "not listed by `opencode providers list`"
	}
	return false, "unknown harness"
}

func runQuiet(ctx context.Context, bin string, env []string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), env...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	_ = cmd.Run()
	return buf.String()
}

func firstOutputLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
