package store

import (
	"path/filepath"
	"testing"
)

func TestLaneDenialCountsPerAgentWindow(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.AddEvent("eng-1", "lane_denied", `{"tool":"Write"}`)
	_ = st.AddEvent("eng-1", "lane_denied", `{"tool":"Bash"}`)
	_ = st.AddEvent("eng-2", "lane_denied", `{}`)
	_ = st.AddEvent("eng-1", "guard:lane", `{}`) // the older duplicate name is not counted
	got, err := st.LaneDenialCounts(0)
	if err != nil || got["eng-1"] != 2 || got["eng-2"] != 1 || len(got) != 2 {
		t.Fatalf("counts = %v err=%v", got, err)
	}
	if got, _ := st.LaneDenialCounts(1 << 40); len(got) != 0 {
		t.Fatalf("future window counted %v", got)
	}
}
