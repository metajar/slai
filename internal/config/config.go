package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Default values applied by ApplyDefaults.
const (
	DefaultIntegrationBranch = "slai/integration"
	DefaultPermissionMode    = "manual"
	// DefaultWorkingPermissionMode is what presets give the agents that run
	// commands (orchestrator, PM, principal, engineers): Claude's classifier-based
	// "auto" mode, so routine commands (go test, git status) do not stop them for
	// approval. It is not a bypass mode. Focus-group personas keep the team default.
	DefaultWorkingPermissionMode = "auto"
	DefaultMaxTurnsPerTask       = 40
	DefaultMaxConcurrent         = 5
)

// IsWorkingRole reports whether role runs commands and so gets
// DefaultWorkingPermissionMode from the presets and the wizard.
func IsWorkingRole(role string) bool { return role != RoleFocusGroup }

// PermissionModeFor returns the --permission-mode an agent launches with.
func (t *Team) PermissionModeFor(a Agent) string {
	if a.PermissionMode != "" {
		return a.PermissionMode
	}
	return t.Defaults.PermissionMode
}

// PermissionModes lists the values accepted by `claude --permission-mode`.
var PermissionModes = []string{"acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan"}

// Roles lists all valid agent roles.
var Roles = []string{RoleOrchestrator, RolePM, RolePrincipal, RoleEngineer, RoleFocusGroup}

var idRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,23}$`)

// DefaultModels returns the default alias map.
func DefaultModels() map[string]string {
	return map[string]string{
		"opus":   "opus",
		"sonnet": "sonnet",
		"haiku":  "haiku",
		"fable":  "claude-fable-5-1",
	}
}

// Load reads, parses (rejecting unknown keys), fills defaults in, and
// validates the team file at path.
func Load(path string) (*Team, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse is Load for in-memory data.
func Parse(data []byte) (*Team, error) {
	var t Team
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&t); err != nil {
		return nil, fmt.Errorf("parse team config: %w", err)
	}
	t.ApplyDefaults()
	if err := t.Validate(); err != nil {
		return nil, err
	}
	return &t, nil
}

// Marshal renders the team as YAML.
func Marshal(t *Team) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(t); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Write validates t and writes it to path.
func Write(path string, t *Team) error {
	if err := t.Validate(); err != nil {
		return err
	}
	b, err := Marshal(t)
	if err != nil {
		return err
	}
	// temp file + rename: a reader (the supervisor) never sees half a file
	tmp, err := os.CreateTemp(filepath.Dir(path), ".team-*.yaml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// ApplyDefaults fills zero-valued optional fields. Existing values are kept;
// default models are merged in only for missing aliases.
func (t *Team) ApplyDefaults() {
	if t.Project.IntegrationBranch == "" {
		t.Project.IntegrationBranch = DefaultIntegrationBranch
	}
	if t.Defaults.PermissionMode == "" {
		t.Defaults.PermissionMode = DefaultPermissionMode
	}
	if t.Defaults.MaxTurnsPerTask == 0 {
		t.Defaults.MaxTurnsPerTask = DefaultMaxTurnsPerTask
	}
	if t.Defaults.MaxConcurrentAgents == 0 {
		t.Defaults.MaxConcurrentAgents = DefaultMaxConcurrent
	}
	if t.Models == nil {
		t.Models = map[string]string{}
	}
	for k, v := range DefaultModels() {
		if _, ok := t.Models[k]; !ok {
			t.Models[k] = v
		}
	}
}

// ModelFor is the model value an agent launches with: claude agents go through the
// alias table, other harnesses take provider/model verbatim.
func (t *Team) ModelFor(a Agent) string {
	if a.HarnessKind() != HarnessClaude {
		return a.Model
	}
	return t.ResolveModel(a.Model)
}

// ResolveModel maps an agent's model (alias or raw claude-* string) to the
// value passed to --model. Unknown values are returned unchanged.
func (t *Team) ResolveModel(agentModel string) string {
	if v, ok := t.Models[agentModel]; ok {
		return v
	}
	return agentModel
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Validate checks every rule and returns all violations joined (errors.Join),
// or nil.
func (t *Team) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if t.Version != 1 {
		add("version must be 1, got %d", t.Version)
	}
	if strings.TrimSpace(t.Project.Name) == "" {
		add("project.name is required")
	}
	if t.Project.MainBranch == "" {
		add("project.main_branch is required")
	}
	if t.Project.IntegrationBranch == "" {
		add("project.integration_branch is required")
	}
	if !contains(PermissionModes, t.Defaults.PermissionMode) {
		add("defaults.permission_mode %q invalid (want one of %s)", t.Defaults.PermissionMode, strings.Join(PermissionModes, ", "))
	}
	if t.Defaults.MaxTurnsPerTask < 1 {
		add("defaults.max_turns_per_task must be >= 1, got %d", t.Defaults.MaxTurnsPerTask)
	}
	if t.Defaults.MaxConcurrentAgents < 1 {
		add("defaults.max_concurrent_agents must be >= 1, got %d", t.Defaults.MaxConcurrentAgents)
	}
	if t.Defaults.Caps.PerAgentUSD < 0 || t.Defaults.Caps.DailyUSD < 0 || t.Defaults.Caps.PerTaskUSD < 0 || t.Defaults.Caps.PerRunUSD < 0 {
		add("defaults.spend_caps values must be >= 0")
	}
	if t.Memory.PackTokens < 0 {
		add("memory.pack_tokens must be >= 0, got %d", t.Memory.PackTokens)
	}
	if t.Project.TestTimeoutSeconds < 0 {
		add("project.test_timeout_seconds must be >= 0, got %d", t.Project.TestTimeoutSeconds)
	}
	for _, e := range t.Notify.Events {
		if !contains(NotifyEvents, e) {
			add("notify.events: unknown event %q (allowed: %s)", e, strings.Join(NotifyEvents, ", "))
		}
	}
	if t.Notify.TimeoutSeconds < 0 {
		add("notify.timeout_seconds must be >= 0, got %d", t.Notify.TimeoutSeconds)
	}
	if len(t.Notify.Events) > 0 && strings.TrimSpace(t.Notify.Command) == "" {
		add("notify.events is set but notify.command is empty")
	}

	counts := map[string]int{}
	seen := map[string]bool{}
	for i, a := range t.Agents {
		if !idRe.MatchString(a.ID) {
			add("agents[%d]: id %q must match %s", i, a.ID, idRe)
		}
		if seen[a.ID] {
			add("agents[%d]: duplicate id %q", i, a.ID)
		}
		seen[a.ID] = true
		if !contains(Roles, a.Role) {
			add("agents[%d] (%s): unknown role %q", i, a.ID, a.Role)
		}
		counts[a.Role]++
		if a.PermissionMode != "" && !contains(PermissionModes, a.PermissionMode) {
			add("agents[%d] (%s): permission_mode %q invalid (want one of %s)", i, a.ID, a.PermissionMode, strings.Join(PermissionModes, ", "))
		}
		switch kind := a.HarnessKind(); {
		case !contains(Harnesses, kind):
			add("agents[%d] (%s): unknown harness %q (want one of %s)", i, a.ID, a.Harness, strings.Join(Harnesses, ", "))
		case kind != HarnessClaude:
			if prov, model, ok := strings.Cut(a.Model, "/"); !ok || strings.TrimSpace(prov) == "" || strings.TrimSpace(model) == "" {
				add("agents[%d] (%s): model %q must be provider/model for harness %s", i, a.ID, a.Model, kind)
			}
		default:
			if _, ok := t.Models[a.Model]; !ok && !strings.HasPrefix(a.Model, "claude-") {
				add("agents[%d] (%s): model %q is neither a key in models nor a raw string starting with \"claude-\"", i, a.ID, a.Model)
			}
		}
	}
	if counts[RoleOrchestrator] != 1 {
		add("exactly one orchestrator required, found %d", counts[RoleOrchestrator])
	}
	if counts[RolePrincipal] > 1 {
		add("at most one principal_engineer allowed, found %d", counts[RolePrincipal])
	}
	if counts[RolePM] > 1 {
		add("at most one product_manager allowed, found %d", counts[RolePM])
	}
	if t.Defaults.RequireReview && counts[RolePrincipal] == 0 {
		add("defaults.require_review is true but no principal_engineer is configured")
	}

	aliases := make([]string, 0, len(t.Models))
	for k := range t.Models {
		aliases = append(aliases, k)
	}
	sort.Strings(aliases)
	for _, k := range aliases {
		if t.Models[k] == "" {
			add("models.%s: value must not be empty", k)
		}
	}
	return errors.Join(errs...)
}

// PrincipalSharesEngineers reports the model and harness when the principal resolves
// to the same model and harness as every engineer (so a review is the same model
// looking at its own kind of work). ok is false when there is no principal, no engineer,
// or any engineer differs.
func (t *Team) PrincipalSharesEngineers() (model, harness string, ok bool) {
	var principal *Agent
	for i := range t.Agents {
		if t.Agents[i].Role == RolePrincipal {
			principal = &t.Agents[i]
			break
		}
	}
	if principal == nil {
		return "", "", false
	}
	pm, ph := t.ModelFor(*principal), principal.HarnessKind()
	engineers := 0
	for _, a := range t.Agents {
		if a.Role != RoleEngineer {
			continue
		}
		engineers++
		if t.ModelFor(a) != pm || a.HarnessKind() != ph {
			return "", "", false
		}
	}
	return pm, ph, engineers > 0
}
