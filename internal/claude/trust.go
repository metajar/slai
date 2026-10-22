package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// IsTrusted reports (read-only) whether dir or a parent has accepted Claude
// Code's "do you trust this folder" dialog, which blocks every new agent until
// someone answers it. slai never edits Claude's config to answer it for you.
func IsTrusted(dir string) bool {
	path := filepath.Join(os.Getenv("HOME"), ".claude.json")
	if p := os.Getenv("CLAUDE_CONFIG_DIR"); p != "" {
		path = filepath.Join(p, ".claude.json")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var cfg struct {
		Projects map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return false
	}
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if cfg.Projects[d].Trusted {
			return true
		}
		if d == filepath.Dir(d) {
			return false
		}
	}
}
