package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/oorbt/slai/internal/service"
	"github.com/oorbt/slai/internal/store"
)

func runReply(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := replyCmd()
	c.SetArgs(args)
	c.SilenceUsage, c.SilenceErrors = true, true
	r, w, _ := os.Pipe()
	old := os.Stdout
	os.Stdout = w
	err := c.ExecuteContext(context.Background())
	w.Close()
	os.Stdout = old
	b := new(bytes.Buffer)
	b.ReadFrom(r)
	return b.String(), err
}

// askProject is a solo project whose orch is waiting on an ask_user; it returns the ask's event id.
func askProject(t *testing.T) (*store.Store, int64) {
	t.Helper()
	dir := project(t, true)
	st, err := store.Open(filepath.Join(dir, ".slai", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.UpsertAgent(store.Agent{ID: "orch", Role: "orchestrator", Status: "waiting_on_user"}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddEvent("orch", "ask_user", `{"question":"which port?"}`); err != nil {
		t.Fatal(err)
	}
	evs, _ := st.EventsByKind("orch", "ask_user", 1)
	return st, evs[0].ID
}

func TestReplyDeliversLinkedMessageAndMarksAnswered(t *testing.T) {
	st, id := askProject(t)
	if got := service.OpenAskID(st, "orch"); got != id {
		t.Fatalf("OpenAskID = %d, want %d", got, id)
	}
	out, err := runReply(t, itoa(id), "use 8080")
	if err != nil || !strings.Contains(out, "sent to orch") {
		t.Fatalf("out %q err %v", out, err)
	}
	msgs, _ := st.RecentMessages(10)
	if len(msgs) != 1 || msgs[0].To != "orch" || msgs[0].Kind != "user" || msgs[0].ReplyTo != id || msgs[0].Body != "use 8080" {
		t.Fatalf("messages = %+v", msgs)
	}
	rec, ok := service.AskAnswer(st, id)
	if !ok || rec.Answer != "use 8080" || rec.Msg != msgs[0].ID {
		t.Fatalf("answered rec = %+v, %v", rec, ok)
	}
	// the web inbox lists an ask as open only while OpenAskID would; it is closed now
	if got := service.OpenAskID(st, "orch"); got != 0 {
		t.Fatalf("OpenAskID after reply = %d", got)
	}
}

func TestReplyRefusesUnknownAndAnswered(t *testing.T) {
	st, id := askProject(t)
	if _, err := runReply(t, "9999", "x"); err == nil || !strings.Contains(err.Error(), "unknown question") {
		t.Fatalf("unknown id: %v", err)
	}
	if _, err := runReply(t, "abc", "x"); err == nil || !strings.Contains(err.Error(), "not a number") {
		t.Fatalf("non-numeric: %v", err)
	}
	if _, err := runReply(t, itoa(id), "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := runReply(t, itoa(id), "second"); err == nil || !strings.Contains(err.Error(), "not waiting for an answer") {
		t.Fatalf("answered id: %v", err)
	}
	if msgs, _ := st.RecentMessages(10); len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
}

func TestReplyRefusesWhenAgentMovedOn(t *testing.T) {
	st, id := askProject(t)
	st.SetAgentStatus("orch", "idle")
	if _, err := runReply(t, itoa(id), "late"); err == nil || !strings.Contains(err.Error(), "not waiting for an answer") {
		t.Fatalf("err = %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
