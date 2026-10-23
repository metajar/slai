package agentrows

import (
	"strings"
	"testing"
	"time"
)

func TestContextSizeAndThresholdHint(t *testing.T) {
	c := Context{AgentID: "eng-1", Tokens: 142000, Threshold: 150000}
	if got := c.Size(); got != "ctx 142k" || c.Over() {
		t.Fatalf("under: %q over=%v", got, c.Over())
	}
	c.Tokens = 162000
	if got := c.Size(); got != "ctx 162k (over threshold; run `slai clear eng-1`)" || !c.Over() {
		t.Fatalf("over: %q over=%v", got, c.Over())
	}
	c.Tokens = 150000
	if c.Over() {
		t.Fatal("exactly at the threshold is not over it")
	}
	if got := (Context{AgentID: "x"}).Size(); got != "" {
		t.Fatalf("nothing recorded reads %q", got)
	}
}

func TestTokensAndResetWording(t *testing.T) {
	for n, want := range map[int64]string{850: "850", 1000: "1k", 142400: "142k", 142600: "143k", 1_200_000: "1.2M"} {
		if got := Tokens(n); got != want {
			t.Errorf("Tokens(%d) = %q, want %q", n, got, want)
		}
	}
	now := time.Unix(1_800_000_000, 0)
	if got := (Context{}).Reset(now); got != "never reset" {
		t.Fatal(got)
	}
	if got := (Context{LastResetAt: now.Unix() - 180}).Reset(now); !strings.HasPrefix(got, "reset 3m") {
		t.Fatal(got)
	}
}
