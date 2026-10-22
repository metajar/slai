package cmd

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"

	"github.com/oorbt/slai/internal/buildinfo"
)

// Set via -ldflags "-X ...cmd.version=... -X ...cmd.commit=... -X ...date=..."; used
// only when the binary carries no VCS stamp (go run, go build -buildvcs=false).
var (
	version = "" // release version (goreleaser); overrides buildinfo.Version() when set
	commit  = ""
	date    = ""
)

// vcsInfo is the VCS stamp Go embeds in the binary.
type vcsInfo struct {
	Revision string
	Time     string
	Dirty    bool
}

func readVCS() vcsInfo {
	v := vcsInfo{Revision: commit, Time: date}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				v.Revision = s.Value
			case "vcs.time":
				v.Time = s.Value
			case "vcs.modified":
				v.Dirty = s.Value == "true"
			}
		}
	}
	return v
}

func versionString() string {
	v := readVCS()
	rev := v.Revision
	if rev == "" {
		rev = "unknown"
	} else if len(rev) > 12 {
		rev = rev[:12]
	}
	if v.Dirty {
		rev += " (dirty)"
	}
	built := v.Time
	if built == "" {
		built = "unknown"
	}
	ver := buildinfo.Version()
	if version != "" {
		ver = version
	}
	return fmt.Sprintf("slai %s\nbuild %s\nrevision %s\nbuilt %s\n%s/%s",
		ver, buildinfo.ID(), rev, built, runtime.GOOS, runtime.GOARCH)
}

func versionCmd() *cobra.Command {
	return &cobra.Command{Use: "version", Short: "Print the slai version and build", Args: cobra.NoArgs,
		Run: func(c *cobra.Command, _ []string) { fmt.Fprintln(c.OutOrStdout(), versionString()) }}
}
