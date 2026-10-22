package service

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/oorbt/slai/internal/store"
)

// owesAndSilent makes eng-1 owe work (unread message) and be silent, which is
// `stale` without a gate.
func owesAndSilent(t *testing.T, st *store.Store) time.Time {
	t.Helper()
	old := time.Now().Add(-StaleAfter).Unix()
	_ = st.SetKV(lastSeenKey("eng-1"), strconv.FormatInt(old, 10))
	if _, err := st.AddMessage(store.Message{From: "orch", To: "eng-1", Kind: "note", Body: "x"}); err != nil {
		t.Fatal(err)
	}
	return time.Now().Add(3 * StaleAfter)
}

func TestGatedAgentIsBlockedNotStale(t *testing.T) {
	s := newSvc(t, true)
	st := s.Store
	now := owesAndSilent(t, st)
	cfg := GateConfigOf(s.Team)
	if h := AgentHealthGated(st, "eng-1", true, now, cfg); h.Liveness != "stale" {
		t.Fatalf("ungated: %+v", h)
	}

	// per-agent pause
	_ = st.SetAgentPaused("eng-1", true)
	if h := AgentHealthGated(st, "eng-1", true, now, cfg); h.Liveness != "gated" || h.Reason != "paused" {
		t.Fatalf("paused: %+v", h)
	}
	// loop-guard hold wins and names the time
	until := now.Add(time.Hour)
	_ = st.SetKV(kvHoldPrefix+"eng-1", strconv.FormatInt(until.Unix(), 10))
	want := "held until " + until.Format("15:04")
	if h := AgentHealthGated(st, "eng-1", true, now, cfg); h.Reason != want {
		t.Fatalf("held: %+v want %q", h, want)
	}
	_ = st.SetAgentPaused("eng-1", false)
	_ = st.DeleteKV(kvHoldPrefix + "eng-1")

	// global pause
	_ = st.SetKV(kvGlobalPaused, "1")
	if h := AgentHealth(st, "eng-1", true, now); h.Liveness != "gated" || h.Reason != "paused" {
		t.Fatalf("global pause: %+v", h)
	}
	_ = st.SetKV(kvGlobalPaused, "0")

	// spend cap
	_ = st.AddUsage("eng-1", 0, 0, 3)
	capped := GateConfig{PerAgentUSD: 1}
	if h := AgentHealthGated(st, "eng-1", true, now, capped); h.Reason != "spend capped" {
		t.Fatalf("spend: %+v", h)
	}
}

func TestQueuedForSlot(t *testing.T) {
	s := newSvc(t, true)
	st := s.Store
	now := owesAndSilent(t, st)
	_ = st.SetAgentStatus("eng-1", "idle")
	_ = st.SetAgentStatus("pm", "working")
	cfg := GateConfig{MaxConcurrent: 1}
	g := ReadAgentGate(st, cfg, "eng-1", now)
	if !g.QueuedForSlot {
		t.Fatalf("want queued: %+v", g)
	}
	if h := AgentHealthGated(st, "eng-1", true, now, cfg); h.Reason != "queued for slot" {
		t.Fatalf("health: %+v", h)
	}
	if ReadAgentGate(st, GateConfig{MaxConcurrent: 5}, "eng-1", now).QueuedForSlot {
		t.Fatal("free slot must not queue")
	}
}

func TestBoardCarriesWakeGates(t *testing.T) {
	s := newSvc(t, true)
	st := s.Store
	_ = st.SetKV(kvGlobalPaused, "1")
	_ = st.SetAgentPaused("eng-1", true)
	until := time.Now().Add(time.Hour).Unix()
	_ = st.SetKV(kvHoldPrefix+"eng-1", strconv.FormatInt(until, 10))
	out, err := s.GetBoard(context.Background(), "orch", StatusFilter{})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var got struct {
		Agents []struct {
			ID            string `json:"id"`
			Paused        bool   `json:"paused"`
			HeldUntil     int64  `json:"held_until"`
			QueuedForSlot bool   `json:"queued_for_slot"`
		} `json:"agents"`
		WakeGate WakeGate `json:"wake_gate"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !got.WakeGate.Paused || !got.WakeGate.LoopHold || got.WakeGate.SpendCapped {
		t.Fatalf("wake_gate: %+v", got.WakeGate)
	}
	found := false
	for _, a := range got.Agents {
		if a.ID == "eng-1" {
			found = true
			if !a.Paused || a.HeldUntil != until {
				t.Fatalf("eng-1: %+v", a)
			}
		}
	}
	if !found {
		t.Fatal("eng-1 missing from board")
	}
}

// A held team is gated with the reason "team hold" (never stale), and the board's
// wake_gate names the hold.
func TestTeamHoldIsGatedNotStale(t *testing.T) {
	s := newSvc(t, true)
	st := s.Store
	now := owesAndSilent(t, st)
	_ = st.SetKV(kvGlobalPaused, "1")
	_ = st.SetKV(kvTeamHold, `{"name":"lunch","since":1700000000}`)
	h := AgentHealth(st, "eng-1", true, now)
	if h.Liveness != "gated" || h.Reason != "team hold" {
		t.Fatalf("held team: %+v, want gated / team hold", h)
	}
	w := NewGateSource(st, GateConfigOf(s.Team), now).Wake([]string{"eng-1"})
	if !w.TeamHold || w.HoldName != "lunch" || w.HoldSince != 1700000000 || !w.Paused {
		t.Fatalf("wake gate = %+v", w)
	}
	// a hold record left behind after a plain resume is not a hold
	_ = st.SetKV(kvGlobalPaused, "0")
	if w := NewGateSource(st, GateConfigOf(s.Team), now).Wake(nil); w.TeamHold {
		t.Fatalf("stale hold record counted: %+v", w)
	}
}
