package supervisor

import (
	"encoding/json"
)

// KVTeamHold is "team.hold": a named, persistent hold of the whole team (`slai hold`).
// A hold is the global pause plus this record, so every gate that honours the pause
// (wakes, driver reminders and digests, health restarts, redirects) honours it, and
// it survives a supervisor restart and `slai down` + `slai up` because it lives in
// the state database. Sessions are never touched.
const KVTeamHold = "team.hold"

// Hold is the record behind KVTeamHold.
type Hold struct {
	Name  string `json:"name"`
	Since int64  `json:"since"`
}

// HoldTeam puts the team on hold. Agents finish the turn they are in; nothing new is
// delivered. Holding an already held team renames the hold and keeps its start time.
func (s *Supervisor) HoldTeam(name string) error {
	h := Hold{Name: name, Since: s.now().Unix()}
	if cur, ok := s.CurrentHold(); ok {
		h.Since = cur.Since
	}
	b, _ := json.Marshal(h)
	if err := s.Store.SetKV(KVTeamHold, string(b)); err != nil {
		return err
	}
	if err := s.Pause(); err != nil {
		return err
	}
	s.event("", "team_hold", map[string]any{"name": name})
	return nil
}

// CurrentHold reports the team hold, if one is in force.
func (s *Supervisor) CurrentHold() (Hold, bool) {
	v := s.kvGet(KVTeamHold)
	var h Hold
	if v == "" || json.Unmarshal([]byte(v), &h) != nil || !s.Paused() {
		return h, false
	}
	return h, true
}

// ReleaseHold lifts the team hold. The wake loop then delivers what is owed on its
// next tick: messages stay pending while held, and reminders resume from the idle
// time they had, which the clock kept running through the hold.
func (s *Supervisor) ReleaseHold() (Hold, bool, error) {
	h, ok := s.CurrentHold()
	if !ok {
		return h, false, nil
	}
	if err := s.Resume(); err != nil {
		return h, true, err
	}
	s.event("", "team_resume", map[string]any{"name": h.Name, "held_s": s.now().Unix() - h.Since})
	return h, true, nil
}
