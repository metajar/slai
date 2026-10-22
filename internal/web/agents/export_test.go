package agents

import "os/exec"

// SetLookPath fakes the machine's PATH for the harness-binary check.
func SetLookPath(f func(string) (string, error)) { lookPath = f }

// ResetLookPath restores the real lookup.
func ResetLookPath() { lookPath = exec.LookPath }

// SetPushSource gives a Push its pane source, as the module does at registration.
func SetPushSource(p *Push, src PaneSource) { p.setSource(src) }
