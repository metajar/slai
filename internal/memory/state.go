package memory

import (
	"context"
	"fmt"
)

// The states people see (`slai memory ls`, doctor, the web page). Unlike the stored status
// (proposed | verified), these are computed against integration now.
const (
	StateFresh      = "fresh"      // verified, cites paths, none changed
	StateStale      = "stale"      // a cited path changed or vanished (whatever the status)
	StateUnverified = "unverified" // not verified, or cites no path so it can never be grounded
	StateInvalid    = "invalid"    // the file does not parse; kept on disk, kept out of packs
)

// View is a fact with its state computed now.
type View struct {
	Fact
	State  string
	Reason string // why it is stale ("path changed", "path no longer exists")
}

// staleAgainst compares the blobs f recorded with those found on integration (blobs maps a
// path to its blob id for the paths that exist). It is the one definition of stale.
func staleAgainst(f Fact, blobs map[string]string) (stale bool, reason string) {
	for _, p := range f.Paths {
		switch b, ok := blobs[p.Path]; {
		case !ok:
			return true, p.Path + " no longer exists"
		case b != p.Blob:
			return true, p.Path + " changed"
		}
	}
	return false, ""
}

func stateOf(f Fact, stale bool, reason string) View {
	v := View{Fact: f, Reason: reason}
	switch {
	case stale:
		v.State = StateStale
	case f.Status != StatusVerified || len(f.Paths) == 0:
		v.State = StateUnverified
	default:
		v.State = StateFresh
	}
	return v
}

// View computes f's state with one ls-tree call.
func (m *Memory) View(ctx context.Context, f Fact) (View, error) {
	res, err := m.Check(ctx, f)
	if err != nil {
		return View{Fact: f}, fmt.Errorf("checking %s: %w", f.ID, err)
	}
	return stateOf(f, res.Stale, res.Reason), nil
}

// Views lists every fact with its state, and the files that do not parse. Unlike calling
// View per fact it asks git once for every cited path of every fact, so a page of hundreds
// of facts costs one git invocation.
func (m *Memory) Views(ctx context.Context) ([]View, []Invalid, error) {
	facts, bad, err := m.List()
	if err != nil {
		return nil, nil, err
	}
	var paths []string
	seen := map[string]bool{}
	for _, f := range facts {
		for _, p := range f.Paths {
			if !seen[p.Path] {
				seen[p.Path] = true
				paths = append(paths, p.Path)
			}
		}
	}
	blobs := map[string]string{}
	if len(paths) > 0 {
		if blobs, err = m.Git.LsTree(ctx, m.rev(), paths); err != nil {
			return nil, nil, err
		}
	}
	vs := make([]View, 0, len(facts))
	for _, f := range facts {
		stale, reason := staleAgainst(f, blobs)
		vs = append(vs, stateOf(f, stale, reason))
	}
	return vs, bad, nil
}

// CurrentBlobs returns, for each path f cites, its blob id on integration now ("" when the
// path is gone), with one ls-tree call.
func (m *Memory) CurrentBlobs(ctx context.Context, f Fact) (map[string]string, error) {
	out := map[string]string{}
	if len(f.Paths) == 0 {
		return out, nil
	}
	names := make([]string, len(f.Paths))
	for i, p := range f.Paths {
		names[i] = p.Path
	}
	blobs, err := m.Git.LsTree(ctx, m.rev(), names)
	if err != nil {
		return nil, err
	}
	for _, n := range names {
		out[n] = blobs[n]
	}
	return out, nil
}
