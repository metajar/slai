package service

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/oorbt/slai/internal/roles"
)

// Whoami answers who the caller is, from slai's record of the connection (the id the MCP
// server was started for), never from an argument: an agent that has lost track of its
// identity still gets the true answer. The data is the same as .slai/agents.json.
func (s *Service) Whoami(ctx context.Context, caller string) (any, error) {
	if err := s.begin(caller, "whoami", struct{}{}); err != nil {
		return nil, err
	}
	_ = s.Store.SetKV(KVWhoami+caller, strconv.FormatInt(time.Now().Unix(), 10)) // proof it re-grounded after a reset
	id, ok := roles.IdentityOf(s.team(), caller)
	if !ok {
		return nil, fmt.Errorf("unknown agent %q", caller)
	}
	out := map[string]any{
		"id": id.ID, "role": id.Role, "responsibilities": id.Responsibilities, "model": id.Model, "harness": id.Harness,
		"reports_to": id.ReportsTo, "teammates": id.Teammates,
		"identity":     roles.IdentityLine(id),
		"current_task": nil,
	}
	if tid, _ := s.Store.WorkingTask(caller); tid != "" {
		task := map[string]any{"id": tid}
		if t, err := s.task(tid); err == nil {
			task["title"], task["status"] = t.Title, t.Status
		}
		out["current_task"] = task
	}
	return out, nil
}
