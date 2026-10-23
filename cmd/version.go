package cmd

import (
	"encoding/json"
	"fmt"
	"io"
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

// versionInfo is what `slai version` reports. The text form prints more (build time, platform);
// --json is {version, commit, build}.
type versionInfo struct {
	Version  string `json:"version"`
	Commit   string `json:"commit"` // "unknown" when the binary carries no VCS stamp
	Build    string `json:"build"`
	Dirty    bool   `json:"-"`
	Built    string `json:"-"`
	OS, Arch string `json:"-"`
}

func readVersion() versionInfo {
	v := readVCS()
	rev := v.Revision
	if rev == "" {
		rev = "unknown"
	} else if len(rev) > 12 {
		rev = rev[:12]
	}
	built := v.Time
	if built == "" {
		built = "unknown"
	}
	ver := buildinfo.Version()
	if version != "" {
		ver = version
	}
	return versionInfo{Version: ver, Build: buildinfo.ID(), Commit: rev, Dirty: v.Dirty, Built: built, OS: runtime.GOOS, Arch: runtime.GOARCH}
}

func versionString() string {
	v := readVersion()
	rev := v.Commit
	if v.Dirty {
		rev += " (dirty)"
	}
	return fmt.Sprintf("slai %s\nbuild %s\nrevision %s\nbuilt %s\n%s/%s", v.Version, v.Build, rev, v.Built, v.OS, v.Arch)
}

// writeJSON prints v as one indented JSON document (the style of `slai status --json`).
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func versionCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{Use: "version", Short: "Print the slai version and build", Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if asJSON {
				return writeJSON(c.OutOrStdout(), readVersion())
			}
			fmt.Fprintln(c.OutOrStdout(), versionString())
			return nil
		}}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	return c
}
