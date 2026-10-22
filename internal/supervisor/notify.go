package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/oorbt/slai/internal/config"
)

const kvNotifyCursor = "notify.cursor" // id of the last event the notify hook has looked at

// notifyEventOf maps an activity-log row to a notify event name ("" = not one).
// Events are written exactly once (escalations through the fire-once state), so
// reading the log in order is what makes each notification fire once.
func notifyEventOf(kind, data string) string {
	var d struct {
		Reason     string `json:"reason"`
		Escalation bool   `json:"escalation"`
	}
	_ = json.Unmarshal([]byte(data), &d)
	switch {
	case d.Escalation:
		return "escalation"
	case kind == "ask_user" && d.Reason == "spend_cap":
		return "" // the spend_cap event already covers it
	case kind == "ask_user":
		return "awaiting_user"
	case kind == "request_review":
		return "review_requested"
	case kind == "merge_requested":
		return "review_approved"
	case kind == "finish":
		return "run_finished"
	case kind == "spend_warn", kind == "spend_cap":
		return kind
	}
	return ""
}

// notifyTick runs team.yaml notify.command once for each new configured event. The
// command runs in the background with a timeout, so a slow or hanging command never
// delays the tick; failures are logged and nothing else. With no notify.command
// nothing is read or changed. Events from before notify was first enabled are not
// replayed.
func (s *Supervisor) notifyTick(ctx context.Context) {
	n := s.Team.Notify
	if n.Command == "" {
		return
	}
	cur, have := int64(atoi(s.kvGet(kvNotifyCursor))), s.kvGet(kvNotifyCursor) != ""
	evs, err := s.Store.EventsAfter(cur, "", 200)
	if err != nil {
		s.logf("notify: %v", err)
		return
	}
	if !have { // first run: start from now
		last := cur
		if len(evs) > 0 {
			last = s.Store.LastEventID()
		}
		_ = s.Store.SetKV(kvNotifyCursor, strconv.FormatInt(last, 10))
		return
	}
	for _, e := range evs {
		cur = e.ID
		if name := notifyEventOf(e.Kind, e.Data); name != "" && n.Wants(name) {
			s.runNotify(ctx, n, name, e.Agent, e.At, e.Data)
		}
	}
	_ = s.Store.SetKV(kvNotifyCursor, strconv.FormatInt(cur, 10))
}

func (s *Supervisor) runNotify(ctx context.Context, n config.Notify, name, agent string, at int64, data string) {
	timeout := time.Duration(n.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = config.DefaultNotifyTimeoutSeconds * time.Second
	}
	payload, _ := json.Marshal(map[string]any{"event": name, "agent": agent, "at": at, "project": s.Team.Project.Name, "data": json.RawMessage(data)})
	go func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		cmd := exec.Command("sh", "-c", n.Command)
		cmd.Dir = s.Dir
		cmd.Env = append(os.Environ(), "SLAI_EVENT="+name, "SLAI_AGENT="+agent, "SLAI_PROJECT="+s.Dir)
		cmd.Stdin = bytes.NewReader(payload)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := cmd.Start(); err != nil {
			s.logf("notify %s: %v", name, err)
			return
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				s.logf("notify %s: command failed: %v", name, err)
			}
		case <-c.Done():
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // the whole group: sh and what it started
			<-done
			s.logf("notify %s: command killed after %s", name, timeout)
		}
	}()
}
