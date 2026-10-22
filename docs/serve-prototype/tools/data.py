"""Fake but realistic data for the slai serve prototype, drawn from this very team.
Nothing here is read from a real store; it is only used by gen.py."""

# liveness key -> (chip class, icon, label)
LIVE = {
    "working":      ("ok",   "activity",        "working"),
    "idle":         ("idle", "moon-stars",      "idle"),
    "needs_you":    ("user", "hand-stop",       "needs you"),
    "stuck":        ("bad",  "lock-exclamation", "stuck on a prompt"),
    "hooks":        ("warn", "plug-connected-x", "hooks failing"),
    "gated":        ("warn", "player-pause",    "gated"),
    "dead":         ("bad",  "skull",           "dead"),
    "limited":      ("warn", "clock-pause",     "rate limited"),
}

AGENTS = [
    # id, role, group, harness, model, live, reason, task, last, spend_today
    dict(id="orch", role="orchestrator", group="Orchestrator", harness="claude", model="opus", live="working",
         reason="hook 9s ago · answering fg-newbie", task="", last="9s ago", spend=14.20, av="orch"),
    dict(id="pm", role="product_manager", group="Planning", harness="claude", model="sonnet", live="idle",
         reason="waiting on focus-group replies", task="", last="4m ago", spend=6.85, av="pm"),
    dict(id="principal", role="principal_engineer", group="Planning", harness="claude", model="opus", live="working",
         reason="hook 21s ago · reviewing T-0075 round 2", task="T-0075", last="21s ago", spend=18.60, av="principal"),
    dict(id="eng-1", role="engineer", group="Engineers", harness="claude", model="sonnet", live="working",
         reason="hook 3s ago · Bash(go test ./internal/store/...)", task="T-0079", last="3s ago", spend=21.30, av="eng"),
    dict(id="eng-2", role="engineer", group="Engineers", harness="claude", model="sonnet", live="working",
         reason="hook 6s ago · Write(docs/serve-prototype/…)", task="T-0087", last="6s ago", spend=12.45, av="eng"),
    dict(id="eng-3", role="engineer", group="Engineers", harness="claude", model="sonnet", live="idle",
         reason="finished T-0086 · waiting for the next task", task="", last="2m ago", spend=9.10, av="eng"),
    dict(id="eng-4", role="engineer", group="Engineers", harness="opencode", model="glm-5", live="stuck",
         reason="permission dialog open for 11m: Bash(rm -rf node_modules)", task="T-0080", last="11m ago", spend=5.30, av="eng"),
    dict(id="fg-newbie", role="focus_group", group="Focus group", harness="claude", model="haiku", live="idle",
         reason="answered ask #518 · idle", task="", last="14m ago", spend=1.20, av="fg"),
    dict(id="fg-power", role="focus_group", group="Focus group", harness="claude", model="haiku", live="dead",
         reason="session exited 3m ago · revive gave up after 3 tries (1 ≥ 3)", task="", last="3m ago", spend=0.95, av="fg"),
    dict(id="fg-skeptic", role="focus_group", group="Focus group", harness="claude", model="haiku", live="limited",
         reason="rate limit hit · resets 14:20 (in 38m)", task="", last="6m ago", spend=1.50, av="fg"),
]
AGENT = {a["id"]: a for a in AGENTS}

