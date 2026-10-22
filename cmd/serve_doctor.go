package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/oorbt/slai/internal/config"
	"github.com/oorbt/slai/internal/tmux"
)

// DoctorReport is the web Doctor page's source: the text `slai doctor` prints (it calls the
// same runDoctorChecks) and its failure count. It never passes --fix.
func (a *webActions) DoctorReport(ctx context.Context) (string, int, error) {
	var b strings.Builder
	bad, err := runDoctorChecks(ctx, a.Dir, false, &b)
	return b.String(), bad, err
}

// DoctorProbe runs `slai doctor --lane` or `--panes` for the page's Run button and returns
// what the command prints. Both are read-only: --lane sends synthetic payloads to the guard
// function (nothing is executed or written) and --panes only captures pane text.
func (a *webActions) DoctorProbe(ctx context.Context, which string) (string, error) {
	var b strings.Builder
	switch which {
	case "lane":
		team, err := config.Load(filepath.Join(a.Dir, ".slai", "team.yaml"))
		if err != nil {
			return "", err
		}
		bad, err := doctorLane(team, a.Dir, "", &b)
		if err != nil {
			return "", err
		}
		if bad > 0 {
			fmt.Fprintf(&b, "%d lane guard probe(s) failed\n", bad)
		}
	case "panes":
		if err := doctorPanes(ctx, a.Dir, tmux.New(socketName()), &b); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unknown probe %q (want lane or panes)", which)
	}
	return b.String(), nil
}
