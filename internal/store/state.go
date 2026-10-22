package store

// Task statuses (PRD 6.2).
const (
	StatusAssigned         = "assigned"
	StatusInProgress       = "in_progress"
	StatusInReview         = "in_review"
	StatusChangesRequested = "changes_requested"
	StatusMerged           = "merged"
	StatusBlocked          = "blocked"
	StatusCancelled        = "cancelled"
	StatusDone             = "done"     // finished non-code work (e.g. a PM spec); never merged
	StatusReverted         = "reverted" // was merged, then `slai revert` took it back out of the integration branch
)

var validStatus = map[string]bool{
	StatusAssigned: true, StatusInProgress: true, StatusInReview: true,
	StatusChangesRequested: true, StatusMerged: true, StatusBlocked: true, StatusCancelled: true, StatusDone: true, StatusReverted: true,
}

var allowed = map[string]map[string]bool{
	StatusAssigned:         {StatusInProgress: true},
	StatusInProgress:       {StatusInReview: true, StatusDone: true},
	StatusInReview:         {StatusMerged: true, StatusChangesRequested: true, StatusInProgress: true},
	StatusChangesRequested: {StatusInProgress: true},
	StatusBlocked:          {StatusInProgress: true},
}

// CanTransition reports whether a task may move from one status to another.
// merged and cancelled are terminal; any non-terminal state may go to
// blocked or cancelled. Same-state transitions are not allowed.
func CanTransition(from, to string) bool {
	if !validStatus[from] || !validStatus[to] || from == to {
		return false
	}
	if from == StatusMerged || from == StatusCancelled || from == StatusDone || from == StatusReverted {
		return false
	}
	if to == StatusBlocked || to == StatusCancelled {
		return true
	}
	return allowed[from][to]
}
