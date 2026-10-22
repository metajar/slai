package harness

import "path/filepath"

// RunFileNames are the generated files `slai doctor` checks exist for a pi agent
// (opencode lists its own in opencode.go).
func (piHarness) RunFileNames() []string {
	return []string{"system.md", filepath.Base(PiExtensionPath("", "x"))}
}
