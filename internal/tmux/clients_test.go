package tmux

import (
	"testing"
	"time"
)

func TestParseClients(t *testing.T) {
	cs := parseClients("1700000000 /dev/pts/3\n\nnot-a-number /dev/pts/4\n")
	if len(cs) != 2 || cs[0].Name != "/dev/pts/3" || cs[0].Activity.Unix() != 1700000000 {
		t.Fatalf("clients = %+v", cs)
	}
	if time.Since(cs[1].Activity) > time.Minute {
		t.Fatal("an unreadable activity time must read as active, not idle since the epoch")
	}
}
