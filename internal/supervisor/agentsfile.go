package supervisor

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/oorbt/slai/internal/roles"
)

// AgentsFilePath is .slai/agents.json: who every agent is, for an agent (or a person) that
// has lost track. It lives under .slai, which is never tracked in git.
func AgentsFilePath(dir string) string { return filepath.Join(dir, ".slai", "agents.json") }

// writeAgentsFile rewrites .slai/agents.json from the team, only when its content changed.
// Prepare calls it on every start and every reconcile, i.e. on every team change.
func (s *Supervisor) writeAgentsFile() error {
	want := roles.AgentsFile(s.Team)
	path := AgentsFilePath(s.Dir)
	if cur, err := os.ReadFile(path); err == nil && bytes.Equal(cur, want) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, want, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