# id, status, assignee, priority, title, deps, waiting_on, rounds, needs_me
TASKS = [
    ("T-0071", "merged",      "eng-1", 0, "Per-agent quality stats: slai status --stats", [], [], 1, 0),
    ("T-0072", "merged",      "eng-2", 0, "TUI send box: Tab-completes task ids, branch names and agent ids", [], [], 2, 0),
    ("T-0073", "merged",      "eng-1", 1, "Per-task spend cap with a warning at 80%", [], [], 1, 0),
    ("T-0074", "merged",      "eng-3", 0, "Spend vs caps in slai status and the TUI header", ["T-0073"], [], 1, 0),
    ("T-0075", "in_review",   "eng-2", 0, "TUI stats panel: per-agent / per-model quality numbers", ["T-0071"], [], 2, 0),
    ("T-0076", "merged",      "eng-3", 2, "False hooks_failing alert: spinner timer ticks count as “pane changing”", [], [], 1, 0),
    ("T-0077", "cancelled",   "eng-4", 0, "slai doctor --lane: first draft (superseded by T-0079)", [], [], 0, 0),
    ("T-0078", "merged",      "pm",    0, "Spec: slai serve, a web UI for managing the team", [], [], 1, 0),
    ("T-0079", "in_review",   "eng-1", 1, "Team memory M1: fact store and index", [], [], 1, 0),
    ("T-0080", "in_progress", "eng-4", 0, "Team memory M2: capture facts from review verdicts", ["T-0079"], [], 0, 1),
    ("T-0081", "assigned",    "eng-3", 0, "slai revert T-xxxx: per-task rollback on the integration branch", [], [], 0, 0),
    ("T-0082", "blocked",     "eng-4", 0, "Desktop notification when ask_user is waiting", [], [], 0, 1),
    ("T-0083", "assigned",    "eng-1", 0, "Team memory M3: context pack (≤1500 tokens) on assignment and restart wakes", ["T-0075", "T-0079", "T-0080"], ["T-0075", "T-0079", "T-0080"], 0, 0),
    ("T-0084", "assigned",    "eng-2", 0, "Team memory M6: TUI memory view and per-task facts-used", ["T-0083"], ["T-0083"], 0, 0),
    ("T-0085", "reverted",    "eng-3", 0, "Auto-nudge idle engineers after 5 minutes", [], [], 1, 0),
    ("T-0086", "merged",      "eng-3", 3, "Worktree branch switch: slai moved a worktree to a queued task", [], [], 2, 0),
    ("T-0087", "in_progress", "eng-2", 2, "slai serve Phase 1: clickable static Tabler prototype", [], [], 0, 0),
]
STATUSES = ["assigned", "in_progress", "blocked", "in_review", "merged", "cancelled", "reverted"]
STATUS_LABEL = {"assigned": "Assigned", "in_progress": "In progress", "blocked": "Blocked", "in_review": "In review",
                "merged": "Merged", "cancelled": "Cancelled", "reverted": "Reverted"}
STATUS_ICON = {"assigned": "inbox", "in_progress": "player-play", "blocked": "hand-stop", "in_review": "eye-check",
               "merged": "git-merge", "cancelled": "circle-x", "reverted": "arrow-back-up"}
STATUS_CLS = {"assigned": "idle", "in_progress": "info", "blocked": "warn", "in_review": "user",
              "merged": "ok", "cancelled": "idle", "reverted": "bad"}

STATS_AGENTS = [
    # agent, harness, model, merged, cancelled, reverted, rounds, rework, refused, spend, per_merged, median
    ("eng-1", "claude", "sonnet", 9, 0, 0, 1.4, 22, 1, 61.30, 4.70, "1h12m"),
    ("eng-2", "claude", "sonnet", 8, 1, 0, 1.6, 38, 0, 48.90, 4.10, "58m"),
    ("eng-3", "claude", "sonnet", 7, 0, 1, 1.3, 14, 2, 39.40, 3.80, "44m"),
    ("eng-4", "opencode", "glm-5", 3, 1, 0, 2.3, 67, 3, 14.60, 3.20, "2h05m"),
]
STATS_GROUPS = [
    ("claude", "sonnet", 24, 1, 1, 1.4, 25, 3, 149.60, 4.19, "58m"),
    ("opencode", "glm-5", 3, 1, 0, 2.3, 67, 3, 14.60, 3.20, "2h05m"),
]

