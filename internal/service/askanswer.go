package service

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/oorbt/slai/internal/store"
)

// kvAsked is the key prefix under which an answered ask_user event is recorded (+ the event id).
// The web inbox and `slai reply` both write it, so either surface shows the ask as answered.
const kvAsked = "web.answered."

// AnswerRec is what is stored under web.answered.<id>.
type AnswerRec struct {
	At     int64  `json:"at"`
	Msg    int64  `json:"msg"`
	Answer string `json:"answer"`
}

// ErrAskNotOpen: the ask is answered already or its agent has moved on.
var ErrAskNotOpen = errors.New("that question is not waiting for an answer any more (the agent moved on, or it was already answered)")

// AskAnswer returns the recorded answer to the ask_user event, if there is one.
func AskAnswer(st *store.Store, eventID int64) (AnswerRec, bool) {
	val, ok, _ := st.GetKV(kvAsked + strconv.FormatInt(eventID, 10))
	var r AnswerRec
	if !ok || json.Unmarshal([]byte(val), &r) != nil {
		return r, false
	}
	return r, true
}

// OpenAskID is the id of the ask_user event the agent is waiting on and nobody has answered,
// or 0 when there is none.
func OpenAskID(st *store.Store, agent string) int64 {
	ag, err := st.AgentByID(agent)
	if err != nil || ag.Status != "waiting_on_user" {
		return 0
	}
	evs, err := st.EventsByKind(agent, "ask_user", 1)
	if err != nil || len(evs) == 0 {
		return 0
	}
	if _, done := AskAnswer(st, evs[0].ID); done {
		return 0
	}
	return evs[0].ID
}

// AnswerAsk answers the ask_user event id as the user: it checks that the ask is still open,
// sends the text to the asking agent through send (the supervisor's SendUser, with the ask
// id as reply_to where the caller can set it) and records the answer so every surface shows
// it as answered. It returns the agent that was answered. This is the one code path behind
// the web inbox's answer button and `slai reply`.
func AnswerAsk(st *store.Store, now time.Time, id int64, text string, send func(to, body string) (int64, error)) (string, error) {
	ev, err := st.GetEvent(id)
	if err != nil || ev.Kind != "ask_user" || ev.Agent == "" {
		return "", errors.New("unknown question: only an agent's ask_user can be answered")
	}
	if _, done := AskAnswer(st, id); done {
		return "", ErrAskNotOpen
	}
	ag, err := st.AgentByID(ev.Agent)
	if err != nil || ag.Status != "waiting_on_user" {
		return "", ErrAskNotOpen
	}
	if newest, _ := st.EventsByKind(ev.Agent, "ask_user", 1); len(newest) == 0 || newest[0].ID != id {
		return "", ErrAskNotOpen
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("the answer is empty")
	}
	if err := CheckText("answer", text); err != nil {
		return "", err
	}
	msg, err := send(ev.Agent, text)
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(AnswerRec{At: now.Unix(), Msg: msg, Answer: text})
	_ = st.SetKV(kvAsked+strconv.FormatInt(id, 10), string(b))
	return ev.Agent, nil
}
