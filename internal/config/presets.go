package config

import "fmt"

// PresetNames lists the available presets.
var PresetNames = []string{"solo", "squad", "full"}

// Preset builds a Team for "solo", "squad" (default for ""), or "full".
// It returns nil for an unknown name.
func Preset(name, projectName, description, mainBranch string) *Team {
	if name == "" {
		name = "squad"
	}
	if mainBranch == "" {
		mainBranch = "main"
	}
	t := &Team{
		Version: 1,
		Project: Project{
			Name:              projectName,
			Description:       description,
			MainBranch:        mainBranch,
			IntegrationBranch: DefaultIntegrationBranch,
		},
		Defaults: Defaults{
			PermissionMode:      DefaultPermissionMode,
			MaxTurnsPerTask:     DefaultMaxTurnsPerTask,
			MaxConcurrentAgents: DefaultMaxConcurrent,
			RequireReview:       true,
		},
		Models: DefaultModels(),
	}
	orch := Agent{ID: "orch", Role: RoleOrchestrator, Model: "opus", PermissionMode: DefaultWorkingPermissionMode}
	pm := Agent{ID: "pm", Role: RolePM, Model: "sonnet", PermissionMode: DefaultWorkingPermissionMode}
	principal := Agent{ID: "principal", Role: RolePrincipal, Model: "fable", PermissionMode: DefaultWorkingPermissionMode}
	eng := func(n int, focus string) Agent {
		return Agent{ID: fmt.Sprintf("eng-%d", n), Role: RoleEngineer, Model: "sonnet", Focus: focus, PermissionMode: DefaultWorkingPermissionMode}
	}
	fgNewbie := Agent{ID: "fg-newbie", Role: RoleFocusGroup, Model: "haiku", Persona: "First-time user, non-technical and unfamiliar with the product"}
	fgPower := Agent{ID: "fg-power", Role: RoleFocusGroup, Model: "haiku", Persona: "Power user who uses the product daily and wants speed and shortcuts"}
	fgSkeptic := Agent{ID: "fg-skeptic", Role: RoleFocusGroup, Model: "haiku", Persona: "Skeptical evaluator comparing against competing products"}

	switch name {
	case "solo":
		t.Defaults.RequireReview = false // no principal in this preset
		t.Defaults.MaxConcurrentAgents = 2
		t.Agents = []Agent{orch, eng(1, "")}
	case "squad":
		t.Agents = []Agent{orch, pm, principal, eng(1, "backend"), eng(2, "frontend"), fgNewbie, fgPower, fgSkeptic}
	case "full":
		t.Defaults.MaxConcurrentAgents = 8
		t.Agents = []Agent{orch, pm, principal, eng(1, "backend"), eng(2, "frontend"), eng(3, "backend"), eng(4, "frontend"), fgNewbie, fgPower, fgSkeptic}
	default:
		return nil
	}
	return t
}
