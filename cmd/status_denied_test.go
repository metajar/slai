package cmd

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStatusDeniedCount(t *testing.T) {
	b, _ := json.Marshal(statusAgent{ID: "eng-1", DeniedCount: 3})
	if !strings.Contains(string(b), `"denied_count":3`) {
		t.Fatalf("json = %s", b)
	}
	b, _ = json.Marshal(statusAgent{ID: "eng-1"})
	if !strings.Contains(string(b), `"denied_count":0`) {
		t.Fatalf("zero must still be present: %s", b)
	}
	if got := deniedNote(3); got != "  lane-denied(24h)=3" {
		t.Fatalf("note = %q", got)
	}
	if deniedNote(0) != "" {
		t.Fatal("zero denials must print nothing extra")
	}
}
