package config

import (
	"errors"
	"fmt"
)

// idPrefix is the id stem SuggestID uses per role.
var idPrefix = map[string]string{
	RoleOrchestrator: "orch",
	RolePM:           "pm",
	RolePrincipal:    "principal",
	RoleEngineer:     "eng",
	RoleFocusGroup:   "fg",
}

// SuggestID returns a free agent id for role: eng-N for engineers, fg-N for
// focus groups (the smallest free N), the plain role name for singleton roles
// ("pm"), with -N appended when that is taken.
func SuggestID(t *Team, role string) string {
	prefix, ok := idPrefix[role]
	if !ok {
		prefix = "agent"
	}
	taken := map[string]bool{}
	for _, a := range t.Agents {
		taken[a.ID] = true
	}
	numbered := role == RoleEngineer || role == RoleFocusGroup
	if !numbered && !taken[prefix] {
		return prefix
	}
	start := 1
	if !numbered {
		start = 2
	}
	for n := start; ; n++ {
		if id := fmt.Sprintf("%s-%d", prefix, n); !taken[id] {
			return id
		}
	}
}

// DefaultModelFor is the model alias the presets give a role.
func DefaultModelFor(role string) string {
	switch role {
	case RoleOrchestrator:
		return "opus"
	case RolePrincipal:
		return "fable"
	case RoleFocusGroup:
		return "haiku"
	}
	return "sonnet"
}

// AddAgent appends a to t and validates the result. On error t is unchanged and
// the error lists only what the new agent broke (id, role, model, mode, or a
// singleton role already filled), not problems the team already had.
func AddAgent(t *Team, a Agent) error {
	before := map[string]bool{}
	for _, e := range unwrapAll(t.Validate()) {
		before[e.Error()] = true
	}
	t.Agents = append(t.Agents, a)
	var errs []error
	for _, e := range unwrapAll(t.Validate()) {
		if !before[e.Error()] {
			errs = append(errs, e)
		}
	}
	if len(errs) > 0 {
		t.Agents = t.Agents[:len(t.Agents)-1]
		return errors.Join(errs...)
	}
	return nil
}

func unwrapAll(err error) []error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		return j.Unwrap()
	}
	return []error{err}
}

// HarnessBinary is the executable a harness needs on PATH.
func HarnessBinary(kind string) string {
	switch kind {
	case HarnessOpenCode:
		return "opencode"
	case HarnessPi:
		return "pi"
	}
	return "claude"
}

// ModelHint tells the user what to type as the model for a harness.
func ModelHint(kind string) string {
	if kind == HarnessClaude || kind == "" {
		return "alias or claude-* id (opus, sonnet, haiku, fable)"
	}
	return "provider/model, e.g. zai/glm-5.3"
}

// CheckHarnessBinary returns an error naming the missing executable when the
// harness cannot be launched on this machine. lookPath is exec.LookPath in production.
func CheckHarnessBinary(kind string, lookPath func(string) (string, error)) error {
	bin := HarnessBinary(kind)
	if _, err := lookPath(bin); err != nil {
		return fmt.Errorf("%s not found on PATH (harness %s)", bin, kind)
	}
	return nil
}