LOG = [
    ("11:42:08", "eng-2", "hook:posttooluse", "T-0087", "Write docs/serve-prototype/assets/app.js", True),
    ("11:42:03", "principal", "request_changes", "T-0075", "round 2: 0 comments. approve pending", False),
    ("11:41:51", "eng-4", "pane_decision", "T-0080", "type_blocked · claude.modal · “Do you want to proceed?”", False),
    ("11:41:20", "orch", "message", "", "orch → eng-3: T-0086 merged, pick up T-0081 when ready", False),
    ("11:40:44", "eng-1", "request_review", "T-0079", "Team memory M1: fact store and index (round 1)", False),
    ("11:39:02", "supervisor", "wake", "", "eng-3 woken: 2 new items", False),
    ("11:38:15", "eng-3", "report", "T-0086", "done · merged", False),
    ("11:36:40", "supervisor", "lane_denied", "T-0080", "eng-4 Write /home/…/.config/opencode/config.json · outside worktree", False),
    ("11:34:12", "supervisor", "health", "", "fg-power dead · revive attempt 3/3 failed: tmux session gone", False),
    ("11:31:47", "principal", "merge_refused", "T-0074", "test gate failed: TestSpendHeaderShowsCaps (internal/tui) — 1 of 41 packages", False),
    ("11:29:30", "supervisor", "restart", "", "eng-3 restart: context 91% full (cause: context_full)", False),
    ("11:27:09", "pm", "ask", "", "pm → fg-skeptic: react to the serve prototype screenshots", False),
    ("11:25:55", "supervisor", "redirect", "T-0083", "course correction delivered to eng-1 (interrupted at hook 8s)", False),
    ("11:22:18", "eng-2", "hook:pretooluse", "T-0087", "Bash go vet ./...", True),
    ("11:20:00", "supervisor", "hold", "", "team hold “release-window” lifted", False),
]

MEMORY = [
    # id, title, status, paths [(path, recorded, current)], used_by, text
    ("m-014", "Panes: Claude Code 2.1.307 prints no “esc to interrupt”", "fresh",
     [("internal/hooks/fallback.go", "a3f19c2", "a3f19c2")], ["T-0068", "T-0069", "T-0076"],
     "The working spinner is a whole line: glyph + Verb + … + (Ns · ↓ tokens). The glyph rotates (✻ ✽ ✶ ✢ · *). Match whole lines in the bottom 10 only, so prose and quoted spinners stay idle."),
    ("m-011", "Only the supervisor writes to tmux", "fresh",
     [(".mex/ROUTER.md", "5be0d44", "5be0d44")], ["T-0054", "T-0059"],
     "Non-negotiable. The TUI redirect path sets a request; the supervisor sends the keys through safeToType. No other package builds tmux commands."),
    ("m-009", "KeySpec.InterruptSeq is per harness, never hard-coded", "stale",
     [("internal/harness/claude.go", "e1c0a7b", "9d82f13"), ("internal/supervisor/redirect.go", "c2d4410", "c2d4410")], ["T-0054"],
     "The harness lint test rejects Named(\"Escape\") outside internal/harness. Stale: claude.go changed after this fact was written. Re-verify before you rely on the key name."),
    ("m-007", "Store migrations: 0004 is taken, 0005 is next", "stale",
     [("internal/store/migrations/", "7a9e0f5", "b310c68")], ["T-0059", "T-0079"],
     "Add a new numbered file, never edit an applied one. The e2e harness runs all migrations on an empty db."),
    ("m-005", "Lane guard blocks redirects to unresolved shell variables", "unverified",
     [("internal/hooks/laneguard.go", "—", "4c71e8d")], [],
     "A Bash redirect such as > $D/file is denied because the guard cannot resolve $D. Use literal paths. Recorded from a denied tool call; nobody has re-read the code since."),
    ("m-003", "SLAI_TEST_TIMESCALE shortens supervisor timers in e2e", "fresh",
     [("internal/e2e/harness.go", "f00d5ee", "f00d5ee")], ["T-0062", "T-0076"],
     "Set it to a value like 20 to run FallbackStale (30s) in 1.5s. Tests that sleep for real time are a smell."),
    ("m-002", "Spend caps: warn at 80%, pause at 100% + ask_user", "invalid",
     [("internal/service/caps.go", "2b6fa9e", "(deleted)")], ["T-0073"],
     "The file this cited was removed when caps moved to internal/spend. Invalid: the path no longer exists. Delete or rewrite."),
    ("m-001", "Worktrees live under .slai/worktrees and are never committed", "fresh",
     [(".gitignore", "0c18a27", "0c18a27")], ["T-0086"],
     "Engineers must check git branch --show-current before committing; T-0086 fixed the bug that switched a worktree to a queued task."),
]
